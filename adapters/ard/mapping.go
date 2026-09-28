package ard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

var (
	publisherPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	identifierPattern = regexp.MustCompile(`^urn:air:[A-Za-z0-9.-]+(?::[A-Za-z0-9._-]+)+$`)
	agentIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	versionPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

func Export(source controlplane.AgentManifest, manifestDigest, publisher, agentCardURL string) (Entry, LossReport, error) {
	if source.Protocol != "arop/v1" || source.Kind != "AgentManifest" || len(source.Skills) == 0 {
		return Entry{}, LossReport{}, errors.New("unsupported AROP manifest")
	}
	if !publisherPattern.MatchString(publisher) || strings.Contains(publisher, "..") {
		return Entry{}, LossReport{}, errors.New("ARD publisher is invalid")
	}
	endpoint, err := publicHTTPS(agentCardURL)
	if err != nil {
		return Entry{}, LossReport{}, err
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(manifestDigest) {
		return Entry{}, LossReport{}, errors.New("manifest digest is invalid")
	}
	description := source.IDentity.Summary
	if source.IDentity.Description != nil {
		description = *source.IDentity.Description
	}
	capabilities := make([]string, 0, len(source.Skills))
	queries := []string{}
	for _, skill := range source.Skills {
		capabilities = append(capabilities, string(skill.ID))
		summary := skill.Name
		if skill.Summary != nil {
			summary = *skill.Summary
		}
		queries = append(queries, "Use "+source.IDentity.Name+" to "+summary)
	}
	if source.Presentation != nil && source.Presentation.SuggestedPrompts != nil && len(*source.Presentation.SuggestedPrompts) != 0 {
		queries = append([]string(nil), (*source.Presentation.SuggestedPrompts)...)
	}
	queries = unique(queries)
	if len(queries) == 1 {
		queries = append(queries, "Ask "+source.IDentity.Name+" for "+source.Skills[0].Name)
	}
	if len(queries) > 5 {
		queries = queries[:5]
	}
	tags := []string{"arop", "a2a"}
	if source.IDentity.Tags != nil {
		tags = append(tags, (*source.IDentity.Tags)...)
	}
	if source.IDentity.Categories != nil {
		for _, item := range *source.IDentity.Categories {
			tags = append(tags, string(item))
		}
	}
	entry := Entry{Context: BaseContext, Identifier: "urn:air:" + strings.ToLower(publisher) + ":arop:" + string(source.IDentity.ID), DisplayName: source.IDentity.Name, Type: AgentCardMediaType, URL: endpoint.String(), RepresentativeQueries: queries, Capabilities: unique(capabilities), Description: description, Tags: unique(tags), Version: string(source.IDentity.Version), Metadata: map[string]any{"aropProtocol": "arop/v1", "aropManifestDigest": manifestDigest}}
	items := []LossItem{
		{Source: "AgentManifest.identity", Target: "ARD identity/display/version", Level: "exact"},
		{Source: "AgentManifest.skills", Target: "ARD capabilities/representativeQueries", Level: "extended", Reason: "input/output schemas remain in the referenced signed Agent Card package"},
		{Source: "public A2A Agent Card", Target: "ARD url", Level: "exact"},
		{Source: "runtime/deployment state", Target: "none", Level: "unsupported", Reason: "discovery never publishes readiness, instance topology, leases, capacity, or credentials"},
	}
	report := LossReport{SourceProtocol: "arop/v1", TargetProtocol: "ard/0.91", SpecVersion: SpecVersion, Overall: "unsupported", SourceDigest: manifestDigest, Items: items, NotExported: []string{"runtime_instances", "runtime_endpoints", "lease", "health", "readiness", "capacity", "deployment_credentials", "authorization_snapshot", "internal_labels"}}
	return entry, report, nil
}

func Import(document []byte, existing *Candidate) (Candidate, ImportDisposition, error) {
	entry, sourceDigest, err := DecodeEntry(document)
	if err != nil {
		return Candidate{}, "", err
	}
	parts := strings.Split(entry.Identifier, ":")
	if len(parts) < 5 {
		return Candidate{}, "", errors.New("ARD identifier is incomplete")
	}
	agentID := parts[len(parts)-1]
	if len(agentID) > 64 || !agentIDPattern.MatchString(agentID) {
		return Candidate{}, "", errors.New("ARD identifier cannot be represented as an AROP agent_id")
	}
	if !versionPattern.MatchString(entry.Version) || len(entry.Version) > 128 {
		return Candidate{}, "", errors.New("ARD entry version is required for AROP import")
	}
	candidate := Candidate{AgentID: agentID, Version: entry.Version, Name: entry.DisplayName, Summary: entry.Description, Capabilities: append([]string(nil), entry.Capabilities...), Tags: append([]string(nil), entry.Tags...), A2AAgentCardURL: entry.URL, A2AAgentCardData: cloneRawMap(entry.Data), SourceIdentifier: entry.Identifier, SourceDigest: sourceDigest, RuntimeReady: false, RequiresReview: true, TrustVerified: false, Extensions: cloneRawMap(entry.Extensions)}
	if existing == nil {
		return candidate, CandidateNew, nil
	}
	if existing.AgentID != candidate.AgentID || existing.Version != candidate.Version {
		return Candidate{}, "", errors.New("existing candidate identity does not match")
	}
	if existing.SourceDigest != candidate.SourceDigest {
		return Candidate{}, "", errors.New("ARD version conflict: immutable version has different source digest")
	}
	return candidate, CandidateUnchanged, nil
}

func publicHTTPS(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("ARD artifact URL must be absolute HTTPS without credentials, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, errors.New("ARD artifact URL host is not public")
	}
	if address := net.ParseIP(host); address != nil && (address.IsPrivate() || address.IsLoopback() || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast()) {
		return nil, errors.New("ARD artifact URL address is not public")
	}
	return parsed, nil
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, item := range values {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	sort.Strings(result)
	return result
}

func digest(document []byte) (string, error) {
	canonical, err := jsoncanonicalizer.Transform(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}
func validateEntry(entry Entry) error {
	if !identifierPattern.MatchString(entry.Identifier) || entry.DisplayName == "" || entry.Type != AgentCardMediaType {
		return errors.New("unsupported ARD entry")
	}
	if (entry.URL == "") == (entry.Data == nil) {
		return errors.New("ARD entry must contain exactly one of url or data")
	}
	if entry.URL != "" {
		if _, err := publicHTTPS(entry.URL); err != nil {
			return err
		}
	}
	if entry.ID != "" && entry.ID != entry.Identifier {
		return fmt.Errorf("ARD @id and identifier differ")
	}
	if err := validateContext(entry.Context); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, capability := range entry.Capabilities {
		if capability == "" || seen[capability] {
			return errors.New("ARD capabilities are invalid")
		}
		seen[capability] = true
	}
	for _, value := range entry.Metadata {
		switch value.(type) {
		case nil, string, bool, json.Number, float64:
		default:
			return errors.New("ARD metadata values must be scalar")
		}
	}
	if len(entry.TrustManifest) != 0 {
		var trust struct {
			Identity string `json:"identity"`
		}
		if err := json.Unmarshal(entry.TrustManifest, &trust); err != nil || trust.Identity == "" {
			return errors.New("ARD TrustManifest identity is invalid")
		}
		publisher := strings.Split(entry.Identifier, ":")[2]
		if !trustIdentityMatchesPublisher(trust.Identity, publisher) {
			return errors.New("ARD trust identity does not match publisher")
		}
	}
	return nil
}

func validateContext(value any) error {
	if value == nil {
		return nil
	}
	switch item := value.(type) {
	case string:
		if item != BaseContext {
			return errors.New("ARD context does not include the pinned base context")
		}
	case []any:
		found := false
		for _, child := range item {
			if text, ok := child.(string); ok && text == BaseContext {
				found = true
			}
		}
		if !found {
			return errors.New("ARD context does not include the pinned base context")
		}
	case map[string]any:
		return errors.New("ARD context object cannot safely establish the pinned base vocabulary")
	default:
		return errors.New("ARD context is invalid")
	}
	return nil
}

func trustIdentityMatchesPublisher(identity, publisher string) bool {
	if parsed, err := url.Parse(identity); err == nil && (parsed.Scheme == "https" || parsed.Scheme == "spiffe") {
		return strings.EqualFold(parsed.Hostname(), publisher)
	}
	if strings.HasPrefix(identity, "did:web:") {
		return strings.EqualFold(strings.Split(strings.TrimPrefix(identity, "did:web:"), ":")[0], publisher)
	}
	return false
}
