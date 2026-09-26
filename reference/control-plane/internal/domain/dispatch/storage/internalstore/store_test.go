package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	sqliteuow "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	_ "modernc.org/sqlite"
)

func TestSQLiteAtomicReservationFencingCapacityAndPublicKeys(t *testing.T) {
	db, err := sql.Open("sqlite", "file:dispatch-store?mode=memory&cache=shared&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, migration := range []string{"0030_registry.sql", "0040_run.sql", "0050_dispatch.sql"} {
		applyMigration(t, db, "../../../../../migrations/sqlite/"+migration)
	}
	uow, err := sqliteuow.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, SQLite, "https://control.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	runID, secondRunID := testUUID("run_", "1"), testUUID("run_", "2")
	insertRun(t, db, SQLite, runID, now)
	insertRun(t, db, SQLite, secondRunID, now)
	insertInstance(t, db, SQLite, now)
	signer, err := dispatch.NewProcessSigner(now.Add(-time.Minute), "key-20260927", time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	active, err := signer.ActiveKey(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := signer.VerificationKeys(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.DispatchableRun(context.Background(), "acme", runID)
	if err != nil {
		t.Fatal(err)
	}
	command := reserveCommand(view, candidate(now, 1), active, keys, now, "1", "a")
	if _, _, err = store.Reserve(context.Background(), command); err == nil {
		t.Fatal("dispatch mutation outside UnitOfWork was accepted")
	}
	var first dispatch.Attempt
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		first, _, inner = store.Reserve(ctx, command)
		return inner
	})
	if err != nil || first.AttemptNumber != 1 || first.FencingToken != 1 || first.RegistryResourceVersion != 1 || first.State != dispatch.StateIssued {
		t.Fatalf("first reservation: %#v %v", first, err)
	}
	var replay bool
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		var got dispatch.Attempt
		got, replay, inner = store.Reserve(ctx, command)
		if inner == nil && got.AttemptID != first.AttemptID {
			t.Fatalf("replay attempt=%s want=%s", got.AttemptID, first.AttemptID)
		}
		return inner
	})
	if err != nil || !replay {
		t.Fatalf("idempotent replay=%v err=%v", replay, err)
	}
	if _, err = db.Exec(`UPDATE arop_registry_instances SET resource_version=2,registry_revision=2,heartbeat_sequence=2 WHERE tenant_id='acme' AND instance_id='runtime-a'`); err != nil {
		t.Fatal(err)
	}
	second, err := store.DispatchableRun(context.Background(), "acme", secondRunID)
	if err != nil {
		t.Fatal(err)
	}
	capacityCommand := reserveCommand(second, candidate(now, 2), active, keys, now, "2", "b")
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		_, _, inner := store.Reserve(ctx, capacityCommand)
		return inner
	})
	if failure, ok := dispatch.AsError(err); !ok || failure.Category != dispatch.CategoryCapacity || failure.Reason != dispatch.ReasonNoCapacity {
		t.Fatalf("keepalive resource-version reset active capacity: %#v", err)
	}
	later := now.Add(11 * time.Minute)
	current, err := store.DispatchableRun(context.Background(), "acme", runID)
	if err != nil || current.State != run.StateDispatching || current.StateVersion != 2 {
		t.Fatalf("updated run: %#v %v", current, err)
	}
	retryCommand := reserveCommand(current, candidate(later, 2), active, keys, later, "3", "c")
	var retried dispatch.Attempt
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		retried, _, inner = store.Reserve(ctx, retryCommand)
		return inner
	})
	if err != nil || retried.AttemptNumber != 2 || retried.FencingToken != 2 || retried.DeploymentID != first.DeploymentID || retried.RegistryResourceVersion != 2 {
		t.Fatalf("retry reservation: %#v %v", retried, err)
	}
	var oldState string
	if err = db.QueryRow(`SELECT attempt_state FROM arop_dispatch_attempts WHERE tenant_id='acme' AND attempt_id=?`, first.AttemptID).Scan(&oldState); err != nil || oldState != "expired" {
		t.Fatalf("old attempt state=%q err=%v", oldState, err)
	}
	public, err := store.Keys(context.Background(), later)
	if err != nil || len(public) != 1 || public[0].KeyID != active.KeyID || public[0].X == "" || public[0].Y == "" {
		t.Fatalf("public keys: %#v %v", public, err)
	}
	restartedSigner, err := dispatch.NewProcessSigner(later, "key-restarted-20260927", time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	restartedKeys, _ := restartedSigner.VerificationKeys(context.Background(), later)
	if err = uow.Within(context.Background(), func(ctx context.Context) error { return store.SyncKeys(ctx, restartedKeys, later) }); err != nil {
		t.Fatal(err)
	}
	public, err = store.Keys(context.Background(), later)
	if err != nil || len(public) != 2 || public[0].Status != dispatch.KeyRetiring || public[1].Status != dispatch.KeyActive || public[0].VerifyUntil.Before(later.Add(10*time.Minute)) {
		t.Fatalf("restart rotation metadata: %#v %v", public, err)
	}
	if err = VerifySchema(SQLite)(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE INDEX arop_dispatch_unexpected_idx ON arop_dispatch_attempts(attempt_id)`); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(SQLite)(context.Background(), db); err == nil {
		t.Fatal("SQLite dispatch schema verifier accepted an extra index")
	}
}

func applyMigration(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
		if _, err = db.Exec(statement); err != nil {
			t.Fatalf("migration %s: %v\n%s", path, err, statement)
		}
	}
}

func insertRun(t *testing.T, db *sql.DB, dialect Dialect, runID string, now time.Time) {
	t.Helper()
	_, err := db.Exec(testQuery(dialect, `INSERT INTO arop_runs(tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,input_json,labels_json,conversation_ref,effect_level,effect_id,state,state_version,authorization_snapshot_json,authorization_snapshot_digest,traceparent,tracestate,deadline_at,created_at,updated_at,cancel_requested_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"acme", runID, "image.generate", "1.0.0", "default", "sha256:"+strings.Repeat("a", 64), `[{"type":"text","text":"hello"}]`, `{}`, nil, "none", nil, "queued", 1, `{}`, "sha256:"+strings.Repeat("b", 64), "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", nil, formatTime(now.Add(time.Hour)), formatTime(now), formatTime(now), nil)
	if err != nil {
		t.Fatal(err)
	}
}

func insertInstance(t *testing.T, db *sql.DB, dialect Dialect, now time.Time) {
	t.Helper()
	bindings, _ := json.Marshal([]map[string]any{{"agent_id": "image.generate", "agent_version": "1.0.0", "skill_ids": []string{"default"}, "manifest_digest": "sha256:" + strings.Repeat("a", 64)}})
	runtime, _ := json.Marshal(map[string]any{"healthy": true, "ready": true, "capacity": map[string]any{"max_concurrency": 1, "max_queue_depth": 0, "active_runs": 0, "available_slots": 1, "queue_depth": 0}, "runtime_version": "1.0.0", "protocol_versions": []string{"1.0"}, "transport_profiles": []string{"direct"}, "capabilities": map[string]any{"streaming": true, "stream_resume": true, "cancellation": true, "status_query": true, "event_outbox": "durable"}, "labels": map[string]string{}})
	operator, _ := json.Marshal(map[string]any{"enabled": true, "weight": 100, "priority": 1})
	_, err := db.Exec(testQuery(dialect, `INSERT INTO arop_registry_instances(tenant_id,instance_id,session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		"acme", "runtime-a", testUUID("ses_", "4"), "image-service", "prod", 7, 1, 1, testUUID("lease_", "5"), formatTime(now.Add(30*time.Minute)), 1, "https://runtime.example.invalid", "/healthz", string(bindings), string(runtime), string(operator), false, nil, "registered", formatTime(now), formatTime(now))
	if err != nil {
		t.Fatal(err)
	}
}

func candidate(now time.Time, resourceVersion uint64) dispatch.Candidate {
	return dispatch.Candidate{InstanceID: "runtime-a", SessionID: testUUID("ses_", "4"), ServiceID: "image-service", Generation: 7, ResourceVersion: resourceVersion, Priority: 1, Weight: 100, AvailableSlots: 1, TransportProfile: "direct", Endpoint: "https://runtime.example.invalid", LeaseExpiresAt: time.Date(2026, 9, 27, 9, 30, 0, 0, time.UTC)}
}

func reserveCommand(view dispatch.RunView, candidate dispatch.Candidate, active dispatch.KeyMetadata, keys []dispatch.KeyMetadata, now time.Time, suffix, digest string) dispatch.ReserveCommand {
	return dispatch.ReserveCommand{Run: view, Candidate: candidate, AttemptID: testUUID("att_", suffix), DeploymentID: testUUID("dep_", suffix), TokenID: testUUID("tok_", suffix), SigningKey: active, SigningKeys: keys, KeyDigest: strings.Repeat(digest, 64), RequestDigest: "sha256:" + strings.Repeat(digest, 64), Now: now, LeaseExpires: now.Add(10 * time.Minute), TicketExpires: now.Add(5 * time.Minute)}
}

func testUUID(prefix, suffix string) string {
	value := []byte("018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c")
	copy(value[len(value)-len(suffix):], suffix)
	return prefix + string(value)
}

func testQuery(dialect Dialect, value string) string {
	if dialect == SQLite {
		return value
	}
	result, index := strings.Builder{}, 1
	for _, character := range value {
		if character == '?' {
			result.WriteString("$")
			result.WriteString(strconv.Itoa(index))
			index++
		} else {
			result.WriteRune(character)
		}
	}
	return result.String()
}
