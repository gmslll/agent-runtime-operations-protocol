package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Store struct {
	db      *sql.DB
	lookup  durable.TransactionLookup
	dialect Dialect
}

func New(db *sql.DB, lookup durable.TransactionLookup, dialect Dialect) (*Store, error) {
	if db == nil || lookup == nil || dialect != SQLite && dialect != Postgres {
		return nil, errors.New("event store dependencies are required")
	}
	return &Store{db: db, lookup: lookup, dialect: dialect}, nil
}

type attemptView struct {
	TenantID, RunID, AttemptID, DeploymentID, InstanceID, SessionID, ServiceID string
	Generation, FencingToken                                                   uint64
	State                                                                      dispatch.AttemptState
	LeaseExpiresAt                                                             time.Time
	Endpoint                                                                   string
	RunState                                                                   run.State
}

func (store *Store) CreateSession(ctx context.Context, command event.SessionCommand) (event.Session, error) {
	if command.Request.Validate() != nil || !utc(command.Now) || !utc(command.ExpiresAt) || !utc(command.LeaseExpiresAt) || !command.ExpiresAt.After(command.Now) || !command.LeaseExpiresAt.After(command.ExpiresAt) || event.TokenDigest(command.Token) != command.TokenDigest {
		return event.Session{}, event.NewError(event.CategoryValidation, event.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return event.Session{}, err
	}
	if err = store.lock(ctx, tx, "event/attempt/"+command.Request.AttemptID); err != nil {
		return event.Session{}, err
	}
	view, err := store.loadAttempt(ctx, tx, command.Request.TenantID, command.Request.AttemptID)
	if err != nil {
		if typed, ok := event.AsError(err); ok && typed.Category == event.CategoryNotFound {
			return event.Session{}, event.NewError(event.CategoryAuthentication, event.ReasonAuthentication)
		}
		return event.Session{}, err
	}
	request := command.Request
	if view.TenantID != request.TenantID || view.RunID != request.RunID || view.AttemptID != request.AttemptID || view.DeploymentID != request.DeploymentID || view.Generation != request.Generation || view.FencingToken != request.FencingToken {
		return event.Session{}, event.NewError(event.CategoryAuthorization, event.ReasonAttemptFenced)
	}
	if view.State != dispatch.StateIssued && view.State != dispatch.StateAccepted || !command.Now.Before(view.LeaseExpiresAt) || view.RunState.Terminal() {
		return event.Session{}, event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	if err = store.requireActiveRuntime(ctx, tx, view, command.Now); err != nil {
		return event.Session{}, err
	}
	if command.LeaseExpiresAt.After(view.LeaseExpiresAt) {
		result, updateErr := tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET lease_expires_at=? WHERE tenant_id=? AND attempt_id=? AND generation=? AND fencing_token=? AND lease_expires_at=?`), formatTime(command.LeaseExpiresAt), view.TenantID, view.AttemptID, view.Generation, view.FencingToken, formatTime(view.LeaseExpiresAt))
		if updateErr != nil {
			return event.Session{}, store.classify(updateErr)
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return event.Session{}, event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
		}
		view.LeaseExpiresAt = command.LeaseExpiresAt
	}
	expires := minimumTime(command.ExpiresAt, view.LeaseExpiresAt)
	if !expires.After(command.Now) {
		return event.Session{}, event.NewError(event.CategoryAuthentication, event.ReasonSessionExpired)
	}
	session := event.Session{TenantID: view.TenantID, RunID: view.RunID, AttemptID: view.AttemptID, DeploymentID: view.DeploymentID, InstanceID: view.InstanceID, SessionID: view.SessionID, Generation: view.Generation, FencingToken: view.FencingToken, Token: command.Token, TokenDigest: command.TokenDigest, ExpiresAt: expires, CreatedAt: command.Now}
	if session.Validate() != nil {
		return event.Session{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_event_sessions(tenant_id,run_id,attempt_id,deployment_id,instance_id,runtime_session_id,generation,fencing_token,token_digest,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), session.TenantID, session.RunID, session.AttemptID, session.DeploymentID, session.InstanceID, session.SessionID, session.Generation, session.FencingToken, session.TokenDigest, formatTime(session.ExpiresAt), formatTime(session.CreatedAt))
	if err != nil {
		return event.Session{}, store.classify(err)
	}
	if view.State == dispatch.StateIssued {
		result, updateErr := tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='accepted',accepted_at=? WHERE tenant_id=? AND attempt_id=? AND attempt_state='issued'`), formatTime(command.Now), view.TenantID, view.AttemptID)
		if updateErr != nil {
			return event.Session{}, store.classify(updateErr)
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return event.Session{}, event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
		}
	}
	if view.RunState == run.StateDispatching {
		result, updateErr := tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='running',state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state='dispatching'`), formatTime(command.Now), view.TenantID, view.RunID)
		if updateErr != nil {
			return event.Session{}, store.classify(updateErr)
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return event.Session{}, event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
		}
	}
	return session, nil
}

func (store *Store) Append(ctx context.Context, command event.AppendCommand) (event.Ack, error) {
	if command.Request.Validate() != nil || command.TokenDigest != event.TokenDigest(command.Request.Token) || len(command.IdempotencyKeyDigest) != 64 || !strings.HasPrefix(command.RequestDigest, "sha256:") || !utc(command.Now) {
		return event.Ack{}, event.NewError(event.CategoryValidation, event.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return event.Ack{}, err
	}
	var session event.Session
	var expires, created string
	err = tx.QueryRowContext(ctx, store.query(`SELECT tenant_id,run_id,attempt_id,deployment_id,instance_id,runtime_session_id,generation,fencing_token,token_digest,expires_at,created_at FROM arop_event_sessions WHERE token_digest=?`), command.TokenDigest).Scan(&session.TenantID, &session.RunID, &session.AttemptID, &session.DeploymentID, &session.InstanceID, &session.SessionID, &session.Generation, &session.FencingToken, &session.TokenDigest, &expires, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return event.Ack{}, event.NewError(event.CategoryAuthentication, event.ReasonAuthentication)
		}
		return event.Ack{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	// Timestamps are parsed and validated before the capability is trusted.
	session.ExpiresAt, err = parseTime(expires)
	if err == nil {
		session.CreatedAt, err = parseTime(created)
	}
	session.Token = command.Request.Token
	if err != nil || session.Validate() != nil || session.RunID != command.Request.RunID || session.AttemptID != command.Request.AttemptID || session.FencingToken != command.Request.FencingToken || !command.Now.Before(session.ExpiresAt) {
		return event.Ack{}, event.NewError(event.CategoryAuthentication, event.ReasonSessionExpired)
	}
	if err = store.lock(ctx, tx, "event/run/"+session.TenantID+"/"+session.RunID, "event/attempt/"+session.TenantID+"/"+session.AttemptID); err != nil {
		return event.Ack{}, err
	}
	if ack, stored, found, lookupErr := store.loadBatch(ctx, tx, session.TenantID, session.AttemptID, command.IdempotencyKeyDigest); lookupErr != nil {
		return event.Ack{}, lookupErr
	} else if found {
		if stored != command.RequestDigest {
			return event.Ack{}, event.NewError(event.CategoryConflict, event.ReasonIdempotencyConflict)
		}
		return ack, nil
	}
	view, err := store.loadAttempt(ctx, tx, session.TenantID, session.AttemptID)
	if err != nil {
		return event.Ack{}, err
	}
	if view.TenantID != session.TenantID || view.RunID != session.RunID || view.DeploymentID != session.DeploymentID || view.InstanceID != session.InstanceID || view.SessionID != session.SessionID || view.Generation != session.Generation || view.FencingToken != session.FencingToken || view.State != dispatch.StateAccepted || !command.Now.Before(view.LeaseExpiresAt) {
		return event.Ack{}, event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	if err = store.requireActiveRuntime(ctx, tx, view, command.Now); err != nil {
		return event.Ack{}, err
	}
	expectedSource := strings.TrimSuffix(view.Endpoint, "/v1/runs") + "/instances/" + view.InstanceID
	for _, incoming := range command.Request.Events {
		if incoming.Source != expectedSource {
			return event.Ack{}, event.NewError(event.CategoryAuthorization, event.ReasonAuthorization)
		}
	}
	lastProducer, err := store.lastProducer(ctx, tx, session)
	if err != nil {
		return event.Ack{}, err
	}
	lastRun, terminal, err := store.runProjection(ctx, tx, session)
	if err != nil {
		return event.Ack{}, err
	}
	duplicates := []string{}
	runState := view.RunState
	projectionChanged := false
	for _, incoming := range command.Request.Events {
		encoded, digest, digestErr := incoming.Canonical()
		if digestErr != nil {
			return event.Ack{}, digestErr
		}
		var storedDigest string
		var storedProducer uint64
		lookupErr := tx.QueryRowContext(ctx, store.query(`SELECT event_digest,producer_sequence FROM arop_event_ledger WHERE tenant_id=? AND source=? AND event_id=?`), session.TenantID, incoming.Source, incoming.ID).Scan(&storedDigest, &storedProducer)
		if lookupErr == nil {
			if storedDigest != digest || storedProducer != incoming.ProducerSequence {
				return event.Ack{}, event.NewError(event.CategoryConflict, event.ReasonEventIDConflict)
			}
			duplicates = append(duplicates, incoming.ID)
			continue
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return event.Ack{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
		}
		if incoming.ProducerSequence != lastProducer+1 {
			return event.Ack{}, event.NewError(event.CategoryConflict, event.ReasonProducerSequence)
		}
		lastProducer++
		lastRun++
		if lastRun > event.MaxSafeInteger {
			return event.Ack{}, event.NewError(event.CategoryCapacity, event.ReasonBatchTooLarge)
		}
		applied := !terminal
		if applied {
			if projection, isTerminal, terminalErr := event.Terminal(incoming); terminalErr != nil {
				return event.Ack{}, terminalErr
			} else if isTerminal {
				terminal = true
				runState = projection.State
				projectionChanged = true
				if err = store.applyTerminal(ctx, tx, session, incoming, lastRun, projection, command.Now); err != nil {
					return event.Ack{}, err
				}
			} else if state, ok := event.NonTerminalState(incoming); ok {
				runState = state
				projectionChanged = true
				if err = store.applyState(ctx, tx, session, state, command.Now); err != nil {
					return event.Ack{}, err
				}
			} else if usage, ok, usageErr := event.Usage(incoming); usageErr != nil {
				return event.Ack{}, usageErr
			} else if ok {
				projectionChanged = true
				if err = store.applyUsage(ctx, tx, session, usage, command.Now); err != nil {
					return event.Ack{}, err
				}
			}
		}
		_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_event_ledger(tenant_id,run_id,run_sequence,attempt_id,producer_sequence,event_id,source,event_type,event_digest,envelope_json,occurred_at,received_at,projection_applied) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), session.TenantID, session.RunID, lastRun, session.AttemptID, incoming.ProducerSequence, incoming.ID, incoming.Source, incoming.Type, digest, string(encoded), formatTime(incoming.Time), formatTime(command.Now), applied)
		if err != nil {
			return event.Ack{}, store.classify(err)
		}
	}
	if lastProducer == 0 || lastRun == 0 {
		return event.Ack{}, event.NewError(event.CategoryConflict, event.ReasonProducerSequence)
	}
	if err = store.saveProjections(ctx, tx, session, lastProducer, lastRun, command.Now); err != nil {
		return event.Ack{}, err
	}
	if !projectionChanged {
		var current string
		if err = tx.QueryRowContext(ctx, store.query(`SELECT state FROM arop_runs WHERE tenant_id=? AND run_id=?`), session.TenantID, session.RunID).Scan(&current); err != nil {
			return event.Ack{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
		}
		runState = run.State(current)
	}
	sort.Strings(duplicates)
	duplicatesJSON, _ := json.Marshal(duplicates)
	ack := event.Ack{AcceptedThroughProducerSequence: lastProducer, AssignedRunSequence: lastRun, DuplicateEventIDs: duplicates, RunState: runState}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_event_batches(tenant_id,attempt_id,batch_id,idempotency_key_digest,request_digest,accepted_through_producer_sequence,assigned_run_sequence,duplicate_event_ids_json,run_state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`), session.TenantID, session.AttemptID, command.Request.BatchID, command.IdempotencyKeyDigest, command.RequestDigest, ack.AcceptedThroughProducerSequence, ack.AssignedRunSequence, string(duplicatesJSON), string(ack.RunState), formatTime(command.Now))
	if err != nil {
		return event.Ack{}, store.classify(err)
	}
	return ack, nil
}

