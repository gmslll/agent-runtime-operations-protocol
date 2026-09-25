package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
	"github.com/jackc/pgx/v5/pgconn"
)

// Repository persists asset descriptors and grants in PostgreSQL.
// Every write requires the caller's UnitOfWork transaction.
type Repository struct {
	db     *sql.DB
	lookup durable.TransactionLookup
}

func New(db *sql.DB, lookup durable.TransactionLookup) (*Repository, error) {
	if db == nil || lookup == nil {
		return nil, errors.New("PostgreSQL asset repository dependencies are required")
	}
	return &Repository{db: db, lookup: lookup}, nil
}

func (repository *Repository) CreateAsset(ctx context.Context, asset assets.AssetRecord) (assets.AssetRecord, error) {
	if err := asset.Validate(); err != nil {
		return assets.AssetRecord{}, err
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.AssetRecord{}, err
	}
	if err := lockKeys(ctx, tx, "asset/id/"+asset.TenantID+"/"+asset.AssetID, "asset/idempotency/"+asset.TenantID+"/"+asset.IdempotencyKeyDigest); err != nil {
		return assets.AssetRecord{}, err
	}
	if stored, err := scanAsset(ctx, tx, `tenant_id=$1 AND idempotency_key_digest=$2`, asset.TenantID, asset.IdempotencyKeyDigest); err == nil {
		if stored.IdempotencyRequestDigest == asset.IdempotencyRequestDigest {
			return stored, nil
		}
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonIdempotencyConflict)
	} else if !assets.HasStorageReason(err, assets.StorageReasonNotFound) {
		return assets.AssetRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO arop_assets(
tenant_id,principal_id,credential_id,run_id,asset_id,name,media_type,size_bytes,content_digest,content_bytes,object_key,status,revision,
created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, asset.TenantID, asset.PrincipalID, asset.CredentialID, asset.RunID, asset.AssetID, asset.Name, asset.MediaType, asset.SizeBytes,
		asset.ContentDigest, nonNilBytes(asset.ContentBytes), asset.ObjectKey, string(asset.Status), asset.Revision, asset.CreatedAtNs, asset.UpdatedAtNs,
		asset.IdempotencyKeyDigest, asset.IdempotencyRequestDigest)
	if err == nil {
		return asset, nil
	}
	return resolveAssetInsert(ctx, tx, asset, err)
}

func (repository *Repository) GetAsset(ctx context.Context, tenantID, assetID string) (assets.AssetRecord, error) {
	return scanAsset(ctx, repository.queryer(ctx), `tenant_id=$1 AND asset_id=$2`, tenantID, assetID)
}

