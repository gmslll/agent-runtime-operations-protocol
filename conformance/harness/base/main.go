// Command base runs P06 protocol-foundation fixtures and emits the standard
// auditable JSON and JUnit reports.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/testinventory"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core"
	"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
)

const (
	expectedCommand       = "make test-protocol-foundation"
	goTestInventoryPath   = "conformance/fixtures/state-machines/base/go-test-inventory.json"
	goTestInventorySHA256 = "6e49e6acbcb8c3a3f7f379fcb3f26d52f937a1cf2c7318d1ee4d1d9b85ceb867"
)

type goTestInventory struct {
	SchemaVersion int                      `json:"schema_version"`
	Packages      []goTestInventoryPackage `json:"packages"`
}

type goTestInventoryPackage struct {
	Package string   `json:"package"`
	Tests   []string `json:"tests"`
}

type machineFixture struct {
	Machine  string         `json:"machine"`
	States   []string       `json:"states"`
	Initial  string         `json:"initial"`
	Terminal []string       `json:"terminal"`
	Accepted [][2]string    `json:"accepted"`
	Rejected [][2]string    `json:"rejected"`
	Cases    []registryCase `json:"cases,omitempty"`
}

type registrySnapshot struct {
	State      core.RegistryState `json:"state"`
	SessionID  string             `json:"session_id"`
	Generation uint64             `json:"generation"`
}
type registryExpectation struct {
	State      core.RegistryState `json:"state"`
	SessionID  string             `json:"session_id"`
	Generation uint64             `json:"generation"`
	Error      string             `json:"error"`
}
type registryCase struct {
	Name       string              `json:"name"`
	Before     registrySnapshot    `json:"before"`
	Action     string              `json:"action"`
	SessionID  string              `json:"session_id"`
	Generation uint64              `json:"generation"`
	Expect     registryExpectation `json:"expect"`
}

type offsetStep struct {
	Offset int    `json:"offset"`
	Delta  string `json:"delta"`
}
type offsetCase struct {
	Initial string       `json:"initial"`
	Offset  int          `json:"offset,omitempty"`
	Delta   string       `json:"delta,omitempty"`
	Steps   []offsetStep `json:"steps,omitempty"`
	Result  string       `json:"result,omitempty"`
	Bytes   int          `json:"bytes,omitempty"`
	Reason  string       `json:"reason,omitempty"`
}
type offsetFixture struct {
	Accepted []offsetCase `json:"accepted"`
	Rejected []offsetCase `json:"rejected"`
}

type wireFixture struct {
	ResourceIDs struct {
		Accepted [][2]string `json:"accepted"`
		Rejected [][2]string `json:"rejected"`
	} `json:"resource_ids"`
	Traceparent struct {
		Accepted []string `json:"accepted"`
		Rejected []string `json:"rejected"`
	} `json:"traceparent"`
	Tracestate struct {
		Accepted []string `json:"accepted"`
		Rejected []string `json:"rejected"`
	} `json:"tracestate"`
}

type identifierFixture struct {
	Validators []struct {
		Kind     string   `json:"kind"`
		Accepted []string `json:"accepted"`
		Rejected []string `json:"rejected"`
	} `json:"validators"`
}

type discoveryFixture struct {
	Cases []struct {
		Name               string `json:"name"`
		LeaseAlive         bool   `json:"lease_alive"`
		Healthy            bool   `json:"healthy"`
		Ready              bool   `json:"ready"`
		Enabled            bool   `json:"enabled"`
		Draining           bool   `json:"draining"`
		CapacityAvailable  bool   `json:"capacity_available"`
		ProtocolCompatible bool   `json:"protocol_compatible"`
		BindingMatches     bool   `json:"binding_matches"`
		Discoverable       bool   `json:"discoverable"`
	} `json:"cases"`
}

func main() {
	root, err := structuredfile.FindRoot(".")
	if err != nil {
		fatal(err)
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}

	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./conformance/harness/base"
	}
	var commandErr error
	if command != expectedCommand {
		commandErr = fmt.Errorf("command = %q, want exact %q", command, expectedCommand)
	}
	add("exact-command", commandErr, expectedCommand)
	inputs, err := staticInputs(root)
	if err != nil {
		fatal(fmt.Errorf("P06 static input closure rejected before execution: %w", err))
	}
	add("static-input-closure", nil, fmt.Sprintf("%d P06 source, fixture, schema, and baseline files", len(inputs)))

	runtimeEvidence := []report.RuntimeEvidence{}
	goLog, goTestChecks, goTestErr := runGoProtocolTests(root)
	checks = append(checks, goTestChecks...)
	add("go-protocol-tests", goTestErr, "all sdk/go/protocol packages passed without cache selection")
	runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "go-protocol-test-log", SHA256: report.Hash(goLog), Bytes: int64(len(goLog))})

	runMachineFixtures(root, &checks, add)
	runOffsetFixtures(root, &checks, add)
	runWireFixtures(root, &checks, add)
	runIdentifierFixtures(root, &checks, add)
	runDiscoveryFixtures(root, &checks, add)
	runErrorFixtures(root, &checks, add)
	runDocumentAndSchemaProbes(&checks, add)
	manifestLog := runManifestFixtures(root, &checks, add)
	runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "manifest-cross-language-log", SHA256: report.Hash(manifestLog), Bytes: int64(len(manifestLog))})

	result, writeErr := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P06", Suite: "arop-protocol-foundation",
		Class: "arop.protocol-foundation", Command: command,
		CheckerPath: "conformance/harness/base/main.go", InputPaths: inputs,
		RuntimeInputPaths: []string{}, RuntimeEvidence: runtimeEvidence, Checks: checks,
		Summary:   map[string]any{"fixture_root": "conformance/fixtures/state-machines/base", "runtime_input_count": 0},
		AuditNote: "P06 uses strict recursive JSON parsing, an offline schema closure, UTF-8 byte offsets, and language-neutral state-machine fixtures; runtime_inputs is intentionally empty.",
	})
	if writeErr != nil {
		fatal(writeErr)
	}
	verified, mode, verifyErr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P06/report.json"})
	if verifyErr != nil {
		fatal(verifyErr)
	}
	if mode != "current-worktree" || verified.Success != result.Success {
		fatal(fmt.Errorf("P06 report self-verification returned mode=%s success=%t", mode, verified.Success))
	}
	if !result.Success {
		fatal(errors.New("P06 protocol foundation checks failed; see build/reports/P06/report.json"))
	}
	fmt.Printf("AROP protocol foundation passed: %d checks.\n", len(result.Checks))
}

func runGoProtocolTests(root string) ([]byte, []report.Check, error) {
	inventory, inventoryChecks, err := loadGoTestInventory(root)
	if err != nil {
		return nil, inventoryChecks, err
	}
	scratch, err := os.MkdirTemp("", "arop-p06-go-test-")
	if err != nil {
		return nil, inventoryChecks, err
	}
	defer os.RemoveAll(scratch)
	cache := filepath.Join(scratch, "cache")
	temp := filepath.Join(scratch, "tmp")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, inventoryChecks, err
	}
	if err := os.MkdirAll(temp, 0o755); err != nil {
		return nil, inventoryChecks, err
	}
	command := exec.Command("go", "test", "-count=1", "-run=.", "-json", "./cmd/arop", "./sdk/go/protocol/core", "./sdk/go/protocol/manifest")
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0", "GOCACHE": cache, "GODEBUG": "", "GOENV": "off",
		"GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOTOOLCHAIN": "local",
		"GOTMPDIR": temp, "GOWORK": "off",
	})
	output, commandErr := command.CombinedOutput()
	evaluation, evaluationErr := testinventory.Evaluate(inventory, output, commandErr)
	for _, check := range evaluation.Checks {
		check.Name = "p06-" + check.Name
		inventoryChecks = append(inventoryChecks, check)
	}
	if evaluationErr != nil {
		return output, inventoryChecks, fmt.Errorf("evaluate exact Go test inventory: %w", evaluationErr)
	}
	if !evaluation.Passed {
		return output, inventoryChecks, fmt.Errorf("exact Go test inventory failed: %s", strings.Join(evaluation.Details, "; "))
	}
	return output, inventoryChecks, nil
}

func loadGoTestInventory(root string) (testinventory.Inventory, []report.Check, error) {
	checks := []report.Check{}
	path := filepath.Join(root, filepath.FromSlash(goTestInventoryPath))
	data, err := os.ReadFile(path)
	if err != nil {
		checks = append(checks, report.Check{Name: "p06-go-test-inventory-load", Passed: false, Detail: err.Error()})
		return testinventory.Inventory{}, checks, err
	}
	digest := report.Hash(data)
	digestErr := error(nil)
	if digest != goTestInventorySHA256 {
		digestErr = fmt.Errorf("inventory sha256=%s want pinned %s", digest, goTestInventorySHA256)
	}
	checks = append(checks, report.Check{Name: "p06-go-test-inventory-digest", Passed: digestErr == nil, Detail: fmt.Sprintf("sha256=%s", digest)})
	if digestErr != nil {
		return testinventory.Inventory{}, checks, digestErr
	}
	var document goTestInventory
	if err := core.DecodeAuthoring(data, &document); err != nil {
		checks = append(checks, report.Check{Name: "p06-go-test-inventory-shape", Passed: false, Detail: err.Error()})
		return testinventory.Inventory{}, checks, err
	}
	problems := []string{}
	if document.SchemaVersion != 1 {
		problems = append(problems, fmt.Sprintf("schema_version=%d want 1", document.SchemaVersion))
	}
	wantPackages := []string{
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop",
		"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core",
		"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest",
	}
	gotPackages := make([]string, 0, len(document.Packages))
	converted := testinventory.Inventory{SchemaVersion: document.SchemaVersion}
	seenTests := map[string]bool{}
	for _, pkg := range document.Packages {
		gotPackages = append(gotPackages, pkg.Package)
		if len(pkg.Tests) == 0 {
			problems = append(problems, pkg.Package+" has no tests")
		}
		previous := ""
		for _, test := range pkg.Tests {
			key := pkg.Package + "/" + test
			if test == "" || !strings.HasPrefix(test, "Test") {
				problems = append(problems, "invalid test terminal "+key)
			}
			if previous != "" && test <= previous {
				problems = append(problems, pkg.Package+" tests are not strictly sorted at "+test)
			}
			if seenTests[key] {
				problems = append(problems, "duplicate test terminal "+key)
			}
			seenTests[key] = true
			previous = test
		}
		converted.Packages = append(converted.Packages, testinventory.Package{Package: pkg.Package, Tests: append([]string(nil), pkg.Tests...)})
	}
	if !reflect.DeepEqual(gotPackages, wantPackages) {
		problems = append(problems, fmt.Sprintf("packages=%v want exact ordered set %v", gotPackages, wantPackages))
	}
	staticTests, staticErr := discoverTopLevelGoTests(root, document)
	wantTopLevelTests := []string{}
	for _, pkg := range document.Packages {
		for _, test := range pkg.Tests {
			if !strings.Contains(test, "/") {
				wantTopLevelTests = append(wantTopLevelTests, pkg.Package+"/"+test)
			}
		}
	}
	if staticErr != nil {
		problems = append(problems, staticErr.Error())
	} else if !reflect.DeepEqual(staticTests, wantTopLevelTests) {
		problems = append(problems, fmt.Sprintf("statically discovered top-level tests=%v want exact %v", staticTests, wantTopLevelTests))
	}
	shapeErr := error(nil)
	if len(problems) != 0 {
		shapeErr = errors.New(strings.Join(problems, "; "))
	}
	checks = append(checks, report.Check{Name: "p06-go-test-inventory-shape", Passed: shapeErr == nil, Detail: fmt.Sprintf("packages=%d tests=%d problems=%v", len(document.Packages), len(seenTests), problems)})
	if shapeErr != nil {
		return testinventory.Inventory{}, checks, shapeErr
	}
	return converted, checks, nil
}

