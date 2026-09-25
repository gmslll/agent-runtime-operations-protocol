// Command harness executes the deterministic P08 Control Plane platform
// acceptance suite and emits the repository's standard JSON and JUnit reports.
// It deliberately treats package tests as untrusted evidence: package and
// top-level testcase terminals are rediscovered from go test -json output.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	expectedCommand = "make test-control-plane-platform"
	checkerPath     = "reference/control-plane/internal/app/platform/testdata/harness/main.go"
	reportPath      = "build/reports/P08/report.json"
	p10WaiverPath   = "reference/control-plane/internal/identity/testdata/transition/p10-baseline-transition-waiver.json"
	nestedModule    = "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane"
	rootModule      = "github.com/gmslll/agent-runtime-operations-protocol"
	p13WaiverPath   = "reference/control-plane/internal/domain/assets/testdata/transition/p13-baseline-transition-waiver.json"
)

var rootRequirementPattern = regexp.MustCompile(`(?m)^\s*(?:require\s+)?github\.com/gmslll/agent-runtime-operations-protocol\s+(v0\.0\.0-([0-9]{14})-([0-9a-f]{12}))\s*$`)

type modulePin struct {
	Path    string
	Version string
}

var rootExternalModules = []modulePin{
	{Path: "github.com/cyberphone/json-canonicalization", Version: "v0.0.0-20241213102144-19d51d7fe467"},
	{Path: "github.com/santhosh-tekuri/jsonschema/v6", Version: "v6.0.2"},
	{Path: "golang.org/x/text", Version: "v0.14.0"},
}

var nestedExternalModules = []modulePin{
	{Path: "github.com/cyberphone/json-canonicalization", Version: "v0.0.0-20241213102144-19d51d7fe467"},
	{Path: "github.com/dustin/go-humanize", Version: "v1.0.1"},
	{Path: "github.com/gofrs/flock", Version: "v0.13.0"},
	{Path: "github.com/google/uuid", Version: "v1.6.0"},
	{Path: "github.com/jackc/pgpassfile", Version: "v1.0.0"},
	{Path: "github.com/jackc/pgservicefile", Version: "v0.0.0-20240606120523-5a60cdf6a761"},
	{Path: "github.com/jackc/pgx/v5", Version: "v5.8.0"},
	{Path: "github.com/jackc/puddle/v2", Version: "v2.2.2"},
	{Path: "github.com/mattn/go-isatty", Version: "v0.0.20"},
	{Path: "github.com/ncruces/go-strftime", Version: "v1.0.0"},
	{Path: "github.com/remyoudompheng/bigfft", Version: "v0.0.0-20230129092748-24d4a6f8daec"},
	{Path: "github.com/santhosh-tekuri/jsonschema/v6", Version: "v6.0.2"},
	{Path: "golang.org/x/exp", Version: "v0.0.0-20251023183803-a4bb9ffd2546"},
	{Path: "golang.org/x/sync", Version: "v0.17.0"},
	{Path: "golang.org/x/sys", Version: "v0.37.0"},
	{Path: "golang.org/x/text", Version: "v0.29.0"},
	{Path: "go.yaml.in/yaml/v3", Version: "v3.0.5"},
	{Path: "modernc.org/libc", Version: "v1.67.6"},
	{Path: "modernc.org/mathutil", Version: "v1.7.1"},
	{Path: "modernc.org/memory", Version: "v1.11.0"},
	{Path: "modernc.org/sqlite", Version: "v1.46.1"},
}

var ownerDirectories = []string{
	"reference/control-plane/cmd/aropd",
	"reference/control-plane/internal/adapters/observability/memory",
	"reference/control-plane/internal/app/platform",
	"reference/control-plane/internal/app/platform/httpadapter",
	"reference/control-plane/internal/app/platform/ports",
	"reference/control-plane/internal/ports/observability",
}

var p09CompositionInputs = []string{
	"reference/control-plane/internal/storage/migrate/testdata/engine-versions/baseline-transition-waiver.json",
	"reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go",
	p10WaiverPath,
	"reference/control-plane/migrations/postgres/0001_base.sql",
	"reference/control-plane/migrations/postgres/0005_identity.sql",
	"reference/control-plane/migrations/sqlite/0001_base.sql",
	"reference/control-plane/migrations/sqlite/0005_identity.sql",
}

var p09CompositionDirectories = []string{
	"reference/control-plane/internal/adapters/observability/durable",
	"reference/control-plane/internal/adapters/storage/postgres",
	"reference/control-plane/internal/adapters/storage/sqlite",
	"reference/control-plane/internal/storage/migrate",
	"reference/control-plane/internal/adapters/secrets",
	"reference/control-plane/internal/identity",
	"reference/control-plane/internal/ports/secrets",
}

var expectedTests = map[string][]string{
	nestedModule + "/cmd/aropd": {
		"TestCompositionRejectsInvalidConfiguration",
	},
	nestedModule + "/internal/adapters/observability/memory": {
		"TestStoreIsBoundedQueryableAndRedacted",
	},
	nestedModule + "/internal/app/platform": {
		"TestConfigValidationFailsClosed",
		"TestInjectedClockIDAndFaultAreDeterministic",
		"TestReadinessIsTruthful",
	},
	nestedModule + "/internal/app/platform/httpadapter": {
		"TestAssetRoutesSeparateIdentityFromGrantAuthentication",
		"TestHandlerChainPropagatesCorrelatesAndRedacts",
		"TestPublicationRoutesEnforceScopeUoWBoundaryAndBodyLimit",
	},
	nestedModule + "/internal/ports/observability": {
		"TestObservabilityValuesValidateAndRedact",
	},
}

var requiredSubtests = map[string][]string{
	nestedModule + "/cmd/aropd/TestCompositionRejectsInvalidConfiguration": {
		"serve-error",
		"shutdown-error-forces-close-and-waits",
		"shutdown-timeout-forces-close-and-waits",
	},
	nestedModule + "/internal/adapters/observability/memory/TestStoreIsBoundedQueryableAndRedacted": {
		"atomic-observation-rejects-partial-publish",
		"mismatched-observation-pair-rejected-without-publish",
		"paired-eviction-keeps-observation-aligned",
	},
	nestedModule + "/internal/adapters/observability/memory/TestStoreIsBoundedQueryableAndRedacted/mismatched-observation-pair-rejected-without-publish": {
		"http-status-outcome",
	},
	nestedModule + "/internal/app/platform/TestConfigValidationFailsClosed": {
		"unequal-observability-capacity-rejected",
	},
	nestedModule + "/internal/app/platform/TestInjectedClockIDAndFaultAreDeterministic": {
		"after-fault-rolls-back-inside-transaction",
	},
	nestedModule + "/internal/app/platform/httpadapter/TestHandlerChainPropagatesCorrelatesAndRedacts": {
		"health-body-boundary-empty-vs-chunked-byte",
		"incoming-client-request-id-not-authoritative",
	},
}

var sensitiveSentinels = []string{
	"p08-bearer-value",
	"p08-cookie-value",
	"p08-secret-value",
}

var frozenP05Inputs = map[string]string{
	"go.work.example": "8c6d9ceef33a559f25b16447d8a2f50128618ba3c941edb2f1fc13254256c7f8",
	"reference/control-plane/internal/server/server.go":      "1de7a69dec00c8746f67f5b274529399d9f3d06d8420cf86d1bb8b22a2e9c950",
	"reference/control-plane/internal/server/server_test.go": "671cdc8b7352f1fb9dc6a094d93b05029381aa49ba2ed444ef49274e9c60790d",
}

type fileEntry struct {
	Path   string
	Mode   os.FileMode
	SHA256 string
	Bytes  int64
}

type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

type commandResult struct {
	Argv   []string
	Output []byte
	Err    error
}