func (repository *Repository) UpdateAsset(ctx context.Context, tenantID, assetID string, expectedRevision int64, update assets.AssetUpdate) (assets.AssetRecord, error) {
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.AssetRecord{}, err
	}
	if err := lockKeys(ctx, tx, "asset/id/"+tenantID+"/"+assetID); err != nil {
		return assets.AssetRecord{}, err
	}
	current, err := scanAsset(ctx, tx, `tenant_id=$1 AND asset_id=$2`, tenantID, assetID)
	if err != nil {
		return assets.AssetRecord{}, err
	}
	if err := update.Validate(current.CreatedAtNs); err != nil {
		return assets.AssetRecord{}, err
	}
	if expectedRevision != current.Revision {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	result, err := tx.ExecContext(ctx, `UPDATE arop_assets SET name=$1,media_type=$2,size_bytes=$3,content_digest=$4,content_bytes=$5,object_key=$6,status=$7,revision=revision+1,updated_at_ns=$8
WHERE tenant_id=$9 AND asset_id=$10 AND revision=$11`, update.Name, update.MediaType, update.SizeBytes, update.ContentDigest,
		nonNilBytes(update.ContentBytes), update.ObjectKey, string(update.Status), update.UpdatedAtNs, tenantID, assetID, expectedRevision)
	if err != nil {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	return scanAsset(ctx, tx, `tenant_id=$1 AND asset_id=$2`, tenantID, assetID)
}

func (repository *Repository) CreateGrant(ctx context.Context, grant assets.GrantRecord) (assets.GrantRecord, error) {
	if err := grant.Validate(); err != nil {
		return assets.GrantRecord{}, err
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.GrantRecord{}, err
	}
	if err := lockKeys(ctx, tx,
		"grant/id/"+grant.TenantID+"/"+grant.GrantID,
		"grant/idempotency/"+grant.TenantID+"/"+grant.IdempotencyKeyDigest,
		"grant/token/"+grant.TokenDigest); err != nil {
		return assets.GrantRecord{}, err
	}
	if stored, err := scanGrant(ctx, tx, `tenant_id=$1 AND idempotency_key_digest=$2`, grant.TenantID, grant.IdempotencyKeyDigest); err == nil {
		if stored.IdempotencyRequestDigest == grant.IdempotencyRequestDigest {
			return stored, nil
		}
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonIdempotencyConflict)
	} else if !assets.HasStorageReason(err, assets.StorageReasonNotFound) {
		return assets.GrantRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO arop_asset_grants(
tenant_id,principal_id,credential_id,grant_id,asset_id,run_id,operation,name,media_type,size_bytes,content_digest,audience,token_key_id,token_digest,use_limit,use_count,not_before_ns,expires_at_ns,status,revision,
created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, grant.TenantID, grant.PrincipalID, grant.CredentialID, grant.GrantID, grant.AssetID, grant.RunID, string(grant.Operation),
		grant.Name, grant.MediaType, grant.SizeBytes, grant.ContentDigest, grant.Audience, grant.TokenKeyID, grant.TokenDigest, grant.UseLimit, grant.UseCount, grant.NotBeforeNs, grant.ExpiresAtNs, string(grant.Status), grant.Revision,
		grant.CreatedAtNs, grant.UpdatedAtNs, grant.IdempotencyKeyDigest, grant.IdempotencyRequestDigest)
	if err == nil {
		return grant, nil
	}
	return resolveGrantInsert(ctx, tx, grant, err)
}

func (repository *Repository) GetGrant(ctx context.Context, tenantID, grantID string) (assets.GrantRecord, error) {
	return scanGrant(ctx, repository.queryer(ctx), `tenant_id=$1 AND grant_id=$2`, tenantID, grantID)
}

func (repository *Repository) GetGrantByTokenDigest(ctx context.Context, tokenDigest string) (assets.GrantRecord, error) {
	return scanGrant(ctx, repository.queryer(ctx), `token_digest=$1`, tokenDigest)
}

func (repository *Repository) GetGrantByIdempotencyDigest(ctx context.Context, tenantID, keyDigest string) (assets.GrantRecord, error) {
	return scanGrant(ctx, repository.queryer(ctx), `tenant_id=$1 AND idempotency_key_digest=$2`, tenantID, keyDigest)
}

func (repository *Repository) ConsumeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (assets.GrantRecord, error) {
	return repository.transitionGrant(ctx, tenantID, grantID, expectedRevision, nowNs, true)
}

func (repository *Repository) RevokeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (assets.GrantRecord, error) {
	return repository.transitionGrant(ctx, tenantID, grantID, expectedRevision, nowNs, false)
}

func (repository *Repository) transitionGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64, consume bool) (assets.GrantRecord, error) {
	if nowNs < 1 {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonValidation)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.GrantRecord{}, err
	}
	if err := lockKeys(ctx, tx, "grant/id/"+tenantID+"/"+grantID); err != nil {
		return assets.GrantRecord{}, err
	}
	current, err := scanGrant(ctx, tx, `tenant_id=$1 AND grant_id=$2`, tenantID, grantID)
	if err != nil {
		return assets.GrantRecord{}, err
	}
	if current.Revision != expectedRevision || current.Status != assets.GrantActive {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	if consume && current.ExpiresAtNs <= nowNs {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonExpired)
	}
	statement := `UPDATE arop_asset_grants SET status='revoked',revision=revision+1,updated_at_ns=$1 WHERE tenant_id=$2 AND grant_id=$3 AND revision=$4 AND status='active'`
	args := []any{nowNs, tenantID, grantID, expectedRevision}
	if consume {
		statement = `UPDATE arop_asset_grants SET use_count=use_count+1,status=CASE WHEN use_count+1=use_limit THEN 'consumed' ELSE 'active' END,revision=revision+1,updated_at_ns=$1
WHERE tenant_id=$2 AND grant_id=$3 AND revision=$4 AND status='active' AND use_count<use_limit AND expires_at_ns>$5`
		args = []any{nowNs, tenantID, grantID, expectedRevision, nowNs}
	}
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	return scanGrant(ctx, tx, `tenant_id=$1 AND grant_id=$2`, tenantID, grantID)
}

func (repository *Repository) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := repository.lookup(ctx)
	if !ok {
		return nil, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	return tx, nil
}

func (repository *Repository) queryer(ctx context.Context) migrate.Queryer {
	if tx, ok := repository.lookup(ctx); ok {
		return tx
	}
	return repository.db
}

