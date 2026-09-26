package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/internaltest"
	registrystore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/sqlite"
)

func TestSQLiteRegistryRepository(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqliteadapter.Open(filepath.Join(directory, "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := os.Getenv("AROP_P14_MIGRATION_ROOT")
	if root == "" {
		root = filepath.Join("..", "..", "..", "..", "..", "migrations")
	}
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql", "0030_registry.sql"} {
		contents, readErr := os.ReadFile(filepath.Join(root, "sqlite", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
			if _, execErr := db.ExecContext(context.Background(), statement); execErr != nil {
				t.Fatalf("apply %s: %v", name, execErr)
			}
		}
	}
	unit, err := sqliteadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := registrystore.New(db, unit.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	if err := registrystore.VerifySchema()(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	internaltest.RunRepositoryMatrix(t, unit, repository)
	if _, err := db.Exec(`UPDATE arop_registry_events SET event_type='updated' WHERE revision=1`); err == nil {
		t.Fatal("append-only event ledger accepted UPDATE")
	}
	if _, err := db.Exec(`DELETE FROM arop_registry_events WHERE revision=1`); err == nil {
		t.Fatal("append-only event ledger accepted DELETE")
	}
	if _, err := db.Exec(`CREATE INDEX arop_registry_events_unexpected_idx ON arop_registry_events(instance_id)`); err != nil {
		t.Fatal(err)
	}
	if err := registrystore.VerifySchema()(context.Background(), db); err == nil {
		t.Fatal("VerifySchema accepted an unexpected registry index")
	}
}
