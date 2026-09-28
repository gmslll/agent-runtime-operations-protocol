package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

var (
	ErrUnauthorized        = errors.New("MCP invocation is not authorized")
	ErrUnavailable         = errors.New("MCP dependency is unavailable")
	ErrEffectInProgress    = errors.New("MCP effect is already in progress")
	ErrEffectIndeterminate = errors.New("MCP effect outcome is indeterminate")
	ErrEffectConflict      = errors.New("MCP effect_id was reused for a different invocation")
	effectIDPattern        = regexp.MustCompile(`^eff_[A-Za-z0-9._:-]+$`)
	requestIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	runIDPattern           = regexp.MustCompile(`^run_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	attemptIDPattern       = regexp.MustCompile(`^att_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	toolNamePattern        = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)
)

type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

type Client interface {
	CallTool(context.Context, JSONRPCRequest) (CallToolResult, error)
	ReadResource(context.Context, JSONRPCRequest) (ReadResourceResult, error)
	NotifyCancelled(context.Context, JSONRPCRequest) error
}

// EffectJournal must be durable in production. Reserve is atomic: a completed
// reservation returns its prior result, a reserved one blocks a second side
// effect, and a new reservation grants the caller ownership.
type EffectJournal interface {
	Reserve(context.Context, string, string) (EffectReservation, error)
	Complete(context.Context, string, string, CallToolResult) error
}

type Adapter struct {
	Authorizer Authorizer
	Client     Client
	Effects    EffectJournal
}

func (adapter Adapter) CallTool(ctx context.Context, invocation Invocation, name string, arguments map[string]json.RawMessage) (CallToolResult, error) {
	level, effectID, err := validateInvocation(invocation)
	if err != nil {
		return CallToolResult{}, err
	}
	if !toolNamePattern.MatchString(name) {
		return CallToolResult{}, errors.New("MCP tool name is invalid")
	}
	if adapter.Authorizer == nil || adapter.Client == nil {
		return CallToolResult{}, ErrUnavailable
	}
	target := Target{Kind: "tool", Name: name, Operation: "tools/call", Effect: level}
	if err := adapter.Authorizer.Authorize(ctx, authorization(invocation, target)); err != nil {
		return CallToolResult{}, ErrUnauthorized
	}
	request, fingerprint, err := toolRequest(invocation, name, arguments, level, effectID)
	if err != nil {
		return CallToolResult{}, err
	}
	if level == "write" || level == "irreversible" {
		if adapter.Effects == nil {
			return CallToolResult{}, ErrUnavailable
		}
		reservation, reserveErr := adapter.Effects.Reserve(ctx, effectID, fingerprint)
		if reserveErr != nil {
			return CallToolResult{}, ErrEffectConflict
		}
		switch reservation.State {
		case "completed":
			return cloneResult(reservation.Result), nil
		case "reserved":
			return CallToolResult{}, ErrEffectInProgress
		case "new":
		default:
			return CallToolResult{}, ErrUnavailable
		}
	}
	result, callErr := adapter.Client.CallTool(ctx, request)
	if callErr != nil {
		if level == "write" || level == "irreversible" {
			return CallToolResult{}, ErrEffectIndeterminate
		}
		return CallToolResult{}, ErrUnavailable
	}
	if err := validateToolResult(result); err != nil {
		if level == "write" || level == "irreversible" {
			return CallToolResult{}, ErrEffectIndeterminate
		}
		return CallToolResult{}, ErrUnavailable
	}
	if level == "write" || level == "irreversible" {
		if err := adapter.Effects.Complete(ctx, effectID, fingerprint, cloneResult(result)); err != nil {
			return CallToolResult{}, ErrEffectIndeterminate
		}
	}
	return cloneResult(result), nil
}

func (adapter Adapter) ReadResource(ctx context.Context, invocation Invocation, uri string) (ReadResourceResult, error) {
	level, _, err := validateInvocation(invocation)
	if err != nil {
		return ReadResourceResult{}, err
	}
	if level != "none" && level != "read" {
		return ReadResourceResult{}, errors.New("MCP resource reads require none/read effects")
	}
	if uri == "" || strings.ContainsAny(uri, "\r\n") {
		return ReadResourceResult{}, errors.New("MCP resource URI is invalid")
	}
	if adapter.Authorizer == nil || adapter.Client == nil {
		return ReadResourceResult{}, ErrUnavailable
	}
	target := Target{Kind: "resource", Operation: "resources/read", URI: uri, Effect: level}
	if err := adapter.Authorizer.Authorize(ctx, authorization(invocation, target)); err != nil {
		return ReadResourceResult{}, ErrUnauthorized
	}
	request := JSONRPCRequest{JSONRPC: "2.0", ID: invocation.RequestID, Method: "resources/read", Params: ReadResourceParams{URI: uri, Meta: invocationMeta(invocation, level, "")}}
	result, err := adapter.Client.ReadResource(ctx, request)
	if err != nil || len(result.Contents) == 0 {
		return ReadResourceResult{}, ErrUnavailable
	}
	for _, content := range result.Contents {
		if content.URI == "" {
			return ReadResourceResult{}, ErrUnavailable
		}
	}
	return result, nil
}

