package assets

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"time"
)

// PersistenceAdapter is the single translation boundary between the asset
// broker domain model and the exact SQLite/PostgreSQL persistence rows.
type PersistenceAdapter struct{ repository StorageRepository }

func NewPersistenceAdapter(repository StorageRepository) (*PersistenceAdapter, error) {
	if repository == nil {
		return nil, errors.New("asset storage repository is required")
	}
	return &PersistenceAdapter{repository: repository}, nil
}

func (adapter *PersistenceAdapter) Put(ctx context.Context, asset Asset) error {
	if adapter == nil || adapter.repository == nil || asset.Validate() != nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	stored, err := adapter.repository.GetAsset(ctx, asset.Binding.TenantID, asset.Binding.AssetID)
	if HasStorageReason(err, StorageReasonNotFound) {
		_, err = adapter.repository.CreateAsset(ctx, assetRecord(asset))
		return translateStorageError(err)
	}
	if err != nil {
		return translateStorageError(err)
	}
	if !assetRecordIdentity(stored, asset.Binding) {
		return NewError(CategoryConflict, ReasonIdempotencyConflict)
	}
	if stored.Status == statusForAsset(asset.State) {
		return nil
	}
	if stored.Status != AssetPending || asset.State != StateReady {
		return NewError(CategoryConflict, ReasonIdempotencyConflict)
	}
	_, err = adapter.repository.UpdateAsset(ctx, stored.TenantID, stored.AssetID, stored.Revision, AssetUpdate{
		Name: asset.Binding.Name, MediaType: asset.Binding.MediaType, SizeBytes: asset.Binding.SizeBytes,
		ContentDigest: asset.Binding.Digest, ContentBytes: append([]byte(nil), asset.Content...), ObjectKey: stored.ObjectKey, Status: statusForAsset(asset.State),
		UpdatedAtNs: assetUpdateTime(asset),
	})
	return translateStorageError(err)
}

func (adapter *PersistenceAdapter) Get(ctx context.Context, tenantID, assetID string) (Asset, error) {
	record, err := adapter.repository.GetAsset(ctx, tenantID, assetID)
	if err != nil {
		return Asset{}, translateStorageError(err)
	}
	return domainAsset(record)
}

