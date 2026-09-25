package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

// Repository persists immutable publication versions in SQLite. Mutations are
// accepted only inside the P09 UnitOfWork transaction carried by ctx.
type Repository struct {
	db       *sql.DB
	lookup   durable.TransactionLookup
	digester publication.ManifestDigester
}

func New(db *sql.DB, lookup durable.TransactionLookup, digester publication.ManifestDigester) (*Repository, error) {
	if db == nil || lookup == nil || digester == nil {
		return nil, errors.New("SQLite publication repository dependencies are required")
	}
	return &Repository{db: db, lookup: lookup, digester: digester}, nil
}

func (repository *Repository) Create(ctx context.Context, record *publication.Record) error {
	if record == nil || record.ValidateWithDigest(ctx, repository.digester) != nil {
		return publication.NewError(publication.CategoryValidation, publication.ReasonBundleInvalid)
	}
	tx, ok := repository.lookup(ctx)
	if !ok {
		return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM arop_publications WHERE tenant_id=? AND agent_id=? AND version=?`, record.TenantID, record.AgentID, record.Version).Scan(&exists); err == nil {
		return publication.NewError(publication.CategoryConflict, publication.ReasonImmutableConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM arop_publications WHERE tenant_id=? AND idempotency_key_digest=?`, record.TenantID, record.IdempotencyKeyDigest).Scan(&exists); err == nil {
		return publication.NewError(publication.CategoryConflict, publication.ReasonIdempotencyConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO arop_publications(
tenant_id,agent_id,version,manifest_digest,bundle_semantic_digest,canonical_manifest,
publisher_principal_id,idempotency_key_digest,idempotency_request_digest,published_at_ns,revision
) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.TenantID, record.AgentID, record.Version, record.ManifestDigest,
		record.BundleSemanticDigest, []byte(record.CanonicalManifest), record.PublisherPrincipalID,
		record.IdempotencyKeyDigest, record.IdempotencyRequestDigest, record.PublishedAt.UnixNano(), record.Revision)
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint failed") {
		if strings.Contains(message, "idempotency_key_digest") {
			return publication.NewError(publication.CategoryConflict, publication.ReasonIdempotencyConflict)
		}
		return publication.NewError(publication.CategoryConflict, publication.ReasonImmutableConflict)
	}
	return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
}

func (repository *Repository) Get(ctx context.Context, tenantID, agentID, version string) (publication.Record, error) {
	return repository.query(ctx, `tenant_id=? AND agent_id=? AND version=?`, tenantID, agentID, version)
}

func (repository *Repository) GetByIdempotencyDigest(ctx context.Context, tenantID, digest string) (publication.Record, error) {
	return repository.query(ctx, `tenant_id=? AND idempotency_key_digest=?`, tenantID, digest)
}

func (repository *Repository) query(ctx context.Context, predicate string, arguments ...any) (publication.Record, error) {
	queryer := migrate.Queryer(repository.db)
	if tx, ok := repository.lookup(ctx); ok {
		queryer = tx
	}
	var record publication.Record
	var publishedAt int64
	err := queryer.QueryRowContext(ctx, `SELECT tenant_id,publisher_principal_id,agent_id,version,manifest_digest,
bundle_semantic_digest,canonical_manifest,idempotency_key_digest,idempotency_request_digest,published_at_ns,revision
FROM arop_publications WHERE `+predicate, arguments...).Scan(&record.TenantID, &record.PublisherPrincipalID,
		&record.AgentID, &record.Version, &record.ManifestDigest, &record.BundleSemanticDigest, &record.CanonicalManifest,
		&record.IdempotencyKeyDigest, &record.IdempotencyRequestDigest, &publishedAt, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return publication.Record{}, publication.NewError(publication.CategoryNotFound, publication.ReasonNotFound)
	}
	if err != nil {
		return publication.Record{}, publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	record.PublishedAt = time.Unix(0, publishedAt).UTC()
	if err := record.ValidateWithDigest(ctx, repository.digester); err != nil {
		return publication.Record{}, publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	return record, nil
}

var _ publication.Repository = (*Repository)(nil)
