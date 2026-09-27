// Package sqlite is the reference local durable provider adapter.
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/provider"
	_ "modernc.org/sqlite"
)

var verifySequence atomic.Uint64

// Store implements provider.DurableStore with SQLite transactions.
type Store struct {
	db        *sql.DB
	migration []byte
}

// Open creates or verifies a local provider database. Every existing path
// component must be real (not a symlink), preventing database redirection.
func Open(ctx context.Context, path string, migration []byte) (*Store, error) {
	if len(migration) == 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid provider database configuration")
	}
	if err := validatePath(path); err != nil {
		return nil, err
	}
	dsnURL := &url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys%281%29&_pragma=busy_timeout%285000%29&_txlock=immediate"}
	dsn := dsnURL.String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.New("open provider database")
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &Store{db: database, migration: bytes.Clone(migration)}
	if err = store.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error { return store.db.Close() }

func (store *Store) Within(ctx context.Context, callback func(context.Context, provider.Transaction) error) error {
	if callback == nil {
		return errors.New("nil provider transaction")
	}
	transaction, err := store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errors.New("begin provider transaction")
	}
	if err = callback(ctx, &tx{tx: transaction}); err != nil {
		_ = transaction.Rollback()
		return err
	}
	if err = transaction.Commit(); err != nil {
		return errors.New("commit provider transaction")
	}
	return nil
}

func (store *Store) GetInbox(ctx context.Context, runID, attemptID string) (provider.InboxRecord, error) {
	return loadInbox(ctx, store.db, runID, attemptID)
}

