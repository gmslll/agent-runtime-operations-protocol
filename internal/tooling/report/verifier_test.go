package report

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func strictReportFixture() Report {
	zero := strings.Repeat("0", 64)
	return Report{
		SchemaVersion: 1,
		GeneratedAt:   "2026-09-22T01:02:03Z",
		Success:       true,
		Provenance: Provenance{
			Git:             GitProvenance{strings.Repeat("a", 40), false, []string{}},
			Command:         "make fixture",
			Runtime:         Runtime{"v1", "go1", RuntimeOS{"test", "1", "test"}},
			Checker:         DigestedPath{"checker.go", zero},
			Inputs:          InputDigest{zero, []InputFile{{"input", zero, 1}}},
			RuntimeInputs:   InputDigest{zero, []InputFile{}},
			RuntimeEvidence: []RuntimeEvidence{},
			TestcaseCount:   1,
			AuditNote:       "fixture",
		},
		Summary: map[string]any{"checks": 1, "passed": 1, "failed": 0, "testcase_count": 1},
		Checks:  []Check{{"fixture", true, "ok"}},
		Errors:  []string{},
	}
}

func TestStrictReportSchemaRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	valid, err := json.Marshal(strictReportFixture())
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func([]byte) []byte{
		"top-level duplicate": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1))
		},
		"nested duplicate": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"head":"`, `"head":"`+strings.Repeat("a", 40)+`","head":"`, 1))
		},
		"trailing document": func(data []byte) []byte { return append(data, []byte(` {}`)...) },
		"unknown top-level": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"errors":[]`, `"errors":[],"unexpected":true`, 1))
		},
		"unknown check field": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"detail":"ok"`, `"detail":"ok","unexpected":true`, 1))
		},
		"bad date-time": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), "2026-09-22T01:02:03Z", "not-a-date", 1))
		},
		"fractional count": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"testcase_count":1`, `"testcase_count":1.5`, 1))
		},
		"bad digest": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), strings.Repeat("0", 64), "BAD", 1))
		},
		"empty inputs": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"files":[{"path":"input","sha256":"`+strings.Repeat("0", 64)+`","bytes":1}]`, `"files":[]`, 1))
		},
		"missing nested": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"arch":"test"`, `"arch_missing":"test"`, 1))
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeStrict(root, mutate(append([]byte{}, valid...))); err == nil {
				t.Fatalf("malformed report accepted")
			}
		})
	}
}

func TestWriterPropagatesMarshalFailure(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	r := strictReportFixture()
	r.Summary["unmarshalable"] = func() {}
	if _, err := marshalValidated(root, &r); err == nil || !strings.Contains(err.Error(), "marshal report JSON") {
		t.Fatalf("marshal failure was not propagated: %v", err)
	}
}

func TestWriterCommandAndGitErrorsPropagate(t *testing.T) {
	t.Parallel()
	if _, err := command(t.TempDir(), "definitely-not-an-arop-command"); err == nil {
		t.Fatal("missing runtime command was converted to a placeholder")
	}
	if _, err := gitStatus(t.TempDir()); err == nil {
		t.Fatal("git status failure was ignored")
	}
}

func TestVerifierCurrentReports(t *testing.T) {
	if value := os.Getenv("AROP_VERIFY_CURRENT"); value != "" && value != "1" {
		t.Fatalf("AROP_VERIFY_CURRENT must be 1 when set, got %q", value)
	}
}

func TestVerifierRejectsCorruptedDigest(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	r := strictReportFixture()
	r.Provenance.Inputs.Files[0].SHA256 = strings.Repeat("f", 64)
	r.Provenance.Inputs.SHA256 = strings.Repeat("f", 64)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeStrict(root, data)
	if err != nil {
		t.Fatal(err)
	}
	if equalInputFiles(decoded.Provenance.Inputs.Files, []InputFile{{"input", strings.Repeat("0", 64), 1}}) {
		t.Fatal("corrupted digest was not represented in decoded report")
	}
}

func TestWriterRuntimeIsGo(t *testing.T) {
	if runtime.Version() == "" {
		t.Fatal("Go runtime unavailable")
	}
}

