package identity

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	sqlitestore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

const identityMigrationVersion = int64(5)

func TestIdentityMigrationSQLiteLifecycle(t *testing.T) {
	t.Run("empty-to-five", func(t *testing.T) {
		db, runner := newSQLiteIdentityRunner(t, false)
		result, err := runner.Migrate(context.Background())
		requireMigrationNoError(t, err)
		if result.FromVersion != 0 || result.ToVersion != identityMigrationVersion || !migrationVersionsEqual(result.Applied, []int64{1, 5}) {
			t.Fatalf("unexpected empty migration result: %#v", result)
		}
		requireReadyAtFive(t, runner)
		assertIdentityConstraints(t, db)
	})

	t.Run("one-to-five", func(t *testing.T) {
		_, runner := newSQLiteIdentityRunner(t, true)
		result, err := runner.Migrate(context.Background())
		requireMigrationNoError(t, err)
		if result.FromVersion != 1 || result.ToVersion != 5 || !migrationVersionsEqual(result.Applied, []int64{5}) || result.Snapshot == nil {
			t.Fatalf("unexpected one-to-five result: %#v", result)
		}
		requireReadyAtFive(t, runner)
	})

	t.Run("idempotent-at-five", func(t *testing.T) {
		_, runner := newSQLiteIdentityRunner(t, false)
		_, err := runner.Migrate(context.Background())
		requireMigrationNoError(t, err)
		result, err := runner.Migrate(context.Background())
		requireMigrationNoError(t, err)
		if result.FromVersion != 5 || result.ToVersion != 5 || len(result.Applied) != 0 || result.Snapshot != nil {
			t.Fatalf("unexpected idempotent result: %#v", result)
		}
	})

	t.Run("dirty-five-is-not-ready", func(t *testing.T) {
		db, runner := migratedSQLiteIdentityRunner(t)
		_, err := db.Exec(`UPDATE arop_schema_migrations SET dirty=1 WHERE version=5`)
		requireMigrationNoError(t, err)
		status, err := runner.Status(context.Background())
		requireMigrationNoError(t, err)
		if status.Ready || status.Reason != "migration_history_incompatible" {
			t.Fatalf("dirty migration did not fail closed: %#v", status)
		}
		if _, err := runner.Migrate(context.Background()); err == nil {
			t.Fatal("dirty migration history was accepted")
		}
	})

	t.Run("checksum-drift-is-not-ready", func(t *testing.T) {
		db, runner := migratedSQLiteIdentityRunner(t)
		_, err := db.Exec(`UPDATE arop_schema_migrations SET checksum=? WHERE version=5`, strings.Repeat("0", 64))
		requireMigrationNoError(t, err)
		status, err := runner.Status(context.Background())
		requireMigrationNoError(t, err)
		if status.Ready || status.Reason != "migration_history_incompatible" {
			t.Fatalf("checksum drift did not fail closed: %#v", status)
		}
	})

	t.Run("schema-tamper-is-not-ready", func(t *testing.T) {
		db, runner := migratedSQLiteIdentityRunner(t)
		_, err := db.Exec(`DROP INDEX arop_credentials_audience_status_expiry_idx`)
		requireMigrationNoError(t, err)
		status, err := runner.Status(context.Background())
		requireMigrationNoError(t, err)
		if status.Ready || status.Reason != "schema_verification_failed" {
			t.Fatalf("schema tamper did not fail closed: %#v", status)
		}
	})
}

