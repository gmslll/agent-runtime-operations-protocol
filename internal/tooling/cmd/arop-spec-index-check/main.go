package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/specindex"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/testinventory"
)

type nodeResult struct {
	Checks     []report.Check `json:"checks"`
	Errors     []string       `json:"errors"`
	InputPaths []string       `json:"input_paths"`
	Summary    map[string]any `json:"summary"`
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	dir := filepath.Join(root, "build/reports/P01")
	fatal(os.MkdirAll(dir, 0o755))
	resultPath := filepath.Join(dir, "spec-index-results.json")
	moduleRoot := filepath.Join(root, "node_modules")
	if info, statErr := os.Stat(moduleRoot); statErr != nil || !info.IsDir() {
		moduleRoot = filepath.Join(filepath.Dir(filepath.Dir(root)), "node_modules")
		info, statErr = os.Stat(moduleRoot)
		if statErr != nil {
			fatal(fmt.Errorf("locate locked node_modules: %w", statErr))
		}
		if !info.IsDir() {
			fatal(fmt.Errorf("locked node_modules path is not a directory: %s", moduleRoot))
		}
	}
	node := exec.Command("node",
		"--permission",
		"--allow-fs-read="+root,
		"--allow-fs-read="+moduleRoot,
		"--allow-fs-write="+dir,
		"--disable-proto=throw",
		"--no-addons",
		"scripts/spec-index-check.mjs",
	)
	node.Dir = root
	node.Env = append(withoutNodeLoaderEnvironment(os.Environ()), "AROP_RESULT_FILE="+resultPath)
	node.Stdout = os.Stdout
	node.Stderr = os.Stderr
	nodeErr := node.Run()
	var result nodeResult
	nodeResultBytes, readErr := os.ReadFile(resultPath)
	var resultErr error
	if readErr == nil {
		resultErr = structuredfile.Load(resultPath, &result)
	}
	combinedNodeErr := errors.Join(nodeErr, readErr, resultErr)
	if combinedNodeErr != nil {
		result.Checks = append(result.Checks, report.Check{Name: "node-schema-spec-validation", Passed: false, Detail: combinedNodeErr.Error()})
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

	inventory, inventoryErr := testinventory.Load(root)
	if inventoryErr != nil {
		result.Checks = append(result.Checks, report.Check{Name: "p01-fixed-test-inventory", Passed: false, Detail: inventoryErr.Error()})
	}
	testRun := testinventory.RunResult{}
	var testRunErr error
	if inventoryErr == nil {
		testRun, testRunErr = testinventory.Run(root, inventory)
		result.Checks = append(result.Checks, testRun.Evaluation.Checks...)
		result.Checks = append(result.Checks, report.Check{Name: "p01-fixed-test-inventory", Passed: testRun.Evaluation.Passed && testRunErr == nil, Detail: strings.Join(testRun.Evaluation.Details, "; ")})
	}
	testOut := testRun.Output
	testLog := filepath.Join(dir, "governance-tests.log")
	fatal(os.WriteFile(testLog, testOut, 0o644))
	counts := testRun.Evaluation.Counts
	goTestPassed := inventoryErr == nil && testRunErr == nil && testRun.Evaluation.Passed && counts["fail"] == 0 && counts["skip"] == 0 && counts["package_fail"] == 0 && counts["package_skip"] == 0 && counts["cache"] == 0 && counts["no_tests"] == 0 && counts["executed"] > 0
	detail := fmt.Sprintf("packages=%d package-pass=%d package-fail=%d package-skip=%d executed=%d pass=%d fail=%d skip=%d cache=%d no-tests=%d exact=%t argv=%q", counts["packages"], counts["package_pass"], counts["package_fail"], counts["package_skip"], counts["executed"], counts["pass"], counts["fail"], counts["skip"], counts["cache"], counts["no_tests"], testRun.Evaluation.Passed, strings.Join(testRun.Command, " "))
	if inventoryErr != nil {
		detail += "; " + inventoryErr.Error()
	}
	if testRunErr != nil {
		detail += "; " + testRunErr.Error()
	}
	result.Checks = append(result.Checks, report.Check{Name: "go-governance-tests", Passed: goTestPassed, Detail: detail})
	result.Summary["go_testcases_executed"] = counts["executed"]
	result.Summary["go_testcases_passed"] = counts["pass"]
	result.Summary["go_testcases_failed"] = counts["fail"]
	result.Summary["go_testcases_skipped"] = counts["skip"]
	result.Summary["go_test_packages"] = counts["packages"]
	result.Summary["go_test_packages_failed"] = counts["package_fail"]
	result.Summary["go_test_packages_passed"] = counts["package_pass"]
	result.Summary["go_test_packages_skipped"] = counts["package_skip"]
	result.Summary["go_test_cache_hits"] = counts["cache"]
	result.Summary["go_test_packages_without_tests"] = counts["no_tests"]

	seenCheckNames := make(map[string]struct{}, len(result.Checks)+1)
	var duplicateCheckNames []string
	for _, check := range result.Checks {
		if _, exists := seenCheckNames[check.Name]; exists {
			duplicateCheckNames = append(duplicateCheckNames, check.Name)
		}
		seenCheckNames[check.Name] = struct{}{}
	}
	sort.Strings(duplicateCheckNames)
	uniqueCheckNamesErr := error(nil)
	if len(duplicateCheckNames) != 0 {
		uniqueCheckNamesErr = fmt.Errorf("duplicate check names: %s", strings.Join(duplicateCheckNames, ", "))
	}
	result.Checks = append(result.Checks, report.Check{Name: "generated-check-name-uniqueness", Passed: uniqueCheckNamesErr == nil, Detail: detailOr(uniqueCheckNamesErr, "all generated check names are unique")})

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
	r, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P01", Suite: "arop-spec-index-check", Class: "arop.spec-index", Command: command, CheckerPath: "internal/tooling/cmd/arop-spec-index-check/main.go", InputPaths: inputs, RuntimeEvidence: runtimeEvidence, Checks: result.Checks, Summary: result.Summary, ControlledInputScope: "P01", AuditNote: "Node is confined to strict Schema/spec/manifest validation; Go owns governance, evidence, negative tests and canonical report generation."})
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

func detailOr(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func withoutNodeLoaderEnvironment(environment []string) []string {
	blocked := map[string]bool{"AROP_RESULT_FILE": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true}
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if !blocked[strings.ToUpper(key)] {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func governanceStaticInputs(root string) ([]string, error) {
	inputs := []string{"go.mod", "go.sum", "Makefile", "package.json", "internal/tooling/cmd/arop-spec-index-check/main.go", "internal/tooling/controlledinput/manifest.go", "internal/tooling/controlledinput/manifest_test.go", "scripts/spec-index-check.mjs", "scripts/lib/repository.mjs", "spec/p01-test-inventory.yaml", "spec/schemas/p01-test-inventory.schema.json", "spec/schemas/check-report.schema.json", "spec/schemas/artifact-manifest.schema.json", "spec/schemas/requirements.schema.json", "spec/schemas/conflicts.schema.json", "spec/schemas/planning-audit-evidence.schema.json", "spec/schemas/user-gate-evidence.schema.json", "spec/schemas/canonical-evidence-summary.schema.json", "spec/schemas/trusted-key-registry.schema.json"}
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