func discoverTopLevelGoTests(root string, inventory goTestInventory) ([]string, error) {
	packageDirectories := map[string]string{
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop":                 "cmd/arop",
		"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/core":     "sdk/go/protocol/core",
		"github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest": "sdk/go/protocol/manifest",
	}
	discovered := []string{}
	for _, pkg := range inventory.Packages {
		directory, ok := packageDirectories[pkg.Package]
		if !ok {
			return nil, fmt.Errorf("no source directory pinned for package %s", pkg.Package)
		}
		if err := requireRealDirectoryPath(root, directory); err != nil {
			return nil, fmt.Errorf("secure Go test package %s: %w", pkg.Package, err)
		}
		absoluteDirectory := filepath.Join(root, filepath.FromSlash(directory))
		err := filepath.WalkDir(absoluteDirectory, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(absoluteDirectory, path)
			if err != nil {
				return err
			}
			if relative != "." && entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink is forbidden inside P06 Go package source: %s/%s", directory, filepath.ToSlash(relative))
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			if filepath.Dir(path) != absoluteDirectory {
				return fmt.Errorf("un-inventoried nested Go test package: %s/%s", directory, filepath.ToSlash(relative))
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspect %s: %w", entry.Name(), err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("Go test source must be a regular file: %s/%s", directory, entry.Name())
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse Go test source %s/%s: %w", directory, entry.Name(), err)
			}
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Recv != nil {
					continue
				}
				name := function.Name.Name
				if name == "TestMain" {
					return fmt.Errorf("custom TestMain is forbidden in P06 exact-inventory packages: %s/%s", directory, entry.Name())
				}
				if isGoTestName(name) {
					discovered = append(discovered, pkg.Package+"/"+name)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan Go test package %s: %w", pkg.Package, err)
		}
	}
	if err := rejectGoTestFiles(root, "conformance/harness/base"); err != nil {
		return nil, err
	}
	sort.Strings(discovered)
	return discovered, nil
}

func rejectGoTestFiles(root, directory string) error {
	return filepath.WalkDir(filepath.Join(root, filepath.FromSlash(directory)), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			relative, relativeErr := filepath.Rel(root, path)
			if relativeErr != nil {
				return relativeErr
			}
			return fmt.Errorf("uninventoried P06 harness test file is forbidden: %s", filepath.ToSlash(relative))
		}
		return nil
	})
}

func requireRealDirectoryPath(root, relative string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	current := absoluteRoot
	components := []string{"."}
	components = append(components, strings.Split(filepath.Clean(filepath.FromSlash(relative)), string(filepath.Separator))...)
	for _, component := range components {
		if component != "." {
			if component == "" || component == ".." || filepath.IsAbs(component) {
				return fmt.Errorf("invalid relative directory component %q", component)
			}
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory path component is a symlink: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("directory path component is not a directory: %s", current)
		}
	}
	return nil
}

func isGoTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") || name == "Test" {
		return false
	}
	next, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "Test"))
	return !unicode.IsLower(next)
}

func runManifestFixtures(root string, checks *[]report.Check, add func(string, error, string)) []byte {
	data, err := os.ReadFile(filepath.Join(root, "examples/manifests/digests.json"))
	if err != nil {
		add("manifest:vectors:load", err, "")
		return []byte(err.Error())
	}
	vectors := map[string]string{}
	if err := core.DecodeAuthoring(data, &vectors); err != nil {
		add("manifest:vectors:load", err, "")
		return []byte(err.Error())
	}
	paths := make([]string, 0, len(vectors))
	for path := range vectors {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	validFiles, validGlobErr := manifestFixturePaths(root, "valid")
	var vectorCoverageErr error
	if validGlobErr != nil {
		vectorCoverageErr = validGlobErr
	} else if !sameStringSet(paths, validFiles) || len(paths) == 0 {
		vectorCoverageErr = fmt.Errorf("digest vector paths=%v want exact valid manifests=%v", paths, validFiles)
	}
	add("manifest:vectors:coverage", vectorCoverageErr, fmt.Sprintf("%d digest vectors exactly cover valid manifests", len(vectors)))
	var evidence bytes.Buffer
	for _, path := range paths {
		expected := vectors[path]
		actual, goErr := manifest.DigestFile(filepath.Join(root, filepath.FromSlash(path)))
		if goErr == nil && actual != expected {
			goErr = fmt.Errorf("digest=%s want=%s", actual, expected)
		}
		add("manifest:go-valid:"+path, goErr, expected)
		stdout, stderr, nodeErr := runNodeManifestDigest(root, path)
		if nodeErr == nil && strings.TrimSpace(string(stdout)) != expected {
			nodeErr = fmt.Errorf("digest=%s want=%s", strings.TrimSpace(string(stdout)), expected)
		}
		if nodeErr == nil && len(bytes.TrimSpace(stderr)) != 0 {
			nodeErr = fmt.Errorf("unexpected stderr: %s", strings.TrimSpace(string(stderr)))
		}
		add("manifest:node-valid:"+path, nodeErr, expected)
		fmt.Fprintf(&evidence, "valid\x00%s\x00%s\x00%d\x00%d\n", path, expected, len(stdout), len(stderr))
	}
	invalidRelative, globErr := manifestFixturePaths(root, "invalid")
	if globErr != nil {
		add("manifest:invalid:coverage", globErr, "")
		return evidence.Bytes()
	}
	requiredInvalid := []string{
		"examples/manifests/invalid/draft-2019-schema.yaml",
		"examples/manifests/invalid/duplicate-skill.yaml",
		"examples/manifests/invalid/extension-digest.yaml",
		"examples/manifests/invalid/extension-invalid-data.yaml",
		"examples/manifests/invalid/extension-multiple-of.yaml",
		"examples/manifests/invalid/extension-prototype-key.yaml",
		"examples/manifests/invalid/extension-transitive-ref.yaml",
		"examples/manifests/invalid/forbidden-endpoint.yaml",
		"examples/manifests/invalid/leaked-secret.yaml",
		"examples/manifests/invalid/local-schema-path.yaml",
		"examples/manifests/invalid/nested-ref-whitespace.yaml",
		"examples/manifests/invalid/non-string-key.yaml",
		"examples/manifests/invalid/proto-key.json",
		"examples/manifests/invalid/recursive-anchor-boolean.yaml",
		"examples/manifests/invalid/recursive-anchor-string.yaml",
		"examples/manifests/invalid/recursive-ref.yaml",
		"examples/manifests/invalid/required-extension-missing-payload.yaml",
		"examples/manifests/invalid/schema-id-whitespace.yaml",
		"examples/manifests/invalid/schema-ref-backtick.yaml",
		"examples/manifests/invalid/schema-ref-brace.yaml",
		"examples/manifests/invalid/schema-ref-bracket.yaml",
		"examples/manifests/invalid/schema-ref-caret.yaml",
		"examples/manifests/invalid/schema-ref-case-mismatch.yaml",
		"examples/manifests/invalid/schema-ref-pipe.yaml",
		"examples/manifests/invalid/schema-ref-unicode.yaml",
		"examples/manifests/invalid/schema-ref-whitespace.yaml",
		"examples/manifests/invalid/timeout-range.yaml",
		"examples/manifests/invalid/unknown-field.json",
		"examples/manifests/invalid/unsafe-integer.json",
		"examples/manifests/invalid/write-without-idempotency.yaml",
	}
	coverageErr := error(nil)
	if !sameStringSet(invalidRelative, requiredInvalid) {
		coverageErr = fmt.Errorf("invalid Manifest files=%v want exact=%v", invalidRelative, requiredInvalid)
	}
	if len(invalidRelative) == 0 {
		coverageErr = errors.Join(coverageErr, errors.New("invalid Manifest corpus is empty"))
	}
	add("manifest:invalid:coverage", coverageErr, fmt.Sprintf("%d invalid manifests include every required negative", len(invalidRelative)))
	for _, relative := range invalidRelative {
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		_, goErr := manifest.DigestFile(absolute)
		if goErr == nil {
			goErr = fmt.Errorf("invalid Manifest produced a Go digest")
		} else {
			goErr = nil
		}
		add("manifest:go-invalid:"+relative, goErr, "rejected before digest")
		stdout, stderr, commandErr := runNodeManifestDigest(root, relative)
		nodeErr := error(nil)
		if commandErr == nil {
			nodeErr = fmt.Errorf("invalid Manifest produced a Node digest")
		} else if len(bytes.TrimSpace(stdout)) != 0 {
			nodeErr = fmt.Errorf("invalid Manifest wrote digest-like stdout: %s", strings.TrimSpace(string(stdout)))
		}
		add("manifest:node-invalid:"+relative, nodeErr, "rejected before digest")
		fmt.Fprintf(&evidence, "invalid\x00%s\x00%d\x00%d\n", relative, len(stdout), len(stderr))
	}
	runManifestGoCLI(root, vectors, &evidence, add)
	runManifestRelativeIDParity(root, &evidence, add)
	runManifestEmbeddedResourceParity(root, &evidence, add)
	runManifestNumberParity(root, &evidence, add)
	runManifestSchemaDialectParity(root, &evidence, add)
	runManifestSchemaProfileRejections(root, &evidence, add)
	runManifestExtensionPortableConstraints(root, &evidence, add)
	runManifestStrictParserParity(root, &evidence, add)
	runManifestSymlinkParity(root, &evidence, add)
	runManifestMakeInjectionParity(root, &evidence, add)
	return evidence.Bytes()
}

func runManifestSchemaProfileRejections(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-schema-profile-")
	if err != nil {
		add("manifest:schema-profile:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.yaml"))
	if err != nil {
		add("manifest:schema-profile:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	document := func(schema string) string {
		return fmt.Sprintf(`protocol: arop/v1
kind: AgentManifest
identity: {id: parity.schema-profile, version: 1.0.0, name: Schema profile parity, summary: Publisher schemas use the deterministic AROP Draft 2020-12 keyword profile., owner: {team: protocol-team}}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: %s
    output_schema: {type: object}
execution: {default_timeout_seconds: 30, max_timeout_seconds: 60, effects: {level: none, idempotency: supported, human_confirmation: false}, capabilities: {}}
`, schema)
	}
	cases := []struct{ name, schema string }{
		{"format-email", `{type: string, format: email}`},
		{"format-uri", `{type: string, format: uri}`},
		{"format-uri-reference", `{type: string, format: uri-reference}`},
		{"pattern-redos", `{type: string, pattern: "^(a+)+$"}`},
		{"pattern-backslash-q", `{type: string, pattern: '\q'}`},
		{"pattern-backslash-8", `{type: string, pattern: '\8'}`},
		{"pattern-control", `{type: string, pattern: '\c1'}`},
		{"pattern-invalid-group", `{type: string, pattern: '(?<1>a)'}`},
		{"pattern-set-operation", `{type: string, pattern: '[\p{ASCII}&&\p{Letter}]'}`},
		{"pattern-properties", `{type: object, patternProperties: {'^x': {type: string}}}`},
		{"multiple-of", `{type: number, multipleOf: 0.1}`},
	}
	for _, item := range cases {
		if err := os.WriteFile(filepath.Join(packageRoot, "manifest.yaml"), []byte(document(item.schema)), 0o600); err != nil {
			add("manifest:schema-profile:"+item.name+":setup", err, "")
			continue
		}
		_, goErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
		if goErr == nil {
			goErr = errors.New("Go accepted a forbidden publisher Schema keyword")
		} else {
			goErr = nil
		}
		add("manifest:schema-profile:"+item.name+":go", goErr, "forbidden before Schema compilation")
		stdout, stderr, commandErr := runNodeManifestDigest(root, relativeManifest)
		nodeErr := error(nil)
		if commandErr == nil {
			nodeErr = errors.New("Node accepted a forbidden publisher Schema keyword")
		} else if len(bytes.TrimSpace(stdout)) != 0 {
			nodeErr = fmt.Errorf("Node CLI wrote stdout: %s", strings.TrimSpace(string(stdout)))
		}
		add("manifest:schema-profile:"+item.name+":node", nodeErr, "forbidden before Schema compilation")
		fmt.Fprintf(evidence, "schema-profile\x00%s\x00%d\x00%d\n", item.name, len(stdout), len(stderr))
	}
}

func runManifestEmbeddedResourceParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-embedded-resource-")
	if err != nil {
		add("manifest:embedded-resource:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.json"))
	if err != nil {
		add("manifest:embedded-resource:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	document := func(inputSchema, extension string) string {
		extensionField := ""
		if extension != "" {
			extensionField = `,"extensions":{"com.example.arop.resource.v1":` + extension + `}`
		}
		return fmt.Sprintf(`{"protocol":"arop/v1","kind":"AgentManifest","identity":{"id":"parity.embedded-resource","version":"1.0.0","name":"Embedded resource parity","summary":"Embedded JSON Schema resources resolve independently of object key order.","owner":{"team":"protocol-team"}},"skills":[{"id":"default","name":"Test","invoke_modes":["params"],"input_schema":%s,"output_schema":{"type":"object"}}],"execution":{"default_timeout_seconds":30,"max_timeout_seconds":60,"effects":{"level":"none","idempotency":"supported","human_confirmation":false},"capabilities":{}}%s}`, inputSchema, extensionField)
	}
	run := func(name, manifestDocument string, valid bool) {
		if err := os.WriteFile(filepath.Join(packageRoot, "manifest.json"), []byte(manifestDocument), 0o600); err != nil {
			add("manifest:embedded-resource:"+name+":setup", err, "")
			return
		}
		goDigest, goCommandErr := manifest.DigestPackageFile(packageRoot, "manifest.json")
		stdout, stderr, nodeCommandErr := runNodeManifestDigest(root, relativeManifest)
		var goErr, nodeErr error
		if valid {
			goErr = goCommandErr
			nodeErr = nodeCommandErr
			if goErr == nil && nodeErr == nil && strings.TrimSpace(string(stdout)) != goDigest {
				nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(stdout)), goDigest)
			}
		} else {
			if goCommandErr == nil {
				goErr = errors.New("Go accepted invalid embedded-resource case")
			}
			if nodeCommandErr == nil {
				nodeErr = errors.New("Node accepted invalid embedded-resource case")
			} else if len(bytes.TrimSpace(stdout)) != 0 {
				nodeErr = fmt.Errorf("Node wrote stdout for invalid case: %s", strings.TrimSpace(string(stdout)))
			}
		}
		add("manifest:embedded-resource:"+name+":go", goErr, "embedded-resource policy enforced")
		add("manifest:embedded-resource:"+name+":node", nodeErr, "Go/Node embedded-resource parity")
		fmt.Fprintf(evidence, "embedded-resource\x00%s\x00%t\x00%d\x00%d\n", name, valid, len(stdout), len(stderr))
	}

	run("skill-sibling-id", document(`{"$ref":"embedded-input","$defs":{"embedded":{"$id":"embedded-input","type":"object","additionalProperties":false}}}`, ""), true)
	fragmentSchema := `{"$defs":{"payload":{"additionalProperties":false,"properties":{"toString":{"type":"string"}},"required":["toString"],"type":"object"}},"$ref":"#/$defs/payload"}`
	if err := os.MkdirAll(filepath.Join(packageRoot, "schemas"), 0o755); err != nil {
		add("manifest:embedded-resource:extension-setup", err, "")
		return
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "schemas", "fragment.schema"), []byte(fragmentSchema), 0o600); err != nil {
		add("manifest:embedded-resource:extension-setup", err, "")
		return
	}
	fragmentEnvelope := func(data string) string {
		return `{"schema_ref":"./schemas/fragment.schema","schema_digest":"sha256:3470ca54c63c5cf15dabca0122909d95a19f38b59d9e8a4faf1fc4c7ab31a6e8","data":` + data + `}`
	}
	run("extension-inherited-required", document(`{"type":"object"}`, fragmentEnvelope(`{}`)), false)
	run("extension-own-required", document(`{"type":"object"}`, fragmentEnvelope(`{"toString":"owned"}`)), true)
	nonFragmentSchema := `{"$defs":{"payload":{"$id":"payload","additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value"],"type":"object"}},"$ref":"payload"}`
	if err := os.WriteFile(filepath.Join(packageRoot, "schemas", "non-fragment.schema"), []byte(nonFragmentSchema), 0o600); err != nil {
		add("manifest:embedded-resource:extension-setup", err, "")
		return
	}
	nonFragmentEnvelope := `{"schema_ref":"./schemas/non-fragment.schema","schema_digest":"sha256:835c7a1aa55a3eca4081a0065d36a6e2ba9d2a7090212468309722bd11bd3440","data":{"value":"ok"}}`
	run("extension-non-fragment-id", document(`{"type":"object"}`, nonFragmentEnvelope), false)
}

