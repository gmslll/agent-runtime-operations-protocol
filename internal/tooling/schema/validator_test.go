package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	for _, unsupported := range []string{
		`(?=a)a`, `(a)\1`, `\A`, `\z`, `\Qliteral\E`, `[[:alpha:]]`, `^.$`,
		`\-`, `\!`, `\_`, `\,`, `\:`,
	} {
		if _, err := compileECMAScript(unsupported); err == nil {
			t.Fatalf("non-portable/backtracking pattern %q was accepted", unsupported)
		}
	}

	notSchema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","not":{"pattern":"^(a+)+b$|^a+$"}}`
	if err := os.WriteFile(filepath.Join(root, "schema.json"), []byte(notSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := ValidateFile(root, "schema.json", strings.Repeat("a", 20_000)); err == nil {
		t.Fatal("linear regexp engine failed open under not")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("linear regexp engine exceeded not budget: %s", elapsed)
	}

	var many strings.Builder
	many.WriteString(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{`)
	manyValue := map[string]any{}
	for index := 0; index < 40; index++ {
		if index != 0 {
			many.WriteByte(',')
		}
		fmt.Fprintf(&many, `"p%d":{"type":"string","pattern":"^(a+)+$"}`, index)
		manyValue[fmt.Sprintf("p%d", index)] = strings.Repeat("a", 20_000) + "!"
	}
	many.WriteString(`},"additionalProperties":false}`)
	if err := os.WriteFile(filepath.Join(root, "schema.json"), []byte(many.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	if err := ValidateFile(root, "schema.json", manyValue); err == nil {
		t.Fatal("multi-pattern invalid value was accepted")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("schema validation multiplied regexp work: %s", elapsed)
	}
}

func TestRepositorySchemaLoaderResolvesOnlyLocalCanonicalIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "schemas", "common"), 0o700); err != nil {
		t.Fatal(err)
	}
	common := `{"$id":"https://arop.invalid/schemas/v1/common/value.schema.json","$schema":"https://json-schema.org/draft/2020-12/schema","type":"string","const":"ok"}`
	if err := os.WriteFile(filepath.Join(root, "schemas", "common", "value.schema.json"), []byte(common), 0o600); err != nil {
		t.Fatal(err)
	}
	main := `{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"https://arop.invalid/schemas/v1/common/value.schema.json"}`
	if err := os.WriteFile(filepath.Join(root, "schema.json"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(root, "schema.json", "ok"); err != nil {
		t.Fatalf("canonical repository reference rejected: %v", err)
	}
	if err := ValidateFile(root, "schema.json", "wrong"); err == nil {
		t.Fatal("referenced constraint was not enforced")
	}

	for _, ref := range []string{
		"https://example.com/schemas/v1/common/value.schema.json",
		"https://arop.invalid/other/value.schema.json",
		"https://arop.invalid/schemas/v1/../common/value.schema.json",
		"http://arop.invalid/schemas/v1/common/value.schema.json",
	} {
		content := fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":%q}`, ref)
		if err := os.WriteFile(filepath.Join(root, "schema.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ValidateFile(root, "schema.json", "ok"); err == nil {
			t.Fatalf("unsafe or remote reference %q was accepted", ref)
		}
	}
}

func TestPlanningAndGateSummarySchemasAreStrict(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	manifestRaw, err := os.ReadFile(filepath.Join(root, "spec", "artifact-manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := structuredfile.Parse(manifestRaw, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifestMap, ok := manifest.(map[string]any)
	if !ok {
		t.Fatalf("artifact manifest parsed as %T, want map[string]any", manifest)
	}
	for _, candidatePath := range []string{".git/config", "..inside/file"} {
		candidate := cloneMap(t, manifestMap)
		candidate["artifacts"].([]any)[0].(map[string]any)["path"] = candidatePath
		if err := ValidateFile(root, "spec/schemas/artifact-manifest.schema.json", candidate); err != nil {
			t.Fatalf("valid portable artifact path %q rejected: %v", candidatePath, err)
		}
	}
	for _, candidatePath := range []string{"../escape", "foo/../bar", "/abs", "foo//bar", "foo/.", "foo\rbar", "foo\nbar"} {
		candidate := cloneMap(t, manifestMap)
		candidate["artifacts"].([]any)[0].(map[string]any)["path"] = candidatePath
		if err := ValidateFile(root, "spec/schemas/artifact-manifest.schema.json", candidate); err == nil {
			t.Fatalf("unsafe artifact path %q accepted", candidatePath)
		}
	}
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
	gate["subject"].(map[string]any)["planning_audit_promoted_jcs_sha256"] = "sha256:" + digest
	gate["subject"].(map[string]any)["planning_audit_promoted_file_sha256"] = "sha256:" + digest
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
