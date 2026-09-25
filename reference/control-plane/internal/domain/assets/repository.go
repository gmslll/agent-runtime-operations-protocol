// Package assets holds repository-local persistence rows for the asset broker.
// Protocol and HTTP types are not available yet; later integration adapts these
// rows at the service boundary.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

type AssetStatus string

const (
	AssetPending   AssetStatus = "pending"
	AssetAvailable AssetStatus = "available"
	AssetIsolated  AssetStatus = "isolated"
)

type GrantStatus string

const (
	GrantActive   GrantStatus = "active"
	GrantConsumed GrantStatus = "consumed"
	GrantRevoked  GrantStatus = "revoked"
)

type StorageOperation string

const (
	StorageOperationUpload   StorageOperation = "upload"
	StorageOperationDownload StorageOperation = "download"
)

type StorageReason string

const (
	StorageReasonValidation          StorageReason = "validation"
	StorageReasonUnavailable         StorageReason = "unavailable"
	StorageReasonNotFound            StorageReason = "not_found"
	StorageReasonConflict            StorageReason = "conflict"
	StorageReasonIdempotencyConflict StorageReason = "idempotency_conflict"
	StorageReasonNotYetValid         StorageReason = "not_yet_valid"
	StorageReasonExpired             StorageReason = "expired"
)

type StorageError struct{ Reason StorageReason }

func (err StorageError) Error() string { return "asset repository: " + string(err.Reason) }

func NewStorageError(reason StorageReason) error { return StorageError{Reason: reason} }

func HasStorageReason(err error, reason StorageReason) bool {
	var failure StorageError
	return errors.As(err, &failure) && failure.Reason == reason
}

// Asset is one tenant-owned object descriptor. The body stays outside the row.
type AssetRecord struct {
	TenantID                 string
	PrincipalID              string
	CredentialID             string
	RunID                    string
	AssetID                  string
	Name                     string
	MediaType                string
	SizeBytes                int64
	ContentDigest            string
	ContentBytes             []byte
	ObjectKey                string
	Status                   AssetStatus
	Revision                 int64
	CreatedAtNs              int64
	UpdatedAtNs              int64
	IdempotencyKeyDigest     string
	IdempotencyRequestDigest string
}

// AssetUpdate is the CAS payload. Identity and idempotency columns stay fixed.
type AssetUpdate struct {
	Name          string
	MediaType     string
	SizeBytes     int64
	ContentDigest string
	ContentBytes  []byte
	ObjectKey     string
	Status        AssetStatus
	UpdatedAtNs   int64
}

// Grant authorizes one operation. run_id is stored without a run-table foreign key.
type GrantRecord struct {
	TenantID                 string
	PrincipalID              string
	CredentialID             string
	GrantID                  string
	AssetID                  string
	RunID                    string
	Operation                StorageOperation
	Name                     string
	MediaType                string
	SizeBytes                int64
	ContentDigest            string
	Audience                 string
	TokenKeyID               string
	TokenDigest              string
	UseLimit                 int64
	UseCount                 int64
	NotBeforeNs              int64
	ExpiresAtNs              int64
	Status                   GrantStatus
	Revision                 int64
	CreatedAtNs              int64
	UpdatedAtNs              int64
	IdempotencyKeyDigest     string
	IdempotencyRequestDigest string
}

// Repository is the storage port both engines implement.
type StorageRepository interface {
	CreateAsset(ctx context.Context, asset AssetRecord) (AssetRecord, error)
	GetAsset(ctx context.Context, tenantID, assetID string) (AssetRecord, error)
	UpdateAsset(ctx context.Context, tenantID, assetID string, expectedRevision int64, update AssetUpdate) (AssetRecord, error)
	CreateGrant(ctx context.Context, grant GrantRecord) (GrantRecord, error)
	GetGrant(ctx context.Context, tenantID, grantID string) (GrantRecord, error)
	GetGrantByTokenDigest(ctx context.Context, tokenDigest string) (GrantRecord, error)
	GetGrantByIdempotencyDigest(ctx context.Context, tenantID, keyDigest string) (GrantRecord, error)
	ConsumeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (GrantRecord, error)
	RevokeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (GrantRecord, error)
}

var (
	storageTenantPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	storageAssetPattern      = regexp.MustCompile(`^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	storageGrantPattern      = regexp.MustCompile(`^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	storageRunPattern        = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	storagePrincipalPattern  = regexp.MustCompile(`^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	storageCredentialPattern = regexp.MustCompile(`^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	storageAudiencePattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._:-][a-z0-9]+)*$`)
	storageTokenKeyPattern   = regexp.MustCompile(`^atk_[A-Za-z0-9._-]{1,60}$`)
	storageDigestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	storageHexPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	storageMediaPattern      = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
	storageKeyPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,255}$`)
)

