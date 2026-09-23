package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	pgstore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqlitestore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

const migrationLockKey int64 = 0x41524f5009

var databaseSequence atomic.Uint64

type unitOfWork interface {
	Within(context.Context, func(context.Context) error) error
	Transaction(context.Context) (*sql.Tx, bool)
}

type backend struct {
	dialect   migrate.Dialect
	db        *sql.DB
	locker    migrate.Locker
	uow       unitOfWork
	path, dsn string
	close     func()
}

func TestMigrationEngineConformance(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("empty-to-current", func(t *testing.T) { testEmptyToCurrent(t, dialect) })
			t.Run("n-minus-one-to-current", func(t *testing.T) { testNMinusOne(t, dialect) })
			t.Run("idempotent", func(t *testing.T) { testIdempotent(t, dialect) })
			t.Run("dirty-refusal", func(t *testing.T) { testDirtyRefusal(t, dialect) })
			t.Run("checksum-drift-refusal", func(t *testing.T) { testChecksumDrift(t, dialect) })
			t.Run("ahead-refusal", func(t *testing.T) { testAhead(t, dialect) })
			t.Run("gap-refusal", func(t *testing.T) { testGap(t, dialect) })
			t.Run("sparse-catalog", func(t *testing.T) {
				t.Run("empty-1-5-10-20", func(t *testing.T) { testSparseEmpty(t, dialect) })
				t.Run("one-to-five", func(t *testing.T) { testSparseOneToFive(t, dialect) })
				t.Run("idempotent-at-five", func(t *testing.T) { testSparseIdempotent(t, dialect) })
				t.Run("dirty-five", func(t *testing.T) { testSparseDirty(t, dialect) })
				t.Run("checksum-five", func(t *testing.T) { testSparseChecksum(t, dialect) })
				t.Run("history-unknown-seven", func(t *testing.T) { testSparseUnknownHistory(t, dialect) })
				t.Run("missing-five-before-ten", func(t *testing.T) { testSparseMissingHistory(t, dialect) })
				t.Run("catalog-missing-applied-five", func(t *testing.T) { testSparseMissingDefinition(t, dialect) })
				t.Run("duplicate-version-refusal", func(t *testing.T) { testSparseDuplicateCatalog(t, dialect) })
				t.Run("unordered-version-refusal", func(t *testing.T) { testSparseUnorderedCatalog(t, dialect) })
			})
			t.Run("schema-artifact-refusal", func(t *testing.T) {
				for _, artifact := range []string{"index", "check", "unique"} {
					artifact := artifact
					t.Run(artifact, func(t *testing.T) { testSchemaArtifactRefusal(t, dialect, artifact) })
				}
			})
			t.Run("unmanaged-database-refusal", func(t *testing.T) {
				t.Run("user-object", func(t *testing.T) { testUnmanagedDatabaseRefusal(t, dialect) })
				if dialect == migrate.DialectPostgres {
					t.Run("non-public-schema", testPostgresNonPublicSchemaRefusal)
				}
			})
			t.Run("counterfeit-schema-refusal", func(t *testing.T) {
				t.Run("weak-columns", func(t *testing.T) { testCounterfeitSchemaRefusal(t, dialect) })
				t.Run("history-constraint-body", func(t *testing.T) { testCounterfeitHistoryConstraintRefusal(t, dialect) })
			})
			t.Run("read-only-refusal", func(t *testing.T) { testReadOnlyRefusal(t, dialect) })
			t.Run("concurrent-serialization", func(t *testing.T) { testConcurrent(t, dialect) })
			t.Run("backup-restore", func(t *testing.T) {
				t.Run("successful-upgrade", func(t *testing.T) { testProductionBackupUpgrade(t, dialect) })
				t.Run("migration-failure-restores-v1", func(t *testing.T) { testBackupRestore(t, dialect) })
				t.Run("corrupt-snapshot-refusal", func(t *testing.T) { testSnapshotRefusal(t, dialect, "corrupt") })
				t.Run("truncated-snapshot-refusal", func(t *testing.T) { testSnapshotRefusal(t, dialect, "truncated") })
				t.Run("wrong-snapshot-refusal", func(t *testing.T) { testSnapshotRefusal(t, dialect, "wrong") })
				t.Run("restore-failure-preserves-original", func(t *testing.T) { testRestoreFailurePreservesOriginal(t, dialect) })
				t.Run("snapshot-metadata-survives-adapter-restart", func(t *testing.T) { testSnapshotMetadataSurvivesRestart(t, dialect) })
			})
		})
	}
}

func TestFilesystemBoundaryConformance(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		t.Run("parent-directory-symlink", testSQLiteParentDirectorySymlink)
		t.Run("database-symlink", testSQLiteDatabaseSymlink)
		t.Run("lock-final-symlink", testSQLiteLockFinalSymlink)
		t.Run("lock-ancestor-symlink", testSQLiteLockAncestorSymlink)
	})
	t.Run("migrations", func(t *testing.T) {
		for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
			dialect := dialect
			t.Run(string(dialect)+"-engine-directory-symlink", func(t *testing.T) { testMigrationDirectorySymlink(t, dialect) })
		}
	})
}

// TestMigrationProcessWorker is re-executed by the concurrency acceptance
// case. Its no-op parent invocation is part of the exact test inventory.
func TestMigrationProcessWorker(t *testing.T) {
	dialectText := os.Getenv("AROP_P09_PROCESS_DIALECT")
	if dialectText == "" {
		return
	}
	dialect := migrate.Dialect(dialectText)
	var db *sql.DB
	var locker migrate.Locker
	var err error
	switch dialect {
	case migrate.DialectSQLite:
		path := requireAbsoluteEnv(t, "AROP_P09_PROCESS_DATABASE")
		db, err = sqlitestore.Open(path)
		if err == nil {
			locker, err = sqlitestore.NewMigrationLocker(path + ".migration.lock")
		}
	case migrate.DialectPostgres:
		db, err = pgstore.Open(context.Background(), requireEnv(t, "AROP_P09_PROCESS_DATABASE"))
		if err == nil {
			locker, err = pgstore.NewMigrationLocker(db, migrationLockKey)
		}
	default:
		t.Fatalf("unsupported process dialect %q", dialectText)
	}
	requireNoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	catalog := fixtureCatalog(t, dialect, 2)
	runner, err := migrate.NewRunner(db, catalog, locker, migrate.WithVerifier(fixtureSchemaVerifier(catalog.TargetVersion())))
	requireNoError(t, err)
	barrier := requireAbsoluteEnv(t, "AROP_P09_PROCESS_BARRIER")
	pid := os.Getpid()
	requireNoError(t, os.WriteFile(filepath.Join(barrier, fmt.Sprintf("ready-%d", pid)), []byte("ready\n"), 0o600))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Lstat(filepath.Join(barrier, "start")); err == nil {
			break
		}
		if !os.IsNotExist(err) || time.Now().After(deadline) {
			t.Fatal("migration process barrier was not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(filepath.Join(barrier, fmt.Sprintf("result-%d", pid)), []byte(fmt.Sprintf("%d\n", len(result.Applied))), 0o600))
}

func testSQLiteParentDirectorySymlink(t *testing.T) {
	root := newBoundaryRoot(t)
	realDirectory := filepath.Join(root, "real")
	requireNoError(t, os.Mkdir(realDirectory, 0o700))
	linkedDirectory := filepath.Join(root, "linked")
	requireNoError(t, os.Symlink(realDirectory, linkedDirectory))
	if db, err := sqlitestore.Open(filepath.Join(linkedDirectory, "storage.db")); err == nil {
		_ = db.Close()
		t.Fatal("SQLite database below symlinked parent was accepted")
	}
}

func testSQLiteDatabaseSymlink(t *testing.T) {
	root := newBoundaryRoot(t)
	target := filepath.Join(root, "target.db")
	requireNoError(t, os.WriteFile(target, nil, 0o600))
	linked := filepath.Join(root, "linked.db")
	requireNoError(t, os.Symlink(target, linked))
	if db, err := sqlitestore.Open(linked); err == nil {
		_ = db.Close()
		t.Fatal("symlinked SQLite database was accepted")
	}
}

func testSQLiteLockFinalSymlink(t *testing.T) {
	root := newBoundaryRoot(t)
	target := filepath.Join(root, "target.lock")
	requireNoError(t, os.WriteFile(target, nil, 0o600))
	linked := filepath.Join(root, "linked.lock")
	requireNoError(t, os.Symlink(target, linked))
	if _, err := sqlitestore.NewMigrationLocker(linked); err == nil {
		t.Fatal("symlinked SQLite lock was accepted")
	}
}

func testSQLiteLockAncestorSymlink(t *testing.T) {
	root := newBoundaryRoot(t)
	realDirectory := filepath.Join(root, "real")
	requireNoError(t, os.Mkdir(realDirectory, 0o700))
	linkedDirectory := filepath.Join(root, "linked")
	requireNoError(t, os.Symlink(realDirectory, linkedDirectory))
	if _, err := sqlitestore.NewMigrationLocker(filepath.Join(linkedDirectory, "storage.lock")); err == nil {
		t.Fatal("SQLite lock below symlinked parent was accepted")
	}
}

func testMigrationDirectorySymlink(t *testing.T, dialect migrate.Dialect) {
	root := newBoundaryRoot(t)
	realDirectory := filepath.Join(root, "real")
	requireNoError(t, os.Mkdir(realDirectory, 0o700))
	data := fixtureSQL(t, dialect, 1)
	requireNoError(t, os.WriteFile(filepath.Join(realDirectory, "0001_fixture.sql"), data, 0o600))
	requireNoError(t, os.Symlink(realDirectory, filepath.Join(root, "engine")))
	if _, err := migrate.LoadCatalog(os.DirFS(root), "engine", dialect); err == nil {
		t.Fatal("symlinked migration engine directory was accepted")
	}
}

func newBoundaryRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp(requireAbsoluteEnv(t, "AROP_P09_BACKUP_ROOT"), "boundary-")
	requireNoError(t, err)
	requireNoError(t, os.Chmod(root, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestUnitOfWorkConformance(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("success", func(t *testing.T) { testUoWSuccess(t, dialect) })
			t.Run("error", func(t *testing.T) { testUoWError(t, dialect) })
			t.Run("panic", func(t *testing.T) { testUoWPanic(t, dialect) })
			t.Run("cancel", func(t *testing.T) { testUoWCancel(t, dialect) })
			t.Run("nested-refusal", func(t *testing.T) { testUoWNested(t, dialect) })
		})
	}
}

func TestDurableObservationStoreConformance(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("atomic-pair", func(t *testing.T) { testDurableAtomicPair(t, dialect) })
			t.Run("query", func(t *testing.T) { testDurableQuery(t, dialect) })
			t.Run("truthful-readiness", func(t *testing.T) { testDurableReadiness(t, dialect) })
			t.Run("transaction-rollback", func(t *testing.T) { testDurableRollback(t, dialect) })
		})
	}
}

