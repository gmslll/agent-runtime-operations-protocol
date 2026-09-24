package identity

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestCredentialStoreRequiresTransactionAndPersistsOnlyVerifier(t *testing.T) {
	db := openIdentityDatabase(t)
	unit, err := sqlite.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCredentialStore(db, migrate.DialectSQLite, unit.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	record := CredentialRecord{CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000001", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", TenantID: "tenant-a", SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.invoke", "agent.read"}, SecretVerifier: strings.Repeat("a", 64), IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Minute), Status: CredentialActive, Revision: 1, IdempotencyDigest: strings.Repeat("b", 64), IdempotencyRequestDigest: strings.Repeat("c", 64)}
	if err := store.Create(context.Background(), &record); err == nil || !strings.Contains(err.Error(), "unit-of-work") {
		t.Fatalf("out-of-transaction write accepted: %v", err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Create(ctx, &record) }); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(context.Background(), record.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TenantID != record.TenantID || loaded.SubjectID != record.SubjectID || loaded.SecretVerifier != record.SecretVerifier || loaded.IdempotencyRequestDigest != record.IdempotencyRequestDigest || strings.Contains(loaded.SecretVerifier, credentialPrefix) {
		t.Fatalf("unexpected stored record: %+v", loaded)
	}
	var columns string
	rows, err := db.Query(`SELECT name FROM pragma_table_info('arop_credentials') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns += " " + name
	}
	for _, forbidden := range []string{"raw_secret", "credential_value", "bearer", "password"} {
		if strings.Contains(columns, forbidden) {
			t.Fatalf("secret-bearing column found: %s", columns)
		}
	}
}

func TestCredentialStoreIsolatesSameSubjectAcrossTenants(t *testing.T) {
	db := openIdentityDatabase(t)
	unit, _ := sqlite.NewUnitOfWork(db)
	store, _ := NewCredentialStore(db, migrate.DialectSQLite, unit.Transaction)
	now := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	first := storeRecord(1, now, "a", "b")
	second := storeRecord(2, now, "c", "d")
	second.TenantID = "tenant-b"
	second.PrincipalID = "prn_01956e7b-9abc-7def-8abc-000000000002"
	second.IdempotencyRequestDigest = strings.Repeat("f", 64)
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Create(ctx, &first) }); err != nil {
		t.Fatal(err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Create(ctx, &second) }); err != nil {
		t.Fatal(err)
	}
	loadedFirst, err := store.Get(context.Background(), first.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	loadedSecond, err := store.Get(context.Background(), second.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedFirst.TenantID != "tenant-a" || loadedSecond.TenantID != "tenant-b" || loadedFirst.SubjectID != loadedSecond.SubjectID || loadedFirst.PrincipalID == loadedSecond.PrincipalID {
		t.Fatalf("tenant isolation failed: %+v %+v", loadedFirst, loadedSecond)
	}
}

func TestCredentialStoreRotateRevokeAndRollback(t *testing.T) {
	db := openIdentityDatabase(t)
	unit, _ := sqlite.NewUnitOfWork(db)
	store, _ := NewCredentialStore(db, migrate.DialectSQLite, unit.Transaction)
	now := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	original := storeRecord(1, now, "a", "b")
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Create(ctx, &original) }); err != nil {
		t.Fatal(err)
	}
	replacement := storeRecord(2, now.Add(time.Second), "c", "d")
	replacement.PrincipalID, replacement.SubjectID = original.PrincipalID, original.SubjectID
	injected := errors.New("after replacement")
	if err := unit.Within(context.Background(), func(ctx context.Context) error {
		if err := store.Replace(ctx, original, &replacement, now.Add(time.Second)); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("rollback injection: %v", err)
	}
	loaded, _ := store.Get(context.Background(), original.CredentialID)
	if loaded.Status != CredentialActive {
		t.Fatal("replacement survived rollback")
	}
	if _, err := store.Get(context.Background(), replacement.CredentialID); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("replacement row survived rollback: %v", err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Replace(ctx, loaded, &replacement, now.Add(time.Second)) }); err != nil {
		t.Fatal(err)
	}
	old, _ := store.Get(context.Background(), original.CredentialID)
	if old.Status != CredentialReplaced || old.ReplacementID != replacement.CredentialID {
		t.Fatalf("old lifecycle mismatch: %+v", old)
	}
	current, _ := store.Get(context.Background(), replacement.CredentialID)
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return store.Revoke(ctx, current, now.Add(2*time.Second)) }); err != nil {
		t.Fatal(err)
	}
	revoked, _ := store.Get(context.Background(), replacement.CredentialID)
	if revoked.Status != CredentialRevoked || revoked.RevokedAt.IsZero() {
		t.Fatalf("revocation mismatch: %+v", revoked)
	}
}

type idempotencyBarrierRepository struct {
	CredentialRepository
	calls atomic.Int32
	ready sync.WaitGroup
}

func newIdempotencyBarrierRepository(repository CredentialRepository) *idempotencyBarrierRepository {
	barrier := &idempotencyBarrierRepository{CredentialRepository: repository}
	barrier.ready.Add(2)
	return barrier
}

func (repository *idempotencyBarrierRepository) GetByIdempotencyDigest(ctx context.Context, digest string) (CredentialRecord, error) {
	result, err := repository.CredentialRepository.GetByIdempotencyDigest(ctx, digest)
	if repository.calls.Add(1) <= 2 {
		repository.ready.Done()
		repository.ready.Wait()
	}
	return result, err
}

func TestCrossServiceConcurrentIdempotencyUsesCommittedWinner(t *testing.T) {
	for _, different := range []bool{false, true} {
		name := "same-fingerprint"
		if different {
			name = "different-fingerprint"
		}
		t.Run(name, func(t *testing.T) {
			db := openIdentityDatabase(t)
			db.SetMaxOpenConns(1)
			unitOne, _ := sqlite.NewUnitOfWork(db)
			unitTwo, _ := sqlite.NewUnitOfWork(db)
			store, _ := NewCredentialStore(db, migrate.DialectSQLite, func(ctx context.Context) (*sql.Tx, bool) {
				if tx, ok := unitOne.Transaction(ctx); ok {
					return tx, true
				}
				return unitTwo.Transaction(ctx)
			})
			barrier := newIdempotencyBarrierRepository(store)
			clock := &identityClock{now: time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)}
			ids := &identityIDs{}
			observations := &fakeObservability{}
			newService := func(unit *sqlite.UnitOfWork) *Service {
				service, err := New(Dependencies{Clock: clock, IDs: ids, Faults: &identityFault{}, UoW: unit, Observability: observations, Repository: barrier, Random: func(value []byte) (int, error) {
					for index := range value {
						value[index] = byte(index + 1)
					}
					return len(value), nil
				}, MaximumTTL: 10 * time.Minute, CacheTTL: time.Minute, AllowedKinds: []string{"service"}, AllowedAudiences: []string{"reference-control-plane"}, AllowedScopes: []string{"agent.read", "agent.invoke"}, AllowedTenants: []string{"tenant-a", "tenant-b"}})
				if err != nil {
					t.Fatal(err)
				}
				return service
			}
			services := []*Service{newService(unitOne), newService(unitTwo)}
			type result struct {
				issued IssuedCredential
				err    error
			}
			results := make(chan result, 2)
			for index, service := range services {
				request := issueRequest("shared-cross-service-key")
				if different && index == 1 {
					request.SubjectID = "developer-2"
				}
				go func(service *Service, request IssueRequest) {
					issued, err := service.Issue(context.Background(), request)
					results <- result{issued, err}
				}(service, request)
			}
			var successes, replays, conflicts int
			for range 2 {
				current := <-results
				switch {
				case current.err == nil && current.issued.Credential != "":
					successes++
				case errors.Is(current.err, ErrSecretUnavailable) && current.issued.Credential == "":
					replays++
				case errors.Is(current.err, ErrCredentialConflict) && current.issued.Credential == "":
					conflicts++
				default:
					t.Fatalf("unexpected outcome: %+v %v", current.issued, current.err)
				}
			}
			if successes != 1 || (!different && replays != 1) || (different && conflicts != 1) {
				t.Fatalf("success=%d replay=%d conflict=%d", successes, replays, conflicts)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM arop_credentials`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("credential rows=%d err=%v", count, err)
			}
		})
	}
}

