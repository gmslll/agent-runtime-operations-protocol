package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	assetpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets/storage/postgres"
	assetsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets/storage/sqlite"
	dispatchpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch/storage/postgres"
	dispatchsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch/storage/sqlite"
	eventpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event/storage/postgres"
	eventsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event/storage/sqlite"
	publicationpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/postgres"
	publicationsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/sqlite"
	registrypostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/postgres"
	registrysqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry/storage/sqlite"
	runpostgres "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run/storage/postgres"
	runsqlite "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/identity"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

var databaseSequence atomic.Uint64

func TestProductionMigration0060Conformance(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("empty-and-idempotent", func(t *testing.T) {
				database := openDatabase(t, dialect)
				defer database.close()
				runner := newRunner(t, database, loadCatalog(t, migrate.P20ProductionCatalog(), dialect), p20Verifier(dialect), nil)
				result, err := runner.Migrate(context.Background())
				requireNoError(t, err)
				if result.FromVersion != 0 || result.ToVersion != 60 || !reflect.DeepEqual(result.Applied, []int64{1, 5, 10, 20, 30, 40, 50, 60}) || result.Snapshot != nil {
					t.Fatalf("empty migration result: %#v", result)
				}
				requireNoError(t, runner.Check(context.Background()))
				replay, err := runner.Migrate(context.Background())
				requireNoError(t, err)
				if replay.FromVersion != 60 || replay.ToVersion != 60 || len(replay.Applied) != 0 || replay.Snapshot != nil {
					t.Fatalf("idempotent migration result: %#v", replay)
				}
			})

			t.Run("p19-to-p20", func(t *testing.T) {
				database := openDatabase(t, dialect)
				defer database.close()
				p19 := newRunner(t, database, loadCatalog(t, migrate.P19ProductionCatalog(), dialect), p19Verifier(dialect), nil)
				setup, err := p19.Migrate(context.Background())
				requireNoError(t, err)
				if setup.ToVersion != 50 || !reflect.DeepEqual(setup.Applied, []int64{1, 5, 10, 20, 30, 40, 50}) {
					t.Fatalf("P19 setup result: %#v", setup)
				}
				current := newRunner(t, database, loadCatalog(t, migrate.P20ProductionCatalog(), dialect), p20Verifier(dialect), database.backup)
				result, err := current.Migrate(context.Background())
				requireNoError(t, err)
				if result.FromVersion != 50 || result.ToVersion != 60 || !reflect.DeepEqual(result.Applied, []int64{60}) || result.Snapshot == nil {
					t.Fatalf("P19 to P20 result: %#v", result)
				}
				requireNoError(t, current.Check(context.Background()))
			})

			t.Run("dirty-history-fails-closed", func(t *testing.T) {
				database := openDatabase(t, dialect)
				defer database.close()
				runner := newRunner(t, database, loadCatalog(t, migrate.P20ProductionCatalog(), dialect), p20Verifier(dialect), nil)
				_, err := runner.Migrate(context.Background())
				requireNoError(t, err)
				statement := `UPDATE arop_schema_migrations SET dirty=1,applied_at_ns=NULL WHERE version=60`
				if dialect == migrate.DialectPostgres {
					statement = `UPDATE arop_schema_migrations SET dirty=TRUE,applied_at_ns=NULL WHERE version=60`
				}
				_, err = database.db.ExecContext(context.Background(), statement)
				requireNoError(t, err)
				status, err := runner.Status(context.Background())
				requireNoError(t, err)
				if status.Ready || status.Reason != "migration_history_incompatible" {
					t.Fatalf("dirty status: %#v", status)
				}
				if _, err = runner.Migrate(context.Background()); err == nil {
					t.Fatal("dirty migration history was accepted")
				}
			})
		})
	}
}

type testDatabase struct {
	db      *sql.DB
	locker  migrate.Locker
	backup  migrate.BackupRestore
	cleanup func()
}

func (database testDatabase) close() { database.cleanup() }

