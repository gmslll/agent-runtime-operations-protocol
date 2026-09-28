// Package a2a maps AROP v1 public objects to the A2A v1.0 wire model.
// It deliberately models only the A2A surface that AROP can preserve and
// rejects unsupported forms instead of inventing apparently lossless data.
package a2a

import "encoding/json"

const (
	ProtocolVersion = "1.0"
	FixtureRelease  = "1.0.1"
	AdapterVersion  = "arop-a2a-v1"
	ProtoSHA256     = "e195bf96ab630c69797851970203e1b2b6b19528f2e9803b7d904b91a5104016"
)

type MappingLevel string

const (
	Exact       MappingLevel = "exact"
	Extended    MappingLevel = "extended"
	Lossy       MappingLevel = "lossy"
	Unsupported MappingLevel = "unsupported"
)

type OriginalObjectRef struct {
	Protocol string `json:"protocol"`
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	SHA256   string `json:"sha256"`
}

type MappingItem struct {
	Source string       `json:"source"`
	Target string       `json:"target"`
	Level  MappingLevel `json:"level"`
	Reason string       `json:"reason,omitempty"`
}

type MappingReport struct {
	AdapterVersion   string            `json:"adapter_version"`
	SourceProtocol   string            `json:"source_protocol"`
	TargetProtocol   string            `json:"target_protocol"`
	FixtureRelease   string            `json:"fixture_release"`
	Overall          MappingLevel      `json:"overall"`
	Original         OriginalObjectRef `json:"original_object_ref"`
	Items            []MappingItem     `json:"items"`
	UnmappedSecurity []string          `json:"unmapped_security_semantics,omitempty"`
}

type AgentCard struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description"`
	SupportedInterfaces  []AgentInterface          `json:"supportedInterfaces"`
	Version              string                    `json:"version"`
	Capabilities         AgentCapabilities         `json:"capabilities"`
	SecuritySchemes      map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []SecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                  `json:"defaultInputModes"`
	DefaultOutputModes   []string                  `json:"defaultOutputModes"`
	Skills               []AgentSkill              `json:"skills"`
	IconURL              string                    `json:"iconUrl,omitempty"`
}

type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type AgentCapabilities struct {
	Streaming bool `json:"streaming,omitempty"`
}

type SecurityScheme struct {
	HTTPAuthSecurityScheme *HTTPAuthSecurityScheme `json:"httpAuthSecurityScheme,omitempty"`
}

type HTTPAuthSecurityScheme struct {
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

type SecurityRequirement struct {
	Schemes map[string]StringList `json:"schemes"`
}
type StringList struct {
	List []string `json:"list"`
}

type AgentSkill struct {
	ID                   string                `json:"id"`
	Name                 string                `json:"name"`
	Description          string                `json:"description"`
	Tags                 []string              `json:"tags"`
	Examples             []string              `json:"examples,omitempty"`
	InputModes           []string              `json:"inputModes,omitempty"`
	OutputModes          []string              `json:"outputModes,omitempty"`
	SecurityRequirements []SecurityRequirement `json:"securityRequirements,omitempty"`
}

type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId,omitempty"`
	Status    TaskStatus     `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type TaskStatus struct {
	State     string   `json:"state"`
	Message   *Message `json:"message,omitempty"`
	Timestamp string   `json:"timestamp,omitempty"`
}

type Message struct {
	MessageID       string         `json:"messageId"`
	ContextID       string         `json:"contextId,omitempty"`
	TaskID          string         `json:"taskId,omitempty"`
	Role            string         `json:"role"`
	Parts           []Part         `json:"parts"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Extensions      []string       `json:"extensions,omitempty"`
	ReferenceTaskID []string       `json:"referenceTaskIds,omitempty"`
}

type Part struct {
	Text      *string         `json:"text,omitempty"`
	Raw       *string         `json:"raw,omitempty"`
	URL       *string         `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

type Artifact struct {
	ArtifactID  string         `json:"artifactId"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parts       []Part         `json:"parts"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Extensions  []string       `json:"extensions,omitempty"`
}

// StreamResponse mirrors the A2A v1.0 streaming response oneof. Exactly one
// member must be populated by an adapter operation.
type StreamResponse struct {
	Task           *Task                    `json:"task,omitempty"`
	Message        *Message                 `json:"message,omitempty"`
	StatusUpdate   *TaskStatusUpdateEvent   `json:"statusUpdate,omitempty"`
	ArtifactUpdate *TaskArtifactUpdateEvent `json:"artifactUpdate,omitempty"`
}

type TaskStatusUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type TaskArtifactUpdateEvent struct {
	TaskID    string         `json:"taskId"`
	ContextID string         `json:"contextId"`
	Artifact  Artifact       `json:"artifact"`
	Append    bool           `json:"append"`
	LastChunk bool           `json:"lastChunk"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}