func runManifestExtensionPortableConstraints(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-extension-profile-")
	if err != nil {
		add("manifest:extension-profile:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["at","code"],"properties":{"at":{"type":"string","format":"date-time"},"code":{"type":"string","pattern":"^[A-Z]{3}$"}},"additionalProperties":false}`
	if err := os.MkdirAll(filepath.Join(packageRoot, "schemas"), 0o755); err != nil {
		add("manifest:extension-profile:setup", err, "")
		return
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "schemas", "payload.schema"), []byte(schema), 0o600); err != nil {
		add("manifest:extension-profile:setup", err, "")
		return
	}
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.yaml"))
	if err != nil {
		add("manifest:extension-profile:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	document := func(at, code string) string {
		return fmt.Sprintf(`protocol: arop/v1
kind: AgentManifest
identity: {id: parity.extension-profile, version: 1.0.0, name: Extension profile parity, summary: Portable date-time and fixed ASCII patterns validate extension data identically., owner: {team: protocol-team}}
skills:
  - {id: default, name: Test, invoke_modes: [params], input_schema: {type: object}, output_schema: {type: object}}
execution: {default_timeout_seconds: 30, max_timeout_seconds: 60, effects: {level: none, idempotency: supported, human_confirmation: false}, capabilities: {}}
extensions:
  com.example.arop.audit.v1:
    schema_ref: ./schemas/payload.schema
    schema_digest: sha256:7001a8610f1d645de3a6b5f7bf280c8a75e9ece76e605e8706ab33619c1a0fbf
    data: {at: %s, code: %s}
`, strconv.Quote(at), strconv.Quote(code))
	}
	cases := []struct {
		name, at, code string
		valid          bool
	}{
		{"valid", "2020-02-29T23:59:59.123456789+23:59", "ABC", true},
		{"space-separator", "2020-01-01 00:00:00Z", "ABC", false},
		{"invalid-calendar", "2021-02-29T00:00:00Z", "ABC", false},
		{"invalid-offset", "2020-01-01T00:00:00+24:00", "ABC", false},
		{"lowercase-separators", "2020-01-01t00:00:00z", "ABC", false},
		{"pattern-case", "2020-01-01T00:00:00Z", "ABc", false},
		{"pattern-final-newline", "2020-01-01T00:00:00Z", "ABC\n", false},
	}
	for _, item := range cases {
		if err := os.WriteFile(filepath.Join(packageRoot, "manifest.yaml"), []byte(document(item.at, item.code)), 0o600); err != nil {
			add("manifest:extension-profile:"+item.name+":setup", err, "")
			continue
		}
		goDigest, goCommandErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
		stdout, stderr, nodeCommandErr := runNodeManifestDigest(root, relativeManifest)
		var goErr, nodeErr error
		if item.valid {
			goErr = goCommandErr
			nodeErr = nodeCommandErr
			if goErr == nil && nodeErr == nil && strings.TrimSpace(string(stdout)) != goDigest {
				nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(stdout)), goDigest)
			}
		} else {
			if goCommandErr == nil {
				goErr = errors.New("Go accepted invalid extension data")
			}
			if nodeCommandErr == nil {
				nodeErr = errors.New("Node accepted invalid extension data")
			} else if len(bytes.TrimSpace(stdout)) != 0 {
				nodeErr = fmt.Errorf("Node wrote stdout for invalid extension data: %s", strings.TrimSpace(string(stdout)))
			}
		}
		add("manifest:extension-profile:"+item.name+":go", goErr, "portable extension payload validation")
		add("manifest:extension-profile:"+item.name+":node", nodeErr, "portable extension payload validation")
		fmt.Fprintf(evidence, "extension-profile\x00%s\x00%t\x00%d\x00%d\n", item.name, item.valid, len(stdout), len(stderr))
	}
}

func runManifestSchemaDialectParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-schema-dialect-")
	if err != nil {
		add("manifest:schema-dialect:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.yaml"))
	if err != nil {
		add("manifest:schema-dialect:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	document := func(schema string) string {
		return fmt.Sprintf(`protocol: arop/v1
kind: AgentManifest
identity: {id: parity.schema-dialect, version: 1.0.0, name: Schema dialect parity, summary: Standard Draft 2020-12 schemas have the same Go and Node acceptance set., owner: {team: protocol-team}}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: %s
    output_schema: {type: object}
execution: {default_timeout_seconds: 30, max_timeout_seconds: 60, effects: {level: none, idempotency: supported, human_confirmation: false}, capabilities: {}}
`, schema)
	}
	cases := []struct{ name, schema string }{
		{"type-array", `{type: [string, number]}`},
		{"if-only", `{if: {const: x}}`},
		{"then-only", `{then: {type: string}}`},
		{"min-contains", `{minContains: 1}`},
		{"max-contains", `{maxContains: 2}`},
		{"prefix-items", `{prefixItems: [{type: string}]}`},
		{"format-date-time", `{type: string, format: date-time}`},
		{"portable-pattern", `{type: string, pattern: '^[A-Z]{3}$'}`},
	}
	for _, item := range cases {
		if err := os.WriteFile(filepath.Join(packageRoot, "manifest.yaml"), []byte(document(item.schema)), 0o600); err != nil {
			add("manifest:schema-dialect:"+item.name+":setup", err, "")
			continue
		}
		goDigest, goErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
		add("manifest:schema-dialect:"+item.name+":go", goErr, "valid Draft 2020-12 schema accepted")
		stdout, stderr, nodeErr := runNodeManifestDigest(root, relativeManifest)
		if nodeErr == nil && strings.TrimSpace(string(stdout)) != goDigest {
			nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(stdout)), goDigest)
		}
		if nodeErr == nil && len(bytes.TrimSpace(stderr)) != 0 {
			nodeErr = fmt.Errorf("unexpected stderr: %s", strings.TrimSpace(string(stderr)))
		}
		add("manifest:schema-dialect:"+item.name+":node", nodeErr, "Go/Node Draft 2020-12 acceptance parity")
		fmt.Fprintf(evidence, "schema-dialect\x00%s\x00%s\x00%d\x00%d\n", item.name, goDigest, len(stdout), len(stderr))
	}
}

func runManifestMakeInjectionParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	testRoot, err := os.MkdirTemp(root, ".p06-make-injection-")
	if err != nil {
		add("manifest:make-injection:setup", err, "")
		return
	}
	defer os.RemoveAll(testRoot)
	cases := []struct{ name, payload string }{
		{"semicolon", "missing.yaml;touch " + filepath.Join(testRoot, "semicolon")},
		{"quote", "missing' ; touch " + filepath.Join(testRoot, "quote")},
		{"subshell", "$(touch " + filepath.Join(testRoot, "subshell") + ")"},
		{"newline", "missing.yaml\ntouch " + filepath.Join(testRoot, "newline")},
	}
	for _, item := range cases {
		sentinel := filepath.Join(testRoot, item.name)
		command := exec.Command("make", "manifest-digest", "FILE="+item.payload)
		command.Dir = root
		command.Env = cleanEnvironment(os.Environ(), map[string]string{})
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		commandErr := command.Run()
		var checkErr error
		if commandErr == nil {
			checkErr = errors.New("injection payload unexpectedly produced a digest")
		}
		if _, statErr := os.Lstat(sentinel); statErr == nil {
			checkErr = errors.Join(checkErr, errors.New("injection payload created its sentinel"))
		} else if !os.IsNotExist(statErr) {
			checkErr = errors.Join(checkErr, statErr)
		}
		if bytes.Contains(stdout.Bytes(), []byte("sha256:")) {
			checkErr = errors.Join(checkErr, fmt.Errorf("injection payload wrote digest stdout: %s", strings.TrimSpace(stdout.String())))
		}
		add("manifest:make-injection:"+item.name, checkErr, "payload remains inert and produces no digest")
		fmt.Fprintf(evidence, "make-injection\x00%s\x00%d\x00%d\n", item.name, stdout.Len(), stderr.Len())
	}
}

func runManifestGoCLI(root string, vectors map[string]string, evidence *bytes.Buffer, add func(string, error, string)) {
	const valid = "examples/manifests/valid/complete.yaml"
	stdout, stderr, err := runGoManifestDigest(root, valid)
	expected := vectors[valid]
	if err == nil && strings.TrimSpace(string(stdout)) != expected {
		err = fmt.Errorf("digest=%s want=%s", strings.TrimSpace(string(stdout)), expected)
	}
	if err == nil && len(bytes.TrimSpace(stderr)) != 0 {
		err = fmt.Errorf("unexpected stderr: %s", strings.TrimSpace(string(stderr)))
	}
	add("manifest:go-cli:valid-relative-ref", err, "Go CLI uses DigestFile and matches the golden digest")
	fmt.Fprintf(evidence, "go-cli-valid\x00%s\x00%d\x00%d\n", expected, len(stdout), len(stderr))

	const invalid = "examples/manifests/invalid/extension-invalid-data.yaml"
	stdout, stderr, commandErr := runGoManifestDigest(root, invalid)
	err = nil
	if commandErr == nil {
		err = errors.New("Go CLI accepted an invalid Manifest")
	} else if len(bytes.TrimSpace(stdout)) != 0 {
		err = fmt.Errorf("invalid Manifest wrote stdout: %s", strings.TrimSpace(string(stdout)))
	}
	add("manifest:go-cli:invalid-empty-stdout", err, "Go CLI rejects before emitting a digest")
	fmt.Fprintf(evidence, "go-cli-invalid\x00%d\x00%d\n", len(stdout), len(stderr))
}

func runManifestNumberParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-number-")
	if err != nil {
		add("manifest:number:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.yaml"))
	if err != nil {
		add("manifest:number:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	document := func(number string) string {
		return fmt.Sprintf(`protocol: arop/v1
kind: AgentManifest
identity:
  id: parity.number
  version: 1.0.0
  name: Number parity
  summary: Exercises interoperable JSON-compatible YAML number normalization.
  owner: {team: protocol-team}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: {const: %s}
    output_schema: {type: object}
execution:
  default_timeout_seconds: 30
  max_timeout_seconds: 60
  effects: {level: none, idempotency: supported, human_confirmation: false}
  capabilities: {}
`, number)
	}
	manifestPath := filepath.Join(packageRoot, "manifest.yaml")
	for _, number := range []string{"0e-400", "0.000e999"} {
		if err := os.WriteFile(manifestPath, []byte(document(number)), 0o600); err != nil {
			add("manifest:number:accepted:"+number+":setup", err, "")
			continue
		}
		goDigest, goErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
		add("manifest:number:accepted:"+number+":go", goErr, "exact zero accepted")
		stdout, stderr, nodeErr := runNodeManifestDigest(root, relativeManifest)
		if nodeErr == nil && strings.TrimSpace(string(stdout)) != goDigest {
			nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(stdout)), goDigest)
		}
		if nodeErr == nil && len(bytes.TrimSpace(stderr)) != 0 {
			nodeErr = fmt.Errorf("unexpected stderr: %s", strings.TrimSpace(string(stderr)))
		}
		add("manifest:number:accepted:"+number+":node", nodeErr, "Go/Node exact-zero parity")
		fmt.Fprintf(evidence, "number-valid\x00%s\x00%s\x00%d\x00%d\n", number, goDigest, len(stdout), len(stderr))
	}

	if err := os.WriteFile(manifestPath, []byte(document("1e-4000")), 0o600); err != nil {
		add("manifest:number:underflow:setup", err, "")
		return
	}
	_, goErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
	if goErr == nil {
		goErr = errors.New("Go accepted nonzero numeric underflow")
	} else {
		goErr = nil
	}
	add("manifest:number:underflow:go", goErr, "nonzero underflow rejected")
	stdout, stderr, commandErr := runNodeManifestDigest(root, relativeManifest)
	nodeErr := error(nil)
	if commandErr == nil {
		nodeErr = errors.New("Node accepted nonzero numeric underflow")
	} else if len(bytes.TrimSpace(stdout)) != 0 {
		nodeErr = fmt.Errorf("invalid Manifest wrote stdout: %s", strings.TrimSpace(string(stdout)))
	}
	add("manifest:number:underflow:node", nodeErr, "nonzero underflow rejected without digest")
	fmt.Fprintf(evidence, "number-invalid\x001e-4000\x00%d\x00%d\n", len(stdout), len(stderr))
}

func runManifestStrictParserParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-parser-")
	if err != nil {
		add("manifest:parser:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	yamlBase := []byte(`protocol: arop/v1
kind: AgentManifest
identity:
  id: parity.parser
  version: 1.0.0
  name: Parser parity
  summary: ASCII_SUMMARY
  owner: {team: protocol-team}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: {type: object}
    output_schema: {type: object}
execution:
  default_timeout_seconds: 30
  max_timeout_seconds: 60
  effects: {level: none, idempotency: supported, human_confirmation: false}
  capabilities: {}
`)
	jsonBase := `{"protocol":"arop/v1","kind":"AgentManifest","identity":{"id":"parity.parser","version":"1.0.0","name":"Parser parity","summary":%s,"owner":{"team":"protocol-team"}},"skills":[{"id":"default","name":"Test","invoke_modes":["params"],"input_schema":{"type":"object"},"output_schema":{"type":"object"}}],"execution":{"default_timeout_seconds":30,"max_timeout_seconds":60,"effects":{"level":"none","idempotency":"supported","human_confirmation":false},"capabilities":{}}}`
	invalidUTF8YAML := bytes.Replace(yamlBase, []byte("ASCII_SUMMARY"), []byte{0xff}, 1)
	invalidUTF8JSON := bytes.Replace([]byte(fmt.Sprintf(jsonBase, `"ASCII_SUMMARY"`)), []byte("ASCII_SUMMARY"), []byte{0xff}, 1)
	cases := []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"invalid-utf8.yaml", invalidUTF8YAML, false},
		{"invalid-utf8.json", invalidUTF8JSON, false},
		{"unpaired-high.json", []byte(fmt.Sprintf(jsonBase, `"\ud800"`)), false},
		{"unpaired-low.json", []byte(fmt.Sprintf(jsonBase, `"\udc00"`)), false},
		{"valid-pair.json", []byte(fmt.Sprintf(jsonBase, `"\ud83d\ude00"`)), true},
		{"anchor-alias.yaml", bytes.Replace(yamlBase, []byte("summary: ASCII_SUMMARY"), []byte("summary: &summary Parser\n  description: *summary"), 1), false},
		{"duplicate.yaml", append(append([]byte(nil), yamlBase...), []byte("kind: AgentManifest\n")...), false},
		{"multiple.yaml", append(append([]byte(nil), yamlBase...), []byte("---\nkind: AgentManifest\n")...), false},
		{"explicit-tag.yaml", bytes.Replace(yamlBase, []byte("summary: ASCII_SUMMARY"), []byte("summary: !!timestamp 2026-09-23"), 1), false},
		{"non-string-key.yaml", bytes.Replace(yamlBase, []byte("input_schema: {type: object}"), []byte("input_schema: {1: true}"), 1), false},
		{"duplicate.json", []byte(strings.Replace(fmt.Sprintf(jsonBase, `"Parser"`), `"kind":"AgentManifest",`, `"kind":"AgentManifest","kind":"AgentManifest",`, 1)), false},
		{"trailing.json", append([]byte(fmt.Sprintf(jsonBase, `"Parser"`)), []byte(` {}`)...), false},
	}
	for _, item := range cases {
		manifestPath := filepath.Join(packageRoot, item.name)
		if err := os.WriteFile(manifestPath, item.data, 0o600); err != nil {
			add("manifest:parser:"+item.name+":setup", err, "")
			continue
		}
		relative, err := filepath.Rel(root, manifestPath)
		if err != nil {
			add("manifest:parser:"+item.name+":setup", err, "")
			continue
		}
		relative = filepath.ToSlash(relative)
		goStdout, goStderr, goCommandErr := runGoManifestDigest(root, relative)
		nodeStdout, nodeStderr, nodeCommandErr := runNodeManifestDigest(root, relative)
		var goErr, nodeErr error
		if item.valid {
			if goCommandErr != nil {
				goErr = goCommandErr
			}
			if nodeCommandErr != nil {
				nodeErr = nodeCommandErr
			}
			if goErr == nil && nodeErr == nil && strings.TrimSpace(string(goStdout)) != strings.TrimSpace(string(nodeStdout)) {
				nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(nodeStdout)), strings.TrimSpace(string(goStdout)))
			}
		} else {
			if goCommandErr == nil {
				goErr = errors.New("Go CLI accepted malformed authoring bytes")
			} else if len(bytes.TrimSpace(goStdout)) != 0 {
				goErr = fmt.Errorf("Go CLI wrote stdout: %s", strings.TrimSpace(string(goStdout)))
			}
			if nodeCommandErr == nil {
				nodeErr = errors.New("Node CLI accepted malformed authoring bytes")
			} else if len(bytes.TrimSpace(nodeStdout)) != 0 {
				nodeErr = fmt.Errorf("Node CLI wrote stdout: %s", strings.TrimSpace(string(nodeStdout)))
			}
		}
		add("manifest:parser:"+item.name+":go", goErr, "strict parser behavior")
		add("manifest:parser:"+item.name+":node", nodeErr, "strict parser behavior")
		fmt.Fprintf(evidence, "parser\x00%s\x00%t\x00%d\x00%d\x00%d\x00%d\n", item.name, item.valid, len(goStdout), len(goStderr), len(nodeStdout), len(nodeStderr))
	}
}

func runManifestSymlinkParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	testRoot, err := os.MkdirTemp(root, ".p06-symlink-")
	if err != nil {
		add("manifest:symlink:setup", err, "")
		return
	}
	defer os.RemoveAll(testRoot)
	manifestDocument := []byte(`protocol: arop/v1
kind: AgentManifest
identity: {id: parity.symlink, version: 1.0.0, name: Symlink parity, summary: Secure package paths reject symlink components., owner: {team: protocol-team}}
skills:
  - {id: default, name: Test, invoke_modes: [params], input_schema: {$ref: ./schemas/value.schema}, output_schema: {type: object}}
execution: {default_timeout_seconds: 30, max_timeout_seconds: 60, effects: {level: none, idempotency: supported, human_confirmation: false}, capabilities: {}}
`)
	schemaDocument := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`)
	write := func(file string, data []byte) error {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		return os.WriteFile(file, data, 0o600)
	}

	realPackage := filepath.Join(testRoot, "real-package")
	setupErr := write(filepath.Join(realPackage, "manifest.yaml"), manifestDocument)
	setupErr = errors.Join(setupErr, write(filepath.Join(realPackage, "schemas", "value.schema"), schemaDocument))
	ancestorAlias := filepath.Join(testRoot, "ancestor-alias")
	setupErr = errors.Join(setupErr, os.Symlink(realPackage, ancestorAlias))
	manifestFinal := filepath.Join(testRoot, "manifest-final.yaml")
	setupErr = errors.Join(setupErr, os.Symlink(filepath.Join(realPackage, "manifest.yaml"), manifestFinal))

	refFinalPackage := filepath.Join(testRoot, "ref-final")
	setupErr = errors.Join(setupErr, write(filepath.Join(refFinalPackage, "manifest.yaml"), manifestDocument))
	setupErr = errors.Join(setupErr, os.MkdirAll(filepath.Join(refFinalPackage, "schemas"), 0o755))
	setupErr = errors.Join(setupErr, os.Symlink(filepath.Join(realPackage, "schemas", "value.schema"), filepath.Join(refFinalPackage, "schemas", "value.schema")))

	refParentPackage := filepath.Join(testRoot, "ref-parent")
	setupErr = errors.Join(setupErr, write(filepath.Join(refParentPackage, "manifest.yaml"), manifestDocument))
	setupErr = errors.Join(setupErr, os.Symlink(filepath.Join(realPackage, "schemas"), filepath.Join(refParentPackage, "schemas")))
	if setupErr != nil {
		add("manifest:symlink:setup", setupErr, "")
		return
	}

	cases := []struct{ name, manifestPath string }{
		{"lexical-ancestor", filepath.Join(ancestorAlias, "manifest.yaml")},
		{"manifest-final", manifestFinal},
		{"ref-final", filepath.Join(refFinalPackage, "manifest.yaml")},
		{"ref-parent", filepath.Join(refParentPackage, "manifest.yaml")},
	}
	for _, item := range cases {
		name, manifestPath := item.name, item.manifestPath
		relative, relErr := filepath.Rel(root, manifestPath)
		if relErr != nil {
			add("manifest:symlink:"+name+":setup", relErr, "")
			continue
		}
		relative = filepath.ToSlash(relative)
		goStdout, goStderr, goCommandErr := runGoManifestDigest(root, relative)
		nodeStdout, nodeStderr, nodeCommandErr := runNodeManifestDigest(root, relative)
		var goErr, nodeErr error
		if goCommandErr == nil {
			goErr = errors.New("Go CLI accepted a symlinked package path")
		} else if len(bytes.TrimSpace(goStdout)) != 0 {
			goErr = fmt.Errorf("Go CLI wrote stdout: %s", strings.TrimSpace(string(goStdout)))
		} else if !strings.Contains(strings.ToLower(goCommandErr.Error()), "symlink") {
			goErr = fmt.Errorf("Go rejection was not a symlink boundary error: %v", goCommandErr)
		}
		if nodeCommandErr == nil {
			nodeErr = errors.New("Node CLI accepted a symlinked package path")
		} else if len(bytes.TrimSpace(nodeStdout)) != 0 {
			nodeErr = fmt.Errorf("Node CLI wrote stdout: %s", strings.TrimSpace(string(nodeStdout)))
		} else if !strings.Contains(strings.ToLower(nodeCommandErr.Error()), "symlink") {
			nodeErr = fmt.Errorf("Node rejection was not a symlink boundary error: %v", nodeCommandErr)
		}
		add("manifest:symlink:"+name+":go", goErr, "symlink boundary rejected")
		add("manifest:symlink:"+name+":node", nodeErr, "symlink boundary rejected")
		fmt.Fprintf(evidence, "symlink\x00%s\x00%d\x00%d\x00%d\x00%d\n", name, len(goStdout), len(goStderr), len(nodeStdout), len(nodeStderr))
	}
}

