package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
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
		if !effectPattern.MatchString(intent.EffectID) || utf8.RuneCountInString(intent.EffectID) < 8 || utf8.RuneCountInString(intent.EffectID) > 200 {
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
	Labels          json.RawMessage
	ConversationRef string
	Tracestate      string
	DeadlineAt      time.Time
	Effects         EffectIntent
	Metadata        platform.RequestMetadata
	IdempotencyKey  string
}

func (request CreateRequest) Validate(now time.Time) error {
	if request.Caller.Validate() != nil || request.Agent.Validate() != nil || !validRunInput(request.Input) || request.Effects.Validate() != nil || !validIdempotencyKey(request.IdempotencyKey) || !utc(request.DeadlineAt) || !request.DeadlineAt.After(now) || request.DeadlineAt.Sub(now) > 7*24*time.Hour {
		return errors.New("invalid run request")
	}
	if _, err := canonicalLabels(request.Labels); err != nil {
		return errors.New("invalid run labels")
	}
	if err := (protocolcore.TraceContext{Traceparent: request.Metadata.Traceparent(), Tracestate: request.Tracestate}).Validate(); err != nil {
		return errors.New("invalid run trace context")
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
	Labels                           json.RawMessage
	ConversationRef                  string
	Effects                          EffectIntent
	State                            State
	StateVersion                     uint64
	AuthorizationSnapshot            json.RawMessage
	AuthorizationSnapshotDigest      string
	Traceparent                      string
	Tracestate                       string
	DeadlineAt, CreatedAt, UpdatedAt time.Time
	CancelRequestedAt                *time.Time
	Usage                            Usage
	Result                           json.RawMessage
}

func (run Run) Validate() error {
	if !slugPattern.MatchString(run.TenantID) || !prefixedUUID("run_", run.RunID) || run.Agent.Validate() != nil || !validRunInput(run.Input) || run.Effects.Validate() != nil || run.StateVersion == 0 || run.StateVersion > MaxSafeInteger || !digestPattern.MatchString(run.AuthorizationSnapshotDigest) || !json.Valid(run.AuthorizationSnapshot) || !utc(run.DeadlineAt) || !utc(run.CreatedAt) || !utc(run.UpdatedAt) || run.UpdatedAt.Before(run.CreatedAt) || run.Usage.Validate() != nil {
		return errors.New("invalid run")
	}
	if _, err := canonicalLabels(run.Labels); err != nil {
		return errors.New("invalid run labels")
	}
	if len(run.Result) != 0 {
		var result map[string]json.RawMessage
		if json.Unmarshal(run.Result, &result) != nil || result == nil {
			return errors.New("invalid run result")
		}
	}
	if err := (protocolcore.TraceContext{Traceparent: run.Traceparent, Tracestate: run.Tracestate}).Validate(); err != nil {
		return errors.New("invalid run trace context")
	}
	switch run.State {
	case StateQueued, StateDispatching, StateRunning, StateWaitingInput, StateCancelRequested, StateSucceeded, StateFailed, StateCancelled, StateTimedOut:
	default:
		return errors.New("invalid state")
	}
	if run.ConversationRef != "" && !prefixedUUID("conv_", run.ConversationRef) {
		return errors.New("invalid run conversation reference")
	}
	var snapshot AuthorizationSnapshot
	if json.Unmarshal(run.AuthorizationSnapshot, &snapshot) != nil {
		return errors.New("invalid authorization snapshot")
	}
	canonical, digest, _, err := snapshot.Canonical()
	if err != nil || digest != run.AuthorizationSnapshotDigest || canonical.TenantID != run.TenantID || canonical.Agent != run.Agent || canonical.Operation != OperationCreate {
		return errors.New("authorization snapshot binding mismatch")
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

// OverdueRun identifies a non-terminal run whose deadline has passed.
type OverdueRun struct {
	TenantID, RunID string
	StateVersion    uint64
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

func validRunInput(raw json.RawMessage) bool {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var parts []json.RawMessage
	if decoder.Decode(&parts) != nil || len(parts) < 1 || len(parts) > 256 {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
}

func canonicalLabels(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("labels are not UTF-8")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("invalid labels")
	}
	labels := map[string]string{}
	for decoder.More() {
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok {
			return nil, errors.New("invalid label key")
		}
		if _, duplicate := labels[key]; duplicate {
			return nil, errors.New("duplicate label key")
		}
		var value string
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid label value")
		}
		labels[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(labels) > 64 {
		return nil, errors.New("invalid labels")
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("labels contain trailing value")
	}
	for key, value := range labels {
		if !slugPattern.MatchString(key) || utf8.RuneCountInString(key) > 64 || value == "" || utf8.RuneCountInString(value) > 256 {
			return nil, fmt.Errorf("invalid label %q", key)
		}
	}
	encoded, err := json.Marshal(labels)
	return json.RawMessage(encoded), err
}
func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
