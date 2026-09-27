package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Store struct {
	db           *sql.DB
	lookup       durable.TransactionLookup
	dialect      Dialect
	eventResults bool
}

func New(db *sql.DB, lookup durable.TransactionLookup, dialect Dialect) (*Store, error) {
	if db == nil || lookup == nil || dialect != SQLite && dialect != Postgres {
		return nil, errors.New("run store dependencies are required")
	}
	return &Store{db: db, lookup: lookup, dialect: dialect}, nil
}

func (store *Store) EnableEventResults() { store.eventResults = true }

func (store *Store) Create(ctx context.Context, record run.Run, keyDigest, requestDigest string, outbox run.Outbox) error {
	if record.Validate() != nil || outbox.Validate() != nil || record.RunID != outbox.RunID || len(keyDigest) != 64 {
		return run.NewError(run.CategoryValidation, run.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return err
	}
	if err = store.lock(ctx, tx, "run/idempotency/"+record.TenantID+"/"+keyDigest, "run/id/"+record.TenantID+"/"+record.RunID); err != nil {
		return err
	}
	if _, _, found, err := store.idempotency(ctx, tx, record.TenantID, keyDigest); err != nil {
		return err
	} else if found {
		return run.NewError(run.CategoryConflict, run.ReasonIdempotencyConflict)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_runs(tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,input_json,labels_json,conversation_ref,effect_level,effect_id,state,state_version,authorization_snapshot_json,authorization_snapshot_digest,traceparent,tracestate,deadline_at,created_at,updated_at,cancel_requested_at,usage_input_tokens,usage_output_tokens,usage_duration_ms,usage_billable_units) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), record.TenantID, record.RunID, record.Agent.ID, record.Agent.Version, record.Agent.SkillID, record.Agent.ManifestDigest, string(record.Input), labelsJSON(record.Labels), nullable(record.ConversationRef), string(record.Effects.Level), nullable(record.Effects.EffectID), string(record.State), record.StateVersion, string(record.AuthorizationSnapshot), record.AuthorizationSnapshotDigest, record.Traceparent, nullable(record.Tracestate), formatTime(record.DeadlineAt), formatTime(record.CreatedAt), formatTime(record.UpdatedAt), nil, record.Usage.InputTokens, record.Usage.OutputTokens, record.Usage.DurationMS, record.Usage.BillableUnits)
	if err != nil {
		return store.classify(err)
	}
	if _, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_run_idempotency(tenant_id,key_digest,request_digest,run_id,created_at) VALUES(?,?,?,?,?)`), record.TenantID, keyDigest, requestDigest, record.RunID, formatTime(record.CreatedAt)); err != nil {
		return store.classify(err)
	}
	return store.insertOutbox(ctx, tx, outbox)
}

func (store *Store) Get(ctx context.Context, tenantID, runID string) (run.Run, error) {
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	return store.load(ctx, queryer, tenantID, runID)
}
func (store *Store) GetByIdempotency(ctx context.Context, tenantID, keyDigest string) (run.Run, string, error) {
	queryer := migrate.Queryer(store.db)
	if tx, ok := store.lookup(ctx); ok {
		queryer = tx
	}
	var runID, requestDigest string
	err := queryer.QueryRowContext(ctx, store.query(`SELECT run_id,request_digest FROM arop_run_idempotency WHERE tenant_id=? AND key_digest=?`), tenantID, keyDigest).Scan(&runID, &requestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, "", run.NewError(run.CategoryNotFound, run.ReasonRunNotFound)
	}
	if err != nil {
		return run.Run{}, "", run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	record, err := store.load(ctx, queryer, tenantID, runID)
	return record, requestDigest, err
}

func (store *Store) Cancel(ctx context.Context, tenantID, runID string, command run.Command, keyDigest, commandDigest string, now time.Time, outboxFactory run.OutboxFactory) (run.Run, error) {
	tx, err := store.writeTx(ctx)
	if err != nil {
		return run.Run{}, err
	}
	if err = store.lock(ctx, tx, "run/id/"+tenantID+"/"+runID, "run/command/"+tenantID+"/"+runID+"/"+keyDigest); err != nil {
		return run.Run{}, err
	}
	var storedDigest string
	err = tx.QueryRowContext(ctx, store.query(`SELECT command_digest FROM arop_run_commands WHERE tenant_id=? AND run_id=? AND key_digest=?`), tenantID, runID, keyDigest).Scan(&storedDigest)
	if err == nil {
		if storedDigest != commandDigest {
			return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonIdempotencyConflict)
		}
		return store.load(ctx, tx, tenantID, runID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	record, err := store.load(ctx, tx, tenantID, runID)
	if err != nil {
		return run.Run{}, err
	}
	if record.StateVersion != command.ExpectedStateVersion {
		return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonStateVersionConflict)
	}
	if record.State.Terminal() {
		return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonTerminalStateConflict)
	}
	next := record.StateVersion + 1
	if next > run.MaxSafeInteger {
		return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonStateVersionConflict)
	}
	outbox, err := outboxFactory(ctx, next)
	if err != nil {
		return run.Run{}, err
	}
	_, err = tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='cancel_requested',state_version=?,updated_at=?,cancel_requested_at=? WHERE tenant_id=? AND run_id=? AND state_version=?`), next, formatTime(now), formatTime(now), tenantID, runID, record.StateVersion)
	if err != nil {
		return run.Run{}, store.classify(err)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_run_commands(tenant_id,run_id,command_id,key_digest,command_digest,result_state_version,created_at) VALUES(?,?,?,?,?,?,?)`), tenantID, runID, command.CommandID, keyDigest, commandDigest, next, formatTime(now))
	if err != nil {
		return run.Run{}, store.classify(err)
	}
	if err = store.insertOutbox(ctx, tx, outbox); err != nil {
		return run.Run{}, err
	}
	return store.load(ctx, tx, tenantID, runID)
}

func (store *Store) Expire(ctx context.Context, tenantID, runID string, expected uint64, now time.Time, outbox run.Outbox) (run.Run, error) {
	tx, err := store.writeTx(ctx)
	if err != nil {
		return run.Run{}, err
	}
	if err = store.lock(ctx, tx, "run/id/"+tenantID+"/"+runID); err != nil {
		return run.Run{}, err
	}
	record, err := store.load(ctx, tx, tenantID, runID)
	if err != nil {
		return run.Run{}, err
	}
	if record.State.Terminal() {
		return record, nil
	}
	if record.StateVersion != expected {
		return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonStateVersionConflict)
	}
	next := expected + 1
	if next > run.MaxSafeInteger {
		return run.Run{}, run.NewError(run.CategoryConflict, run.ReasonStateVersionConflict)
	}
	_, err = tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='timed_out',state_version=?,updated_at=? WHERE tenant_id=? AND run_id=? AND state_version=?`), next, formatTime(now), tenantID, runID, expected)
	if err != nil {
		return run.Run{}, store.classify(err)
	}
	outbox.StateVersion = next
	if err = store.insertOutbox(ctx, tx, outbox); err != nil {
		return run.Run{}, err
	}
	return store.load(ctx, tx, tenantID, runID)
}