func TestDurableConfigurationAndReadiness(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("explicit-mode", func(t *testing.T) { testExplicitMode(t, dialect) })
			t.Run("no-fallback", func(t *testing.T) { testNoFallback(t, dialect) })
			t.Run("migration-gated-ready", func(t *testing.T) { testMigrationGatedReady(t, dialect) })
			t.Run("p08-regression", func(t *testing.T) { testP08ConfigurationRegression(t) })
		})
	}
}

func testEmptyToCurrent(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	catalog := fixtureCatalog(t, dialect, 2)
	runner := newRunner(t, b, catalog)
	result, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 0 || result.ToVersion != 2 || !equalInt64(result.Applied, []int64{1, 2}) {
		t.Fatalf("unexpected empty migration result: %+v", result)
	}
	requireReady(t, runner, 2)
	requireFixtureGeneration(t, b.db, true)
}

func testNMinusOne(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	backup := newBackup(t, b)
	v2 := newRunner(t, b, fixtureCatalog(t, dialect, 2), migrate.WithBackupRestore(backup))
	result, err := v2.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 1 || result.ToVersion != 2 || !equalInt64(result.Applied, []int64{2}) || result.Snapshot == nil {
		t.Fatalf("unexpected N-1 result: %+v", result)
	}
	requireReady(t, v2, 2)
	requireFixtureGeneration(t, b.db, true)
}

func testIdempotent(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, fixtureCatalog(t, dialect, 2))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	result, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 2 || result.ToVersion != 2 || len(result.Applied) != 0 || result.Snapshot != nil {
		t.Fatalf("idempotent rerun mutated: %+v", result)
	}
	requireHistoryCount(t, b.db, 2)
}

func testDirtyRefusal(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	fault := func(context.Context, string, int64) error { return errors.New("injected after dirty commit") }
	runner := newRunner(t, b, fixtureCatalog(t, dialect, 2), migrate.WithFaultHook(fault))
	if _, err := runner.Migrate(context.Background()); err == nil {
		t.Fatal("faulted migration succeeded")
	}
	status, err := runner.Status(context.Background())
	requireNoError(t, err)
	if status.Ready || status.Reason != "migration_history_incompatible" {
		t.Fatalf("dirty status was not fail-closed: %+v", status)
	}
	if _, err := runner.Migrate(context.Background()); err == nil {
		t.Fatal("dirty rerun succeeded")
	}
}

func testChecksumDrift(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	original := fixtureSQL(t, dialect, 1)
	changed := append(append([]byte{}, original...), []byte("\n-- drift\n")...)
	migration, err := migrate.NewMigration(1, "fixture", changed)
	requireNoError(t, err)
	catalog, err := migrate.NewCatalog(dialect, []migrate.Migration{migration})
	requireNoError(t, err)
	drifted := newRunner(t, b, catalog)
	if _, err = drifted.Migrate(context.Background()); err == nil {
		t.Fatal("checksum drift succeeded")
	}
	status, err := drifted.Status(context.Background())
	requireNoError(t, err)
	if status.Ready || status.Reason != "migration_history_incompatible" {
		t.Fatalf("drift readiness: %+v", status)
	}
}

func testAhead(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	full := newRunner(t, b, fixtureCatalog(t, dialect, 2))
	_, err := full.Migrate(context.Background())
	requireNoError(t, err)
	old := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	if _, err = old.Migrate(context.Background()); err == nil {
		t.Fatal("ahead database accepted")
	}
	status, err := old.Status(context.Background())
	requireNoError(t, err)
	if status.Ready || status.Reason != "migration_history_incompatible" {
		t.Fatalf("ahead readiness: %+v", status)
	}
}

func testGap(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, fixtureCatalog(t, dialect, 2))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	_, err = b.db.Exec(`DELETE FROM arop_schema_migrations WHERE version=1`)
	requireNoError(t, err)
	if _, err = runner.Migrate(context.Background()); err == nil {
		t.Fatal("migration history gap accepted")
	}
	status, err := runner.Status(context.Background())
	requireNoError(t, err)
	if status.Ready || status.Reason != "migration_history_incompatible" {
		t.Fatalf("gap readiness: %+v", status)
	}
}

func testSparseEmpty(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, sparseCatalog(t, dialect, 1, 5, 10, 20))
	result, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 0 || result.ToVersion != 20 || !equalInt64(result.Applied, []int64{1, 5, 10, 20}) {
		t.Fatalf("unexpected sparse migration result: %+v", result)
	}
	requireReady(t, runner, 20)
	requireHistoryCount(t, b.db, 4)
}

func testSparseOneToFive(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, sparseCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	v5 := newRunner(t, b, sparseCatalog(t, dialect, 1, 5), migrate.WithBackupRestore(newBackup(t, b)))
	result, err := v5.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 1 || result.ToVersion != 5 || !equalInt64(result.Applied, []int64{5}) || result.Snapshot == nil {
		t.Fatalf("unexpected sparse 1 to 5 result: %+v", result)
	}
	requireReady(t, v5, 5)
}

func testSparseIdempotent(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, sparseCatalog(t, dialect, 1, 5))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	result, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 5 || result.ToVersion != 5 || len(result.Applied) != 0 || result.Snapshot != nil {
		t.Fatalf("sparse idempotent rerun mutated: %+v", result)
	}
}

func testSparseDirty(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	fault := func(_ context.Context, _ string, version int64) error {
		if version == 5 {
			return errors.New("injected sparse migration fault")
		}
		return nil
	}
	runner := newRunner(t, b, sparseCatalog(t, dialect, 1, 5), migrate.WithFaultHook(fault))
	if _, err := runner.Migrate(context.Background()); err == nil {
		t.Fatal("dirty sparse migration succeeded")
	}
	status, err := runner.Status(context.Background())
	requireNoError(t, err)
	if status.Ready || status.Reason != "migration_history_incompatible" {
		t.Fatalf("dirty sparse readiness: %+v", status)
	}
}

