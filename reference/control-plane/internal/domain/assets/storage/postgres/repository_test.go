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
	assetID      = "asset_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	grantID      = "grant_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	runID        = "run_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	principalID  = "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
	credentialID = "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c"
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
	if _, err := repository.CreateAsset(context.Background(), asset); !assets.HasStorageReason(err, assets.StorageReasonUnavailable) {
		t.Fatalf("write outside transaction = %v", err)
	}
	if _, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) { return repository.CreateAsset(ctx, asset) }); err != nil {
		t.Fatal(err)
	}
	replay, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) { return repository.CreateAsset(ctx, asset) })
	if err != nil || replay.Revision != 1 {
		t.Fatalf("idempotent replay = %+v, %v", replay, err)
	}
	conflict := asset
	conflict.IdempotencyRequestDigest = strings.Repeat("b", 64)
	if _, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) { return repository.CreateAsset(ctx, conflict) }); !assets.HasStorageReason(err, assets.StorageReasonIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := repository.GetAsset(context.Background(), "tenant-b", asset.AssetID); !assets.HasStorageReason(err, assets.StorageReasonNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	updated, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) {
		return repository.UpdateAsset(ctx, asset.TenantID, asset.AssetID, 1, assets.AssetUpdate{
			Name: "photo.png", MediaType: "image/png", SizeBytes: 12, ContentDigest: "sha256:a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447", ContentBytes: []byte("hello world\n"),
			ObjectKey: "tenant-a/photo.png", Status: assets.AssetAvailable, UpdatedAtNs: asset.CreatedAtNs + 5,
		})
	})
	if err != nil || updated.Revision != 2 || updated.Status != assets.AssetAvailable {
		t.Fatalf("update = %+v, %v", updated, err)
	}
	if _, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) {
		return repository.UpdateAsset(ctx, asset.TenantID, asset.AssetID, 2, assets.AssetUpdate{
			Name: "photo.png", MediaType: "image/png", SizeBytes: 0, ContentDigest: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", ContentBytes: []byte{},
			ObjectKey: "tenant-a/photo.png", Status: assets.AssetPending, UpdatedAtNs: asset.CreatedAtNs + 6,
		})
	}); !assets.HasStorageReason(err, assets.StorageReasonConflict) {
		t.Fatalf("ready-to-pending downgrade = %v", err)
	}
	if _, err := within(t, unit, func(ctx context.Context) (assets.AssetRecord, error) {
		return repository.UpdateAsset(ctx, asset.TenantID, asset.AssetID, 1, assets.AssetUpdate{
			Name: "photo.png", MediaType: "image/png", SizeBytes: 12, ContentDigest: "sha256:a948904f2f0f479b8f8197694b30184b0d2ed1c1cd2a1ec0fb85d299a192a447", ContentBytes: []byte("hello world\n"),
			ObjectKey: "tenant-a/photo.png", Status: assets.AssetAvailable, UpdatedAtNs: asset.CreatedAtNs + 6,
		})
	}); !assets.HasStorageReason(err, assets.StorageReasonConflict) {
		t.Fatalf("stale CAS = %v", err)
	}
	grant := sampleGrant(asset.TenantID, strings.Repeat("c", 64), 1)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) { return repository.CreateGrant(ctx, grant) }); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"malformed-principal": `UPDATE arop_assets SET principal_id='prn_-2345678-1234-7123-a123-123456789abc' WHERE asset_id='` + assetID + `'`,
		"empty-media-subtype": `UPDATE arop_assets SET media_type='ab/' WHERE asset_id='` + assetID + `'`,
		"malformed-audience":  `UPDATE arop_asset_grants SET audience='a..b' WHERE grant_id='` + grantID + `'`,
		"grant-backslash":     `UPDATE arop_asset_grants SET name=E'a\\b' WHERE grant_id='` + grantID + `'`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("%s bypassed PostgreSQL schema", name)
		}
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) {
		return repository.ConsumeGrant(ctx, grant.TenantID, grant.GrantID, 1, grant.NotBeforeNs-1)
	}); !assets.HasStorageReason(err, assets.StorageReasonNotYetValid) {
		t.Fatalf("not-yet-valid consume = %v", err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) { return repository.CreateGrant(ctx, grant) }); err != nil {
		t.Fatal(err)
	}
	other := grant
	other.IdempotencyRequestDigest = strings.Repeat("d", 64)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) { return repository.CreateGrant(ctx, other) }); !assets.HasStorageReason(err, assets.StorageReasonIdempotencyConflict) {
		t.Fatalf("grant idempotency conflict = %v", err)
	}
	missing := grant
	missing.GrantID = "grant_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	missing.AssetID = "asset_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5d"
	missing.IdempotencyKeyDigest = strings.Repeat("e", 64)
	missing.TokenDigest = strings.Repeat("7", 64)
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) { return repository.CreateGrant(ctx, missing) }); !assets.HasStorageReason(err, assets.StorageReasonValidation) {
		t.Fatalf("grant without asset = %v", err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) {
		return repository.ConsumeGrant(ctx, grant.TenantID, grant.GrantID, 1, grant.ExpiresAtNs)
	}); !assets.HasStorageReason(err, assets.StorageReasonExpired) {
		t.Fatalf("expired consume = %v", err)
	}
	revoked, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) {
		return repository.RevokeGrant(ctx, grant.TenantID, grant.GrantID, 1, grant.CreatedAtNs+1)
	})
	if err != nil || revoked.Status != assets.GrantRevoked {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if _, err := withinGrant(t, unit, func(ctx context.Context) (assets.GrantRecord, error) {
		return repository.ConsumeGrant(ctx, "tenant-b", grant.GrantID, 2, grant.CreatedAtNs+1)
	}); !assets.HasStorageReason(err, assets.StorageReasonNotFound) {
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

func sampleAsset(tenant, key string) assets.AssetRecord {
	return assets.AssetRecord{
		TenantID: tenant, PrincipalID: principalID, CredentialID: credentialID, RunID: runID, AssetID: assetID, Name: "product.png", MediaType: "image/png", SizeBytes: 32,
		ContentDigest: "sha256:" + strings.Repeat("ab", 32), ObjectKey: "objects/product.png", Status: assets.AssetPending,
		Revision: 1, CreatedAtNs: 1_700_000_000_000_000_000, UpdatedAtNs: 1_700_000_000_000_000_000,
		IdempotencyKeyDigest: key, IdempotencyRequestDigest: strings.Repeat("1", 64),
	}
}

func sampleGrant(tenant, key string, limit int64) assets.GrantRecord {
	return assets.GrantRecord{
		TenantID: tenant, PrincipalID: principalID, CredentialID: credentialID, GrantID: grantID, AssetID: assetID, RunID: runID, Operation: assets.StorageOperationUpload,
		Name: "product.png", MediaType: "image/png", SizeBytes: 32, ContentDigest: "sha256:" + strings.Repeat("ab", 32), Audience: "asset-broker", TokenKeyID: "atk_v1",
		TokenDigest: strings.Repeat("f", 64), UseLimit: limit, UseCount: 0, NotBeforeNs: 1_700_000_000_000_000_000, ExpiresAtNs: 1_700_000_000_000_000_100,
		Status: assets.GrantActive, Revision: 1, CreatedAtNs: 1_700_000_000_000_000_000, UpdatedAtNs: 1_700_000_000_000_000_000,
		IdempotencyKeyDigest: key, IdempotencyRequestDigest: strings.Repeat("2", 64),
	}
}

func within(t *testing.T, unit *postgresadapter.UnitOfWork, callback func(context.Context) (assets.AssetRecord, error)) (assets.AssetRecord, error) {
	t.Helper()
	var result assets.AssetRecord
	err := unit.Within(context.Background(), func(ctx context.Context) error {
		var callErr error
		result, callErr = callback(ctx)
		return callErr
	})
	return result, err
}

func withinGrant(t *testing.T, unit *postgresadapter.UnitOfWork, callback func(context.Context) (assets.GrantRecord, error)) (assets.GrantRecord, error) {
	t.Helper()
	var result assets.GrantRecord
	err := unit.Within(context.Background(), func(ctx context.Context) error {
		var callErr error
		result, callErr = callback(ctx)
		return callErr
	})
	return result, err
}
