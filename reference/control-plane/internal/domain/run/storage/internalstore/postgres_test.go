package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	postgresuow "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAtomicLifecycle(t *testing.T) {
	dsn := os.Getenv("AROP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AROP_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	contents, err := os.ReadFile("../../../../../migrations/postgres/0040_run.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
		if _, err = db.Exec(statement); err != nil {
			t.Fatalf("migration: %v\n%s", err, statement)
		}
	}
	if err = VerifySchema(Postgres)(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	uow, err := postgresuow.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, Postgres)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	record := testRecord(t, now, run.EffectIntent{Level: run.EffectRead})
	outbox := run.Outbox{OutboxID: "out_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", TenantID: "acme", RunID: record.RunID, Kind: "run-queued", StateVersion: 1, Payload: json.RawMessage(`{"state":"queued"}`), CreatedAt: now}
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		return store.Create(ctx, record, strings.Repeat("c", 64), "sha256:"+strings.Repeat("d", 64), outbox)
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(context.Background(), "acme", record.RunID)
	if err != nil || loaded.RunID != record.RunID {
		t.Fatalf("load: %#v %v", loaded, err)
	}
	if _, err = db.Exec(`CREATE INDEX arop_run_unexpected_idx ON arop_runs(run_id)`); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(Postgres)(context.Background(), db); err == nil {
		t.Fatal("PostgreSQL run schema verifier accepted an extra index")
	}
}
