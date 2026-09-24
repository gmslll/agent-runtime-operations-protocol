package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
	record := CredentialRecord{CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000001", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.invoke", "agent.read"}, SecretVerifier: strings.Repeat("a", 64), IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Minute), Status: CredentialActive, Revision: 1, IdempotencyDigest: strings.Repeat("b", 64)}
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
	if loaded.SubjectID != record.SubjectID || loaded.SecretVerifier != record.SecretVerifier || strings.Contains(loaded.SecretVerifier, credentialPrefix) {
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

func storeRecord(index int, now time.Time, verifier, idempotency string) CredentialRecord {
	return CredentialRecord{CredentialID: "cred_01956e7b-9abc-7def-8abc-" + leftPad(index), PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", SubjectID: "developer-1", Kind: "service", Audience: "reference-control-plane", Scopes: []string{"agent.read"}, SecretVerifier: strings.Repeat(verifier, 64), IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Minute), Status: CredentialActive, Revision: 1, IdempotencyDigest: strings.Repeat(idempotency, 64)}
}
func leftPad(value int) string {
	if value == 1 {
		return "000000000001"
	}
	return "000000000002"
}

func openIdentityDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/identity.sqlite?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema := `CREATE TABLE arop_dev_principals(principal_id TEXT PRIMARY KEY,subject_id TEXT NOT NULL UNIQUE,status TEXT NOT NULL CHECK(status IN ('active','disabled')),created_at_ns INTEGER NOT NULL,updated_at_ns INTEGER NOT NULL,revision INTEGER NOT NULL CHECK(revision>0));
CREATE TABLE arop_credentials(credential_id TEXT PRIMARY KEY,principal_id TEXT NOT NULL REFERENCES arop_dev_principals(principal_id),credential_kind TEXT NOT NULL,audience TEXT NOT NULL,scope_canonical TEXT NOT NULL,secret_verifier TEXT NOT NULL UNIQUE CHECK(length(secret_verifier)=64),issued_at_ns INTEGER NOT NULL,not_before_at_ns INTEGER NOT NULL,expires_at_ns INTEGER NOT NULL,status TEXT NOT NULL CHECK(status IN ('active','revoked','replaced')),revoked_at_ns INTEGER,replaced_at_ns INTEGER,replacement_credential_id TEXT,revision INTEGER NOT NULL CHECK(revision>0),idempotency_key_digest TEXT NOT NULL UNIQUE CHECK(length(idempotency_key_digest)=64));`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}
