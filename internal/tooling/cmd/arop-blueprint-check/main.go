package main

import (
	"fmt"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	"os"
)

func main() {
	root, err := structuredfile.FindRoot(".")
	if err != nil {
		panic(err)
	}
	checks, summary, _ := blueprint.Run(root)
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-blueprint-check"
	}
	inputs := []string{"README.md", "AGENTS.md", "Makefile", "package.json", "docs/ARCHITECTURE.md", "docs/DECISIONS.md", "docs/DEVELOPMENT_PLAN.md", "docs/DIRECTORY_STRUCTURE.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "docs/PUBLIC_PROJECT_AND_ADOPTION.md", "docs/SDK_AND_DX.md", "spec/artifact-manifest.yaml", "spec/requirements.yaml", "spec/schemas/artifact-manifest.schema.json", "spec/schemas/check-report.schema.json", "internal/tooling/blueprint/check.go", "internal/tooling/cmd/arop-blueprint-check/main.go", "internal/tooling/report/writer.go", "internal/tooling/structuredfile/files.go"}
	r, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P02", Suite: "arop-blueprint-check", Class: "arop.blueprint", Command: command, CheckerPath: "internal/tooling/cmd/arop-blueprint-check/main.go", InputPaths: inputs, Checks: checks, Summary: summary, AuditNote: "Go checks enforce planning, report-closure and implementation-runtime boundaries; they do not replace P03 independent review."})
	if err != nil {
		panic(err)
	}
	if !r.Success {
		fmt.Fprintln(os.Stderr, "AROP blueprint check failed; see build/reports/P02/report.json")
		os.Exit(1)
	}
	fmt.Printf("AROP blueprint check passed: %v phases, %v artifacts, %d checks.\n", summary["phases"], summary["artifacts"], len(r.Checks))
}