func TestIdentityMigrationDialectContract(t *testing.T) {
	sqliteSQL := readIdentityMigration(t, migrate.DialectSQLite, "0005_identity.sql")
	postgresSQL := readIdentityMigration(t, migrate.DialectPostgres, "0005_identity.sql")

	for name, contents := range map[string]string{"sqlite": sqliteSQL, "postgres": postgresSQL} {
		t.Run(name, func(t *testing.T) {
			if strings.Count(contents, "\n-- arop:statement\n") != 3 {
				t.Fatal("identity migration must contain exactly four statements")
			}
			lower := strings.ToLower(contents)
			for _, forbidden := range []string{"raw_secret", "secret_value", "password", "cookie", "token", "asset", "console", "url"} {
				if strings.Contains(lower, forbidden) {
					t.Fatalf("migration contains forbidden field or responsibility %q", forbidden)
				}
			}
		})
	}

	for _, table := range []string{"arop_dev_principals", "arop_credentials"} {
		sqliteColumns := migrationTableColumns(t, sqliteSQL, table)
		postgresColumns := migrationTableColumns(t, postgresSQL, table)
		if strings.Join(sqliteColumns, ",") != strings.Join(postgresColumns, ",") {
			t.Fatalf("%s column order differs: SQLite=%v PostgreSQL=%v", table, sqliteColumns, postgresColumns)
		}
	}

	constraintPattern := regexp.MustCompile(`(?m)^\s*CONSTRAINT\s+([a-z0-9_]+)\s+`)
	indexPattern := regexp.MustCompile(`(?m)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+([a-z0-9_]+)\s+`)
	if got, want := migrationCaptures(constraintPattern, sqliteSQL), migrationCaptures(constraintPattern, postgresSQL); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("constraint names differ: SQLite=%v PostgreSQL=%v", got, want)
	}
	if got, want := migrationCaptures(indexPattern, sqliteSQL), migrationCaptures(indexPattern, postgresSQL); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("index names differ: SQLite=%v PostgreSQL=%v", got, want)
	}

}

func assertIdentityConstraints(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO arop_dev_principals(principal_id,subject_id,status,created_at_ns,updated_at_ns,revision) VALUES(?,?,?,?,?,?)`, "principal-1", "subject-1", "active", 1, 1, 1)
	requireMigrationNoError(t, err)
	insertCredential := `INSERT INTO arop_credentials(credential_id,principal_id,credential_kind,audience,scope_canonical,secret_verifier,issued_at_ns,not_before_at_ns,expires_at_ns,status,revoked_at_ns,replaced_at_ns,replacement_credential_id,revision,idempotency_key_digest,idempotency_request_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err = db.Exec(insertCredential, "credential-1", "principal-1", "service", "control-plane", "agent:invoke", strings.Repeat("a", 64), 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("b", 64), strings.Repeat("c", 64))
	requireMigrationNoError(t, err)

	invalid := []struct {
		name string
		args []any
	}{
		{"raw-verifier-shape", []any{"credential-2", "principal-1", "service", "control-plane", "agent:invoke", "plaintext", 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("d", 64), strings.Repeat("e", 64)}},
		{"invalid-lifecycle", []any{"credential-3", "principal-1", "service", "control-plane", "agent:invoke", strings.Repeat("f", 64), 1, 1, 2, "revoked", nil, nil, nil, 1, strings.Repeat("1", 64), strings.Repeat("2", 64)}},
		{"unknown-principal", []any{"credential-4", "missing", "service", "control-plane", "agent:invoke", strings.Repeat("3", 64), 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("4", 64), strings.Repeat("5", 64)}},
		{"duplicate-verifier", []any{"credential-5", "principal-1", "service", "control-plane", "agent:invoke", strings.Repeat("a", 64), 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("6", 64), strings.Repeat("7", 64)}},
		{"duplicate-idempotency", []any{"credential-6", "principal-1", "service", "control-plane", "agent:invoke", strings.Repeat("8", 64), 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("b", 64), strings.Repeat("9", 64)}},
		{"invalid-idempotency-request", []any{"credential-7", "principal-1", "service", "control-plane", "agent:invoke", strings.Repeat("0", 64), 1, 1, 2, "active", nil, nil, nil, 1, strings.Repeat("1", 64), "not-a-digest"}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(insertCredential, test.args...); err == nil {
				t.Fatal("invalid credential row was accepted")
			}
		})
	}
}

