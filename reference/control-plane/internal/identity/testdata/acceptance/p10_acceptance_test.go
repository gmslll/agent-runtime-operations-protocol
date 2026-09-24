package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
				AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"secret.read"}, AllowedTenants: []string{"reference-dev"},
			})
			requireNoError(t, err)

			issueMetadata := requestMetadata(t, ids)
			issued, err := service.Issue(context.Background(), identity.IssueRequest{
				TenantID: "reference-dev", SubjectID: "p10-acceptance", Kind: "service", Audience: "reference-control-plane",
				Scopes: []string{"secret.read"}, TTL: 5 * time.Minute, IdempotencyKey: "p10-issue-1", Metadata: issueMetadata,
			})
			requireNoError(t, err)
			if issued.Credential == "" || strings.Contains(issued.Credential, "plaintext") {
				t.Fatal("issued credential is missing or unsafe")
			}
			principal, err := service.Authenticate(context.Background(), identity.AuthenticateRequest{Credential: issued.Credential, TenantID: "reference-dev", Audience: "reference-control-plane", Scopes: []string{"secret.read"}, Metadata: requestMetadata(t, ids)})
			requireNoError(t, err)
			if principal.CredentialID != issued.CredentialID {
				t.Fatal("authenticated principal does not match issued credential")
			}
			requireNoError(t, service.Revoke(context.Background(), identity.RevokeRequest{CredentialID: issued.CredentialID, Metadata: requestMetadata(t, ids)}))
			_, authenticationErr := service.Authenticate(context.Background(), identity.AuthenticateRequest{Credential: issued.Credential, TenantID: "reference-dev", Audience: "reference-control-plane", Scopes: []string{"secret.read"}, Metadata: requestMetadata(t, ids)})
			if authenticationErr == nil {
				t.Fatal("revoked credential authenticated")
			}
			if strings.Contains(authenticationErr.Error(), issued.Credential) {
				t.Fatal("authentication error leaked presented credential")
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
			testRejectedMutationAuditAndReadiness(t, db, uow, observations, repository, clock, ids)
			assertNoCredentialEgress(t, db, observations, issued.Credential)

			t.Run("one-to-five", func(t *testing.T) { testOneToFive(t, dialect) })
			for _, failure := range []string{"apply", "verifier", "restore"} {
				failure := failure
				t.Run("one-to-five-recovery-"+failure, func(t *testing.T) { testOneToFiveRecovery(t, dialect, failure) })
			}
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

func testOneToFiveRecovery(t *testing.T, dialect migrate.Dialect, failure string) {
	db, uow, backup, cleanup := acceptanceDatabase(t, dialect)
	defer cleanup()
	rootPath := requiredEnv(t, "AROP_P10_MIGRATION_ROOT")
	p09Catalog := loadP09Catalog(t, rootPath, dialect)
	p09, err := migrate.NewRunner(db, p09Catalog, testLocker{}, migrate.WithVerifier(durable.VerifySchema(dialect)))
	requireNoError(t, err)
	_, err = p09.Migrate(context.Background())
	requireNoError(t, err)
	seedDurableObservation(t, db, uow, dialect, p09)

	p10Catalog, err := migrate.LoadCatalogClosure(os.DirFS(rootPath), migrate.CurrentProductionCatalog(), dialect)
	requireNoError(t, err)
	verifier := combinedVerifier(dialect)
	usedBackup := backup
	switch failure {
	case "apply":
		p10Catalog = failingApplyCatalog(t, rootPath, dialect)
	case "verifier":
		base := verifier
		verifier = func(ctx context.Context, query migrate.Queryer) error {
			if err := base(ctx, query); err != nil {
				return err
			}
			return errors.New("injected identity verifier failure")
		}
	case "restore":
		p10Catalog = failingApplyCatalog(t, rootPath, dialect)
		usedBackup = &restoreFailureBackup{delegate: backup}
	default:
		t.Fatalf("unknown recovery case %q", failure)
	}
	runner, err := migrate.NewRunner(db, p10Catalog, testLocker{}, migrate.WithVerifier(verifier), migrate.WithBackupRestore(usedBackup))
	requireNoError(t, err)
	_, err = runner.Migrate(context.Background())
	if err == nil {
		t.Fatal("injected migration failure succeeded")
	}
	if failure == "restore" {
		if !strings.Contains(err.Error(), "arop_p10_missing_apply_target") || !strings.Contains(err.Error(), "restore pre-migration backup") || !strings.Contains(err.Error(), "injected restore failure") || runner.Check(context.Background()) == nil {
			t.Fatalf("restore failure did not retain original+restore error and not-ready state: %v", err)
		}
		return
	}
	assertRestoredV1(t, db, dialect, p09)
}

func loadP09Catalog(t *testing.T, rootPath string, dialect migrate.Dialect) *migrate.Catalog {
	t.Helper()
	files := fstest.MapFS{}
	for _, declared := range migrate.P09ProductionCatalog().Migrations {
		contents, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(declared.Path)))
		requireNoError(t, err)
		files[declared.Path] = &fstest.MapFile{Data: contents, Mode: 0o600}
	}
	catalog, err := migrate.LoadCatalogClosure(files, migrate.P09ProductionCatalog(), dialect)
	requireNoError(t, err)
	return catalog
}

