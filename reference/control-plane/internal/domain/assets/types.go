// Package assets is the reference Control Plane asset broker domain.
// Tokens, objects, and network decisions stay inside this package until a
// later phase adds HTTP and storage adapters.
package assets

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	MaxAssetBytes   int64 = 100 * 1024 * 1024
	MaxGrantTTL           = 15 * time.Minute
	MaxGrantUses          = 8
	MaxRedirectHops       = 3
)

var (
	tenantPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	principalPattern   = regexp.MustCompile(`^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	credentialPattern  = regexp.MustCompile(`^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	runPattern         = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	assetPattern       = regexp.MustCompile(`^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	grantPattern       = regexp.MustCompile(`^agnt_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	mediaPattern       = regexp.MustCompile(`^[a-z0-9!#$&^_.+-]+/[a-z0-9!#$&^_.+-]+$`)
	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,200}$`)
)

type Operation string

const (
	OperationUpload   Operation = "asset.upload"
	OperationDownload Operation = "asset.download"
)

func (operation Operation) valid() bool {
	return operation == OperationUpload || operation == OperationDownload
}

type State string

const (
	StateQuarantine State = "quarantine"
	StateReady      State = "ready"
)

func (state State) valid() bool {
	return state == StateQuarantine || state == StateReady
}

// Caller is an already authenticated control-plane identity. Asset
// authorization never infers identity from the asset body or the grant.
type Caller struct {
	TenantID     string
	PrincipalID  string
	CredentialID string
}

func (caller Caller) Validate() error {
	if !tenantPattern.MatchString(caller.TenantID) || !principalPattern.MatchString(caller.PrincipalID) || !credentialPattern.MatchString(caller.CredentialID) {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	return nil
}

// Binding is the mandatory scope of every grant and stored object.
type Binding struct {
	TenantID     string
	PrincipalID  string
	CredentialID string
	RunID        string
	AssetID      string
	Operation    Operation
	MediaType    string
	SizeBytes    int64
	Digest       string
}

func (binding Binding) Validate() error {
	if !tenantPattern.MatchString(binding.TenantID) || !principalPattern.MatchString(binding.PrincipalID) || !credentialPattern.MatchString(binding.CredentialID) {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	if !runPattern.MatchString(binding.RunID) || !assetPattern.MatchString(binding.AssetID) || !binding.Operation.valid() {
		return NewError(CategoryValidation, ReasonBindingMismatch)
	}
	if !mediaPattern.MatchString(binding.MediaType) || binding.SizeBytes < 0 || binding.SizeBytes > MaxAssetBytes || !digestPattern.MatchString(binding.Digest) {
		if binding.SizeBytes > MaxAssetBytes {
			return NewError(CategoryCapacity, ReasonAssetTooLarge)
		}
		return NewError(CategoryValidation, ReasonBindingMismatch)
	}
	return nil
}

func (binding Binding) SameIdentity(other Binding) bool {
	return binding.TenantID == other.TenantID &&
		binding.PrincipalID == other.PrincipalID &&
		binding.CredentialID == other.CredentialID &&
		binding.RunID == other.RunID &&
		binding.AssetID == other.AssetID &&
		binding.Operation == other.Operation &&
		binding.MediaType == other.MediaType &&
		binding.SizeBytes == other.SizeBytes &&
		binding.Digest == other.Digest
}

type Asset struct {
	Binding   Binding
	State     State
	CreatedAt time.Time
	ReadyAt   time.Time
}

func (asset Asset) Validate() error {
	if err := asset.Binding.Validate(); err != nil {
		return err
	}
	if !asset.State.valid() || asset.CreatedAt.IsZero() || asset.CreatedAt.Location() != time.UTC {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if asset.State == StateReady && (asset.ReadyAt.IsZero() || asset.ReadyAt.Before(asset.CreatedAt)) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if asset.State == StateQuarantine && !asset.ReadyAt.IsZero() {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

// Grant is the durable record. Token is never stored; only OpaqueDigest is.
type Grant struct {
	GrantID              string
	Binding              Binding
	NotBefore            time.Time
	ExpiresAt            time.Time
	MaxUses              int
	Uses                 int
	Revoked              bool
	IdempotencyKeyDigest string
	OpaqueDigest         string
}

func (grant Grant) Validate() error {
	if err := grant.Binding.Validate(); err != nil {
		return err
	}
	if !grantPattern.MatchString(grant.GrantID) || len(grant.OpaqueDigest) != 64 || strings.Trim(grant.OpaqueDigest, "0123456789abcdef") != "" {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if grant.NotBefore.IsZero() || grant.ExpiresAt.IsZero() || grant.NotBefore.Location() != time.UTC || grant.ExpiresAt.Location() != time.UTC {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if !grant.ExpiresAt.After(grant.NotBefore) || grant.ExpiresAt.Sub(grant.NotBefore) > MaxGrantTTL {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if grant.MaxUses < 1 || grant.MaxUses > MaxGrantUses || grant.Uses < 0 || grant.Uses > grant.MaxUses {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if len(grant.IdempotencyKeyDigest) != 64 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func (grant Grant) UsableAt(now time.Time) error {
	if grant.Revoked {
		return NewError(CategoryAuthorization, ReasonGrantRevoked)
	}
	if now.Before(grant.NotBefore) {
		return NewError(CategoryAuthorization, ReasonGrantNotYetValid)
	}
	if !now.Before(grant.ExpiresAt) {
		return NewError(CategoryAuthorization, ReasonGrantExpired)
	}
	if grant.Uses >= grant.MaxUses {
		return NewError(CategoryAuthorization, ReasonGrantUsesExhausted)
	}
	return nil
}

type IssueRequest struct {
	Caller         Caller
	RunID          string
	AssetID        string
	Operation      Operation
	MediaType      string
	SizeBytes      int64
	Digest         string
	IdempotencyKey string
	NotBefore      time.Time
	ExpiresAt      time.Time
	MaxUses        int
}

func (request IssueRequest) Binding() Binding {
	return Binding{
		TenantID:     request.Caller.TenantID,
		PrincipalID:  request.Caller.PrincipalID,
		CredentialID: request.Caller.CredentialID,
		RunID:        request.RunID,
		AssetID:      request.AssetID,
		Operation:    request.Operation,
		MediaType:    request.MediaType,
		SizeBytes:    request.SizeBytes,
		Digest:       request.Digest,
	}
}

func (request IssueRequest) Validate() error {
	if err := request.Caller.Validate(); err != nil {
		return err
	}
	if !idempotencyPattern.MatchString(request.IdempotencyKey) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if request.MaxUses < 1 || request.MaxUses > MaxGrantUses {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return request.Binding().Validate()
}

type IssuedGrant struct {
	Grant Grant
	// Token is the one-time opaque secret returned to the caller. Empty on
	// idempotent replay; the caller must retain the original token.
	Token  string
	Replay bool
}

type UploadReceipt struct {
	Caller         Caller
	GrantToken     string
	MediaType      string
	SizeBytes      int64
	Digest         string
	IdempotencyKey string
}

type PromoteRequest struct {
	Caller  Caller
	AssetID string
	Digest  string
}

type RevokeRequest struct {
	Caller  Caller
	GrantID string
}

type ConnectRequest struct {
	Caller     Caller
	GrantToken string
	URL        string
}

func sameCaller(caller Caller, binding Binding) bool {
	return caller.TenantID == binding.TenantID && caller.PrincipalID == binding.PrincipalID && caller.CredentialID == binding.CredentialID
}

func downloadMatches(stored, requested Binding) bool {
	return stored.TenantID == requested.TenantID &&
		stored.PrincipalID == requested.PrincipalID &&
		stored.CredentialID == requested.CredentialID &&
		stored.RunID == requested.RunID &&
		stored.AssetID == requested.AssetID &&
		stored.MediaType == requested.MediaType &&
		stored.SizeBytes == requested.SizeBytes &&
		stored.Digest == requested.Digest &&
		requested.Operation == OperationDownload
}

func validateDigest(digest string) error {
	if !digestPattern.MatchString(digest) {
		return errors.New("digest")
	}
	return nil
}
