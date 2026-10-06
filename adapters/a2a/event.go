package a2a

import (
	"encoding/json"
	"errors"
	"fmt"

	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

// ExportEvent converts an AROP event into one A2A v1.0 StreamResponse. AROP
// ordering, offsets, trace, and attempt identity are retained in metadata when
// A2A has no native field for them.
func ExportEvent(source eventwire.EventEnvelope) (StreamResponse, MappingReport, error) {
	if _, err := eventwire.EncodeEventEnvelope(source); err != nil {
		return StreamResponse{}, MappingReport{}, fmt.Errorf("invalid AROP event: %w", err)
	}
	metadata := eventMetadata(source)
	aropMetadata := metadata["arop"].(map[string]any)
	items := []MappingItem{
		{Source: "Event.runid", Target: "StreamResponse.*.taskId", Level: Exact},
		{Source: "Event.id/attemptid/sequence/trace", Target: "StreamResponse.*.metadata.arop", Level: Extended, Reason: "A2A has no native attempt, sequence, or trace envelope fields"},
	}
	var response StreamResponse
	data, err := json.Marshal(source.Data)
	if err != nil {
		return StreamResponse{}, MappingReport{}, err
	}
	switch source.Type {
	case "io.arop.output.delta.v1":
		value, decodeErr := eventwire.DecodeOutputDeltaData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		text := value.Delta
		aropMetadata["outputId"] = value.OutputID
		aropMetadata["utf8Offset"] = int64(value.Offset)
		response.ArtifactUpdate = &TaskArtifactUpdateEvent{TaskID: string(source.Runid), ContextID: string(source.Runid), Artifact: Artifact{ArtifactID: value.OutputID, Parts: []Part{{Text: &text, MediaType: "text/plain"}}}, Append: true, Metadata: metadata}
		items = append(items, MappingItem{Source: "output.delta", Target: "artifactUpdate(append=true)", Level: Extended, Reason: "AROP UTF-8 offset is retained in metadata"})
	case "io.arop.output.snapshot.v1":
		value, decodeErr := eventwire.DecodeOutputSnapshotData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		parts, mapped, mapErr := eventParts(value.Content)
		if mapErr != nil {
			return StreamResponse{}, MappingReport{}, mapErr
		}
		aropMetadata["digest"] = value.Digest
		aropMetadata["revision"] = int64(value.Revision)
		response.ArtifactUpdate = &TaskArtifactUpdateEvent{TaskID: string(source.Runid), ContextID: string(source.Runid), Artifact: Artifact{ArtifactID: value.OutputID, Parts: parts}, Append: false, Metadata: metadata}
		items = append(items, mapped...)
		items = append(items, MappingItem{Source: "output.snapshot", Target: "artifactUpdate(append=false)", Level: Extended, Reason: "AROP digest and revision are retained in metadata"})
	case "io.arop.progress.updated.v1":
		value, decodeErr := eventwire.DecodeProgressEventData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		aropMetadata["progress"] = int64(value.Progress)
		if value.StepID != nil {
			aropMetadata["stepId"] = *value.StepID
		}
		if value.State != nil {
			aropMetadata["progressState"] = *value.State
		}
		status := TaskStatus{State: "TASK_STATE_WORKING", Timestamp: string(source.Time)}
		if value.Message != nil {
			message := *value.Message
			status.Message = &Message{MessageID: string(source.ID), TaskID: string(source.Runid), ContextID: string(source.Runid), Role: "ROLE_AGENT", Parts: []Part{{Text: &message, MediaType: "text/plain"}}}
		}
		response.StatusUpdate = &TaskStatusUpdateEvent{TaskID: string(source.Runid), ContextID: string(source.Runid), Status: status, Metadata: metadata}
		items = append(items, MappingItem{Source: "progress.updated", Target: "statusUpdate(WORKING)", Level: Extended, Reason: "numeric progress and step identity are retained in metadata"})
	case "io.arop.run.started.v1", "io.arop.run.waiting_input.v1", "io.arop.run.cancel_requested.v1":
		value, decodeErr := eventwire.DecodeLifecycleStateData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		state, level, reason, mapErr := mapRunState(value.State)
		if mapErr != nil {
			return StreamResponse{}, MappingReport{}, mapErr
		}
		response.StatusUpdate = &TaskStatusUpdateEvent{TaskID: string(source.Runid), ContextID: string(source.Runid), Status: TaskStatus{State: state, Timestamp: string(source.Time)}, Metadata: metadata}
		items = append(items, MappingItem{Source: "run." + value.State, Target: "statusUpdate." + state, Level: level, Reason: reason})
	case "io.arop.run.succeeded.v1", "io.arop.run.failed.v1", "io.arop.run.cancelled.v1", "io.arop.run.timed_out.v1":
		value, decodeErr := eventwire.DecodeLifecycleTerminalData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		state, level, reason, mapErr := mapRunState(value.State)
		if mapErr != nil {
			return StreamResponse{}, MappingReport{}, mapErr
		}
		task := Task{ID: string(source.Runid), ContextID: string(source.Runid), Status: TaskStatus{State: state, Timestamp: string(source.Time)}, Metadata: metadata}
		if value.Snapshot != nil {
			parts, mapped, partErr := eventParts(value.Snapshot.Content)
			if partErr != nil {
				return StreamResponse{}, MappingReport{}, partErr
			}
			task.Artifacts = append(task.Artifacts, Artifact{ArtifactID: "snapshot", Name: "AROP final snapshot", Parts: parts, Metadata: map[string]any{"digest": value.Snapshot.Digest, "revision": value.Snapshot.Revision}})
			items = append(items, mapped...)
		}
		if value.ResultRef != nil {
			wire, marshalErr := json.Marshal(value.ResultRef)
			if marshalErr != nil {
				return StreamResponse{}, MappingReport{}, marshalErr
			}
			task.Artifacts = append(task.Artifacts, Artifact{ArtifactID: "result-ref", Name: "AROP result reference", Parts: []Part{{Data: wire, MediaType: "application/json", Metadata: map[string]any{"aropPartType": "data_ref"}}}})
		}
		if value.Error != nil {
			message := value.Error.Message
			task.Status.Message = &Message{MessageID: string(source.ID), TaskID: string(source.Runid), ContextID: string(source.Runid), Role: "ROLE_AGENT", Parts: []Part{{Text: &message, MediaType: "text/plain"}}, Metadata: map[string]any{"aropErrorCode": value.Error.Code, "retryable": value.Error.Retryable}}
		}
		response.Task = &task
		items = append(items, MappingItem{Source: "run." + value.State, Target: "Task.status." + state, Level: level, Reason: reason})
	case "io.arop.usage.updated.v1":
		value, decodeErr := eventwire.DecodeUsageEventData(data)
		if decodeErr != nil {
			return StreamResponse{}, MappingReport{}, decodeErr
		}
		aropMetadata["usage"] = value
		response.StatusUpdate = &TaskStatusUpdateEvent{TaskID: string(source.Runid), ContextID: string(source.Runid), Status: TaskStatus{State: "TASK_STATE_WORKING", Timestamp: string(source.Time)}, Metadata: metadata}
		items = append(items, MappingItem{Source: "usage.updated", Target: "statusUpdate.metadata.arop.usage", Level: Extended, Reason: "A2A v1.0 has no core usage event"})
	default:
		return StreamResponse{}, MappingReport{}, fmt.Errorf("unsupported AROP event type %q", source.Type)
	}
	if streamResponseMembers(response) != 1 {
		return StreamResponse{}, MappingReport{}, errors.New("A2A StreamResponse must contain exactly one payload")
	}
	report, err := newReport("Event", string(source.ID), source, items)
	return response, report, err
}

// ImportTaskState maps an A2A task state into AROP lifecycle semantics. It
// deliberately does not create an AROP Run: authorization and Run creation
// remain Control Plane responsibilities.
func ImportTaskState(source Task) (string, MappingLevel, string, error) {
	wire, err := json.Marshal(source)
	if err != nil {
		return "", Unsupported, "", err
	}
	if _, err := DecodeTask(wire); err != nil {
		return "", Unsupported, "", err
	}
	return mapA2AState(source.Status.State)
}

func eventMetadata(source eventwire.EventEnvelope) map[string]any {
	metadata := map[string]any{"eventId": source.ID, "attemptId": source.Attemptid, "producerSequence": source.Producersequence, "traceparent": source.Traceparent, "eventType": source.Type}
	if source.Runsequence != nil {
		metadata["runSequence"] = *source.Runsequence
	}
	return map[string]any{"arop": metadata}
}

func eventParts(content []eventwire.AROPV1ContentPart) ([]Part, []MappingItem, error) {
	converted := make([]runwire.AROPV1ContentPart, 0, len(content))
	for _, part := range content {
		wire, err := json.Marshal(part)
		if err != nil {
			return nil, nil, err
		}
		var value runwire.AROPV1ContentPart
		if err := json.Unmarshal(wire, &value); err != nil {
			return nil, nil, err
		}
		converted = append(converted, value)
	}
	return AROPContentToParts(converted)
}

func streamResponseMembers(value StreamResponse) int {
	members := 0
	if value.Task != nil {
		members++
	}
	if value.Message != nil {
		members++
	}
	if value.StatusUpdate != nil {
		members++
	}
	if value.ArtifactUpdate != nil {
		members++
	}
	return members
}
