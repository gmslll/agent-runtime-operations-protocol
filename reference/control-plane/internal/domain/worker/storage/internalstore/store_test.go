package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sqliteuow "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/worker"
	_ "modernc.org/sqlite"
)

const workerTrace = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestSQLiteWorkerClaimRenewFencingCompleteAndReplay(t *testing.T) {
	db, uow, store, now := workerDatabase(t)
	request := claimRequest(now)
	first := claimCommand(request, now, 1)
	var claim worker.Claim
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		claim, inner = store.Claim(ctx, first)
		return inner
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim.AttemptID != workerID("att_", 1) || claim.FencingToken != 1 || claim.LeaseToken != first.LeaseToken {
		t.Fatalf("unexpected first claim: %#v", claim)
	}
	var plaintext int
	if err := db.QueryRow(`SELECT count(*) FROM arop_worker_claims WHERE lease_token_digest=?`, first.LeaseToken).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Fatalf("plaintext lease token stored: count=%d err=%v", plaintext, err)
	}
	renew := worker.RenewCommand{Request: worker.RenewRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, LeaseToken: claim.LeaseToken, FencingToken: claim.FencingToken, LeaseSeconds: 120}, LeaseTokenDigest: claim.LeaseTokenDigest, Now: now.Add(10 * time.Second), ExpiresAt: now.Add(130 * time.Second)}
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		claim, inner = store.Renew(ctx, renew)
		return inner
	}); err != nil || !claim.LeaseExpiresAt.Equal(renew.ExpiresAt) {
		t.Fatalf("renew: %#v %v", claim, err)
	}
	release := worker.ReleaseCommand{Request: worker.ReleaseRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, LeaseToken: claim.LeaseToken, FencingToken: claim.FencingToken, Reason: "retryable_failure"}, LeaseTokenDigest: claim.LeaseTokenDigest, ReplacementAttemptID: workerID("att_", 2), TokenID: workerID("tok_", 2), Now: now.Add(20 * time.Second)}
	if err := uow.Within(context.Background(), func(ctx context.Context) error { return store.Release(ctx, release) }); err != nil {
		t.Fatalf("release: %v", err)
	}
	stale := completeCommand(request, claim, now.Add(21*time.Second), 1)
	err := uow.Within(context.Background(), func(ctx context.Context) error { _, inner := store.Complete(ctx, stale); return inner })
	if failure, ok := worker.AsError(err); !ok || failure.Category != worker.CategoryNotFound && failure.Reason != worker.ReasonClaimExpired {
		t.Fatalf("released worker completed stale attempt: %v", err)
	}
	second := claimCommand(request, now.Add(22*time.Second), 3)
	if err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		claim, inner = store.Claim(ctx, second)
		return inner
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if claim.AttemptID != workerID("att_", 2) || claim.AttemptNumber != 2 || claim.FencingToken != 2 {
		t.Fatalf("reclaim did not preserve replacement fence: %#v", claim)
	}
	complete := completeCommand(request, claim, now.Add(30*time.Second), 2)
	var replay bool
	if err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		replay, inner = store.Complete(ctx, complete)
		return inner
	}); err != nil || replay {
		t.Fatalf("complete replay=%v err=%v", replay, err)
	}
	if err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		replay, inner = store.Complete(ctx, complete)
		return inner
	}); err != nil || !replay {
		t.Fatalf("completion replay=%v err=%v", replay, err)
	}
	wrongToken := complete
	wrongToken.Request.LeaseToken = "wlt_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	wrongToken.LeaseTokenDigest = worker.TokenDigest(wrongToken.Request.LeaseToken)
	err = uow.Within(context.Background(), func(ctx context.Context) error { _, inner := store.Complete(ctx, wrongToken); return inner })
	if failure, ok := worker.AsError(err); !ok || failure.Reason != worker.ReasonIdempotencyConflict {
		t.Fatalf("completion replay accepted wrong lease token: %v", err)
	}
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_worker_completions", 1)
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_worker_outbox", 1)
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_event_ledger", 1)
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_event_capacity_releases", 1)
	var state string
	if err = db.QueryRow(`SELECT state FROM arop_runs WHERE tenant_id=? AND run_id=?`, request.Caller.TenantID, claim.RunID).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("run terminal state=%q err=%v", state, err)
	}
	mutated := complete
	mutated.Request.Result = json.RawMessage(strings.ReplaceAll(string(mutated.Request.Result), `"output_tokens":2`, `"output_tokens":3`))
	mutated.RequestDigest, _ = worker.RequestDigest(struct{ Result json.RawMessage }{mutated.Request.Result})
	err = uow.Within(context.Background(), func(ctx context.Context) error { _, inner := store.Complete(ctx, mutated); return inner })
	if failure, ok := worker.AsError(err); !ok || failure.Reason != worker.ReasonIdempotencyConflict {
		t.Fatalf("mutated idempotent completion accepted: %v", err)
	}
}

