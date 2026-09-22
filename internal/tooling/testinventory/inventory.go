// Package testinventory loads and enforces the tracked, exact P01 Go test
// inventory. The inventory is a set of package/test terminal IDs, not a count.
package testinventory

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
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	InventoryPath = "spec/p01-test-inventory.yaml"
	SchemaPath    = "spec/schemas/p01-test-inventory.schema.json"
	modulePath    = "github.com/gmslll/agent-runtime-operations-protocol"
)

type Inventory struct {
	SchemaVersion  int       `json:"schema_version"`
	InventoryID    string    `json:"inventory_id"`
	SchemaArtifact string    `json:"schema_artifact"`
	OwnerArtifact  string    `json:"owner_artifact"`
	OwnerPhase     string    `json:"owner_phase"`
	Packages       []Package `json:"packages"`
}

type Package struct {
	Path        string   `json:"path"`
	Package     string   `json:"package"`
	ArtifactIDs []string `json:"artifact_ids"`
	OwnerPhase  string   `json:"owner_phase"`
	Tests       []string `json:"tests"`
}

type artifact struct {
	ID         string
	Path       string
	Status     string
	OwnerPhase string
}

type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

type Evaluation struct {
	Checks  []report.Check
	Counts  map[string]int
	Passed  bool
	Details []string
}

type RunResult struct {
	Evaluation Evaluation
	Output     []byte
	Command    []string
	Err        error
}

func Load(root string) (Inventory, error) {
	var inventory Inventory
	if _, _, err := schema.ValidatePath(root, SchemaPath, InventoryPath); err != nil {
		return inventory, err
	}
	if err := structuredfile.Load(filepath.Join(root, filepath.FromSlash(InventoryPath)), &inventory); err != nil {
		return inventory, err
	}
	manifestValue, _, err := schema.ValidatePath(root, "spec/schemas/artifact-manifest.schema.json", "spec/artifact-manifest.yaml")
	if err != nil {
		return inventory, err
	}
	catalog := []artifact{}
	document, ok := manifestValue.(map[string]any)
	if !ok {
		return inventory, fmt.Errorf("artifact manifest is not an object")
	}
	items, ok := document["artifacts"].([]any)
	if !ok {
		return inventory, fmt.Errorf("artifact manifest artifacts is not an array")
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return inventory, fmt.Errorf("artifact manifest entry is not an object")
		}
		catalog = append(catalog, artifact{ID: fmt.Sprint(item["id"]), Path: fmt.Sprint(item["path"]), Status: fmt.Sprint(item["status"]), OwnerPhase: stringValue(item["owner_phase"])})
	}
	if problems := inventoryProblems(root, inventory, catalog); len(problems) != 0 {
		return inventory, fmt.Errorf("invalid P01 test inventory: %s", strings.Join(problems, "; "))
	}
	return inventory, nil
}