func newSQLiteIdentityRunner(t *testing.T, startAtOne bool) (*sql.DB, *migrate.Runner) {
	t.Helper()
	temporaryRoot, err := filepath.EvalSymlinks(t.TempDir())
	requireMigrationNoError(t, err)
	databasePath := filepath.Join(temporaryRoot, "identity.db")
	db, err := sqlitestore.Open(databasePath)
	requireMigrationNoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if startAtOne {
		base, err := migrate.NewMigration(1, "base", []byte(readIdentityMigration(t, migrate.DialectSQLite, "0001_base.sql")))
		requireMigrationNoError(t, err)
		catalog, err := migrate.NewCatalog(migrate.DialectSQLite, []migrate.Migration{base})
		requireMigrationNoError(t, err)
		runner, err := migrate.NewRunner(db, catalog, migrationTestLocker{}, migrate.WithVerifier(baseSQLiteVerifier))
		requireMigrationNoError(t, err)
		_, err = runner.Migrate(context.Background())
		requireMigrationNoError(t, err)
	}
	catalog, err := migrate.LoadCatalog(os.DirFS(migrationControlPlaneRoot(t)), "migrations/sqlite", migrate.DialectSQLite)
	requireMigrationNoError(t, err)
	runner, err := migrate.NewRunner(db, catalog, migrationTestLocker{}, migrate.WithVerifier(identitySQLiteVerifier), migrate.WithBackupRestore(migrationTestBackup{}))
	requireMigrationNoError(t, err)
	return db, runner
}

func migratedSQLiteIdentityRunner(t *testing.T) (*sql.DB, *migrate.Runner) {
	t.Helper()
	db, runner := newSQLiteIdentityRunner(t, false)
	_, err := runner.Migrate(context.Background())
	requireMigrationNoError(t, err)
	return db, runner
}

func requireReadyAtFive(t *testing.T, runner *migrate.Runner) {
	t.Helper()
	status, err := runner.Status(context.Background())
	requireMigrationNoError(t, err)
	if !status.Ready || status.Reason != "ok" || status.CurrentVersion != 5 || status.TargetVersion != 5 || len(status.Applied) != 2 {
		t.Fatalf("identity database is not truthfully ready at version 5: %#v", status)
	}
}

func baseSQLiteVerifier(ctx context.Context, query migrate.Queryer) error {
	var count int
	if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='arop_observations'`).Scan(&count); err != nil || count != 1 {
		return errors.New("base observation table missing")
	}
	return nil
}

func identitySQLiteVerifier(ctx context.Context, query migrate.Queryer) error {
	for _, object := range []struct{ kind, name string }{
		{"table", "arop_observations"}, {"table", "arop_dev_principals"}, {"table", "arop_credentials"},
		{"index", "arop_credentials_principal_status_expiry_idx"}, {"index", "arop_credentials_audience_status_expiry_idx"},
	} {
		var count int
		if err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, object.kind, object.name).Scan(&count); err != nil || count != 1 {
			return errors.New("identity schema object missing")
		}
	}
	return nil
}

func readIdentityMigration(t *testing.T, dialect migrate.Dialect, name string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(migrationControlPlaneRoot(t), "migrations", string(dialect), name))
	requireMigrationNoError(t, err)
	return string(contents)
}

func migrationControlPlaneRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func migrationTableColumns(t *testing.T, contents, table string) []string {
	t.Helper()
	start := strings.Index(contents, "CREATE TABLE "+table+" (\n")
	if start < 0 {
		t.Fatalf("table %s missing", table)
	}
	body := contents[start+len("CREATE TABLE "+table+" (\n"):]
	end := strings.Index(body, "\n)\n")
	if end < 0 {
		t.Fatalf("table %s is not canonically terminated", table)
	}
	var columns []string
	for _, line := range strings.Split(body[:end], "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, ","))
		if line == "" || strings.HasPrefix(line, "CONSTRAINT ") || strings.HasPrefix(line, "(") {
			continue
		}
		columns = append(columns, strings.Fields(line)[0])
	}
	return columns
}

func migrationCaptures(pattern *regexp.Regexp, contents string) []string {
	var values []string
	for _, match := range pattern.FindAllStringSubmatch(contents, -1) {
		values = append(values, match[1])
	}
	sort.Strings(values)
	return values
}

func migrationVersionsEqual(left, right []int64) bool {
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

func requireMigrationNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type migrationTestLocker struct{}

func (migrationTestLocker) Lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

type migrationTestBackup struct{}

func (migrationTestBackup) Create(context.Context, int64, int64) (migrate.Snapshot, error) {
	return migrate.Snapshot{ID: "test-snapshot", Digest: strings.Repeat("0", 64)}, nil
}

func (migrationTestBackup) Restore(context.Context, migrate.Snapshot) error { return nil }