func TestSQLiteWorkerCompletionRollsBackTerminalOnOutboxFailure(t *testing.T) {
	db, uow, store, now := workerDatabase(t)
	request := claimRequest(now)
	command := claimCommand(request, now, 10)
	var claim worker.Claim
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		claim, inner = store.Claim(ctx, command)
		return inner
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_worker_outbox BEFORE INSERT ON arop_worker_outbox BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	complete := completeCommand(request, claim, now.Add(time.Second), 10)
	err := uow.Within(context.Background(), func(ctx context.Context) error { _, inner := store.Complete(ctx, complete); return inner })
	if err == nil {
		t.Fatal("outbox failure committed terminal result")
	}
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_worker_completions", 0)
	assertWorkerCount(t, db, "SELECT count(*) FROM arop_event_ledger", 0)
	var state string
	if err := db.QueryRow(`SELECT state FROM arop_runs WHERE tenant_id=? AND run_id=?`, request.Caller.TenantID, claim.RunID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("terminal escaped rollback state=%q err=%v", state, err)
	}
}

func TestSQLiteWorkerSessionRestartFencesUnexpiredClaim(t *testing.T) {
	db, uow, store, now := workerDatabase(t)
	request := claimRequest(now)
	firstCommand := claimCommand(request, now, 20)
	var first worker.Claim
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		first, inner = store.Claim(ctx, firstCommand)
		return inner
	}); err != nil {
		t.Fatal(err)
	}
	newSession := workerID("ses_", 2)
	if _, err := db.Exec(`UPDATE arop_registry_instances SET session_id=?,generation=8,resource_version=10 WHERE tenant_id='acme' AND instance_id='worker-a'`, newSession); err != nil {
		t.Fatal(err)
	}
	request.SessionID, request.Generation = newSession, 8
	secondCommand := claimCommand(request, now.Add(time.Second), 21)
	var second worker.Claim
	if err := uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		second, inner = store.Claim(ctx, secondCommand)
		return inner
	}); err != nil {
		t.Fatalf("restart reclaim: %v", err)
	}
	if second.AttemptID == first.AttemptID || second.AttemptNumber != first.AttemptNumber+1 || second.FencingToken != first.FencingToken+1 || second.SessionID != newSession {
		t.Fatalf("restart did not fence claim: first=%#v second=%#v", first, second)
	}
	stale := completeCommand(claimRequest(now), first, now.Add(2*time.Second), 22)
	err := uow.Within(context.Background(), func(ctx context.Context) error { _, inner := store.Complete(ctx, stale); return inner })
	if failure, ok := worker.AsError(err); !ok || failure.Reason != worker.ReasonClaimNotFound && failure.Reason != worker.ReasonClaimExpired {
		t.Fatalf("stale session completed fenced attempt: %v", err)
	}
}

func TestSQLiteWorkerSchemaIsExact(t *testing.T) {
	db, _, _, _ := workerDatabase(t)
	if err := VerifySchema(SQLite)(context.Background(), db); err != nil {
		t.Fatalf("verify schema: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX arop_worker_unexpected_idx ON arop_worker_claims(worker_id)`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(SQLite)(context.Background(), db); err == nil {
		t.Fatal("schema verifier accepted unexpected worker index")
	}
}

func workerDatabase(t *testing.T) (*sql.DB, *sqliteuow.UnitOfWork, *Store, time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "worker.db")+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "migrations", "sqlite"))
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql", "0030_registry.sql", "0040_run.sql", "0050_dispatch.sql", "0060_event.sql", "0070_worker.sql"} {
		data, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil {
			t.Fatal(readErr)
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
	uow, err := sqliteuow.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	insertWorkerPrerequisites(t, db, now)
	return db, uow, store, now
}

