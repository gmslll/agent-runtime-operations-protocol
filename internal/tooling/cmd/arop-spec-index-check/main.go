package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/specindex"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type nodeResult struct {
	Checks     []report.Check `json:"checks"`
	Errors     []string       `json:"errors"`
	InputPaths []string       `json:"input_paths"`
	Summary    map[string]any `json:"summary"`
}
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	dir := filepath.Join(root, "build/reports/P01")
	fatal(os.MkdirAll(dir, 0o755))
	resultPath := filepath.Join(dir, "spec-index-results.json")
	node := exec.Command("node", "scripts/spec-index-check.mjs")
	node.Dir = root
	node.Env = append(os.Environ(), "AROP_RESULT_FILE="+resultPath)
	node.Stdout = os.Stdout
	node.Stderr = os.Stderr
	nodeErr := node.Run()
	var result nodeResult
	nodeResultBytes, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		result.Checks = append(result.Checks, report.Check{Name: "node-schema-spec-validation", Passed: false, Detail: readErr.Error()})
	} else if err := structuredfile.Load(resultPath, &result); err != nil {
		result.Checks = append(result.Checks, report.Check{Name: "node-schema-spec-validation", Passed: false, Detail: err.Error()})
	}
	if nodeErr != nil {
		result.Checks = append(result.Checks, report.Check{Name: "node-schema-spec-validation", Passed: false, Detail: nodeErr.Error()})
	} else {
		result.Checks = append(result.Checks, report.Check{Name: "node-schema-spec-validation", Passed: true, Detail: "Node executed only structured-file/Schema/offline-ref validation"})
	}
	governanceChecks, governanceSummary, _ := specindex.Run(root)
	result.Checks = append(result.Checks, governanceChecks...)
	if result.Summary == nil {
		result.Summary = map[string]any{}
	}
	for key, value := range governanceSummary {
		result.Summary[key] = value
	}

	testArgs := []string{"test", "-count=1", "-json", "./internal/tooling/structuredfile", "./internal/tooling/schema", "./internal/tooling/report", "./internal/tooling/evidence", "./internal/tooling/specindex", "./internal/tooling/blueprint"}
	test := exec.Command("go", testArgs...)
	test.Dir = root
	test.Env = append(os.Environ(), "AROP_VERIFY_CURRENT=1")
	testOut, testErr := test.CombinedOutput()
	testLog := filepath.Join(dir, "governance-tests.log")
	fatal(os.WriteFile(testLog, testOut, 0o644))
	testChecks, counts, parseErr := structuredGoTestChecks(testOut)
	result.Checks = append(result.Checks, testChecks...)
	goTestPassed := testErr == nil && parseErr == nil && counts["fail"] == 0 && counts["skip"] == 0 && counts["executed"] > 0 && !bytes.Contains(testOut, []byte("(cached)"))
	detail := fmt.Sprintf("executed=%d pass=%d fail=%d skip=%d uncached=%t", counts["executed"], counts["pass"], counts["fail"], counts["skip"], !bytes.Contains(testOut, []byte("(cached)")))
	if testErr != nil {
		detail += "; " + testErr.Error()
	}
	if parseErr != nil {
		detail += "; " + parseErr.Error()
	}
	result.Checks = append(result.Checks, report.Check{Name: "go-governance-tests", Passed: goTestPassed, Detail: detail})
	result.Summary["go_testcases_executed"] = counts["executed"]
	result.Summary["go_testcases_passed"] = counts["pass"]
	result.Summary["go_testcases_failed"] = counts["fail"]
	result.Summary["go_testcases_skipped"] = counts["skip"]

	inputs := append([]string{}, result.InputPaths...)
	staticInputs, staticErr := governanceStaticInputs(root)
	fatal(staticErr)
	inputs = append(inputs, staticInputs...)
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-spec-index-check"
	}
	runtimeEvidence := []report.RuntimeEvidence{
		{Kind: "go-governance-test-log", SHA256: report.Hash(testOut), Bytes: int64(len(testOut))},
		{Kind: "node-schema-validation-result", SHA256: report.Hash(nodeResultBytes), Bytes: int64(len(nodeResultBytes))},
	}
	r, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P01", Suite: "arop-spec-index-check", Class: "arop.spec-index", Command: command, CheckerPath: "internal/tooling/cmd/arop-spec-index-check/main.go", InputPaths: inputs, RuntimeEvidence: runtimeEvidence, Checks: result.Checks, Summary: result.Summary, AuditNote: "Node is confined to strict Schema/spec/manifest validation; Go owns governance, evidence, negative tests and canonical report generation."})
	fatal(err)
	verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P01/report.json"})
	if verifyErr != nil {
		fatal(fmt.Errorf("self-verification failed in %s mode: %w", mode, verifyErr))
	}
	if mode != "current-worktree" || !verified.Success {
		fatal(fmt.Errorf("self-verification returned mode=%s success=%t", mode, verified.Success))
	}
	if !r.Success {
		fmt.Fprintln(os.Stderr, "AROP spec index check failed; see build/reports/P01/report.json")
		os.Exit(1)
	}
	fmt.Printf("AROP spec index check passed: %d checks (%d uncached Go tests).\n", len(r.Checks), counts["executed"])
}

func structuredGoTestChecks(data []byte) ([]report.Check, map[string]int, error) {
	checks := []report.Check{}
	counts := map[string]int{"executed": 0, "pass": 0, "fail": 0, "skip": 0}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seen := map[string]bool{}
	for scanner.Scan() {
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return checks, counts, fmt.Errorf("parse go test -json: %w", err)
		}
		if event.Test == "" || (event.Action != "pass" && event.Action != "fail" && event.Action != "skip") {
			continue
		}
		key := event.Package + "/" + event.Test
		if seen[key] {
			continue
		}
		seen[key] = true
		counts["executed"]++
		counts[event.Action]++
		checks = append(checks, report.Check{Name: "go-test:" + key, Passed: event.Action == "pass", Detail: fmt.Sprintf("action=%s elapsed=%.3fs", event.Action, event.Elapsed)})
	}
	return checks, counts, scanner.Err()
}

func governanceStaticInputs(root string) ([]string, error) {
	inputs := []string{"go.mod", "go.sum", "Makefile", "package.json", "internal/tooling/cmd/arop-spec-index-check/main.go", "scripts/spec-index-check.mjs", "scripts/lib/repository.mjs", "spec/schemas/check-report.schema.json", "spec/schemas/artifact-manifest.schema.json", "spec/schemas/requirements.schema.json", "spec/schemas/conflicts.schema.json", "spec/schemas/planning-audit-evidence.schema.json", "spec/schemas/user-gate-evidence.schema.json", "spec/schemas/canonical-evidence-summary.schema.json", "spec/schemas/trusted-key-registry.schema.json"}
	err := filepath.WalkDir(filepath.Join(root, "internal/tooling"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil {
			inputs = append(inputs, filepath.ToSlash(rel))
		}
		return relErr
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(inputs)
	unique := inputs[:0]
	for _, input := range inputs {
		if len(unique) == 0 || unique[len(unique)-1] != input {
			unique = append(unique, input)
		}
	}
	return unique, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