func failingApplyCatalog(t *testing.T, rootPath string, dialect migrate.Dialect) *migrate.Catalog {
	t.Helper()
	directory := string(dialect)
	base, err := os.ReadFile(filepath.Join(rootPath, directory, "0001_base.sql"))
	requireNoError(t, err)
	identitySQL, err := os.ReadFile(filepath.Join(rootPath, directory, "0005_identity.sql"))
	requireNoError(t, err)
	identitySQL = append(identitySQL, []byte("\n-- arop:statement\nINSERT INTO arop_p10_missing_apply_target(id) VALUES(1);\n")...)
	v1, err := migrate.NewMigration(1, "base", base)
	requireNoError(t, err)
	v5, err := migrate.NewMigration(5, "identity", identitySQL)
	requireNoError(t, err)
	catalog, err := migrate.NewCatalog(dialect, []migrate.Migration{v1, v5})
	requireNoError(t, err)
	return catalog
}

func seedDurableObservation(t *testing.T, db *sql.DB, uow platformports.UnitOfWork, dialect migrate.Dialect, runner *migrate.Runner) {
	t.Helper()
	store, err := durable.New(db, dialect, transactionLookup(uow), runner.Check)
	requireNoError(t, err)
	now := time.Unix(100, 0).UTC()
	audit := observability.AuditEntry{ID: "aud_01956e7b-9abc-7def-8abc-0123456789ab", OccurredAt: now, RequestID: "req_01956e7b-9abc-7def-8abc-0123456789ab", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Operation: "p10.migration.seed", Outcome: observability.OutcomeSucceeded, HTTPStatus: 200}
	span := observability.SpanRecord{TraceID: audit.TraceID, SpanID: "00f067aa0ba902b7", RequestID: audit.RequestID, Operation: audit.Operation, StartedAt: now, EndedAt: now, Status: observability.SpanStatusOK}
	requireNoError(t, store.AppendObservation(context.Background(), audit, span))
}

func assertRestoredV1(t *testing.T, db *sql.DB, dialect migrate.Dialect, p09 *migrate.Runner) {
	t.Helper()
	var versions, observations int
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM arop_schema_migrations WHERE version=1 AND dirty=`+placeholder(dialect, 1), false).Scan(&versions))
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM arop_observations WHERE operation='p10.migration.seed'`).Scan(&observations))
	if versions != 1 || observations != 1 || identityTableExists(t, db, dialect) {
		t.Fatalf("restore did not return exact v1: history=%d observations=%d identity_table=%t", versions, observations, identityTableExists(t, db, dialect))
	}
	requireNoError(t, p09.Check(context.Background()))
}