type moduleDownloadDocument struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Info     string `json:"Info"`
	GoMod    string `json:"GoMod"`
	Zip      string `json:"Zip"`
	Dir      string `json:"Dir"`
	Sum      string `json:"Sum"`
	GoModSum string `json:"GoModSum"`
	Error    string `json:"Error"`
}

type standaloneResult struct {
	Test              commandResult
	List              commandResult
	BootstrapEvidence []byte
	Version           string
	Commit            string
	ModuleCache       string
	BootstrapErr      error
	CleanupErr        error
}

type treeEntry struct {
	Mode   string
	Type   string
	Object string
	Path   string
}

type p10WaiverView struct {
	SchemaVersion int    `json:"schema_version"`
	WaiverID      string `json:"waiver_id"`
	Status        string `json:"status"`
	Policy        struct {
		OwnerPhaseSemantics  string `json:"owner_phase_semantics"`
		OwnershipTransferred bool   `json:"ownership_transferred"`
	} `json:"policy"`
	Transition struct {
		FromPhases []string `json:"from_phases"`
		ToPhase    string   `json:"to_phase"`
		Reason     string   `json:"reason"`
	} `json:"transition"`
	AffectedArtifacts []string `json:"affected_artifacts"`
	SourceClosure     []struct {
		Path           string   `json:"path"`
		ChangeType     string   `json:"change_type"`
		BaselineSHA256 *string  `json:"baseline_sha256"`
		CurrentSHA256  *string  `json:"current_sha256"`
		ArtifactIDs    []string `json:"artifact_ids"`
		OwnerPhases    []string `json:"owner_phases"`
	} `json:"source_closure"`
	Acceptance []struct {
		Phase   string `json:"phase"`
		Command string `json:"command"`
		Report  string `json:"report"`
	} `json:"acceptance"`
	Constraints []string `json:"constraints"`
}

func loadP10WaiverView(root string) (p10WaiverView, error) {
	data, err := readRegular(root, p10WaiverPath)
	if err != nil {
		return p10WaiverView{}, err
	}
	if _, err := structuredfile.Parse(data, ".json"); err != nil {
		return p10WaiverView{}, fmt.Errorf("strict parse P10 waiver: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var waiver p10WaiverView
	if err := decoder.Decode(&waiver); err != nil {
		return p10WaiverView{}, fmt.Errorf("strict decode P10 waiver: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return p10WaiverView{}, errors.New("P10 waiver contains trailing JSON")
		}
		return p10WaiverView{}, err
	}
	return waiver, nil
}

func verifyP10TransitionDeclaration(root string) error {
	waiver, err := loadP10WaiverView(root)
	if err != nil {
		return err
	}
	if waiver.SchemaVersion != 1 || waiver.WaiverID != "P10-P08-P09-BASELINE-TRANSITION-001" || (waiver.Status != "declared" && waiver.Status != "validated") {
		return errors.New("P10 waiver identity/version/status mismatch")
	}
	if waiver.Policy.OwnerPhaseSemantics != "first-introduction-and-accountability" || waiver.Policy.OwnershipTransferred || !reflect.DeepEqual(waiver.Transition.FromPhases, []string{"P08", "P09"}) || waiver.Transition.ToPhase != "P10" || waiver.Transition.Reason == "" {
		return errors.New("P10 waiver transition or ownership policy mismatch")
	}
	wantAcceptance := [][3]string{{"P08", "make test-control-plane-platform", "build/reports/P08/report.json"}, {"P09", "make test-storage-migrations", "build/reports/P09/report.json"}, {"P10", "make test-identity-secrets", "build/reports/P10/report.json"}}
	if len(waiver.Acceptance) != len(wantAcceptance) {
		return errors.New("P10 waiver acceptance length mismatch")
	}
	for index, want := range wantAcceptance {
		got := waiver.Acceptance[index]
		if got.Phase != want[0] || got.Command != want[1] || got.Report != want[2] {
			return errors.New("P10 waiver acceptance binding mismatch")
		}
	}
	required := []string{"control-plane-platform-foundation", "phase-report-p08", "reference-control-plane-server"}
	for _, artifact := range required {
		found := false
		for _, got := range waiver.AffectedArtifacts {
			if got == artifact {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("P10 waiver misses P08 artifact %s", artifact)
		}
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p10WaiverPath)))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		return errors.New("P10 waiver must be regular mode 0644")
	}
	return nil
}

func p10AllowsPackage(root, importPath string) bool {
	output, err := runP10TransitionChecker(root, "allowed-packages")
	if err != nil {
		return false
	}
	for _, pkg := range strings.Fields(string(output)) {
		if pkg == importPath {
			return true
		}
	}
	return false
}

func p12AllowsPackage(root, importPath string) bool {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/internal/domain/publication/testdata/transition/p12-baseline-transition-waiver.json"))
	if err != nil || !bytes.Contains(data, []byte(`"status": "validated"`)) || !bytes.Contains(data, []byte(`"commit": "31da1c8353b61e5eceae97f90b5e633e29abf3e5"`)) {
		return false
	}
	return strings.HasPrefix(importPath, nestedModule+"/internal/domain/publication")
}

func p13AllowsPackage(root, importPath string) bool {
	data, err := os.ReadFile(filepath.Join(root, p13WaiverPath))
	if err != nil || !bytes.Contains(data, []byte(`"status": "validated"`)) || !bytes.Contains(data, []byte(`"commit": "3db6ee93a693d62d47e2d2fd25c5de43749f2e7d"`)) {
		return false
	}
	return strings.HasPrefix(importPath, nestedModule+"/internal/domain/assets") || importPath == rootModule+"/sdk/go/generated/asset"
}