func (store *Store) ReserveEffect(ctx context.Context, reservation run.EffectReservation) (bool, error) {
	tx, err := store.writeTx(ctx)
	if err != nil {
		return false, err
	}
	if err = store.lock(ctx, tx, "run/effect/"+reservation.TenantID+"/"+reservation.EffectID); err != nil {
		return false, err
	}
	var runID, digest string
	err = tx.QueryRowContext(ctx, store.query(`SELECT run_id,semantic_digest FROM arop_run_effects WHERE tenant_id=? AND effect_id=?`), reservation.TenantID, reservation.EffectID).Scan(&runID, &digest)
	if err == nil {
		if runID == reservation.RunID && digest == reservation.SemanticDigest {
			return true, nil
		}
		return false, run.NewError(run.CategoryConflict, run.ReasonEffectConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_run_effects(tenant_id,run_id,effect_id,semantic_digest,created_at) VALUES(?,?,?,?,?)`), reservation.TenantID, reservation.RunID, reservation.EffectID, reservation.SemanticDigest, formatTime(reservation.CreatedAt))
	if err != nil {
		return false, store.classify(err)
	}
	return false, nil
}

func (store *Store) load(ctx context.Context, queryer migrate.Queryer, tenantID, runID string) (run.Run, error) {
	var record run.Run
	var input, labels, auth string
	var conversation, effectID, tracestate, cancel sql.NullString
	var state, effectLevel, deadline, created, updated string
	statement := `SELECT r.tenant_id,r.run_id,r.agent_id,r.agent_version,r.skill_id,r.manifest_digest,r.input_json,r.labels_json,r.conversation_ref,r.effect_level,r.effect_id,r.state,r.state_version,r.authorization_snapshot_json,r.authorization_snapshot_digest,r.traceparent,r.tracestate,r.deadline_at,r.created_at,r.updated_at,r.cancel_requested_at,r.usage_input_tokens,r.usage_output_tokens,r.usage_duration_ms,r.usage_billable_units FROM arop_runs r WHERE r.tenant_id=? AND r.run_id=?`
	destinations := []any{&record.TenantID, &record.RunID, &record.Agent.ID, &record.Agent.Version, &record.Agent.SkillID, &record.Agent.ManifestDigest, &input, &labels, &conversation, &effectLevel, &effectID, &state, &record.StateVersion, &auth, &record.AuthorizationSnapshotDigest, &record.Traceparent, &tracestate, &deadline, &created, &updated, &cancel, &record.Usage.InputTokens, &record.Usage.OutputTokens, &record.Usage.DurationMS, &record.Usage.BillableUnits}
	var result sql.NullString
	if store.eventResults {
		statement = `SELECT r.tenant_id,r.run_id,r.agent_id,r.agent_version,r.skill_id,r.manifest_digest,r.input_json,r.labels_json,r.conversation_ref,r.effect_level,r.effect_id,r.state,r.state_version,r.authorization_snapshot_json,r.authorization_snapshot_digest,r.traceparent,r.tracestate,r.deadline_at,r.created_at,r.updated_at,r.cancel_requested_at,r.usage_input_tokens,r.usage_output_tokens,r.usage_duration_ms,r.usage_billable_units,p.terminal_result_json FROM arop_runs r LEFT JOIN arop_event_run_projections p ON p.tenant_id=r.tenant_id AND p.run_id=r.run_id WHERE r.tenant_id=? AND r.run_id=?`
		destinations = append(destinations, &result)
	}
	err := queryer.QueryRowContext(ctx, store.query(statement), tenantID, runID).Scan(destinations...)
	if errors.Is(err, sql.ErrNoRows) {
		return run.Run{}, run.NewError(run.CategoryNotFound, run.ReasonRunNotFound)
	}
	if err != nil {
		return run.Run{}, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	record.Input = json.RawMessage(input)
	record.Labels = json.RawMessage(labels)
	record.AuthorizationSnapshot = json.RawMessage(auth)
	if result.Valid {
		record.Result = json.RawMessage(result.String)
	}
	record.ConversationRef = conversation.String
	record.Tracestate = tracestate.String
	record.Effects = run.EffectIntent{Level: run.EffectLevel(effectLevel), EffectID: effectID.String}
	record.State = run.State(state)
	record.DeadlineAt, err = parseTime(deadline)
	if err != nil {
		return run.Run{}, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	record.CreatedAt, _ = parseTime(created)
	record.UpdatedAt, _ = parseTime(updated)
	if cancel.Valid {
		value, parseErr := parseTime(cancel.String)
		if parseErr != nil {
			return run.Run{}, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
		}
		record.CancelRequestedAt = &value
	}
	if record.Validate() != nil {
		return run.Run{}, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	return record, nil
}
func (store *Store) idempotency(ctx context.Context, tx *sql.Tx, tenant, key string) (string, string, bool, error) {
	var request, runID string
	err := tx.QueryRowContext(ctx, store.query(`SELECT request_digest,run_id FROM arop_run_idempotency WHERE tenant_id=? AND key_digest=?`), tenant, key).Scan(&request, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	return request, runID, true, nil
}
func (store *Store) insertOutbox(ctx context.Context, tx *sql.Tx, outbox run.Outbox) error {
	if outbox.Validate() != nil {
		return run.NewError(run.CategoryValidation, run.ReasonInvalidRequest)
	}
	_, err := tx.ExecContext(ctx, store.query(`INSERT INTO arop_run_outbox(outbox_id,tenant_id,run_id,event_kind,state_version,payload_json,created_at,published_at) VALUES(?,?,?,?,?,?,?,NULL)`), outbox.OutboxID, outbox.TenantID, outbox.RunID, outbox.Kind, outbox.StateVersion, string(outbox.Payload), formatTime(outbox.CreatedAt))
	return store.classify(err)
}
func (store *Store) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := store.lookup(ctx)
	if !ok {
		return nil, run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
	}
	return tx, nil
}
func (store *Store) lock(ctx context.Context, tx *sql.Tx, keys ...string) error {
	if store.dialect == SQLite {
		return nil
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
		}
	}
	return nil
}
func (store *Store) query(value string) string {
	if store.dialect == SQLite {
		return value
	}
	number := 0
	mapped := strings.Map(func(r rune) rune {
		if r != '?' {
			return r
		}
		number++
		return rune(0xE000 + number)
	}, value)
	for index := 1; index <= number; index++ {
		mapped = strings.ReplaceAll(mapped, string(rune(0xE000+index)), fmt.Sprintf("$%d", index))
	}
	return mapped
}
func (store *Store) classify(err error) error {
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") {
		return run.NewError(run.CategoryConflict, run.ReasonIdempotencyConflict)
	}
	return run.NewError(run.CategoryDependency, run.ReasonDependencyUnavailable)
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func labelsJSON(value json.RawMessage) string {
	if len(value) == 0 {
		return "{}"
	}
	return string(value)
}
func formatTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(value string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000000000Z", value)
}
