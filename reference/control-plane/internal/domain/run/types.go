package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
)

const MaxSafeInteger uint64 = 9007199254740991

var (
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	effectPattern = regexp.MustCompile(`^eff_[A-Za-z0-9._:-]{4,196}$`)
	scopePattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
)

type Operation string

const (
	OperationCreate  Operation = "run.create"
	OperationRead    Operation = "run.read"
	OperationCommand Operation = "run.command"
	OperationExpire  Operation = "run.expire"
	OperationEffect  Operation = "run.effect.reserve"
)

type Caller struct {
	TenantID     string
	PrincipalID  string
	CredentialID string
	Scopes       []string
}

func (caller Caller) Validate() error {
	if !slugPattern.MatchString(caller.TenantID) || !prefixedUUID("prn_", caller.PrincipalID) || !prefixedUUID("cred_", caller.CredentialID) || len(caller.Scopes) == 0 {
		return errors.New("invalid caller")
	}
	seen := map[string]struct{}{}
	for _, scope := range caller.Scopes {
		if !scopePattern.MatchString(scope) {
			return errors.New("invalid scope")
		}
		if _, ok := seen[scope]; ok {
			return errors.New("duplicate scope")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

type AgentBinding struct{ ID, Version, SkillID, ManifestDigest string }

func (binding AgentBinding) Validate() error {
	if !slugPattern.MatchString(binding.ID) || binding.Version == "" || !slugPattern.MatchString(binding.SkillID) || !digestPattern.MatchString(binding.ManifestDigest) {
		return errors.New("invalid agent binding")
	}
	return nil
}

type EffectLevel string

const (
	EffectNone         EffectLevel = "none"
	EffectRead         EffectLevel = "read"
	EffectWrite        EffectLevel = "write"
	EffectIrreversible EffectLevel = "irreversible"
)

type EffectIntent struct {
	Level    EffectLevel
	EffectID string
}

func (intent EffectIntent) Validate() error {
	switch intent.Level {
	case EffectNone, EffectRead:
		if intent.EffectID != "" {
			return errors.New("read-only effect has identifier")
		}
	case EffectWrite, EffectIrreversible:
		if !effectPattern.MatchString(intent.EffectID) {
			return errors.New("stable effect identifier is required")
		}
	default:
		return errors.New("invalid effect level")
	}
	return nil
}

type CreateRequest struct {
	Caller          Caller
	Agent           AgentBinding
	Input           json.RawMessage
	ConversationRef string
	DeadlineAt      time.Time
	Effects         EffectIntent
	Metadata        platform.RequestMetadata
	IdempotencyKey  string
}

func (request CreateRequest) Validate(now time.Time) error {
	if request.Caller.Validate() != nil || request.Agent.Validate() != nil || len(request.Input) == 0 || !json.Valid(request.Input) || request.Effects.Validate() != nil || !validIdempotencyKey(request.IdempotencyKey) || !utc(request.DeadlineAt) || !request.DeadlineAt.After(now) || request.DeadlineAt.Sub(now) > 7*24*time.Hour {
		return errors.New("invalid run request")
	}
	if request.ConversationRef != "" && !prefixedUUID("conv_", request.ConversationRef) {
		return errors.New("invalid conversation reference")
	}
	return nil
}

type AuthorizationSnapshot struct {
	TenantID     string       `json:"tenant_id"`
	PrincipalID  string       `json:"principal_id"`
	CredentialID string       `json:"credential_id"`
	Operation    Operation    `json:"operation"`
	Agent        AgentBinding `json:"agent"`
	Scopes       []string     `json:"scopes"`
	BudgetClass  string       `json:"budget_class"`
	RiskClass    string       `json:"risk_class"`
}

func (snapshot AuthorizationSnapshot) Canonical() (AuthorizationSnapshot, string, []byte, error) {
	if snapshot.Caller().Validate() != nil || snapshot.Operation != OperationCreate && snapshot.Operation != OperationRead && snapshot.Operation != OperationCommand && snapshot.Operation != OperationExpire && snapshot.Operation != OperationEffect || snapshot.Agent.Validate() != nil || !slugPattern.MatchString(snapshot.BudgetClass) || !slugPattern.MatchString(snapshot.RiskClass) {
		return AuthorizationSnapshot{}, "", nil, errors.New("invalid authorization snapshot")
	}
	snapshot.Scopes = slices.Clone(snapshot.Scopes)
	sort.Strings(snapshot.Scopes)
	for index, scope := range snapshot.Scopes {
		if index > 0 && scope == snapshot.Scopes[index-1] {
			return AuthorizationSnapshot{}, "", nil, errors.New("duplicate authorization scope")
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return AuthorizationSnapshot{}, "", nil, err
	}
	digest := sha256.Sum256(encoded)
	return snapshot, "sha256:" + hex.EncodeToString(digest[:]), encoded, nil
}
func (snapshot AuthorizationSnapshot) Caller() Caller {
	return Caller{TenantID: snapshot.TenantID, PrincipalID: snapshot.PrincipalID, CredentialID: snapshot.CredentialID, Scopes: snapshot.Scopes}
}

type State string

const (
	StateQueued          State = "queued"
	StateDispatching     State = "dispatching"
	StateRunning         State = "running"
	StateWaitingInput    State = "waiting_input"
	StateCancelRequested State = "cancel_requested"
	StateSucceeded       State = "succeeded"
	StateFailed          State = "failed"
	StateCancelled       State = "cancelled"
	StateTimedOut        State = "timed_out"
)

func (state State) Terminal() bool {
	return state == StateSucceeded || state == StateFailed || state == StateCancelled || state == StateTimedOut
}

type Usage struct{ InputTokens, OutputTokens, DurationMS, BillableUnits uint64 }

func (usage Usage) Validate() error {
	if usage.InputTokens > MaxSafeInteger || usage.OutputTokens > MaxSafeInteger || usage.DurationMS > MaxSafeInteger || usage.BillableUnits > MaxSafeInteger {
		return errors.New("usage exceeds wire safe integer")
	}
	return nil
}

type Run struct {
	TenantID, RunID                  string
	Agent                            AgentBinding
	Input                            json.RawMessage
	ConversationRef                  string
	Effects                          EffectIntent
	State                            State
	StateVersion                     uint64
	AuthorizationSnapshot            json.RawMessage
	AuthorizationSnapshotDigest      string
	Traceparent                      string
	DeadlineAt, CreatedAt, UpdatedAt time.Time
	CancelRequestedAt                *time.Time
	Usage                            Usage
}

func (run Run) Validate() error {
	if !slugPattern.MatchString(run.TenantID) || !prefixedUUID("run_", run.RunID) || run.Agent.Validate() != nil || len(run.Input) == 0 || !json.Valid(run.Input) || run.Effects.Validate() != nil || run.StateVersion == 0 || run.StateVersion > MaxSafeInteger || !digestPattern.MatchString(run.AuthorizationSnapshotDigest) || !json.Valid(run.AuthorizationSnapshot) || !utc(run.DeadlineAt) || !utc(run.CreatedAt) || !utc(run.UpdatedAt) || run.UpdatedAt.Before(run.CreatedAt) || run.Usage.Validate() != nil {
		return errors.New("invalid run")
	}
	if run.State == "" {
		return errors.New("invalid state")
	}
	return nil
}

type Command struct {
	CommandID            string
	ExpectedStateVersion uint64
	Type                 string
	Data                 json.RawMessage
}

func (command Command) Validate() error {
	if !prefixedUUID("cmd_", command.CommandID) || command.ExpectedStateVersion == 0 || command.ExpectedStateVersion > MaxSafeInteger || command.Type != "run.cancel" || len(command.Data) == 0 || !json.Valid(command.Data) {
		return errors.New("invalid command")
	}
	return nil
}

type CommandRequest struct {
	Caller         Caller
	RunID          string
	IdempotencyKey string
	Command        Command
	Metadata       platform.RequestMetadata
}

type EffectReservation struct {
	TenantID, RunID, EffectID, SemanticDigest string
	CreatedAt                                 time.Time
}

type Outbox struct {
	OutboxID, TenantID, RunID, Kind string
	StateVersion                    uint64
	Payload                         json.RawMessage
	CreatedAt                       time.Time
}

func (outbox Outbox) Validate() error {
	if !prefixedUUID("out_", outbox.OutboxID) || !slugPattern.MatchString(outbox.TenantID) || !prefixedUUID("run_", outbox.RunID) || !slugPattern.MatchString(outbox.Kind) || outbox.StateVersion == 0 || outbox.StateVersion > MaxSafeInteger || !json.Valid(outbox.Payload) || !utc(outbox.CreatedAt) {
		return errors.New("invalid outbox")
	}
	return nil
}

func prefixedUUID(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidV7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}
func validIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 200 {
		return false
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
