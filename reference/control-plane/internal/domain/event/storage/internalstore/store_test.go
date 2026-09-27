package internalstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	_ "modernc.org/sqlite"
)

const (
	testTrace  = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	testIssuer = "https://control.example.invalid"
)

var (
	testNonce      = fmt.Sprintf("%012x", uint64(time.Now().UnixNano())&0xffffffffffff)
	testTenant     = "tenant-p20-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	testRun        = "run_01932f13-0cd2-7a82-8fa3-" + testNonce
	testAttempt    = "att_01932f13-0cd2-7a82-8fa3-" + testNonce
	testDeployment = "dep_01932f13-0cd2-7a82-8fa3-" + testNonce
	testSession    = "ses_01932f13-0cd2-7a82-8fa3-" + testNonce
	testPrincipal  = "prn_01932f13-0cd2-7a82-8fa3-" + testNonce
	testCredential = "cred_01932f13-0cd2-7a82-8fa3-" + testNonce
	testLeaseID    = "lease_01932f13-0cd2-7a82-8fa3-" + testNonce
	testTokenID    = "tok_01932f13-0cd2-7a82-8fa3-" + testNonce
)

func TestSQLiteEventLedgerTerminalIsAtomicAndImmutable(t *testing.T) {
	db, uow, store, _, now := eventDatabase(t)
	if err := VerifySchema(SQLite)(context.Background(), db); err != nil {
		t.Fatalf("verify event schema: %v", err)
	}
	request := event.SessionRequest{TenantID: testTenant, RunID: testRun, AttemptID: testAttempt, DeploymentID: testDeployment, Generation: 7, FencingToken: 1}
	var session event.Session
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var err error
		session, err = store.CreateSession(ctx, event.SessionCommand{Request: request, Token: "evtcap_" + strings.Repeat("A", 43), TokenDigest: event.TokenDigest("evtcap_" + strings.Repeat("A", 43)), Now: now, ExpiresAt: now.Add(3 * time.Minute), LeaseExpiresAt: now.Add(5 * time.Minute)})
		return err
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if session.Token == "" || session.ExpiresAt.After(now.Add(3*time.Minute)) {
		t.Fatalf("invalid session: %+v", session)
	}
	wrongSource := eventValue(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef20", "io.kinglucky.arop.run.started.v1", `{"state":"started"}`, now)
	wrongSource.Source = "https://other-runtime.example.invalid/instances/instance-a"
	wrongBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef29", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "wrong-source", Events: []event.Envelope{wrongSource}}
	err := uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: wrongBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("9", 64), RequestDigest: event.BatchDigest(wrongBatch), Now: now.Add(time.Second)})
		return inner
	})
	if failure, ok := event.AsError(err); !ok || failure.Category != event.CategoryAuthorization {
		t.Fatalf("foreign event source accepted: %v", err)
	}
	assertCount(t, db, "SELECT count(*) FROM arop_event_ledger", 0)
	events := []event.Envelope{
		eventValue(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef21", "io.kinglucky.arop.run.started.v1", `{"state":"started"}`, now),
		eventValue(2, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef22", "io.kinglucky.arop.usage.updated.v1", `{"input_tokens":3,"output_tokens":2,"duration_ms":100,"billable_units":1}`, now.Add(time.Second)),
		eventValue(3, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef23", "io.kinglucky.arop.run.succeeded.v1", `{"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"usage":{"input_tokens":3,"output_tokens":2,"duration_ms":100,"billable_units":1},"completed_at":"2026-09-21T08:00:03Z"}`, now.Add(2*time.Second)),
	}
	requestBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef30", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "batch-key-0001", Events: events}
	var ack event.Ack
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var err error
		ack, err = store.Append(ctx, event.AppendCommand{Request: requestBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("b", 64), RequestDigest: event.BatchDigest(requestBatch), Now: now.Add(3 * time.Second)})
		return err
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if ack.AcceptedThroughProducerSequence != 3 || ack.AssignedRunSequence != 3 || ack.RunState != "succeeded" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	assertCount(t, db, "SELECT count(*) FROM arop_event_ledger", 3)
	assertCount(t, db, "SELECT count(*) FROM arop_event_capacity_releases", 1)
	var state string
	var input, output, duration, billable uint64
	if err := db.QueryRow(`SELECT state,usage_input_tokens,usage_output_tokens,usage_duration_ms,usage_billable_units FROM arop_runs WHERE tenant_id=? AND run_id=?`, testTenant, testRun).Scan(&state, &input, &output, &duration, &billable); err != nil || state != "succeeded" || input != 3 || output != 2 || duration != 100 || billable != 1 {
		t.Fatalf("terminal projection: state=%s usage=%d/%d/%d/%d err=%v", state, input, output, duration, billable, err)
	}
	if _, err := db.Exec(`UPDATE arop_event_ledger SET event_type='tampered'`); err == nil {
		t.Fatal("immutable ledger accepted update")
	}

	// A new idempotency key can replay byte-identical events without mutating projections.
	replay := requestBatch
	replay.BatchID = "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef31"
	replay.IdempotencyKey = "batch-key-0002"
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var err error
		ack, err = store.Append(ctx, event.AppendCommand{Request: replay, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("c", 64), RequestDigest: event.BatchDigest(replay), Now: now.Add(4 * time.Second)})
		return err
	}); err != nil {
		t.Fatalf("duplicate replay: %v", err)
	}
	if len(ack.DuplicateEventIDs) != 3 || ack.AssignedRunSequence != 3 {
		t.Fatalf("duplicate ack: %+v", ack)
	}

	// A legal late terminal event is retained for audit but cannot overwrite the first terminal.
	late := eventValue(4, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef24", "io.kinglucky.arop.run.failed.v1", `{"state":"failed","error":{"code":"LATE","category":"internal","message":"late","retryable":false},"usage":{"input_tokens":9,"output_tokens":9,"duration_ms":999,"billable_units":9},"completed_at":"2026-09-21T08:00:04Z"}`, now.Add(4*time.Second))
	lateBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef32", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "batch-key-0003", Events: []event.Envelope{late}}
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		_, err := store.Append(ctx, event.AppendCommand{Request: lateBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("d", 64), RequestDigest: event.BatchDigest(lateBatch), Now: now.Add(5 * time.Second)})
		return err
	}); err != nil {
		t.Fatalf("late terminal: %v", err)
	}
	var applied bool
	if err := db.QueryRow(`SELECT projection_applied FROM arop_event_ledger WHERE event_id=?`, late.ID).Scan(&applied); err != nil || applied {
		t.Fatalf("late terminal applied=%v err=%v", applied, err)
	}
	if err := db.QueryRow(`SELECT state FROM arop_runs WHERE tenant_id=? AND run_id=?`, testTenant, testRun).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("terminal overwritten: %s %v", state, err)
	}
	conflict := events[1]
	conflict.Data = json.RawMessage(`{"input_tokens":4,"output_tokens":2,"duration_ms":100,"billable_units":1}`)
	conflictBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef33", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "batch-key-0004", Events: []event.Envelope{conflict}}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: conflictBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("f", 64), RequestDigest: event.BatchDigest(conflictBatch), Now: now.Add(6 * time.Second)})
		return inner
	})
	if failure, ok := event.AsError(err); !ok || failure.Reason != event.ReasonEventIDConflict {
		t.Fatalf("event ID conflict accepted: %v", err)
	}
	outOfOrder := eventValue(6, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef26", "io.kinglucky.arop.run.started.v1", `{"state":"started"}`, now.Add(6*time.Second))
	sequenceBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef34", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "batch-key-0005", Events: []event.Envelope{outOfOrder}}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: sequenceBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("1", 64), RequestDigest: event.BatchDigest(sequenceBatch), Now: now.Add(7 * time.Second)})
		return inner
	})
	if failure, ok := event.AsError(err); !ok || failure.Reason != event.ReasonProducerSequence {
		t.Fatalf("out-of-order event accepted: %v", err)
	}
	assertCount(t, db, "SELECT count(*) FROM arop_event_ledger", 4)
	if _, err = db.Exec(`UPDATE arop_registry_instances SET generation=8 WHERE tenant_id=? AND instance_id=?`, testTenant, "instance-a"); err != nil {
		t.Fatal(err)
	}
	fenced := eventValue(5, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef25", "io.kinglucky.arop.run.started.v1", `{"state":"started"}`, now.Add(7*time.Second))
	fencedBatch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef35", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "fenced-runtime", Events: []event.Envelope{fenced}}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: fencedBatch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("2", 64), RequestDigest: event.BatchDigest(fencedBatch), Now: now.Add(8 * time.Second)})
		return inner
	})
	if failure, ok := event.AsError(err); !ok || failure.Reason != event.ReasonAttemptFenced {
		t.Fatalf("superseded runtime session appended event: %v", err)
	}
	assertCount(t, db, "SELECT count(*) FROM arop_event_ledger", 4)
}

func TestSQLiteTerminalAndCapacityReleaseRollBackTogether(t *testing.T) {
	db, uow, store, _, now := eventDatabase(t)
	eventToken := "evtcap_" + strings.Repeat("R", 43)
	var session event.Session
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		session, inner = store.CreateSession(ctx, event.SessionCommand{Request: event.SessionRequest{TenantID: testTenant, RunID: testRun, AttemptID: testAttempt, DeploymentID: testDeployment, Generation: 7, FencingToken: 1}, Token: eventToken, TokenDigest: event.TokenDigest(eventToken), Now: now, ExpiresAt: now.Add(2 * time.Minute), LeaseExpiresAt: now.Add(5 * time.Minute)})
		return inner
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_event_release BEFORE INSERT ON arop_event_capacity_releases BEGIN SELECT RAISE(ABORT,'injected release failure'); END`); err != nil {
		t.Fatal(err)
	}
	terminal := eventValue(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef51", "io.kinglucky.arop.run.succeeded.v1", `{"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"usage":{"input_tokens":1,"output_tokens":1,"duration_ms":1},"completed_at":"2026-09-21T08:00:01Z"}`, now.Add(time.Second))
	batch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef52", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "rollback-batch", Events: []event.Envelope{terminal}}
	err := uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: batch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("2", 64), RequestDigest: event.BatchDigest(batch), Now: now.Add(2 * time.Second)})
		return inner
	})
	if err == nil {
		t.Fatal("injected capacity release failure committed")
	}
	assertCount(t, db, "SELECT count(*) FROM arop_event_ledger", 0)
	assertCount(t, db, "SELECT count(*) FROM arop_event_capacity_releases", 0)
	var state string
	if err := db.QueryRow(`SELECT state FROM arop_runs WHERE tenant_id=? AND run_id=?`, testTenant, testRun).Scan(&state); err != nil || state != "running" {
		t.Fatalf("terminal escaped rollback: state=%s err=%v", state, err)
	}
}

func eventDatabase(t *testing.T) (*sql.DB, *sqliteadapter.UnitOfWork, *Store, string, time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "event.db")+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	applySQLiteMigrations(t, db)
	uow, err := sqliteadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := keyMetadata(private, now)
	insertPrerequisites(t, db, key, now)
	token := signedToken(t, private, key, now)
	return db, uow, store, token, now
}

func applySQLiteMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "migrations", "sqlite"))
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql", "0030_registry.sql", "0040_run.sql", "0050_dispatch.sql", "0060_event.sql"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(data), "-- arop:statement") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err = db.Exec(statement); err != nil {
				t.Fatalf("apply %s: %v\n%s", name, err, statement)
			}
		}
	}
}

func insertPrerequisites(t *testing.T, db *sql.DB, key dispatch.KeyMetadata, now time.Time) {
	t.Helper()
	stamp := formatTime(now)
	deadline := formatTime(now.Add(time.Hour))
	_, err := db.Exec(`INSERT INTO arop_registry_instances(tenant_id,instance_id,session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, testTenant, "instance-a", testSession, "service-a", "test", 7, 9, 9, "lease_01932f13-0cd2-7a82-8fa3-1cb5ce13ef18", formatTime(now.Add(10*time.Minute)), 1, "https://runtime.example.invalid", "/v1/health/ready", `[{"agent_id":"agent-a","agent_version":"1.0.0","skill_ids":["answer"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, `{"healthy":true,"ready":true,"runtime_version":"test","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":1,"max_queue_depth":0,"active_runs":0,"available_slots":1,"queue_depth":0},"labels":{}}`, `{"enabled":true,"weight":100,"priority":0}`, 0, nil, "registered", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_runs(tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,input_json,labels_json,conversation_ref,effect_level,effect_id,state,state_version,authorization_snapshot_json,authorization_snapshot_digest,traceparent,tracestate,deadline_at,created_at,updated_at,cancel_requested_at,usage_input_tokens,usage_output_tokens,usage_duration_ms,usage_billable_units) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, testTenant, testRun, "agent-a", "1.0.0", "answer", "sha256:"+strings.Repeat("a", 64), `[{"type":"text","text":"go"}]`, `{}`, nil, "none", nil, "dispatching", 2, `{}`, "sha256:"+strings.Repeat("b", 64), testTrace, nil, deadline, stamp, stamp, nil, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_deployments(tenant_id,service_id,deployment_id,created_at,updated_at) VALUES(?,?,?,?,?)`, testTenant, "service-a", testDeployment, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_keys(key_id,key_status,public_x,public_y,not_before,sign_until,verify_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, key.KeyID, string(key.Status), key.X, key.Y, formatTime(key.NotBefore), formatTime(key.SignUntil), formatTime(key.VerifyUntil), formatTime(key.CreatedAt), stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_attempts(tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, testTenant, testRun, testAttempt, 1, 1, "tok_01932f13-0cd2-7a82-8fa3-1cb5ce13ef17", testDeployment, "instance-a", testSession, "service-a", 7, 9, "issued", "direct", "https://runtime.example.invalid/v1/runs", testIssuer+"/deployments/"+testDeployment, key.KeyID, formatTime(now.Add(10*time.Minute)), formatTime(now.Add(4*time.Minute)), stamp, nil, nil, nil, testTrace, nil, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
}

func keyMetadata(private *ecdsa.PrivateKey, now time.Time) dispatch.KeyMetadata {
	return dispatch.KeyMetadata{KeyID: "dispatch-key-" + strings.TrimPrefix(testTenant, "tenant-p20-"), Status: dispatch.KeyActive, X: base64.RawURLEncoding.EncodeToString(padded(private.X, 32)), Y: base64.RawURLEncoding.EncodeToString(padded(private.Y, 32)), NotBefore: now.Add(-time.Minute), SignUntil: now.Add(time.Hour), VerifyUntil: now.Add(2 * time.Hour), CreatedAt: now.Add(-time.Minute)}
}
func signedToken(t *testing.T, private *ecdsa.PrivateKey, key dispatch.KeyMetadata, now time.Time) string {
	t.Helper()
	claims := dispatch.TokenClaims{Issuer: testIssuer, Audience: testIssuer + "/deployments/" + testDeployment, Subject: testPrincipal, AuthorizedParty: testCredential, TokenID: testTokenID, RunID: testRun, AttemptID: testAttempt, AgentID: "agent-a", AgentVersion: "1.0.0", SkillID: "answer", DeploymentID: testDeployment, InstanceID: "instance-a", TransportProfile: "direct", Endpoint: "https://runtime.example.invalid/v1/runs", Generation: 7, FencingToken: 1, Scopes: []string{"agent:invoke", "run:stream"}, IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix()}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": key.KeyID, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	encoding := base64.RawURLEncoding
	input := encoding.EncodeToString(header) + "." + encoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := append(padded(r, 32), padded(s, 32)...)
	return input + "." + encoding.EncodeToString(signature)
}
func padded(value *big.Int, size int) []byte {
	raw := value.Bytes()
	result := make([]byte, size)
	copy(result[size-len(raw):], raw)
	return result
}

func eventValue(sequence uint64, id, kind, data string, occurred time.Time) event.Envelope {
	schema := "lifecycle-events-v1.schema.json"
	if strings.Contains(kind, ".usage.") {
		schema = "usage-events-v1.schema.json"
	}
	return event.Envelope{SpecVersion: "1.0", ID: id, Source: "https://runtime.example.invalid/instances/instance-a", Type: kind, Subject: "runs/" + testRun, Time: occurred, DataContentType: "application/json", DataSchema: "https://arop.invalid/schemas/v1/events/" + schema, RunID: testRun, AttemptID: testAttempt, ProducerSequence: sequence, Traceparent: testTrace, Data: json.RawMessage(data)}
}
func assertCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s got=%d want=%d err=%v", query, got, want, err)
	}
}
