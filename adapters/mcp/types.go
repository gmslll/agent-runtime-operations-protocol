// Package mcp provides a transport-neutral interoperability boundary between
// an authorized AROP Run/Attempt and MCP tool/resource calls. It is not an MCP
// server implementation and never turns MCP transport state into an AROP Run.
package mcp

import (
	"encoding/json"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

const (
	ProtocolRevision  = "2026-07-28"
	InvocationMetaKey = "invalid.arop/invocation"
	EffectMetaKey     = "invalid.arop/effect"
)

type Invocation struct {
	RequestID                   string
	RunID                       string
	AttemptID                   string
	AuthorizationSnapshotDigest string
	Effects                     runwire.Effects
	Traceparent                 string
	Tracestate                  string
}

type Target struct {
	Kind      string
	Name      string
	Operation string
	URI       string
	Effect    string
}

type AuthorizationRequest struct {
	RunID                       string
	AttemptID                   string
	AuthorizationSnapshotDigest string
	Target                      Target
}

type JSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type CallToolParams struct {
	Name      string                     `json:"name"`
	Arguments map[string]json.RawMessage `json:"arguments,omitempty"`
	Meta      map[string]any             `json:"_meta,omitempty"`
}

type ReadResourceParams struct {
	URI  string         `json:"uri"`
	Meta map[string]any `json:"_meta,omitempty"`
}

type CancelledParams struct {
	RequestID string `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

type Content struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
}

type CallToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

type ReadResourceResult struct {
	Contents []ResourceContent `json:"contents"`
}

type EffectReservation struct {
	State  string
	Result CallToolResult
}
