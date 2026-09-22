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

func TestVerifierCurrentReports(t *testing.T) {
	if os.Getenv("AROP_VERIFY_CURRENT") != "1" {
		t.Skip("current reports are verified by make test-report-verifier")
	}
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	for _, path := range []string{"build/reports/P01/report.json", "build/reports/P02/report.json"} {
		r, mode, err := Verify(VerifyOptions{Root: root, ReportPath: path})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if mode != "current-worktree" || !r.Success {
			t.Fatalf("%s unexpected mode/success", path)
		}
	}
}

func TestVerifierRejectsCorruptedDigest(t *testing.T) {
	if os.Getenv("AROP_VERIFY_CURRENT") != "1" {
		t.Skip("requires generated P01 report")
	}
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	source := filepath.Join(root, "build/reports/P01/report.json")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	r.Provenance.Inputs.Files[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	dir := filepath.Join(root, "build/report-verifier-tests/corrupted-go")
	if err = os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	bad, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "report.json"), append(bad, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	junit, err := os.ReadFile(filepath.Join(root, "build/reports/P01/junit.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "junit.xml"), junit, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Verify(VerifyOptions{Root: root, ReportPath: "build/report-verifier-tests/corrupted-go/report.json"}); err == nil {
		t.Fatal("corrupted digest passed")
	}
}

func TestWriterRuntimeIsGo(t *testing.T) {
	if runtime.Version() == "" {
		t.Fatal("Go runtime unavailable")
	}
}

func TestVerifierHistoricalLineage(t *testing.T) {
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	gitTest(t, root, "config", "user.name", "AROP Verifier Test")
	gitTest(t, root, "config", "user.email", "verifier@invalid.example")
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
	fixture := Report{SchemaVersion: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Success: true, Provenance: Provenance{Git: GitProvenance{ancestor, false, []string{}}, Command: "fixture", Runtime: Runtime{"node fixture", "go fixture", RuntimeOS{"test", "test", "test"}}, Checker: DigestedPath{"stable.txt", input.SHA256}, Inputs: InputDigest{Aggregate([]InputFile{input}), []InputFile{input}}, TestcaseCount: 1, AuditNote: "fixture"}, Summary: map[string]any{"checks": 1, "passed": 1, "failed": 0, "testcase_count": 1}, Checks: []Check{{"fixture", true, "ok"}}, Errors: []string{}}
	path := writeReportFixture(t, root, "historical", fixture)
	if _, mode, err := Verify(VerifyOptions{Root: root, ReportPath: path, AllowAncestor: true}); err != nil || mode != "ancestor-commit" {
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
