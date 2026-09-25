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
	byIdempotency, err := repository.GetByIdempotencyDigest(context.Background(), record.TenantID, record.IdempotencyKeyDigest)
	if err != nil || byIdempotency.AgentID != record.AgentID {
		t.Fatalf("GetByIdempotencyDigest() = %#v, %v", byIdempotency, err)
	}
	if _, err := repository.GetByIdempotencyDigest(context.Background(), "tenant-b", record.IdempotencyKeyDigest); !hasReason(err, publication.ReasonNotFound) {
		t.Fatalf("cross-tenant idempotency lookup error = %v", err)
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
	rolledBack := validRecord(t, record.TenantID, "agent.rollback", "1.0.0", strings.Repeat("e", 64))
	if err := unit.Within(context.Background(), func(ctx context.Context) error {
		if err := repository.Create(ctx, &rolledBack); err != nil {
			return err
		}
		return errors.New("force rollback")
	}); err == nil {
		t.Fatal("unit of work accepted forced rollback")
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
	for _, constraint := range []string{
		"arop_publications_pkey", "arop_publications_idempotency_unique", "arop_publications_tenant_check",
		"arop_publications_agent_check", "arop_publications_version_check", "arop_publications_manifest_digest_check",
		"arop_publications_bundle_digest_check", "arop_publications_manifest_check", "arop_publications_publisher_check",
		"arop_publications_idempotency_key_check", "arop_publications_idempotency_request_check",
		"arop_publications_published_check", "arop_publications_revision_check",
	} {
		t.Run("missing-"+constraint, func(t *testing.T) {
			db, _, _ := newRepository(t)
			replaceSQLiteTable(t, db, removeSQLiteConstraint(t, sqlitePublicationDDL(t), constraint))
			if err := publicationstore.VerifySchema()(context.Background(), db); err == nil {
				t.Fatal("VerifySchema accepted missing SQLite constraint")
			}
		})
	}
	for name, tamper := range map[string]func(*testing.T, *sql.DB){
		"missing-manifest-index":  func(t *testing.T, db *sql.DB) { mustExec(t, db, `DROP INDEX arop_publications_manifest_digest_idx`) },
		"missing-published-index": func(t *testing.T, db *sql.DB) { mustExec(t, db, `DROP INDEX arop_publications_tenant_published_idx`) },
		"extra-index": func(t *testing.T, db *sql.DB) {
			mustExec(t, db, `CREATE INDEX arop_publications_extra_idx ON arop_publications(version)`)
		},
		"weakened-constraint": func(t *testing.T, db *sql.DB) {
			replaceSQLiteTable(t, db, strings.Replace(sqlitePublicationDDL(t), "CHECK(revision = 1)", "CHECK(revision >= 1)", 1))
		},
		"extra-constraint": func(t *testing.T, db *sql.DB) {
			ddl := sqlitePublicationDDL(t)
			replaceSQLiteTable(t, db, strings.TrimSuffix(ddl, ")")+", CONSTRAINT arop_publications_extra CHECK(revision = 1))")
		},
		"without-rowid": func(t *testing.T, db *sql.DB) {
			replaceSQLiteTable(t, db, strings.TrimSuffix(sqlitePublicationDDL(t), "\n")+" WITHOUT ROWID\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, _, _ := newRepository(t)
			if err := publicationstore.VerifySchema()(context.Background(), db); err != nil {
				t.Fatalf("VerifySchema() error = %v", err)
			}
			tamper(t, db)
			if err := publicationstore.VerifySchema()(context.Background(), db); err == nil {
				t.Fatal("VerifySchema accepted tampered schema")
			}
		})
	}
}

func removeSQLiteConstraint(t *testing.T, ddl, name string) string {
	t.Helper()
	lines := strings.Split(ddl, "\n")
	found := false
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, "CONSTRAINT "+name+" ") {
			found = true
			continue
		}
		filtered = append(filtered, line)
	}
	if !found {
		t.Fatalf("constraint %s not found", name)
	}
	for index := len(filtered) - 1; index >= 0; index-- {
		if strings.TrimSpace(filtered[index]) == ")" && index > 0 {
			filtered[index-1] = strings.TrimSuffix(filtered[index-1], ",")
			break
		}
	}
	return strings.Join(filtered, "\n")
}

func TestConcurrentCreateReturnsTypedConflict(t *testing.T) {
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
			db, unit, repository := newRepository(t)
			first := validRecord(t, "tenant-a", "agent.echo", "1.0.0", strings.Repeat("a", 64))
			second := test.second(first)
			start := make(chan struct{})
			results := make(chan error, 2)
			for _, record := range []publication.Record{first, second} {
				record := record
				go func() {
					<-start
					results <- unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &record) })
				}()
			}
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

func TestConcurrentCreateContinuesAfterWinnerRollback(t *testing.T) {
	for _, test := range []struct {
		name   string
		second func(publication.Record) publication.Record
	}{
		{"version-key", func(record publication.Record) publication.Record {
			record.IdempotencyKeyDigest = strings.Repeat("e", 64)
			return record
		}},
		{"idempotency-key", func(record publication.Record) publication.Record { record.Version = "1.0.1"; return record }},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, unit, repository := newRepository(t)
			winner := validRecord(t, "tenant-a", "agent.echo", "1.0.0", strings.Repeat("a", 64))
			follower := test.second(winner)
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
			followerStarted, followerDone := make(chan struct{}), make(chan error, 1)
			go func() {
				close(followerStarted)
				followerDone <- unit.Within(context.Background(), func(ctx context.Context) error { return repository.Create(ctx, &follower) })
			}()
			<-followerStarted
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
			if _, err := repository.Get(context.Background(), follower.TenantID, follower.AgentID, follower.Version); err != nil {
				t.Fatalf("follower record missing: %v", err)
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func sqlitePublicationDDL(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "migrations", "sqlite", "0010_publication.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(contents), "\n-- arop:statement\n")[0]
}

func replaceSQLiteTable(t *testing.T, db *sql.DB, ddl string) {
	t.Helper()
	mustExec(t, db, `DROP TABLE arop_publications`)
	mustExec(t, db, ddl)
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
