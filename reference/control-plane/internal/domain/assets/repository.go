// Package assets holds repository-local persistence rows for the asset broker.
// Protocol and HTTP types are not available yet; later integration adapts these
// rows at the service boundary.
package assets

import (
	"context"
	"errors"
	"regexp"
	"strings"
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

type Operation string

const (
	OperationUpload   Operation = "upload"
	OperationDownload Operation = "download"
)

type Reason string

const (
	ReasonValidation          Reason = "validation"
	ReasonUnavailable         Reason = "unavailable"
	ReasonNotFound            Reason = "not_found"
	ReasonConflict            Reason = "conflict"
	ReasonIdempotencyConflict Reason = "idempotency_conflict"
	ReasonExpired             Reason = "expired"
)

type Error struct{ Reason Reason }

func (err Error) Error() string { return "asset repository: " + string(err.Reason) }

func NewError(reason Reason) error { return Error{Reason: reason} }

func HasReason(err error, reason Reason) bool {
	var failure Error
	return errors.As(err, &failure) && failure.Reason == reason
}

// Asset is one tenant-owned object descriptor. The body stays outside the row.
type Asset struct {
	TenantID                 string
	AssetID                  string
	Name                     string
	MediaType                string
	SizeBytes                int64
	ContentDigest            string
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
	ObjectKey     string
	Status        AssetStatus
	UpdatedAtNs   int64
}

// Grant authorizes one operation. run_id is stored without a run-table foreign key.
type Grant struct {
	TenantID                 string
	GrantID                  string
	AssetID                  string
	RunID                    string
	Operation                Operation
	TokenDigest              string
	UseLimit                 int64
	UseCount                 int64
	ExpiresAtNs              int64
	Status                   GrantStatus
	Revision                 int64
	CreatedAtNs              int64
	UpdatedAtNs              int64
	IdempotencyKeyDigest     string
	IdempotencyRequestDigest string
}

// Repository is the storage port both engines implement.
type Repository interface {
	CreateAsset(ctx context.Context, asset Asset) (Asset, error)
	GetAsset(ctx context.Context, tenantID, assetID string) (Asset, error)
	UpdateAsset(ctx context.Context, tenantID, assetID string, expectedRevision int64, update AssetUpdate) (Asset, error)
	CreateGrant(ctx context.Context, grant Grant) (Grant, error)
	GetGrant(ctx context.Context, tenantID, grantID string) (Grant, error)
	ConsumeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (Grant, error)
	RevokeGrant(ctx context.Context, tenantID, grantID string, expectedRevision, nowNs int64) (Grant, error)
}

var (
	tenantPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	assetPattern  = regexp.MustCompile(`^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	grantPattern  = regexp.MustCompile(`^grnt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	runPattern    = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mediaPattern  = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
	keyPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,255}$`)
)

func (asset Asset) Validate() error {
	if !tenantPattern.MatchString(asset.TenantID) || len(asset.TenantID) > 128 ||
		!assetPattern.MatchString(asset.AssetID) || !validName(asset.Name) ||
		!mediaPattern.MatchString(asset.MediaType) || len(asset.MediaType) > 127 ||
		asset.SizeBytes < 0 || asset.SizeBytes > 104857600 || !digestPattern.MatchString(asset.ContentDigest) ||
		!validObjectKey(asset.ObjectKey) || !validAssetStatus(asset.Status) || asset.Revision < 1 ||
		asset.CreatedAtNs < 1 || asset.UpdatedAtNs < asset.CreatedAtNs ||
		!hexPattern.MatchString(asset.IdempotencyKeyDigest) || !hexPattern.MatchString(asset.IdempotencyRequestDigest) {
		return NewError(ReasonValidation)
	}
	return nil
}

func (update AssetUpdate) Validate(createdAtNs int64) error {
	if !validName(update.Name) || !mediaPattern.MatchString(update.MediaType) || len(update.MediaType) > 127 ||
		update.SizeBytes < 0 || update.SizeBytes > 104857600 || !digestPattern.MatchString(update.ContentDigest) ||
		!validObjectKey(update.ObjectKey) || !validAssetStatus(update.Status) || update.UpdatedAtNs < createdAtNs {
		return NewError(ReasonValidation)
	}
	return nil
}

func (grant Grant) Validate() error {
	if !tenantPattern.MatchString(grant.TenantID) || len(grant.TenantID) > 128 ||
		!grantPattern.MatchString(grant.GrantID) || !assetPattern.MatchString(grant.AssetID) ||
		!runPattern.MatchString(grant.RunID) || (grant.Operation != OperationUpload && grant.Operation != OperationDownload) ||
		!hexPattern.MatchString(grant.TokenDigest) || grant.UseLimit < 1 || grant.UseLimit > 1000 ||
		grant.UseCount < 0 || grant.UseCount > grant.UseLimit || grant.ExpiresAtNs < 1 ||
		!validGrantStatus(grant.Status, grant.UseCount, grant.UseLimit) || grant.Revision < 1 ||
		grant.CreatedAtNs < 1 || grant.UpdatedAtNs < grant.CreatedAtNs ||
		!hexPattern.MatchString(grant.IdempotencyKeyDigest) || !hexPattern.MatchString(grant.IdempotencyRequestDigest) {
		return NewError(ReasonValidation)
	}
	return nil
}

func validName(name string) bool {
	return len(name) >= 1 && len(name) <= 512 && !containsDotDot(name)
}

func validObjectKey(key string) bool {
	return keyPattern.MatchString(key) && !containsDotDot(key)
}

func containsDotDot(value string) bool { return strings.Contains(value, "..") }

func validAssetStatus(status AssetStatus) bool {
	return status == AssetPending || status == AssetAvailable || status == AssetIsolated
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
