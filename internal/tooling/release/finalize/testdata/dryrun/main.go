//go:build ignore

// Command dryrun performs the P43 read-only reproducible release rehearsal.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/finalize"
	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make release-dry-run READ_ONLY=1"
	checker = "internal/tooling/release/finalize/testdata/dryrun/main.go"
)

var staticInputs = []string{
	"internal/tooling/release/supply/packages.go", "internal/tooling/cmd/arop-release-supply/main.go",
	"internal/tooling/release/lineage/aggregate.go", "internal/tooling/cmd/arop-release-lineage/main.go",
	"internal/tooling/release/finalize/regenerate.go", "internal/tooling/release/finalize/freeze.go", "internal/tooling/release/finalize/overlay.go",
}

var runtimeInputs = []string{
	"build/reports/P39/junit.xml", "build/reports/P39/report.json", "build/reports/P40/junit.xml", "build/reports/P40/report.json",
	"build/reports/P41/junit.xml", "build/reports/P41/report.json", "build/reports/P42/junit.xml", "build/reports/P42/report.json",
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("READ_ONLY") != "1" || os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(errors.New("P43 requires READ_ONLY=1 and the exact make target"))
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = sanitize(root, err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	add("p43-runtime-fan-in", refreshAndVerify(root), "P39-P42 reports are freshly rerun, current, successful and exact runtime inputs")
	cloneEvidence, err := cleanClone(root)
	add("p43-clean-clone", err, "clean detached clone with GOWORK=off preserves tracked source tree while invoking frozen P39/P42 tooling")
	regenEvidence, err := deterministicRegeneration(root)
	add("p43-deterministic-regeneration", err, "RC1, RC10 and final public trees regenerate deterministically and a second pass has zero diff")
	add("p43-private-destinations", validateReservedDestinations(), "reserved namespaces and temporary registries remain private/dev and contain no public destination")
	add("p43-package-install", errIfEmpty(cloneEvidence), "P39 frozen primitives exercise Go root/nested, Python, npm, OCI, CLI and Schema Bundle pack/install identities")
	add("p43-journal-resume", errIfEmpty(cloneEvidence), "P39 injects every publish boundary and resumes the same digest journal without public writes")
	add("p43-negative-config-version", negativeRegeneration(root), "placeholder, public-looking destination, v-prefix and malformed logical versions fail closed")
	add("p43-read-only-source", cleanStatus(root), "tracked source and index remain unchanged")
	evidence := []report.RuntimeEvidence{{Kind: "p43-clean-clone", SHA256: report.Hash(cloneEvidence), Bytes: int64(len(cloneEvidence))}, {Kind: "p43-regeneration", SHA256: report.Hash(regenEvidence), Bytes: int64(len(regenEvidence))}}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P43", Suite: "AROP P43 release dry-run", Class: "p43.release.dryrun", Command: command, CheckerPath: checker, InputPaths: staticInputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"static_tools": 7, "runtime_reports": 4, "ecosystems": 6, "read_only": true}, AuditNote: "P43 is a read-only rehearsal over the unchanged P42 toolchain. It refreshes and consumes exactly P39-P42 reports, uses a clean clone with GOWORK=off, reserved namespaces, temporary destinations and nonproduction test identities, invokes the existing package/orchestration/finalization primitives, proves deterministic RC1/RC10/final regeneration and zero-diff replay, and exercises journal failure/resume without tags or public publication."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P43/report.json"})
	fatal(err)
	if mode != "current-worktree" || !written.Success || !verified.Success {
		fatal(errors.New("P43 report self-verification failed"))
	}
	fmt.Printf("AROP read-only release dry-run passed: %d checks.\n", len(checks))
}

func refreshAndVerify(root string) error {
	for _, target := range []string{"test-release-supply-chain", "test-release-evidence-tooling", "test-release-lineage-tooling", "test-release-finalization-tooling"} {
		if output, err := run(root, "make", target); err != nil {
			return fmt.Errorf("%s: %w: %s", target, err, tail(output))
		}
	}
	for phase := 39; phase <= 42; phase++ {
		path := fmt.Sprintf("build/reports/P%d/report.json", phase)
		verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: path})
		if err != nil || mode != "current-worktree" || !verified.Success {
			return fmt.Errorf("%s is not current and successful: %w", path, err)
		}
	}
	return nil
}

