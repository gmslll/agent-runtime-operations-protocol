package acceptance

import (
	"context"
	"database/sql"
	"fmt"
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
	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/identity"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

var databaseSequence atomic.Uint64

func TestP10DurableIdentityConformance(t *testing.T) {
	for _, dialect := range []migrate.Dialect{migrate.DialectSQLite, migrate.DialectPostgres} {
		dialect := dialect
		t.Run(string(dialect), func(t *testing.T) {
			db, uow, _, cleanup := acceptanceDatabase(t, dialect)
			defer cleanup()
			catalog, err := migrate.LoadCatalogClosure(os.DirFS(requiredEnv(t, "AROP_P10_MIGRATION_ROOT")), migrate.CurrentProductionCatalog(), dialect)
			requireNoError(t, err)
			runner, err := migrate.NewRunner(db, catalog, testLocker{}, migrate.WithVerifier(combinedVerifier(dialect)))
			requireNoError(t, err)
			result, err := runner.Migrate(context.Background())
			requireNoError(t, err)
			if result.FromVersion != 0 || result.ToVersion != 5 || len(result.Applied) != 2 {
				t.Fatalf("unexpected migration result: %#v", result)
			}
			requireNoError(t, runner.Check(context.Background()))
			idempotent, err := runner.Migrate(context.Background())
			requireNoError(t, err)
			if idempotent.FromVersion != 5 || idempotent.ToVersion != 5 || len(idempotent.Applied) != 0 || idempotent.Snapshot != nil {
				t.Fatalf("idempotent migration mutated state: %#v", idempotent)
			}
			assertMigrationFailuresClosed(t, db, dialect, runner)

			observations, err := durable.New(db, dialect, transactionLookup(uow), runner.Check)
			requireNoError(t, err)
			repository, err := identity.NewCredentialStore(db, dialect, transactionLookup(uow))
			requireNoError(t, err)
			clock := platform.RealClock{}
			ids := platform.SystemIDSource{Clock: clock}
			service, err := identity.New(identity.Dependencies{
				Clock: clock, IDs: ids, Faults: platform.NoopFaultHook{}, UoW: uow,
				Observability: observations, Repository: repository,
				AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"secret.read"},
			})
			requireNoError(t, err)

			issueMetadata := requestMetadata(t, ids)
			issued, err := service.Issue(context.Background(), identity.IssueRequest{
				SubjectID: "p10-acceptance", Kind: "service", Audience: "reference-control-plane",
				Scopes: []string{"secret.read"}, TTL: 5 * time.Minute, IdempotencyKey: "p10-issue-1", Metadata: issueMetadata,
			})
			requireNoError(t, err)
			if issued.Credential == "" || strings.Contains(issued.Credential, "plaintext") {
				t.Fatal("issued credential is missing or unsafe")
			}
			principal, err := service.Authenticate(context.Background(), identity.AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"secret.read"}, Metadata: requestMetadata(t, ids)})
			requireNoError(t, err)
			if principal.CredentialID != issued.CredentialID {
				t.Fatal("authenticated principal does not match issued credential")
			}
			requireNoError(t, service.Revoke(context.Background(), identity.RevokeRequest{CredentialID: issued.CredentialID, Metadata: requestMetadata(t, ids)}))
			if _, err := service.Authenticate(context.Background(), identity.AuthenticateRequest{Credential: issued.Credential, Audience: "reference-control-plane", Scopes: []string{"secret.read"}, Metadata: requestMetadata(t, ids)}); err == nil {
				t.Fatal("revoked credential authenticated")
			}
			audits, err := observations.QueryAudit(context.Background(), observability.AuditQuery{Limit: 100})
			requireNoError(t, err)
			if len(audits) < 4 {
				t.Fatalf("durable lifecycle audit count=%d want>=4", len(audits))
			}
			var persistedVerifier, requestDigest string
			requireNoError(t, db.QueryRow(`SELECT secret_verifier,idempotency_request_digest FROM arop_credentials WHERE credential_id=`+placeholder(dialect, 1), issued.CredentialID).Scan(&persistedVerifier, &requestDigest))
			if persistedVerifier == issued.Credential || len(persistedVerifier) != 64 || len(requestDigest) != 64 {
				t.Fatal("credential plaintext or malformed request fingerprint persisted")
			}

			t.Run("one-to-five", func(t *testing.T) { testOneToFive(t, dialect) })
		})
	}
}

func testOneToFive(t *testing.T, dialect migrate.Dialect) {
	db, _, backup, cleanup := acceptanceDatabase(t, dialect)
	defer cleanup()
	rootPath := requiredEnv(t, "AROP_P10_MIGRATION_ROOT")
	p09Files := fstest.MapFS{}
	for _, declared := range migrate.P09ProductionCatalog().Migrations {
		contents, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(declared.Path)))
		requireNoError(t, err)
		p09Files[declared.Path] = &fstest.MapFile{Data: contents, Mode: 0o600}
	}
	p09Catalog, err := migrate.LoadCatalogClosure(p09Files, migrate.P09ProductionCatalog(), dialect)
	requireNoError(t, err)
	p09, err := migrate.NewRunner(db, p09Catalog, testLocker{}, migrate.WithVerifier(durable.VerifySchema(dialect)))
	requireNoError(t, err)
	result, err := p09.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 0 || result.ToVersion != 1 || len(result.Applied) != 1 {
		t.Fatalf("unexpected P09 setup result: %#v", result)
	}
	p10Catalog, err := migrate.LoadCatalogClosure(os.DirFS(rootPath), migrate.CurrentProductionCatalog(), dialect)
	requireNoError(t, err)
	p10, err := migrate.NewRunner(db, p10Catalog, testLocker{}, migrate.WithVerifier(combinedVerifier(dialect)), migrate.WithBackupRestore(backup))
	requireNoError(t, err)
	result, err = p10.Migrate(context.Background())
	requireNoError(t, err)
	if result.FromVersion != 1 || result.ToVersion != 5 || len(result.Applied) != 1 || result.Applied[0] != 5 || result.Snapshot == nil {
		t.Fatalf("unexpected P10 staged result: %#v", result)
	}
	requireNoError(t, p10.Check(context.Background()))
}

