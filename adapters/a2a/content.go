package a2a

import (
	"encoding/json"
	"errors"
	"fmt"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

func AROPContentToParts(content []runwire.AROPV1ContentPart) ([]Part, []MappingItem, error) {
	if len(content) == 0 {
		return nil, nil, errors.New("AROP content is empty")
	}
	parts := make([]Part, 0, len(content))
	items := []MappingItem{}
	for index, source := range content {
		switch {
		case source.Text != nil:
			text := source.Text.Text
			parts = append(parts, Part{Text: &text, MediaType: "text/plain"})
			items = append(items, MappingItem{Source: fmt.Sprintf("ContentPart[%d].text", index), Target: fmt.Sprintf("Part[%d].text", index), Level: Exact})
		case source.Json != nil:
			if !json.Valid(source.Json.Value) {
				return nil, nil, errors.New("invalid AROP JSON content")
			}
			parts = append(parts, Part{Data: append(json.RawMessage(nil), source.Json.Value...), MediaType: "application/json"})
			items = append(items, MappingItem{Source: fmt.Sprintf("ContentPart[%d].json", index), Target: fmt.Sprintf("Part[%d].data", index), Level: Exact})
		case source.AssetRef != nil:
			wire, err := json.Marshal(source.AssetRef.Asset)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, Part{Data: wire, Filename: source.AssetRef.Asset.Name, MediaType: source.AssetRef.Asset.MediaType, Metadata: map[string]any{"aropPartType": "asset_ref"}})
			items = append(items, MappingItem{Source: fmt.Sprintf("ContentPart[%d].asset_ref", index), Target: fmt.Sprintf("Part[%d].data", index), Level: Extended, Reason: "brokered AROP AssetRef remains structured data; no URL is fabricated"})
		case source.DataRef != nil:
			wire, err := json.Marshal(source.DataRef.Data)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, Part{Data: wire, MediaType: "application/json", Metadata: map[string]any{"aropPartType": "data_ref"}})
			items = append(items, MappingItem{Source: fmt.Sprintf("ContentPart[%d].data_ref", index), Target: fmt.Sprintf("Part[%d].data", index), Level: Extended, Reason: "AROP actions and expiry remain in structured data"})
		default:
			return nil, nil, errors.New("AROP content part has no variant")
		}
	}
	return parts, items, nil
}

func A2APartsToAROP(parts []Part) ([]runwire.AROPV1ContentPart, []MappingItem, error) {
	if len(parts) == 0 {
		return nil, nil, errors.New("A2A parts are empty")
	}
	result := make([]runwire.AROPV1ContentPart, 0, len(parts))
	items := []MappingItem{}
	for index, part := range parts {
		variants := 0
		if part.Text != nil {
			variants++
		}
		if part.Raw != nil {
			variants++
		}
		if part.URL != nil {
			variants++
		}
		if part.Data != nil {
			variants++
		}
		if variants != 1 {
			return nil, nil, fmt.Errorf("A2A Part[%d] must select exactly one content variant", index)
		}
		switch {
		case part.Text != nil:
			result = append(result, runwire.AROPV1ContentPart{Text: &runwire.AROPV1ContentPartText{Type: "text", Text: *part.Text}})
			items = append(items, MappingItem{Source: fmt.Sprintf("Part[%d].text", index), Target: fmt.Sprintf("ContentPart[%d].text", index), Level: Exact})
		case part.Data != nil:
			if !json.Valid(part.Data) {
				return nil, nil, errors.New("invalid A2A data part")
			}
			result = append(result, runwire.AROPV1ContentPart{Json: &runwire.AROPV1ContentPartJson{Type: "json", Value: append(json.RawMessage(nil), part.Data...)}})
			level := Exact
			reason := ""
			if part.Metadata != nil && part.Metadata["aropPartType"] != nil {
				level = Lossy
				reason = "typed AROP reference cannot be reconstructed without generated union internals"
			}
			items = append(items, MappingItem{Source: fmt.Sprintf("Part[%d].data", index), Target: fmt.Sprintf("ContentPart[%d].json", index), Level: level, Reason: reason})
		case part.Raw != nil:
			return nil, nil, fmt.Errorf("A2A Part[%d].raw is unsupported; import through the Asset Broker", index)
		case part.URL != nil:
			return nil, nil, fmt.Errorf("A2A Part[%d].url is unsupported; untrusted URLs are not fetched by the mapper", index)
		}
	}
	return result, items, nil
}

func resultArtifacts(source runwire.RunResult) ([]Artifact, []MappingItem, error) {
	artifacts := []Artifact{}
	items := []MappingItem{}
	if source.Snapshot != nil {
		parts, mapped, err := AROPContentToParts(source.Snapshot.Content)
		if err != nil {
			return nil, nil, err
		}
		artifacts = append(artifacts, Artifact{ArtifactID: "snapshot", Name: "AROP final snapshot", Parts: parts, Metadata: map[string]any{"digest": source.Snapshot.Digest, "revision": source.Snapshot.Revision}})
		items = append(items, mapped...)
		items = append(items, MappingItem{Source: "RunResult.snapshot", Target: "Task.artifacts[snapshot]", Level: Extended, Reason: "snapshot digest and revision remain AROP metadata"})
	}
	if source.ResultRef != nil {
		wire, err := json.Marshal(source.ResultRef)
		if err != nil {
			return nil, nil, err
		}
		artifacts = append(artifacts, Artifact{ArtifactID: "result-ref", Name: "AROP result reference", Parts: []Part{{Data: wire, MediaType: "application/json", Metadata: map[string]any{"aropPartType": "data_ref"}}}})
		items = append(items, MappingItem{Source: "RunResult.result_ref", Target: "Task.artifacts[result-ref]", Level: Extended, Reason: "controlled result actions and expiry remain structured data"})
	}
	return artifacts, items, nil
}
