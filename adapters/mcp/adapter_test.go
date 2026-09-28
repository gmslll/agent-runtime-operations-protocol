package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

func TestReadToolPropagatesTraceAndRequiresAuthorization(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	client := &fakeClient{toolResult: CallToolResult{Content: []Content{{Type: "text", Text: "ok"}}}}
	adapter := Adapter{Authorizer: authorizer, Client: client}
	result, err := adapter.CallTool(context.Background(), invocation("read", ""), "search", map[string]json.RawMessage{"query": json.RawMessage(`"AROP"`)})
	if err != nil || result.Content[0].Text != "ok" || client.toolCalls != 1 || len(authorizer.requests) != 1 {
		t.Fatalf("result=%+v calls=%d auth=%+v err=%v", result, client.toolCalls, authorizer.requests, err)
	}
	params := client.lastTool.Params.(CallToolParams)
	if params.Meta["traceparent"] != testTrace || params.Meta[EffectMetaKey] != nil || authorizer.requests[0].Target.Effect != "read" {
		t.Fatalf("params=%+v auth=%+v", params, authorizer.requests[0])
	}
}

func TestWriteToolUsesStableEffectJournalAcrossAttempts(t *testing.T) {
	journal := newFakeJournal()
	client := &fakeClient{toolResult: CallToolResult{StructuredContent: json.RawMessage(`{"created":true}`)}}
	adapter := Adapter{Authorizer: &fakeAuthorizer{}, Client: client, Effects: journal}
	first := invocation("write", "eff_order:1234")
	result, err := adapter.CallTool(context.Background(), first, "create_order", map[string]json.RawMessage{"sku": json.RawMessage(`"A"`)})
	if err != nil || string(result.StructuredContent) != `{"created":true}` {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	second := first
	second.AttemptID = "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef13"
	result, err = adapter.CallTool(context.Background(), second, "create_order", map[string]json.RawMessage{"sku": json.RawMessage(`"A"`)})
	if err != nil || client.toolCalls != 1 || string(result.StructuredContent) != `{"created":true}` {
		t.Fatalf("dedupe result=%+v calls=%d err=%v", result, client.toolCalls, err)
	}
	params := client.lastTool.Params.(CallToolParams)
	effect := params.Meta[EffectMetaKey].(map[string]any)
	if effect["effectId"] != "eff_order:1234" || effect["level"] != "write" {
		t.Fatalf("effect meta=%+v", effect)
	}
	if _, err := adapter.CallTool(context.Background(), second, "create_order", map[string]json.RawMessage{"sku": json.RawMessage(`"B"`)}); !errors.Is(err, ErrEffectConflict) {
		t.Fatalf("effect reuse mismatch err=%v", err)
	}
	pending := invocation("write", "eff_pending:1234")
	_, fingerprint, err := toolRequest(pending, "create_order", map[string]json.RawMessage{"sku": json.RawMessage(`"C"`)}, "write", "eff_pending:1234")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Reserve(context.Background(), "eff_pending:1234", fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CallTool(context.Background(), pending, "create_order", map[string]json.RawMessage{"sku": json.RawMessage(`"C"`)}); !errors.Is(err, ErrEffectInProgress) {
		t.Fatalf("in-flight duplicate err=%v", err)
	}
}

func TestWriteToolFailsClosedWithoutEffectOrDurableJournal(t *testing.T) {
	adapter := Adapter{Authorizer: &fakeAuthorizer{}, Client: &fakeClient{toolResult: CallToolResult{Content: []Content{{Type: "text", Text: "ok"}}}}}
	invalid := invocation("write", "")
	if _, err := adapter.CallTool(context.Background(), invalid, "write", nil); err == nil {
		t.Fatal("write without effect_id accepted")
	}
	if _, err := adapter.CallTool(context.Background(), invocation("write", "eff_write:1234"), "write", nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("write without durable journal err=%v", err)
	}
	read := invocation("read", "")
	if _, err := adapter.CallTool(context.Background(), read, "write", map[string]json.RawMessage{"bad": json.RawMessage(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("duplicate-key tool arguments accepted")
	}
}

func TestResourceReadIsAuthorizedAndCannotCarryWriteEffects(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	client := &fakeClient{resourceResult: ReadResourceResult{Contents: []ResourceContent{{URI: "mcp://docs/item", MimeType: "text/plain", Text: "value"}}}}
	adapter := Adapter{Authorizer: authorizer, Client: client}
	result, err := adapter.ReadResource(context.Background(), invocation("read", ""), "mcp://docs/item")
	if err != nil || result.Contents[0].Text != "value" || authorizer.requests[0].Target.Operation != "resources/read" {
		t.Fatalf("result=%+v auth=%+v err=%v", result, authorizer.requests, err)
	}
	if _, err := adapter.ReadResource(context.Background(), invocation("irreversible", "eff_delete:1234"), "mcp://docs/item"); err == nil {
		t.Fatal("write-class resource read accepted")
	}
}

func TestDenyDependencyErrorAndCancelAreFailClosedAndRedacted(t *testing.T) {
	authorizer := &fakeAuthorizer{err: errors.New("secret bearer credential")}
	client := &fakeClient{toolErr: errors.New("dsn=password bearer=secret")}
	adapter := Adapter{Authorizer: authorizer, Client: client}
	if _, err := adapter.CallTool(context.Background(), invocation("read", ""), "search", nil); !errors.Is(err, ErrUnauthorized) || strings.Contains(err.Error(), "secret") || client.toolCalls != 0 {
		t.Fatalf("authorization error leaked or called dependency: %v calls=%d", err, client.toolCalls)
	}
	authorizer.err = nil
	if _, err := adapter.CallTool(context.Background(), invocation("read", ""), "search", nil); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "password") {
		t.Fatalf("dependency error leaked: %v", err)
	}
	if err := adapter.Cancel(context.Background(), invocation("read", ""), strings.Repeat("x", 300)); err != nil {
		t.Fatal(err)
	}
	if client.lastCancel.Method != "notifications/cancelled" || client.lastCancel.Params.(CancelledParams).Reason != "AROP invocation cancelled" {
		t.Fatalf("cancel=%+v", client.lastCancel)
	}
}

func TestFixturesPinMCPRevisionWithoutEndpointOrCredential(t *testing.T) {
	for _, name := range []string{"testdata/tool-call.json", "testdata/resource-read.json", "testdata/compatibility.json"} {
		wire, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(wire))
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "password") || strings.Contains(lower, "session_id") || strings.Contains(lower, "access_token") {
			t.Fatalf("fixture contains forbidden deployment/session material: %s", name)
		}
		var value any
		if err := protocolcore.DecodeAuthoring(wire, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	wire, _ := os.ReadFile("testdata/compatibility.json")
	if !strings.Contains(string(wire), `"protocol_revision": "`+ProtocolRevision+`"`) || !strings.Contains(string(wire), `"effect_meta_key": "`+EffectMetaKey+`"`) {
		t.Fatalf("compatibility pin mismatch: %s", wire)
	}
}

const testTrace = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func invocation(level, effectID string) Invocation {
	effects := runwire.Effects{}
	switch level {
	case "none":
		effects.None = &runwire.EffectsNone{Level: level}
	case "read":
		effects.Read = &runwire.EffectsRead{Level: level}
	case "write":
		effects.Write = &runwire.EffectsWrite{Level: level, EffectID: runwire.EffectId(effectID)}
	case "irreversible":
		effects.Irreversible = &runwire.EffectsIrreversible{Level: level, EffectID: runwire.EffectId(effectID)}
	}
	return Invocation{RequestID: "mcp-request-1", RunID: "run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10", AttemptID: "att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12", AuthorizationSnapshotDigest: "sha256:" + strings.Repeat("a", 64), Effects: effects, Traceparent: testTrace}
}

type fakeAuthorizer struct {
	requests []AuthorizationRequest
	err      error
}

func (value *fakeAuthorizer) Authorize(_ context.Context, request AuthorizationRequest) error {
	value.requests = append(value.requests, request)
	return value.err
}

type fakeClient struct {
	mu             sync.Mutex
	toolCalls      int
	toolResult     CallToolResult
	toolErr        error
	resourceResult ReadResourceResult
	resourceErr    error
	lastTool       JSONRPCRequest
	lastCancel     JSONRPCRequest
}

func (value *fakeClient) CallTool(_ context.Context, request JSONRPCRequest) (CallToolResult, error) {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.toolCalls++
	value.lastTool = request
	return cloneResult(value.toolResult), value.toolErr
}
func (value *fakeClient) ReadResource(_ context.Context, _ JSONRPCRequest) (ReadResourceResult, error) {
	return value.resourceResult, value.resourceErr
}
func (value *fakeClient) NotifyCancelled(_ context.Context, request JSONRPCRequest) error {
	value.lastCancel = request
	return nil
}

type journalRecord struct {
	fingerprint, state string
	result             CallToolResult
}
type fakeJournal struct {
	mu      sync.Mutex
	records map[string]journalRecord
}

func newFakeJournal() *fakeJournal { return &fakeJournal{records: map[string]journalRecord{}} }
func (value *fakeJournal) Reserve(_ context.Context, effectID, fingerprint string) (EffectReservation, error) {
	value.mu.Lock()
	defer value.mu.Unlock()
	record, exists := value.records[effectID]
	if exists && record.fingerprint != fingerprint {
		return EffectReservation{}, errors.New("fingerprint mismatch")
	}
	if exists {
		return EffectReservation{State: record.state, Result: cloneResult(record.result)}, nil
	}
	value.records[effectID] = journalRecord{fingerprint: fingerprint, state: "reserved"}
	return EffectReservation{State: "new"}, nil
}
func (value *fakeJournal) Complete(_ context.Context, effectID, fingerprint string, result CallToolResult) error {
	value.mu.Lock()
	defer value.mu.Unlock()
	record, exists := value.records[effectID]
	if !exists || record.fingerprint != fingerprint || record.state != "reserved" {
		return errors.New("invalid completion")
	}
	record.state, record.result = "completed", cloneResult(result)
	value.records[effectID] = record
	return nil
}