func runManifestRelativeIDParity(root string, evidence *bytes.Buffer, add func(string, error, string)) {
	packageRoot, err := os.MkdirTemp(root, ".p06-relative-id-")
	if err != nil {
		add("manifest:relative-id:setup", err, "")
		return
	}
	defer os.RemoveAll(packageRoot)
	write := func(relative, content string) error {
		file := filepath.Join(packageRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		return os.WriteFile(file, []byte(content), 0o600)
	}
	manifestDocument := `protocol: arop/v1
kind: AgentManifest
identity:
  id: parity.relative-id
  version: 1.0.0
  name: Relative ID parity
  summary: Exercises retrieval URI handling for package-local schemas.
  owner: {team: protocol-team}
skills:
  - id: default
    name: Test
    invoke_modes: [params]
    input_schema: {$ref: ./schemas/root.schema}
    output_schema: {type: object}
execution:
  default_timeout_seconds: 30
  max_timeout_seconds: 60
  effects: {level: none, idempotency: supported, human_confirmation: false}
  capabilities: {}
`
	setupErr := write("manifest.yaml", manifestDocument)
	setupErr = errors.Join(setupErr, write("schemas/root.schema", `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$id":"sub/base.schema",
  "$ref":"leaf.schema#/$defs/x"
}`))
	setupErr = errors.Join(setupErr, write("schemas/sub/leaf.schema", `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$defs":{"x":{"type":"object"}}
}`))
	if setupErr != nil {
		add("manifest:relative-id:setup", setupErr, "")
		return
	}
	relativeManifest, err := filepath.Rel(root, filepath.Join(packageRoot, "manifest.yaml"))
	if err != nil {
		add("manifest:relative-id:setup", err, "")
		return
	}
	relativeManifest = filepath.ToSlash(relativeManifest)
	goDigest, goErr := manifest.DigestPackageFile(packageRoot, "manifest.yaml")
	add("manifest:relative-id:go-valid", goErr, "relative $id changes the ref base")
	stdout, stderr, nodeErr := runNodeManifestDigest(root, relativeManifest)
	if nodeErr == nil && strings.TrimSpace(string(stdout)) != goDigest {
		nodeErr = fmt.Errorf("Node digest=%s want Go digest=%s", strings.TrimSpace(string(stdout)), goDigest)
	}
	if nodeErr == nil && len(bytes.TrimSpace(stderr)) != 0 {
		nodeErr = fmt.Errorf("unexpected stderr: %s", strings.TrimSpace(string(stderr)))
	}
	add("manifest:relative-id:node-valid", nodeErr, "Go/Node retrieval URI parity")
	fmt.Fprintf(evidence, "relative-id-valid\x00%s\x00%d\x00%d\n", goDigest, len(stdout), len(stderr))

	duplicateSchema := `{
  "allOf":[
    {"$id":"same.schema","type":"object"},
    {"$id":"same.schema","type":"object"}
  ]
}`
	if err := write("schemas/root.schema", duplicateSchema); err != nil {
		add("manifest:relative-id:invalid-setup", err, "")
		return
	}
	_, goErr = manifest.DigestPackageFile(packageRoot, "manifest.yaml")
	if goErr == nil {
		goErr = errors.New("Go accepted duplicate resolved $id")
	} else {
		goErr = nil
	}
	add("manifest:relative-id:go-invalid", goErr, "duplicate resolved $id rejected")
	stdout, stderr, commandErr := runNodeManifestDigest(root, relativeManifest)
	nodeErr = nil
	if commandErr == nil {
		nodeErr = errors.New("Node accepted duplicate resolved $id")
	} else if len(bytes.TrimSpace(stdout)) != 0 {
		nodeErr = fmt.Errorf("invalid Manifest wrote stdout: %s", strings.TrimSpace(string(stdout)))
	}
	add("manifest:relative-id:node-invalid", nodeErr, "duplicate resolved $id rejected without digest")
	fmt.Fprintf(evidence, "relative-id-invalid\x00%d\x00%d\n", len(stdout), len(stderr))
}

func manifestFixturePaths(root, class string) ([]string, error) {
	paths := []string{}
	for _, extension := range []string{"*.json", "*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(root, "examples/manifests", class, extension))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			relative, err := filepath.Rel(root, match)
			if err != nil {
				return nil, err
			}
			paths = append(paths, filepath.ToSlash(relative))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func requireStringMembers(actual, required []string) error {
	present := make(map[string]bool, len(actual))
	for _, value := range actual {
		present[value] = true
	}
	var err error
	for _, value := range required {
		if !present[value] {
			err = errors.Join(err, fmt.Errorf("required fixture %q is missing", value))
		}
	}
	return err
}

func runNodeManifestDigest(root, relative string) ([]byte, []byte, error) {
	command := exec.Command("node", "--permission", "--allow-fs-read=.", "--disable-proto=throw", "--no-addons", "scripts/manifest-digest.mjs", relative)
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ(), map[string]string{})
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		err = fmt.Errorf("node manifest digest: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func runGoManifestDigest(root, relative string) ([]byte, []byte, error) {
	command := exec.Command("go", "run", "./cmd/arop", "manifest", "digest", relative)
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ(), map[string]string{
		"CGO_ENABLED": "0", "GOENV": "off", "GOFLAGS": "-mod=readonly",
		"GOWORK": "off", "GOTOOLCHAIN": "local",
	})
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		err = fmt.Errorf("go manifest digest: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func cleanEnvironment(inherited []string, overrides map[string]string) []string {
	blocked := map[string]bool{
		"AROP_CHECK_COMMAND": true, "GOCACHE": true, "GOCACHEPROG": true,
		"GODEBUG": true, "GOENV": true, "GOEXPERIMENT": true, "GOFLAGS": true,
		"GOMODCACHE": true, "GOPROXY": true, "GOROOT": true, "GOTOOLCHAIN": true,
		"GOTMPDIR": true, "GOWORK": true, "NODE_OPTIONS": true, "NODE_PATH": true,
		"NPM_CONFIG_NODE_OPTIONS": true,
	}
	result := make([]string, 0, len(inherited)+len(overrides))
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(key)
		if !blocked[upper] && !strings.HasPrefix(upper, "GIT_") {
			result = append(result, item)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func runMachineFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	for _, name := range []string{"attempt", "registry", "run"} {
		var fixture machineFixture
		path := "conformance/fixtures/state-machines/base/" + name + ".json"
		if err := loadStrict(root, path, &fixture); err != nil {
			add("fixture:"+name+":load", err, "")
			continue
		}
		add("fixture:"+name+":load", nil, "strict JSON fixture loaded")
		if fixture.Machine != name {
			add("fixture:"+name+":identity", fmt.Errorf("machine=%q want filename identity %q", fixture.Machine, name), "")
		} else {
			add("fixture:"+name+":identity", nil, "filename and machine identity match")
		}
		validateMachineFixture(name, fixture, add)
		if fixture.Machine == "registry" {
			runRegistryCases(fixture.Cases, add)
		}
	}
}

func validateMachineFixture(name string, fixture machineFixture, add func(string, error, string)) {
	states := map[string]bool{}
	var metadataErr error
	for _, state := range fixture.States {
		if state == "" || states[state] {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("duplicate/empty state %q", state))
		}
		states[state] = true
	}
	if !states[fixture.Initial] {
		metadataErr = errors.Join(metadataErr, fmt.Errorf("initial state %q is not declared", fixture.Initial))
	}
	terminals := map[string]bool{}
	for _, state := range fixture.Terminal {
		if !states[state] || terminals[state] {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("terminal state %q is unknown or duplicate", state))
		}
		terminals[state] = true
	}
	allowed := map[string]bool{}
	for _, transition := range fixture.Accepted {
		key := transition[0] + "\x00" + transition[1]
		if !states[transition[0]] || !states[transition[1]] || allowed[key] {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("accepted transition %s->%s is unknown or duplicate", transition[0], transition[1]))
		}
		allowed[key] = true
		if terminals[transition[0]] {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("terminal state %s has outgoing transition", transition[0]))
		}
	}
	rejected := map[string]bool{}
	for _, transition := range fixture.Rejected {
		key := transition[0] + "\x00" + transition[1]
		if !states[transition[0]] || !states[transition[1]] || rejected[key] || allowed[key] {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("rejected transition %s->%s is unknown, duplicate, or allowed", transition[0], transition[1]))
		}
		rejected[key] = true
	}
	if len(states) == 0 {
		metadataErr = errors.Join(metadataErr, errors.New("states is empty"))
	}
	expectedStates, expectedTerminals, expectedInitial, expectedErr := expectedMachineMetadata(fixture.Machine)
	if expectedErr != nil {
		metadataErr = errors.Join(metadataErr, expectedErr)
	} else {
		if !sameStringSet(fixture.States, expectedStates) {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("states=%v want exact=%v", fixture.States, expectedStates))
		}
		if !sameStringSet(fixture.Terminal, expectedTerminals) {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("terminal=%v want exact=%v", fixture.Terminal, expectedTerminals))
		}
		if fixture.Initial != expectedInitial {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("initial=%s want=%s", fixture.Initial, expectedInitial))
		}
	}
	add("fixture:"+name+":metadata", metadataErr, fmt.Sprintf("states=%d allowed=%d terminal=%d", len(states), len(allowed), len(terminals)))
	for _, from := range fixture.States {
		for _, to := range fixture.States {
			key := from + "\x00" + to
			actualErr := machineTransition(fixture.Machine, from, to)
			var err error
			if allowed[key] && actualErr != nil {
				err = fmt.Errorf("fixture allows transition but core rejected it: %w", actualErr)
			}
			if !allowed[key] && actualErr == nil {
				err = fmt.Errorf("transition absent from the unique positive set was accepted")
			}
			add(fmt.Sprintf("fixture:%s:matrix:%s->%s", name, from, to), err, fmt.Sprintf("allowed=%t", allowed[key]))
		}
	}
	for _, state := range fixture.States {
		for _, pair := range [][2]string{{"__unknown__", state}, {state, "__unknown__"}} {
			err := machineTransition(fixture.Machine, pair[0], pair[1])
			if err == nil {
				err = fmt.Errorf("unknown state transition accepted")
			} else {
				err = nil
			}
			add(fmt.Sprintf("fixture:%s:unknown:%s->%s", name, pair[0], pair[1]), err, "rejected")
		}
	}
}

func expectedMachineMetadata(machine string) ([]string, []string, string, error) {
	switch machine {
	case "run":
		return stringsOf(core.RunStates()), stringsOf(core.RunTerminalStates()), string(core.RunQueued), nil
	case "attempt":
		return stringsOf(core.AttemptStates()), stringsOf(core.AttemptTerminalStates()), string(core.AttemptCreated), nil
	case "registry":
		return stringsOf(core.RegistryStates()), []string{}, string(core.RegistryUnregistered), nil
	default:
		return nil, nil, "", fmt.Errorf("unknown machine %q", machine)
	}
}

func stringsOf[T ~string](values []T) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func samePairSet(left, right [][2]string) bool {
	leftValues := make([]string, len(left))
	rightValues := make([]string, len(right))
	for index, pair := range left {
		leftValues[index] = pair[0] + "\x00" + pair[1]
	}
	for index, pair := range right {
		rightValues[index] = pair[0] + "\x00" + pair[1]
	}
	return sameStringSet(leftValues, rightValues)
}

func runRegistryCases(cases []registryCase, add func(string, error, string)) {
	seen := map[string]bool{}
	byName := map[string]registryCase{}
	for _, item := range cases {
		var err error
		if item.Name == "" || seen[item.Name] {
			err = fmt.Errorf("duplicate/empty registry case name %q", item.Name)
		}
		seen[item.Name] = true
		byName[item.Name] = item
		before := core.RegistrySession{State: item.Before.State, SessionID: item.Before.SessionID, Generation: item.Before.Generation}
		after := before
		var actionErr error
		switch item.Action {
		case "register":
			after, actionErr = before.Register(item.SessionID)
		case "keepalive":
			after, actionErr = before.Keepalive(item.SessionID, item.Generation)
		case "drain":
			after, actionErr = before.Drain(item.SessionID, item.Generation)
		case "expire":
			after, actionErr = before.Expire(item.SessionID, item.Generation)
		case "deregister":
			after, actionErr = before.Deregister(item.SessionID, item.Generation)
		default:
			actionErr = fmt.Errorf("unknown registry action %q", item.Action)
		}
		actualError := ""
		if actionErr != nil {
			actualError = actionErr.Error()
		}
		if actualError != item.Expect.Error {
			err = errors.Join(err, fmt.Errorf("error=%q want=%q", actualError, item.Expect.Error))
		}
		if after.State != item.Expect.State || after.SessionID != item.Expect.SessionID || after.Generation != item.Expect.Generation {
			err = errors.Join(err, fmt.Errorf("after=%+v want={State:%s SessionID:%s Generation:%d}", after, item.Expect.State, item.Expect.SessionID, item.Expect.Generation))
		}
		if actionErr != nil && after != before {
			err = errors.Join(err, fmt.Errorf("failed action mutated state: before=%+v after=%+v", before, after))
		}
		add("fixture:registry:case:"+item.Name, err, item.Action+" behaved as declared")
	}
	required := []string{
		"initial-register", "new-session-fences-old-generation", "current-keepalive", "current-drain", "current-expire",
		"draining-instance-reregisters-and-remains-draining",
		"expired-lease-cannot-revive", "expired-instance-reregisters-with-new-session", "current-deregister",
		"old-session-keepalive-fenced", "old-session-drain-fenced", "old-session-deregister-fenced", "old-session-expire-fenced",
		"old-generation-keepalive-fenced", "old-generation-drain-fenced", "old-generation-deregister-fenced", "old-generation-expire-fenced",
		"same-session-register-rejected", "generation-overflow-rejected", "deregistered-instance-reregisters",
	}
	actual := make([]string, 0, len(seen))
	for name := range seen {
		actual = append(actual, name)
	}
	var coverageErr error
	if !sameStringSet(actual, required) {
		coverageErr = fmt.Errorf("registry cases=%v want exact=%v", actual, required)
	}
	coverageErr = errors.Join(coverageErr, validateRegistryCaseSemantics(byName))
	add("fixture:registry:case-coverage", coverageErr, fmt.Sprintf("%d required action/fencing cases", len(required)))
}

