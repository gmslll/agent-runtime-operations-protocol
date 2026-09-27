package internalstore

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresWorkerSchemaIsExact(t *testing.T) {
	dsn := os.Getenv("AROP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AROP_TEST_POSTGRES_DSN is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = VerifySchema(Postgres)(context.Background(), db); err != nil {
		t.Fatalf("verify schema: %v", err)
	}
	if _, err = db.Exec(`CREATE INDEX arop_worker_unexpected_idx ON arop_worker_claims(worker_id)`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP INDEX IF EXISTS arop_worker_unexpected_idx`)
	if err = VerifySchema(Postgres)(context.Background(), db); err == nil {
		t.Fatal("schema verifier accepted unexpected worker index")
	}
}
