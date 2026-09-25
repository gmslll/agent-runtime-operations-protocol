package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqliteadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/sqlite"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	publicationstore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/sqlite"
)

type digester struct{}

func (digester) DigestManifest(_ context.Context, value []byte) (string, error) {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func TestRepositoryRequiresUnitOfWorkAndPersistsImmutableRecord(t *testing.T) {
	db, unit, repository := newRepository(t)
	record := validRecord(t, "tenant-a", "agent.echo", "1.0.0", strings.Repeat("a", 64))
	if err := repository.Create(context.Background(), &record); !hasReason(err, publication.ReasonDependencyUnavailable) {
		t.Fatalf("Create outside unit of work error = %v", err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := repository.Get(context.Background(), record.TenantID, record.AgentID, record.Version)
	if err != nil || got.IdempotencyRequestDigest != record.IdempotencyRequestDigest || string(got.CanonicalManifest) != string(record.CanonicalManifest) {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	if _, err := repository.Get(context.Background(), "tenant-b", record.AgentID, record.Version); !hasReason(err, publication.ReasonNotFound) {
		t.Fatalf("cross-tenant Get error = %v", err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); !hasReason(err, publication.ReasonImmutableConflict) {
		t.Fatalf("duplicate immutable Create error = %v", err)
	}
	second := validRecord(t, record.TenantID, record.AgentID, "1.0.1", record.IdempotencyKeyDigest)
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &second) }); !hasReason(err, publication.ReasonIdempotencyConflict) {
		t.Fatalf("duplicate idempotency Create error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM arop_publications`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("publication count = %d, %v", count, err)
	}
}

func TestRepositoryValidatesDigestOnWriteAndHydration(t *testing.T) {
	db, unit, repository := newRepository(t)
	record := validRecord(t, "tenant-a", "agent.echo", "1.0.0", strings.Repeat("b", 64))
	record.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); !hasReason(err, publication.ReasonBundleInvalid) {
		t.Fatalf("invalid digest Create error = %v", err)
	}
	record = validRecord(t, "tenant-a", "agent.echo", "1.0.0", strings.Repeat("b", 64))
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE arop_publications SET manifest_digest=?`, "sha256:"+strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(context.Background(), record.TenantID, record.AgentID, record.Version); !hasReason(err, publication.ReasonDependencyUnavailable) {
		t.Fatalf("tampered hydration error = %v", err)
	}
}

func TestVerifySchemaRejectsTamper(t *testing.T) {
	db, _, _ := newRepository(t)
	if err := publicationstore.VerifySchema()(context.Background(), db); err != nil {
		t.Fatalf("VerifySchema() error = %v", err)
	}
	if _, err := db.Exec(`DROP INDEX arop_publications_manifest_digest_idx`); err != nil {
		t.Fatal(err)
	}
	if err := publicationstore.VerifySchema()(context.Background(), db); err == nil {
		t.Fatal("VerifySchema accepted missing index")
	}
}

func newRepository(t *testing.T) (*sql.DB, *sqliteadapter.UnitOfWork, *publicationstore.Repository) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(directory, "publication.sqlite")
	db, err := sqliteadapter.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql"} {
		migrationPath := filepath.Join("..", "..", "..", "..", "..", "migrations", "sqlite", name)
		contents, err := os.ReadFile(migrationPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
		}
	}
	unit, err := sqliteadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := publicationstore.New(db, unit.Transaction, digester{})
	if err != nil {
		t.Fatal(err)
	}
	return db, unit, repository
}

func validRecord(t *testing.T, tenantID, agentID, version, idempotencyDigest string) publication.Record {
	t.Helper()
	manifest := []byte(`{"kind":"AgentManifest"}`)
	digest, _ := (digester{}).DigestManifest(context.Background(), manifest)
	return publication.Record{
		TenantID: tenantID, PublisherPrincipalID: "prn_01890abc-def0-7123-8abc-def012345678", AgentID: agentID, Version: version,
		ManifestDigest: digest, BundleSemanticDigest: "sha256:" + strings.Repeat("c", 64), CanonicalManifest: manifest,
		IdempotencyKeyDigest: idempotencyDigest, IdempotencyRequestDigest: strings.Repeat("d", 64),
		PublishedAt: time.Date(2026, 9, 25, 1, 2, 3, 0, time.UTC), Revision: 1,
	}
}

func hasReason(err error, reason publication.ErrorReason) bool {
	failure, ok := publication.AsError(err)
	return ok && failure.Reason == reason && !errors.Is(err, sql.ErrNoRows)
}