func validateRegistryCaseSemantics(cases map[string]registryCase) error {
	var result error
	unchangedFailure := func(item registryCase, action, errorCode string) error {
		if item.Action != action || item.Expect.Error != errorCode ||
			item.Expect.State != item.Before.State || item.Expect.SessionID != item.Before.SessionID ||
			item.Expect.Generation != item.Before.Generation {
			return fmt.Errorf("%s does not bind action=%s, error=%s, and no mutation", item.Name, action, errorCode)
		}
		return nil
	}
	for _, action := range []string{"keepalive", "drain", "deregister", "expire"} {
		name := "old-session-" + action + "-fenced"
		item := cases[name]
		if err := unchangedFailure(item, action, "INSTANCE_GENERATION_FENCED"); err != nil ||
			item.Before.State != core.RegistryRegistered || item.SessionID == item.Before.SessionID || item.Generation != item.Before.Generation {
			result = errors.Join(result, err, fmt.Errorf("%s must isolate a stale session with the current generation", name))
		}
		name = "old-generation-" + action + "-fenced"
		item = cases[name]
		if err := unchangedFailure(item, action, "INSTANCE_GENERATION_FENCED"); err != nil ||
			item.Before.State != core.RegistryRegistered || item.SessionID != item.Before.SessionID || item.Generation >= item.Before.Generation {
			result = errors.Join(result, err, fmt.Errorf("%s must isolate a stale generation of the current session", name))
		}
	}
	currentStates := map[string]core.RegistryState{
		"keepalive":  core.RegistryRegistered,
		"drain":      core.RegistryDraining,
		"expire":     core.RegistryExpired,
		"deregister": core.RegistryDeregistered,
	}
	currentBeforeStates := map[string]core.RegistryState{
		"keepalive":  core.RegistryRegistered,
		"drain":      core.RegistryRegistered,
		"expire":     core.RegistryDraining,
		"deregister": core.RegistryRegistered,
	}
	for action, expectedState := range currentStates {
		item := cases["current-"+action]
		if item.Action != action || item.Before.State != currentBeforeStates[action] ||
			item.SessionID != item.Before.SessionID || item.Generation != item.Before.Generation ||
			item.Expect.Error != "" || item.Expect.State != expectedState ||
			item.Expect.SessionID != item.Before.SessionID || item.Expect.Generation != item.Before.Generation {
			result = errors.Join(result, fmt.Errorf("current-%s does not bind current credentials and expected state %s", action, expectedState))
		}
	}
	initial := cases["initial-register"]
	if initial.Action != "register" || initial.Before.State != core.RegistryUnregistered || initial.Before.SessionID != "" || initial.Before.Generation != 0 ||
		initial.SessionID == "" || initial.Expect.Error != "" || initial.Expect.State != core.RegistryRegistered ||
		initial.Expect.SessionID != initial.SessionID || initial.Expect.Generation != 1 {
		result = errors.Join(result, errors.New("initial-register semantics drifted"))
	}
	newSession := cases["new-session-fences-old-generation"]
	if newSession.Action != "register" || newSession.Before.State != core.RegistryRegistered || newSession.SessionID == newSession.Before.SessionID ||
		newSession.Expect.Error != "" || newSession.Expect.State != core.RegistryRegistered || newSession.Expect.SessionID != newSession.SessionID ||
		newSession.Expect.Generation != newSession.Before.Generation+1 {
		result = errors.Join(result, errors.New("new-session-fences-old-generation semantics drifted"))
	}
	drainingReregister := cases["draining-instance-reregisters-and-remains-draining"]
	if drainingReregister.Action != "register" || drainingReregister.Before.State != core.RegistryDraining ||
		drainingReregister.SessionID == drainingReregister.Before.SessionID || drainingReregister.Expect.Error != "" ||
		drainingReregister.Expect.State != core.RegistryDraining || drainingReregister.Expect.SessionID != drainingReregister.SessionID ||
		drainingReregister.Expect.Generation != drainingReregister.Before.Generation+1 {
		result = errors.Join(result, errors.New("draining-instance-reregisters-and-remains-draining semantics drifted"))
	}
	expiredKeepalive := cases["expired-lease-cannot-revive"]
	if err := unchangedFailure(expiredKeepalive, "keepalive", "LEASE_EXPIRED"); err != nil || expiredKeepalive.Before.State != core.RegistryExpired ||
		expiredKeepalive.SessionID != expiredKeepalive.Before.SessionID || expiredKeepalive.Generation != expiredKeepalive.Before.Generation {
		result = errors.Join(result, err, errors.New("expired-lease-cannot-revive semantics drifted"))
	}
	for _, name := range []string{"expired-instance-reregisters-with-new-session", "deregistered-instance-reregisters"} {
		item := cases[name]
		expectedBefore := core.RegistryExpired
		if name == "deregistered-instance-reregisters" {
			expectedBefore = core.RegistryDeregistered
		}
		if item.Action != "register" || item.Before.State != expectedBefore || item.SessionID == item.Before.SessionID || item.Expect.Error != "" ||
			item.Expect.State != core.RegistryRegistered || item.Expect.SessionID != item.SessionID || item.Expect.Generation != item.Before.Generation+1 {
			result = errors.Join(result, fmt.Errorf("%s semantics drifted", name))
		}
	}
	reused := cases["same-session-register-rejected"]
	if err := unchangedFailure(reused, "register", "REGISTRY_SESSION_REUSED"); err != nil || reused.SessionID != reused.Before.SessionID {
		result = errors.Join(result, err, errors.New("same-session-register-rejected semantics drifted"))
	}
	overflow := cases["generation-overflow-rejected"]
	if err := unchangedFailure(overflow, "register", "REGISTRY_GENERATION_OVERFLOW"); err != nil || overflow.Before.Generation != ^uint64(0) || overflow.SessionID == overflow.Before.SessionID {
		result = errors.Join(result, err, errors.New("generation-overflow-rejected semantics drifted"))
	}
	return result
}

func machineTransition(machine, from, to string) error {
	switch machine {
	case "run":
		return core.ValidateRunTransition(core.RunState(from), core.RunState(to))
	case "attempt":
		return core.ValidateAttemptTransition(core.AttemptState(from), core.AttemptState(to))
	case "registry":
		return core.ValidateRegistryTransition(core.RegistryState(from), core.RegistryState(to))
	default:
		return fmt.Errorf("unknown machine %q", machine)
	}
}

func runOffsetFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	var fixture offsetFixture
	path := "conformance/fixtures/state-machines/base/utf8-offsets.json"
	if err := loadStrict(root, path, &fixture); err != nil {
		add("fixture:utf8:load", err, "")
		return
	}
	reasons := make([]string, 0, len(fixture.Rejected))
	for _, item := range fixture.Rejected {
		reasons = append(reasons, item.Reason)
	}
	expectedReasons := []string{"middle of UTF-8 code point", "not append position", "conflicting retransmission"}
	var coverageErr error
	if len(fixture.Accepted) != 3 || !sameStringSet(reasons, expectedReasons) {
		coverageErr = fmt.Errorf("accepted=%d rejected reasons=%v; want 3 and exact %v", len(fixture.Accepted), reasons, expectedReasons)
	}
	if coverageErr == nil {
		asciiCase, unicodeCase, duplicateCase := fixture.Accepted[0], fixture.Accepted[1], fixture.Accepted[2]
		if asciiCase.Initial != "" || !sameOffsetSteps(asciiCase.Steps, []offsetStep{{Offset: 0, Delta: "hello"}}) || asciiCase.Result != "hello" || asciiCase.Bytes != 5 ||
			unicodeCase.Initial != "" || !sameOffsetSteps(unicodeCase.Steps, []offsetStep{{Offset: 0, Delta: "商品"}, {Offset: 6, Delta: "😀"}, {Offset: 10, Delta: "é"}}) || unicodeCase.Result != "商品😀é" || unicodeCase.Bytes != 13 ||
			duplicateCase.Initial != "前缀" || !sameOffsetSteps(duplicateCase.Steps, []offsetStep{{Offset: 6, Delta: "ok"}, {Offset: 6, Delta: "ok"}}) || duplicateCase.Result != "前缀ok" || duplicateCase.Bytes != 8 {
			coverageErr = errors.New("accepted UTF-8 corpus differs from the exact ASCII, multilingual/emoji/combining, and duplicate-delivery authority")
		}
	}
	if coverageErr == nil {
		byReason := map[string]offsetCase{}
		for _, item := range fixture.Rejected {
			byReason[item.Reason] = item
		}
		middle, appendOnly, conflict := byReason["middle of UTF-8 code point"], byReason["not append position"], byReason["conflicting retransmission"]
		if middle.Initial != "商品" || middle.Offset != 1 || middle.Delta != "x" || len(middle.Steps) != 0 ||
			appendOnly.Initial != "商品" || appendOnly.Offset != 3 || appendOnly.Delta != "x" || len(appendOnly.Steps) != 0 ||
			conflict.Initial != "ok" || !sameOffsetSteps(conflict.Steps, []offsetStep{{Offset: 2, Delta: "a"}, {Offset: 2, Delta: "b"}}) {
			coverageErr = errors.New("rejected UTF-8 corpus differs from exact byte-boundary, append-only, and conflicting-retransmission cases")
		}
	}
	add("fixture:utf8:coverage", coverageErr, "exact ASCII, multilingual, emoji, combining, duplicate and boundary cases present")
	for index, item := range fixture.Accepted {
		accumulator, err := core.NewTextAccumulator(item.Initial)
		for _, step := range item.Steps {
			if err == nil {
				err = accumulator.Apply(step.Offset, step.Delta)
			}
		}
		if err == nil && (accumulator.String() != item.Result || accumulator.Bytes() != item.Bytes) {
			err = fmt.Errorf("result=%q bytes=%d, want %q/%d", accumulator.String(), accumulator.Bytes(), item.Result, item.Bytes)
		}
		add(fmt.Sprintf("fixture:utf8:accept:%d", index), err, "UTF-8 byte offsets agree")
	}
	for index, item := range fixture.Rejected {
		accumulator, err := core.NewTextAccumulator(item.Initial)
		steps := item.Steps
		if len(steps) == 0 {
			steps = []offsetStep{{Offset: item.Offset, Delta: item.Delta}}
		}
		for _, step := range steps {
			if err == nil {
				err = accumulator.Apply(step.Offset, step.Delta)
			}
		}
		if err == nil {
			err = fmt.Errorf("invalid offset case was accepted: %s", item.Reason)
		} else {
			err = nil
		}
		add(fmt.Sprintf("fixture:utf8:reject:%d", index), err, "rejected "+item.Reason)
	}
}

