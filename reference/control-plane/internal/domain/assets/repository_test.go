package assets

import "testing"

func TestRowsRejectMalformedIdentity(t *testing.T) {
	asset := AssetRecord{TenantID: "Tenant", AssetID: "asset", Name: "a", MediaType: "image/png", ContentDigest: "sha256:" + repeat('a', 64), ObjectKey: "a", Status: AssetPending, Revision: 1, CreatedAtNs: 1, UpdatedAtNs: 1, IdempotencyKeyDigest: repeat('b', 64), IdempotencyRequestDigest: repeat('c', 64)}
	if !HasStorageReason(asset.Validate(), StorageReasonValidation) {
		t.Fatal("malformed asset was accepted")
	}
	grant := GrantRecord{TenantID: "tenant", GrantID: "grant", AssetID: assetID(), RunID: "run", Operation: StorageOperationUpload, TokenDigest: repeat('d', 64), UseLimit: 1, Status: GrantActive, Revision: 1, CreatedAtNs: 1, UpdatedAtNs: 1, ExpiresAtNs: 2, IdempotencyKeyDigest: repeat('e', 64), IdempotencyRequestDigest: repeat('f', 64)}
	if !HasStorageReason(grant.Validate(), StorageReasonValidation) {
		t.Fatal("malformed grant was accepted")
	}
}

func assetID() string { return "asset_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c" }

func repeat(character byte, count int) string {
	value := make([]byte, count)
	for index := range value {
		value[index] = character
	}
	return string(value)
}