func identityTableExists(t *testing.T, db *sql.DB, dialect migrate.Dialect) bool {
	t.Helper()
	var exists bool
	if dialect == migrate.DialectPostgres {
		requireNoError(t, db.QueryRow(`SELECT to_regclass('public.arop_credentials') IS NOT NULL`).Scan(&exists))
		return exists
	}
	var count int
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='arop_credentials'`).Scan(&count))
	return count != 0
}

type restoreFailureBackup struct{ delegate migrate.BackupRestore }

func (backup *restoreFailureBackup) Create(ctx context.Context, from, to int64) (migrate.Snapshot, error) {
	return backup.delegate.Create(ctx, from, to)
}
func (*restoreFailureBackup) Restore(context.Context, migrate.Snapshot) error {
	return errors.New("injected restore failure")
}

type failingObservationWriter struct {
	delegate observability.ObservationWriter
	fail     atomic.Bool
}

func (writer *failingObservationWriter) AppendObservation(ctx context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if writer.fail.Load() {
		return errors.New("injected durable observation failure")
	}
	return writer.delegate.AppendObservation(ctx, audit, span)
}

func testRejectedMutationAuditAndReadiness(t *testing.T, db *sql.DB, uow platformports.UnitOfWork, observations observability.Store, repository identity.CredentialRepository, clock platformports.Clock, ids platformports.IDSource) {
	t.Helper()
	rejected := identity.IssueRequest{TenantID: "reference-dev", SubjectID: "rejected-subject", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"secret.read"}, IdempotencyKey: "rejected-request", Metadata: requestMetadata(t, ids)}
	if _, err := identityService(t, uow, observations, repository, clock, ids).Issue(context.Background(), rejected); err == nil {
		t.Fatal("invalid mutation was accepted")
	}
	audits, err := observations.QueryAudit(context.Background(), observability.AuditQuery{Operation: "credential.issue", Outcome: observability.OutcomeRejected, Limit: 100})
	requireNoError(t, err)
	if len(audits) == 0 {
		t.Fatal("rejected mutation did not persist a durable security observation")
	}

	writer := &failingObservationWriter{delegate: observations}
	writer.fail.Store(true)
	service := identityService(t, uow, writer, repository, clock, ids)
	var before, after int
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM arop_credentials`).Scan(&before))
	failed := identity.IssueRequest{TenantID: "reference-dev", SubjectID: "audit-failure-subject", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"secret.read"}, TTL: time.Minute, IdempotencyKey: "audit-failure-request", Metadata: requestMetadata(t, ids)}
	if _, err := service.Issue(context.Background(), failed); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("audit failure did not fail closed: %v", err)
	}
	requireNoError(t, db.QueryRow(`SELECT COUNT(*) FROM arop_credentials`).Scan(&after))
	if after != before {
		t.Fatalf("audit failure committed credential mutation: before=%d after=%d", before, after)
	}
	if service.Check(context.Background()) == nil {
		t.Fatal("audit failure left identity readiness green")
	}
	writer.fail.Store(false)
	failed.Metadata = requestMetadata(t, ids)
	failed.TTL = 0
	if _, err := service.Issue(context.Background(), failed); err == nil {
		t.Fatal("recovery rejection unexpectedly succeeded")
	}
	requireNoError(t, service.Check(context.Background()))
}

func identityService(t *testing.T, uow platformports.UnitOfWork, observations observability.ObservationWriter, repository identity.CredentialRepository, clock platformports.Clock, ids platformports.IDSource) *identity.Service {
	t.Helper()
	service, err := identity.New(identity.Dependencies{
		Clock: clock, IDs: ids, Faults: platform.NoopFaultHook{}, UoW: uow,
		Observability: observations, Repository: repository,
		AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"secret.read"}, AllowedTenants: []string{"reference-dev"},
	})
	requireNoError(t, err)
	return service
}

func assertNoCredentialEgress(t *testing.T, db *sql.DB, observations observability.Store, credential string) {
	t.Helper()
	audits, err := observations.QueryAudit(context.Background(), observability.AuditQuery{Limit: 1000})
	requireNoError(t, err)
	spans, err := observations.QueryTrace(context.Background(), observability.TraceQuery{Limit: 1000})
	requireNoError(t, err)
	encoded, err := json.Marshal(struct {
		Audits []observability.AuditEntry
		Spans  []observability.SpanRecord
	}{audits, spans})
	requireNoError(t, err)
	if strings.Contains(string(encoded), credential) {
		t.Fatal("credential leaked into durable Audit/Trace storage")
	}
	rows, err := db.Query(`SELECT p.tenant_id,p.subject_id,c.credential_kind,c.audience,c.scope_canonical,c.secret_verifier,c.idempotency_key_digest,c.idempotency_request_digest FROM arop_credentials c JOIN arop_dev_principals p ON p.principal_id=c.principal_id`)
	requireNoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var values [8]string
		requireNoError(t, rows.Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6], &values[7]))
		for _, value := range values {
			if strings.Contains(value, credential) {
				t.Fatal("credential leaked into durable identity database")
			}
		}
	}
	requireNoError(t, rows.Err())
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
	backupDirectory := t.TempDir()
	requireNoError(t, os.Chmod(backupDirectory, 0o700))
	backup, err := postgresadapter.NewBackupRestore(db, parsed.String(), backupDirectory, pgDump)
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
