package a2a

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

func TestCompatibilityPinAndStrictUpstreamFixtures(t *testing.T) {
	t.Parallel()
	cardWire, err := os.ReadFile("testdata/upstream-v1.0.1-agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	if err := decodeStrict(cardWire, &card); err != nil {
		t.Fatal(err)
	}
	if card.SupportedInterfaces[0].ProtocolVersion != ProtocolVersion || card.Skills[0].ID != "answer" {
		t.Fatalf("card=%+v", card)
	}
	taskWire, err := os.ReadFile("testdata/upstream-v1.0.1-task.json")
	if err != nil {
		t.Fatal(err)
	}
	if task, err := DecodeTask(taskWire); err != nil || task.Status.State != "TASK_STATE_COMPLETED" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	var compatibility map[string]any
	wire, err := os.ReadFile("testdata/compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(wire, &compatibility); err != nil {
		t.Fatal(err)
	}
	upstream, ok := compatibility["upstream"].(map[string]any)
	if !ok || upstream["fixture_release"] != FixtureRelease || upstream["normative_proto_sha256"] != ProtoSHA256 {
		t.Fatalf("compatibility=%+v", compatibility)
	}
}

func TestExportAgentCardPreservesPublicIdentityAndHidesRuntimeSecurity(t *testing.T) {
	t.Parallel()
	source := fixtureManifest(t)
	card, report, err := ExportAgentCard(source, "https://agents.example.invalid/a2a/demo")
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "Publication Example" || card.Version != "1.0.0" || card.Skills[0].ID != "default" || card.SupportedInterfaces[0].ProtocolVersion != "1.0" {
		t.Fatalf("card=%+v", card)
	}
	wire, _ := json.Marshal(card)
	for _, secret := range []string{"dispatch_ticket", "run_token", "runtime_lease", "deployment_routing"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("card leaked %s", secret)
		}
	}
	if report.Overall != Lossy || !strings.HasPrefix(report.Original.SHA256, "sha256:") || len(report.UnmappedSecurity) != 6 {
		t.Fatalf("report=%+v", report)
	}
}

func TestRunTaskStateMappingIsExplicit(t *testing.T) {
	t.Parallel()
	want := map[string]struct {
		state string
		level MappingLevel
	}{"queued": {"TASK_STATE_SUBMITTED", Exact}, "dispatching": {"TASK_STATE_SUBMITTED", Extended}, "running": {"TASK_STATE_WORKING", Exact}, "waiting_input": {"TASK_STATE_INPUT_REQUIRED", Exact}, "cancel_requested": {"TASK_STATE_WORKING", Extended}, "succeeded": {"TASK_STATE_COMPLETED", Exact}, "failed": {"TASK_STATE_FAILED", Exact}, "cancelled": {"TASK_STATE_CANCELED", Exact}, "timed_out": {"TASK_STATE_FAILED", Lossy}}
	for source, expected := range want {
		source := source
		expected := expected
		t.Run(source, func(t *testing.T) {
			state, level, reason, err := mapRunState(source)
			if err != nil || state != expected.state || level != expected.level || (level != Exact && reason == "") {
				t.Fatalf("state=%s level=%s reason=%q err=%v", state, level, reason, err)
			}
		})
	}
}

func TestExportTaskPreservesTerminalSnapshotAndTimeoutLoss(t *testing.T) {
	t.Parallel()
	status := fixtureRunStatus(t, "succeeded", true)
	task, report, err := ExportTask(status)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != "TASK_STATE_COMPLETED" || len(task.Artifacts) != 1 || task.Artifacts[0].ArtifactID != "snapshot" || report.Overall != Extended {
		t.Fatalf("task=%+v report=%+v", task, report)
	}
	if task.Metadata["arop"] == nil {
		t.Fatal("missing original AROP metadata")
	}
	if state, level, reason, err := mapRunState("timed_out"); err != nil || state != "TASK_STATE_FAILED" || level != Lossy || reason == "" {
		t.Fatalf("timeout mapping state=%s level=%s reason=%q err=%v", state, level, reason, err)
	}
}

func TestContentMappingRejectsUntrustedURLAndInlineBytes(t *testing.T) {
	t.Parallel()
	text := "hello"
	raw := "AA=="
	remote := "https://metadata.invalid/latest"
	content, items, err := A2APartsToAROP([]Part{{Text: &text}, {Data: json.RawMessage(`{"answer":42}`)}})
	if err != nil || len(content) != 2 || items[0].Level != Exact {
		t.Fatalf("content=%+v items=%+v err=%v", content, items, err)
	}
	if _, _, err := A2APartsToAROP([]Part{{Raw: &raw}}); err == nil {
		t.Fatal("inline raw unexpectedly accepted")
	}
	if _, _, err := A2APartsToAROP([]Part{{URL: &remote}}); err == nil {
		t.Fatal("untrusted URL unexpectedly accepted")
	}
}

func TestStrictA2ADecodeRejectsUnknownDuplicateTrailingAndState(t *testing.T) {
	t.Parallel()
	for name, wire := range map[string]string{"unknown": `{"id":"x","status":{"state":"TASK_STATE_WORKING"},"future":true}`, "duplicate": `{"id":"x","id":"y","status":{"state":"TASK_STATE_WORKING"}}`, "trailing": `{"id":"x","status":{"state":"TASK_STATE_WORKING"}} null`, "state": `{"id":"x","status":{"state":"invented"}}`} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTask([]byte(wire)); err == nil {
				t.Fatal("invalid task accepted")
			}
		})
	}
}

