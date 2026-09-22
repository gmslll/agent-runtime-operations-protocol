package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func TestDraft2020AndFormatAssertions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "schema.json"), []byte(`{
      "$schema":"https://json-schema.org/draft/2020-12/schema",
      "type":"object","required":["kind","when","count","digest"],
      "properties":{
        "kind":{"const":"test"},"when":{"type":"string","format":"date-time"},
        "count":{"type":"integer"},"digest":{"type":"string","pattern":"^[a-f0-9]{64}$"}
      },"additionalProperties":false
    }`), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `{"kind":"test","when":"2026-09-22T01:02:03Z","count":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	value, err := structuredfile.Parse([]byte(valid), "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(root, "schema.json", value); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	for _, invalid := range []string{
		`{"kind":"wrong","when":"2026-09-22T01:02:03Z","count":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"kind":"test","when":"not-a-date","count":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"kind":"test","when":"2026-09-22T01:02:03Z","count":1.5,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"kind":"test","when":"2026-09-22T01:02:03Z","count":1,"digest":"BAD"}`,
		`{"kind":"test","when":"2026-09-22T01:02:03Z","count":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extra":true}`,
	} {
		value, err := structuredfile.Parse([]byte(invalid), "json")
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateFile(root, "schema.json", value); err == nil {
			t.Fatalf("invalid document accepted: %s", invalid)
		}
	}
}

func TestPlanningAndGateSummarySchemasAreStrict(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	digest := strings.Repeat("a", 64)
	results := make([]any, 14)
	for i := range results {
		results[i] = map[string]any{"id": "IR-" + string(rune('A'+i)), "result": "PASS"}
	}
	result := map[string]any{
		"requirements": results, "phases": []any{map[string]any{"id": "P01", "result": "PASS"}},
		"must_fix_count": 0, "must_fix": []any{}, "should_fix_count": 0, "should_fix": []any{}, "verdict": "PASS",
	}
	subject := map[string]any{
		"commit": strings.Repeat("b", 40), "plan_last_phase": "P53", "requirements_sha256": digest,
		"plan_sha256": digest, "blueprint_sha256": digest, "artifact_manifest_sha256": digest,
	}
	base := map[string]any{
		"schema_version": 1, "kind": "arop-planning-audit-summary", "subject": subject,
		"reviewer_id": "reviewer-1", "result": result, "attested_at": "2026-09-22T01:02:03Z",
		"summary_sha256":      "sha256:" + digest,
		"authentication":      map[string]any{"mode": "manual-trusted-channel", "confirmation_sha256": "sha256:" + digest, "limitation": "manual identity and independence verification is not cryptographically replayable"},
		"raw_evidence_sha256": "sha256:" + digest,
	}
	if err := ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", base); err != nil {
		t.Fatalf("valid planning candidate rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"bad date":       func(v map[string]any) { v["attested_at"] = "not-a-date" },
		"top extra":      func(v map[string]any) { v["extra"] = true },
		"subject extra":  func(v map[string]any) { v["subject"].(map[string]any)["extra"] = true },
		"result extra":   func(v map[string]any) { v["result"].(map[string]any)["extra"] = true },
		"fraction count": func(v map[string]any) { v["result"].(map[string]any)["should_fix_count"] = 0.5 },
		"bad phase":      func(v map[string]any) { v["subject"].(map[string]any)["plan_last_phase"] = "P00" },
		"bad digest":     func(v map[string]any) { v["subject"].(map[string]any)["plan_sha256"] = strings.ToUpper(digest) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneMap(t, base)
			mutate(candidate)
			if err := ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", candidate); err == nil {
				t.Fatal("invalid canonical evidence summary accepted")
			}
		})
	}

	gate := cloneMap(t, base)
	gate["kind"] = "arop-user-gate-summary"
	gate["gate"] = "P04"
	delete(gate, "reviewer_id")
	gate["approver_id"] = "owner-1"
	gate["subject"].(map[string]any)["planning_audit_summary_sha256"] = "sha256:" + digest
	if err := ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", gate); err != nil {
		t.Fatalf("valid Gate candidate rejected: %v", err)
	}
	gate["reviewer_id"] = "must-not-be-present"
	if err := ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", gate); err == nil {
		t.Fatal("Gate candidate with reviewer_id accepted")
	}
}

func cloneMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
