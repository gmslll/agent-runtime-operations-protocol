package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	publicationstore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication/storage/postgres"
)

type digester struct{}

func (digester) DigestManifest(_ context.Context, value []byte) (string, error) {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	if repository, err := publicationstore.New(nil, nil, nil); err == nil || repository != nil {
		t.Fatal("New accepted missing durable dependencies")
	}
}

func TestPostgresRepositoryAndSchema(t *testing.T) {
	dsn := os.Getenv("AROP_P12_POSTGRES_URL")
	if dsn == "" {
		t.Skip("AROP_P12_POSTGRES_URL is required for PostgreSQL integration")
	}
	db, err := postgresadapter.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql"} {
		migrationPath := filepath.Join("..", "..", "..", "..", "..", "migrations", "postgres", name)
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
	unit, err := postgresadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := publicationstore.New(db, unit.Transaction, digester{})
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"kind":"AgentManifest"}`)
	manifestDigest, _ := (digester{}).DigestManifest(context.Background(), manifest)
	record := publication.Record{
		TenantID: "tenant-a", PublisherPrincipalID: "prn_01890abc-def0-7123-8abc-def012345678", AgentID: "agent.echo", Version: "1.0.0",
		ManifestDigest: manifestDigest, BundleSemanticDigest: "sha256:" + strings.Repeat("c", 64), CanonicalManifest: manifest,
		IdempotencyKeyDigest: strings.Repeat("a", 64), IdempotencyRequestDigest: strings.Repeat("d", 64),
		PublishedAt: time.Date(2026, 9, 25, 1, 2, 3, 0, time.UTC), Revision: 1,
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) }); !hasReason(err, publication.ReasonImmutableConflict) {
		t.Fatalf("duplicate immutable Create error = %v", err)
	}
	second := record
	second.Version = "1.0.1"
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &second) }); !hasReason(err, publication.ReasonIdempotencyConflict) {
		t.Fatalf("duplicate idempotency Create error = %v", err)
	}
	got, err := repository.Get(context.Background(), record.TenantID, record.AgentID, record.Version)
	if err != nil || got.ManifestDigest != record.ManifestDigest || string(got.CanonicalManifest) != string(record.CanonicalManifest) {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
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

func hasReason(err error, reason publication.ErrorReason) bool {
	failure, ok := publication.AsError(err)
	return ok && failure.Reason == reason
}
