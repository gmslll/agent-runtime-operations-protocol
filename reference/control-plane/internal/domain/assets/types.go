// Package assets is the reference Control Plane asset broker domain.
// Tokens, objects, and network decisions stay inside this package until a
// later phase adds HTTP and storage adapters.
package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
)

const (
	MaxAssetBytes      int64 = 100 * 1024 * 1024
	MaxGrantTTL              = 15 * time.Minute
	MaxGrantUses             = 8
	MaxRedirectHops          = 3
	AssetTokenAudience       = "asset-broker"
)

var (
	tenantPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	principalPattern   = regexp.MustCompile(`^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	credentialPattern  = regexp.MustCompile(`^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	runPattern         = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	assetPattern       = regexp.MustCompile(`^asset_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	grantPattern       = regexp.MustCompile(`^grant_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	opaqueTokenPattern = regexp.MustCompile(`^agt_[A-Za-z0-9_-]{43}$`)
	tokenKeyPattern    = regexp.MustCompile(`^atk_[A-Za-z0-9._-]{1,60}$`)
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
	if len(caller.TenantID) > 128 || !tenantPattern.MatchString(caller.TenantID) || !principalPattern.MatchString(caller.PrincipalID) || !credentialPattern.MatchString(caller.CredentialID) {
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
	Name         string
	MediaType    string
	SizeBytes    int64
	Digest       string
}

func (binding Binding) Validate() error {
	if len(binding.TenantID) > 128 || !tenantPattern.MatchString(binding.TenantID) || !principalPattern.MatchString(binding.PrincipalID) || !credentialPattern.MatchString(binding.CredentialID) {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	if !runPattern.MatchString(binding.RunID) || !assetPattern.MatchString(binding.AssetID) || !binding.Operation.valid() {
		return NewError(CategoryValidation, ReasonBindingMismatch)
	}
	if !validAssetName(binding.Name) || !mediaPattern.MatchString(binding.MediaType) || len(binding.MediaType) > 127 || binding.SizeBytes < 0 || binding.SizeBytes > MaxAssetBytes || !digestPattern.MatchString(binding.Digest) {
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
		binding.Name == other.Name &&
		binding.MediaType == other.MediaType &&
		binding.SizeBytes == other.SizeBytes &&
		binding.Digest == other.Digest
}

type Asset struct {
	Binding   Binding
	State     State
	Content   []byte
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
	if asset.State == StateQuarantine && len(asset.Content) != 0 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if asset.State == StateReady {
		if int64(len(asset.Content)) != asset.Binding.SizeBytes {
			return NewError(CategoryValidation, ReasonDigestMismatch)
		}
		digest := sha256.Sum256(asset.Content)
		if asset.Binding.Digest != "sha256:"+hex.EncodeToString(digest[:]) {
			return NewError(CategoryValidation, ReasonDigestMismatch)
		}
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
	Audience             string
	TokenKeyID           string
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
	if grant.Audience != AssetTokenAudience || !tokenKeyPattern.MatchString(grant.TokenKeyID) {
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
	Metadata       platform.RequestMetadata
	Caller         Caller
	RunID          string
	AssetID        string
	Operation      Operation
	Name           string
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
		Name:         request.Name,
		MediaType:    request.MediaType,
		SizeBytes:    request.SizeBytes,
		Digest:       request.Digest,
	}
}

func validAssetName(name string) bool {
	return len(name) >= 1 && len(name) <= 512 && !strings.Contains(name, "..") && !strings.ContainsAny(name, "\\/\x00\r\n")
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
	if !runPattern.MatchString(request.RunID) || !request.Operation.valid() {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	switch request.Operation {
	case OperationUpload:
		if request.AssetID != "" && !assetPattern.MatchString(request.AssetID) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if !validAssetName(request.Name) || !mediaPattern.MatchString(request.MediaType) || len(request.MediaType) > 127 || request.SizeBytes < 0 || request.SizeBytes > MaxAssetBytes || validateDigest(request.Digest) != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case OperationDownload:
		if !assetPattern.MatchString(request.AssetID) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	}
	return nil
}

type IssuedGrant struct {
	Grant Grant
	// Token is the opaque secret returned to the caller. The issuer must be
	// deterministic for an identical durable Grant so idempotent replay can
	// return the exact same public response without storing plaintext.
	Token  string
	Replay bool
}

type UploadReceipt struct {
	Metadata   platform.RequestMetadata
	GrantID    string
	GrantToken string
	Content    []byte
}

type PromoteRequest struct {
	Metadata platform.RequestMetadata
	Caller   Caller
	AssetID  string
	Digest   string
	Content  []byte
}

type RevokeRequest struct {
	Metadata platform.RequestMetadata
	Caller   Caller
	GrantID  string
}

type ConnectRequest struct {
	Metadata   platform.RequestMetadata
	Caller     Caller
	GrantToken string
	URL        string
}

type DownloadReceipt struct {
	Metadata   platform.RequestMetadata
	GrantID    string
	GrantToken string
}

type DownloadResult struct {
	Content   []byte
	Name      string
	MediaType string
	Digest    string
}

func sameCaller(caller Caller, binding Binding) bool {
	return caller.TenantID == binding.TenantID && caller.PrincipalID == binding.PrincipalID && caller.CredentialID == binding.CredentialID
}

func downloadMatches(stored, requested Binding) bool {
	return stored.TenantID == requested.TenantID &&
		stored.RunID == requested.RunID &&
		stored.AssetID == requested.AssetID &&
		stored.Name == requested.Name &&
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
