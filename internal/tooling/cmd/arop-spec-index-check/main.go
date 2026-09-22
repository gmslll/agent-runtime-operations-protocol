package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
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
	fatal(os.MkdirAll(dir, 0755))
	resultPath := filepath.Join(dir, "spec-index-results.json")
	node := exec.Command("node", "scripts/spec-index-check.mjs")
	node.Dir = root
	node.Env = append(os.Environ(), "AROP_RESULT_FILE="+resultPath)
	node.Stdout = os.Stdout
	node.Stderr = os.Stderr
	nodeErr := node.Run()
	test := exec.Command("go", "test", "./internal/tooling/report", "./internal/tooling/evidence")
	test.Dir = root
	testOut, testErr := test.CombinedOutput()
	testLog := filepath.Join(dir, "governance-tests.log")
	fatal(os.WriteFile(testLog, testOut, 0644))
	var result nodeResult
	data, readErr := os.ReadFile(resultPath)
	fatal(readErr)
	fatal(json.Unmarshal(data, &result))
	if nodeErr != nil {
		result.Checks = append(result.Checks, report.Check{"node-schema-spec-validation", false, nodeErr.Error()})
	} else {
		result.Checks = append(result.Checks, report.Check{"node-schema-spec-validation", true, "Node stayed inside Schema/spec/manifest validation and returned structured results to Go"})
	}
	if testErr != nil {
		result.Checks = append(result.Checks, report.Check{"go-governance-tests", false, string(testOut)})
	} else {
		result.Checks = append(result.Checks, report.Check{"go-governance-tests", true, "Go report/evidence/lineage tests passed"})
	}
	inputs := append(result.InputPaths, "internal/tooling/cmd/arop-spec-index-check/main.go", "internal/tooling/structuredfile/files.go", filepath.ToSlash(rel(root, resultPath)), filepath.ToSlash(rel(root, testLog)))
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-spec-index-check"
	}
	r, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P01", Suite: "arop-spec-index-check", Class: "arop.spec-index", Command: command, CheckerPath: "internal/tooling/cmd/arop-spec-index-check/main.go", InputPaths: inputs, Checks: result.Checks, Summary: result.Summary, AuditNote: "Node is confined to Schema/spec/manifest checks; Go owns orchestration, evidence and canonical report generation."})
	fatal(err)
	if !r.Success {
		fmt.Fprintln(os.Stderr, "AROP spec index check failed; see build/reports/P01/report.json")
		os.Exit(1)
	}
	fmt.Printf("AROP spec index check passed: %d checks. Reports: build/reports/P01/report.json and junit.xml\n", len(r.Checks))
}
func rel(root, path string) string { v, _ := filepath.Rel(root, path); return v }
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
