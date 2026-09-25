package postgres_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	postgresadapter "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/storage/postgres"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	assetstore "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets/storage/postgres"
)

const (
	assetID = "asset_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	grantID = "grnt_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	runID   = "run_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
)

func TestPostgresAssetRepository(t *testing.T) {
	dsn := os.Getenv("AROP_P13_POSTGRES_URL")
	if dsn == "" {
		t.Skip("AROP_P13_POSTGRES_URL is required")
	}
	db, err := postgresadapter.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"0001_base.sql", "0005_identity.sql", "0010_publication.sql", "0020_asset.sql"} {
		contents, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "migrations", "postgres", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(contents), "\n-- arop:statement\n") {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
		}
	}
	if err := assetstore.VerifySchema()(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	unit, err := postgresadapter.NewUnitOfWork(db)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := assetstore.New(db, unit.Transaction)
	if err != nil {
		t.Fatal(err)
	}
	asset := sampleAsset("tenant-a", strings.Repeat("a", 64))
	if _, err := repository.CreateAsset(context.Background(), asset); !assets.HasReason(err, assets.ReasonUnavailable) {
		t.Fatalf("write outside transaction = %v", err)
	}
	if _, err := within(t, unit, func(ctx context.Context) (assets.Asset, error) { return repository.CreateAsset(ctx, asset) }); err != nil {
		t.Fatal(err)
	}
	replay, err := within(t, unit, func(ctx context.Context) (assets.Asset, error) { return repository.CreateAsset(ctx, asset) })
	if err != nil || replay.Revision != 1 {
		t.Fatalf("idempotent replay = %+v, %v", replay, err)
	}
	conflict := asset
	conflict.IdempotencyRequestDigest = strings.Repeat("b", 64)
	if _, err := within(t, unit, func(ctx context.Context) (assets.Asset, error) { return repository.CreateAsset(ctx, conflict) }); !assets.HasReason(err, assets.ReasonIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := repository.GetAsset(context.Background(), "tenant-b", asset.AssetID); !assets.HasReason(err, assets.ReasonNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	updated, err := within(t, unit, func(ctx context.Context) (assets.Asset, error) {
		return repository.UpdateAsset(ctx, asset.TenantID, asset.AssetID, 1, assets.AssetUpdate{
			Name: "photo.png", MediaType: "image/png", SizeBytes: 12, ContentDigest: asset.ContentDigest,
			ObjectKey: "tenant-a/photo.png", Status: assets.AssetAvailable, UpdatedAtNs: asset.CreatedAtNs + 5,
		})
	})
	if err != nil || updated.Revision != 2 || updated.Status != assets.AssetAvailable {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := within(t, unit, func(ctx context.Context) (assets.Asset, error) {
		return repository.UpdateAsset(ctx, asset.TenantID, asset.AssetID, 1, assets.AssetUpdate{
			Name: "photo.png", MediaType: "image/png", SizeBytes: 12, ContentDigest: asset.ContentDigest,
			ObjectKey: "tenant-a/photo.png", Status: assets.AssetAvailable, UpdatedAtNs: asset.CreatedAtNs + 6,
		})
	}); !assets.HasReason(err, assets.ReasonConflict) {
		t.Fatalf("stale CAS = %v", err)
	}
	grant := sampleGrant(asset.TenantID, strings.Repeat("c", 64), 1)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) { return repository.CreateGrant(ctx, grant) }); err != nil {
		t.Fatal(err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) { return repository.CreateGrant(ctx, grant) }); err != nil {
		t.Fatal(err)
	}
	other := grant
	other.IdempotencyRequestDigest = strings.Repeat("d", 64)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) { return repository.CreateGrant(ctx, other) }); !assets.HasReason(err, assets.ReasonIdempotencyConflict) {
		t.Fatalf("grant idempotency conflict = %v", err)
	}
	missing := grant
	missing.GrantID = "grnt_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	missing.AssetID = "asset_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	missing.IdempotencyKeyDigest = strings.Repeat("e", 64)
	missing.TokenDigest = strings.Repeat("7", 64)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) { return repository.CreateGrant(ctx, missing) }); !assets.HasReason(err, assets.ReasonValidation) {
		t.Fatalf("grant without asset = %v", err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) {
		return repository.ConsumeGrant(ctx, grant.TenantID, grant.GrantID, 1, grant.ExpiresAtNs)
	}); !assets.HasReason(err, assets.ReasonExpired) {
		t.Fatalf("expired consume = %v", err)
	}
	revoked, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) {
		return repository.RevokeGrant(ctx, grant.TenantID, grant.GrantID, 1, grant.CreatedAtNs+1)
	})
	if err != nil || revoked.Status != assets.GrantRevoked {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.Grant, error) {
		return repository.ConsumeGrant(ctx, "tenant-b", grant.GrantID, 2, grant.CreatedAtNs+1)
	}); !assets.HasReason(err, assets.ReasonNotFound) {
		t.Fatalf("cross-tenant consume = %v", err)
	}
	var references int
	if err := db.QueryRow(`SELECT count(*) FROM pg_constraint fk JOIN pg_class c ON c.oid=fk.confrelid JOIN pg_class t ON t.oid=fk.conrelid WHERE t.relname IN ('arop_assets','arop_asset_grants') AND c.relname='arop_runs'`).Scan(&references); err != nil || references != 0 {
		t.Fatalf("run foreign key count = %d, %v", references, err)
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	if repository, err := assetstore.New(nil, nil); err == nil || repository != nil {
		t.Fatal("New accepted missing dependencies")
	}
}

func sampleAsset(tenant, key string) assets.Asset {
	return assets.Asset{
		TenantID: tenant, AssetID: assetID, Name: "product.png", MediaType: "image/png", SizeBytes: 32,
		ContentDigest: "sha256:" + strings.Repeat("ab", 32), ObjectKey: "objects/product.png", Status: assets.AssetPending,
		Revision: 1, CreatedAtNs: 1_700_000_000_000_000_000, UpdatedAtNs: 1_700_000_000_000_000_000,
		IdempotencyKeyDigest: key, IdempotencyRequestDigest: strings.Repeat("1", 64),
	}
}

func sampleGrant(tenant, key string, limit int64) assets.Grant {
	return assets.Grant{
		TenantID: tenant, GrantID: grantID, AssetID: assetID, RunID: runID, Operation: assets.OperationUpload,
		TokenDigest: strings.Repeat("f", 64), UseLimit: limit, UseCount: 0, ExpiresAtNs: 1_700_000_000_000_000_100,
		Status: assets.GrantActive, Revision: 1, CreatedAtNs: 1_700_000_000_000_000_000, UpdatedAtNs: 1_700_000_000_000_000_000,
		IdempotencyKeyDigest: key, IdempotencyRequestDigest: strings.Repeat("2", 64),
	}
}

func within(t *testing.T, unit *postgresadapter.UnitOfWork, callback func(context.Context) (assets.Asset, error)) (assets.Asset, error) {
	t.Helper()
	var result assets.Asset
	err := unit.Within(context.Background(), func(ctx context.Context) error {
		var callErr error
		result, callErr = callback(ctx)
		return callErr
	})
	return result, err
}

func withinGrant(t *testing.T, unit *postgresadapter.UnitOfWork, callback func(context.Context) (assets.Grant, error)) (assets.Grant, error) {
	t.Helper()
	var result assets.Grant
	err := unit.Within(context.Background(), func(ctx context.Context) error {
		var callErr error
		result, callErr = callback(ctx)
		return callErr
	})
	return result, err
}
