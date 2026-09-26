package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
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
	issuer  string
}

func New(db *sql.DB, lookup durable.TransactionLookup, dialect Dialect, issuer string) (*Store, error) {
	parsed, err := url.Parse(issuer)
	if db == nil || lookup == nil || dialect != SQLite && dialect != Postgres || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, errors.New("dispatch store dependencies are required")
	}
	return &Store{db: db, lookup: lookup, dialect: dialect, issuer: strings.TrimSuffix(issuer, "/")}, nil
}

func (store *Store) DispatchableRun(ctx context.Context, tenantID, runID string) (dispatch.RunView, error) {
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	return store.loadRun(ctx, queryer, tenantID, runID)
}

func (store *Store) GetByIdempotency(ctx context.Context, tenantID, keyDigest string) (dispatch.Attempt, string, error) {
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	var attemptID, requestDigest string
	err := queryer.QueryRowContext(ctx, store.query(`SELECT attempt_id,request_digest FROM arop_dispatch_idempotency WHERE tenant_id=? AND key_digest=?`), tenantID, keyDigest).Scan(&attemptID, &requestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Attempt{}, "", dispatch.NewError(dispatch.CategoryNotFound, dispatch.ReasonRunNotFound)
	}
	if err != nil {
		return dispatch.Attempt{}, "", dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	attempt, err := store.loadAttempt(ctx, queryer, tenantID, attemptID)
	return attempt, requestDigest, err
}

func (store *Store) Reserve(ctx context.Context, command dispatch.ReserveCommand) (dispatch.Attempt, bool, error) {
	if command.Run.Validate() != nil || command.Candidate.Validate(command.Now) != nil || !validHex(command.KeyDigest, 64) || !strings.HasPrefix(command.RequestDigest, "sha256:") || !utc(command.Now) || !utc(command.LeaseExpires) || !utc(command.TicketExpires) || command.TicketExpires.After(command.LeaseExpires) || !strings.HasPrefix(command.AttemptID, "att_") || !strings.HasPrefix(command.TokenID, "tok_") || !strings.HasPrefix(command.DeploymentID, "dep_") || command.SigningKey.Validate(5*time.Minute) != nil || len(command.SigningKeys) == 0 {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return dispatch.Attempt{}, false, err
	}
	if err = store.lock(ctx, tx, "dispatch/run/"+command.Run.TenantID+"/"+command.Run.RunID, "dispatch/service/"+command.Run.TenantID+"/"+command.Candidate.ServiceID, "dispatch/instance/"+command.Run.TenantID+"/"+command.Candidate.InstanceID); err != nil {
		return dispatch.Attempt{}, false, err
	}
	if existing, stored, lookupErr := store.GetByIdempotency(ctx, command.Run.TenantID, command.KeyDigest); lookupErr == nil {
		if stored != command.RequestDigest {
			return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonIdempotencyConflict)
		}
		return existing, true, nil
	} else if !isNotFound(lookupErr) {
		return dispatch.Attempt{}, false, lookupErr
	}
	current, err := store.loadRun(ctx, tx, command.Run.TenantID, command.Run.RunID)
	if err != nil {
		return dispatch.Attempt{}, false, err
	}
	if current.StateVersion != command.Run.StateVersion || current.Agent != command.Run.Agent || current.AuthorizationSnapshotHash != command.Run.AuthorizationSnapshotHash || !current.DeadlineAt.Equal(command.Run.DeadlineAt) {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonRunNotDispatchable)
	}
	if _, err = tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='expired',closed_at=? WHERE tenant_id=? AND run_id=? AND attempt_state IN ('issued','accepted') AND lease_expires_at<=?`), formatTime(command.Now), command.Run.TenantID, command.Run.RunID, formatTime(command.Now)); err != nil {
		return dispatch.Attempt{}, false, store.classify(err)
	}
	var active int
	if err = tx.QueryRowContext(ctx, store.query(`SELECT count(*) FROM arop_dispatch_attempts WHERE tenant_id=? AND run_id=? AND attempt_state IN ('issued','accepted')`), command.Run.TenantID, command.Run.RunID).Scan(&active); err != nil {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	if active != 0 {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonRunNotDispatchable)
	}
	if err = store.verifyCandidate(ctx, tx, command); err != nil {
		return dispatch.Attempt{}, false, err
	}
	deploymentID, err := store.deployment(ctx, tx, command)
	if err != nil {
		return dispatch.Attempt{}, false, err
	}
	if err = store.syncKeys(ctx, tx, command.SigningKeys, command.Now); err != nil {
		return dispatch.Attempt{}, false, err
	}
	var number uint64
	if err = tx.QueryRowContext(ctx, store.query(`SELECT coalesce(max(attempt_number),0)+1 FROM arop_dispatch_attempts WHERE tenant_id=? AND run_id=?`), command.Run.TenantID, command.Run.RunID).Scan(&number); err != nil || number == 0 || number > dispatch.MaxSafeInteger {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	attempt := dispatch.Attempt{TenantID: command.Run.TenantID, RunID: command.Run.RunID, AttemptID: command.AttemptID, TokenID: command.TokenID, AttemptNumber: number, FencingToken: number, DeploymentID: deploymentID, InstanceID: command.Candidate.InstanceID, SessionID: command.Candidate.SessionID, ServiceID: command.Candidate.ServiceID, Generation: command.Candidate.Generation, RegistryResourceVersion: command.Candidate.ResourceVersion, State: dispatch.StateIssued, TransportProfile: command.Candidate.TransportProfile, Endpoint: command.Candidate.InvocationEndpoint(), Audience: store.issuer + "/deployments/" + deploymentID, SigningKeyID: command.SigningKey.KeyID, LeaseExpiresAt: command.LeaseExpires, TicketExpiresAt: command.TicketExpires, CreatedAt: command.Now, Traceparent: command.Run.Traceparent, Tracestate: command.Run.Tracestate, IdempotencyKeyHash: command.KeyDigest, IdempotencyRequestHash: command.RequestDigest}
	if attempt.Validate() != nil {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_dispatch_attempts(tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), attempt.TenantID, attempt.RunID, attempt.AttemptID, attempt.AttemptNumber, attempt.FencingToken, attempt.TokenID, attempt.DeploymentID, attempt.InstanceID, attempt.SessionID, attempt.ServiceID, attempt.Generation, attempt.RegistryResourceVersion, string(attempt.State), attempt.TransportProfile, attempt.Endpoint, attempt.Audience, attempt.SigningKeyID, formatTime(attempt.LeaseExpiresAt), formatTime(attempt.TicketExpiresAt), formatTime(attempt.CreatedAt), nil, nil, nil, attempt.Traceparent, nullable(attempt.Tracestate), attempt.IdempotencyKeyHash, attempt.IdempotencyRequestHash)
	if err != nil {
		return dispatch.Attempt{}, false, store.classify(err)
	}
	if _, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_dispatch_idempotency(tenant_id,key_digest,request_digest,run_id,attempt_id,created_at) VALUES(?,?,?,?,?,?)`), attempt.TenantID, command.KeyDigest, command.RequestDigest, attempt.RunID, attempt.AttemptID, formatTime(command.Now)); err != nil {
		return dispatch.Attempt{}, false, store.classify(err)
	}
	result, err := tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='dispatching',state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state_version=? AND state IN ('queued','dispatching')`), formatTime(command.Now), command.Run.TenantID, command.Run.RunID, command.Run.StateVersion)
	if err != nil {
		return dispatch.Attempt{}, false, store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return dispatch.Attempt{}, false, dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonRunNotDispatchable)
	}
	return attempt, false, nil
}

func (store *Store) SyncKeys(ctx context.Context, keys []dispatch.KeyMetadata, now time.Time) error {
	tx, err := store.writeTx(ctx)
	if err != nil {
		return err
	}
	if !utc(now) {
		return dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	return store.syncKeys(ctx, tx, keys, now)
}

func (store *Store) Keys(ctx context.Context, now time.Time) ([]dispatch.KeyMetadata, error) {
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	rows, err := queryer.QueryContext(ctx, store.query(`SELECT key_id,key_status,public_x,public_y,not_before,sign_until,verify_until,created_at FROM arop_dispatch_keys WHERE verify_until>? ORDER BY key_id`), formatTime(now))
	if err != nil {
		return nil, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	defer rows.Close()
	var result []dispatch.KeyMetadata
	for rows.Next() {
		var key dispatch.KeyMetadata
		var notBefore, signUntil, verifyUntil, createdAt string
		if rows.Scan(&key.KeyID, &key.Status, &key.X, &key.Y, &notBefore, &signUntil, &verifyUntil, &createdAt) != nil {
			return nil, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
		}
		var parseErr error
		key.NotBefore, parseErr = parseTime(notBefore)
		if parseErr == nil {
			key.SignUntil, parseErr = parseTime(signUntil)
		}
		if parseErr == nil {
			key.VerifyUntil, parseErr = parseTime(verifyUntil)
		}
		if parseErr == nil {
			key.CreatedAt, parseErr = parseTime(createdAt)
		}
		if parseErr != nil {
			return nil, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
		}
		result = append(result, key)
	}
	if rows.Err() != nil {
		return nil, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	return result, nil
}

func (store *Store) verifyCandidate(ctx context.Context, tx *sql.Tx, command dispatch.ReserveCommand) error {
	var sessionID, serviceID, leaseExpires, endpoint, bindingsJSON, runtimeJSON, operatorJSON, status string
	var generation, resourceVersion uint64
	var draining bool
	err := tx.QueryRowContext(ctx, store.query(`SELECT session_id,service_id,generation,resource_version,lease_expires_at,endpoint_base_url,bindings_json,runtime_json,operator_json,draining,status FROM arop_registry_instances WHERE tenant_id=? AND instance_id=?`), command.Run.TenantID, command.Candidate.InstanceID).Scan(&sessionID, &serviceID, &generation, &resourceVersion, &leaseExpires, &endpoint, &bindingsJSON, &runtimeJSON, &operatorJSON, &draining, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.NewError(dispatch.CategoryCapacity, dispatch.ReasonNoCapacity)
	}
	if err != nil {
		return dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	lease, parseErr := parseTime(leaseExpires)
	var runtime struct {
		Healthy           bool     `json:"healthy"`
		Ready             bool     `json:"ready"`
		TransportProfiles []string `json:"transport_profiles"`
		Capacity          struct {
			AvailableSlots uint64 `json:"available_slots"`
		} `json:"capacity"`
	}
	var operator struct {
		Enabled  bool   `json:"enabled"`
		Weight   uint64 `json:"weight"`
		Priority uint64 `json:"priority"`
	}
	var bindings []struct {
		AgentID        string   `json:"agent_id"`
		AgentVersion   string   `json:"agent_version"`
		SkillIDs       []string `json:"skill_ids"`
		ManifestDigest string   `json:"manifest_digest"`
	}
	if parseErr != nil || json.Unmarshal([]byte(bindingsJSON), &bindings) != nil || json.Unmarshal([]byte(runtimeJSON), &runtime) != nil || json.Unmarshal([]byte(operatorJSON), &operator) != nil || sessionID != command.Candidate.SessionID || serviceID != command.Candidate.ServiceID || generation != command.Candidate.Generation || resourceVersion != command.Candidate.ResourceVersion || endpoint != command.Candidate.Endpoint || status != "registered" || draining || !runtime.Healthy || !runtime.Ready || !operator.Enabled || operator.Priority != command.Candidate.Priority || operator.Weight != command.Candidate.Weight || runtime.Capacity.AvailableSlots == 0 || !slices.Contains(runtime.TransportProfiles, command.Candidate.TransportProfile) || !lease.After(command.Now) || !hasBinding(bindings, command.Run) {
		return dispatch.NewError(dispatch.CategoryCapacity, dispatch.ReasonNoCapacity)
	}
	var reserved uint64
	if err = tx.QueryRowContext(ctx, store.query(`SELECT count(*) FROM arop_dispatch_attempts WHERE tenant_id=? AND instance_id=? AND session_id=? AND generation=? AND attempt_state IN ('issued','accepted') AND lease_expires_at>?`), command.Run.TenantID, command.Candidate.InstanceID, command.Candidate.SessionID, command.Candidate.Generation, formatTime(command.Now)).Scan(&reserved); err != nil {
		return dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	if reserved >= runtime.Capacity.AvailableSlots {
		return dispatch.NewError(dispatch.CategoryCapacity, dispatch.ReasonNoCapacity)
	}
	return nil
}

func hasBinding(bindings []struct {
	AgentID        string   `json:"agent_id"`
	AgentVersion   string   `json:"agent_version"`
	SkillIDs       []string `json:"skill_ids"`
	ManifestDigest string   `json:"manifest_digest"`
}, view dispatch.RunView) bool {
	for _, binding := range bindings {
		if binding.AgentID == view.Agent.ID && binding.AgentVersion == view.Agent.Version && binding.ManifestDigest == view.Agent.ManifestDigest && slices.Contains(binding.SkillIDs, view.Agent.SkillID) {
			return true
		}
	}
	return false
}

func (store *Store) deployment(ctx context.Context, tx *sql.Tx, command dispatch.ReserveCommand) (string, error) {
	var existing string
	err := tx.QueryRowContext(ctx, store.query(`SELECT deployment_id FROM arop_dispatch_deployments WHERE tenant_id=? AND service_id=?`), command.Run.TenantID, command.Candidate.ServiceID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_dispatch_deployments(tenant_id,service_id,deployment_id,created_at,updated_at) VALUES(?,?,?,?,?)`), command.Run.TenantID, command.Candidate.ServiceID, command.DeploymentID, formatTime(command.Now), formatTime(command.Now))
	if err != nil {
		return "", store.classify(err)
	}
	return command.DeploymentID, nil
}