func runP10TransitionChecker(root, mode string) ([]byte, error) {
	path := filepath.Join(root, "reference/control-plane/internal/storage/migrate/testdata/engine-versions/transitioncheck/check.go")
	cmd := exec.Command("go", "run", "-modfile="+filepath.Join(root, "go.mod"), path, mode)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("P10 transition checker %s: %w: %s", mode, err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	checks := []report.Check{}
	add := func(name string, checkErr error, success string) {
		detail := success
		if checkErr != nil {
			detail = checkErr.Error()
		}
		checks = append(checks, report.Check{Name: name, Passed: checkErr == nil, Detail: detail})
	}

	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./" + checkerPath
	}
	var commandErr error
	if command != expectedCommand {
		commandErr = fmt.Errorf("command=%q want exact %q", command, expectedCommand)
	}
	add("exact-command", commandErr, expectedCommand)

	add("p08-owner-boundary", requireOwnerDirectories(root), "the four P08 owner roots and their required platform subpackages exist as real directories")
	_, p10TransitionErr := runP10TransitionChecker(root, "validate")
	add("p10-transition-declaration", p10TransitionErr, "the shared strict validator independently binds Git touch history, manifest owners/DAG, canonical closure, and all P08/P09/P10 acceptances")
	add("p05-baseline-unchanged", verifyFrozenInputs(root), "nested module/workspace and internal/server P05 baselines are byte-identical")
	add("production-import-boundary", verifyProductionImports(root), "P08 production imports stay within stdlib and exact P08 packages, with cmd/aropd alone allowed the approved P09 storage assembly imports")
	add("trace-validator-reuse", verifyTraceValidatorReuse(root), "request metadata uses the root core TraceContext validator without a copied trace regex or validator")
	add("redaction-fixture-coverage", verifySentinelFixtures(root), "tests exercise secret, authorization and cookie redaction sentinels")

	inputPaths, before, inputErr := staticInputs(root)
	if inputErr != nil {
		fatal(inputErr)
	}
	add("static-input-closure-before", nil, fmt.Sprintf("%d regular symlink-free files", len(inputPaths)))

	standalone := runStandaloneAcceptance(root)
	add("standalone-commit-proxy", standalone.BootstrapErr, fmt.Sprintf("%s@%s reconstructed from commit %s, prefetched, and verified before GOPROXY=off execution", rootModule, standalone.Version, standalone.Commit))
	add("standalone-temp-cleanup", standalone.CleanupErr, "commit-backed proxy and isolated Go caches were removed")
	result := standalone.Test
	testChecks, testErr := evaluateGoTests(result.Output, result.Err)
	checks = append(checks, testChecks...)
	add("nested-go-test-command", errors.Join(testErr, commandFailure(result)), strings.Join(result.Argv, " "))
	add("runtime-log-redaction", rejectSentinels(result.Output), "runtime evidence contains no raw redaction sentinel")
	listResult := standalone.List
	add("production-go-list", verifyProductionList(root, standalone.Version, standalone.ModuleCache, listResult), "go list dependency closure contains only stdlib, exact approved P08/P09 composition packages, root core and checksum-locked modules")

	afterPaths, after, afterErr := staticInputs(root)
	if afterErr == nil && !reflect.DeepEqual(inputPaths, afterPaths) {
		afterErr = errors.New("static input path set changed during P08 execution")
	}
	if afterErr == nil && !reflect.DeepEqual(before, after) {
		afterErr = manifestDifference(before, after)
	}
	add("static-input-closure-after", afterErr, "P08 input path/mode/digest/bytes remained unchanged")

	runtimeEvidence := []report.RuntimeEvidence{
		{Kind: "commit-proxy-bootstrap", SHA256: report.Hash(standalone.BootstrapEvidence), Bytes: int64(len(standalone.BootstrapEvidence))},
		{Kind: "nested-go-test-json", SHA256: report.Hash(result.Output), Bytes: int64(len(result.Output))},
		{Kind: "production-go-list-json", SHA256: report.Hash(listResult.Output), Bytes: int64(len(listResult.Output))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P08", Suite: "arop-control-plane-platform", Class: "arop.control-plane-platform",
		Command: command, CheckerPath: checkerPath, InputPaths: inputPaths, RuntimeInputPaths: []string{}, RuntimeEvidence: runtimeEvidence,
		Checks: checks,
		Summary: map[string]any{
			"external_proxy_category":        "credential-free-https",
			"root_locked_external_modules":   len(rootExternalModules),
			"nested_locked_external_modules": len(nestedExternalModules),
			"owner_artifacts":                4,
			"p09_composition_inputs":         len(p09CompositionInputs),
			"p09_composition_package_roots":  len(p09CompositionDirectories),
			"required_packages":              len(expectedTests),
			"required_subtests":              requiredSubtestCount(),
			"required_top_tests":             requiredTestCount(),
			"runtime_input_count":            0,
		},
		AuditNote: "P08 has no runtime_inputs. Its static closure binds all P08 owner sources plus frozen P05 module/server baselines, report tooling, the strict P08-to-P09 transition waiver, and every production source or migration loaded by the approved P09 durable cmd composition; the nested root pseudo-version is reconstructed from its exact Git commit into an isolated file proxy. Exact checksum-locked dependencies are prefetched through one credential-free HTTPS proxy, then tests and dependency discovery run with GOWORK=off and GOPROXY=off. Proxy category is non-secret; download outputs, absolute cache paths, verification, test, and list outputs are represented only by digest and byte-count runtime_evidence.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: reportPath})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(fmt.Errorf("P08 report self-verification returned mode=%s success=%t", mode, verified.Success))
	}
	if !written.Success {
		fatal(errors.New("P08 Control Plane platform checks failed; see " + reportPath))
	}
	fmt.Printf("AROP Control Plane platform passed: %d checks.\n", len(written.Checks))
}

func requireOwnerDirectories(root string) error {
	problems := []string{}
	for _, relative := range ownerDirectories {
		if err := requireRealDirectory(root, relative); err != nil {
			problems = append(problems, relative+": "+err.Error())
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func verifyFrozenInputs(root string) error {
	problems := []string{}
	paths := make([]string, 0, len(frozenP05Inputs))
	for path := range frozenP05Inputs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data, err := readRegular(root, path)
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			continue
		}
		if got := digest(data); got != frozenP05Inputs[path] {
			problems = append(problems, fmt.Sprintf("%s sha256=%s want frozen=%s", path, got, frozenP05Inputs[path]))
		}
	}
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func verifyProductionImports(root string) error {
	platform := nestedModule + "/internal/app/platform"
	httpAdapter := platform + "/httpadapter"
	platformPorts := platform + "/ports"
	observability := nestedModule + "/internal/ports/observability"
	memory := nestedModule + "/internal/adapters/observability/memory"
	rootCore := rootModule + "/sdk/go/protocol/core"
	rootGenerated := rootModule + "/sdk/go/generated/control-plane"
	durable := nestedModule + "/internal/adapters/observability/durable"
	postgres := nestedModule + "/internal/adapters/storage/postgres"
	sqlite := nestedModule + "/internal/adapters/storage/sqlite"
	migrations := nestedModule + "/internal/storage/migrate"
	allowedByDirectory := map[string]map[string]bool{
		"reference/control-plane/internal/app/platform": {
			platformPorts: true,
			observability: true,
			rootCore:      true,
		},
		"reference/control-plane/internal/app/platform/httpadapter": {
			platform: true, platformPorts: true, rootGenerated: true,
			nestedModule + "/internal/domain/publication": true,
		},
		"reference/control-plane/internal/app/platform/ports":  {},
		"reference/control-plane/internal/ports/observability": {},
		"reference/control-plane/internal/adapters/observability/memory": {
			observability: true,
		},
		"reference/control-plane/cmd/aropd": {
			httpAdapter: true, platform: true, platformPorts: true,
			observability: true, memory: true, durable: true,
			postgres: true, sqlite: true, migrations: true,
			nestedModule + "/internal/domain/publication":                  true,
			nestedModule + "/internal/domain/publication/storage/postgres": true,
			nestedModule + "/internal/domain/publication/storage/sqlite":   true,
		},
	}
	problems := []string{}
	for _, relativeRoot := range ownerDirectories {
		absoluteOwnerRoot := filepath.Join(root, filepath.FromSlash(relativeRoot))
		err := filepath.WalkDir(absoluteOwnerRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				if relativeRoot == "reference/control-plane/internal/app/platform" && path != absoluteOwnerRoot && (entry.Name() == "httpadapter" || entry.Name() == "ports") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imported := range parsed.Imports {
				value, err := strconvUnquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if !strings.Contains(strings.SplitN(value, "/", 2)[0], ".") {
					if relativeRoot == "reference/control-plane/internal/app/platform" && value == "net/http" {
						relative, _ := filepath.Rel(root, path)
						problems = append(problems, filepath.ToSlash(relative)+" parent platform must not import net/http")
					}
					continue
				}
				if !allowedByDirectory[relativeRoot][value] {
					if (relativeRoot == "reference/control-plane/cmd/aropd" && p10AllowsPackage(root, value)) || p12AllowsPackage(root, value) || p13AllowsPackage(root, value) {
						continue
					}
					relative, _ := filepath.Rel(root, path)
					problems = append(problems, filepath.ToSlash(relative)+" violates P08 import DAG with "+value)
				}
			}
			return nil
		})
		if err != nil {
			problems = append(problems, relativeRoot+": "+err.Error())
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func verifyTraceValidatorReuse(root string) error {
	const requestPath = "reference/control-plane/internal/app/platform/request.go"
	paths, err := filepath.Glob(filepath.Join(root, "reference", "control-plane", "internal", "app", "platform", "*.go"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("platform production source closure is empty")
	}
	hasTraceContext, hasValidateCall, sawRequest := false, false, false
	problems := []string{}
	for _, absolute := range paths {
		if strings.HasSuffix(absolute, "_test.go") {
			continue
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		data, err := readRegular(root, relative)
		if err != nil {
			return err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), relative, data, 0)
		if err != nil {
			return err
		}
		isRequest := relative == requestPath
		if isRequest {
			sawRequest = true
		}
		coreAlias := ""
		for _, imported := range parsed.Imports {
			importPath, err := strconvUnquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if importPath == "unicode/utf8" {
				problems = append(problems, relative+" performs local UTF-8 validation instead of using core TraceContext")
			}
			if isRequest && importPath == rootModule+"/sdk/go/protocol/core" {
				coreAlias = "core"
				if imported.Name != nil {
					coreAlias = imported.Name.Name
				}
				if coreAlias == "." || coreAlias == "_" {
					problems = append(problems, "root core trace validator must use an explicit non-dot import")
				}
			}
		}
		if isRequest && coreAlias == "" {
			problems = append(problems, "request.go does not import the root core protocol package")
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.ValueSpec:
				for _, name := range value.Names {
					normalized := strings.ToLower(name.Name)
					if normalized == "traceparentpattern" || normalized == "tracecontextpattern" {
						problems = append(problems, relative+" declares copied trace pattern "+name.Name)
					}
				}
			case *ast.FuncDecl:
				normalized := strings.ToLower(value.Name.Name)
				if normalized == "validatetracecontext" || normalized == "validatetraceparent" {
					problems = append(problems, relative+" declares copied trace validator "+value.Name.Name)
				}
			case *ast.CompositeLit:
				selector, ok := value.Type.(*ast.SelectorExpr)
				identifier, idOK := selectorX(selector)
				if isRequest && ok && idOK && identifier == coreAlias && selector.Sel.Name == "TraceContext" {
					hasTraceContext = true
				}
			case *ast.CallExpr:
				if isRequest {
					if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Validate" {
						hasValidateCall = true
					}
				}
			case *ast.BasicLit:
				if strings.Contains(value.Value, "[0-9a-f]{32}") || strings.Contains(value.Value, "[0-9a-f]{16}") {
					problems = append(problems, relative+" contains a copied trace identifier regex literal")
				}
			}
			return true
		})
	}
	if !sawRequest {
		problems = append(problems, "missing request.go")
	}
	if !hasTraceContext {
		problems = append(problems, "missing core.TraceContext construction")
	}
	if !hasValidateCall {
		problems = append(problems, "missing TraceContext.Validate call")
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func selectorX(selector *ast.SelectorExpr) (string, bool) {
	if selector == nil {
		return "", false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return identifier.Name, true
}

func verifySentinelFixtures(root string) error {
	found := map[string]bool{}
	problems := []string{}
	for _, relativeRoot := range ownerDirectories {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relativeRoot)), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("redaction fixture closure contains symlink %s", path)
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := readRegular(root, filepath.ToSlash(relative))
			if err != nil {
				return err
			}
			for _, sentinel := range sensitiveSentinels {
				if bytes.Contains(data, []byte(sentinel)) {
					found[sentinel] = true
				}
			}
			return nil
		})
		if err != nil {
			problems = append(problems, relativeRoot+": "+err.Error())
		}
	}
	missing := []string{}
	for _, sentinel := range sensitiveSentinels {
		if !found[sentinel] {
			missing = append(missing, sentinel)
		}
	}
	if len(missing) != 0 {
		problems = append(problems, fmt.Sprintf("P08 tests do not contain exact redaction sentinels %v", missing))
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func runStandaloneAcceptance(root string) (result standaloneResult) {
	testArgv := append([]string{"go", "test", "-count=1", "-run=.", "-json"}, sortedPackagePaths()...)
	listPackages := append(sortedPackagePaths(), "./internal/app/platform/ports")
	sort.Strings(listPackages)
	listArgv := append([]string{"go", "list", "-deps", "-json"}, listPackages...)
	result.Test.Argv = testArgv
	result.List.Argv = listArgv

	temporaryRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err == nil && (!filepath.IsAbs(temporaryRoot) || filepath.Clean(temporaryRoot) != temporaryRoot) {
		err = errors.New("resolved P08 temporary root is not absolute and clean")
	}
	var scratch string
	if err == nil {
		scratch, err = os.MkdirTemp(temporaryRoot, "arop-p08-standalone-")
	}
	if err != nil {
		result.BootstrapErr = err
		result.Test.Err = err
		result.List.Err = err
		return result
	}
	defer func() {
		if err := removeAllWritable(scratch); err != nil {
			result.CleanupErr = err
			return
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			result.CleanupErr = fmt.Errorf("standalone scratch still exists after cleanup: %s", scratch)
		}
	}()

	proxyRoot := filepath.Join(scratch, "proxy")
	cache := filepath.Join(scratch, "cache")
	goPath := filepath.Join(scratch, "gopath")
	moduleCache := filepath.Join(scratch, "modcache")
	result.ModuleCache = moduleCache
	moduleCopy := filepath.Join(scratch, "module")
	temporary := filepath.Join(scratch, "tmp")
	for _, directory := range []string{proxyRoot, cache, goPath, moduleCache, moduleCopy, temporary} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			result.BootstrapErr = err
			result.Test.Err = err
			result.List.Err = err
			return result
		}
	}

	version, commit, commitTime, rootGoMod, rootGoSum, err := resolveRootRequirement(root)
	result.Version, result.Commit = version, commit
	if err == nil {
		err = writeCommitProxy(root, proxyRoot, version, commit, commitTime, rootGoMod)
	}
	if err == nil {
		err = copyNestedModuleFiles(root, moduleCopy)
	}
	nestedRoot := filepath.Join(root, "reference", "control-plane")
	baseEnvironment := map[string]string{
		"CGO_ENABLED": "0", "GOCACHE": cache, "GODEBUG": "", "GOENV": "off",
		"GOFLAGS": "-mod=readonly", "GOMODCACHE": moduleCache, "GONOSUMDB": "*", "GOPATH": goPath, "GOPROXY": "off", "GOSUMDB": "off",
		"GOTOOLCHAIN": "local", "GOTMPDIR": temporary, "GOWORK": "off",
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	}
	var download, verify commandResult
	externalDownloads := []commandResult{}
	if err == nil {
		proxyEnvironment := cloneStrings(baseEnvironment)
		proxyEnvironment["GOPROXY"] = (&url.URL{Scheme: "file", Path: proxyRoot}).String() + ",off"
		download = runCommand(moduleCopy, proxyEnvironment, []string{"go", "mod", "download", "-json", rootModule + "@" + version})
		err = commandFailure(download)
	}
	if err == nil {
		err = validateDownloadedRoot(root, scratch, version, rootGoMod, download.Output)
	}
	if err == nil {
		externalEnvironment := cloneStrings(baseEnvironment)
		externalEnvironment["GOPROXY"] = "https://proxy.golang.org"
		for _, dependency := range rootExternalModules {
			external := runCommand(moduleCopy, externalEnvironment, []string{"go", "mod", "download", "-json", dependency.Path + "@" + dependency.Version})
			externalDownloads = append(externalDownloads, external)
			if err = commandFailure(external); err != nil {
				break
			}
			if err = validateExternalDownload(root, scratch, dependency.Path, dependency.Version, rootGoSum, external.Output); err != nil {
				break
			}
		}
		if err == nil {
			nestedDownload := runCommand(moduleCopy, externalEnvironment, []string{"go", "mod", "download", "-json", "all"})
			externalDownloads = append(externalDownloads, nestedDownload)
			if err = commandFailure(nestedDownload); err == nil {
				err = validateNestedDownloads(root, scratch, nestedDownload.Output)
			}
		}
	}
	if err == nil {
		verify = runCommand(nestedRoot, baseEnvironment, []string{"go", "mod", "verify"})
		err = commandFailure(verify)
	}
	evidenceParts := [][]byte{
		[]byte("version=" + version + "\ncommit=" + commit + "\n"),
		download.Output,
	}
	for _, external := range externalDownloads {
		evidenceParts = append(evidenceParts, external.Output)
	}
	evidenceParts = append(evidenceParts, verify.Output)
	result.BootstrapEvidence = bytes.Join(evidenceParts, []byte("\n"))
	result.BootstrapErr = err
	if err != nil {
		wrapped := fmt.Errorf("standalone commit-proxy bootstrap: %w", err)
		result.Test.Err = wrapped
		result.List.Err = wrapped
		return result
	}

	result.Test = runCommand(nestedRoot, baseEnvironment, testArgv)
	result.List = runCommand(nestedRoot, baseEnvironment, listArgv)
	return result
}

func resolveRootRequirement(root string) (string, string, time.Time, []byte, []byte, error) {
	nestedGoMod, err := readRegular(root, "reference/control-plane/go.mod")
	if err != nil {
		return "", "", time.Time{}, nil, nil, err
	}
	matches := rootRequirementPattern.FindAllSubmatch(nestedGoMod, -1)
	if len(matches) != 1 {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("nested go.mod must contain exactly one exact root pseudo-version requirement; found %d", len(matches))
	}
	version, timestampText, shortCommit := string(matches[0][1]), string(matches[0][2]), string(matches[0][3])
	wantTime, err := time.Parse("20060102150405", timestampText)
	if err != nil {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("parse root pseudo-version timestamp: %w", err)
	}
	commitOutput, err := runGit(root, "rev-parse", shortCommit+"^{commit}")
	if err != nil {
		return "", "", time.Time{}, nil, nil, err
	}
	commit := strings.TrimSpace(string(commitOutput))
	if len(commit) != 40 || !strings.HasPrefix(commit, shortCommit) {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("resolved commit %q does not match pseudo-version suffix %s", commit, shortCommit)
	}
	if _, err := runGit(root, "merge-base", "--is-ancestor", commit, "HEAD"); err != nil {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("root pseudo-version commit is not an ancestor of HEAD: %w", err)
	}
	commitTimeOutput, err := runGit(root, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return "", "", time.Time{}, nil, nil, err
	}
	commitTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(commitTimeOutput)))
	if err != nil {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("parse root commit time: %w", err)
	}
	if !commitTime.UTC().Equal(wantTime.UTC()) {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("pseudo-version timestamp %s does not match commit time %s", wantTime.UTC().Format(time.RFC3339), commitTime.UTC().Format(time.RFC3339))
	}
	rootGoMod, err := runGit(root, "show", commit+":go.mod")
	if err != nil {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("read root go.mod at %s: %w", commit, err)
	}
	rootGoSum, err := runGit(root, "show", commit+":go.sum")
	if err != nil {
		return "", "", time.Time{}, nil, nil, fmt.Errorf("read root go.sum at %s: %w", commit, err)
	}
	if err := validateExternalModuleLocks(rootGoMod, rootGoSum); err != nil {
		return "", "", time.Time{}, nil, nil, err
	}
	currentGoMod, currentModErr := readRegular(root, "go.mod")
	currentGoSum, currentSumErr := readRegular(root, "go.sum")
	if currentModErr != nil || currentSumErr != nil || !bytes.Equal(currentGoMod, rootGoMod) || !bytes.Equal(currentGoSum, rootGoSum) {
		return "", "", time.Time{}, nil, nil, errors.New("current root go.mod/go.sum differ from the pinned root commit")
	}
	return version, commit, commitTime.UTC(), rootGoMod, rootGoSum, nil
}