func (store *Store) ListRecoverable(ctx context.Context, now time.Time, limit int) ([]provider.InboxRecord, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT `+inboxColumns+` FROM provider_inbox WHERE state IN ('accepted','running','cancel_requested') ORDER BY updated_at,run_id,attempt_id LIMIT ?`, limit)
	if err != nil {
		return nil, errors.New("list recoverable provider attempts")
	}
	defer rows.Close()
	values := []provider.InboxRecord{}
	for rows.Next() {
		value, scanErr := scanInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		values = append(values, value)
	}
	if err = rows.Err(); err != nil {
		return nil, errors.New("read recoverable provider attempts")
	}
	return values, nil
}

func (store *Store) ListOutbox(ctx context.Context, runID, attemptID string, after uint64, limit int) ([]provider.OutboxRecord, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT run_id,attempt_id,sequence,event_id,event_type,envelope_json,created_at,delivered_at FROM provider_outbox WHERE run_id=? AND attempt_id=? AND sequence>? ORDER BY sequence LIMIT ?`, runID, attemptID, after, limit)
	if err != nil {
		return nil, errors.New("list provider outbox")
	}
	defer rows.Close()
	values := []provider.OutboxRecord{}
	for rows.Next() {
		var value provider.OutboxRecord
		var created string
		var delivered sql.NullString
		if err = rows.Scan(&value.RunID, &value.AttemptID, &value.Sequence, &value.EventID, &value.EventType, &value.Envelope, &created, &delivered); err != nil {
			return nil, errors.New("scan provider outbox")
		}
		if value.CreatedAt, err = parseTime(created); err != nil || !json.Valid(value.Envelope) {
			return nil, errors.New("invalid provider outbox row")
		}
		if delivered.Valid {
			parsed, parseErr := parseTime(delivered.String)
			if parseErr != nil {
				return nil, errors.New("invalid provider outbox delivery time")
			}
			value.DeliveredAt = &parsed
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (store *Store) Ready(ctx context.Context) error {
	if err := store.db.PingContext(ctx); err != nil {
		return errors.New("provider database unavailable")
	}
	return VerifySchema(ctx, store.db, store.migration)
}

type tx struct{ tx *sql.Tx }

func (transaction *tx) GetInbox(ctx context.Context, runID, attemptID string) (provider.InboxRecord, error) {
	return loadInbox(ctx, transaction.tx, runID, attemptID)
}

func (transaction *tx) CreateInbox(ctx context.Context, value provider.InboxRecord) error {
	if err := validateInbox(value); err != nil {
		return err
	}
	_, err := transaction.tx.ExecContext(ctx, `INSERT INTO provider_inbox(run_id,attempt_id,request_digest,authorization_digest,request_json,result_json,agent_id,agent_version,skill_id,deployment_id,instance_id,generation,fencing_token,state_version,traceparent,tracestate,state,deadline_at,created_at,updated_at,cancel_requested_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		value.RunID, value.AttemptID, value.RequestDigest, value.AuthorizationDigest, []byte(value.RequestJSON), nullableJSON(value.ResultJSON), value.AgentID, value.AgentVersion, value.SkillID, value.DeploymentID, value.InstanceID, value.Generation, value.FencingToken, value.StateVersion, value.Traceparent, nullableString(value.Tracestate), string(value.State), formatTime(value.DeadlineAt), formatTime(value.CreatedAt), formatTime(value.UpdatedAt), nullableTime(value.CancelRequestedAt))
	if err != nil {
		return classify(err)
	}
	return nil
}

func (transaction *tx) UpdateInbox(ctx context.Context, value provider.InboxRecord, expectedVersion uint64) error {
	if err := validateInbox(value); err != nil || expectedVersion == 0 || value.StateVersion != expectedVersion+1 {
		return provider.ErrConflict
	}
	result, err := transaction.tx.ExecContext(ctx, `UPDATE provider_inbox SET result_json=?,state_version=?,state=?,updated_at=?,cancel_requested_at=? WHERE run_id=? AND attempt_id=? AND state_version=?`, nullableJSON(value.ResultJSON), value.StateVersion, string(value.State), formatTime(value.UpdatedAt), nullableTime(value.CancelRequestedAt), value.RunID, value.AttemptID, expectedVersion)
	if err != nil {
		return classify(err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return provider.ErrConflict
	}
	return nil
}

func (transaction *tx) GetEffect(ctx context.Context, effectID string) (provider.EffectRecord, error) {
	var value provider.EffectRecord
	var state, started, updated string
	var result []byte
	err := transaction.tx.QueryRowContext(ctx, `SELECT effect_id,run_id,attempt_id,request_digest,state,result_json,started_at,updated_at FROM provider_effects WHERE effect_id=?`, effectID).Scan(&value.EffectID, &value.RunID, &value.AttemptID, &value.RequestDigest, &state, &result, &started, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return provider.EffectRecord{}, provider.ErrNotFound
	}
	if err != nil {
		return provider.EffectRecord{}, errors.New("load provider effect")
	}
	value.State = provider.EffectState(state)
	value.Result = bytes.Clone(result)
	if value.StartedAt, err = parseTime(started); err != nil {
		return provider.EffectRecord{}, errors.New("invalid provider effect time")
	}
	if value.UpdatedAt, err = parseTime(updated); err != nil {
		return provider.EffectRecord{}, errors.New("invalid provider effect time")
	}
	return value, nil
}

func (transaction *tx) CreateEffect(ctx context.Context, value provider.EffectRecord) error {
	if value.State != provider.EffectStarted || value.EffectID == "" || value.RunID == "" || value.AttemptID == "" || !digest(value.RequestDigest) || !utc(value.StartedAt) || !value.UpdatedAt.Equal(value.StartedAt) {
		return errors.New("invalid provider effect")
	}
	_, err := transaction.tx.ExecContext(ctx, `INSERT INTO provider_effects(effect_id,run_id,attempt_id,request_digest,state,result_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, value.EffectID, value.RunID, value.AttemptID, value.RequestDigest, string(value.State), nil, formatTime(value.StartedAt), formatTime(value.UpdatedAt))
	return classify(err)
}

func (transaction *tx) CompleteEffect(ctx context.Context, effectID, requestDigest string, resultJSON json.RawMessage, now time.Time) error {
	if effectID == "" || !digest(requestDigest) || !json.Valid(resultJSON) || !utc(now) {
		return errors.New("invalid provider effect completion")
	}
	result, err := transaction.tx.ExecContext(ctx, `UPDATE provider_effects SET state='completed',result_json=?,updated_at=? WHERE effect_id=? AND request_digest=? AND state='started'`, []byte(resultJSON), formatTime(now), effectID, requestDigest)
	if err != nil {
		return classify(err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return provider.ErrConflict
	}
	return nil
}

func (transaction *tx) AppendOutbox(ctx context.Context, value provider.OutboxRecord) (uint64, error) {
	if value.RunID == "" || value.AttemptID == "" || value.EventID == "" || value.EventType == "" || !json.Valid(value.Envelope) || !utc(value.CreatedAt) {
		return 0, errors.New("invalid provider outbox event")
	}
	var sequence uint64
	if err := transaction.tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0)+1 FROM provider_outbox WHERE run_id=? AND attempt_id=?`, value.RunID, value.AttemptID).Scan(&sequence); err != nil || sequence == 0 || sequence > 9007199254740991 {
		return 0, errors.New("allocate provider outbox sequence")
	}
	_, err := transaction.tx.ExecContext(ctx, `INSERT INTO provider_outbox(run_id,attempt_id,sequence,event_id,event_type,envelope_json,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`, value.RunID, value.AttemptID, sequence, value.EventID, value.EventType, []byte(value.Envelope), formatTime(value.CreatedAt), nullableTime(value.DeliveredAt))
	if err != nil {
		return 0, classify(err)
	}
	return sequence, nil
}

func (transaction *tx) MarkOutboxDelivered(ctx context.Context, runID, attemptID string, sequence uint64, delivered time.Time) error {
	if sequence == 0 || !utc(delivered) {
		return errors.New("invalid provider outbox acknowledgement")
	}
	result, err := transaction.tx.ExecContext(ctx, `UPDATE provider_outbox SET delivered_at=? WHERE run_id=? AND attempt_id=? AND sequence=? AND delivered_at IS NULL`, formatTime(delivered), runID, attemptID, sequence)
	if err != nil {
		return classify(err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return provider.ErrConflict
	}
	return nil
}

const inboxColumns = `run_id,attempt_id,request_digest,authorization_digest,request_json,result_json,agent_id,agent_version,skill_id,deployment_id,instance_id,generation,fencing_token,state_version,traceparent,tracestate,state,deadline_at,created_at,updated_at,cancel_requested_at`

type rowScanner interface{ Scan(...any) error }

func loadInbox(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, runID, attemptID string) (provider.InboxRecord, error) {
	return scanInbox(queryer.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM provider_inbox WHERE run_id=? AND attempt_id=?`, runID, attemptID))
}

func scanInbox(row rowScanner) (provider.InboxRecord, error) {
	var value provider.InboxRecord
	var request, result []byte
	var trace, state, deadline, created, updated string
	var cancel sql.NullString
	if err := row.Scan(&value.RunID, &value.AttemptID, &value.RequestDigest, &value.AuthorizationDigest, &request, &result, &value.AgentID, &value.AgentVersion, &value.SkillID, &value.DeploymentID, &value.InstanceID, &value.Generation, &value.FencingToken, &value.StateVersion, &value.Traceparent, &trace, &state, &deadline, &created, &updated, &cancel); errors.Is(err, sql.ErrNoRows) {
		return provider.InboxRecord{}, provider.ErrNotFound
	} else if err != nil {
		return provider.InboxRecord{}, errors.New("load provider inbox")
	}
	value.RequestJSON, value.ResultJSON, value.Tracestate, value.State = bytes.Clone(request), bytes.Clone(result), trace, provider.InboxState(state)
	var err error
	if value.DeadlineAt, err = parseTime(deadline); err != nil {
		return provider.InboxRecord{}, errors.New("invalid provider inbox deadline")
	}
	if value.CreatedAt, err = parseTime(created); err != nil {
		return provider.InboxRecord{}, errors.New("invalid provider inbox created time")
	}
	if value.UpdatedAt, err = parseTime(updated); err != nil {
		return provider.InboxRecord{}, errors.New("invalid provider inbox updated time")
	}
	if cancel.Valid {
		parsed, parseErr := parseTime(cancel.String)
		if parseErr != nil {
			return provider.InboxRecord{}, errors.New("invalid provider inbox cancel time")
		}
		value.CancelRequestedAt = &parsed
	}
	if err = validateInbox(value); err != nil {
		return provider.InboxRecord{}, errors.New("invalid provider inbox row")
	}
	return value, nil
}

func (store *Store) migrate(ctx context.Context) error {
	checksum := migrationDigest(store.migration)
	transaction, err := store.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return errors.New("begin provider migration")
	}
	defer transaction.Rollback()
	var history int
	if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='provider_schema_history'`).Scan(&history); err != nil {
		return errors.New("inspect provider migration history")
	}
	if history == 0 {
		var objects int
		if err = transaction.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects); err != nil || objects != 0 {
			return errors.New("provider database is not empty")
		}
		if _, err = transaction.ExecContext(ctx, string(store.migration)); err != nil {
			return errors.New("apply provider migration")
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO provider_schema_history(version,checksum,applied_at) VALUES(1,?,?)`, checksum, formatTime(time.Now().UTC())); err != nil {
			return errors.New("record provider migration")
		}
	} else if history == 1 {
		var count int
		var stored string
		if err = transaction.QueryRowContext(ctx, `SELECT count(*),coalesce(max(checksum),'') FROM provider_schema_history`).Scan(&count, &stored); err != nil || count != 1 || stored != checksum {
			return errors.New("provider migration history mismatch")
		}
	} else {
		return errors.New("provider migration history is ambiguous")
	}
	if err = transaction.Commit(); err != nil {
		return errors.New("commit provider migration")
	}
	return VerifySchema(ctx, store.db, store.migration)
}