func (asset AssetRecord) Validate() error {
	if !storageTenantPattern.MatchString(asset.TenantID) || len(asset.TenantID) > 128 ||
		!storagePrincipalPattern.MatchString(asset.PrincipalID) || !storageCredentialPattern.MatchString(asset.CredentialID) || !storageRunPattern.MatchString(asset.RunID) ||
		!storageAssetPattern.MatchString(asset.AssetID) || !validName(asset.Name) ||
		!storageMediaPattern.MatchString(asset.MediaType) || len(asset.MediaType) > 127 ||
		asset.SizeBytes < 0 || asset.SizeBytes > 104857600 || !storageDigestPattern.MatchString(asset.ContentDigest) ||
		!validObjectKey(asset.ObjectKey) || !validAssetStatus(asset.Status) || asset.Revision < 1 ||
		asset.CreatedAtNs < 1 || asset.UpdatedAtNs < asset.CreatedAtNs ||
		!storageHexPattern.MatchString(asset.IdempotencyKeyDigest) || !storageHexPattern.MatchString(asset.IdempotencyRequestDigest) {
		return NewStorageError(StorageReasonValidation)
	}
	if !validStoredContent(asset.Status, asset.ContentBytes, asset.SizeBytes, asset.ContentDigest) {
		return NewStorageError(StorageReasonValidation)
	}
	return nil
}

func (update AssetUpdate) Validate(createdAtNs int64) error {
	if !validName(update.Name) || !storageMediaPattern.MatchString(update.MediaType) || len(update.MediaType) > 127 ||
		update.SizeBytes < 0 || update.SizeBytes > 104857600 || !storageDigestPattern.MatchString(update.ContentDigest) ||
		!validObjectKey(update.ObjectKey) || !validAssetStatus(update.Status) || update.UpdatedAtNs < createdAtNs {
		return NewStorageError(StorageReasonValidation)
	}
	if !validStoredContent(update.Status, update.ContentBytes, update.SizeBytes, update.ContentDigest) {
		return NewStorageError(StorageReasonValidation)
	}
	return nil
}

func (grant GrantRecord) Validate() error {
	if !storageTenantPattern.MatchString(grant.TenantID) || len(grant.TenantID) > 128 ||
		!storagePrincipalPattern.MatchString(grant.PrincipalID) || !storageCredentialPattern.MatchString(grant.CredentialID) ||
		!storageGrantPattern.MatchString(grant.GrantID) || !storageAssetPattern.MatchString(grant.AssetID) ||
		!storageRunPattern.MatchString(grant.RunID) || (grant.Operation != StorageOperationUpload && grant.Operation != StorageOperationDownload) ||
		!validName(grant.Name) || !storageMediaPattern.MatchString(grant.MediaType) || len(grant.MediaType) > 127 || grant.SizeBytes < 0 || grant.SizeBytes > 104857600 || !storageDigestPattern.MatchString(grant.ContentDigest) ||
		!storageAudiencePattern.MatchString(grant.Audience) || len(grant.Audience) > 100 || !storageTokenKeyPattern.MatchString(grant.TokenKeyID) ||
		!storageHexPattern.MatchString(grant.TokenDigest) || grant.UseLimit < 1 || grant.UseLimit > 8 ||
		grant.UseCount < 0 || grant.UseCount > grant.UseLimit || grant.ExpiresAtNs < 1 ||
		grant.NotBeforeNs < 1 || grant.ExpiresAtNs <= grant.NotBeforeNs || grant.ExpiresAtNs-grant.NotBeforeNs > int64(15*time.Minute) ||
		!validGrantStatus(grant.Status, grant.UseCount, grant.UseLimit) || grant.Revision < 1 ||
		grant.CreatedAtNs < 1 || grant.UpdatedAtNs < grant.CreatedAtNs ||
		!storageHexPattern.MatchString(grant.IdempotencyKeyDigest) || !storageHexPattern.MatchString(grant.IdempotencyRequestDigest) {
		return NewStorageError(StorageReasonValidation)
	}
	return nil
}

func validName(name string) bool {
	return len(name) >= 1 && len(name) <= 512 && !containsDotDot(name)
}

func validObjectKey(key string) bool {
	return storageKeyPattern.MatchString(key) && !containsDotDot(key)
}

func containsDotDot(value string) bool { return strings.Contains(value, "..") }

func validAssetStatus(status AssetStatus) bool {
	return status == AssetPending || status == AssetAvailable || status == AssetIsolated
}

func validStoredContent(status AssetStatus, content []byte, size int64, digest string) bool {
	if status == AssetPending {
		return len(content) == 0
	}
	if status == AssetIsolated {
		return len(content) == 0
	}
	if status != AssetAvailable || int64(len(content)) != size {
		return false
	}
	sum := sha256.Sum256(content)
	return digest == "sha256:"+hex.EncodeToString(sum[:])
}

func validGrantStatus(status GrantStatus, useCount, useLimit int64) bool {
	switch status {
	case GrantActive:
		return useCount < useLimit
	case GrantConsumed:
		return useCount == useLimit
	case GrantRevoked:
		return true
	default:
		return false
	}
}