func (store *Store) syncKeys(ctx context.Context, tx *sql.Tx, keys []dispatch.KeyMetadata, now time.Time) error {
	keys = append([]dispatch.KeyMetadata(nil), keys...)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Status != keys[j].Status {
			return keys[i].Status == dispatch.KeyRetiring
		}
		return keys[i].KeyID < keys[j].KeyID
	})
	active, activeKeyID := 0, ""
	for _, key := range keys {
		if key.Validate(5*time.Minute) != nil || !key.VerifyUntil.After(now) {
			return dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
		}
		if key.Status == dispatch.KeyActive {
			active++
			activeKeyID = key.KeyID
		}
	}
	if active != 1 {
		return dispatch.NewError(dispatch.CategoryValidation, dispatch.ReasonInvalidRequest)
	}
	minimumOverlap := formatTime(now.Add(10 * time.Minute))
	if _, err := tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_keys SET key_status='retiring',sign_until=?,verify_until=CASE WHEN verify_until<? THEN ? ELSE verify_until END,updated_at=? WHERE key_status='active' AND key_id<>?`), formatTime(now), minimumOverlap, minimumOverlap, formatTime(now), activeKeyID); err != nil {
		return store.classify(err)
	}
	for _, key := range keys {
		var x, y string
		err := tx.QueryRowContext(ctx, store.query(`SELECT public_x,public_y FROM arop_dispatch_keys WHERE key_id=?`), key.KeyID).Scan(&x, &y)
		if err == nil && (x != key.X || y != key.Y) {
			return dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonTicketInvalid)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
		}
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_dispatch_keys(key_id,key_status,public_x,public_y,not_before,sign_until,verify_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`), key.KeyID, string(key.Status), key.X, key.Y, formatTime(key.NotBefore), formatTime(key.SignUntil), formatTime(key.VerifyUntil), formatTime(key.CreatedAt), formatTime(now))
		} else {
			_, err = tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_keys SET key_status=?,not_before=?,sign_until=?,verify_until=?,updated_at=? WHERE key_id=?`), string(key.Status), formatTime(key.NotBefore), formatTime(key.SignUntil), formatTime(key.VerifyUntil), formatTime(now), key.KeyID)
		}
		if err != nil {
			return store.classify(err)
		}
	}
	return nil
}

func (store *Store) loadRun(ctx context.Context, queryer migrate.Queryer, tenantID, runID string) (dispatch.RunView, error) {
	var value dispatch.RunView
	var state, deadline string
	err := queryer.QueryRowContext(ctx, store.query(`SELECT tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,state,state_version,authorization_snapshot_digest,traceparent,coalesce(tracestate,''),deadline_at FROM arop_runs WHERE tenant_id=? AND run_id=?`), tenantID, runID).Scan(&value.TenantID, &value.RunID, &value.Agent.ID, &value.Agent.Version, &value.Agent.SkillID, &value.Agent.ManifestDigest, &state, &value.StateVersion, &value.AuthorizationSnapshotHash, &value.Traceparent, &value.Tracestate, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.RunView{}, dispatch.NewError(dispatch.CategoryNotFound, dispatch.ReasonRunNotFound)
	}
	if err != nil {
		return dispatch.RunView{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	value.State = run.State(state)
	value.DeadlineAt, err = parseTime(deadline)
	if err != nil || value.Validate() != nil {
		return dispatch.RunView{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	return value, nil
}

func (store *Store) loadAttempt(ctx context.Context, queryer migrate.Queryer, tenantID, attemptID string) (dispatch.Attempt, error) {
	var value dispatch.Attempt
	var state string
	var lease, ticket, created string
	var accepted, closed, failure, tracestate sql.NullString
	err := queryer.QueryRowContext(ctx, store.query(`SELECT tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest FROM arop_dispatch_attempts WHERE tenant_id=? AND attempt_id=?`), tenantID, attemptID).Scan(&value.TenantID, &value.RunID, &value.AttemptID, &value.AttemptNumber, &value.FencingToken, &value.TokenID, &value.DeploymentID, &value.InstanceID, &value.SessionID, &value.ServiceID, &value.Generation, &value.RegistryResourceVersion, &state, &value.TransportProfile, &value.Endpoint, &value.Audience, &value.SigningKeyID, &lease, &ticket, &created, &accepted, &closed, &failure, &value.Traceparent, &tracestate, &value.IdempotencyKeyHash, &value.IdempotencyRequestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return dispatch.Attempt{}, dispatch.NewError(dispatch.CategoryNotFound, dispatch.ReasonRunNotFound)
	}
	if err != nil {
		return dispatch.Attempt{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	value.State = dispatch.AttemptState(state)
	var parseErr error
	value.LeaseExpiresAt, parseErr = parseTime(lease)
	if parseErr == nil {
		value.TicketExpiresAt, parseErr = parseTime(ticket)
	}
	if parseErr == nil {
		value.CreatedAt, parseErr = parseTime(created)
	}
	if accepted.Valid && parseErr == nil {
		parsed, err := parseTime(accepted.String)
		value.AcceptedAt, parseErr = &parsed, err
	}
	if closed.Valid && parseErr == nil {
		parsed, err := parseTime(closed.String)
		value.ClosedAt, parseErr = &parsed, err
	}
	if parseErr != nil {
		return dispatch.Attempt{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	value.FailureCode, value.Tracestate = failure.String, tracestate.String
	if value.Validate() != nil {
		return dispatch.Attempt{}, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
	}
	return value, nil
}

func (store *Store) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := store.lookup(ctx)
	if !ok || tx == nil {
		return nil, dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
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
			return dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
		}
	}
	return nil
}

func (store *Store) query(value string) string {
	if store.dialect == SQLite {
		return value
	}
	result, index := strings.Builder{}, 1
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
		return dispatch.NewError(dispatch.CategoryConflict, dispatch.ReasonIdempotencyConflict)
	}
	return dispatch.NewError(dispatch.CategoryDependency, dispatch.ReasonDependencyUnavailable)
}

func formatTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(value string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000000000Z", value)
}
func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}
func isNotFound(err error) bool {
	failure, ok := dispatch.AsError(err)
	return ok && failure.Category == dispatch.CategoryNotFound
}