func testSparseChecksum(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	original := sparseCatalog(t, dialect, 1, 5)
	_, err := newRunner(t, b, original).Migrate(context.Background())
	requireNoError(t, err)
	v1 := sparseMigration(t, dialect, 1)
	v5 := sparseMigration(t, dialect, 5)
	v5.SQL = append(append([]byte{}, v5.SQL...), []byte("\n-- checksum drift\n")...)
	v5.Checksum = ""
	drifted, err := migrate.NewCatalog(dialect, []migrate.Migration{v1, v5})
	requireNoError(t, err)
	if _, err := newRunner(t, b, drifted).Migrate(context.Background()); err == nil {
		t.Fatal("sparse checksum drift succeeded")
	}
}

func testSparseUnknownHistory(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	installed := newRunner(t, b, sparseCatalog(t, dialect, 1, 5))
	_, err := installed.Migrate(context.Background())
	requireNoError(t, err)
	_, err = b.db.Exec(`UPDATE arop_schema_migrations SET version=7,name='unknown' WHERE version=5`)
	requireNoError(t, err)
	if _, err := newRunner(t, b, sparseCatalog(t, dialect, 1, 5, 10)).Migrate(context.Background()); err == nil {
		t.Fatal("unknown sparse history version succeeded")
	}
}

func testSparseMissingHistory(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	catalog := sparseCatalog(t, dialect, 1, 5, 10)
	_, err := newRunner(t, b, catalog).Migrate(context.Background())
	requireNoError(t, err)
	_, err = b.db.Exec(`DELETE FROM arop_schema_migrations WHERE version=5`)
	requireNoError(t, err)
	if _, err := newRunner(t, b, catalog).Migrate(context.Background()); err == nil {
		t.Fatal("sparse history missing version 5 succeeded")
	}
}

func testSparseMissingDefinition(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	_, err := newRunner(t, b, sparseCatalog(t, dialect, 1, 5)).Migrate(context.Background())
	requireNoError(t, err)
	if _, err := newRunner(t, b, sparseCatalog(t, dialect, 1, 10)).Migrate(context.Background()); err == nil {
		t.Fatal("catalog missing already-applied version 5 succeeded")
	}
}

func testSparseDuplicateCatalog(t *testing.T, dialect migrate.Dialect) {
	v1 := sparseMigration(t, dialect, 1)
	v5 := sparseMigration(t, dialect, 5)
	if _, err := migrate.NewCatalog(dialect, []migrate.Migration{v1, v5, v5}); err == nil {
		t.Fatal("duplicate sparse migration version accepted")
	}
}

func testSparseUnorderedCatalog(t *testing.T, dialect migrate.Dialect) {
	v1 := sparseMigration(t, dialect, 1)
	v5 := sparseMigration(t, dialect, 5)
	if _, err := migrate.NewCatalog(dialect, []migrate.Migration{v5, v1}); err == nil {
		t.Fatal("unordered sparse migration catalog accepted")
	}
}

func testSchemaArtifactRefusal(t *testing.T, dialect migrate.Dialect, artifact string) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	switch artifact {
	case "index":
		_, err = b.db.Exec(`DROP INDEX arop_observations_request_idx`)
	case "check":
		err = removeObservationConstraint(t, b, "check")
	case "unique":
		err = removeObservationConstraint(t, b, "unique")
	default:
		t.Fatalf("unsupported schema artifact %q", artifact)
	}
	requireNoError(t, err)
	requireRunnerNotReady(t, runner)
}

func removeObservationConstraint(t *testing.T, b *backend, kind string) error {
	t.Helper()
	if b.dialect == migrate.DialectPostgres {
		name := map[string]string{
			"check":  "arop_observations_outcome_check",
			"unique": "arop_observations_trace_span_unique",
		}[kind]
		if name == "" {
			return fmt.Errorf("unsupported PostgreSQL constraint kind %q", kind)
		}
		_, err := b.db.Exec(`ALTER TABLE arop_observations DROP CONSTRAINT "` + name + `"`)
		return err
	}
	var definition string
	if err := b.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='arop_observations'`).Scan(&definition); err != nil {
		return err
	}
	needle := "  CONSTRAINT arop_observations_trace_span_unique UNIQUE(trace_id, span_id),\n"
	if kind == "check" {
		needle = "  CONSTRAINT arop_observations_outcome_check CHECK(outcome IN ('succeeded','rejected','failed')),\n"
	}
	weakened := strings.Replace(definition, needle, "", 1)
	if weakened == definition {
		return fmt.Errorf("SQLite %s constraint text was not found", kind)
	}
	statements := []string{
		`ALTER TABLE arop_observations RENAME TO arop_observations_original`,
		weakened,
		`DROP TABLE arop_observations_original`,
		`CREATE INDEX arop_observations_request_idx ON arop_observations(request_id, occurred_at_ns, audit_id)`,
		`CREATE INDEX arop_observations_trace_idx ON arop_observations(trace_id, occurred_at_ns, audit_id)`,
		`CREATE INDEX arop_observations_operation_idx ON arop_observations(operation, occurred_at_ns, audit_id)`,
	}
	for _, statement := range statements {
		if _, err := b.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func testUnmanagedDatabaseRefusal(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	_, err := b.db.Exec(`CREATE TABLE unmanaged_business_data(id BIGINT PRIMARY KEY, value TEXT NOT NULL)`)
	requireNoError(t, err)
	_, err = b.db.Exec(`INSERT INTO unmanaged_business_data(id,value) VALUES(1,'must-survive')`)
	requireNoError(t, err)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	if _, err = runner.Migrate(context.Background()); err == nil {
		t.Fatal("unmanaged non-empty database was adopted")
	}
	var value string
	requireNoError(t, b.db.QueryRow(`SELECT value FROM unmanaged_business_data WHERE id=1`).Scan(&value))
	if value != "must-survive" {
		t.Fatalf("unmanaged row changed to %q", value)
	}
	var historyCount int
	if dialect == migrate.DialectSQLite {
		requireNoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='arop_schema_migrations'`).Scan(&historyCount))
	} else {
		requireNoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='arop_schema_migrations'`).Scan(&historyCount))
	}
	if historyCount != 0 {
		t.Fatal("failed unmanaged adoption created migration history")
	}
}

func testPostgresNonPublicSchemaRefusal(t *testing.T) {
	b := newBackend(t, migrate.DialectPostgres)
	_, err := b.db.Exec(`CREATE SCHEMA extra; CREATE TABLE extra.unmanaged_business_data(id BIGINT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO extra.unmanaged_business_data(id,value) VALUES(1,'must-survive')`)
	requireNoError(t, err)
	runner := newRunner(t, b, baseCatalog(t, migrate.DialectPostgres), migrate.WithVerifier(durable.VerifySchema(migrate.DialectPostgres)))
	requireRunnerNotReady(t, runner)
	if _, err = runner.Migrate(context.Background()); err == nil {
		t.Fatal("database with non-public user schema was adopted")
	}
	var value string
	requireNoError(t, b.db.QueryRow(`SELECT value FROM extra.unmanaged_business_data WHERE id=1`).Scan(&value))
	if value != "must-survive" {
		t.Fatalf("non-public unmanaged row changed to %q", value)
	}
	var historyCount int
	requireNoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='arop_schema_migrations'`).Scan(&historyCount))
	if historyCount != 0 {
		t.Fatal("failed non-public adoption created migration history")
	}
}

func testCounterfeitSchemaRefusal(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	_, err := b.db.Exec(`CREATE TABLE arop_schema_migrations(version BIGINT, name TEXT, checksum TEXT, dirty BOOLEAN)`)
	requireNoError(t, err)
	contents, err := os.ReadFile(filepath.Join(requireAbsoluteEnv(t, "AROP_P09_MIGRATION_ROOT"), string(dialect), "0001_base.sql"))
	requireNoError(t, err)
	digest := sha256.Sum256(contents)
	statement := `INSERT INTO arop_schema_migrations(version,name,checksum,dirty) VALUES(?,?,?,?)`
	if dialect == migrate.DialectPostgres {
		statement = `INSERT INTO arop_schema_migrations(version,name,checksum,dirty) VALUES($1,$2,$3,$4)`
	}
	_, err = b.db.Exec(statement, 1, "base", hex.EncodeToString(digest[:]), false)
	requireNoError(t, err)
	_, err = b.db.Exec(`CREATE TABLE arop_observations(
audit_id TEXT, occurred_at_ns BIGINT, request_id TEXT, trace_id TEXT, operation TEXT, outcome TEXT,
http_status INTEGER, span_id TEXT, parent_span_id TEXT, started_at_ns BIGINT, ended_at_ns BIGINT, span_status TEXT)`)
	requireNoError(t, err)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	requireRunnerNotReady(t, runner)
	if _, err = runner.Migrate(context.Background()); err == nil {
		t.Fatal("counterfeit weak schema was accepted by migration")
	}
}