func (store *Store) loadAttempt(ctx context.Context, q migrate.Queryer, tenantID, attemptID string) (attemptView, error) {
	var view attemptView
	var state, lease, runState string
	err := q.QueryRowContext(ctx, store.query(`SELECT a.tenant_id,a.run_id,a.attempt_id,a.deployment_id,a.instance_id,a.session_id,a.service_id,a.generation,a.fencing_token,a.attempt_state,a.lease_expires_at,a.endpoint,r.state FROM arop_dispatch_attempts a JOIN arop_runs r ON r.tenant_id=a.tenant_id AND r.run_id=a.run_id WHERE a.tenant_id=? AND a.attempt_id=?`), tenantID, attemptID).Scan(&view.TenantID, &view.RunID, &view.AttemptID, &view.DeploymentID, &view.InstanceID, &view.SessionID, &view.ServiceID, &view.Generation, &view.FencingToken, &state, &lease, &view.Endpoint, &runState)
	if errors.Is(err, sql.ErrNoRows) {
		return attemptView{}, event.NewError(event.CategoryNotFound, event.ReasonAttemptNotFound)
	}
	if err != nil {
		return attemptView{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	view.State = dispatch.AttemptState(state)
	view.RunState = run.State(runState)
	view.LeaseExpiresAt, err = parseTime(lease)
	if err != nil {
		return attemptView{}, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	return view, nil
}

func (store *Store) requireActiveRuntime(ctx context.Context, q migrate.Queryer, view attemptView, now time.Time) error {
	var sessionID, serviceID, status, lease, endpoint string
	var generation uint64
	err := q.QueryRowContext(ctx, store.query(`SELECT session_id,service_id,generation,status,lease_expires_at,endpoint_base_url FROM arop_registry_instances WHERE tenant_id=? AND instance_id=?`), view.TenantID, view.InstanceID).Scan(&sessionID, &serviceID, &generation, &status, &lease, &endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	if err != nil {
		return event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	leaseExpiresAt, err := parseTime(lease)
	if err != nil {
		return event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	wantEndpoint := strings.TrimSuffix(endpoint, "/") + "/v1/runs"
	if sessionID != view.SessionID || serviceID != view.ServiceID || generation != view.Generation || status != "registered" || !now.Before(leaseExpiresAt) || wantEndpoint != view.Endpoint {
		return event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	return nil
}

func (store *Store) loadBatch(ctx context.Context, q migrate.Queryer, tenant, attempt, key string) (event.Ack, string, bool, error) {
	var ack event.Ack
	var duplicates, state, requestDigest string
	err := q.QueryRowContext(ctx, store.query(`SELECT request_digest,accepted_through_producer_sequence,assigned_run_sequence,duplicate_event_ids_json,run_state FROM arop_event_batches WHERE tenant_id=? AND attempt_id=? AND idempotency_key_digest=?`), tenant, attempt, key).Scan(&requestDigest, &ack.AcceptedThroughProducerSequence, &ack.AssignedRunSequence, &duplicates, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Ack{}, "", false, nil
	}
	if err != nil {
		return event.Ack{}, "", false, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	if json.Unmarshal([]byte(duplicates), &ack.DuplicateEventIDs) != nil {
		return event.Ack{}, "", false, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	ack.RunState = run.State(state)
	return ack, requestDigest, true, nil
}

func (store *Store) lastProducer(ctx context.Context, q migrate.Queryer, s event.Session) (uint64, error) {
	var value uint64
	err := q.QueryRowContext(ctx, store.query(`SELECT last_producer_sequence FROM arop_event_attempt_projections WHERE tenant_id=? AND attempt_id=?`), s.TenantID, s.AttemptID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_attempt_projections(tenant_id,run_id,attempt_id,last_producer_sequence,updated_at) VALUES(?,?,?,?,?)`), s.TenantID, s.RunID, s.AttemptID, 0, formatTime(s.CreatedAt))
		return 0, store.classify(err)
	}
	if err != nil {
		return 0, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	return value, nil
}
func (store *Store) runProjection(ctx context.Context, q migrate.Queryer, s event.Session) (uint64, bool, error) {
	var value uint64
	var terminal sql.NullString
	err := q.QueryRowContext(ctx, store.query(`SELECT last_run_sequence,terminal_state FROM arop_event_run_projections WHERE tenant_id=? AND run_id=?`), s.TenantID, s.RunID).Scan(&value, &terminal)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_run_projections(tenant_id,run_id,last_run_sequence,terminal_state,terminal_result_json,terminal_event_id,updated_at) VALUES(?,?,?,?,?,?,?)`), s.TenantID, s.RunID, 0, nil, nil, nil, formatTime(s.CreatedAt))
		return 0, false, store.classify(err)
	}
	if err != nil {
		return 0, false, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	return value, terminal.Valid, nil
}
func (store *Store) saveProjections(ctx context.Context, q migrate.Queryer, s event.Session, producer, runSequence uint64, now time.Time) error {
	if _, err := q.ExecContext(ctx, store.query(`UPDATE arop_event_attempt_projections SET last_producer_sequence=?,updated_at=? WHERE tenant_id=? AND attempt_id=?`), producer, formatTime(now), s.TenantID, s.AttemptID); err != nil {
		return store.classify(err)
	}
	if _, err := q.ExecContext(ctx, store.query(`UPDATE arop_event_run_projections SET last_run_sequence=?,updated_at=? WHERE tenant_id=? AND run_id=?`), runSequence, formatTime(now), s.TenantID, s.RunID); err != nil {
		return store.classify(err)
	}
	return nil
}

func (store *Store) applyState(ctx context.Context, q migrate.Queryer, s event.Session, state run.State, now time.Time) error {
	result, err := q.ExecContext(ctx, store.query(`UPDATE arop_runs SET state=?,state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND state_version<9007199254740991`), string(state), formatTime(now), s.TenantID, s.RunID)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	return nil
}
func (store *Store) applyUsage(ctx context.Context, q migrate.Queryer, s event.Session, usage run.Usage, now time.Time) error {
	result, err := q.ExecContext(ctx, store.query(`UPDATE arop_runs SET usage_input_tokens=?,usage_output_tokens=?,usage_duration_ms=?,usage_billable_units=?,state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND state_version<9007199254740991`), usage.InputTokens, usage.OutputTokens, usage.DurationMS, usage.BillableUnits, formatTime(now), s.TenantID, s.RunID)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	return nil
}
func (store *Store) applyTerminal(ctx context.Context, q migrate.Queryer, s event.Session, incoming event.Envelope, runSequence uint64, projection event.TerminalProjection, now time.Time) error {
	result, err := q.ExecContext(ctx, store.query(`UPDATE arop_runs SET state=?,usage_input_tokens=?,usage_output_tokens=?,usage_duration_ms=?,usage_billable_units=?,state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND state_version<9007199254740991`), string(projection.State), projection.Usage.InputTokens, projection.Usage.OutputTokens, projection.Usage.DurationMS, projection.Usage.BillableUnits, formatTime(now), s.TenantID, s.RunID)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return event.NewError(event.CategoryConflict, event.ReasonAttemptFenced)
	}
	if _, err = q.ExecContext(ctx, store.query(`UPDATE arop_event_run_projections SET terminal_state=?,terminal_result_json=?,terminal_event_id=?,updated_at=? WHERE tenant_id=? AND run_id=? AND terminal_state IS NULL`), string(projection.State), string(projection.Result), incoming.ID, formatTime(now), s.TenantID, s.RunID); err != nil {
		return store.classify(err)
	}
	// Deferred FK makes the capacity release and its terminal ledger append one atomic unit.
	if _, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_capacity_releases(tenant_id,attempt_id,run_id,run_sequence,terminal_event_id,released_at) VALUES(?,?,?,?,?,?)`), s.TenantID, s.AttemptID, s.RunID, runSequence, incoming.ID, formatTime(now)); err != nil {
		return store.classify(err)
	}
	return nil
}

func (store *Store) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := store.lookup(ctx)
	if !ok || tx == nil {
		return nil, event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
	}
	return tx, nil
}
func (store *Store) lock(ctx context.Context, tx *sql.Tx, keys ...string) error {
	if store.dialect == SQLite {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
		}
	}
	return nil
}
func (store *Store) query(value string) string {
	if store.dialect == SQLite {
		return value
	}
	var result strings.Builder
	index := 1
	for _, character := range value {
		if character == '?' {
			result.WriteString(fmt.Sprintf("$%d", index))
			index++
		} else {
			result.WriteRune(character)
		}
	}
	return result.String()
}
func (store *Store) classify(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "unique") || strings.Contains(text, "constraint") || strings.Contains(text, "duplicate") {
		return event.NewError(event.CategoryConflict, event.ReasonIdempotencyConflict)
	}
	return event.NewError(event.CategoryDependency, event.ReasonDependencyUnavailable)
}
func formatTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(value string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000000000Z", value)
}
func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
func minimumTime(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}
