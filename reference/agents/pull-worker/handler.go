// Package pullworker provides the vendor-neutral reference Worker Pull
// handler. It depends only on the public Go SDK and generated wire model.
package pullworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
	workersdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/worker"
)

type EchoHandler struct {
	Clock func() time.Time
}

func (handler EchoHandler) Handle(ctx context.Context, task workersdk.Task) (workersdk.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return workersdk.Outcome{}, err
	}
	content := append([]workerwire.AROPV1ContentPart(nil), task.Claim.RunRequest.Input...)
	encoded, err := json.Marshal(content)
	if err != nil {
		return workersdk.Outcome{}, errors.New("encode reference output")
	}
	digest := sha256.Sum256(encoded)
	clock := handler.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if err := ctx.Err(); err != nil {
		return workersdk.Outcome{}, err
	}
	return workersdk.Outcome{Result: workerwire.AROPV1TerminalRunResult{
		SchemaVersion: 1,
		RunID:         task.Claim.RunID,
		State:         "succeeded",
		Snapshot: &workerwire.Snapshot{
			Revision: 1,
			Content:  content,
			Digest:   workerwire.Sha256Digest("sha256:" + hex.EncodeToString(digest[:])),
		},
		Usage:       workerwire.Usage{InputTokens: 0, OutputTokens: 0, DurationMs: 0},
		CompletedAt: workerwire.DateTime(clock().UTC().Format(time.RFC3339Nano)),
	}}, nil
}

var _ workersdk.Handler = EchoHandler{}
