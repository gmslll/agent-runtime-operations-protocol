package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

var bearerRequirement = SecurityRequirement{Schemes: map[string]StringList{"arop_bearer": {List: []string{}}}}

func ExportAgentCard(source controlplane.AgentManifest, interfaceURL string) (AgentCard, MappingReport, error) {
	endpoint, err := url.Parse(interfaceURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return AgentCard{}, MappingReport{}, errors.New("A2A interface must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if source.Protocol != "arop/v1" || source.Kind != "AgentManifest" || len(source.Skills) == 0 {
		return AgentCard{}, MappingReport{}, errors.New("unsupported AROP manifest")
	}
	description := source.IDentity.Summary
	if source.IDentity.Description != nil {
		description = *source.IDentity.Description
	}
	inputModes, outputModes := manifestModes(source.Content)
	streaming := source.Execution.Capabilities.Streaming != nil && *source.Execution.Capabilities.Streaming
	card := AgentCard{
		Name: source.IDentity.Name, Description: description, Version: string(source.IDentity.Version),
		SupportedInterfaces:  []AgentInterface{{URL: endpoint.String(), ProtocolBinding: "HTTP+JSON", ProtocolVersion: ProtocolVersion}},
		Capabilities:         AgentCapabilities{Streaming: streaming},
		SecuritySchemes:      map[string]SecurityScheme{"arop_bearer": {HTTPAuthSecurityScheme: &HTTPAuthSecurityScheme{Scheme: "Bearer", BearerFormat: "JWT"}}},
		SecurityRequirements: []SecurityRequirement{bearerRequirement}, DefaultInputModes: inputModes, DefaultOutputModes: outputModes,
		Skills: make([]AgentSkill, 0, len(source.Skills)),
	}
	tags := manifestTags(source)
	for _, skill := range source.Skills {
		summary := skill.Name
		if skill.Summary != nil {
			summary = *skill.Summary
		}
		card.Skills = append(card.Skills, AgentSkill{ID: string(skill.ID), Name: skill.Name, Description: summary, Tags: append([]string(nil), tags...), InputModes: append([]string(nil), inputModes...), OutputModes: append([]string(nil), outputModes...), SecurityRequirements: []SecurityRequirement{bearerRequirement}})
	}
	items := []MappingItem{
		{Source: "AgentManifest.identity", Target: "AgentCard.name/description/version", Level: Exact},
		{Source: "AgentManifest.skills", Target: "AgentCard.skills", Level: Extended, Reason: "AROP JSON Schemas remain referenced by the original signed package"},
		{Source: "AgentManifest.execution.capabilities.streaming", Target: "AgentCard.capabilities.streaming", Level: Exact},
		{Source: "AROP Control Plane authentication", Target: "AgentCard.securitySchemes.arop_bearer", Level: Extended, Reason: "credential issuance and enterprise scopes remain AROP-governed"},
		{Source: "Runtime Registry/Deployment routing", Target: "AgentCard.supportedInterfaces", Level: Lossy, Reason: "the configured public adapter URL replaces private runtime topology"},
	}
	report, err := newReport("AgentManifest", string(source.IDentity.ID)+"@"+string(source.IDentity.Version), source, items)
	if err != nil {
		return AgentCard{}, MappingReport{}, err
	}
	report.UnmappedSecurity = []string{"authorization_snapshot", "dispatch_ticket", "run_token", "runtime_lease", "deployment_routing", "effect_reservation"}
	return card, report, nil
}

func ExportTask(source runwire.RunStatus) (Task, MappingReport, error) {
	state, level, reason, err := mapRunState(source.State)
	if err != nil {
		return Task{}, MappingReport{}, err
	}
	task := Task{ID: string(source.RunID), Status: TaskStatus{State: state, Timestamp: string(source.UpdatedAt)}, Metadata: map[string]any{"arop": map[string]any{"protocol": "arop/v1", "runId": source.RunID, "stateVersion": source.StateVersion, "traceparent": source.Trace.Traceparent}}}
	items := []MappingItem{{Source: "Run.run_id", Target: "Task.id", Level: Exact}, {Source: "Run.state", Target: "Task.status.state", Level: level, Reason: reason}, {Source: "Run.trace", Target: "Task.metadata.arop.traceparent", Level: Extended, Reason: "A2A has no core trace field"}}
	if source.Result != nil {
		artifacts, artifactItems, mapErr := resultArtifacts(*source.Result)
		if mapErr != nil {
			return Task{}, MappingReport{}, mapErr
		}
		task.Artifacts = artifacts
		items = append(items, artifactItems...)
		if source.Result.Error != nil {
			message := source.Result.Error.Message
			task.Status.Message = &Message{MessageID: "arop-status-" + string(source.RunID), TaskID: string(source.RunID), Role: "ROLE_AGENT", Parts: []Part{{Text: &message, MediaType: "text/plain"}}, Metadata: map[string]any{"aropErrorCode": source.Result.Error.Code, "retryable": source.Result.Error.Retryable}}
			items = append(items, MappingItem{Source: "RunResult.error", Target: "Task.status.message", Level: Extended, Reason: "typed AROP error fields are preserved in metadata"})
		}
	}
	report, err := newReport("RunStatus", string(source.RunID), source, items)
	if err != nil {
		return Task{}, MappingReport{}, err
	}
	report.UnmappedSecurity = []string{"authorization_snapshot_digest", "physical_attempt", "effect_id", "first_terminal_state_fencing"}
	return task, report, nil
}

func mapRunState(state string) (string, MappingLevel, string, error) {
	switch state {
	case "queued":
		return "TASK_STATE_SUBMITTED", Exact, "", nil
	case "dispatching":
		return "TASK_STATE_SUBMITTED", Extended, "physical dispatch has no A2A task state", nil
	case "running":
		return "TASK_STATE_WORKING", Exact, "", nil
	case "waiting_input":
		return "TASK_STATE_INPUT_REQUIRED", Exact, "", nil
	case "cancel_requested":
		return "TASK_STATE_WORKING", Extended, "cancel request is not cancellation confirmation", nil
	case "succeeded":
		return "TASK_STATE_COMPLETED", Exact, "", nil
	case "failed":
		return "TASK_STATE_FAILED", Exact, "", nil
	case "cancelled":
		return "TASK_STATE_CANCELED", Exact, "", nil
	case "timed_out":
		return "TASK_STATE_FAILED", Lossy, "A2A 1.0 has no distinct timed-out terminal state", nil
	default:
		return "", Unsupported, "", fmt.Errorf("unsupported AROP run state %q", state)
	}
}

func newReport(kind, id string, source any, items []MappingItem) (MappingReport, error) {
	wire, err := json.Marshal(source)
	if err != nil {
		return MappingReport{}, err
	}
	digest := sha256.Sum256(wire)
	overall := Exact
	for _, item := range items {
		if rank(item.Level) > rank(overall) {
			overall = item.Level
		}
	}
	return MappingReport{AdapterVersion: AdapterVersion, SourceProtocol: "arop/v1", TargetProtocol: "a2a/1.0", FixtureRelease: FixtureRelease, Overall: overall, Original: OriginalObjectRef{Protocol: "arop", Version: "1", Kind: kind, ID: id, SHA256: "sha256:" + hex.EncodeToString(digest[:])}, Items: items}, nil
}

func rank(level MappingLevel) int {
	switch level {
	case Exact:
		return 0
	case Extended:
		return 1
	case Lossy:
		return 2
	default:
		return 3
	}
}
func manifestModes(content *controlplane.Content) ([]string, []string) {
	inputs, outputs := []string{"application/json"}, []string{"application/json"}
	if content == nil {
		return inputs, outputs
	}
	if content.AcceptedMediaTypes != nil && len(*content.AcceptedMediaTypes) != 0 {
		inputs = append([]string(nil), (*content.AcceptedMediaTypes)...)
		outputs = append([]string(nil), (*content.AcceptedMediaTypes)...)
	}
	if content.InputTypes != nil {
		inputs = contentTypeModes(*content.InputTypes)
	}
	if content.OutputTypes != nil {
		outputs = contentTypeModes(*content.OutputTypes)
	}
	return uniqueSorted(inputs), uniqueSorted(outputs)
}
func contentTypeModes(values controlplane.ContentTypes) []string {
	result := []string{}
	for _, value := range values {
		switch value {
		case "text":
			result = append(result, "text/plain")
		case "json", "data_ref":
			result = append(result, "application/json")
		case "asset_ref":
			result = append(result, "application/octet-stream")
		}
	}
	if len(result) == 0 {
		result = []string{"application/json"}
	}
	return result
}
func manifestTags(source controlplane.AgentManifest) []string {
	values := []string{"arop"}
	if source.IDentity.Categories != nil {
		for _, value := range *source.IDentity.Categories {
			values = append(values, string(value))
		}
	}
	if source.IDentity.Tags != nil {
		values = append(values, (*source.IDentity.Tags)...)
	}
	return uniqueSorted(values)
}
func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