func insertWorkerPrerequisites(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	tenant, runID, attemptID := "acme", workerID("run_", 1), workerID("att_", 1)
	sessionID, deploymentID := workerID("ses_", 1), workerID("dep_", 1)
	stamp, deadline := formatTime(now), formatTime(now.Add(time.Hour))
	bindings := `[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`
	runtimeState := `{"healthy":true,"ready":true,"runtime_version":"p24","protocol_versions":["1.0"],"transport_profiles":["worker_pull"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":0,"available_slots":2,"queue_depth":0},"labels":{}}`
	_, err := db.Exec(`INSERT INTO arop_registry_instances(tenant_id,instance_id,session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, "worker-a", sessionID, "worker-service", "test", 7, 9, 9, workerID("lease_", 1), formatTime(now.Add(10*time.Minute)), 1, "https://worker.example.invalid", "/health/ready", bindings, runtimeState, `{"enabled":true,"weight":100,"priority":0}`, false, nil, "registered", stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_runs(tenant_id,run_id,agent_id,agent_version,skill_id,manifest_digest,input_json,labels_json,conversation_ref,effect_level,effect_id,state,state_version,authorization_snapshot_json,authorization_snapshot_digest,traceparent,tracestate,deadline_at,created_at,updated_at,cancel_requested_at,usage_input_tokens,usage_output_tokens,usage_duration_ms,usage_billable_units) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, runID, "image.generate", "1.0.0", "default", "sha256:"+strings.Repeat("a", 64), `[{"type":"text","text":"work"}]`, `{}`, nil, "none", nil, "dispatching", 2, `{}`, "sha256:"+strings.Repeat("b", 64), workerTrace, nil, deadline, stamp, stamp, nil, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_deployments(tenant_id,service_id,deployment_id,created_at,updated_at) VALUES(?,?,?,?,?)`, tenant, "worker-service", deploymentID, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_keys(key_id,key_status,public_x,public_y,not_before,sign_until,verify_until,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "worker-signing-key", "active", strings.Repeat("A", 43), strings.Repeat("B", 43), stamp, deadline, formatTime(now.Add(2*time.Hour)), stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO arop_dispatch_attempts(tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tenant, runID, attemptID, 1, 1, workerID("tok_", 1), deploymentID, "worker-a", sessionID, "worker-service", 7, 9, "issued", "worker_pull", "https://worker.example.invalid/v1/runs", "https://control.example.invalid/deployments/"+deploymentID, "worker-signing-key", formatTime(now.Add(5*time.Minute)), formatTime(now.Add(5*time.Minute)), stamp, nil, nil, nil, workerTrace, nil, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
}

func claimRequest(now time.Time) worker.ClaimRequest {
	return worker.ClaimRequest{Caller: worker.Caller{TenantID: "acme", PrincipalID: workerID("prn_", 1), CredentialID: workerID("cred_", 1), Scopes: []string{"worker:claim", "worker:complete"}}, WorkerID: "worker-a", SessionID: workerID("ses_", 1), Generation: 7, AvailableSlots: 2, SupportedBindings: []worker.Binding{{AgentID: "image.generate", Version: "1.0.0", SkillID: "default", ManifestDigest: "sha256:" + strings.Repeat("a", 64)}}, LeaseSeconds: 60}
}

func claimCommand(request worker.ClaimRequest, now time.Time, sequence int) worker.ClaimCommand {
	token := "wlt_" + strings.Repeat(string(rune('A'+sequence%20)), 43)
	return worker.ClaimCommand{Request: request, ClaimID: workerID("clm_", sequence), LeaseToken: token, LeaseTokenDigest: worker.TokenDigest(token), ReplacementAttemptID: workerID("att_", sequence+20), TokenID: workerID("tok_", sequence+20), Now: now, LeaseExpiresAt: now.Add(time.Minute)}
}

func completeCommand(request worker.ClaimRequest, claim worker.Claim, now time.Time, sequence int) worker.CompleteCommand {
	result := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"run_id":%q,"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:%s"},"usage":{"input_tokens":1,"output_tokens":2,"duration_ms":10},"completed_at":%q}`, claim.RunID, strings.Repeat("e", 64), now.Format(time.RFC3339Nano)))
	requestValue := worker.CompleteRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, CompletionID: workerID("cmp_", sequence), AttemptID: claim.AttemptID, LeaseToken: claim.LeaseToken, IdempotencyKey: fmt.Sprintf("complete-key-%04d", sequence), FencingToken: claim.FencingToken, Result: result, CompletedAt: now}
	digest, _ := worker.RequestDigest(struct{ Result json.RawMessage }{result})
	return worker.CompleteCommand{Request: requestValue, LeaseTokenDigest: claim.LeaseTokenDigest, KeyDigest: worker.KeyDigest(requestValue.IdempotencyKey), RequestDigest: digest, EventID: workerID("evt_", sequence), OutboxID: workerID("out_", sequence), Now: now}
}

func workerID(prefix string, sequence int) string {
	return fmt.Sprintf("%s01932f13-0cd2-7a82-8fa3-%012x", prefix, sequence)
}
func assertWorkerCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s got=%d want=%d err=%v", query, got, want, err)
	}
}
