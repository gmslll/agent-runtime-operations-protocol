package faultinjection

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func completePlan() Plan {
	return Plan{SchemaVersion: 1, Seed: 350035, Steps: []Step{
		{Action: ActionDuplicate, Checkpoint: CheckpointRequestAccepted, Occurrence: 1, Target: "primary"},
		{Action: ActionDrop, Checkpoint: CheckpointBeforeUseCase, Occurrence: 1, Target: "replica-a"},
		{Action: ActionReorder, Checkpoint: CheckpointAfterUseCase, Occurrence: 1, Target: "replica-b"},
		{Action: ActionDelay, Checkpoint: CheckpointRequestAccepted, Occurrence: 2, DelayMS: 250},
		{Action: ActionPartition, Checkpoint: CheckpointBeforeUseCase, Occurrence: 2, Target: "replica-a"},
		{Action: ActionCrash, Checkpoint: CheckpointAfterUseCase, Occurrence: 2, Target: "primary"},
	}}
}

func TestSameSeedReproducesTimelineNodesAndDigest(t *testing.T) {
	first, err := Simulate(completePlan())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Simulate(completePlan())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !strings.HasPrefix(first.Digest, "sha256:") || first.Primary != "replica-b" {
		t.Fatalf("simulation is not reproducible: %+v %+v", first, second)
	}
	if len(first.Timeline) != 6 || len(first.Applied) != 4 {
		t.Fatalf("unexpected timeline/applied closure: %+v", first)
	}
}

func TestAllFaultClassesRemainIdempotentReplayableAndOrdered(t *testing.T) {
	result, err := Simulate(completePlan())
	if err != nil {
		t.Fatal(err)
	}
	actions := map[Action]bool{}
	for _, item := range result.Timeline {
		actions[item.Action] = true
	}
	for _, action := range []Action{ActionDuplicate, ActionDrop, ActionReorder, ActionDelay, ActionCrash, ActionPartition} {
		if !actions[action] {
			t.Fatalf("missing fault class %s", action)
		}
	}
	seen := map[string]bool{}
	for _, id := range result.Applied {
		if seen[id] {
			t.Fatalf("duplicate application %s", id)
		}
		seen[id] = true
	}
	for _, node := range result.Nodes {
		if node.Available && node.Applied != len(result.Applied) {
			t.Fatalf("healthy node did not converge: %+v", node)
		}
	}
}

func TestPlanAndCheckpointValidationFailClosed(t *testing.T) {
	valid, _ := json.Marshal(completePlan())
	if _, err := DecodePlan(valid); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		[]byte(`{"schema_version":1,"seed":1,"steps":[{"action":"remote-shell","checkpoint":"platform.request.accepted","occurrence":1}]}`),
		[]byte(`{"schema_version":1,"seed":1,"steps":[{"action":"drop","checkpoint":"arbitrary.remote","occurrence":1}]}`),
		[]byte(`{"schema_version":1,"seed":1,"steps":[{"action":"delay","checkpoint":"platform.request.accepted","occurrence":1,"delay_ms":0}]}`),
		[]byte(`{"schema_version":1,"seed":1,"steps":[],"unknown":true}`),
	} {
		if _, err := DecodePlan(data); err == nil {
			t.Fatalf("invalid plan accepted: %s", data)
		}
	}
	adapter, err := NewAdapter(completePlan())
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.Check(cancelled, CheckpointRequestAccepted); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel did not fail closed: %v", err)
	}
	if err := adapter.Check(context.Background(), Checkpoint("remote.arbitrary")); err == nil {
		t.Fatal("unknown checkpoint accepted")
	}
}
