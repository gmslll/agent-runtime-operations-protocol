package internalstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	applyPostgresWorkerMigrations(t, db)
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

func applyPostgresWorkerMigrations(t *testing.T, db *sql.DB) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "migrations", "postgres"))
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql", "0030_registry.sql", "0040_run.sql", "0050_dispatch.sql", "0060_event.sql", "0070_worker.sql"} {
		data, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, statement := range strings.Split(string(data), "-- arop:statement") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("apply %s: %v\n%s", name, err, statement)
			}
		}
	}
}