func testCounterfeitHistoryConstraintRefusal(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	statement := `CREATE TABLE arop_schema_migrations (
version INTEGER NOT NULL,
name TEXT NOT NULL,
checksum TEXT NOT NULL,
dirty INTEGER NOT NULL,
started_at_ns INTEGER NOT NULL,
applied_at_ns INTEGER,
CONSTRAINT arop_schema_migrations_pkey PRIMARY KEY(version),
CONSTRAINT arop_schema_migrations_version_check CHECK((version BETWEEN 1 AND 9999) OR 1=1),
CONSTRAINT arop_schema_migrations_name_check CHECK(length(name) BETWEEN 1 AND 128),
CONSTRAINT arop_schema_migrations_checksum_check CHECK(length(checksum) = 64 AND checksum NOT GLOB '*[^0-9a-f]*'),
CONSTRAINT arop_schema_migrations_dirty_check CHECK(dirty IN (0,1)),
CONSTRAINT arop_schema_migrations_started_check CHECK(started_at_ns > 0),
CONSTRAINT arop_schema_migrations_applied_check CHECK(applied_at_ns IS NULL OR applied_at_ns >= started_at_ns)
)`
	if dialect == migrate.DialectPostgres {
		statement = `CREATE TABLE arop_schema_migrations (
version BIGINT NOT NULL,
name TEXT NOT NULL,
checksum TEXT NOT NULL,
dirty BOOLEAN NOT NULL,
started_at_ns BIGINT NOT NULL,
applied_at_ns BIGINT,
CONSTRAINT arop_schema_migrations_pkey PRIMARY KEY(version),
CONSTRAINT arop_schema_migrations_version_check CHECK((version BETWEEN 1 AND 9999) OR 1=1),
CONSTRAINT arop_schema_migrations_name_check CHECK(length(name) BETWEEN 1 AND 128),
CONSTRAINT arop_schema_migrations_checksum_check CHECK(checksum ~ '^[0-9a-f]{64}$'),
CONSTRAINT arop_schema_migrations_started_check CHECK(started_at_ns > 0),
CONSTRAINT arop_schema_migrations_applied_check CHECK(applied_at_ns IS NULL OR applied_at_ns >= started_at_ns)
)`
	}
	_, err := b.db.Exec(statement)
	requireNoError(t, err)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	requireRunnerNotReady(t, runner)
	if _, err = runner.Migrate(context.Background()); err == nil {
		t.Fatal("counterfeit migration-history constraint body was accepted")
	}
}

func testReadOnlyRefusal(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	b.db.SetMaxOpenConns(1)
	b.db.SetMaxIdleConns(1)
	statement := `PRAGMA query_only=ON`
	if dialect == migrate.DialectPostgres {
		statement = `SET default_transaction_read_only=on`
	}
	_, err = b.db.Exec(statement)
	requireNoError(t, err)
	requireRunnerNotReady(t, runner)
}

func requireRunnerNotReady(t *testing.T, runner *migrate.Runner) {
	t.Helper()
	if err := runner.Check(context.Background()); err == nil {
		t.Fatal("damaged storage reported ready")
	}
	status, err := runner.Status(context.Background())
	if err == nil && status.Ready {
		t.Fatalf("damaged storage status reported ready: %+v", status)
	}
}

func testConcurrent(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	database := b.path
	if dialect == migrate.DialectPostgres {
		database = b.dsn
	}
	commands := make([]*exec.Cmd, 6)
	outputs := make([]*bytes.Buffer, len(commands))
	barrier := t.TempDir()
	processContext, cancelProcesses := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelProcesses()
	for index := range commands {
		command := exec.CommandContext(processContext, os.Args[0], "-test.run=^TestMigrationProcessWorker$", "-test.count=1")
		command.Env = append(cleanProcessEnvironment(os.Environ()),
			"AROP_P09_PROCESS_DIALECT="+string(dialect),
			"AROP_P09_PROCESS_DATABASE="+database,
			"AROP_P09_PROCESS_BARRIER="+barrier,
			"AROP_P09_ENGINE_FIXTURES="+requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES"),
		)
		outputs[index] = &bytes.Buffer{}
		command.Stdout, command.Stderr = outputs[index], outputs[index]
		commands[index] = command
	}
	for index := range commands {
		requireNoError(t, commands[index].Start())
	}
	waitForProcessFiles(t, barrier, "ready-", len(commands))
	requireNoError(t, os.WriteFile(filepath.Join(barrier, "start"), []byte("start\n"), 0o600))
	for index := range commands {
		err := commands[index].Wait()
		if err != nil {
			detail := strings.ReplaceAll(outputs[index].String(), database, "<database>")
			detail = strings.ReplaceAll(detail, requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES"), "<engine-fixtures>")
			if len(detail) > 2048 {
				detail = detail[:2048]
			}
			t.Fatalf("migration subprocess %d failed: %v (sha256=%s bytes=%d output=%q)", index, err, hash(outputs[index].Bytes()), outputs[index].Len(), detail)
		}
	}
	waitForProcessFiles(t, barrier, "result-", len(commands))
	entries, err := os.ReadDir(barrier)
	requireNoError(t, err)
	appliers := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "result-") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(barrier, entry.Name()))
		requireNoError(t, readErr)
		if strings.TrimSpace(string(data)) != "0" {
			appliers++
		}
	}
	if appliers != 1 {
		t.Fatalf("concurrent migrations with applied versions=%d want=1", appliers)
	}
	catalog := fixtureCatalog(t, dialect, 2)
	runner := newRunner(t, b, catalog, migrate.WithVerifier(fixtureSchemaVerifier(2)))
	requireHistoryCount(t, b.db, 2)
	requireReady(t, runner, 2)
}

func cleanProcessEnvironment(_ []string) []string {
	return []string{"HOME=/nonexistent", "LANG=C", "LC_ALL=C", "TZ=UTC"}
}

func waitForProcessFiles(t *testing.T, directory, prefix string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(directory)
		requireNoError(t, err)
		count := 0
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
				count++
			}
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process barrier %s count=%d want=%d", prefix, count, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fixtureSchemaVerifier(target int64) migrate.Verifier {
	return func(ctx context.Context, queryer migrate.Queryer) error {
		rows, err := queryer.QueryContext(ctx, `SELECT fixture_id,fixture_value FROM arop_engine_fixture WHERE 1=0`)
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		generation, generationErr := queryer.QueryContext(ctx, `SELECT generation FROM arop_engine_fixture WHERE 1=0`)
		if target == 1 {
			if generationErr == nil {
				_ = generation.Close()
				return errors.New("fixture v1 unexpectedly has generation column")
			}
			return nil
		}
		if generationErr != nil {
			return generationErr
		}
		return generation.Close()
	}
}

func testBackupRestore(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	backup := newBackup(t, b)
	failingVerifier := func(ctx context.Context, queryer migrate.Queryer) error {
		if err := fixtureSchemaVerifier(2)(ctx, queryer); err != nil {
			return err
		}
		return errors.New("injected final verifier failure after v2 apply")
	}
	upgrade := newRunner(t, b, fixtureCatalog(t, dialect, 2), migrate.WithBackupRestore(backup), migrate.WithVerifier(failingVerifier))
	if _, err = upgrade.Migrate(context.Background()); err == nil {
		t.Fatal("faulted upgrade succeeded")
	}
	if backup.restoreCount() != 1 {
		t.Fatalf("restore count=%d want=1 (migration error: %v)", backup.restoreCount(), err)
	}
	reopened := reopenBackend(t, b)
	v1Again := newRunner(t, reopened, fixtureCatalog(t, dialect, 1))
	requireReady(t, v1Again, 1)
	requireExactV1Fixture(t, reopened.db)
	requireFixtureSentinel(t, reopened.db)
}

