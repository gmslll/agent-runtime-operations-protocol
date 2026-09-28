package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
)

func main() {
	response := runner.DriverResponse{Protocol: runner.DriverProtocol, Outcome: "fail"}
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 20<<20))
	if err == nil {
		var invocation runner.Invocation
		err = protocolcore.DecodeAuthoring(input, &invocation)
		response.ScenarioID = invocation.ScenarioID
		if err == nil {
			err = execute(invocation)
		}
	}
	if err == nil {
		response.Outcome = "pass"
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	if err != nil {
		os.Exit(1)
	}
}

func execute(invocation runner.Invocation) error {
	if invocation.Protocol != runner.DriverProtocol || invocation.ScenarioID == "" || invocation.Fixture.Bytes <= 0 {
		return fmt.Errorf("invalid invocation")
	}
	fixture, err := base64.StdEncoding.Strict().DecodeString(invocation.Fixture.DataBase64)
	if err != nil || int64(len(fixture)) != invocation.Fixture.Bytes {
		return fmt.Errorf("invalid fixture")
	}
	sum := sha256.Sum256(fixture)
	if "sha256:"+hex.EncodeToString(sum[:]) != invocation.Fixture.SHA256 {
		return fmt.Errorf("fixture digest mismatch")
	}
	operation, _ := invocation.Request["operation"].(string)
	switch operation {
	case "manifest.validate":
		return manifest.Validate(fixture)
	case "event-envelope.validate":
		_, err := eventwire.DecodeEventEnvelope(fixture)
		return err
	case "trace-context.validate":
		var vectors struct {
			ResourceIDs json.RawMessage `json:"resource_ids"`
			Traceparent struct {
				Accepted []string `json:"accepted"`
				Rejected []string `json:"rejected"`
			} `json:"traceparent"`
			Tracestate json.RawMessage `json:"tracestate"`
		}
		if err := protocolcore.DecodeAuthoring(fixture, &vectors); err != nil {
			return err
		}
		for _, value := range vectors.Traceparent.Accepted {
			if err := protocolcore.ValidateTraceParent(value); err != nil {
				return err
			}
		}
		for _, value := range vectors.Traceparent.Rejected {
			if protocolcore.ValidateTraceParent(value) == nil {
				return fmt.Errorf("invalid traceparent accepted")
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported operation")
	}
}