func sameOffsetSteps(left, right []offsetStep) bool {
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

func runWireFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	var fixture wireFixture
	if err := loadStrict(root, "conformance/fixtures/state-machines/base/wire-vectors.json", &fixture); err != nil {
		add("fixture:wire:load", err, "")
		return
	}
	expectedTraceAccepted := []string{
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00-ab",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-a",
	}
	expectedTraceRejected := []string{
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-",
	}
	var wireCoverageErr error
	if !sameStringSet(fixture.Traceparent.Accepted, expectedTraceAccepted) || !sameStringSet(fixture.Traceparent.Rejected, expectedTraceRejected) ||
		!sameStringSet(fixture.Tracestate.Accepted, []string{"", "vendor=value", "vendor=value,other=opaque", "供应商=值", "vendor=value\nother=value"}) ||
		len(fixture.Tracestate.Rejected) != 1 || len(fixture.Tracestate.Rejected[0]) != 513 {
		wireCoverageErr = errors.New("traceparent/tracestate corpus does not match the required positive and negative authority")
	}
	add("fixture:wire:coverage", wireCoverageErr, "exact W3C structural, semantic and future-version cases present")
	traceSchema, schemaErr := os.ReadFile(filepath.Join(root, "schemas/common/trace.schema.json"))
	const traceSchemaID = "https://arop.invalid/schemas/v1/common/trace.schema.json"
	var traceSet *core.SchemaSet
	if schemaErr == nil {
		traceSet, schemaErr = core.NewSchemaSet(map[string][]byte{traceSchemaID: traceSchema})
	}
	add("fixture:traceparent:schema", schemaErr, "offline trace schema compiled")
	resourceDefinitions := map[string]string{
		"session": "sessionId", "deployment": "deploymentId", "lease": "leaseId", "run": "runId",
		"attempt": "attemptId", "event": "eventId", "asset": "assetId", "conversation": "conversationRef",
	}
	expectedResourceAccepted := [][2]string{
		{"session", "ses_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"deployment", "dep_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"lease", "lease_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"attempt", "att_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"run", "run_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"event", "evt_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"asset", "asset_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"conversation", "conv_01956e7b-9abc-7def-8abc-0123456789ab"},
	}
	expectedResourceRejected := [][2]string{
		{"session", "boot_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"deployment", "deployment_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"lease", "lease_01956e7b-9abc-6def-8abc-0123456789ab"},
		{"attempt", "attempt_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"run", "run_01956e7b-9abc-6def-8abc-0123456789ab"},
		{"event", "event_01956e7b-9abc-7def-8abc-0123456789ab"},
		{"asset", "asset_01956e7b-9abc-7def-0abc-0123456789ab"},
		{"conversation", "conversation_01956e7b-9abc-7def-8abc-0123456789ab"},
	}
	resourceSet, resourceSchemas, resourceSchemaErr := buildIdentifierSchemaSet(root, "resource", resourceDefinitions)
	add("fixture:resource-id:schema", resourceSchemaErr, "all resource ID families compiled from the offline identifiers schema")
	acceptedKinds, rejectedKinds := map[string]bool{}, map[string]bool{}
	for index, item := range fixture.ResourceIDs.Accepted {
		acceptedKinds[item[0]] = true
		err := core.ValidateResourceID(core.ResourceKind(item[0]), item[1])
		if err == nil && resourceSet != nil {
			encoded, marshalErr := json.Marshal(item[1])
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = resourceSet.Validate(resourceSchemas[item[0]], encoded)
			}
		}
		add(fmt.Sprintf("fixture:id:accept:%d", index), err, "accepted by core and schema")
	}
	for index, item := range fixture.ResourceIDs.Rejected {
		rejectedKinds[item[0]] = true
		coreErr := core.ValidateResourceID(core.ResourceKind(item[0]), item[1])
		encoded, marshalErr := json.Marshal(item[1])
		schemaValidationErr := marshalErr
		if schemaValidationErr == nil && resourceSet != nil {
			schemaValidationErr = resourceSet.Validate(resourceSchemas[item[0]], encoded)
		}
		var err error
		if coreErr == nil || schemaValidationErr == nil {
			err = fmt.Errorf("invalid ID accepted: core_error=%v schema_error=%v", coreErr, schemaValidationErr)
		}
		add(fmt.Sprintf("fixture:id:reject:%d", index), err, "rejected")
	}
	var resourceCoverageErr error
	if len(acceptedKinds) != len(resourceDefinitions) || len(rejectedKinds) != len(resourceDefinitions) {
		resourceCoverageErr = fmt.Errorf("accepted kinds=%d rejected kinds=%d want=%d each", len(acceptedKinds), len(rejectedKinds), len(resourceDefinitions))
	}
	for kind := range resourceDefinitions {
		if !acceptedKinds[kind] || !rejectedKinds[kind] {
			resourceCoverageErr = errors.Join(resourceCoverageErr, fmt.Errorf("resource kind %s lacks positive or negative vector", kind))
		}
	}
	if !samePairSet(fixture.ResourceIDs.Accepted, expectedResourceAccepted) || !samePairSet(fixture.ResourceIDs.Rejected, expectedResourceRejected) {
		resourceCoverageErr = errors.Join(resourceCoverageErr, errors.New("resource ID corpus differs from the exact prefix, UUID version/variant, and legacy-alias authority"))
	}
	add("fixture:resource-id:coverage", resourceCoverageErr, "all 8 resource ID prefixes have schema/core positive and negative vectors")
	for index, value := range fixture.Traceparent.Accepted {
		err := core.ValidateTraceParent(value)
		if err == nil && traceSet != nil {
			document, marshalErr := json.Marshal(core.TraceContext{Traceparent: value})
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = traceSet.Validate(traceSchemaID, document)
			}
		}
		add(fmt.Sprintf("fixture:traceparent:accept:%d", index), err, "accepted by core and structural schema")
	}
	for index, value := range fixture.Traceparent.Rejected {
		coreErr := core.ValidateTraceParent(value)
		document, marshalErr := json.Marshal(core.TraceContext{Traceparent: value})
		schemaValidationErr := marshalErr
		if schemaValidationErr == nil && traceSet != nil {
			schemaValidationErr = traceSet.Validate(traceSchemaID, document)
		}
		var err error
		if coreErr == nil || schemaValidationErr == nil {
			err = fmt.Errorf("invalid traceparent accepted: core_error=%v schema_error=%v", coreErr, schemaValidationErr)
		}
		add(fmt.Sprintf("fixture:traceparent:reject:%d", index), err, "rejected")
	}
	validTraceparent := fixture.Traceparent.Accepted[0]
	for index, value := range fixture.Tracestate.Accepted {
		context := core.TraceContext{Traceparent: validTraceparent, Tracestate: value}
		err := context.Validate()
		if err == nil && traceSet != nil {
			document, marshalErr := json.Marshal(context)
			if marshalErr != nil {
				err = marshalErr
			} else {
				err = traceSet.Validate(traceSchemaID, document)
			}
		}
		add(fmt.Sprintf("fixture:tracestate:accept:%d", index), err, "accepted by core and schema")
	}
	for index, value := range fixture.Tracestate.Rejected {
		context := core.TraceContext{Traceparent: validTraceparent, Tracestate: value}
		coreErr := context.Validate()
		document, marshalErr := json.Marshal(context)
		schemaValidationErr := marshalErr
		if schemaValidationErr == nil && traceSet != nil {
			schemaValidationErr = traceSet.Validate(traceSchemaID, document)
		}
		var err error
		if coreErr == nil || schemaValidationErr == nil {
			err = fmt.Errorf("negative accepted: core_error=%v schema_error=%v", coreErr, schemaValidationErr)
		}
		add(fmt.Sprintf("fixture:tracestate:reject:%d", index), err, "rejected by core and schema")
	}
}

func runIdentifierFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	var fixture identifierFixture
	if err := loadStrict(root, "conformance/fixtures/state-machines/base/identifiers.json", &fixture); err != nil {
		add("fixture:identifiers:load", err, "")
		return
	}
	definitionNames := map[string]string{
		"slug": "slug", "uuid_v7": "uuidV7", "effect_id": "effectId",
		"semantic_version": "semanticVersion", "sha256_digest": "sha256Digest",
		"capability_id": "capabilityId", "extension_id": "extensionId",
	}
	requiredVectors := map[string]struct {
		accepted []string
		rejected []string
	}{
		"slug": {
			accepted: []string{"image.generate", "default", "runtime-service_1"},
			rejected: []string{"", "Image.Generate", "bad space", "a..b", "-leading"},
		},
		"uuid_v7": {
			accepted: []string{"01956e7b-9abc-7def-8abc-0123456789ab"},
			rejected: []string{"01956e7b-9abc-6def-8abc-0123456789ab", "01956E7B-9ABC-7DEF-8ABC-0123456789AB", "not-a-uuid"},
		},
		"effect_id": {
			accepted: []string{"eff_order:1234", "eff_A.b-c_123"},
			rejected: []string{"eff_x", "effect_order:1234", "eff_bad/value"},
		},
		"semantic_version": {
			accepted: []string{"0.0.0", "1.0.0", "1.2.3-rc.1+build-5"},
			rejected: []string{"v1.0.0", "01.0.0", "1.0", "1.0.0+", "1.0.0-01"},
		},
		"sha256_digest": {
			accepted: []string{"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			rejected: []string{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "sha256:ABCDEF", "sha512:0123"},
		},
		"capability_id": {
			accepted: []string{"streaming.resume.v1", "delivery.worker-pull.v2"},
			rejected: []string{"streaming.v0", "Streaming.resume.v1", "streaming_resume.v1", "streaming.resume"},
		},
		"extension_id": {
			accepted: []string{"com.example.governance.v1", "io.acme.agent-tools.v2"},
			rejected: []string{"example.governance.v1", "com.Example.governance.v1", "com.example.governance.v0", "com.example.v1"},
		},
	}
	set, wrapperIDs, err := buildIdentifierSchemaSet(root, "identifier", definitionNames)
	if err != nil {
		add("fixture:identifiers:schema", err, "")
		return
	}
	add("fixture:identifiers:schema", nil, "all public identifier validators are bound to schema definitions")
	seen := map[string]bool{}
	for _, group := range fixture.Validators {
		var metadataErr error
		if seen[group.Kind] || wrapperIDs[group.Kind] == "" {
			metadataErr = fmt.Errorf("duplicate or unknown validator kind")
		}
		if len(group.Accepted) == 0 || len(group.Rejected) == 0 {
			metadataErr = errors.Join(metadataErr, errors.New("validator requires non-empty positive and negative vectors"))
		}
		required, known := requiredVectors[group.Kind]
		if known && (!sameStringSet(group.Accepted, required.accepted) || !sameStringSet(group.Rejected, required.rejected)) {
			metadataErr = errors.Join(metadataErr, fmt.Errorf("validator corpus drifted: accepted=%v rejected=%v", group.Accepted, group.Rejected))
		}
		if metadataErr != nil {
			add("fixture:identifier:"+group.Kind+":metadata", metadataErr, "")
			continue
		}
		seen[group.Kind] = true
		for index, value := range group.Accepted {
			validationErr := validateIdentifier(group.Kind, value)
			encoded, marshalErr := json.Marshal(value)
			if validationErr == nil && marshalErr == nil {
				validationErr = set.Validate(wrapperIDs[group.Kind], encoded)
			} else if validationErr == nil {
				validationErr = marshalErr
			}
			add(fmt.Sprintf("fixture:identifier:%s:accept:%d", group.Kind, index), validationErr, "accepted by core and schema")
		}
		for index, value := range group.Rejected {
			coreErr := validateIdentifier(group.Kind, value)
			encoded, marshalErr := json.Marshal(value)
			schemaValidationErr := marshalErr
			if schemaValidationErr == nil {
				schemaValidationErr = set.Validate(wrapperIDs[group.Kind], encoded)
			}
			var validationErr error
			if coreErr == nil || schemaValidationErr == nil {
				validationErr = fmt.Errorf("negative accepted: core_error=%v schema_error=%v", coreErr, schemaValidationErr)
			}
			add(fmt.Sprintf("fixture:identifier:%s:reject:%d", group.Kind, index), validationErr, "rejected by core and schema")
		}
	}
	if len(seen) != len(definitionNames) {
		add("fixture:identifiers:coverage", fmt.Errorf("validators=%d want=%d", len(seen), len(definitionNames)), "")
	} else {
		add("fixture:identifiers:coverage", nil, "every public identifier family has positive and negative vectors")
	}
}

func buildIdentifierSchemaSet(root, prefix string, definitions map[string]string) (*core.SchemaSet, map[string]string, error) {
	identifierSchema, err := os.ReadFile(filepath.Join(root, "schemas/common/identifiers.schema.json"))
	if err != nil {
		return nil, nil, err
	}
	const identifierSchemaID = "https://arop.invalid/schemas/v1/common/identifiers.schema.json"
	resources := map[string][]byte{identifierSchemaID: identifierSchema}
	wrapperIDs := map[string]string{}
	for kind, definition := range definitions {
		location := "https://arop.invalid/conformance/" + prefix + "-" + kind + ".schema.json"
		wrapperIDs[kind] = location
		resources[location] = []byte(fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":%q,"$ref":%q}`, location, identifierSchemaID+"#/$defs/"+definition))
	}
	set, err := core.NewSchemaSet(resources)
	return set, wrapperIDs, err
}

func validateIdentifier(kind, value string) error {
	switch kind {
	case "slug":
		return core.ValidateSlug(value)
	case "uuid_v7":
		return core.ValidateUUIDv7(value)
	case "effect_id":
		return core.ValidateEffectID(value)
	case "semantic_version":
		return core.ValidateSemanticVersion(value)
	case "sha256_digest":
		return core.ValidateSHA256Digest(value)
	case "capability_id":
		return core.ValidateCapabilityID(value)
	case "extension_id":
		return core.ValidateExtensionID(value)
	default:
		return fmt.Errorf("unknown identifier validator %q", kind)
	}
}

func runDiscoveryFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	var fixture discoveryFixture
	if err := loadStrict(root, "conformance/fixtures/state-machines/base/discovery.json", &fixture); err != nil {
		add("fixture:discovery:load", err, "")
		return
	}
	required := []string{"all-eligible", "lease-dead", "unhealthy", "not-ready", "disabled", "draining", "no-capacity", "protocol-incompatible", "binding-mismatch"}
	seen := map[string]bool{}
	byName := map[string]DiscoveryFacts{}
	for _, item := range fixture.Cases {
		var err error
		if item.Name == "" || seen[item.Name] {
			err = fmt.Errorf("duplicate/empty discovery case name %q", item.Name)
		}
		seen[item.Name] = true
		facts := core.DiscoveryFacts{
			LeaseAlive: item.LeaseAlive, Healthy: item.Healthy, Ready: item.Ready, Enabled: item.Enabled,
			Draining: item.Draining, CapacityAvailable: item.CapacityAvailable,
			ProtocolCompatible: item.ProtocolCompatible, BindingMatches: item.BindingMatches,
		}
		byName[item.Name] = facts
		if actual := facts.Discoverable(); actual != item.Discoverable {
			err = errors.Join(err, fmt.Errorf("discoverable=%t want=%t", actual, item.Discoverable))
		}
		add("fixture:discovery:"+item.Name, err, fmt.Sprintf("discoverable=%t", item.Discoverable))
	}
	actual := make([]string, 0, len(seen))
	for name := range seen {
		actual = append(actual, name)
	}
	var err error
	if !sameStringSet(actual, required) {
		err = fmt.Errorf("discovery cases=%v want exact=%v", actual, required)
	}
	err = errors.Join(err, validateDiscoverySemantics(byName))
	add("fixture:discovery:coverage", err, "all eight discoverability predicates are independently negative")
}