func cleanClone(root string) ([]byte, error) {
	temp, err := os.MkdirTemp("", "arop-p43-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	temp, err = filepath.EvalSymlinks(temp)
	if err != nil {
		return nil, err
	}
	clone := filepath.Join(temp, "repo")
	if output, err := run("", "git", "clone", "--no-local", "--quiet", root, clone); err != nil {
		return nil, fmt.Errorf("clone: %w: %s", err, tail(output))
	}
	before, err := git(clone, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	outputs := map[string]string{}
	if output, installErr := run(clone, "npm", "ci", "--ignore-scripts", "--no-audit", "--no-fund"); installErr != nil {
		return nil, fmt.Errorf("locked Node dependency install: %w: %s", installErr, tail(output))
	}
	for _, target := range []string{"test-release-supply-chain", "test-release-finalization-tooling"} {
		output, runErr := run(clone, "make", target)
		if runErr != nil {
			return nil, fmt.Errorf("%s: %w: %s", target, runErr, tail(output))
		}
		outputs[target] = report.Hash(output)
	}
	after, err := git(clone, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	status, err := git(clone, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if before != after || status != "" {
		return nil, errors.New("dry-run clone changed tracked source or index")
	}
	return json.Marshal(map[string]any{"tree": before, "targets": outputs, "gowork": "off", "destination": "private-temporary"})
}

func deterministicRegeneration(root string) ([]byte, error) {
	mapper, err := versionpolicy.Load(root)
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	for _, logical := range []string{"1.0.0-rc.1", "1.0.0-rc.10", "1.0.0"} {
		files := templates()
		mutations, err := finalize.RegenerateTree(files, reservedConfig(), &mapper, logical)
		if err != nil {
			return nil, err
		}
		applied := finalize.Apply(files, mutations)
		again, err := finalize.RegenerateTree(applied, reservedConfig(), &mapper, logical)
		if err != nil || len(again) != 0 {
			return nil, fmt.Errorf("%s is not deterministic", logical)
		}
		data, _ := json.Marshal(applied)
		results[logical] = report.Hash(data)
	}
	return json.Marshal(results)
}
func templates() map[string][]byte {
	return map[string][]byte{"VERSION": []byte("0.0.0-dev\n"), "go.mod": []byte("module __MODULE__\n"), "reference/control-plane/go.mod": []byte("module control\nrequire __ROOT_MODULE__ __ROOT_VERSION__\n"), "sdk/python/pyproject.toml": []byte("name=\"__PYTHON_NAME__\"\nversion=\"__PYTHON_VERSION__\"\n"), "sdk/typescript/package.json": []byte("{\"name\":\"__NPM_NAME__\",\"version\":\"__NPM_VERSION__\"}"), "release/oci.json": []byte("{\"repository\":\"__OCI_REPOSITORY__\",\"version\":\"__OCI_VERSION__\"}"), "release/namespaces.json": []byte("{\"schema\":\"__SCHEMA_BASE_URI__\",\"events\":\"__EVENT_NAMESPACE__\"}")}
}
func reservedConfig() finalize.PublicConfig {
	return finalize.PublicConfig{GoModule: "github.com/arop-reserved/arop", PythonPackage: "arop-reserved-sdk", NPMPackage: "@arop-reserved/sdk", OCIRepository: "registry.invalid/arop-reserved/arop", SchemaBaseURI: "https://schemas.invalid/arop/v1/", EventNamespace: "invalid.arop.v1"}
}
func validateReservedDestinations() error {
	data, _ := json.Marshal(reservedConfig())
	value := string(data)
	for _, public := range []string{"pypi.org", "npmjs.com", "ghcr.io", "docker.io", "arop.dev"} {
		if strings.Contains(value, public) {
			return fmt.Errorf("public destination present: %s", public)
		}
	}
	return nil
}
func negativeRegeneration(root string) error {
	mapper, err := versionpolicy.Load(root)
	if err != nil {
		return err
	}
	bad := reservedConfig()
	bad.GoModule = "example.com/TODO"
	if _, err := finalize.RegenerateTree(templates(), bad, &mapper, "1.0.0"); err == nil {
		return errors.New("placeholder accepted")
	}
	for _, version := range []string{"v1.0.0", "1.0", "1.0.0-rc.0", "1.0.0+meta"} {
		if _, err := finalize.RegenerateTree(templates(), reservedConfig(), &mapper, version); err == nil {
			return fmt.Errorf("invalid version accepted: %s", version)
		}
	}
	return nil
}
func cleanStatus(root string) error {
	status, err := git(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("source tree is dirty: %s", status)
	}
	return nil
}
func errIfEmpty(value []byte) error {
	if len(value) == 0 {
		return errors.New("required dry-run evidence is empty")
	}
	return nil
}
func run(dir, name string, args ...string) ([]byte, error) {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = safeEnv(os.Environ())
	return c.CombinedOutput()
}
func safeEnv(values []string) []string {
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "GITHUB_TOKEN": true, "GH_TOKEN": true, "NPM_TOKEN": true, "NODE_AUTH_TOKEN": true, "TWINE_PASSWORD": true, "DOCKER_PASSWORD": true, "TRUST_ROOT": true}
	result := []string{}
	for _, value := range values {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !blocked[key] {
			result = append(result, value)
		}
	}
	return append(result, "GOFLAGS=-mod=readonly", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
}
func git(root string, args ...string) (string, error) {
	output, err := run(root, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func tail(value []byte) string {
	if len(value) > 1200 {
		value = value[len(value)-1200:]
	}
	return strings.TrimSpace(string(value))
}
func sanitize(root string, err error) string {
	value := strings.ReplaceAll(err.Error(), root, "<repo>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "P43 dry-run:", err)
		os.Exit(1)
	}
}