// VerifySchema compares every user-defined SQLite object with an independently
// built clean schema and checks foreign-key and database integrity.
func VerifySchema(ctx context.Context, database *sql.DB, migration []byte) error {
	if database == nil || len(migration) == 0 {
		return errors.New("invalid provider schema verifier input")
	}
	want, err := expectedObjects(ctx, migration)
	if err != nil {
		return err
	}
	got, err := schemaObjects(ctx, database)
	if err != nil || !equalObjects(got, want) {
		return errors.New("provider database schema mismatch")
	}
	var integrity string
	if err = database.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("provider database integrity failure")
	}
	rows, err := database.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return errors.New("provider database foreign key check failed")
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		return errors.New("provider database foreign key violation")
	}
	return nil
}

type schemaObject struct{ Type, Name, Table, SQL string }

func expectedObjects(ctx context.Context, migration []byte) ([]schemaObject, error) {
	dsn := fmt.Sprintf("file:arop-provider-schema-%d?mode=memory&cache=private&_pragma=foreign_keys(1)", verifySequence.Add(1))
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.New("open provider schema oracle")
	}
	defer database.Close()
	if _, err = database.ExecContext(ctx, string(migration)); err != nil {
		return nil, errors.New("apply provider schema oracle")
	}
	if _, err = database.ExecContext(ctx, `INSERT INTO provider_schema_history(version,checksum,applied_at) VALUES(1,?,?)`, migrationDigest(migration), formatTime(time.Unix(1, 0).UTC())); err != nil {
		return nil, errors.New("seed provider schema oracle")
	}
	return schemaObjects(ctx, database)
}