func testProductionBackupUpgrade(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	backup := newBackup(t, b)
	v2 := newRunner(t, b, fixtureCatalog(t, dialect, 2), migrate.WithBackupRestore(backup))
	result, err := v2.Migrate(context.Background())
	requireNoError(t, err)
	if result.Snapshot == nil || result.FromVersion != 1 || result.ToVersion != 2 || !equalInt64(result.Applied, []int64{2}) {
		t.Fatalf("production backup upgrade result=%+v", result)
	}
	requireReady(t, v2, 2)
	requireFixtureSentinel(t, b.db)
}

func testSnapshotRefusal(t *testing.T, dialect migrate.Dialect, mode string) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	backup := newBackup(t, b)
	snapshot, err := backup.Create(context.Background(), 1, 2)
	requireNoError(t, err)
	before := fixtureLogicalState(t, b.db)
	candidate := snapshot
	switch mode {
	case "corrupt":
		path := filepath.Join(backup.directory, snapshot.ID)
		data, readErr := os.ReadFile(path)
		requireNoError(t, readErr)
		if len(data) < 16 {
			t.Fatal("snapshot too small for corruption probe")
		}
		data[len(data)/2] ^= 0xff
		requireNoError(t, os.WriteFile(path, data, 0o600))
	case "truncated":
		path := filepath.Join(backup.directory, snapshot.ID)
		info, statErr := os.Stat(path)
		requireNoError(t, statErr)
		requireNoError(t, os.Truncate(path, info.Size()/2))
	case "wrong":
		candidate.ID = "../" + snapshot.ID
	default:
		t.Fatalf("unknown snapshot refusal mode %q", mode)
	}
	if err := backup.Restore(context.Background(), candidate); err == nil {
		t.Fatalf("%s snapshot was restored", mode)
	}
	if after := fixtureLogicalState(t, b.db); after != before {
		t.Fatalf("%s snapshot refusal mutated database: before=%q after=%q", mode, before, after)
	}
	requireReady(t, v1, 1)
}

func testRestoreFailurePreservesOriginal(t *testing.T, dialect migrate.Dialect) {
	if dialect == migrate.DialectSQLite {
		t.Run("mid-copy-cancel", testSQLiteMidCopyRestoreFailure)
		return
	}
	t.Run("archive-rendering", testPostgresArchiveRenderingFailure)
	t.Run("transaction-client", testPostgresTransactionRestoreFailure)
}

func testSQLiteMidCopyRestoreFailure(t *testing.T) {
	b := newBackend(t, migrate.DialectSQLite)
	v1 := newRunner(t, b, fixtureCatalog(t, migrate.DialectSQLite, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	_, err = b.db.Exec(`CREATE TABLE p09_restore_payload(id INTEGER PRIMARY KEY, value BLOB NOT NULL); INSERT INTO p09_restore_payload(id,value) VALUES(1,zeroblob(67108864))`)
	requireNoError(t, err)
	backup := newBackup(t, b)
	snapshot, err := backup.Create(context.Background(), 1, 2)
	requireNoError(t, err)
	_, err = b.db.Exec(`UPDATE arop_engine_fixture SET fixture_value='post-snapshot-state' WHERE fixture_id='acceptance-sentinel'`)
	requireNoError(t, err)
	_, _ = b.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	beforeLogical := fixtureLogicalState(t, b.db)
	beforeBytes, err := fileDigest(b.path)
	requireNoError(t, err)
	restoreErr, writeObserved := cancelSQLiteRestoreAfterWrite(b.path, func(ctx context.Context) error {
		return backup.Restore(ctx, snapshot)
	})
	if !writeObserved {
		t.Fatalf("SQLite restore ended before an actual database write was observed: %v", restoreErr)
	}
	if restoreErr == nil {
		t.Fatal("cancelled mid-copy SQLite restore reported success")
	}
	if after := fixtureLogicalState(t, b.db); after != beforeLogical {
		t.Fatalf("failed SQLite restore changed logical state: before=%q after=%q", beforeLogical, after)
	}
	_, _ = b.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	afterBytes, err := fileDigest(b.path)
	requireNoError(t, err)
	if afterBytes != beforeBytes {
		t.Fatalf("failed SQLite restore changed database bytes: before=%s after=%s", beforeBytes, afterBytes)
	}
	if err := backup.Restore(context.Background(), snapshot); err != nil {
		t.Fatalf("production SQLite restore retry failed: %v", err)
	}
	requireExactV1Fixture(t, b.db)
	requireFixtureSentinel(t, b.db)
	requireReady(t, v1, 1)
}

func cancelSQLiteRestoreAfterWrite(databasePath string, restore func(context.Context) error) (error, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := sqliteWriteStamp(databasePath)
	done := make(chan error, 1)
	go func() { done <- restore(ctx) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case err := <-done:
			return err, false
		case <-ticker.C:
			if sqliteWriteStamp(databasePath) != before {
				cancel()
				return <-done, true
			}
		case <-deadline.C:
			cancel()
			return <-done, false
		}
	}
}

func sqliteWriteStamp(databasePath string) string {
	parts := make([]string, 0, 3)
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		info, err := os.Stat(path)
		if err == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", filepath.Base(path), info.Size(), info.ModTime().UnixNano()))
		} else {
			parts = append(parts, filepath.Base(path)+":missing")
		}
	}
	return strings.Join(parts, "|")
}

