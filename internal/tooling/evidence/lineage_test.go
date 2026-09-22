package evidence

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, root string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = root
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}
func write(t *testing.T, root, path, value string) {
	t.Helper()
	p := filepath.Join(root, path)
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(value), 0644); e != nil {
		t.Fatal(e)
	}
}
func commit(t *testing.T, root, msg string) string {
	run(t, root, "add", ".")
	run(t, root, "commit", "-q", "-m", msg)
	return run(t, root, "rev-parse", "HEAD")
}
func subject(t *testing.T, root, commit string) map[string]any {
	s := map[string]any{"commit": commit}
	for _, i := range PlanningInputs {
		b, e := os.ReadFile(filepath.Join(root, i.Path))
		if e != nil {
			t.Fatal(e)
		}
		s[i.Field] = hash(b)
	}
	return s
}
func TestEvidenceLineage(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q")
	run(t, root, "config", "user.name", "AROP Evidence Test")
	run(t, root, "config", "user.email", "evidence@invalid.example")
	for _, i := range PlanningInputs {
		write(t, root, i.Path, i.Path+" baseline\n")
	}
	base := commit(t, root, "baseline")
	signed := subject(t, root, base)
	write(t, root, "unrelated", "x")
	commit(t, root, "unrelated")
	if _, e := VerifySubjectCommit(root, base, signed); e != nil {
		t.Fatal(e)
	}
	t.Run("missing", func(t *testing.T) {
		if _, e := VerifySubjectCommit(root, strings.Repeat("0", 40), signed); e == nil || !strings.Contains(e.Error(), "does not exist") {
			t.Fatalf("wrong error %v", e)
		}
	})
	t.Run("wrong-signed-digest", func(t *testing.T) {
		bad := map[string]any{}
		for k, v := range signed {
			bad[k] = v
		}
		bad["plan_sha256"] = strings.Repeat("0", 64)
		if _, e := VerifySubjectCommit(root, base, bad); e == nil || !strings.Contains(e.Error(), "signed plan_sha256") {
			t.Fatalf("wrong error %v", e)
		}
	})
	write(t, root, "docs/DEVELOPMENT_PLAN.md", "changed\n")
	commit(t, root, "change")
	if _, e := VerifySubjectCommit(root, base, signed); e == nil || !strings.Contains(e.Error(), "current planning input") {
		t.Fatalf("changed accepted: %v", e)
	}
	write(t, root, "docs/DEVELOPMENT_PLAN.md", "docs/DEVELOPMENT_PLAN.md baseline\n")
	commit(t, root, "revert")
	if _, e := VerifySubjectCommit(root, base, signed); e == nil || !strings.Contains(e.Error(), "changed-then-reverted") {
		t.Fatalf("replay accepted: %v", e)
	}
	original, _ := os.ReadFile(filepath.Join(root, "spec/artifact-manifest.yaml"))
	write(t, root, "spec/artifact-manifest.yaml", string(original)+"drift\n")
	if _, e := VerifySubjectCommit(root, base, signed); e == nil || !strings.Contains(e.Error(), "artifact-manifest.yaml differs") {
		t.Fatalf("manifest drift accepted %v", e)
	}
	os.WriteFile(filepath.Join(root, "spec/artifact-manifest.yaml"), original, 0644)

	deletionRoot := filepath.Join(t.TempDir(), "deleted-case")
	run(t, root, "worktree", "add", "-q", "-b", "deleted-case", deletionRoot, base)
	run(t, deletionRoot, "config", "user.name", "AROP Evidence Test")
	run(t, deletionRoot, "config", "user.email", "evidence@invalid.example")
	run(t, deletionRoot, "rm", "-q", "spec/artifact-manifest.yaml")
	deletionCommit := commit(t, deletionRoot, "delete planning blob")
	t.Run("deleted-current-planning-blob", func(t *testing.T) {
		if _, e := VerifySubjectCommit(deletionRoot, base, signed); e == nil || !strings.Contains(e.Error(), "current planning input") {
			t.Fatalf("deleted blob accepted: %v", e)
		}
	})
	t.Run("subject-commit-missing-blob", func(t *testing.T) {
		missing := map[string]any{}
		for k, v := range signed {
			missing[k] = v
		}
		missing["commit"] = deletionCommit
		if _, e := VerifySubjectCommit(deletionRoot, deletionCommit, missing); e == nil || !strings.Contains(e.Error(), "absent at evidence subject") {
			t.Fatalf("missing subject blob accepted: %v", e)
		}
	})
	t.Run("non-ancestor", func(t *testing.T) {
		missing := map[string]any{}
		for k, v := range signed {
			missing[k] = v
		}
		missing["commit"] = deletionCommit
		if _, e := VerifySubjectCommit(root, deletionCommit, missing); e == nil || !strings.Contains(e.Error(), "not an ancestor") {
			t.Fatalf("non-ancestor accepted: %v", e)
		}
	})
	run(t, root, "worktree", "remove", "--force", deletionRoot)

	t.Run("P04-schema-requires-artifact-manifest", func(t *testing.T) {
		schemaRoot, e := filepath.Abs(filepath.Join("..", "..", ".."))
		if e != nil {
			t.Fatal(e)
		}
		data, e := os.ReadFile(filepath.Join(schemaRoot, "spec/schemas/user-gate-evidence.schema.json"))
		if e != nil {
			t.Fatal(e)
		}
		var schema map[string]any
		if e = json.Unmarshal(data, &schema); e != nil {
			t.Fatal(e)
		}
		properties, _ := schema["properties"].(map[string]any)
		subjectSchema, _ := properties["subject"].(map[string]any)
		required, _ := subjectSchema["required"].([]any)
		found := false
		for _, v := range required {
			if v == "artifact_manifest_sha256" {
				found = true
			}
		}
		if !found {
			t.Fatal("P04 schema permits missing artifact_manifest_sha256")
		}
	})
}
