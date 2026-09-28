package a2a

import (
	"errors"
	"fmt"

	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

func DecodeTask(document []byte) (Task, error) {
	var value Task
	if err := decodeStrict(document, &value); err != nil {
		return Task{}, err
	}
	if value.ID == "" || value.Status.State == "" {
		return Task{}, errors.New("A2A task is incomplete")
	}
	if _, _, _, err := mapA2AState(value.Status.State); err != nil {
		return Task{}, err
	}
	return value, nil
}

func mapA2AState(state string) (string, MappingLevel, string, error) {
	switch state {
	case "TASK_STATE_SUBMITTED":
		return "queued", Exact, "", nil
	case "TASK_STATE_WORKING":
		return "running", Exact, "", nil
	case "TASK_STATE_COMPLETED":
		return "succeeded", Exact, "", nil
	case "TASK_STATE_FAILED":
		return "failed", Exact, "", nil
	case "TASK_STATE_CANCELED":
		return "cancelled", Exact, "", nil
	case "TASK_STATE_INPUT_REQUIRED":
		return "waiting_input", Exact, "", nil
	case "TASK_STATE_REJECTED":
		return "failed", Lossy, "AROP has no rejected terminal state", nil
	case "TASK_STATE_AUTH_REQUIRED":
		return "waiting_input", Lossy, "AROP represents authentication continuation as governed input", nil
	default:
		return "", Unsupported, "", fmt.Errorf("unsupported A2A task state %q", state)
	}
}

func decodeStrict(document []byte, destination any) error {
	return core.DecodeAuthoring(document, destination)
}