func lockKeys(ctx context.Context, tx *sql.Tx, keys ...string) error {
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return assets.NewStorageError(assets.StorageReasonUnavailable)
		}
	}
	return nil
}

func resolveAssetInsert(ctx context.Context, tx *sql.Tx, asset assets.AssetRecord, insertErr error) (assets.AssetRecord, error) {
	if !uniqueViolation(insertErr) {
		if constraintViolation(insertErr) {
			return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonValidation)
		}
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	stored, err := scanAsset(ctx, tx, `tenant_id=$1 AND idempotency_key_digest=$2`, asset.TenantID, asset.IdempotencyKeyDigest)
	if assets.HasStorageReason(err, assets.StorageReasonNotFound) {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	if err != nil {
		return assets.AssetRecord{}, err
	}
	if stored.IdempotencyRequestDigest == asset.IdempotencyRequestDigest {
		return stored, nil
	}
	return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonIdempotencyConflict)
}

func resolveGrantInsert(ctx context.Context, tx *sql.Tx, grant assets.GrantRecord, insertErr error) (assets.GrantRecord, error) {
	if constraintViolation(insertErr) && !uniqueViolation(insertErr) {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonValidation)
	}
	if !uniqueViolation(insertErr) {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	stored, err := scanGrant(ctx, tx, `tenant_id=$1 AND idempotency_key_digest=$2`, grant.TenantID, grant.IdempotencyKeyDigest)
	if assets.HasStorageReason(err, assets.StorageReasonNotFound) {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonConflict)
	}
	if err != nil {
		return assets.GrantRecord{}, err
	}
	if stored.IdempotencyRequestDigest == grant.IdempotencyRequestDigest {
		return stored, nil
	}
	return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonIdempotencyConflict)
}

func uniqueViolation(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505"
}

func constraintViolation(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && (databaseError.Code == "23514" || databaseError.Code == "23503" || databaseError.Code == "23505")
}

func scanAsset(ctx context.Context, queryer migrate.Queryer, predicate string, args ...any) (assets.AssetRecord, error) {
	var asset assets.AssetRecord
	var status string
	err := queryer.QueryRowContext(ctx, `SELECT tenant_id,principal_id,credential_id,run_id,asset_id,name,media_type,size_bytes,content_digest,content_bytes,object_key,status,revision,created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest FROM arop_assets WHERE `+predicate, args...).Scan(
		&asset.TenantID, &asset.PrincipalID, &asset.CredentialID, &asset.RunID, &asset.AssetID, &asset.Name, &asset.MediaType, &asset.SizeBytes, &asset.ContentDigest, &asset.ContentBytes, &asset.ObjectKey, &status, &asset.Revision, &asset.CreatedAtNs, &asset.UpdatedAtNs, &asset.IdempotencyKeyDigest, &asset.IdempotencyRequestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonNotFound)
	}
	if err != nil {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	asset.Status = assets.AssetStatus(status)
	if asset.Validate() != nil {
		return assets.AssetRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	return asset, nil
}

func scanGrant(ctx context.Context, queryer migrate.Queryer, predicate string, args ...any) (assets.GrantRecord, error) {
	var grant assets.GrantRecord
	var operation, status string
	err := queryer.QueryRowContext(ctx, `SELECT tenant_id,principal_id,credential_id,grant_id,asset_id,run_id,operation,name,media_type,size_bytes,content_digest,audience,token_key_id,token_digest,use_limit,use_count,not_before_ns,expires_at_ns,status,revision,created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest FROM arop_asset_grants WHERE `+predicate, args...).Scan(
		&grant.TenantID, &grant.PrincipalID, &grant.CredentialID, &grant.GrantID, &grant.AssetID, &grant.RunID, &operation, &grant.Name, &grant.MediaType, &grant.SizeBytes, &grant.ContentDigest, &grant.Audience, &grant.TokenKeyID, &grant.TokenDigest, &grant.UseLimit, &grant.UseCount, &grant.NotBeforeNs, &grant.ExpiresAtNs, &status, &grant.Revision, &grant.CreatedAtNs, &grant.UpdatedAtNs, &grant.IdempotencyKeyDigest, &grant.IdempotencyRequestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonNotFound)
	}
	if err != nil {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	grant.Operation = assets.StorageOperation(operation)
	grant.Status = assets.GrantStatus(status)
	if grant.Validate() != nil {
		return assets.GrantRecord{}, assets.NewStorageError(assets.StorageReasonUnavailable)
	}
	return grant, nil
}

var _ assets.StorageRepository = (*Repository)(nil)

func nonNilBytes(value []byte) []byte { return append([]byte{}, value...) }