func storeRecord(index int, now time.Time, verifier, idempotency string) CredentialRecord {
	return CredentialRecord{CredentialID: "cred_01956e7b-9abc-7def-8abc-" + leftPad(index), PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", TenantID: "tenant-a", SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.read"}, SecretVerifier: strings.Repeat(verifier, 64), IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Minute), Status: CredentialActive, Revision: 1, IdempotencyDigest: strings.Repeat(idempotency, 64), IdempotencyRequestDigest: strings.Repeat("e", 64)}
}
func leftPad(value int) string {
	if value == 1 {
		return "000000000001"
	}
	return "000000000002"
}

func openIdentityDatabase(t *testing.T) *sql.DB {
	return openIdentityDatabaseWithSchema(t, exactSQLiteIdentitySchema)
}

func openIdentityDatabaseWithSchema(t *testing.T, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/identity.sqlite?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

const exactSQLiteIdentitySchema = `
CREATE TABLE arop_dev_principals (
  principal_id TEXT NOT NULL, tenant_id TEXT NOT NULL, subject_id TEXT NOT NULL, status TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL, updated_at_ns INTEGER NOT NULL, revision INTEGER NOT NULL,
  CONSTRAINT arop_dev_principals_pkey PRIMARY KEY(principal_id),
  CONSTRAINT arop_dev_principals_tenant_subject_unique UNIQUE(tenant_id,subject_id),
  CONSTRAINT arop_dev_principals_principal_id_check CHECK(length(principal_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_dev_principals_tenant_id_check CHECK(length(tenant_id) BETWEEN 1 AND 128 AND tenant_id NOT GLOB '*[^a-z0-9._-]*' AND substr(tenant_id,1,1) GLOB '[a-z]' AND substr(tenant_id,-1,1) GLOB '[a-z0-9]' AND tenant_id NOT GLOB '*[._-][._-]*'),
  CONSTRAINT arop_dev_principals_subject_id_check CHECK(length(subject_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_dev_principals_status_check CHECK(status IN ('active','disabled')),
  CONSTRAINT arop_dev_principals_created_check CHECK(created_at_ns > 0),
  CONSTRAINT arop_dev_principals_updated_check CHECK(updated_at_ns >= created_at_ns),
  CONSTRAINT arop_dev_principals_revision_check CHECK(revision > 0)
);
CREATE INDEX arop_dev_principals_tenant_status_idx ON arop_dev_principals(tenant_id, status, principal_id);
CREATE TABLE arop_credentials (
  credential_id TEXT NOT NULL, principal_id TEXT NOT NULL, credential_kind TEXT NOT NULL,
  audience TEXT NOT NULL, scope_canonical TEXT NOT NULL, secret_verifier TEXT NOT NULL,
  issued_at_ns INTEGER NOT NULL, not_before_at_ns INTEGER NOT NULL, expires_at_ns INTEGER NOT NULL,
  status TEXT NOT NULL, revoked_at_ns INTEGER, replaced_at_ns INTEGER,
  replacement_credential_id TEXT, revision INTEGER NOT NULL,
  idempotency_key_digest TEXT NOT NULL, idempotency_request_digest TEXT NOT NULL,
  CONSTRAINT arop_credentials_pkey PRIMARY KEY(credential_id),
  CONSTRAINT arop_credentials_principal_fkey FOREIGN KEY(principal_id) REFERENCES arop_dev_principals(principal_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
  CONSTRAINT arop_credentials_replacement_fkey FOREIGN KEY(replacement_credential_id) REFERENCES arop_credentials(credential_id) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
  CONSTRAINT arop_credentials_verifier_unique UNIQUE(secret_verifier),
  CONSTRAINT arop_credentials_idempotency_unique UNIQUE(idempotency_key_digest),
  CONSTRAINT arop_credentials_replacement_unique UNIQUE(replacement_credential_id),
  CONSTRAINT arop_credentials_id_check CHECK(length(credential_id) BETWEEN 1 AND 200),
  CONSTRAINT arop_credentials_kind_check CHECK(length(credential_kind) BETWEEN 1 AND 100),
  CONSTRAINT arop_credentials_audience_check CHECK(length(audience) BETWEEN 1 AND 500),
  CONSTRAINT arop_credentials_scope_check CHECK(length(scope_canonical) BETWEEN 1 AND 4096),
  CONSTRAINT arop_credentials_verifier_check CHECK(length(secret_verifier) = 64 AND secret_verifier NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_credentials_idempotency_check CHECK(length(idempotency_key_digest) = 64 AND idempotency_key_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_credentials_idempotency_request_check CHECK(length(idempotency_request_digest) = 64 AND idempotency_request_digest NOT GLOB '*[^0-9a-f]*'),
  CONSTRAINT arop_credentials_time_check CHECK(issued_at_ns > 0 AND not_before_at_ns >= issued_at_ns AND expires_at_ns > not_before_at_ns),
  CONSTRAINT arop_credentials_status_check CHECK(status IN ('active','revoked','replaced')),
  CONSTRAINT arop_credentials_lifecycle_check CHECK(
    (status = 'active' AND revoked_at_ns IS NULL AND replaced_at_ns IS NULL AND replacement_credential_id IS NULL) OR
    (status = 'revoked' AND revoked_at_ns IS NOT NULL AND revoked_at_ns >= issued_at_ns AND replaced_at_ns IS NULL AND replacement_credential_id IS NULL) OR
    (status = 'replaced' AND revoked_at_ns IS NULL AND replaced_at_ns IS NOT NULL AND replaced_at_ns >= issued_at_ns AND replacement_credential_id IS NOT NULL AND replacement_credential_id <> credential_id)
  ),
  CONSTRAINT arop_credentials_revision_check CHECK(revision > 0)
);
CREATE INDEX arop_credentials_principal_status_expiry_idx ON arop_credentials(principal_id, status, expires_at_ns);
CREATE INDEX arop_credentials_audience_status_expiry_idx ON arop_credentials(audience, status, expires_at_ns);`

func TestVerifySchemaSQLite(t *testing.T) {
	if err := VerifySchema(migrate.DialectSQLite)(context.Background(), openIdentityDatabase(t)); err != nil {
		t.Fatalf("exact production schema rejected: %v", err)
	}
	mutations := map[string]func(string) string{
		"renamed-column": func(schema string) string {
			return strings.ReplaceAll(schema, "audience", "audience_removed")
		},
		"missing-constraint": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_scope_check CHECK(length(scope_canonical) BETWEEN 1 AND 4096),\n", "", 1)
		},
		"missing-index": func(schema string) string {
			return strings.Replace(schema, "CREATE INDEX arop_credentials_audience_status_expiry_idx ON arop_credentials(audience, status, expires_at_ns);", "", 1)
		},
		"tampered-time": func(schema string) string {
			return strings.Replace(schema, "expires_at_ns > not_before_at_ns", "expires_at_ns >= not_before_at_ns", 1)
		},
		"tampered-lifecycle": func(schema string) string {
			return strings.Replace(schema, "replacement_credential_id <> credential_id", "replacement_credential_id = credential_id", 1)
		},
		"missing-unique": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_verifier_unique UNIQUE(secret_verifier),\n", "", 1)
		},
		"extra-check": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_revision_check", "  CONSTRAINT arop_credentials_extra_check CHECK(revision < 999999),\n  CONSTRAINT arop_credentials_revision_check", 1)
		},
		"extra-unique": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_revision_check", "  CONSTRAINT arop_credentials_extra_unique UNIQUE(audience),\n  CONSTRAINT arop_credentials_revision_check", 1)
		},
		"extra-foreign-key": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_pkey", "  CONSTRAINT arop_credentials_extra_fkey FOREIGN KEY(principal_id) REFERENCES arop_dev_principals(principal_id),\n  CONSTRAINT arop_credentials_pkey", 1)
		},
		"different-index": func(schema string) string {
			return strings.Replace(schema, "(audience, status, expires_at_ns);", "(audience, expires_at_ns, status);", 1)
		},
		"partial-index": func(schema string) string {
			return strings.Replace(schema, "(audience, status, expires_at_ns);", "(audience, status, expires_at_ns) WHERE status = 'active';", 1)
		},
		"expression-index": func(schema string) string {
			return schema + "\nCREATE INDEX arop_credentials_extra_idx ON arop_credentials(lower(audience));"
		},
		"column-default": func(schema string) string {
			return strings.Replace(schema, "audience TEXT NOT NULL", "audience TEXT NOT NULL DEFAULT 'reference-control-plane'", 1)
		},
		"comment-false-positive": func(schema string) string {
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_scope_check CHECK(length(scope_canonical) BETWEEN 1 AND 4096),", "  -- CONSTRAINT arop_credentials_scope_check CHECK(length(scope_canonical) BETWEEN 1 AND 4096),", 1)
		},
		"wrong-table": func(schema string) string {
			schema = strings.Replace(schema, "  CONSTRAINT arop_dev_principals_created_check CHECK(created_at_ns > 0),\n", "", 1)
			return strings.Replace(schema, "  CONSTRAINT arop_credentials_revision_check", "  CONSTRAINT arop_dev_principals_created_check CHECK(issued_at_ns > 0),\n  CONSTRAINT arop_credentials_revision_check", 1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			db := openIdentityDatabaseWithSchema(t, mutate(exactSQLiteIdentitySchema))
			if err := VerifySchema(migrate.DialectSQLite)(context.Background(), db); err == nil {
				t.Fatal("tampered identity schema accepted")
			}
		})
	}
}

