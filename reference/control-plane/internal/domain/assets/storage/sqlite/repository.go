package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

// Repository persists asset descriptors and grants in SQLite.
// Every write requires the caller's UnitOfWork transaction.
type Repository struct {
	db     *sql.DB
	lookup durable.TransactionLookup
}

func New(db *sql.DB, lookup durable.TransactionLookup) (*Repository, error) {
	if db == nil || lookup == nil {
		return nil, errors.New("SQLite asset repository dependencies are required")
	}
	return &Repository{db: db, lookup: lookup}, nil
}

func (repository *Repository) CreateAsset(ctx context.Context, asset assets.Asset) (assets.Asset, error) {
	if err := asset.Validate(); err != nil {
		return assets.Asset{}, err
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.Asset{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO arop_assets(
tenant_id,asset_id,name,media_type,size_bytes,content_digest,object_key,status,revision,
created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, asset.TenantID, asset.AssetID, asset.Name, asset.MediaType, asset.SizeBytes,
		asset.ContentDigest, asset.ObjectKey, string(asset.Status), asset.Revision, asset.CreatedAtNs, asset.UpdatedAtNs,
		asset.IdempotencyKeyDigest, asset.IdempotencyRequestDigest)
	if err == nil {
		return asset, nil
	}
	return repository.resolveAssetInsert(ctx, tx, asset, err)
}

func (repository *Repository) GetAsset(ctx context.Context, tenantID, assetID string) (assets.Asset, error) {
	return scanAsset(ctx, repository.queryer(ctx), `tenant_id=? AND asset_id=?`, tenantID, assetID)
}

func (repository *Repository) UpdateAsset(ctx context.Context, tenantID, assetID string, expectedRevision int64, update assets.AssetUpdate) (assets.Asset, error) {
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.Asset{}, err
	}
	current, err := scanAsset(ctx, tx, `tenant_id=? AND asset_id=?`, tenantID, assetID)
	if err != nil {
		return assets.Asset{}, err
	}
	if err := update.Validate(current.CreatedAtNs); err != nil {
		return assets.Asset{}, err
	}
	if expectedRevision != current.Revision {
		return assets.Asset{}, assets.NewError(assets.ReasonConflict)
	}
	result, err := tx.ExecContext(ctx, `UPDATE arop_assets SET name=?,media_type=?,size_bytes=?,content_digest=?,object_key=?,status=?,revision=revision+1,updated_at_ns=?
WHERE tenant_id=? AND asset_id=? AND revision=?`, update.Name, update.MediaType, update.SizeBytes, update.ContentDigest,
		update.ObjectKey, string(update.Status), update.UpdatedAtNs, tenantID, assetID, expectedRevision)
	if err != nil {
		return assets.Asset{}, assets.NewError(assets.ReasonUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return assets.Asset{}, assets.NewError(assets.ReasonConflict)
	}
	return scanAsset(ctx, tx, `tenant_id=? AND asset_id=?`, tenantID, assetID)
}

func (repository *Repository) CreateGrant(ctx context.Context, grant assets.Grant) (assets.Grant, error) {
	if err := grant.Validate(); err != nil {
		return assets.Grant{}, err
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.Grant{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO arop_asset_grants(
tenant_id,grant_id,asset_id,run_id,operation,token_digest,use_limit,use_count,expires_at_ns,status,revision,
created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, grant.TenantID, grant.GrantID, grant.AssetID, grant.RunID, string(grant.Operation),
		grant.TokenDigest, grant.UseLimit, grant.UseCount, grant.ExpiresAtNs, string(grant.Status), grant.Revision,
		grant.CreatedAtNs, grant.UpdatedAtNs, grant.IdempotencyKeyDigest, grant.IdempotencyRequestDigest)
	if err == nil {
		return grant, nil
	}
	return repository.resolveGrantInsert(ctx, tx, grant, err)
}

func (repository *Repository) GetGrant(ctx context.Context, tenantID, grantID string) (assets.Grant, error) {
	return scanGrant(ctx, repository.queryer(ctx), `tenant_id=? AND grant_id=?`, tenantID, grantID)
}

func (repository *Repository) ConsumeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (assets.Grant, error) {
	return repository.transitionGrant(ctx, tenantID, grantID, expectedRevision, nowNs, true)
}

func (repository *Repository) RevokeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (assets.Grant, error) {
	return repository.transitionGrant(ctx, tenantID, grantID, expectedRevision, nowNs, false)
}

func (repository *Repository) transitionGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64, consume bool) (assets.Grant, error) {
	if nowNs < 1 {
		return assets.Grant{}, assets.NewError(assets.ReasonValidation)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return assets.Grant{}, err
	}
	current, err := scanGrant(ctx, tx, `tenant_id=? AND grant_id=?`, tenantID, grantID)
	if err != nil {
		return assets.Grant{}, err
	}
	if current.Revision != expectedRevision {
		return assets.Grant{}, assets.NewError(assets.ReasonConflict)
	}
	if current.Status != assets.GrantActive {
		return assets.Grant{}, assets.NewError(assets.ReasonConflict)
	}
	if consume && current.ExpiresAtNs <= nowNs {
		return assets.Grant{}, assets.NewError(assets.ReasonExpired)
	}
	statement := `UPDATE arop_asset_grants SET status='revoked',revision=revision+1,updated_at_ns=? WHERE tenant_id=? AND grant_id=? AND revision=? AND status='active'`
	args := []any{nowNs, tenantID, grantID, expectedRevision}
	if consume {
		statement = `UPDATE arop_asset_grants SET use_count=use_count+1,status=CASE WHEN use_count+1=use_limit THEN 'consumed' ELSE 'active' END,revision=revision+1,updated_at_ns=?
WHERE tenant_id=? AND grant_id=? AND revision=? AND status='active' AND use_count<use_limit AND expires_at_ns>?`
		args = []any{nowNs, tenantID, grantID, expectedRevision, nowNs}
	}
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return assets.Grant{}, assets.NewError(assets.ReasonUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return assets.Grant{}, assets.NewError(assets.ReasonConflict)
	}
	return scanGrant(ctx, tx, `tenant_id=? AND grant_id=?`, tenantID, grantID)
}

func (repository *Repository) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := repository.lookup(ctx)
	if !ok {
		return nil, assets.NewError(assets.ReasonUnavailable)
	}
	return tx, nil
}

func (repository *Repository) queryer(ctx context.Context) migrate.Queryer {
	if tx, ok := repository.lookup(ctx); ok {
		return tx
	}
	return repository.db
}

func (repository *Repository) resolveAssetInsert(ctx context.Context, tx *sql.Tx, asset assets.Asset, insertErr error) (assets.Asset, error) {
	message := strings.ToLower(insertErr.Error())
	if !strings.Contains(message, "unique constraint failed") && !strings.Contains(message, "foreign key") && !strings.Contains(message, "check constraint") {
		return assets.Asset{}, assets.NewError(assets.ReasonUnavailable)
	}
	if strings.Contains(message, "idempotency") {
		stored, err := scanAsset(ctx, tx, `tenant_id=? AND idempotency_key_digest=?`, asset.TenantID, asset.IdempotencyKeyDigest)
		if err != nil {
			return assets.Asset{}, err
		}
		if stored.IdempotencyRequestDigest == asset.IdempotencyRequestDigest {
			return stored, nil
		}
		return assets.Asset{}, assets.NewError(assets.ReasonIdempotencyConflict)
	}
	if strings.Contains(message, "check constraint") || strings.Contains(message, "foreign key") {
		return assets.Asset{}, assets.NewError(assets.ReasonValidation)
	}
	return assets.Asset{}, assets.NewError(assets.ReasonConflict)
}

func (repository *Repository) resolveGrantInsert(ctx context.Context, tx *sql.Tx, grant assets.Grant, insertErr error) (assets.Grant, error) {
	message := strings.ToLower(insertErr.Error())
	if strings.Contains(message, "foreign key") || strings.Contains(message, "check constraint") {
		return assets.Grant{}, assets.NewError(assets.ReasonValidation)
	}
	if !strings.Contains(message, "unique constraint failed") {
		return assets.Grant{}, assets.NewError(assets.ReasonUnavailable)
	}
	stored, err := scanGrant(ctx, tx, `tenant_id=? AND idempotency_key_digest=?`, grant.TenantID, grant.IdempotencyKeyDigest)
	if assets.HasReason(err, assets.ReasonNotFound) {
		return assets.Grant{}, assets.NewError(assets.ReasonConflict)
	}
	if err != nil {
		return assets.Grant{}, err
	}
	if stored.IdempotencyRequestDigest == grant.IdempotencyRequestDigest {
		return stored, nil
	}
	return assets.Grant{}, assets.NewError(assets.ReasonIdempotencyConflict)
}

func scanAsset(ctx context.Context, queryer migrate.Queryer, predicate string, args ...any) (assets.Asset, error) {
	var asset assets.Asset
	var status string
	err := queryer.QueryRowContext(ctx, `SELECT tenant_id,asset_id,name,media_type,size_bytes,content_digest,object_key,status,revision,created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest FROM arop_assets WHERE `+predicate, args...).Scan(
		&asset.TenantID, &asset.AssetID, &asset.Name, &asset.MediaType, &asset.SizeBytes, &asset.ContentDigest, &asset.ObjectKey, &status, &asset.Revision, &asset.CreatedAtNs, &asset.UpdatedAtNs, &asset.IdempotencyKeyDigest, &asset.IdempotencyRequestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Asset{}, assets.NewError(assets.ReasonNotFound)
	}
	if err != nil {
		return assets.Asset{}, assets.NewError(assets.ReasonUnavailable)
	}
	asset.Status = assets.AssetStatus(status)
	if asset.Validate() != nil {
		return assets.Asset{}, assets.NewError(assets.ReasonUnavailable)
	}
	return asset, nil
}

func scanGrant(ctx context.Context, queryer migrate.Queryer, predicate string, args ...any) (assets.Grant, error) {
	var grant assets.Grant
	var operation, status string
	err := queryer.QueryRowContext(ctx, `SELECT tenant_id,grant_id,asset_id,run_id,operation,token_digest,use_limit,use_count,expires_at_ns,status,revision,created_at_ns,updated_at_ns,idempotency_key_digest,idempotency_request_digest FROM arop_asset_grants WHERE `+predicate, args...).Scan(
		&grant.TenantID, &grant.GrantID, &grant.AssetID, &grant.RunID, &operation, &grant.TokenDigest, &grant.UseLimit, &grant.UseCount, &grant.ExpiresAtNs, &status, &grant.Revision, &grant.CreatedAtNs, &grant.UpdatedAtNs, &grant.IdempotencyKeyDigest, &grant.IdempotencyRequestDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return assets.Grant{}, assets.NewError(assets.ReasonNotFound)
	}
	if err != nil {
		return assets.Grant{}, assets.NewError(assets.ReasonUnavailable)
	}
	grant.Operation = assets.Operation(operation)
	grant.Status = assets.GrantStatus(status)
	if grant.Validate() != nil {
		return assets.Grant{}, assets.NewError(assets.ReasonUnavailable)
	}
	return grant, nil
}

var _ assets.Repository = (*Repository)(nil)