func validateExternalModuleLocks(rootGoMod, rootGoSum []byte) error {
	requirements := map[string][]string{}
	for _, line := range strings.Split(string(rootGoMod), "\n") {
		fields := strings.Fields(strings.SplitN(line, "//", 2)[0])
		if len(fields) == 3 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) == 2 && strings.Contains(fields[0], ".") {
			requirements[fields[0]] = append(requirements[fields[0]], fields[1])
		}
	}
	sums := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(rootGoSum)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 {
			sums[fields[0]+" "+fields[1]] = append(sums[fields[0]+" "+fields[1]], fields[2])
		}
	}
	problems := []string{}
	for _, dependency := range rootExternalModules {
		versions := requirements[dependency.Path]
		if len(versions) != 1 || versions[0] != dependency.Version {
			problems = append(problems, fmt.Sprintf("root go.mod lock for %s is %v, want exactly %s", dependency.Path, versions, dependency.Version))
		}
		for _, suffix := range []string{"", "/go.mod"} {
			key := dependency.Path + " " + dependency.Version + suffix
			if len(sums[key]) != 1 {
				problems = append(problems, fmt.Sprintf("root go.sum must contain exactly one %s entry", key))
			}
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func writeCommitProxy(root, proxyRoot, version, commit string, commitTime time.Time, rootGoMod []byte) error {
	directory := filepath.Join(proxyRoot, filepath.FromSlash(rootModule), "@v")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	info, err := json.Marshal(struct {
		Version string    `json:"Version"`
		Time    time.Time `json:"Time"`
	}{Version: version, Time: commitTime})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, version+".info"), append(info, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, version+".mod"), rootGoMod, 0o644); err != nil {
		return err
	}
	archivePath := filepath.Join(directory, version+".zip")
	if err := writeModuleZip(root, archivePath, version, commit); err != nil {
		return err
	}
	infoFile, err := os.Lstat(archivePath)
	if err != nil || !infoFile.Mode().IsRegular() || infoFile.Mode()&os.ModeSymlink != 0 || infoFile.Size() == 0 {
		return fmt.Errorf("commit-backed module archive is not a non-empty regular file")
	}
	return nil
}