// DiscoveryFacts is kept local to the harness so fixture coverage can compare
// every dimension without relying on unexported representation details.
type DiscoveryFacts = core.DiscoveryFacts

func validateDiscoverySemantics(cases map[string]DiscoveryFacts) error {
	baseline := cases["all-eligible"]
	expectedBaseline := DiscoveryFacts{
		LeaseAlive: true, Healthy: true, Ready: true, Enabled: true,
		Draining: false, CapacityAvailable: true, ProtocolCompatible: true, BindingMatches: true,
	}
	if baseline != expectedBaseline || !baseline.Discoverable() {
		return errors.New("all-eligible discovery baseline drifted")
	}
	indices := map[string]int{
		"lease-dead": 0, "unhealthy": 1, "not-ready": 2, "disabled": 3,
		"draining": 4, "no-capacity": 5, "protocol-incompatible": 6, "binding-mismatch": 7,
	}
	vector := func(facts DiscoveryFacts) [8]bool {
		return [8]bool{facts.LeaseAlive, facts.Healthy, facts.Ready, facts.Enabled, facts.Draining, facts.CapacityAvailable, facts.ProtocolCompatible, facts.BindingMatches}
	}
	baseVector := vector(baseline)
	var result error
	for name, expectedDifference := range indices {
		candidate := vector(cases[name])
		differences := 0
		actualDifference := -1
		for index := range baseVector {
			if candidate[index] != baseVector[index] {
				differences++
				actualDifference = index
			}
		}
		if differences != 1 || actualDifference != expectedDifference || cases[name].Discoverable() {
			result = errors.Join(result, fmt.Errorf("%s must flip only discovery dimension %d from all-eligible", name, expectedDifference))
		}
	}
	return result
}

func runErrorFixtures(root string, checks *[]report.Check, add func(string, error, string)) {
	schemaPath := filepath.Join(root, "schemas/common/error.schema.json")
	schemaData, err := os.ReadFile(schemaPath)
	if err != nil {
		add("error-schema:load", err, "")
		return
	}
	const schemaID = "https://arop.invalid/schemas/v1/common/error.schema.json"
	set, err := core.NewSchemaSet(map[string][]byte{schemaID: schemaData})
	if err != nil {
		add("error-schema:compile-offline", err, "")
		return
	}
	add("error-schema:compile-offline", nil, "Draft 2020-12 schema compiled without filesystem/network loaders")
	required := map[string][]string{
		"valid":   {"dependency-unavailable.json", "integral-exponent-retry.json", "resource-version-conflict.json"},
		"invalid": {"bad-trace-id.json", "lowercase-code.json", "missing-retryable.json", "null-details.json", "null-retry-after.json", "null-retryable.json", "null-trace-id.json", "unknown-field.json"},
	}
	for _, class := range []string{"valid", "invalid"} {
		matches, globErr := filepath.Glob(filepath.Join(root, "examples/errors", class, "*.json"))
		if globErr != nil {
			add("errors:"+class+":glob", globErr, "")
			continue
		}
		sort.Strings(matches)
		basenames := make([]string, 0, len(matches))
		for _, path := range matches {
			basenames = append(basenames, filepath.Base(path))
		}
		var coverageErr error
		if !sameStringSet(basenames, required[class]) {
			coverageErr = fmt.Errorf("%s fixtures=%v want exact=%v", class, basenames, required[class])
		}
		add("errors:"+class+":coverage", coverageErr, "exact required error fixture set present")
		for _, path := range matches {
			data, readErr := os.ReadFile(path)
			semanticErr := readErr
			if semanticErr == nil {
				semanticErr = validateErrorFixtureSemantics(class, filepath.Base(path), data)
			}
			schemaValidationErr := readErr
			if schemaValidationErr == nil {
				schemaValidationErr = set.Validate(schemaID, data)
			}
			coreValidationErr := readErr
			if coreValidationErr == nil {
				var wire core.WireError
				coreValidationErr = core.DecodeAuthoring(data, &wire)
				if coreValidationErr == nil {
					coreValidationErr = wire.Validate()
				}
			}
			var validationErr error
			validationErr = errors.Join(validationErr, semanticErr)
			if class == "invalid" {
				if schemaValidationErr == nil || coreValidationErr == nil {
					validationErr = fmt.Errorf("invalid fixture accepted: schema_error=%v core_error=%v", schemaValidationErr, coreValidationErr)
				}
			} else if schemaValidationErr != nil || coreValidationErr != nil {
				validationErr = fmt.Errorf("valid fixture rejected: schema_error=%v core_error=%v", schemaValidationErr, coreValidationErr)
			}
			add("errors:"+class+":"+filepath.Base(path), validationErr, class+" fixture behaved as declared")
		}
	}
}

func validateErrorFixtureSemantics(class, name string, data []byte) error {
	parsed, err := core.ParseJSON(data)
	if err != nil {
		return err
	}
	expectedDocuments := map[string]string{
		"valid/dependency-unavailable.json":    `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"The model service is temporarily unavailable","retryable":true,"retry_after_seconds":5,"details":{},"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"}`,
		"valid/integral-exponent-retry.json":   `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"retry after one second","retryable":true,"retry_after_seconds":1e0}`,
		"valid/resource-version-conflict.json": `{"code":"RESOURCE_VERSION_CONFLICT","category":"conflict","message":"The resource version no longer matches","retryable":true}`,
		"invalid/bad-trace-id.json":            `{"code":"INTERNAL_ERROR","category":"internal","message":"bad trace identifier","retryable":false,"trace_id":"0000"}`,
		"invalid/lowercase-code.json":          `{"code":"dependency_unavailable","category":"dependency","message":"invalid lowercase code","retryable":true}`,
		"invalid/missing-retryable.json":       `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"required retryable is absent"}`,
		"invalid/null-details.json":            `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"details must not be null","retryable":true,"details":null}`,
		"invalid/null-retry-after.json":        `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"retry delay must not be null","retryable":true,"retry_after_seconds":null}`,
		"invalid/null-retryable.json":          `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"retryable must not be null","retryable":null}`,
		"invalid/null-trace-id.json":           `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"trace id must not be null","retryable":true,"trace_id":null}`,
		"invalid/unknown-field.json":           `{"code":"DEPENDENCY_UNAVAILABLE","category":"dependency","message":"unknown authoring field","retryable":true,"internal_stack":"must not cross the wire"}`,
	}
	expectedDocument, exists := expectedDocuments[class+"/"+name]
	if !exists {
		return fmt.Errorf("error fixture %s/%s has no semantic authority", class, name)
	}
	expected, err := core.ParseJSON([]byte(expectedDocument))
	if err != nil {
		return fmt.Errorf("parse embedded error fixture authority: %w", err)
	}
	if !reflect.DeepEqual(parsed, expected) {
		return fmt.Errorf("%s/%s semantic object differs from its exact single-cause authority", class, name)
	}
	return nil
}

func runDocumentAndSchemaProbes(checks *[]report.Check, add func(string, error, string)) {
	for name, document := range map[string]string{
		"top-duplicate":    `{"a":1,"a":2}`,
		"nested-duplicate": `{"a":{"b":1,"b":2}}`,
		"trailing":         `{} {}`,
	} {
		_, err := core.ParseJSON([]byte(document))
		if err == nil {
			err = fmt.Errorf("malformed document accepted")
		} else {
			err = nil
		}
		add("strict-json:"+name, err, "rejected")
	}
	_, invalidUTF8Err := core.ParseJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})
	if invalidUTF8Err == nil {
		invalidUTF8Err = fmt.Errorf("invalid UTF-8 JSON accepted")
	} else {
		invalidUTF8Err = nil
	}
	add("strict-json:invalid-utf8", invalidUTF8Err, "rejected before JSON decoding")
	for name, document := range map[string]string{
		"unpaired-high-surrogate": `{"value":"\ud800"}`,
		"unpaired-low-surrogate":  `{"value":"\udc00"}`,
	} {
		_, err := core.ParseJSON([]byte(document))
		if err == nil {
			err = fmt.Errorf("unpaired surrogate escape accepted")
		} else {
			err = nil
		}
		add("strict-json:"+name, err, "rejected before value normalization")
	}
	type envelope struct {
		Name string `json:"name"`
	}
	var strict envelope
	err := core.DecodeAuthoring([]byte(`{"name":"x","future":true}`), &strict)
	if err == nil {
		err = fmt.Errorf("authoring mode accepted unknown field")
	} else {
		err = nil
	}
	add("authoring-rejects-unknown", err, "strict authoring rejects unknown fields")
	for name, document := range map[string]string{
		"case-folded":            `{"NAME":"x"}`,
		"exact-plus-case-folded": `{"name":"x","Name":"y"}`,
	} {
		var target envelope
		err := core.DecodeAuthoring([]byte(document), &target)
		if err == nil {
			err = fmt.Errorf("case-insensitive field alias accepted")
		} else {
			err = nil
		}
		add("authoring-exact-fields:"+name, err, "rejected")
	}
	var forward envelope
	result, err := core.DecodeForward([]byte(`{"name":"x","future":true}`), &forward, func(pointer string, _ any) core.UnknownFieldAction {
		if pointer == "/future" {
			return core.UnknownPreserve
		}
		return core.UnknownReject
	})
	if err == nil && string(result.Preserved["/future"]) != "true" {
		err = fmt.Errorf("unknown optional field was not preserved")
	}
	add("forward-policy-preserves-optional", err, "forward consumer preserved explicitly allowed optional field")
	_, err = core.NewSchemaSet(map[string][]byte{"https://arop.invalid/root.json": []byte(`{"$id":"https://arop.invalid/root.json","$ref":"https://example.com/remote.json"}`)})
	if err == nil {
		err = fmt.Errorf("unbundled remote schema reference was accepted")
	} else {
		err = nil
	}
	add("offline-schema-rejects-network-ref", err, "unbundled reference rejected without network access")
}

func staticInputs(root string) ([]string, error) {
	paths := []string{
		"Makefile", "go.mod", "go.sum", "package.json", "package-lock.json", "docs/DECISIONS.md", "docs/PROTOCOL_SPECIFICATION.md",
		"docs/REGISTRY_AND_DISCOVERY.md", "docs/RUN_AND_STREAMING.md", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md",
		"spec/artifact-manifest.yaml", "spec/requirements.yaml", "spec/conflicts.yaml", "scripts/manifest-digest.mjs",
	}
	for _, path := range paths {
		if err := requireRegularFilePath(root, path); err != nil {
			return nil, fmt.Errorf("static input %s: %w", path, err)
		}
	}
	for _, directory := range []string{
		"sdk/go/protocol/core", "sdk/go/protocol/manifest", "cmd/arop", "examples/errors", "examples/manifests",
		"conformance/fixtures/state-machines/base", "conformance/harness/base", "schemas", "scripts", "spec/schemas",
		"internal/tooling/controlledinput", "internal/tooling/structuredfile", "internal/tooling/schema", "internal/tooling/report",
		"internal/tooling/testinventory",
	} {
		if err := requireRealDirectoryPath(root, directory); err != nil {
			return nil, fmt.Errorf("static input directory %s: %w", directory, err)
		}
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(directory)), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("static input closure contains symlink %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("static input closure contains non-regular file %s", path)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	unique := map[string]bool{}
	for _, path := range paths {
		unique[path] = true
	}
	paths = paths[:0]
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func requireRegularFilePath(root, relative string) error {
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
	if directory != "." {
		if err := requireRealDirectoryPath(root, directory); err != nil {
			return err
		}
	} else if err := requireRealDirectoryPath(root, "."); err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("file is a symlink: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("file is not regular: %s", path)
	}
	return nil
}

func loadStrict(root, path string, destination any) error {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return err
	}
	return core.DecodeAuthoring(data, destination)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
