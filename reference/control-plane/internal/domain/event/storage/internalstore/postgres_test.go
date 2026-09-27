package internalstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresEventSchema(t *testing.T) {
	dsn := os.Getenv("AROP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AROP_TEST_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	preparePostgresEventDatabase(t, db)
	if err = VerifySchema(Postgres)(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresEventLedgerTerminalAndCapacityRelease(t *testing.T) {
	dsn := os.Getenv("AROP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AROP_TEST_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	preparePostgresEventDatabase(t, db)
	uow, err := postgresadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, Postgres)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := keyMetadata(private, now)
	insertPostgresPrerequisites(t, db, key, now)
	eventToken := "evtcap_" + strings.Repeat("P", 31) + testNonce
	var session event.Session
	if err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		session, inner = store.CreateSession(ctx, event.SessionCommand{Request: event.SessionRequest{TenantID: testTenant, RunID: testRun, AttemptID: testAttempt, DeploymentID: testDeployment, Generation: 7, FencingToken: 1}, Token: eventToken, TokenDigest: event.TokenDigest(eventToken), Now: now, ExpiresAt: now.Add(2 * time.Minute), LeaseExpiresAt: now.Add(5 * time.Minute)})
		return inner
	}); err != nil {
		t.Fatalf("create postgres session: %v", err)
	}
	terminal := eventValue(1, "evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef41", "io.kinglucky.arop.run.succeeded.v1", `{"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"usage":{"input_tokens":1,"output_tokens":2,"duration_ms":3,"billable_units":4},"completed_at":"2026-09-21T08:00:01Z"}`, now.Add(time.Second))
	batch := event.BatchRequest{Token: session.Token, RunID: testRun, BatchID: "batch_01932f13-0cd2-7a82-8fa3-1cb5ce13ef42", AttemptID: testAttempt, FencingToken: 1, IdempotencyKey: "postgres-batch-key", Events: []event.Envelope{terminal}}
	if err = uow.Within(context.Background(), func(ctx context.Context) error {
		_, inner := store.Append(ctx, event.AppendCommand{Request: batch, TokenDigest: event.TokenDigest(session.Token), IdempotencyKeyDigest: strings.Repeat("e", 64), RequestDigest: event.BatchDigest(batch), Now: now.Add(2 * time.Second)})
		return inner
	}); err != nil {
		t.Fatalf("append postgres terminal: %v", err)
	}
	assertPostgresTenantCount(t, db, "arop_event_ledger", 1)
	assertPostgresTenantCount(t, db, "arop_event_capacity_releases", 1)
}

func preparePostgresEventDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset PostgreSQL event test database: %v", err)
	}
	root := os.Getenv("AROP_P20_MIGRATION_ROOT")
	if root == "" {
		root = filepath.Join("..", "..", "..", "..", "..", "migrations")
	}
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql", "0030_registry.sql", "0040_run.sql", "0050_dispatch.sql", "0060_event.sql"} {
		contents, err := os.ReadFile(filepath.Join(root, "postgres", name))
		if err != nil {
			t.Fatalf("read PostgreSQL migration %s: %v", name, err)
		}
		for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
			if _, err = db.Exec(statement); err != nil {
				t.Fatalf("apply PostgreSQL migration %s: %v", name, err)
			}
		}
	}
}

func assertPostgresTenantCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE tenant_id=$1`, testTenant).Scan(&got); err != nil || got != want {
		t.Fatalf("%s tenant count=%d want=%d err=%v", table, got, want, err)
	}
}

func insertPostgresPrerequisites(t *testing.T, db *sql.DB, key dispatch.KeyMetadata, now time.Time) {
	t.Helper()
	stamp := formatTime(now)
	deadline := formatTime(now.Add(time.Hour))
	_, err := db.Exec(`INSERT INTO arop_registry_instances(tenant_id,instance_id,session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`, testTenant, "instance-a", testSession, "service-a", "test", 7, 9, 9, testLeaseID, formatTime(now.Add(10*time.Minute)), 1, "https://runtime.example.invalid", "/v1/health/ready", `[{"agent_id":"agent-a","agent_version":"1.0.0","skill_ids":["answer"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, `{"healthy":true,"ready":true,"runtime_version":"test","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":1,"max_queue_depth":0,"active_runs":0,"available_slots":1,"queue_depth":0},"labels":{}}`, `{"enabled":true,"weight":100,"priority":0}`, false, nil, "registered", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_runs(tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,input_json,labels_json,conversation_ref,effect_level,effect_id,state,state_version,authorization_snapshot_json,authorization_snapshot_digest,traceparent,tracestate,deadline_at,created_at,updated_at,cancel_requested_at,usage_input_tokens,usage_output_tokens,usage_duration_ms,usage_billable_units) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`, testTenant, testRun, "agent-a", "1.0.0", "answer", "sha256:"+strings.Repeat("a", 64), `[{"type":"text","text":"go"}]`, `{}`, nil, "none", nil, "dispatching", 2, `{}`, "sha256:"+strings.Repeat("b", 64), testTrace, nil, deadline, stamp, stamp, nil, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_deployments(tenant_id,service_id,deployment_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5)`, testTenant, "service-a", testDeployment, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	var activeKeyID string
	err = db.QueryRow(`SELECT key_id FROM arop_dispatch_keys WHERE key_status='active' ORDER BY key_id LIMIT 1`).Scan(&activeKeyID)
	if err == sql.ErrNoRows {
		_, err = db.Exec(`INSERT INTO arop_dispatch_keys(key_id,key_status,public_x,public_y,not_before,sign_until,verify_until,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, key.KeyID, string(key.Status), key.X, key.Y, formatTime(key.NotBefore), formatTime(key.SignUntil), formatTime(key.VerifyUntil), formatTime(key.CreatedAt), stamp)
		activeKeyID = key.KeyID
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_attempts(tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`, testTenant, testRun, testAttempt, 1, 1, testTokenID, testDeployment, "instance-a", testSession, "service-a", 7, 9, "issued", "direct", "https://runtime.example.invalid/v1/runs", testIssuer+"/deployments/"+testDeployment, activeKeyID, formatTime(now.Add(10*time.Minute)), formatTime(now.Add(4*time.Minute)), stamp, nil, nil, nil, testTrace, nil, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
}