func writeModuleZip(root, destination, version, commit string) error {
	entries, err := gitTree(root, commit)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	closeWith := func(base error) error {
		if closeErr := archive.Close(); base == nil {
			base = closeErr
		}
		if closeErr := file.Close(); base == nil {
			base = closeErr
		}
		return base
	}
	prefix := rootModule + "@" + version + "/"
	const maxModuleZipBytes int64 = 500 << 20
	const maxSpecialFileBytes int64 = 16 << 20
	var totalBytes int64
	caseFolded := map[string]string{}
	for _, entry := range entries {
		if excludedFromModuleZip(entry.Path) {
			continue
		}
		if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
			return closeWith(fmt.Errorf("unsupported Git tree entry %s %s at %s", entry.Mode, entry.Type, entry.Path))
		}
		if err := validateModuleArchivePath(entry.Path); err != nil {
			return closeWith(err)
		}
		if err := recordArchivePath(entry.Path, caseFolded); err != nil {
			return closeWith(err)
		}
		data, err := runGit(root, "cat-file", "blob", entry.Object)
		if err != nil {
			return closeWith(fmt.Errorf("read blob %s: %w", entry.Path, err))
		}
		totalBytes += int64(len(data))
		if totalBytes > maxModuleZipBytes {
			return closeWith(fmt.Errorf("root module archive exceeds %d bytes", maxModuleZipBytes))
		}
		base := strings.ToLower(filepath.Base(entry.Path))
		if (base == "go.mod" || base == "license") && int64(len(data)) > maxSpecialFileBytes {
			return closeWith(fmt.Errorf("module archive %s exceeds %d bytes", entry.Path, maxSpecialFileBytes))
		}
		header := &zip.FileHeader{Name: prefix + entry.Path, Method: zip.Deflate}
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		if entry.Mode == "100755" {
			header.SetMode(0o755)
		} else {
			header.SetMode(0o644)
		}
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return closeWith(err)
		}
		if _, err := writer.Write(data); err != nil {
			return closeWith(err)
		}
	}
	return closeWith(nil)
}

