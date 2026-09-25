package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if err := repository.Create(context.Background(), &record); !hasReason(err, publication.ReasonDependencyUnavailable) {
		t.Fatalf("Create outside unit of work error = %v", err)
	}
	invalid := record
	invalid.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
	if err := unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &invalid) }); !hasReason(err, publication.ReasonBundleInvalid) {
		t.Fatalf("invalid digest Create error = %v", err)
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
	byIdempotency, err := repository.GetByIdempotencyDigest(context.Background(), record.TenantID, record.IdempotencyKeyDigest)
	if err != nil || byIdempotency.Version != record.Version {
		t.Fatalf("GetByIdempotencyDigest() = %#v, %v", byIdempotency, err)
	}
	if _, err := repository.Get(context.Background(), "tenant-b", record.AgentID, record.Version); !hasReason(err, publication.ReasonNotFound) {
		t.Fatalf("cross-tenant Get error = %v", err)
	}
	if _, err := repository.GetByIdempotencyDigest(context.Background(), "tenant-b", record.IdempotencyKeyDigest); !hasReason(err, publication.ReasonNotFound) {
		t.Fatalf("cross-tenant idempotency error = %v", err)
	}
	rolledBack := record
	rolledBack.AgentID = "agent.rollback"
	rolledBack.IdempotencyKeyDigest = strings.Repeat("e", 64)
	if err := unit.Within(context.Background(), func(ctx context.Context) error {
		if err := repository.Create(ctx, &rolledBack); err != nil {
			return err
		}
		return context.Canceled
	}); err == nil {
		t.Fatal("unit of work accepted forced rollback")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM arop_publications`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("publication count = %d, %v", count, err)
	}
	if err := publicationstore.VerifySchema()(context.Background(), db); err != nil {
		t.Fatalf("VerifySchema() error = %v", err)
	}
	for _, constraint := range []string{
		"arop_publications_pkey", "arop_publications_idempotency_unique", "arop_publications_tenant_check",
		"arop_publications_agent_check", "arop_publications_version_check", "arop_publications_manifest_digest_check",
		"arop_publications_bundle_digest_check", "arop_publications_manifest_check", "arop_publications_publisher_check",
		"arop_publications_idempotency_key_check", "arop_publications_idempotency_request_check",
		"arop_publications_published_check", "arop_publications_revision_check",
	} {
		t.Run("missing-"+constraint, func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`ALTER TABLE arop_publications DROP CONSTRAINT ` + constraint); err != nil {
				t.Fatal(err)
			}
			if err := publicationstore.VerifySchema()(context.Background(), tx); err == nil {
				t.Fatal("VerifySchema accepted missing PostgreSQL constraint")
			}
		})
	}
	for name, mutations := range map[string][2]string{
		"extra-constraint":     {`ALTER TABLE arop_publications ADD CONSTRAINT unexpected_publication_constraint CHECK (revision = 1)`, `ALTER TABLE arop_publications DROP CONSTRAINT unexpected_publication_constraint`},
		"missing-constraint":   {`ALTER TABLE arop_publications DROP CONSTRAINT arop_publications_publisher_check`, `ALTER TABLE arop_publications ADD CONSTRAINT arop_publications_publisher_check CHECK(length(publisher_principal_id) BETWEEN 1 AND 200)`},
		"rewritten-constraint": {`ALTER TABLE arop_publications DROP CONSTRAINT arop_publications_revision_check; ALTER TABLE arop_publications ADD CONSTRAINT arop_publications_revision_check CHECK(revision >= 1)`, `ALTER TABLE arop_publications DROP CONSTRAINT arop_publications_revision_check; ALTER TABLE arop_publications ADD CONSTRAINT arop_publications_revision_check CHECK(revision = 1)`},
		"extra-index":          {`CREATE INDEX unexpected_publication_index ON arop_publications(version)`, `DROP INDEX unexpected_publication_index`},
		"missing-index":        {`DROP INDEX arop_publications_manifest_digest_idx`, `CREATE INDEX arop_publications_manifest_digest_idx ON arop_publications(tenant_id, manifest_digest)`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.Exec(mutations[0]); err != nil {
				t.Fatal(err)
			}
			if err := publicationstore.VerifySchema()(context.Background(), db); err == nil {
				t.Fatal("VerifySchema accepted tampered PostgreSQL schema")
			}
			if _, err := db.Exec(mutations[1]); err != nil {
				t.Fatal(err)
			}
			if err := publicationstore.VerifySchema()(context.Background(), db); err != nil {
				t.Fatalf("restored schema rejected: %v", err)
			}
		})
	}
	if _, err := db.Exec(`CREATE TABLE publication_tenants(tenant_id TEXT PRIMARY KEY); INSERT INTO publication_tenants VALUES('tenant-a'); ALTER TABLE arop_publications ADD CONSTRAINT unexpected_publication_fk FOREIGN KEY(tenant_id) REFERENCES publication_tenants(tenant_id)`); err != nil {
		t.Fatal(err)
	}
	if err := publicationstore.VerifySchema()(context.Background(), db); err == nil {
		t.Fatal("VerifySchema accepted extra foreign key")
	}
	if _, err := db.Exec(`ALTER TABLE arop_publications DROP CONSTRAINT unexpected_publication_fk; DROP TABLE publication_tenants`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE arop_publications SET manifest_digest=$1`, "sha256:"+strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(context.Background(), record.TenantID, record.AgentID, record.Version); !hasReason(err, publication.ReasonDependencyUnavailable) {
		t.Fatalf("tampered hydration error = %v", err)
	}
	if _, err := db.Exec(`UPDATE arop_publications SET manifest_digest=$1`, record.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM arop_publications`); err != nil {
		t.Fatal(err)
	}
	testPostgresCreateRaces(t, db, unit, repository, record)
	testPostgresRollbackRaces(t, db, unit, repository, record)
	testPostgresLockCancellation(t, db, unit, repository, record)
}

func hasReason(err error, reason publication.ErrorReason) bool {
	failure, ok := publication.AsError(err)
	return ok && failure.Reason == reason
}

func testPostgresCreateRaces(t *testing.T, db *sql.DB, unit *postgresadapter.UnitOfWork, repository *publicationstore.Repository, base publication.Record) {
	t.Helper()
	for _, test := range []struct {
		name       string
		second     func(publication.Record) publication.Record
		wantReason publication.ErrorReason
	}{
		{"immutable-version", func(record publication.Record) publication.Record {
			record.IdempotencyKeyDigest = strings.Repeat("e", 64)
			return record
		}, publication.ReasonImmutableConflict},
		{"tenant-idempotency", func(record publication.Record) publication.Record { record.Version = "1.0.1"; return record }, publication.ReasonIdempotencyConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM arop_publications`); err != nil {
				t.Fatal(err)
			}
			first, second := base, test.second(base)
			start := make(chan struct{})
			results := make(chan error, 2)
			var ready sync.WaitGroup
			ready.Add(2)
			for _, record := range []publication.Record{first, second} {
				record := record
				go func() {
					ready.Done()
					<-start
					results <- unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) })
				}()
			}
			ready.Wait()
			close(start)
			one, two := <-results, <-results
			if (one == nil) == (two == nil) {
				t.Fatalf("race results = %v, %v", one, two)
			}
			loser := one
			if loser == nil {
				loser = two
			}
			if !hasReason(loser, test.wantReason) {
				t.Fatalf("loser error = %v, want %s", loser, test.wantReason)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM arop_publications`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("publication count = %d, %v", count, err)
			}
		})
	}
}

func testPostgresRollbackRaces(t *testing.T, db *sql.DB, unit *postgresadapter.UnitOfWork, repository *publicationstore.Repository, base publication.Record) {
	t.Helper()
	for _, test := range []struct {
		name   string
		second func(publication.Record) publication.Record
	}{
		{"version-key-rollback", func(record publication.Record) publication.Record {
			record.IdempotencyKeyDigest = strings.Repeat("e", 64)
			return record
		}},
		{"idempotency-key-rollback", func(record publication.Record) publication.Record { record.Version = "1.0.1"; return record }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := db.Exec(`DELETE FROM arop_publications`); err != nil {
				t.Fatal(err)
			}
			winner, follower := base, test.second(base)
			inserted, release := make(chan struct{}), make(chan struct{})
			winnerDone := make(chan error, 1)
			go func() {
				winnerDone <- unit.Within(context.Background(), func(ctx context.Context) error {
					if err := repository.Create(ctx, &winner); err != nil {
						return err
					}
					close(inserted)
					<-release
					return errors.New("rollback winner")
				})
			}()
			<-inserted
			followerDone := make(chan error, 1)
			go func() {
				followerDone <- unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &follower) })
			}()
			waitForAdvisoryWaiter(t, db)
			close(release)
			if err := <-winnerDone; err == nil {
				t.Fatal("winner unexpectedly committed")
			}
			if err := <-followerDone; err != nil {
				t.Fatalf("follower after rollback error = %v", err)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM arop_publications`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("publication count = %d, %v", count, err)
			}
		})
	}
}

