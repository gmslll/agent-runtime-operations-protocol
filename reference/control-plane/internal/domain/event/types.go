package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	MaxSafeInteger = uint64(9007199254740991)
	MaxBatchEvents = 100
	MaxBatchBytes  = 262144
)

var (
	uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	outputPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
)

type SessionRequest struct {
	TenantID                 string
	RunID, AttemptID         string
	DeploymentID             string
	Generation, FencingToken uint64
	Metadata                 platform.RequestMetadata
}

func (request SessionRequest) Validate() error {
	if !slugPattern.MatchString(request.TenantID) || len(request.TenantID) > 128 || !prefixedUUID("run_", request.RunID) || !prefixedUUID("att_", request.AttemptID) || !prefixedUUID("dep_", request.DeploymentID) || !positiveSafe(request.Generation) || !positiveSafe(request.FencingToken) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type Session struct {
	TenantID, RunID, AttemptID, DeploymentID, InstanceID, SessionID string
	Generation, FencingToken                                        uint64
	Token, TokenDigest                                              string
	ExpiresAt, CreatedAt                                            time.Time
}

func (session Session) Validate() error {
	if !slugPattern.MatchString(session.TenantID) || len(session.TenantID) > 128 || !prefixedUUID("run_", session.RunID) || !prefixedUUID("att_", session.AttemptID) || !prefixedUUID("dep_", session.DeploymentID) || !slugPattern.MatchString(session.InstanceID) || !prefixedUUID("ses_", session.SessionID) || !positiveSafe(session.Generation) || !positiveSafe(session.FencingToken) || !strings.HasPrefix(session.Token, "evtcap_") || len(session.Token) < 50 || len(session.Token) > 178 || !digestPattern.MatchString(session.TokenDigest) || !utc(session.CreatedAt) || !utc(session.ExpiresAt) || !session.ExpiresAt.After(session.CreatedAt) || session.ExpiresAt.Sub(session.CreatedAt) > 5*time.Minute {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type Envelope struct {
	SpecVersion, ID, Source, Type, Subject, DataContentType, DataSchema string
	Time                                                                time.Time
	RunID, AttemptID                                                    string
	ProducerSequence, RunSequence                                       uint64
	Traceparent                                                         string
	Data                                                                json.RawMessage
}

func (event Envelope) Validate() error {
	parsedSource, sourceErr := url.Parse(event.Source)
	parsedSchema, schemaErr := url.Parse(event.DataSchema)
	if event.SpecVersion != "1.0" || !prefixedUUID("evt_", event.ID) || sourceErr != nil || parsedSource.Scheme != "https" || parsedSource.Host == "" || parsedSource.User != nil || parsedSource.RawQuery != "" || parsedSource.Fragment != "" || event.Subject != "runs/"+event.RunID || !utc(event.Time) || event.DataContentType != "application/json" || schemaErr != nil || parsedSchema.Scheme != "https" || parsedSchema.Host != "arop.invalid" || !strings.HasPrefix(parsedSchema.Path, "/schemas/v1/events/") || !prefixedUUID("run_", event.RunID) || !prefixedUUID("att_", event.AttemptID) || !positiveSafe(event.ProducerSequence) || event.RunSequence > MaxSafeInteger || (protocolcore.TraceContext{Traceparent: event.Traceparent}).Validate() != nil || !jsonObject(event.Data) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if err := validatePayload(event.Type, event.DataSchema, event.Data); err != nil {
		return err
	}
	return nil
}

func (event Envelope) Canonical() ([]byte, string, error) {
	if err := event.Validate(); err != nil {
		return nil, "", err
	}
	value := struct {
		SpecVersion      string          `json:"specversion"`
		ID               string          `json:"id"`
		Source           string          `json:"source"`
		Type             string          `json:"type"`
		Subject          string          `json:"subject"`
		Time             string          `json:"time"`
		DataContentType  string          `json:"datacontenttype"`
		DataSchema       string          `json:"dataschema"`
		RunID            string          `json:"runid"`
		AttemptID        string          `json:"attemptid"`
		ProducerSequence uint64          `json:"producersequence"`
		Traceparent      string          `json:"traceparent"`
		Data             json.RawMessage `json:"data"`
	}{event.SpecVersion, event.ID, event.Source, event.Type, event.Subject, event.Time.Format(time.RFC3339Nano), event.DataContentType, event.DataSchema, event.RunID, event.AttemptID, event.ProducerSequence, event.Traceparent, event.Data}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", NewError(CategoryValidation, ReasonInvalidRequest)
	}
	digest := sha256.Sum256(encoded)
	return encoded, "sha256:" + hex.EncodeToString(digest[:]), nil
}

type BatchRequest struct {
	Token, RunID, BatchID, AttemptID, IdempotencyKey string
	FencingToken                                     uint64
	Events                                           []Envelope
	Metadata                                         platform.RequestMetadata
}

func (request BatchRequest) Validate() error {
	if !strings.HasPrefix(request.Token, "evtcap_") || len(request.Token) < 50 || len(request.Token) > 178 || !prefixedUUID("run_", request.RunID) || !prefixedUUID("batch_", request.BatchID) || !prefixedUUID("att_", request.AttemptID) || !positiveSafe(request.FencingToken) || !validIdempotencyKey(request.IdempotencyKey) || len(request.Events) == 0 || len(request.Events) > MaxBatchEvents {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	for _, item := range request.Events {
		if item.RunID != request.RunID || item.AttemptID != request.AttemptID || item.Validate() != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	}
	encoded, _ := json.Marshal(request.Events)
	if len(encoded) > MaxBatchBytes {
		return NewError(CategoryCapacity, ReasonBatchTooLarge)
	}
	return nil
}

type Ack struct {
	AcceptedThroughProducerSequence uint64
	AssignedRunSequence             uint64
	DuplicateEventIDs               []string
	RunState                        run.State
}

func (ack Ack) Validate() error {
	if !positiveSafe(ack.AcceptedThroughProducerSequence) || !positiveSafe(ack.AssignedRunSequence) || len(ack.DuplicateEventIDs) > MaxBatchEvents {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	seen := map[string]struct{}{}
	for _, id := range ack.DuplicateEventIDs {
		if !prefixedUUID("evt_", id) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, ok := seen[id]; ok {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		seen[id] = struct{}{}
	}
	if ack.RunState == "" {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

type TerminalProjection struct {
	State       run.State
	Usage       run.Usage
	Result      json.RawMessage
	CompletedAt time.Time
}

func (projection TerminalProjection) Validate() error {
	if !projection.State.Terminal() || projection.Usage.Validate() != nil || !jsonObject(projection.Result) || !utc(projection.CompletedAt) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func Terminal(event Envelope) (TerminalProjection, bool, error) {
	if !strings.HasPrefix(event.Type, "io.arop.run.") {
		return TerminalProjection{}, false, nil
	}
	stateName := strings.TrimSuffix(strings.TrimPrefix(event.Type, "io.arop.run."), ".v1")
	state := run.State(stateName)
	if !state.Terminal() {
		return TerminalProjection{}, false, nil
	}
	var raw map[string]json.RawMessage
	if strictDecode(event.Data, &raw) != nil {
		return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	allowed := map[string]bool{"state": true, "usage": true, "completed_at": true, "snapshot": true, "result_ref": true, "error": true}
	for key := range raw {
		if !allowed[key] {
			return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
		}
	}
	var stateValue run.State
	var completedText string
	if strictDecode(raw["state"], &stateValue) != nil || strictDecode(raw["completed_at"], &completedText) != nil || stateValue != state {
		return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	var usage struct {
		InputTokens   uint64 `json:"input_tokens"`
		OutputTokens  uint64 `json:"output_tokens"`
		DurationMS    uint64 `json:"duration_ms"`
		BillableUnits uint64 `json:"billable_units"`
	}
	if strictDecode(raw["usage"], &usage) != nil {
		return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	completedAt, err := time.Parse(time.RFC3339Nano, completedText)
	_, hasSnapshot := raw["snapshot"]
	_, hasReference := raw["result_ref"]
	_, hasFailure := raw["error"]
	switch state {
	case run.StateSucceeded:
		if hasSnapshot == hasReference || hasFailure {
			return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case run.StateFailed:
		if !hasFailure || hasSnapshot || hasReference {
			return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case run.StateCancelled, run.StateTimedOut:
		if hasSnapshot || hasReference {
			return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
		}
	}
	projection := TerminalProjection{State: state, Usage: run.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, DurationMS: usage.DurationMS, BillableUnits: usage.BillableUnits}, Result: append(json.RawMessage(nil), event.Data...), CompletedAt: completedAt}
	if err != nil || projection.Validate() != nil {
		return TerminalProjection{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return projection, true, nil
}

func NonTerminalState(event Envelope) (run.State, bool) {
	switch event.Type {
	case "io.arop.run.accepted.v1", "io.arop.run.started.v1":
		return run.StateRunning, true
	case "io.arop.run.waiting_input.v1":
		return run.StateWaitingInput, true
	case "io.arop.run.cancel_requested.v1":
		return run.StateCancelRequested, true
	default:
		return "", false
	}
}

func Usage(event Envelope) (run.Usage, bool, error) {
	if event.Type != "io.arop.usage.updated.v1" {
		return run.Usage{}, false, nil
	}
	var payload struct {
		InputTokens   uint64 `json:"input_tokens"`
		OutputTokens  uint64 `json:"output_tokens"`
		DurationMS    uint64 `json:"duration_ms"`
		BillableUnits uint64 `json:"billable_units"`
	}
	if strictDecode(event.Data, &payload) != nil {
		return run.Usage{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	usage := run.Usage{InputTokens: payload.InputTokens, OutputTokens: payload.OutputTokens, DurationMS: payload.DurationMS, BillableUnits: payload.BillableUnits}
	if usage.Validate() != nil {
		return run.Usage{}, true, NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return usage, true, nil
}

func validatePayload(kind, schema string, data json.RawMessage) error {
	base := "https://arop.invalid/schemas/v1/events/"
	schemaName := ""
	switch {
	case strings.HasPrefix(kind, "io.arop.run."):
		schemaName = "lifecycle-events-v1.schema.json"
		var raw map[string]json.RawMessage
		var stateValue string
		if strictDecode(data, &raw) != nil || strictDecode(raw["state"], &stateValue) != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		want := strings.TrimSuffix(strings.TrimPrefix(kind, "io.arop.run."), ".v1")
		if stateValue != want {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if terminal := run.State(want).Terminal(); terminal {
			if _, err := eventwire.DecodeLifecycleTerminalData(data); err != nil {
				return NewError(CategoryValidation, ReasonInvalidRequest)
			}
			_, _, err := Terminal(Envelope{Type: kind, Data: data})
			if err != nil {
				return err
			}
		} else if len(raw) != 1 {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		} else if _, err := eventwire.DecodeLifecycleStateData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case strings.HasPrefix(kind, "io.arop.output."):
		schemaName = "output-events-v1.schema.json"
		if err := validateOutput(kind, data); err != nil {
			return err
		}
	case kind == "io.arop.progress.updated.v1":
		schemaName = "progress-events-v1.schema.json"
		var payload struct {
			StepID   string `json:"step_id"`
			Progress uint64 `json:"progress"`
			Message  string `json:"message"`
			State    string `json:"state"`
		}
		if strictDecode(data, &payload) != nil || payload.Progress > 100 {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, err := eventwire.DecodeProgressEventData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case kind == "io.arop.usage.updated.v1":
		schemaName = "usage-events-v1.schema.json"
		if _, _, err := Usage(Envelope{Type: kind, Data: data}); err != nil {
			return err
		}
		if _, err := eventwire.DecodeUsageEventData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	default:
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	if schema != base+schemaName {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func validateOutput(kind string, data json.RawMessage) error {
	name := strings.TrimSuffix(strings.TrimPrefix(kind, "io.arop.output."), ".v1")
	var common struct {
		OutputID string `json:"output_id"`
	}
	if json.Unmarshal(data, &common) != nil || !outputPattern.MatchString(common.OutputID) {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	switch name {
	case "delta":
		var value struct {
			OutputID string `json:"output_id"`
			Offset   uint64 `json:"offset"`
			Delta    string `json:"delta"`
		}
		if strictDecode(data, &value) != nil || value.Offset > MaxSafeInteger || !utf8.ValidString(value.Delta) || len(value.Delta) > 1<<20 {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, err := eventwire.DecodeOutputDeltaData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case "snapshot":
		var value struct {
			OutputID string          `json:"output_id"`
			Revision uint64          `json:"revision"`
			Content  json.RawMessage `json:"content"`
			Digest   string          `json:"digest"`
		}
		if strictDecode(data, &value) != nil || !positiveSafe(value.Revision) || !digestPattern.MatchString(value.Digest) || !jsonArray(value.Content) {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, err := eventwire.DecodeOutputSnapshotData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	case "started", "reset", "completed":
		var value struct {
			OutputID string `json:"output_id"`
			State    string `json:"state"`
		}
		if strictDecode(data, &value) != nil || value.State != name {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
		if _, err := eventwire.DecodeOutputStateData(data); err != nil {
			return NewError(CategoryValidation, ReasonInvalidRequest)
		}
	default:
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func BatchDigest(request BatchRequest) string {
	values := make([]string, 0, len(request.Events))
	for _, item := range request.Events {
		_, digest, _ := item.Canonical()
		values = append(values, digest)
	}
	encoded, _ := json.Marshal(struct {
		RunID, AttemptID, BatchID string
		FencingToken              uint64
		Events                    []string
	}{request.RunID, request.AttemptID, request.BatchID, request.FencingToken, values})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func jsonObject(data []byte) bool {
	var value map[string]json.RawMessage
	return strictDecode(data, &value) == nil && value != nil
}
func jsonArray(data []byte) bool {
	var value []json.RawMessage
	return strictDecode(data, &value) == nil && value != nil && len(value) > 0 && len(value) <= 256
}
func prefixedUUID(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidV7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}
func positiveSafe(value uint64) bool { return value > 0 && value <= MaxSafeInteger }
func utc(value time.Time) bool       { return !value.IsZero() && value.Location() == time.UTC }
func validIdempotencyKey(value string) bool {
	return len(value) >= 8 && len(value) <= 200 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
func FormatEvent(event Envelope) string {
	return fmt.Sprintf("%s/%s/%d", event.Source, event.ID, event.ProducerSequence)
}