func validateModuleArchivePath(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || len(path) > 1024 {
		return fmt.Errorf("invalid module archive path %q", path)
	}
	if strings.Contains(path, `\`) {
		return fmt.Errorf("module archive path contains backslash: %q", path)
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." || strings.HasSuffix(element, ".") || strings.HasSuffix(element, " ") {
			return fmt.Errorf("invalid module archive path element %q in %q", element, path)
		}
		stem := strings.ToUpper(strings.SplitN(element, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return fmt.Errorf("module archive path uses reserved Windows name %q", element)
		}
		for _, character := range element {
			if character < 0x20 || strings.ContainsRune(`:*?"<>|`, character) {
				return fmt.Errorf("invalid character in module archive path %q", path)
			}
		}
	}
	return nil
}

func excludedFromModuleZip(path string) bool {
	const nestedDirectory = "reference/control-plane"
	if path == nestedDirectory || strings.HasPrefix(path, nestedDirectory+"/") {
		return true
	}
	for _, element := range strings.Split(path, "/") {
		if element == "vendor" {
			return true
		}
	}
	return false
}

func foldPath(path string) string {
	var folded strings.Builder
	for _, character := range path {
		minimum := character
		for next := unicode.SimpleFold(character); next != character; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

func recordArchivePath(path string, seen map[string]string) error {
	elements := strings.Split(path, "/")
	for index := range elements {
		prefix := strings.Join(elements[:index+1], "/")
		folded := foldPath(prefix)
		if prior := seen[folded]; prior != "" && prior != prefix {
			return fmt.Errorf("case-insensitive module zip path collision: %s and %s", prior, prefix)
		}
		seen[folded] = prefix
	}
	return nil
}

func gitTree(root, commit string) ([]treeEntry, error) {
	output, err := runGit(root, "ls-tree", "-rz", "--full-tree", commit)
	if err != nil {
		return nil, fmt.Errorf("list bootstrap tree: %w", err)
	}
	entries := []treeEntry{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		parts := bytes.SplitN(raw, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed git ls-tree record %q", raw)
		}
		metadata := strings.Fields(string(parts[0]))
		if len(metadata) != 3 {
			return nil, fmt.Errorf("malformed git ls-tree metadata %q", parts[0])
		}
		entries = append(entries, treeEntry{Mode: metadata[0], Type: metadata[1], Object: metadata[2], Path: string(parts[1])})
	}
	return entries, nil
}

func validateDownloadedRoot(root, scratch, version string, rootGoMod, output []byte) error {
	var document moduleDownloadDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode root module download evidence: %w", err)
	}
	if document.Error != "" {
		return errors.New(document.Error)
	}
	if document.Path != rootModule || document.Version != version || document.Sum == "" || document.GoModSum == "" {
		return fmt.Errorf("unexpected root module download identity: path=%q version=%q sum=%q go_mod_sum=%q", document.Path, document.Version, document.Sum, document.GoModSum)
	}
	for label, path := range map[string]string{"info": document.Info, "go.mod": document.GoMod, "zip": document.Zip, "directory": document.Dir} {
		if path == "" || !pathWithin(scratch, path) || pathWithin(root, path) {
			return fmt.Errorf("downloaded %s is outside the isolated cache: %q", label, path)
		}
	}
	downloadedGoMod, err := os.ReadFile(document.GoMod)
	if err != nil {
		return err
	}
	if !bytes.Equal(downloadedGoMod, rootGoMod) {
		return errors.New("downloaded root go.mod differs from the pseudo-version commit")
	}
	nestedGoSum, err := readRegular(root, "reference/control-plane/go.sum")
	if err != nil {
		return err
	}
	want := map[string]string{
		rootModule + " " + version:             document.Sum,
		rootModule + " " + version + "/go.mod": document.GoModSum,
	}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(nestedGoSum)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && strings.HasPrefix(fields[0]+" "+fields[1], rootModule+" ") {
			seen[fields[0]+" "+fields[1]] = fields[2]
		}
	}
	if !reflect.DeepEqual(want, seen) {
		return fmt.Errorf("nested go.sum root entries differ from commit-backed download: want=%v got=%v", want, seen)
	}
	return nil
}

func validateExternalDownload(root, scratch, module, version string, rootGoSum, output []byte) error {
	var document moduleDownloadDocument
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode external module download evidence: %w", err)
	}
	if document.Error != "" {
		return errors.New(document.Error)
	}
	if document.Path != module || document.Version != version || document.Sum == "" || document.GoModSum == "" {
		return fmt.Errorf("unexpected external module identity: path=%q version=%q sum=%q go_mod_sum=%q", document.Path, document.Version, document.Sum, document.GoModSum)
	}
	for label, path := range map[string]string{"info": document.Info, "go.mod": document.GoMod, "zip": document.Zip, "directory": document.Dir} {
		if path == "" || !pathWithin(scratch, path) || pathWithin(root, path) {
			return fmt.Errorf("downloaded external %s is outside the isolated cache: %q", label, path)
		}
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(rootGoSum)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == module && (fields[1] == version || fields[1] == version+"/go.mod") {
			want[fields[0]+" "+fields[1]] = fields[2]
		}
	}
	got := map[string]string{
		module + " " + version:             document.Sum,
		module + " " + version + "/go.mod": document.GoModSum,
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("external module checksums differ from pinned root go.sum for %s@%s: want=%v got=%v", module, version, want, got)
	}
	return nil
}

func validateNestedDownloads(root, scratch string, output []byte) error {
	nestedGoSum, err := readRegular(root, "reference/control-plane/go.sum")
	if err != nil {
		return err
	}
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(nestedGoSum)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return errors.New("nested go.sum contains a malformed entry")
		}
		sums[fields[0]+" "+fields[1]] = fields[2]
	}
	seen := map[string]bool{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var document moduleDownloadDocument
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode nested module download evidence: %w", err)
		}
		if document.Error != "" || document.Path == "" || document.Version == "" || document.Sum == "" || document.GoModSum == "" {
			return fmt.Errorf("nested module download is incomplete for %s", document.Path)
		}
		key := document.Path + " " + document.Version
		if seen[key] {
			return fmt.Errorf("duplicate nested module download %s", key)
		}
		seen[key] = true
		if sums[key] != document.Sum || sums[key+"/go.mod"] != document.GoModSum {
			return fmt.Errorf("nested module checksum mismatch for %s", key)
		}
		for label, downloadedPath := range map[string]string{"info": document.Info, "go.mod": document.GoMod, "zip": document.Zip, "directory": document.Dir} {
			if downloadedPath == "" || !pathWithin(scratch, downloadedPath) || pathWithin(root, downloadedPath) {
				return fmt.Errorf("nested module %s %s escaped isolated cache", key, label)
			}
		}
	}
	for _, dependency := range nestedExternalModules {
		if !seen[dependency.Path+" "+dependency.Version] {
			return fmt.Errorf("nested download omitted locked module %s@%s", dependency.Path, dependency.Version)
		}
	}
	return nil
}

func runGit(root string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-c", "core.autocrlf=false", "-c", "core.safecrlf=true"}, arguments...)...)
	command.Dir = root
	command.Env = cleanEnvironment(os.Environ(), map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1", "GIT_OPTIONAL_LOCKS": "0",
		"LANG": "C", "LC_ALL": "C", "TZ": "UTC",
	})
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runCommand(directory string, environment map[string]string, argv []string) commandResult {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = cleanEnvironment(os.Environ(), environment)
	output, err := command.CombinedOutput()
	return commandResult{Argv: append([]string(nil), argv...), Output: output, Err: err}
}