func (adapter Adapter) Cancel(ctx context.Context, invocation Invocation, reason string) error {
	level, _, err := validateInvocation(invocation)
	if err != nil {
		return err
	}
	if adapter.Authorizer == nil || adapter.Client == nil {
		return ErrUnavailable
	}
	target := Target{Kind: "invocation", Operation: "notifications/cancelled", Effect: level}
	if err := adapter.Authorizer.Authorize(ctx, authorization(invocation, target)); err != nil {
		return ErrUnauthorized
	}
	if strings.TrimSpace(reason) == "" {
		reason = "cancelled"
	}
	request := JSONRPCRequest{JSONRPC: "2.0", Method: "notifications/cancelled", Params: CancelledParams{RequestID: invocation.RequestID, Reason: "AROP invocation cancelled"}}
	if err := adapter.Client.NotifyCancelled(ctx, request); err != nil {
		return ErrUnavailable
	}
	return nil
}

func toolRequest(invocation Invocation, name string, arguments map[string]json.RawMessage, level, effectID string) (JSONRPCRequest, string, error) {
	clean := make(map[string]json.RawMessage, len(arguments))
	for key, value := range arguments {
		if key == "" || !json.Valid(value) {
			return JSONRPCRequest{}, "", errors.New("MCP tool arguments are invalid")
		}
		var decoded any
		if err := protocolcore.DecodeAuthoring(value, &decoded); err != nil {
			return JSONRPCRequest{}, "", errors.New("MCP tool arguments are invalid")
		}
		clean[key] = append(json.RawMessage(nil), value...)
	}
	params := CallToolParams{Name: name, Arguments: clean, Meta: invocationMeta(invocation, level, effectID)}
	fingerprintWire, err := json.Marshal(struct {
		RunID     string                     `json:"run_id"`
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
		Effect    string                     `json:"effect"`
	}{invocation.RunID, name, clean, level})
	if err != nil {
		return JSONRPCRequest{}, "", err
	}
	digest := sha256.Sum256(fingerprintWire)
	return JSONRPCRequest{JSONRPC: "2.0", ID: invocation.RequestID, Method: "tools/call", Params: params}, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateInvocation(invocation Invocation) (string, string, error) {
	if !requestIDPattern.MatchString(invocation.RequestID) || !runIDPattern.MatchString(invocation.RunID) || !attemptIDPattern.MatchString(invocation.AttemptID) || !digestPattern.MatchString(invocation.AuthorizationSnapshotDigest) {
		return "", "", errors.New("AROP invocation identity is incomplete")
	}
	trace := protocolcore.TraceContext{Traceparent: invocation.Traceparent, Tracestate: invocation.Tracestate}
	if err := trace.Validate(); err != nil {
		return "", "", errors.New("AROP trace context is invalid")
	}
	count, level, effectID := 0, "", ""
	if invocation.Effects.None != nil {
		count++
		level = invocation.Effects.None.Level
	}
	if invocation.Effects.Read != nil {
		count++
		level = invocation.Effects.Read.Level
	}
	if invocation.Effects.Write != nil {
		count++
		level = invocation.Effects.Write.Level
		effectID = string(invocation.Effects.Write.EffectID)
	}
	if invocation.Effects.Irreversible != nil {
		count++
		level = invocation.Effects.Irreversible.Level
		effectID = string(invocation.Effects.Irreversible.EffectID)
	}
	if count != 1 || (level != "none" && level != "read" && level != "write" && level != "irreversible") {
		return "", "", errors.New("AROP effect declaration is invalid")
	}
	if (level == "write" || level == "irreversible") && (len(effectID) < 8 || len(effectID) > 200 || !effectIDPattern.MatchString(effectID)) {
		return "", "", errors.New("AROP write effect_id is invalid")
	}
	if (level == "none" || level == "read") && effectID != "" {
		return "", "", errors.New("AROP read effect must not carry effect_id")
	}
	return level, effectID, nil
}

func invocationMeta(invocation Invocation, level, effectID string) map[string]any {
	meta := map[string]any{
		"traceparent":     invocation.Traceparent,
		InvocationMetaKey: map[string]any{"runId": invocation.RunID, "attemptId": invocation.AttemptID, "authorizationSnapshotDigest": invocation.AuthorizationSnapshotDigest},
	}
	if invocation.Tracestate != "" {
		meta["tracestate"] = invocation.Tracestate
	}
	if effectID != "" {
		meta[EffectMetaKey] = map[string]any{"level": level, "effectId": effectID}
	}
	return meta
}

func authorization(invocation Invocation, target Target) AuthorizationRequest {
	return AuthorizationRequest{RunID: invocation.RunID, AttemptID: invocation.AttemptID, AuthorizationSnapshotDigest: invocation.AuthorizationSnapshotDigest, Target: target}
}

func validateToolResult(result CallToolResult) error {
	if len(result.Content) == 0 && len(result.StructuredContent) == 0 {
		return errors.New("empty MCP tool result")
	}
	if len(result.StructuredContent) != 0 && !json.Valid(result.StructuredContent) {
		return errors.New("invalid MCP structured result")
	}
	for _, content := range result.Content {
		if content.Type != "text" && content.Type != "resource" {
			return fmt.Errorf("unsupported MCP content type %q", content.Type)
		}
	}
	return nil
}

func cloneResult(value CallToolResult) CallToolResult {
	result := value
	result.StructuredContent = append(json.RawMessage(nil), value.StructuredContent...)
	result.Content = append([]Content(nil), value.Content...)
	for index := range result.Content {
		result.Content[index].Data = append(json.RawMessage(nil), value.Content[index].Data...)
	}
	return result
}
