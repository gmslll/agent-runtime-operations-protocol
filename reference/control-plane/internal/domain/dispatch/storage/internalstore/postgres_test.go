package internalstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	postgresuow "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAtomicReservationAndExactSchema(t *testing.T) {
	dsn := os.Getenv("AROP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AROP_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, migration := range []string{"0030_registry.sql", "0040_run.sql", "0050_dispatch.sql"} {
		applyMigration(t, db, "../../../../../migrations/postgres/"+migration)
	}
	uow, err := postgresuow.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, uow.Transaction, Postgres, "https://control.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	insertRun(t, db, Postgres, testUUID("run_", "1"), now)
	insertInstance(t, db, Postgres, now)
	signer, err := dispatch.NewProcessSigner(now.Add(-time.Minute), "key-20260927", time.Hour, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	active, _ := signer.ActiveKey(context.Background(), now)
	keys, _ := signer.VerificationKeys(context.Background(), now)
	view, err := store.DispatchableRun(context.Background(), "acme", testUUID("run_", "1"))
	if err != nil {
		t.Fatal(err)
	}
	command := reserveCommand(view, candidate(now, 1), active, keys, now, "1", "a")
	var attempt dispatch.Attempt
	err = uow.Within(context.Background(), func(ctx context.Context) error {
		var inner error
		attempt, _, inner = store.Reserve(ctx, command)
		return inner
	})
	if err != nil || attempt.AttemptNumber != 1 || attempt.FencingToken != 1 {
		t.Fatalf("PostgreSQL reservation: %#v %v", attempt, err)
	}
	if err = VerifySchema(Postgres)(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE INDEX arop_dispatch_unexpected_idx ON arop_dispatch_attempts(attempt_id)`); err != nil {
		t.Fatal(err)
	}
	if err = VerifySchema(Postgres)(context.Background(), db); err == nil {
		t.Fatal("PostgreSQL dispatch schema verifier accepted an extra index")
	}
}
