package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	sqliteuow "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	_ "modernc.org/sqlite"
)

func TestSQLiteAtomicLifecycleAndEffectReservation(t *testing.T) {
	db, err := sql.Open("sqlite", "file:run-store?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	contents, err := os.ReadFile("../../../../../migrations/sqlite/0040_run.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
		if _, err = db.Exec(statement); err != nil {
			t.Fatalf("migration: %v\n%s", err, statement)
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
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	snapshot := json.RawMessage(`{"tenant_id":"acme","principal_id":"prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"}`)
	record := run.Run{TenantID: "acme", RunID: "run_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Agent: run.AgentBinding{ID: "support.agent", Version: "1.2.3", SkillID: "answer", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Input: json.RawMessage(`[{"type":"text","text":"hello"}]`), Effects: run.EffectIntent{Level: run.EffectWrite, EffectID: "eff_invoice-01"}, State: run.StateQueued, StateVersion: 1, AuthorizationSnapshot: snapshot, AuthorizationSnapshotDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", DeadlineAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	outbox := run.Outbox{OutboxID: "out_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TenantID: "acme", RunID: record.RunID, Kind: "run-queued", StateVersion: 1, Payload: json.RawMessage(`{"state":"queued"}`), CreatedAt: now}
	if err = store.Create(context.Background(), record, strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), outbox); err == nil {
		t.Fatal("mutation outside UoW accepted")
	}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		return store.Create(ctx, record, strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), outbox)
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, digest, err := store.GetByIdempotency(context.Background(), "acme", strings.Repeat("a", 64))
	if err != nil || loaded.RunID != record.RunID || digest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("load: %#v %q %v", loaded, digest, err)
	}
	command := run.Command{CommandID: "cmd_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", ExpectedStateVersion: 1, Type: "run.cancel", Data: json.RawMessage(`{"reason":"operator"}`)}
	commandOutbox := run.Outbox{OutboxID: "out_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d", TenantID: "acme", RunID: record.RunID, Kind: "run-command", StateVersion: 2, Payload: json.RawMessage(`{"type":"run.cancel"}`), CreatedAt: now}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		loaded, inner = store.Cancel(ctx, "acme", record.RunID, command, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64), now, commandOutbox)
		return inner
	})
	if err != nil || loaded.State != run.StateCancelRequested || loaded.StateVersion != 2 {
		t.Fatalf("cancel: %#v %v", loaded, err)
	}
	reservation := run.EffectReservation{TenantID: "acme", RunID: record.RunID, EffectID: "eff_invoice-01", SemanticDigest: "sha256:" + strings.Repeat("e", 64), CreatedAt: now}
	var replay bool
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		replay, inner = store.ReserveEffect(ctx, reservation)
		return inner
	})
	if err != nil || replay {
		t.Fatalf("reserve: %v %v", replay, err)
	}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		replay, inner = store.ReserveEffect(ctx, reservation)
		return inner
	})
	if err != nil || !replay {
		t.Fatalf("replay: %v %v", replay, err)
	}
}