func assertMigrationFailuresClosed(t *testing.T, db *sql.DB, dialect migrate.Dialect, runner *migrate.Runner) {
	t.Helper()
	var checksum string
	requireNoError(t, db.QueryRow(`SELECT checksum FROM arop_schema_migrations WHERE version=5`).Scan(&checksum))
	update := `UPDATE arop_schema_migrations SET checksum=? WHERE version=5`
	if dialect == migrate.DialectPostgres {
		update = `UPDATE arop_schema_migrations SET checksum=$1 WHERE version=5`
	}
	requireNoError(t, execStatement(db, update, strings.Repeat("0", 64)))
	if err := runner.Check(context.Background()); err == nil {
		t.Fatal("checksum drift reported ready")
	}
	requireNoError(t, execStatement(db, update, checksum))

	dirty := `UPDATE arop_schema_migrations SET dirty=? WHERE version=5`
	if dialect == migrate.DialectPostgres {
		dirty = `UPDATE arop_schema_migrations SET dirty=$1 WHERE version=5`
	}
	requireNoError(t, execStatement(db, dirty, true))
	if err := runner.Check(context.Background()); err == nil {
		t.Fatal("dirty migration reported ready")
	}
	requireNoError(t, execStatement(db, dirty, false))

	requireNoError(t, execStatement(db, `DROP INDEX arop_credentials_audience_status_expiry_idx`))
	if err := runner.Check(context.Background()); err == nil {
		t.Fatal("identity index tamper reported ready")
	}
	requireNoError(t, execStatement(db, `CREATE INDEX arop_credentials_audience_status_expiry_idx ON arop_credentials(audience, status, expires_at_ns)`))
	requireNoError(t, runner.Check(context.Background()))
}

func execStatement(db *sql.DB, statement string, arguments ...any) error {
	_, err := db.ExecContext(context.Background(), statement, arguments...)
	return err
}

type testLocker struct{}

func (testLocker) Lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

func combinedVerifier(dialect migrate.Dialect) migrate.Verifier {
	base := durable.VerifySchema(dialect)
	identityVerifier := identity.VerifySchema(dialect)
	return func(ctx context.Context, query migrate.Queryer) error {
		if err := base(ctx, query); err != nil {
			return err
		}
		return identityVerifier(ctx, query)
	}
}

func acceptanceDatabase(t *testing.T, dialect migrate.Dialect) (*sql.DB, platformports.UnitOfWork, migrate.BackupRestore, func()) {
	t.Helper()
	if dialect == migrate.DialectSQLite {
		root := t.TempDir()
		path := filepath.Join(root, "p10.db")
		db, err := sqliteadapter.Open(path)
		requireNoError(t, err)
		uow, err := sqliteadapter.NewUnitOfWork(db)
		requireNoError(t, err)
		backupDirectory := filepath.Join(root, "backup")
		requireNoError(t, os.Mkdir(backupDirectory, 0o700))
		backup, err := sqliteadapter.NewBackupRestore(db, path, backupDirectory)
		requireNoError(t, err)
		return db, uow, backup, func() { _ = db.Close() }
	}
	base := requiredEnv(t, "AROP_P10_POSTGRES_URL")
	admin, err := postgresadapter.Open(context.Background(), base)
	requireNoError(t, err)
	name := fmt.Sprintf("arop_p10_identity_%03d", databaseSequence.Add(1))
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE `+name)
	requireNoError(t, err)
	parsed, err := url.Parse(base)
	requireNoError(t, err)
	parsed.Path = "/" + name
	db, err := postgresadapter.Open(context.Background(), parsed.String())
	requireNoError(t, err)
	uow, err := postgresadapter.NewUnitOfWork(db)
	requireNoError(t, err)
	pgDump, err := exec.LookPath("pg_dump")
	requireNoError(t, err)
	pgDump, err = filepath.EvalSymlinks(pgDump)
	requireNoError(t, err)
	backup, err := postgresadapter.NewBackupRestore(db, parsed.String(), t.TempDir(), pgDump)
	requireNoError(t, err)
	return db, uow, backup, func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	}
}

func transactionLookup(unit platformports.UnitOfWork) durable.TransactionLookup {
	switch value := unit.(type) {
	case *sqliteadapter.UnitOfWork:
		return value.Transaction
	case *postgresadapter.UnitOfWork:
		return value.Transaction
	default:
		panic("unsupported unit of work")
	}
}

func requestMetadata(t *testing.T, ids platformports.IDSource) platform.RequestMetadata {
	t.Helper()
	ctx := context.Background()
	requestID, err := ids.NewID(ctx, platformports.IDRequest)
	requireNoError(t, err)
	traceID, err := ids.NewID(ctx, platformports.IDTrace)
	requireNoError(t, err)
	spanID, err := ids.NewID(ctx, platformports.IDSpan)
	requireNoError(t, err)
	return platform.RequestMetadata{RequestID: requestID, TraceID: traceID, SpanID: spanID}
}

func placeholder(dialect migrate.Dialect, index int) string {
	if dialect == migrate.DialectPostgres {
		return fmt.Sprintf("$%d", index)
	}
	return "?"
}

func requiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("missing %s", key)
	}
	return value
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
