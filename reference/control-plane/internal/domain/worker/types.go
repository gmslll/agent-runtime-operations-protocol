package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

const MaxSafeInteger uint64 = 9007199254740991

var (
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	scopePattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
	tokenPattern  = regexp.MustCompile(`^wlt_[A-Za-z0-9_-]{43}$`)
)

type Operation string

const (
	OperationClaim    Operation = "worker.claim"
	OperationRenew    Operation = "worker.renew"
	OperationComplete Operation = "worker.complete"
	OperationRelease  Operation = "worker.release"
)

type Caller struct {
	TenantID, PrincipalID, CredentialID string
	Scopes                              []string
}

func (caller Caller) Validate() error {
	if !validSlug(caller.TenantID) || !prefixedUUID("prn_", caller.PrincipalID) || !prefixedUUID("cred_", caller.CredentialID) || len(caller.Scopes) == 0 || len(caller.Scopes) > 128 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	seen := map[string]bool{}
	for _, scope := range caller.Scopes {
		if len(scope) > 128 || !scopePattern.MatchString(scope) || seen[scope] {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		seen[scope] = true
	}
	return nil
}

type Binding struct {
	AgentID, Version, SkillID, ManifestDigest string
}

func (binding Binding) Validate() error {
	if !validSlug(binding.AgentID) || binding.Version == "" || len(binding.Version) > 128 || !validSlug(binding.SkillID) || !digestPattern.MatchString(binding.ManifestDigest) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func (binding Binding) key() string {
	return binding.AgentID + "\x00" + binding.Version + "\x00" + binding.SkillID + "\x00" + binding.ManifestDigest
}

type ClaimRequest struct {
	Caller            Caller
	WorkerID          string
	SessionID         string
	Generation        uint64
	AvailableSlots    uint64
	SupportedBindings []Binding
	WaitSeconds       uint64
	LeaseSeconds      uint64
	Metadata          platform.RequestMetadata
}

func (request ClaimRequest) Validate() error {
	if request.Caller.Validate() != nil || !validSlug(request.WorkerID) || !prefixedUUID("ses_", request.SessionID) || !positiveSafe(request.Generation) || !positiveSafe(request.AvailableSlots) || request.WaitSeconds > 30 || request.LeaseSeconds < 15 || request.LeaseSeconds > 300 || len(request.SupportedBindings) == 0 || len(request.SupportedBindings) > 256 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	seen := map[string]bool{}
	for _, binding := range request.SupportedBindings {
		if binding.Validate() != nil || seen[binding.key()] {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		seen[binding.key()] = true
	}
	return nil
}

type Claim struct {
	TenantID, ClaimID, WorkerID, SessionID, RunID, AttemptID string
	Generation, AttemptNumber, FencingToken                  uint64
	LeaseToken, LeaseTokenDigest                             string
	LeaseExpiresAt, ClaimedAt                                time.Time
	RunRequest                                               json.RawMessage
}

func (claim Claim) Validate() error {
	if !validSlug(claim.TenantID) || !prefixedUUID("clm_", claim.ClaimID) || !validSlug(claim.WorkerID) || !prefixedUUID("ses_", claim.SessionID) || !positiveSafe(claim.Generation) || !prefixedUUID("run_", claim.RunID) || !prefixedUUID("att_", claim.AttemptID) || !positiveSafe(claim.AttemptNumber) || !positiveSafe(claim.FencingToken) || !tokenPattern.MatchString(claim.LeaseToken) || !digestPattern.MatchString(claim.LeaseTokenDigest) || !utc(claim.ClaimedAt) || !utc(claim.LeaseExpiresAt) || !claim.LeaseExpiresAt.After(claim.ClaimedAt) || claim.LeaseExpiresAt.Sub(claim.ClaimedAt) > 5*time.Minute {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if _, err := workerwire.DecodeWorkerClaim(mustClaimWire(claim)); err != nil {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func mustClaimWire(claim Claim) []byte {
	value := map[string]any{"schema_version": 1, "claim_id": claim.ClaimID, "worker_id": claim.WorkerID, "session_id": claim.SessionID, "generation": claim.Generation, "run_id": claim.RunID, "attempt_id": claim.AttemptID, "attempt_number": claim.AttemptNumber, "fencing_token": claim.FencingToken, "lease_token": claim.LeaseToken, "lease_expires_at": claim.LeaseExpiresAt.Format(time.RFC3339Nano), "run_request": json.RawMessage(claim.RunRequest), "claimed_at": claim.ClaimedAt.Format(time.RFC3339Nano)}
	encoded, _ := json.Marshal(value)
	return encoded
}

func (claim Claim) Wire() ([]byte, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	return mustClaimWire(claim), nil
}

type RenewRequest struct {
	Caller                     Caller
	WorkerID, ClaimID          string
	LeaseToken                 string
	FencingToken, LeaseSeconds uint64
	Metadata                   platform.RequestMetadata
}

func (request RenewRequest) Validate() error {
	if request.Caller.Validate() != nil || !validSlug(request.WorkerID) || !prefixedUUID("clm_", request.ClaimID) || !tokenPattern.MatchString(request.LeaseToken) || !positiveSafe(request.FencingToken) || request.LeaseSeconds < 15 || request.LeaseSeconds > 300 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type CompleteRequest struct {
	Caller                                     Caller
	WorkerID, ClaimID, CompletionID, AttemptID string
	LeaseToken, IdempotencyKey                 string
	FencingToken                               uint64
	Result                                     json.RawMessage
	EffectIDs                                  []string
	CompletedAt                                time.Time
	Metadata                                   platform.RequestMetadata
}

func (request CompleteRequest) Validate() error {
	if request.Caller.Validate() != nil || !validSlug(request.WorkerID) || !prefixedUUID("clm_", request.ClaimID) || !prefixedUUID("cmp_", request.CompletionID) || !prefixedUUID("att_", request.AttemptID) || !tokenPattern.MatchString(request.LeaseToken) || !positiveSafe(request.FencingToken) || !validIdempotencyKey(request.IdempotencyKey) || !utc(request.CompletedAt) || len(request.Result) == 0 || len(request.Result) > 1<<20 || len(request.EffectIDs) > 128 {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	wire := map[string]any{"schema_version": 1, "completion_id": request.CompletionID, "claim_id": request.ClaimID, "attempt_id": request.AttemptID, "fencing_token": request.FencingToken, "lease_token": request.LeaseToken, "result": json.RawMessage(request.Result), "completed_at": request.CompletedAt.Format(time.RFC3339Nano)}
	if len(request.EffectIDs) != 0 {
		wire["effect_ids"] = request.EffectIDs
	}
	encoded, _ := json.Marshal(wire)
	if _, err := workerwire.DecodeWorkerComplete(encoded); err != nil {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type ReleaseRequest struct {
	Caller                        Caller
	WorkerID, ClaimID, LeaseToken string
	FencingToken                  uint64
	Reason                        string
	Metadata                      platform.RequestMetadata
}

func (request ReleaseRequest) Validate() error {
	if request.Caller.Validate() != nil || !validSlug(request.WorkerID) || !prefixedUUID("clm_", request.ClaimID) || !tokenPattern.MatchString(request.LeaseToken) || !positiveSafe(request.FencingToken) || request.Reason != "worker_draining" && request.Reason != "worker_shutdown" && request.Reason != "retryable_failure" {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func TokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func KeyDigest(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func RequestDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func SortedBindings(bindings []Binding) []Binding {
	result := slices.Clone(bindings)
	slices.SortFunc(result, func(a, b Binding) int { return strings.Compare(a.key(), b.key()) })
	return result
}

func validIdempotencyKey(value string) bool {
	return len(value) >= 16 && len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
func validSlug(value string) bool { return len(value) <= 128 && slugPattern.MatchString(value) }
func prefixedUUID(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidV7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}
func positiveSafe(value uint64) bool { return value > 0 && value <= MaxSafeInteger }
func utc(value time.Time) bool       { return !value.IsZero() && value.Location() == time.UTC }
func validDigest(value string) bool  { return digestPattern.MatchString(value) }
func validHex(value string) bool     { return hexPattern.MatchString(value) }
