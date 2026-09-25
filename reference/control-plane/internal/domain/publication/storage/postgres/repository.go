package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
	"github.com/jackc/pgx/v5/pgconn"
)

// Repository persists immutable publication versions in PostgreSQL. It never
// opens a transaction and cannot bypass the P09 UnitOfWork.
type Repository struct {
	db       *sql.DB
	lookup   durable.TransactionLookup
	digester publication.ManifestDigester
}

func New(db *sql.DB, lookup durable.TransactionLookup, digester publication.ManifestDigester) (*Repository, error) {
	if db == nil || lookup == nil || digester == nil {
		return nil, errors.New("PostgreSQL publication repository dependencies are required")
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
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM arop_publications WHERE tenant_id=$1 AND agent_id=$2 AND version=$3`, record.TenantID, record.AgentID, record.Version).Scan(&exists); err == nil {
		return publication.NewError(publication.CategoryConflict, publication.ReasonImmutableConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM arop_publications WHERE tenant_id=$1 AND idempotency_key_digest=$2`, record.TenantID, record.IdempotencyKeyDigest).Scan(&exists); err == nil {
		return publication.NewError(publication.CategoryConflict, publication.ReasonIdempotencyConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO arop_publications(
tenant_id,agent_id,version,manifest_digest,bundle_semantic_digest,canonical_manifest,
publisher_principal_id,idempotency_key_digest,idempotency_request_digest,published_at_ns,revision
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, record.TenantID, record.AgentID, record.Version,
		record.ManifestDigest, record.BundleSemanticDigest, []byte(record.CanonicalManifest), record.PublisherPrincipalID,
		record.IdempotencyKeyDigest, record.IdempotencyRequestDigest, record.PublishedAt.UnixNano(), record.Revision)
	if err == nil {
		return nil
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "23505" {
		if databaseError.ConstraintName == "arop_publications_idempotency_unique" {
			return publication.NewError(publication.CategoryConflict, publication.ReasonIdempotencyConflict)
		}
		return publication.NewError(publication.CategoryConflict, publication.ReasonImmutableConflict)
	}
	return publication.NewError(publication.CategoryDependency, publication.ReasonDependencyUnavailable)
}

func (repository *Repository) Get(ctx context.Context, tenantID, agentID, version string) (publication.Record, error) {
	return repository.query(ctx, `tenant_id=$1 AND agent_id=$2 AND version=$3`, tenantID, agentID, version)
}

func (repository *Repository) GetByIdempotencyDigest(ctx context.Context, tenantID, digest string) (publication.Record, error) {
	return repository.query(ctx, `tenant_id=$1 AND idempotency_key_digest=$2`, tenantID, digest)
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