func TestVerifySchemaRejectsRowsIterationFailure(t *testing.T) {
	const driverName = "identity-schema-iteration-error"
	iterationDriverOnce.Do(func() { sql.Register(driverName, iterationErrorDriver{}) })
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := VerifySchema(migrate.DialectSQLite)(context.Background(), db); err == nil {
		t.Fatal("row iteration failure was accepted")
	}
}

func TestVerifySchemaSQLiteSingleConnectionTransaction(t *testing.T) {
	db := openIdentityDatabase(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(migrate.DialectSQLite)(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("single-connection verifier failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	tampered := openIdentityDatabaseWithSchema(t, strings.Replace(exactSQLiteIdentitySchema, "expires_at_ns > not_before_at_ns", "expires_at_ns >= not_before_at_ns", 1))
	tampered.SetMaxOpenConns(1)
	tamperedTx, err := tampered.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(migrate.DialectSQLite)(ctx, tamperedTx); err == nil {
		_ = tamperedTx.Rollback()
		t.Fatal("single-connection verifier accepted tampered schema")
	}
	if err := tamperedTx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

type iterationErrorDriver struct{}

var iterationDriverOnce sync.Once

func (iterationErrorDriver) Open(string) (driver.Conn, error) { return iterationErrorConn{}, nil }

type iterationErrorConn struct{}

func (iterationErrorConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (iterationErrorConn) Close() error                        { return nil }
func (iterationErrorConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (iterationErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return iterationErrorRows{}, nil
}

type iterationErrorRows struct{}

func (iterationErrorRows) Columns() []string {
	return []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
}
func (iterationErrorRows) Close() error { return nil }
func (iterationErrorRows) Next([]driver.Value) error {
	return errors.New("injected row iteration failure")
}