func inventoryProblems(root string, inventory Inventory, catalog []artifact) []string {
	problems := []string{}
	artifacts := map[string]artifact{}
	for _, item := range catalog {
		artifacts[item.ID] = item
	}
	for _, binding := range []struct {
		id, path, phase string
	}{
		{inventory.SchemaArtifact, SchemaPath, inventory.OwnerPhase},
		{inventory.OwnerArtifact, InventoryPath, inventory.OwnerPhase},
	} {
		item, ok := artifacts[binding.id]
		if !ok || item.Path != binding.path || item.Status != "present" || item.OwnerPhase != binding.phase {
			problems = append(problems, fmt.Sprintf("artifact binding %s must be present at %s and owned by %s", binding.id, binding.path, binding.phase))
		}
	}
	seenPackages := map[string]bool{}
	seenPaths := map[string]bool{}
	seenTests := map[string]bool{}
	lastPackage := ""
	listedTestDirs := map[string]bool{}
	for _, pkg := range inventory.Packages {
		if seenPackages[pkg.Package] || seenPaths[pkg.Path] {
			problems = append(problems, "duplicate package/path "+pkg.Package)
		}
		seenPackages[pkg.Package], seenPaths[pkg.Path] = true, true
		if lastPackage != "" && pkg.Package <= lastPackage {
			problems = append(problems, "packages are not strictly sorted: "+pkg.Package)
		}
		lastPackage = pkg.Package
		expectedPackage := modulePath + strings.TrimPrefix(pkg.Path, ".")
		if pkg.Package != expectedPackage {
			problems = append(problems, pkg.Package+" does not match path "+pkg.Path)
		}
		dir := strings.TrimPrefix(pkg.Path, "./")
		listedTestDirs[dir] = true
		lastArtifact := ""
		for _, id := range pkg.ArtifactIDs {
			if lastArtifact != "" && id <= lastArtifact {
				problems = append(problems, pkg.Package+" artifact_ids are not strictly sorted")
			}
			lastArtifact = id
			item, ok := artifacts[id]
			if !ok || item.Status != "present" || item.OwnerPhase != pkg.OwnerPhase {
				problems = append(problems, fmt.Sprintf("%s artifact %s is absent or not owned by %s", pkg.Package, id, pkg.OwnerPhase))
				continue
			}
			artifactDir := filepath.ToSlash(filepath.Dir(item.Path))
			if artifactDir != dir {
				problems = append(problems, fmt.Sprintf("%s artifact %s is outside package path", pkg.Package, id))
			}
		}
		lastTest := ""
		for _, test := range pkg.Tests {
			key := pkg.Package + "/" + test
			if seenTests[key] {
				problems = append(problems, "duplicate test "+key)
			}
			seenTests[key] = true
			if lastTest != "" && test <= lastTest {
				problems = append(problems, pkg.Package+" tests are not strictly sorted at "+test)
			}
			lastTest = test
		}
	}
	toolingRoot := filepath.Join(root, "internal", "tooling")
	if err := filepath.WalkDir(toolingRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !listedTestDirs[rel] {
			problems = append(problems, "unlisted internal/tooling test package "+rel)
		}
		return nil
	}); err != nil {
		problems = append(problems, "scan internal/tooling tests: "+err.Error())
	}
	return problems
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func Invocation(inventory Inventory, inherited []string, cacheDir, tempDir string) ([]string, []string) {
	args := []string{"test", "-count=1", "-run=.", "-json"}
	for _, pkg := range inventory.Packages {
		args = append(args, pkg.Path)
	}
	remove := map[string]bool{
		"AROP_VERIFY_CURRENT": true,
		"GOCACHE":             true,
		"GOCACHEPROG":         true,
		"GODEBUG":             true,
		"GOENV":               true,
		"GOFLAGS":             true,
		"GOTMPDIR":            true,
		"GOWORK":              true,
	}
	env := make([]string, 0, len(inherited)+7)
	for _, item := range inherited {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		if !remove[strings.ToUpper(key)] {
			env = append(env, item)
		}
	}
	env = append(env,
		"AROP_VERIFY_CURRENT=1",
		"GOCACHE="+cacheDir,
		"GODEBUG=",
		"GOENV=off",
		"GOFLAGS=",
		"GOTMPDIR="+tempDir,
		"GOWORK=off",
	)
	return args, env
}

func Run(root string, inventory Inventory) (RunResult, error) {
	scratch, err := os.MkdirTemp("", "arop-p01-go-test-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(scratch)
	cacheDir := filepath.Join(scratch, "cache")
	tempDir := filepath.Join(scratch, "tmp")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return RunResult{}, err
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return RunResult{}, err
	}
	args, env := Invocation(inventory, os.Environ(), cacheDir, tempDir)
	command := exec.Command("go", args...)
	command.Dir = root
	command.Env = env
	output, commandErr := command.CombinedOutput()
	evaluation, parseErr := Evaluate(inventory, output, commandErr)
	if parseErr != nil {
		return RunResult{Evaluation: evaluation, Output: output, Command: append([]string{"go"}, args...), Err: commandErr}, parseErr
	}
	return RunResult{Evaluation: evaluation, Output: output, Command: append([]string{"go"}, args...), Err: commandErr}, nil
}