func commandFailure(result commandResult) error {
	if result.Err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(result.Output))
	if len(detail) > 2048 {
		detail = detail[:2048] + "..."
	}
	return fmt.Errorf("%s: %w (output sha256=%s bytes=%d): %s", strings.Join(result.Argv, " "), result.Err, report.Hash(result.Output), len(result.Output), detail)
}

func copyNestedModuleFiles(root, destination string) error {
	for _, relative := range []string{"reference/control-plane/go.mod", "reference/control-plane/go.sum"} {
		data, err := readRegular(root, relative)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, filepath.Base(relative)), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func removeAllWritable(path string) error {
	if path == "" || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("refusing unsafe scratch cleanup")
	}
	if err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return os.Chmod(current, 0o600)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(path)
}

func cloneStrings(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func pathWithin(parent, child string) bool {
	parentAbsolute, parentErr := filepath.Abs(parent)
	childAbsolute, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	relative, err := filepath.Rel(parentAbsolute, childAbsolute)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func verifyProductionList(root, rootVersion, moduleCache string, result commandResult) error {
	if result.Err != nil {
		return commandFailure(result)
	}
	type listedPackage struct {
		ImportPath string `json:"ImportPath"`
		Dir        string `json:"Dir"`
		Standard   bool   `json:"Standard"`
		Module     *struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		} `json:"Module"`
	}
	allowed := map[string]string{
		nestedModule + "/cmd/aropd":                               "reference/control-plane/cmd/aropd",
		nestedModule + "/internal/adapters/observability/durable": "reference/control-plane/internal/adapters/observability/durable",
		nestedModule + "/internal/adapters/observability/memory":  "reference/control-plane/internal/adapters/observability/memory",
		nestedModule + "/internal/adapters/storage/postgres":      "reference/control-plane/internal/adapters/storage/postgres",
		nestedModule + "/internal/adapters/storage/sqlite":        "reference/control-plane/internal/adapters/storage/sqlite",
		nestedModule + "/internal/app/platform":                   "reference/control-plane/internal/app/platform",
		nestedModule + "/internal/app/platform/httpadapter":       "reference/control-plane/internal/app/platform/httpadapter",
		nestedModule + "/internal/app/platform/ports":             "reference/control-plane/internal/app/platform/ports",
		nestedModule + "/internal/ports/observability":            "reference/control-plane/internal/ports/observability",
		nestedModule + "/internal/storage/migrate":                "reference/control-plane/internal/storage/migrate",
	}
	seen := map[string]bool{}
	seenExternalModules := map[string]bool{}
	rootCore := rootModule + "/sdk/go/protocol/core"
	rootGenerated := rootModule + "/sdk/go/generated/control-plane"
	rootManifest := rootModule + "/sdk/go/protocol/manifest"
	externalVersions := map[string]string{}
	for _, dependency := range nestedExternalModules {
		externalVersions[dependency.Path] = dependency.Version
	}
	problems := []string{}
	decoder := json.NewDecoder(bytes.NewReader(result.Output))
	for {
		var item listedPackage
		err := decoder.Decode(&item)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode go list output: %w", err)
		}
		if item.Standard {
			continue
		}
		if wantDirectory, ok := allowed[item.ImportPath]; ok {
			seen[item.ImportPath] = true
			want, _ := filepath.Abs(filepath.Join(root, filepath.FromSlash(wantDirectory)))
			actual, _ := filepath.Abs(item.Dir)
			if actual != want || item.Module == nil || item.Module.Path != nestedModule || item.Module.Version != "" {
				problems = append(problems, fmt.Sprintf("%s is not the exact nested main-module package", item.ImportPath))
			}
			continue
		}
		if item.ImportPath == rootCore || item.ImportPath == rootGenerated || item.ImportPath == rootManifest {
			seen[item.ImportPath] = true
			if item.Module == nil || item.Module.Path != rootModule || item.Module.Version != rootVersion || !pathWithin(moduleCache, item.Dir) || pathWithin(root, item.Dir) {
				problems = append(problems, "root core did not resolve from the exact isolated commit-backed module")
			}
			continue
		}
		if p10AllowsPackage(root, item.ImportPath) && item.Module != nil && item.Module.Path == nestedModule && item.Module.Version == "" {
			seen[item.ImportPath] = true
			continue
		}
		if p12AllowsPackage(root, item.ImportPath) && item.Module != nil && item.Module.Path == nestedModule && item.Module.Version == "" {
			seen[item.ImportPath] = true
			continue
		}
		if p13AllowsPackage(root, item.ImportPath) && item.Module != nil {
			if item.Module.Path == nestedModule && item.Module.Version == "" || item.Module.Path == rootModule && item.Module.Version == rootVersion && pathWithin(moduleCache, item.Dir) && !pathWithin(root, item.Dir) {
				seen[item.ImportPath] = true
				continue
			}
		}
		if item.Module != nil {
			if wantVersion, ok := externalVersions[item.Module.Path]; ok && item.Module.Version == wantVersion && pathWithin(moduleCache, item.Dir) && !pathWithin(root, item.Dir) {
				seenExternalModules[item.Module.Path] = true
				continue
			}
		}
		problems = append(problems, "non-stdlib dependency outside exact P08 closure: "+item.ImportPath)
	}
	if !seen[rootCore] {
		problems = append(problems, "missing production dependency "+rootCore)
	}
	for _, dependency := range nestedExternalModules {
		if !seenExternalModules[dependency.Path] {
			problems = append(problems, "missing locked transitive module "+dependency.Path+"@"+dependency.Version)
		}
	}
	for pkg := range allowed {
		if !seen[pkg] {
			problems = append(problems, "missing production package "+pkg)
		}
	}
	sort.Strings(problems)
	if len(problems) != 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func evaluateGoTests(data []byte, commandErr error) ([]report.Check, error) {
	testTerminals := map[string][]string{}
	packageTerminals := map[string][]string{}
	topLevel := map[string]bool{}
	cacheMarkers, emptyMarkers := 0, 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event goTestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse go test -json: %w", err)
		}
		if strings.Contains(event.Output, "(cached)") {
			cacheMarkers++
		}
		if strings.Contains(event.Output, "[no test files]") || strings.Contains(event.Output, "no tests to run") {
			emptyMarkers++
		}
		if event.Action != "pass" && event.Action != "fail" && event.Action != "skip" {
			continue
		}
		if event.Test == "" {
			packageTerminals[event.Package] = append(packageTerminals[event.Package], event.Action)
			continue
		}
		key := event.Package + "/" + event.Test
		testTerminals[key] = append(testTerminals[key], event.Action)
		if !strings.Contains(event.Test, "/") {
			topLevel[key] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	checks := []report.Check{}
	add := func(name string, passed bool, detail string) {
		checks = append(checks, report.Check{Name: name, Passed: passed, Detail: detail})
	}
	wantPackages := expectedPackageNames()
	actualPackages := sortedKeys(packageTerminals)
	add("p08-package-terminal-set", reflect.DeepEqual(wantPackages, actualPackages), setDetail(wantPackages, actualPackages))
	for _, pkg := range wantPackages {
		actions := packageTerminals[pkg]
		add("p08-package:"+pkg, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	wantTop := []string{}
	for pkg, tests := range expectedTests {
		for _, test := range tests {
			wantTop = append(wantTop, pkg+"/"+test)
		}
	}
	sort.Strings(wantTop)
	actualTop := sortedKeys(topLevel)
	add("p08-required-top-level-test-set", reflect.DeepEqual(wantTop, actualTop), setDetail(wantTop, actualTop))
	for _, name := range wantTop {
		actions := testTerminals[name]
		add("p08-test:"+name, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	requiredChildren := []string{}
	for parent, children := range requiredSubtests {
		for _, child := range children {
			requiredChildren = append(requiredChildren, parent+"/"+child)
		}
	}
	sort.Strings(requiredChildren)
	for _, name := range requiredChildren {
		actions := testTerminals[name]
		add("p08-required-subtest:"+name, len(actions) == 1 && actions[0] == "pass", fmt.Sprintf("terminal=%v", actions))
	}
	unexpected := []string{}
	failed := []string{}
	wantTopSet := map[string]bool{}
	for _, name := range wantTop {
		wantTopSet[name] = true
	}
	for name, actions := range testTerminals {
		pkg, test, ok := strings.Cut(name, "/Test")
		if !ok {
			unexpected = append(unexpected, name)
			continue
		}
		fullTest := "Test" + test
		rootTest := strings.SplitN(fullTest, "/", 2)[0]
		if !wantTopSet[pkg+"/"+rootTest] {
			unexpected = append(unexpected, name)
		}
		if len(actions) != 1 || actions[0] != "pass" {
			failed = append(failed, name+"="+fmt.Sprint(actions))
		}
	}
	sort.Strings(unexpected)
	sort.Strings(failed)
	add("p08-no-unexpected-tests", len(unexpected) == 0, fmt.Sprintf("unexpected=%v", unexpected))
	add("p08-all-test-terminals-pass", len(failed) == 0, fmt.Sprintf("failed=%v", failed))
	add("p08-no-cache-or-empty-packages", cacheMarkers == 0 && emptyMarkers == 0, fmt.Sprintf("cache=%d empty=%d", cacheMarkers, emptyMarkers))
	add("p08-go-test-exit", commandErr == nil, fmt.Sprint(commandErr))
	for _, check := range checks {
		if !check.Passed {
			return checks, errors.New("exact P08 Go test evaluation failed")
		}
	}
	return checks, nil
}

func staticInputs(root string) ([]string, []fileEntry, error) {
	paths := []string{
		"Makefile", "go.mod", "go.sum", "go.work.example", "reference/control-plane/go.mod", "reference/control-plane/go.sum",
		"reference/control-plane/internal/server/server.go", "reference/control-plane/internal/server/server_test.go",
		"docs/DECISIONS.md", "docs/DEVELOPMENT_PLAN.md", "docs/DIRECTORY_STRUCTURE.md", "docs/IMPLEMENTATION_BLUEPRINT.md",
		"spec/artifact-manifest.yaml", "spec/requirements.yaml", "spec/schemas/check-report.schema.json",
	}
	paths = append(paths, p09CompositionInputs...)
	for _, directory := range p09CompositionDirectories {
		files, err := immediateRegularFiles(root, directory)
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, files...)
	}
	for _, directory := range append(append([]string{}, ownerDirectories...),
		"internal/tooling/controlledinput", "internal/tooling/report", "internal/tooling/schema", "internal/tooling/structuredfile") {
		files, err := regularFiles(root, directory)
		if err != nil {
			return nil, nil, err
		}
		paths = append(paths, files...)
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
	entries := make([]fileEntry, 0, len(paths))
	for _, path := range paths {
		data, err := readRegular(root, path)
		if err != nil {
			return nil, nil, err
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, fileEntry{Path: path, Mode: info.Mode(), SHA256: digest(data), Bytes: int64(len(data))})
	}
	return paths, entries, nil
}

func immediateRegularFiles(root, relativeDirectory string) ([]string, error) {
	if err := requireRealDirectory(root, relativeDirectory); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(relativeDirectory)))
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("static closure contains symlink %s/%s", relativeDirectory, entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
			return nil, fmt.Errorf("static closure file %s/%s is not regular 0644", relativeDirectory, entry.Name())
		}
		paths = append(paths, relativeDirectory+"/"+entry.Name())
	}
	sort.Strings(paths)
	return paths, nil
}

func regularFiles(root, relativeDirectory string) ([]string, error) {
	if err := requireRealDirectory(root, relativeDirectory); err != nil {
		return nil, err
	}
	paths := []string{}
	err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(relativeDirectory)), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("static closure contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("static closure contains non-regular file %s", path)
		}
		if info.Mode().Perm() != 0o644 {
			return fmt.Errorf("static closure file %s mode=%#o want 0644", path, info.Mode().Perm())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func readRegular(root, relative string) ([]byte, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if relative == "" || clean != relative || filepath.IsAbs(filepath.FromSlash(relative)) || relative == ".." || strings.HasPrefix(relative, "../") {
		return nil, fmt.Errorf("unsafe input path %q", relative)
	}
	if err := requireRealDirectory(root, filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))); err != nil {
		return nil, err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular non-symlink file: %s", relative)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedBefore, err := file.Stat()
	if err != nil || !sameFile(before, openedBefore) {
		return nil, fmt.Errorf("file changed while opening %s", relative)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	openedAfter, openedErr := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if openedErr != nil || pathErr != nil || !sameFile(openedBefore, openedAfter) || !sameFile(openedAfter, pathAfter) {
		return nil, fmt.Errorf("file changed while reading %s", relative)
	}
	return data, nil
}

func sameFile(left, right os.FileInfo) bool {
	return left != nil && right != nil && os.SameFile(left, right) && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func requireRealDirectory(root, relative string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe directory path %q", relative)
	}
	current := absoluteRoot
	components := []string{}
	if clean != "." {
		components = strings.Split(clean, string(filepath.Separator))
	}
	for _, component := range append([]string{""}, components...) {
		if component != "" {
			if component == "." || component == ".." {
				return fmt.Errorf("unsafe directory component %q", component)
			}
			current = filepath.Join(current, component)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("directory component is not a real directory: %s", current)
		}
	}
	return nil
}

func cleanEnvironment(inherited []string, overrides map[string]string) []string {
	allowed := map[string]bool{"PATH": true}
	result := []string{}
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		if allowed[strings.ToUpper(key)] {
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

func rejectSentinels(data []byte) error {
	leaked := []string{}
	for _, sentinel := range sensitiveSentinels {
		if bytes.Contains(data, []byte(sentinel)) {
			leaked = append(leaked, sentinel)
		}
	}
	if len(leaked) != 0 {
		return fmt.Errorf("runtime log leaked redaction sentinels %v", leaked)
	}
	return nil
}

func sortedPackagePaths() []string {
	packages := make([]string, 0, len(expectedTests))
	for pkg := range expectedTests {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	paths := make([]string, len(packages))
	for index, pkg := range packages {
		paths[index] = "." + strings.TrimPrefix(pkg, nestedModule)
	}
	return paths
}

func expectedPackageNames() []string {
	packages := make([]string, 0, len(expectedTests))
	for pkg := range expectedTests {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages
}

func requiredTestCount() int {
	count := 0
	for _, tests := range expectedTests {
		count += len(tests)
	}
	return count
}

func requiredSubtestCount() int {
	count := 0
	for _, tests := range requiredSubtests {
		count += len(tests)
	}
	return count
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func setDetail(expected, actual []string) string {
	return fmt.Sprintf("expected=%v actual=%v", expected, actual)
}

func manifestDifference(before, after []fileEntry) error {
	return fmt.Errorf("static manifest changed: before=%s after=%s", manifestDigest(before), manifestDigest(after))
}

func manifestDigest(entries []fileEntry) string {
	var aggregate bytes.Buffer
	for _, entry := range entries {
		fmt.Fprintf(&aggregate, "%s\x00%#o\x00%s\x00%d\n", entry.Path, entry.Mode, entry.SHA256, entry.Bytes)
	}
	return digest(aggregate.Bytes())
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func strconvUnquote(value string) (string, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("noncanonical Go import literal %q", value)
	}
	var decoded string
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
