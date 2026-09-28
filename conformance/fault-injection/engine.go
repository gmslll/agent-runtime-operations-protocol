// Package faultinjection provides the deterministic, test-only fault driver used
// by the AROP conformance suite. It is deliberately not linked into production
// binaries and exposes no network control surface.
package faultinjection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	protocolcore "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
)

const (
	CheckpointRequestAccepted Checkpoint = "platform.request.accepted"
	CheckpointBeforeUseCase   Checkpoint = "platform.before-use-case"
	CheckpointAfterUseCase    Checkpoint = "platform.after-use-case"
)

type Checkpoint string

func (checkpoint Checkpoint) Validate() error {
	switch checkpoint {
	case CheckpointRequestAccepted, CheckpointBeforeUseCase, CheckpointAfterUseCase:
		return nil
	default:
		return fmt.Errorf("unknown fault checkpoint %q", checkpoint)
	}
}

type Action string

const (
	ActionDuplicate Action = "duplicate"
	ActionDrop      Action = "drop"
	ActionReorder   Action = "reorder"
	ActionDelay     Action = "delay"
	ActionCrash     Action = "crash"
	ActionPartition Action = "partition"
)

func (action Action) Validate() error {
	switch action {
	case ActionDuplicate, ActionDrop, ActionReorder, ActionDelay, ActionCrash, ActionPartition:
		return nil
	default:
		return fmt.Errorf("unknown fault action %q", action)
	}
}