func TestVerifierRejectsEveryCurrentRuntimeMutation(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "AROP Runtime Test")
	gitTest(t, root, "config", "user.email", "runtime@invalid.example")
	schemaSource := filepath.Join("..", "..", "..", "spec", "schemas", "check-report.schema.json")
	schemaData, err := os.ReadFile(schemaSource)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"spec/schemas/check-report.schema.json": schemaData,
		"checker.go":                            []byte("package fixture\n"),
		"input.txt":                             []byte("stable\n"),
		".gitignore":                            []byte("build/\n"),
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "runtime fixture")
	_, err = Write(WriteOptions{Root: root, Directory: "build/runtime", Suite: "runtime", Class: "runtime", Command: "test runtime", CheckerPath: "checker.go", InputPaths: []string{"input.txt"}, Checks: []Check{{Name: "fixture", Passed: true, Detail: "ok"}}, AuditNote: "runtime binding fixture"})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := "build/runtime/report.json"
	if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: reportPath}); err != nil || mode != "current-worktree" {
		t.Fatalf("valid current report rejected mode=%s err=%v", mode, err)
	}
	original, err := os.ReadFile(filepath.Join(root, reportPath))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Report){
		"node":        func(r *Report) { r.Provenance.Runtime.Node += "-tampered" },
		"go":          func(r *Report) { r.Provenance.Runtime.Go += "-tampered" },
		"os platform": func(r *Report) { r.Provenance.Runtime.OS.Platform += "-tampered" },
		"os release":  func(r *Report) { r.Provenance.Runtime.OS.Release += "-tampered" },
		"os arch":     func(r *Report) { r.Provenance.Runtime.OS.Arch += "-tampered" },
	} {
		t.Run(name, func(t *testing.T) {
			var candidate Report
			if err := json.Unmarshal(original, &candidate); err != nil {
				t.Fatal(err)
			}
			mutate(&candidate)
			data, err := json.MarshalIndent(candidate, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, reportPath), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: reportPath}); err == nil || mode != "current-worktree" || !strings.Contains(err.Error(), "runtime does not match") {
				t.Fatalf("runtime mutation accepted mode=%s err=%v", mode, err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, reportPath), original, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierHistoricalLineage(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "AROP Verifier Test")
	gitTest(t, root, "config", "user.email", "verifier@invalid.example")
	schemaSource := filepath.Join("..", "..", "..", "spec", "schemas", "check-report.schema.json")
	schemaData, err := os.ReadFile(schemaSource)
	if err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(root, "spec", "schemas", "check-report.schema.json")
	if err := os.MkdirAll(filepath.Dir(schemaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schemaPath, schemaData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stable.txt"), []byte("stable\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "ancestor")
	ancestor := gitTest(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("descendant\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "descendant")
	current := gitTest(t, root, "rev-parse", "HEAD")
	inputData, _ := os.ReadFile(filepath.Join(root, "stable.txt"))
	input := InputFile{"stable.txt", Hash(inputData), int64(len(inputData))}
	fixture := Report{SchemaVersion: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Success: true, Provenance: Provenance{Git: GitProvenance{ancestor, false, []string{}}, Command: "fixture", Runtime: Runtime{"node fixture", "go fixture", RuntimeOS{"test", "test", "test"}}, Checker: DigestedPath{"stable.txt", input.SHA256}, Inputs: InputDigest{Aggregate([]InputFile{input}), []InputFile{input}}, RuntimeInputs: InputDigest{Aggregate(nil), []InputFile{}}, RuntimeEvidence: []RuntimeEvidence{}, TestcaseCount: 1, AuditNote: "fixture"}, Summary: map[string]any{"checks": 1, "passed": 1, "failed": 0, "testcase_count": 1}, Checks: []Check{{"fixture", true, "ok"}}, Errors: []string{}}
	path := writeReportFixture(t, root, "historical", fixture)
	if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: path, AllowAncestor: true}); err != nil || mode != "ancestor-archive-only" {
		t.Fatalf("ancestor verification failed mode=%s err=%v", mode, err)
	}
	if _, _, err := Verify(VerifyOptions{Root: root, ReportPath: path}); err == nil || !strings.Contains(err.Error(), "ALLOW_ANCESTOR=1") {
		t.Fatalf("ancestor opt-in not enforced: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "stable.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(VerifyOptions{Root: root, ReportPath: path, AllowAncestor: true}); err == nil || !strings.Contains(err.Error(), "current reuse eligibility") {
		t.Fatalf("current drift accepted: %v", err)
	}
	_ = os.WriteFile(filepath.Join(root, "stable.txt"), inputData, 0644)
	missing := fixture
	missing.Provenance.Git.Head = strings.Repeat("0", 40)
	missingPath := writeReportFixture(t, root, "missing", missing)
	if _, _, err := Verify(VerifyOptions{Root: root, ReportPath: missingPath, AllowAncestor: true}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing commit accepted: %v", err)
	}
	gitTest(t, root, "checkout", "-q", "--detach", ancestor)
	_ = os.WriteFile(filepath.Join(root, "branch.txt"), []byte("branch\n"), 0644)
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "nonancestor")
	nonancestor := gitTest(t, root, "rev-parse", "HEAD")
	gitTest(t, root, "checkout", "-q", "--detach", current)
	non := fixture
	non.Provenance.Git.Head = nonancestor
	nonPath := writeReportFixture(t, root, "nonancestor", non)
	if _, _, err := Verify(VerifyOptions{Root: root, ReportPath: nonPath, AllowAncestor: true}); err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("nonancestor accepted: %v", err)
	}
}

func TestVerifierRediscoverControlledInputsForCurrentAndAncestorReports(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "AROP Controlled Report Test")
	gitTest(t, root, "config", "user.email", "controlled-report@invalid.example")
	schemaSource := filepath.Join("..", "..", "..", "spec", "schemas", "check-report.schema.json")
	schemaData, err := os.ReadFile(schemaSource)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		"spec/schemas/check-report.schema.json": schemaData,
		"checker.go":                            []byte("package fixture\n"),
		"input.txt":                             []byte("stable\n"),
		".gitignore":                            []byte("build/\n"),
	} {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, root, "add", ".")
	gitTest(t, root, "commit", "-q", "-m", "controlled report")
	_, err = Write(WriteOptions{Root: root, Directory: "build/reports/P01", Suite: "controlled", Class: "controlled", Command: "make spec-index-check", CheckerPath: "checker.go", InputPaths: []string{"input.txt"}, Checks: []Check{{Name: "fixture", Passed: true, Detail: "ok"}}, ControlledInputScope: "P01", AuditNote: "controlled fixture"})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := "build/reports/P01/report.json"
	if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: reportPath}); err != nil || mode != "current-worktree" {
		t.Fatalf("current controlled report rejected mode=%s err=%v", mode, err)
	}
	original, err := os.ReadFile(filepath.Join(root, reportPath))
	if err != nil {
		t.Fatal(err)
	}
	var tampered Report
	if err := json.Unmarshal(original, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.Provenance.ControlledInputs.Entries[0].SHA256 = strings.Repeat("f", 64)
	tamperedData, err := json.MarshalIndent(tampered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, reportPath), append(tamperedData, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(VerifyOptions{Root: root, ReportPath: reportPath}); err == nil || !strings.Contains(err.Error(), "controlled input manifest") {
		t.Fatalf("tampered controlled manifest accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, reportPath), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "descendant.txt"), []byte("new tracked input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, root, "add", "descendant.txt")
	gitTest(t, root, "commit", "-q", "-m", "descendant")
	if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: reportPath, AllowAncestor: true}); err != nil || mode != "ancestor-archive-only" {
		t.Fatalf("ancestor controlled report rejected mode=%s err=%v", mode, err)
	}
}

func gitTest(t *testing.T, root string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = root
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func writeReportFixture(t *testing.T, root, name string, r Report) string {
	t.Helper()
	dir := filepath.Join(root, "build", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	junit := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<testsuite name=\"fixture\" tests=\"1\" failures=\"0\"><testcase classname=\"fixture\" name=\"fixture\"></testcase></testsuite>\n"
	if err := os.WriteFile(filepath.Join(dir, "junit.xml"), []byte(junit), 0644); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(root, filepath.Join(dir, "report.json"))
	return rel
}
