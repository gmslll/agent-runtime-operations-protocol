// Package ard maps public AROP Agent versions to Agentic Resource Discovery
// entries. Discovery output is never treated as runtime readiness or authority.
package ard

import "encoding/json"

const (
	SpecVersion        = "0.91"
	BaseContext        = "https://agenticresourcediscovery.org/context/v1"
	AgentCardMediaType = "application/a2a-agent-card+json"
	SchemaCommit       = "b76f235a8f461876ad4f1e77abd0eb0eb302b48d"
	SchemaSHA256       = "011b86d55fd5d2883dffae3f0577d26f5efb56ca866eb079edbc78a628f95499"
)

type Entry struct {
	Context               any                        `json:"@context,omitempty"`
	ID                    string                     `json:"@id,omitempty"`
	Identifier            string                     `json:"identifier"`
	DisplayName           string                     `json:"displayName"`
	Type                  string                     `json:"type"`
	URL                   string                     `json:"url,omitempty"`
	Data                  map[string]json.RawMessage `json:"data,omitempty"`
	RepresentativeQueries []string                   `json:"representativeQueries,omitempty"`
	Capabilities          []string                   `json:"capabilities,omitempty"`
	Description           string                     `json:"description,omitempty"`
	Tags                  []string                   `json:"tags,omitempty"`
	Version               string                     `json:"version,omitempty"`
	UpdatedAt             string                     `json:"updatedAt,omitempty"`
	Metadata              map[string]any             `json:"metadata,omitempty"`
	TrustManifest         json.RawMessage            `json:"TrustManifest,omitempty"`
	Extensions            map[string]json.RawMessage `json:"-"`
}

type LossItem struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Level  string `json:"level"`
	Reason string `json:"reason,omitempty"`
}

type LossReport struct {
	SourceProtocol string     `json:"source_protocol"`
	TargetProtocol string     `json:"target_protocol"`
	SpecVersion    string     `json:"spec_version"`
	Overall        string     `json:"overall"`
	SourceDigest   string     `json:"source_digest"`
	Items          []LossItem `json:"items"`
	NotExported    []string   `json:"not_exported"`
}

type Candidate struct {
	AgentID          string                     `json:"agent_id"`
	Version          string                     `json:"version"`
	Name             string                     `json:"name"`
	Summary          string                     `json:"summary"`
	Capabilities     []string                   `json:"capabilities"`
	Tags             []string                   `json:"tags,omitempty"`
	A2AAgentCardURL  string                     `json:"a2a_agent_card_url,omitempty"`
	A2AAgentCardData map[string]json.RawMessage `json:"a2a_agent_card_data,omitempty"`
	SourceIdentifier string                     `json:"source_identifier"`
	SourceDigest     string                     `json:"source_digest"`
	RuntimeReady     bool                       `json:"runtime_ready"`
	RequiresReview   bool                       `json:"requires_review"`
	TrustVerified    bool                       `json:"trust_verified"`
	Extensions       map[string]json.RawMessage `json:"extensions,omitempty"`
}

type ImportDisposition string

const (
	CandidateNew       ImportDisposition = "new"
	CandidateUnchanged ImportDisposition = "unchanged"
)