func openDatabase(t *testing.T, dialect migrate.Dialect) testDatabase {
	t.Helper()
	sequence := databaseSequence.Add(1)
	scratch, err := filepath.EvalSymlinks(requiredEnv(t, "AROP_P20_SCRATCH"))
	requireNoError(t, err)
	if dialect == migrate.DialectSQLite {
		root, err := os.MkdirTemp(scratch, "migration-sqlite-")
		requireNoError(t, err)
		requireNoError(t, os.Chmod(root, 0o700))
		path := filepath.Join(root, "control-plane.db")
		db, err := sqliteadapter.Open(path)
		requireNoError(t, err)
		locker, err := sqliteadapter.NewMigrationLocker(path + ".migration.lock")
		requireNoError(t, err)
		backupDirectory := filepath.Join(root, "backup")
		requireNoError(t, os.Mkdir(backupDirectory, 0o700))
		backup, err := sqliteadapter.NewBackupRestore(db, path, backupDirectory)
		requireNoError(t, err)
		return testDatabase{db: db, locker: locker, backup: backup, cleanup: func() {
			_ = db.Close()
			_ = os.RemoveAll(root)
		}}
	}
	base := requiredEnv(t, "AROP_P20_POSTGRES_URL")
	admin, err := postgresadapter.Open(context.Background(), base)
	requireNoError(t, err)
	name := fmt.Sprintf("arop_p20_migration_%03d", sequence)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+name)
	requireNoError(t, err)
	parsed, err := url.Parse(base)
	requireNoError(t, err)
	parsed.Path = "/" + name
	dsn := parsed.String()
	db, err := postgresadapter.Open(context.Background(), dsn)
	requireNoError(t, err)
	locker, err := postgresadapter.NewMigrationLocker(db, 183000+int64(sequence))
	requireNoError(t, err)
	backupDirectory, err := os.MkdirTemp(scratch, "migration-postgres-backup-")
	requireNoError(t, err)
	requireNoError(t, os.Chmod(backupDirectory, 0o700))
	pgDump, err := exec.LookPath("pg_dump")
	requireNoError(t, err)
	pgDump, err = filepath.EvalSymlinks(pgDump)
	requireNoError(t, err)
	backup, err := postgresadapter.NewBackupRestore(db, dsn, backupDirectory, pgDump)
	requireNoError(t, err)
	return testDatabase{db: db, locker: locker, backup: backup, cleanup: func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		_ = os.RemoveAll(backupDirectory)
	}}
}

func newRunner(t *testing.T, database testDatabase, catalog *migrate.Catalog, verifier migrate.Verifier, backup migrate.BackupRestore) *migrate.Runner {
	t.Helper()
	options := []migrate.Option{migrate.WithVerifier(verifier)}
	if backup != nil {
		options = append(options, migrate.WithBackupRestore(backup))
	}
	runner, err := migrate.NewRunner(database.db, catalog, database.locker, options...)
	requireNoError(t, err)
	return runner
}

func loadCatalog(t *testing.T, closure migrate.CatalogClosure, dialect migrate.Dialect) *migrate.Catalog {
	t.Helper()
	root := requiredEnv(t, "AROP_P20_MIGRATION_ROOT")
	files := fstest.MapFS{}
	for _, migration := range closure.Migrations {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(migration.Path)))
		requireNoError(t, err)
		files[migration.Path] = &fstest.MapFile{Data: contents, Mode: 0o600}
	}
	catalog, err := migrate.LoadCatalogClosure(files, closure, dialect)
	requireNoError(t, err)
	return catalog
}

func p14Verifier(dialect migrate.Dialect) migrate.Verifier {
	return chainVerifiers(durable.VerifySchema(dialect), identity.VerifySchema(dialect), publicationVerifier(dialect), assetVerifier(dialect), registryVerifier(dialect))
}

func p18Verifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return chainVerifiers(p14Verifier(dialect), runpostgres.VerifySchema())
	}
	return chainVerifiers(p14Verifier(dialect), runsqlite.VerifySchema())
}

func p20Verifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return chainVerifiers(p19Verifier(dialect), eventpostgres.VerifySchema())
	}
	return chainVerifiers(p19Verifier(dialect), eventsqlite.VerifySchema())
}

func p19Verifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return chainVerifiers(p18Verifier(dialect), dispatchpostgres.VerifySchema())
	}
	return chainVerifiers(p18Verifier(dialect), dispatchsqlite.VerifySchema())
}

func publicationVerifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return publicationpostgres.VerifySchema()
	}
	return publicationsqlite.VerifySchema()
}

func assetVerifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return assetpostgres.VerifySchema()
	}
	return assetsqlite.VerifySchema()
}

func registryVerifier(dialect migrate.Dialect) migrate.Verifier {
	if dialect == migrate.DialectPostgres {
		return registrypostgres.VerifySchema()
	}
	return registrysqlite.VerifySchema()
}

func chainVerifiers(verifiers ...migrate.Verifier) migrate.Verifier {
	return func(ctx context.Context, query migrate.Queryer) error {
		for _, verifier := range verifiers {
			if err := verifier(ctx, query); err != nil {
				return err
			}
		}
		return nil
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