func newPostgresRestoreScenario(t *testing.T, extraSchema bool) (*backend, *migrate.Runner, *productionBackupHarness, migrate.Snapshot, migrate.Snapshot) {
	t.Helper()
	b := newBackend(t, migrate.DialectPostgres)
	v1 := newRunner(t, b, fixtureCatalog(t, migrate.DialectPostgres, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	_, err = b.db.Exec(`CREATE TABLE p09_restore_payload(id BIGINT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO p09_restore_payload(id,value) SELECT value,md5(value::text)||md5((value+1)::text) FROM generate_series(1,50000) AS value`)
	requireNoError(t, err)
	backup := newBackup(t, b)
	var failureSnapshot, retrySnapshot migrate.Snapshot
	if extraSchema {
		retrySnapshot, err = backup.Create(context.Background(), 1, 2)
		requireNoError(t, err)
		_, err = b.db.Exec(`CREATE SCHEMA extra; CREATE TABLE extra.restore_guard(id BIGINT PRIMARY KEY, value TEXT NOT NULL); INSERT INTO extra.restore_guard(id,value) VALUES(1,'must-survive')`)
		requireNoError(t, err)
		failureSnapshot, err = backup.Create(context.Background(), 1, 2)
		requireNoError(t, err)
	} else {
		failureSnapshot, err = backup.Create(context.Background(), 1, 2)
		requireNoError(t, err)
		retrySnapshot, err = backup.Create(context.Background(), 1, 2)
		requireNoError(t, err)
	}
	_, err = b.db.Exec(`UPDATE arop_engine_fixture SET fixture_value='post-snapshot-state' WHERE fixture_id='acceptance-sentinel'`)
	requireNoError(t, err)
	return b, v1, backup, failureSnapshot, retrySnapshot
}

func testPostgresArchiveRenderingFailure(t *testing.T) {
	b, v1, backup, snapshot, retrySnapshot := newPostgresRestoreScenario(t, false)
	before := fixtureLogicalState(t, b.db)
	snapshot = corruptPostgresSnapshotForStreaming(t, backup, snapshot)
	restoreErr := backup.Restore(context.Background(), snapshot)
	if restoreErr == nil || !strings.Contains(restoreErr.Error(), "archive rendering failed") {
		t.Fatalf("corrupt PostgreSQL archive did not fail in pre-render: %v", restoreErr)
	}
	if after := fixtureLogicalState(t, b.db); after != before {
		t.Fatalf("failed PostgreSQL archive rendering changed database: before=%q after=%q", before, after)
	}
	if err := backup.Restore(context.Background(), retrySnapshot); err != nil {
		t.Fatalf("production restore retry failed: %v (%s)", err, diagnosePostgresRestore(t, migrate.DialectPostgres, backup, retrySnapshot))
	}
	requireExactV1Fixture(t, b.db)
	requireFixtureSentinel(t, b.db)
	requireReady(t, v1, 1)
}

func testPostgresTransactionRestoreFailure(t *testing.T) {
	b, v1, backup, snapshot, retrySnapshot := newPostgresRestoreScenario(t, true)
	before := fixtureLogicalState(t, b.db)
	restoreErr := backup.Restore(context.Background(), snapshot)
	if restoreErr == nil || !strings.Contains(restoreErr.Error(), "restore client failed") {
		t.Fatalf("PostgreSQL transaction conflict did not fail in psql: %v", restoreErr)
	}
	if after := fixtureLogicalState(t, b.db); after != before {
		t.Fatalf("failed PostgreSQL transactional restore changed public database: before=%q after=%q", before, after)
	}
	var extraValue string
	requireNoError(t, b.db.QueryRow(`SELECT value FROM extra.restore_guard WHERE id=1`).Scan(&extraValue))
	if extraValue != "must-survive" {
		t.Fatalf("failed PostgreSQL transactional restore changed non-public sentinel: %q", extraValue)
	}
	requireReady(t, v1, 1)
	_, err := b.db.Exec(`DROP SCHEMA extra CASCADE`)
	requireNoError(t, err)
	if err := backup.Restore(context.Background(), retrySnapshot); err != nil {
		t.Fatalf("production restore retry failed: %v (%s)", err, diagnosePostgresRestore(t, migrate.DialectPostgres, backup, retrySnapshot))
	}
	requireExactV1Fixture(t, b.db)
	requireFixtureSentinel(t, b.db)
	requireReady(t, v1, 1)
}

func corruptPostgresSnapshotForStreaming(t *testing.T, backup *productionBackupHarness, snapshot migrate.Snapshot) migrate.Snapshot {
	t.Helper()
	archive := filepath.Join(backup.directory, snapshot.ID)
	info, err := os.Stat(archive)
	requireNoError(t, err)
	if info.Size() < 32<<10 {
		t.Fatalf("PostgreSQL snapshot is too small for a stream corruption probe: %d", info.Size())
	}
	cut := int64(4096)
	if info.Size()/16 < cut {
		cut = info.Size() / 16
	}
	requireNoError(t, os.Truncate(archive, info.Size()-cut))
	list := exec.Command(filepath.Join(requireAbsoluteEnv(t, "AROP_P09_POSTGRES_BIN"), "pg_restore"), "--list", archive)
	list.Env = []string{"HOME=/nonexistent", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	if output, listErr := list.CombinedOutput(); listErr != nil {
		t.Fatalf("corrupted archive did not reach the restore stream (list sha256=%s bytes=%d): %v", hash(output), len(output), listErr)
	}
	digest, err := fileDigest(archive)
	requireNoError(t, err)
	metadataPath := archive + ".metadata.json"
	data, err := os.ReadFile(metadataPath)
	requireNoError(t, err)
	var metadata map[string]any
	requireNoError(t, json.Unmarshal(data, &metadata))
	metadata["digest"] = digest
	data, err = json.Marshal(metadata)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(metadataPath, append(data, '\n'), 0o600))
	snapshot.Digest = digest
	return snapshot
}

func testSnapshotMetadataSurvivesRestart(t *testing.T, dialect migrate.Dialect) {
	b := newBackend(t, dialect)
	v1 := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	_, err := v1.Migrate(context.Background())
	requireNoError(t, err)
	insertFixtureSentinel(t, b.db)
	backup := newBackup(t, b)
	snapshot, err := backup.Create(context.Background(), 1, 2)
	requireNoError(t, err)
	v2 := newRunner(t, b, fixtureCatalog(t, dialect, 2), migrate.WithBackupRestore(backup))
	_, err = v2.Migrate(context.Background())
	requireNoError(t, err)
	requireReady(t, v2, 2)
	restartedAdapter := newBackupInDirectory(t, b, backup.directory)
	if err := restartedAdapter.Restore(context.Background(), snapshot); err != nil {
		t.Fatalf("production restore after adapter restart failed: %v (%s)", err, diagnosePostgresRestore(t, dialect, backup, snapshot))
	}
	v1Again := newRunner(t, b, fixtureCatalog(t, dialect, 1))
	requireReady(t, v1Again, 1)
	requireExactV1Fixture(t, b.db)
	requireFixtureSentinel(t, b.db)
}

func setupUoWTable(t *testing.T, b *backend) {
	t.Helper()
	_, err := b.db.Exec(`CREATE TABLE p09_uow_probe(id INTEGER PRIMARY KEY, value TEXT NOT NULL)`)
	requireNoError(t, err)
}
func uowInsert(ctx context.Context, uow unitOfWork, dialect migrate.Dialect, id int) error {
	tx, ok := uow.Transaction(ctx)
	if !ok {
		return errors.New("transaction missing from context")
	}
	statement := `INSERT INTO p09_uow_probe(id,value) VALUES(?,?)`
	if dialect == migrate.DialectPostgres {
		statement = `INSERT INTO p09_uow_probe(id,value) VALUES($1,$2)`
	}
	_, err := tx.ExecContext(ctx, statement, id, "value")
	return err
}
func requireUoWCount(t *testing.T, b *backend, want int) {
	t.Helper()
	var count int
	requireNoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM p09_uow_probe`).Scan(&count))
	if count != want {
		t.Fatalf("uow row count=%d want=%d", count, want)
	}
}
func testUoWSuccess(t *testing.T, d migrate.Dialect) {
	b := newBackend(t, d)
	setupUoWTable(t, b)
	requireNoError(t, b.uow.Within(context.Background(), func(ctx context.Context) error { return uowInsert(ctx, b.uow, d, 1) }))
	requireUoWCount(t, b, 1)
}
func testUoWError(t *testing.T, d migrate.Dialect) {
	b := newBackend(t, d)
	setupUoWTable(t, b)
	sentinel := errors.New("callback failed")
	err := b.uow.Within(context.Background(), func(ctx context.Context) error { requireNoError(t, uowInsert(ctx, b.uow, d, 1)); return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error lost: %v", err)
	}
	requireUoWCount(t, b, 0)
}
func testUoWPanic(t *testing.T, d migrate.Dialect) {
	b := newBackend(t, d)
	setupUoWTable(t, b)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed")
			}
		}()
		_ = b.uow.Within(context.Background(), func(ctx context.Context) error { requireNoError(t, uowInsert(ctx, b.uow, d, 1)); panic("p09 panic") })
	}()
	requireUoWCount(t, b, 0)
}
func testUoWCancel(t *testing.T, d migrate.Dialect) {
	b := newBackend(t, d)
	setupUoWTable(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	err := b.uow.Within(ctx, func(txContext context.Context) error {
		requireNoError(t, uowInsert(txContext, b.uow, d, 1))
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result=%v", err)
	}
	requireUoWCount(t, b, 0)
}
func testUoWNested(t *testing.T, d migrate.Dialect) {
	b := newBackend(t, d)
	setupUoWTable(t, b)
	err := b.uow.Within(context.Background(), func(ctx context.Context) error { return b.uow.Within(ctx, func(context.Context) error { return nil }) })
	if err == nil {
		t.Fatal("nested unit of work succeeded")
	}
	requireUoWCount(t, b, 0)
}

func testDurableAtomicPair(t *testing.T, d migrate.Dialect) {
	_, _, store := newDurable(t, d, true)
	audit, span := observation()
	span.TraceID = "22222222222222222222222222222222"
	if err := store.AppendObservation(context.Background(), audit, span); err == nil {
		t.Fatal("mismatched pair persisted")
	}
	requireObservationCounts(t, store, 0)
}
func testDurableQuery(t *testing.T, d migrate.Dialect) {
	_, _, store := newDurable(t, d, true)
	audit, span := observation()
	requireNoError(t, store.AppendObservation(context.Background(), audit, span))
	audits, err := store.QueryAudit(context.Background(), observability.AuditQuery{RequestID: audit.RequestID})
	requireNoError(t, err)
	spans, err := store.QueryTrace(context.Background(), observability.TraceQuery{TraceID: audit.TraceID})
	requireNoError(t, err)
	if len(audits) != 1 || len(spans) != 1 || audits[0] != audit || spans[0] != span {
		t.Fatalf("durable query mismatch: %#v %#v", audits, spans)
	}
}
func testDurableReadiness(t *testing.T, d migrate.Dialect) {
	_, runner, store := newDurable(t, d, false)
	if store.AuditHealth(context.Background()) == nil || store.TraceHealth(context.Background()) == nil {
		t.Fatal("unmigrated store reported healthy")
	}
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	requireNoError(t, store.AuditHealth(context.Background()))
	requireNoError(t, store.TraceHealth(context.Background()))
}
func testDurableRollback(t *testing.T, d migrate.Dialect) {
	b, _, store := newDurable(t, d, true)
	sentinel := errors.New("rollback observation")
	err := b.uow.Within(context.Background(), func(ctx context.Context) error {
		audit, span := observation()
		requireNoError(t, store.AppendObservation(ctx, audit, span))
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error lost: %v", err)
	}
	requireObservationCounts(t, store, 0)
}

func testExplicitMode(t *testing.T, d migrate.Dialect) {
	root := requireAbsoluteEnv(t, "AROP_P09_MIGRATION_ROOT")
	config := platform.DefaultConfig()
	config.MigrationRoot = root
	config.BackupDirectory = newControlledBackupDirectory(t)
	if d == migrate.DialectSQLite {
		config.Mode = platform.ModeSQLite
		config.DatabaseDSN = filepath.Join(requireAbsoluteEnv(t, "AROP_P09_SQLITE_ROOT"), "explicit-mode.db")
	} else {
		config.Mode = platform.ModePostgres
		config.DatabaseDSN = requireEnv(t, "AROP_P09_POSTGRES_URL")
	}
	requireNoError(t, config.Validate())
}
func testNoFallback(t *testing.T, d migrate.Dialect) {
	config := platform.DefaultConfig()
	if d == migrate.DialectSQLite {
		config.Mode = platform.ModeSQLite
		config.DatabaseDSN = ""
	} else {
		config.Mode = platform.ModePostgres
		config.DatabaseDSN = ""
	}
	config.MigrationRoot = requireAbsoluteEnv(t, "AROP_P09_MIGRATION_ROOT")
	config.BackupDirectory = newControlledBackupDirectory(t)
	if config.Validate() == nil {
		t.Fatal("durable mode silently fell back without database")
	}
}
func testMigrationGatedReady(t *testing.T, d migrate.Dialect) {
	_, runner, store := newDurable(t, d, false)
	if store.AuditHealth(context.Background()) == nil {
		t.Fatal("readiness passed before migration")
	}
	_, err := runner.Migrate(context.Background())
	requireNoError(t, err)
	requireNoError(t, store.AuditHealth(context.Background()))
}
func testP08ConfigurationRegression(t *testing.T) {
	config := platform.DefaultConfig()
	if config.Mode != platform.ModeDevelopmentMemory {
		t.Fatalf("default mode=%s", config.Mode)
	}
	requireNoError(t, config.Validate())
}

func newBackend(t *testing.T, dialect migrate.Dialect) *backend {
	t.Helper()
	sequence := databaseSequence.Add(1)
	switch dialect {
	case migrate.DialectSQLite:
		root := requireAbsoluteEnv(t, "AROP_P09_SQLITE_ROOT")
		path := filepath.Join(root, fmt.Sprintf("case-%03d.db", sequence))
		db, err := sqlitestore.Open(path)
		requireNoError(t, err)
		uow, err := sqlitestore.NewUnitOfWork(db)
		requireNoError(t, err)
		locker, err := sqlitestore.NewMigrationLocker(path + ".migration.lock")
		requireNoError(t, err)
		b := &backend{dialect: dialect, db: db, locker: locker, uow: uow, path: path}
		b.close = func() { _ = db.Close() }
		t.Cleanup(b.close)
		return b
	case migrate.DialectPostgres:
		base := requireEnv(t, "AROP_P09_POSTGRES_URL")
		admin, err := pgstore.Open(context.Background(), base)
		requireNoError(t, err)
		name := fmt.Sprintf("arop_p09_case_%03d", sequence)
		_, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+name)
		requireNoError(t, err)
		parsed, err := url.Parse(base)
		requireNoError(t, err)
		parsed.Path = "/" + name
		dsn := parsed.String()
		db, err := pgstore.Open(context.Background(), dsn)
		requireNoError(t, err)
		uow, err := pgstore.NewUnitOfWork(db)
		requireNoError(t, err)
		locker, err := pgstore.NewMigrationLocker(db, migrationLockKey+int64(sequence))
		requireNoError(t, err)
		b := &backend{dialect: dialect, db: db, locker: locker, uow: uow, dsn: dsn}
		b.close = func() {
			_ = db.Close()
			_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
			_ = admin.Close()
		}
		t.Cleanup(b.close)
		return b
	default:
		t.Fatalf("unsupported dialect %s", dialect)
		return nil
	}
}

func freshLocker(t *testing.T, b *backend) migrate.Locker {
	t.Helper()
	if b.dialect == migrate.DialectSQLite {
		locker, err := sqlitestore.NewMigrationLocker(b.path + ".migration.lock")
		requireNoError(t, err)
		return locker
	}
	locker, err := pgstore.NewMigrationLocker(b.db, migrationLockKey+int64(databaseSequence.Load()))
	requireNoError(t, err)
	return locker
}

func fixtureCatalog(t *testing.T, dialect migrate.Dialect, target int) *migrate.Catalog {
	t.Helper()
	if target < 1 || target > 2 {
		t.Fatalf("invalid fixture target %d", target)
	}
	root := requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES")
	if target == 2 {
		catalog, err := migrate.LoadCatalog(os.DirFS(root), string(dialect), dialect)
		requireNoError(t, err)
		return catalog
	}
	data := fixtureSQL(t, dialect, 1)
	catalog, err := migrate.LoadCatalog(fstest.MapFS{string(dialect) + "/0001_fixture.sql": &fstest.MapFile{Data: data, Mode: 0o644}}, string(dialect), dialect)
	requireNoError(t, err)
	return catalog
}

func fixtureSQL(t *testing.T, dialect migrate.Dialect, version int) []byte {
	t.Helper()
	root := requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES")
	data, err := os.ReadFile(filepath.Join(root, string(dialect), fmt.Sprintf("%04d_fixture.sql", version)))
	requireNoError(t, err)
	return data
}

func sparseCatalog(t *testing.T, dialect migrate.Dialect, versions ...int) *migrate.Catalog {
	t.Helper()
	if len(versions) == 4 && equalInts(versions, []int{1, 5, 10, 20}) {
		root := requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES")
		catalog, err := migrate.LoadCatalog(os.DirFS(root), filepath.ToSlash(filepath.Join("sparse", string(dialect))), dialect)
		requireNoError(t, err)
		return catalog
	}
	migrations := make([]migrate.Migration, 0, len(versions))
	for _, version := range versions {
		migrations = append(migrations, sparseMigration(t, dialect, version))
	}
	catalog, err := migrate.NewCatalog(dialect, migrations)
	requireNoError(t, err)
	return catalog
}

func sparseMigration(t *testing.T, dialect migrate.Dialect, version int) migrate.Migration {
	t.Helper()
	names := map[int]string{1: "sparse_base", 5: "sparse_generation", 10: "sparse_timestamp", 20: "sparse_index"}
	name, ok := names[version]
	if !ok {
		t.Fatalf("unsupported sparse migration version %d", version)
	}
	root := requireAbsoluteEnv(t, "AROP_P09_ENGINE_FIXTURES")
	data, err := os.ReadFile(filepath.Join(root, "sparse", string(dialect), fmt.Sprintf("%04d_%s.sql", version, name)))
	requireNoError(t, err)
	migration, err := migrate.NewMigration(int64(version), name, data)
	requireNoError(t, err)
	return migration
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func baseCatalog(t *testing.T, dialect migrate.Dialect) *migrate.Catalog {
	t.Helper()
	root := requireAbsoluteEnv(t, "AROP_P09_MIGRATION_ROOT")
	catalog, err := migrate.LoadCatalog(os.DirFS(root), string(dialect), dialect)
	requireNoError(t, err)
	return catalog
}

func newRunner(t *testing.T, b *backend, catalog *migrate.Catalog, options ...migrate.Option) *migrate.Runner {
	t.Helper()
	options = append([]migrate.Option{migrate.WithVerifier(fixtureSchemaVerifier(catalog.TargetVersion()))}, options...)
	runner, err := migrate.NewRunner(b.db, catalog, b.locker, options...)
	requireNoError(t, err)
	return runner
}
func newRunnerWithFreshLocker(t *testing.T, b *backend, catalog *migrate.Catalog) *migrate.Runner {
	t.Helper()
	runner, err := migrate.NewRunner(b.db, catalog, freshLocker(t, b), migrate.WithVerifier(fixtureSchemaVerifier(catalog.TargetVersion())))
	requireNoError(t, err)
	return runner
}

type productionBackupHarness struct {
	adapter   migrate.BackupRestore
	directory string
	restored  int
}

func newBackup(t *testing.T, b *backend) *productionBackupHarness {
	t.Helper()
	directory, err := os.MkdirTemp(requireAbsoluteEnv(t, "AROP_P09_BACKUP_ROOT"), "adapter-")
	requireNoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	requireNoError(t, os.Chmod(directory, 0o700))
	return newBackupInDirectory(t, b, directory)
}
func newBackupInDirectory(t *testing.T, b *backend, directory string) *productionBackupHarness {
	t.Helper()
	var adapter migrate.BackupRestore
	var err error
	if b.dialect == migrate.DialectSQLite {
		adapter, err = sqlitestore.NewBackupRestore(b.db, b.path, directory)
	} else {
		adapter, err = pgstore.NewBackupRestore(b.db, b.dsn, directory, filepath.Join(requireAbsoluteEnv(t, "AROP_P09_POSTGRES_BIN"), "pg_dump"))
	}
	requireNoError(t, err)
	return &productionBackupHarness{adapter: adapter, directory: directory}
}
func (backup *productionBackupHarness) Create(ctx context.Context, from, to int64) (migrate.Snapshot, error) {
	return backup.adapter.Create(ctx, from, to)
}
func (backup *productionBackupHarness) Restore(ctx context.Context, snapshot migrate.Snapshot) error {
	err := backup.adapter.Restore(ctx, snapshot)
	if err == nil {
		backup.restored++
	}
	return err
}
func (backup *productionBackupHarness) Finalize(ctx context.Context, snapshot migrate.Snapshot) error {
	finalizer, ok := backup.adapter.(migrate.SnapshotFinalizer)
	if !ok {
		return errors.New("production backup adapter does not implement snapshot finalization")
	}
	return finalizer.Finalize(ctx, snapshot)
}
func (backup *productionBackupHarness) restoreCount() int {
	return backup.restored
}

func diagnosePostgresRestore(t *testing.T, dialect migrate.Dialect, backup *productionBackupHarness, snapshot migrate.Snapshot) string {
	t.Helper()
	if dialect != migrate.DialectPostgres {
		return "diagnostic-not-applicable"
	}
	restore := filepath.Join(requireAbsoluteEnv(t, "AROP_P09_POSTGRES_BIN"), "pg_restore")
	archive := filepath.Join(backup.directory, snapshot.ID)
	environment := []string{"HOME=/nonexistent", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	command := exec.Command(restore, "--no-owner", "--no-privileges", "--exit-on-error", "--file=-", archive)
	command.Env = environment
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	detail := strings.ReplaceAll(stderr.String(), backup.directory, "<backup>")
	if len(detail) > 2048 {
		detail = detail[:2048]
	}
	return fmt.Sprintf("standalone=%v stderr=%q", err, detail)
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func reopenBackend(t *testing.T, b *backend) *backend {
	t.Helper()
	requireNoError(t, b.db.PingContext(context.Background()))
	return b
}
func requireReady(t *testing.T, runner *migrate.Runner, version int64) {
	t.Helper()
	status, err := runner.Status(context.Background())
	requireNoError(t, err)
	if !status.Ready || status.CurrentVersion != version || status.TargetVersion != version || status.Reason != "ok" {
		t.Fatalf("unexpected readiness: %+v", status)
	}
	requireNoError(t, runner.Check(context.Background()))
}
func requireHistoryCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var count int
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM arop_schema_migrations`).Scan(&count))
	if count != want {
		t.Fatalf("history count=%d want=%d", count, want)
	}
}
func requireFixtureGeneration(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	rows, err := db.Query(`SELECT generation FROM arop_engine_fixture LIMIT 1`)
	if want {
		requireNoError(t, err)
		_ = rows.Close()
		return
	}
	if err == nil {
		_ = rows.Close()
		t.Fatal("generation column unexpectedly exists")
	}
}
func insertFixtureSentinel(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO arop_engine_fixture(fixture_id,fixture_value) VALUES('acceptance-sentinel','must-survive-exactly')`)
	requireNoError(t, err)
}
func requireFixtureSentinel(t *testing.T, db *sql.DB) {
	t.Helper()
	var value string
	requireNoError(t, db.QueryRow(`SELECT fixture_value FROM arop_engine_fixture WHERE fixture_id='acceptance-sentinel'`).Scan(&value))
	if value != "must-survive-exactly" {
		t.Fatalf("sentinel value=%q", value)
	}
}
func requireExactV1Fixture(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT fixture_id,fixture_value FROM arop_engine_fixture ORDER BY fixture_id`)
	requireNoError(t, err)
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var id, value string
		requireNoError(t, rows.Scan(&id, &value))
		values = append(values, id+"="+value)
	}
	requireNoError(t, rows.Err())
	if strings.Join(values, "|") != "acceptance-sentinel=must-survive-exactly|baseline=v1" {
		t.Fatalf("restored v1 rows=%v", values)
	}
	requireFixtureGeneration(t, db, false)
}
func fixtureLogicalState(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT fixture_id,fixture_value FROM arop_engine_fixture ORDER BY fixture_id`)
	requireNoError(t, err)
	defer rows.Close()
	parts := []string{}
	for rows.Next() {
		var id, value string
		requireNoError(t, rows.Scan(&id, &value))
		parts = append(parts, id+"="+value)
	}
	requireNoError(t, rows.Err())
	var history string
	requireNoError(t, db.QueryRow(`SELECT CAST(version AS TEXT)||':'||name||':'||checksum||':'||CAST(dirty AS TEXT) FROM arop_schema_migrations ORDER BY version LIMIT 1`).Scan(&history))
	return strings.Join(parts, "|") + "#" + history
}
func equalInt64(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func newDurable(t *testing.T, dialect migrate.Dialect, migrated bool) (*backend, *migrate.Runner, *durable.Store) {
	t.Helper()
	b := newBackend(t, dialect)
	runner := newRunner(t, b, baseCatalog(t, dialect), migrate.WithVerifier(durable.VerifySchema(dialect)))
	store, err := durable.New(b.db, dialect, b.uow.Transaction, runner.Check)
	requireNoError(t, err)
	if migrated {
		_, err = runner.Migrate(context.Background())
		requireNoError(t, err)
	}
	return b, runner, store
}
func observation() (observability.AuditEntry, observability.SpanRecord) {
	ended := time.Date(2026, 9, 23, 1, 2, 3, 4, time.UTC)
	audit := observability.AuditEntry{ID: "aud_018f05e0-7b80-7abc-8def-1234567890ab", OccurredAt: ended, RequestID: "req_018f05e0-7b80-7abc-8def-1234567890ac", TraceID: "11111111111111111111111111111111", Operation: "storage.acceptance", Outcome: observability.OutcomeSucceeded, HTTPStatus: 200}
	span := observability.SpanRecord{TraceID: audit.TraceID, SpanID: "1111111111111111", RequestID: audit.RequestID, Operation: audit.Operation, StartedAt: ended.Add(-time.Millisecond), EndedAt: ended, Status: observability.SpanStatusOK}
	return audit, span
}
func requireObservationCounts(t *testing.T, store *durable.Store, want int) {
	t.Helper()
	audits, err := store.QueryAudit(context.Background(), observability.AuditQuery{})
	requireNoError(t, err)
	spans, err := store.QueryTrace(context.Background(), observability.TraceQuery{})
	requireNoError(t, err)
	if len(audits) != want || len(spans) != want {
		t.Fatalf("observation counts audit=%d trace=%d want=%d", len(audits), len(spans), want)
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("required environment %s is empty", key)
	}
	return value
}
func requireAbsoluteEnv(t *testing.T, key string) string {
	t.Helper()
	value := requireEnv(t, key)
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		t.Fatalf("%s must be absolute and clean", key)
	}
	return value
}
func newControlledBackupDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(requireAbsoluteEnv(t, "AROP_P09_BACKUP_ROOT"), "acceptance-")
	requireNoError(t, err)
	requireNoError(t, os.Chmod(directory, 0o700))
	return directory
}
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var _ fs.FS = fstest.MapFS{}