func testPostgresLockCancellation(t *testing.T, db *sql.DB, unit *postgresadapter.UnitOfWork, repository *publicationstore.Repository, base publication.Record) {
	t.Helper()
	t.Run("blocked-lock-context-cancel", func(t *testing.T) {
		if _, err := db.Exec(`DELETE FROM arop_publications`); err != nil {
			t.Fatal(err)
		}
		winner := base
		follower := base
		follower.IdempotencyKeyDigest = strings.Repeat("e", 64)
		inserted, release := make(chan struct{}), make(chan struct{})
		winnerDone := make(chan error, 1)
		go func() {
			winnerDone <- unit.Within(context.Background(), func(ctx context.Context) error {
				if err := repository.Create(ctx, &winner); err != nil {
					return err
				}
				close(inserted)
				<-release
				return errors.New("rollback winner")
			})
		}()
		<-inserted
		ctx, cancel := context.WithCancel(context.Background())
		followerDone := make(chan error, 1)
		go func() {
			followerDone <- unit.Within(ctx, func(txContext context.Context) error { return repository.Create(txContext, &follower) })
		}()
		waitForAdvisoryWaiter(t, db)
		cancel()
		if err := <-followerDone; !hasReason(err, publication.ReasonDependencyUnavailable) {
			t.Fatalf("cancelled lock error = %v", err)
		}
		close(release)
		if err := <-winnerDone; err == nil {
			t.Fatal("winner unexpectedly committed")
		}
	})
}

func waitForAdvisoryWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("concurrent publication transaction did not block on advisory lock")
}