func (adapter *PersistenceAdapter) Save(ctx context.Context, grant Grant) error {
	if adapter == nil || adapter.repository == nil || grant.Validate() != nil {
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	_, err := adapter.repository.CreateGrant(ctx, grantRecord(grant))
	return translateStorageError(err)
}

func (adapter *PersistenceAdapter) GetGrant(ctx context.Context, tenantID, grantID string) (Grant, error) {
	record, err := adapter.repository.GetGrant(ctx, tenantID, grantID)
	if err != nil {
		return Grant{}, translateStorageError(err)
	}
	return domainGrant(record)
}

func (adapter *PersistenceAdapter) FindByOpaqueDigest(ctx context.Context, digest string) (Grant, error) {
	record, err := adapter.repository.GetGrantByTokenDigest(ctx, digest)
	if err != nil {
		return Grant{}, translateStorageError(err)
	}
	return domainGrant(record)
}

func (adapter *PersistenceAdapter) FindByIdempotency(ctx context.Context, tenantID, keyDigest string) (Grant, error) {
	record, err := adapter.repository.GetGrantByIdempotencyDigest(ctx, tenantID, keyDigest)
	if err != nil {
		return Grant{}, translateStorageError(err)
	}
	return domainGrant(record)
}

func (adapter *PersistenceAdapter) Consume(ctx context.Context, tenantID, grantID string, now time.Time) (Grant, error) {
	record, err := adapter.repository.GetGrant(ctx, tenantID, grantID)
	if err != nil {
		return Grant{}, translateStorageError(err)
	}
	record, err = adapter.repository.ConsumeGrant(ctx, tenantID, grantID, record.Revision, now.UTC().UnixNano())
	if err != nil {
		return Grant{}, translateStorageError(err)
	}
	return domainGrant(record)
}

func (adapter *PersistenceAdapter) Revoke(ctx context.Context, tenantID, grantID string, now time.Time) error {
	record, err := adapter.repository.GetGrant(ctx, tenantID, grantID)
	if err != nil {
		return translateStorageError(err)
	}
	_, err = adapter.repository.RevokeGrant(ctx, tenantID, grantID, record.Revision, now.UTC().UnixNano())
	return translateStorageError(err)
}

func assetRecord(asset Asset) AssetRecord {
	created := asset.CreatedAt.UTC().UnixNano()
	identityDigest := bindingDigest(asset.Binding)
	return AssetRecord{
		TenantID: asset.Binding.TenantID, PrincipalID: asset.Binding.PrincipalID, CredentialID: asset.Binding.CredentialID,
		RunID: asset.Binding.RunID, AssetID: asset.Binding.AssetID, Name: asset.Binding.Name,
		MediaType: asset.Binding.MediaType, SizeBytes: asset.Binding.SizeBytes, ContentDigest: asset.Binding.Digest,
		ContentBytes: append([]byte(nil), asset.Content...),
		ObjectKey:    "assets/" + asset.Binding.TenantID + "/" + asset.Binding.AssetID, Status: statusForAsset(asset.State),
		Revision: 1, CreatedAtNs: created, UpdatedAtNs: assetUpdateTime(asset),
		IdempotencyKeyDigest: identityDigest, IdempotencyRequestDigest: identityDigest,
	}
}

func domainAsset(record AssetRecord) (Asset, error) {
	state := StateQuarantine
	if record.Status == AssetAvailable {
		state = StateReady
	} else if record.Status != AssetPending {
		return Asset{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	asset := Asset{Binding: Binding{
		TenantID: record.TenantID, PrincipalID: record.PrincipalID, CredentialID: record.CredentialID,
		RunID: record.RunID, AssetID: record.AssetID, Operation: OperationUpload, Name: record.Name,
		MediaType: record.MediaType, SizeBytes: record.SizeBytes, Digest: record.ContentDigest,
	}, State: state, Content: append([]byte(nil), record.ContentBytes...), CreatedAt: time.Unix(0, record.CreatedAtNs).UTC()}
	if state == StateReady {
		asset.ReadyAt = time.Unix(0, record.UpdatedAtNs).UTC()
	}
	if err := asset.Validate(); err != nil {
		return Asset{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return asset, nil
}

func grantRecord(grant Grant) GrantRecord {
	status := GrantActive
	if grant.Revoked {
		status = GrantRevoked
	} else if grant.Uses == grant.MaxUses {
		status = GrantConsumed
	}
	created := grant.NotBefore.UTC().UnixNano()
	return GrantRecord{
		TenantID: grant.Binding.TenantID, PrincipalID: grant.Binding.PrincipalID, CredentialID: grant.Binding.CredentialID,
		GrantID: grant.GrantID, AssetID: grant.Binding.AssetID, RunID: grant.Binding.RunID,
		Operation: storageOperation(grant.Binding.Operation), Name: grant.Binding.Name, MediaType: grant.Binding.MediaType,
		SizeBytes: grant.Binding.SizeBytes, ContentDigest: grant.Binding.Digest, Audience: grant.Audience,
		TokenKeyID: grant.TokenKeyID, TokenDigest: grant.OpaqueDigest, UseLimit: int64(grant.MaxUses), UseCount: int64(grant.Uses),
		NotBeforeNs: grant.NotBefore.UTC().UnixNano(), ExpiresAtNs: grant.ExpiresAt.UTC().UnixNano(), Status: status,
		Revision: 1, CreatedAtNs: created, UpdatedAtNs: created,
		IdempotencyKeyDigest: grant.IdempotencyKeyDigest, IdempotencyRequestDigest: grantRequestDigest(grant),
	}
}

func domainGrant(record GrantRecord) (Grant, error) {
	grant := Grant{
		GrantID: record.GrantID, Binding: Binding{
			TenantID: record.TenantID, PrincipalID: record.PrincipalID, CredentialID: record.CredentialID,
			RunID: record.RunID, AssetID: record.AssetID, Operation: domainOperation(record.Operation), Name: record.Name,
			MediaType: record.MediaType, SizeBytes: record.SizeBytes, Digest: record.ContentDigest,
		}, NotBefore: time.Unix(0, record.NotBeforeNs).UTC(), ExpiresAt: time.Unix(0, record.ExpiresAtNs).UTC(),
		MaxUses: int(record.UseLimit), Uses: int(record.UseCount), Revoked: record.Status == GrantRevoked,
		Audience: record.Audience, TokenKeyID: record.TokenKeyID,
		IdempotencyKeyDigest: record.IdempotencyKeyDigest, OpaqueDigest: record.TokenDigest,
	}
	if err := grant.Validate(); err != nil {
		return Grant{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return grant, nil
}

func statusForAsset(state State) AssetStatus {
	if state == StateReady {
		return AssetAvailable
	}
	return AssetPending
}

func assetUpdateTime(asset Asset) int64 {
	if asset.State == StateReady {
		return asset.ReadyAt.UTC().UnixNano()
	}
	return asset.CreatedAt.UTC().UnixNano()
}

func storageOperation(operation Operation) StorageOperation {
	if operation == OperationDownload {
		return StorageOperationDownload
	}
	return StorageOperationUpload
}

func domainOperation(operation StorageOperation) Operation {
	if operation == StorageOperationDownload {
		return OperationDownload
	}
	return OperationUpload
}

func assetRecordIdentity(record AssetRecord, binding Binding) bool {
	return record.TenantID == binding.TenantID && record.PrincipalID == binding.PrincipalID &&
		record.CredentialID == binding.CredentialID && record.RunID == binding.RunID && record.AssetID == binding.AssetID &&
		record.Name == binding.Name && record.MediaType == binding.MediaType && record.SizeBytes == binding.SizeBytes && record.ContentDigest == binding.Digest
}

func bindingDigest(binding Binding) string {
	digest := sha256.New()
	writeBindingDigest(digest, binding)
	return hex.EncodeToString(digest.Sum(nil))
}

func grantRequestDigest(grant Grant) string {
	digest := sha256.New()
	writeBindingDigest(digest, grant.Binding)
	writeDigestField(digest, grant.GrantID)
	writeDigestField(digest, grant.Audience)
	writeDigestField(digest, grant.TokenKeyID)
	writeDigestInt64(digest, grant.NotBefore.UnixNano())
	writeDigestInt64(digest, grant.ExpiresAt.UnixNano())
	writeDigestInt64(digest, int64(grant.MaxUses))
	return hex.EncodeToString(digest.Sum(nil))
}

func writeBindingDigest(digest hash.Hash, binding Binding) {
	for _, value := range []string{binding.TenantID, binding.PrincipalID, binding.CredentialID, binding.RunID, binding.AssetID, string(binding.Operation), binding.Name, binding.MediaType, binding.Digest} {
		writeDigestField(digest, value)
	}
	writeDigestInt64(digest, binding.SizeBytes)
}

func writeDigestField(digest hash.Hash, value string) {
	writeDigestInt64(digest, int64(len(value)))
	_, _ = digest.Write([]byte(value))
}

func writeDigestInt64(digest hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = digest.Write(encoded[:])
}

func translateStorageError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case HasStorageReason(err, StorageReasonNotFound):
		return NewError(CategoryNotFound, ReasonNotFound)
	case HasStorageReason(err, StorageReasonConflict), HasStorageReason(err, StorageReasonIdempotencyConflict):
		return NewError(CategoryConflict, ReasonIdempotencyConflict)
	case HasStorageReason(err, StorageReasonExpired):
		return NewError(CategoryAuthorization, ReasonGrantExpired)
	case HasStorageReason(err, StorageReasonNotYetValid):
		return NewError(CategoryAuthorization, ReasonGrantNotYetValid)
	case HasStorageReason(err, StorageReasonValidation):
		return NewError(CategoryValidation, ReasonInvalidRequest)
	default:
		return NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
}

var _ AssetRepository = (*PersistenceAdapter)(nil)
var _ GrantRepository = (*PersistenceAdapter)(nil)