type Step struct {
	Action     Action     `json:"action"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Occurrence int        `json:"occurrence"`
	Target     string     `json:"target,omitempty"`
	DelayMS    int64      `json:"delay_ms,omitempty"`
}

type Plan struct {
	SchemaVersion int    `json:"schema_version"`
	Seed          uint64 `json:"seed"`
	Steps         []Step `json:"steps"`
}

func DecodePlan(data []byte) (Plan, error) {
	var plan Plan
	if err := protocolcore.DecodeAuthoring(data, &plan); err != nil {
		return Plan{}, fmt.Errorf("decode fault plan: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (plan Plan) Validate() error {
	if plan.SchemaVersion != 1 || plan.Seed == 0 || len(plan.Steps) == 0 || len(plan.Steps) > 128 {
		return errors.New("fault plan requires schema_version=1, non-zero seed, and 1..128 steps")
	}
	seen := map[string]bool{}
	for index, step := range plan.Steps {
		if err := step.Action.Validate(); err != nil {
			return fmt.Errorf("step %d: %w", index, err)
		}
		if err := step.Checkpoint.Validate(); err != nil {
			return fmt.Errorf("step %d: %w", index, err)
		}
		if step.Occurrence < 1 || step.Occurrence > 1_000_000 {
			return fmt.Errorf("step %d has invalid occurrence", index)
		}
		if step.Target != "" && step.Target != "primary" && step.Target != "replica-a" && step.Target != "replica-b" {
			return fmt.Errorf("step %d has unknown target", index)
		}
		if step.Action == ActionDelay {
			if step.DelayMS < 1 || step.DelayMS > 60_000 {
				return fmt.Errorf("step %d has invalid delay", index)
			}
		} else if step.DelayMS != 0 {
			return fmt.Errorf("step %d sets delay_ms for a non-delay action", index)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", step.Action, step.Checkpoint, step.Occurrence)
		if seen[key] {
			return fmt.Errorf("step %d duplicates an action/checkpoint/occurrence", index)
		}
		seen[key] = true
	}
	return nil
}

type Fault struct {
	Action Action
	Target string
}

func (fault Fault) Error() string { return "deterministic fault: " + string(fault.Action) }

type TimelineEntry struct {
	Index      int        `json:"index"`
	At         string     `json:"at"`
	Checkpoint Checkpoint `json:"checkpoint"`
	Action     Action     `json:"action"`
	Target     string     `json:"target"`
	EventID    string     `json:"event_id"`
}

type Adapter struct {
	mu         sync.Mutex
	plan       Plan
	clock      time.Time
	occurrence map[Checkpoint]int
	timeline   []TimelineEntry
	ids        uint64
}

func NewAdapter(plan Plan) (*Adapter, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return &Adapter{
		plan:       plan,
		clock:      time.Date(2026, 1, 1, 0, 0, 0, int(plan.Seed%1_000_000)*1_000, time.UTC),
		occurrence: map[Checkpoint]int{},
	}, nil
}

// Check has the same boundary as the Reference FaultHook. The adapter is kept
// in the conformance-only tree so production always uses the no-op hook.
func (adapter *Adapter) Check(ctx context.Context, checkpoint Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.occurrence[checkpoint]++
	for _, step := range adapter.plan.Steps {
		if step.Checkpoint != checkpoint || step.Occurrence != adapter.occurrence[checkpoint] {
			continue
		}
		if step.Action == ActionDelay {
			adapter.clock = adapter.clock.Add(time.Duration(step.DelayMS) * time.Millisecond)
		}
		adapter.ids++
		target := step.Target
		if target == "" {
			target = "primary"
		}
		adapter.timeline = append(adapter.timeline, TimelineEntry{
			Index: len(adapter.timeline) + 1, At: adapter.clock.Format(time.RFC3339Nano),
			Checkpoint: checkpoint, Action: step.Action, Target: target,
			EventID: deterministicID(adapter.plan.Seed, adapter.ids),
		})
		if step.Action != ActionDelay {
			return Fault{Action: step.Action, Target: target}
		}
	}
	return nil
}

func (adapter *Adapter) Timeline() []TimelineEntry {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return append([]TimelineEntry(nil), adapter.timeline...)
}

func (adapter *Adapter) Digest() string {
	data, _ := json.Marshal(adapter.Timeline())
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deterministicID(seed, sequence uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("arop-p35:%d:%d", seed, sequence)))
	return "fault_" + hex.EncodeToString(sum[:12])
}

type Node struct {
	ID          string `json:"id"`
	Available   bool   `json:"available"`
	Partitioned bool   `json:"partitioned"`
	Applied     int    `json:"applied"`
}

type Simulation struct {
	Primary  string          `json:"primary"`
	Nodes    []Node          `json:"nodes"`
	Applied  []string        `json:"applied"`
	Timeline []TimelineEntry `json:"timeline"`
	Digest   string          `json:"digest"`
}

// Simulate exercises the six fault classes without introducing timing races.
// It models the invariant the real HA driver checks: committed operations are
// idempotent, ordered, replayable, and available after deterministic promotion.
func Simulate(plan Plan) (Simulation, error) {
	adapter, err := NewAdapter(plan)
	if err != nil {
		return Simulation{}, err
	}
	nodes := map[string]*Node{
		"primary": {ID: "primary", Available: true}, "replica-a": {ID: "replica-a", Available: true}, "replica-b": {ID: "replica-b", Available: true},
	}
	primary := "primary"
	applied := map[string]bool{}
	ordered := []string{}
	pending := []string{}
	checkpoints := []Checkpoint{CheckpointRequestAccepted, CheckpointBeforeUseCase, CheckpointAfterUseCase}
	for round := 0; round < 4; round++ {
		id := deterministicID(plan.Seed, uint64(round+100))
		for _, checkpoint := range checkpoints {
			faultErr := adapter.Check(context.Background(), checkpoint)
			var fault Fault
			if errors.As(faultErr, &fault) {
				target := fault.Target
				switch fault.Action {
				case ActionCrash:
					nodes[target].Available = false
					if target == primary {
						primary = firstHealthy(nodes)
					}
				case ActionPartition:
					nodes[target].Partitioned = true
				case ActionDrop:
					pending = append(pending, id)
					continue
				case ActionReorder:
					pending = append([]string{id}, pending...)
					continue
				case ActionDuplicate:
					apply(id, applied, &ordered)
					apply(id, applied, &ordered)
				}
			} else if faultErr != nil {
				return Simulation{}, faultErr
			}
		}
		if primary == "" {
			return Simulation{}, errors.New("no promotable node")
		}
		apply(id, applied, &ordered)
	}
	for _, node := range nodes {
		node.Partitioned = false
		if node.Available {
			node.Applied = len(applied)
		}
	}
	for _, id := range pending {
		apply(id, applied, &ordered)
	}
	sort.Strings(ordered)
	resultNodes := []Node{*nodes["primary"], *nodes["replica-a"], *nodes["replica-b"]}
	timeline := adapter.Timeline()
	canonical, _ := json.Marshal(struct {
		Primary string
		Nodes   []Node
		Applied []string
		Events  []TimelineEntry
	}{primary, resultNodes, ordered, timeline})
	digest := sha256.Sum256(canonical)
	return Simulation{Primary: primary, Nodes: resultNodes, Applied: ordered, Timeline: timeline, Digest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func firstHealthy(nodes map[string]*Node) string {
	for _, id := range []string{"replica-a", "replica-b", "primary"} {
		if nodes[id].Available && !nodes[id].Partitioned {
			return id
		}
	}
	return ""
}

func apply(id string, seen map[string]bool, ordered *[]string) {
	if !seen[id] {
		seen[id] = true
		*ordered = append(*ordered, id)
	}
}

type FaultHook interface {
	Check(context.Context, Checkpoint) error
}

var _ FaultHook = (*Adapter)(nil)