func schemaObjects(ctx context.Context, database *sql.DB) ([]schemaObject, error) {
	rows, err := database.QueryContext(ctx, `SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name`)
	if err != nil {
		return nil, errors.New("inspect provider schema")
	}
	defer rows.Close()
	values := []schemaObject{}
	for rows.Next() {
		var value schemaObject
		if err = rows.Scan(&value.Type, &value.Name, &value.Table, &value.SQL); err != nil {
			return nil, errors.New("scan provider schema")
		}
		value.SQL = normalizeSQL(value.SQL)
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Type+values[i].Name < values[j].Type+values[j].Name })
	return values, rows.Err()
}

func equalObjects(left, right []schemaObject) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func normalizeSQL(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func validatePath(path string) error {
	current := filepath.Dir(path)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			return errors.New("provider database parent is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("provider database path contains a symlink")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("provider database path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect provider database path")
	}
	return nil
}

func validateInbox(value provider.InboxRecord) error {
	if value.RunID == "" || value.AttemptID == "" || !digest(value.RequestDigest) || !digest(value.AuthorizationDigest) || !json.Valid(value.RequestJSON) || len(value.ResultJSON) != 0 && !json.Valid(value.ResultJSON) || value.AgentID == "" || value.AgentVersion == "" || value.SkillID == "" || value.DeploymentID == "" || value.InstanceID == "" || value.Generation == 0 || value.FencingToken == 0 || value.StateVersion == 0 || value.StateVersion > 9007199254740991 || !utc(value.DeadlineAt) || !utc(value.CreatedAt) || !utc(value.UpdatedAt) {
		return errors.New("invalid provider inbox")
	}
	switch value.State {
	case provider.InboxAccepted, provider.InboxRunning, provider.InboxCancelRequested, provider.InboxSucceeded, provider.InboxFailed, provider.InboxCancelled, provider.InboxTimedOut:
	default:
		return errors.New("invalid provider inbox state")
	}
	if value.State.Terminal() != (len(value.ResultJSON) != 0) {
		return errors.New("provider inbox terminal result mismatch")
	}
	return nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "unique constraint") || strings.Contains(text, "primary key") {
		return provider.ErrConflict
	}
	return errors.New("provider database operation failed")
}

func migrationDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func digest(value string) bool          { return len(value) == 71 && strings.HasPrefix(value, "sha256:") }
func utc(value time.Time) bool          { return !value.IsZero() && value.Equal(value.UTC()) }
func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !parsed.Equal(parsed.UTC()) {
		return time.Time{}, errors.New("invalid UTC timestamp")
	}
	return parsed, nil
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return []byte(value)
}
func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

var _ provider.DurableStore = (*Store)(nil)
var _ provider.Transaction = (*tx)(nil)