func Evaluate(inventory Inventory, data []byte, commandErr error) (Evaluation, error) {
	expectedPackages := map[string]Package{}
	expectedTests := map[string]bool{}
	for _, pkg := range inventory.Packages {
		expectedPackages[pkg.Package] = pkg
		for _, test := range pkg.Tests {
			expectedTests[pkg.Package+"/"+test] = true
		}
	}
	testTerminals := map[string][]goTestEvent{}
	packageTerminals := map[string][]goTestEvent{}
	packageTestCount := map[string]int{}
	cacheMarkers := 0
	noTestMarkers := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return Evaluation{}, fmt.Errorf("parse go test -json: %w", err)
		}
		if strings.Contains(event.Output, "(cached)") {
			cacheMarkers++
		}
		if strings.Contains(event.Output, "[no test files]") || strings.Contains(event.Output, "no tests to run") {
			noTestMarkers++
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		if event.Test == "" {
			packageTerminals[event.Package] = append(packageTerminals[event.Package], event)
			continue
		}
		key := event.Package + "/" + event.Test
		testTerminals[key] = append(testTerminals[key], event)
		packageTestCount[event.Package]++
	}
	if err := scanner.Err(); err != nil {
		return Evaluation{}, err
	}
	checks := []report.Check{}
	details := []string{}
	counts := map[string]int{"executed": len(testTerminals), "pass": 0, "fail": 0, "skip": 0, "packages": len(packageTerminals), "package_pass": 0, "package_fail": 0, "package_skip": 0, "cache": cacheMarkers, "no_tests": 0}
	for _, events := range testTerminals {
		for _, event := range events {
			counts[event.Action]++
		}
	}
	for name, events := range packageTerminals {
		for _, event := range events {
			counts["package_"+event.Action]++
		}
		if packageTestCount[name] == 0 {
			counts["no_tests"]++
		}
	}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{Name: name, Passed: passed, Detail: detail})
		if !passed {
			details = append(details, name+": "+detail)
		}
	}
	actualPackageNames := sortedKeys(packageTerminals)
	expectedPackageNames := sortedPackageKeys(expectedPackages)
	add("go-package-terminal-set", sameStrings(actualPackageNames, expectedPackageNames), setDifferenceDetail(expectedPackageNames, actualPackageNames))
	for _, name := range expectedPackageNames {
		events := packageTerminals[name]
		passed := len(events) == 1 && events[0].Action == "pass"
		detail := terminalDetail(events)
		if packageTestCount[name] == 0 {
			passed = false
			detail += "; no testcase terminal events"
		}
		add("go-package:"+name, passed, detail)
	}
	actualTestNames := sortedKeys(testTerminals)
	expectedTestNames := sortedBoolKeys(expectedTests)
	add("go-test-terminal-set", sameStrings(actualTestNames, expectedTestNames), setDifferenceDetail(expectedTestNames, actualTestNames))
	for _, name := range expectedTestNames {
		events := testTerminals[name]
		passed := len(events) == 1 && events[0].Action == "pass"
		add("go-test:"+name, passed, terminalDetail(events))
	}
	for _, name := range actualTestNames {
		if expectedTests[name] {
			continue
		}
		events := testTerminals[name]
		add("go-test-unexpected:"+name, false, terminalDetail(events))
	}
	add("go-test-command-exit", commandErr == nil, fmt.Sprint(commandErr))
	add("go-test-no-fail", counts["fail"] == 0 && counts["package_fail"] == 0, fmt.Sprintf("test_fail=%d package_fail=%d", counts["fail"], counts["package_fail"]))
	add("go-test-no-skip", counts["skip"] == 0 && counts["package_skip"] == 0, fmt.Sprintf("test_skip=%d package_skip=%d", counts["skip"], counts["package_skip"]))
	add("go-test-no-cache", cacheMarkers == 0, fmt.Sprintf("cache=%d", cacheMarkers))
	add("go-test-no-empty-package", counts["no_tests"] == 0 && noTestMarkers == 0, fmt.Sprintf("empty_packages=%d markers=%d", counts["no_tests"], noTestMarkers))
	return Evaluation{Checks: checks, Counts: counts, Passed: len(details) == 0, Details: details}, nil
}

func terminalDetail(events []goTestEvent) string {
	if len(events) == 0 {
		return "missing terminal event"
	}
	parts := make([]string, 0, len(events))
	for _, event := range events {
		parts = append(parts, fmt.Sprintf("action=%s elapsed=%.3fs", event.Action, event.Elapsed))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(values map[string]bool) []string { return sortedKeys(values) }

func sortedPackageKeys(values map[string]Package) []string { return sortedKeys(values) }

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func setDifferenceDetail(expected, actual []string) string {
	expectedSet := map[string]bool{}
	actualSet := map[string]bool{}
	for _, value := range expected {
		expectedSet[value] = true
	}
	for _, value := range actual {
		actualSet[value] = true
	}
	missing := []string{}
	extra := []string{}
	for _, value := range expected {
		if !actualSet[value] {
			missing = append(missing, value)
		}
	}
	for _, value := range actual {
		if !expectedSet[value] {
			extra = append(extra, value)
		}
	}
	return fmt.Sprintf("expected=%d actual=%d missing=%v extra=%v", len(expected), len(actual), missing, extra)
}