func TestExportEventPreservesDeltaSnapshotAndLifecycleSemantics(t *testing.T) {
	t.Parallel()
	delta := fixtureEvent(t, "io.arop.output.delta.v1", `{"output_id":"answer","offset":6,"delta":"world"}`)
	response, report, err := ExportEvent(delta)
	if err != nil {
		t.Fatal(err)
	}
	if response.ArtifactUpdate == nil || !response.ArtifactUpdate.Append || response.ArtifactUpdate.Artifact.ArtifactID != "answer" || report.Overall != Extended {
		t.Fatalf("delta response=%+v report=%+v", response, report)
	}
	aropMetadata := response.ArtifactUpdate.Metadata["arop"].(map[string]any)
	if aropMetadata["utf8Offset"] != int64(6) || aropMetadata["attemptId"] == "" {
		t.Fatalf("delta metadata=%+v", aropMetadata)
	}
	snapshot := fixtureEvent(t, "io.arop.output.snapshot.v1", `{"output_id":"answer","revision":2,"content":[{"type":"text","text":"complete"}],"digest":"sha256:`+strings.Repeat("2", 64)+`"}`)
	response, report, err = ExportEvent(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if response.ArtifactUpdate == nil || response.ArtifactUpdate.Append || len(response.ArtifactUpdate.Artifact.Parts) != 1 || report.Overall != Extended {
		t.Fatalf("snapshot response=%+v report=%+v", response, report)
	}
	terminal := fixtureEvent(t, "io.arop.run.cancelled.v1", `{"state":"cancelled","completed_at":"2026-09-28T00:01:00Z","usage":{"duration_ms":1,"input_tokens":0,"output_tokens":0}}`)
	response, _, err = ExportEvent(terminal)
	if err != nil || response.Task == nil || response.Task.Status.State != "TASK_STATE_CANCELED" {
		t.Fatalf("terminal response=%+v err=%v", response, err)
	}
	unsupported := delta
	unsupported.Type = "io.arop.output.future.v1"
	if _, _, err := ExportEvent(unsupported); err == nil {
		t.Fatal("unsupported event was accepted")
	}
}

func TestImportTaskStateDoesNotTurnCancelRequestIntoConfirmation(t *testing.T) {
	t.Parallel()
	state, level, _, err := ImportTaskState(Task{ID: "task-a", Status: TaskStatus{State: "TASK_STATE_CANCELED"}})
	if err != nil || state != "cancelled" || level != Exact {
		t.Fatalf("state=%s level=%s err=%v", state, level, err)
	}
	state, level, reason, err := ImportTaskState(Task{ID: "task-b", Status: TaskStatus{State: "TASK_STATE_REJECTED"}})
	if err != nil || state != "failed" || level != Lossy || reason == "" {
		t.Fatalf("state=%s level=%s reason=%q err=%v", state, level, reason, err)
	}
}

func fixtureManifest(t *testing.T) controlplane.AgentManifest {
	t.Helper()
	wire, err := os.ReadFile(filepath.Join("..", "..", "examples", "manifests", "publication-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := controlplane.DecodeAgentManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixtureRunStatus(t *testing.T, state string, snapshot bool) runwire.RunStatus {
	t.Helper()
	result := ""
	if snapshot {
		result = `,"result":{"schema_version":1,"run_id":"run_01956e7b-9abc-7def-8abc-0123456789ab","state":"` + state + `","completed_at":"2026-09-28T00:01:00Z","snapshot":{"revision":1,"digest":"sha256:` + strings.Repeat("2", 64) + `","content":[{"type":"text","text":"done"}]},"usage":{"duration_ms":100,"input_tokens":1,"output_tokens":1}}`
	}
	wire := `{"schema_version":1,"run_id":"run_01956e7b-9abc-7def-8abc-0123456789ab","agent":{"id":"demo","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:` + strings.Repeat("0", 64) + `"},"state":"` + state + `","state_version":2,"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:01:00Z","deadline_at":"2026-09-28T00:01:00Z","authorization_snapshot_digest":"sha256:` + strings.Repeat("1", 64) + `","trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}` + result + `}`
	value, err := runwire.DecodeRunStatus([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixtureEvent(t *testing.T, eventType, data string) eventwire.EventEnvelope {
	t.Helper()
	wire := `{"specversion":"1.0","id":"evt_01932f13-0cd2-7a82-8fa3-1cb5ce13ef11","source":"https://runtime.example.invalid/instances/runtime-a","type":"` + eventType + `","subject":"runs/run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","time":"2026-09-28T00:01:00Z","datacontenttype":"application/json","dataschema":"https://arop.invalid/schemas/v1/events/output-events-v1.schema.json","runid":"run_01932f13-0cd2-7a82-8fa3-1cb5ce13ef10","attemptid":"att_01932f13-0cd2-7a82-8fa3-1cb5ce13ef12","producersequence":2,"runsequence":3,"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","data":` + data + `}`
	value, err := eventwire.DecodeEventEnvelope([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
