// Command harness is the sole writer of the P25 Go Worker SDK report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-go-worker-sdk"
	checker  = "reference/agents/pull-worker/testdata/harness/main.go"
	waiver   = "reference/agents/pull-worker/testdata/transition/p25-baseline-transition-waiver.json"
	baseline = "7f968a6909cadd33e82f9f9389f23e4a998c3c15"
	carrier  = "13147594ab2258b4bf57553abe03f7543b4b7c51"
)

var requiredOwned = []string{"go-worker-sdk", "reference-worker"}
var requiredConstraints = []string{
	"at-least-once-completion-reuses-stable-idempotency",
	"drain-stops-new-claims-before-waiting",
	"effect-id-is-stable-across-attempts",
	"no-new-persistence",
	"public-sdk-does-not-import-reference-internal",
	"runtime-inputs-remain-empty",
}

type transition struct {
	SchemaVersion     int      `json:"schema_version"`
	TransitionID      string   `json:"transition_id"`
	Status            string   `json:"status"`
	BaselineCommit    string   `json:"baseline_commit"`
	CarrierCommit     string   `json:"carrier_commit"`
	AffectedArtifacts []string `json:"affected_artifacts"`
	SourceClosure     []string `json:"source_closure"`
	AcceptanceClosure []string `json:"acceptance_closure"`
	Constraints       []string `json:"constraints"`
}

type discovered struct {
	Artifacts  []string
	Sources    []string
	Acceptance []string
}

type commandResult struct {
	output []byte
	err    error
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", command))
	}
	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}

	inputs, err := trackedInputs(root)
	add("p25-static-input-closure", err, fmt.Sprintf("%d tracked inputs", len(inputs)))
	add("p25-manifest-inventory", validateManifest(root), "exact two owned artifacts and empty runtime inputs")
	successor, endpoint, endpointErr := immutableEndpoint(root)
	want, discoverErr := discoverTransition(root, endpoint)
	discoverErr = errors.Join(endpointErr, discoverErr)
	add("p25-transition-discovery", discoverErr, "Git and manifest independently discover source, artifact and acceptance closure")
	if os.Getenv("AROP_PRINT_P25_TRANSITION") == "1" {
		encoded, marshalErr := json.MarshalIndent(want, "", "  ")
		fatal(errors.Join(discoverErr, marshalErr))
		fmt.Println(string(encoded))
		return
	}
	add("p25-transition-waiver", validateTransition(root, want), "validated waiver exactly equals discovered closure")
	add("p25-carrier-first", validateCarrier(root, successor, endpoint), "P25 carrier is first after P24 and the next tree-identical carrier freezes P25")
	add("p25-no-new-persistence", validateNoPersistence(root), "P25 introduces no migration, schema or database adapter")

	tests := run(root, nil, "go", "test", "-json", "-race", "-count=1", "./sdk/go/worker", "./reference/agents/pull-worker", "./reference/agents/pull-worker/cmd/arop-pull-worker")
	add("p25-go-tests", tests.err, "SDK and reference worker pass race-enabled tests")
	add("p25-test-terminals", validateTestTerminals(tests.output), "required concurrency, backpressure, retry, cancel, drain and token-safety tests pass exactly")
	vet := run(root, nil, "go", "vet", "./sdk/go/worker", "./reference/agents/pull-worker", "./reference/agents/pull-worker/cmd/arop-pull-worker")
	add("p25-go-vet", vet.err, "SDK and reference worker pass go vet")
	imports := run(root, nil, "go", "list", "-deps", "-f", "{{.ImportPath}}", "./sdk/go/worker", "./reference/agents/pull-worker/...")
	add("p25-import-boundary", validateImports(root, imports), "public SDK has no Reference import and Reference worker has no Control Plane internal import")

	p24 := run(root, nil, "make", "test-worker-service")
	add("p25-p24-regression", p24.err, "P24 Worker Pull service replays at its immutable endpoint and passes current black-box tests")

	evidence := []report.RuntimeEvidence{
		{Kind: "p24-regression", SHA256: report.Hash(p24.output), Bytes: int64(len(p24.output))},
		{Kind: "p25-go-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p25-go-vet", SHA256: report.Hash(vet.output), Bytes: int64(len(vet.output))},
		{Kind: "p25-import-closure", SHA256: report.Hash(imports.output), Bytes: int64(len(imports.output))},
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P25", Suite: "AROP P25 Go Worker SDK", Class: "p25.worker.sdk",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 2, "runtime_inputs": 0, "storage_migrations": 0},
		AuditNote: "P25 provides a redirect-proof Worker Pull client, bounded claim loop, lease renewal, stable completion retry identity, Attempt-independent effect ids, monotonic event metadata, bounded concurrency and graceful drain. The generic reference worker imports only public SDK surfaces, reloads a private regular credential file for rotation, and never accepts token material on its command line. Delivery is explicitly at-least-once and adds no persistence.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P25/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P25 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P25 checks failed; see build/reports/P25/report.json"))
	}
	fmt.Printf("AROP Go Worker SDK passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	manifest, err := loadManifest(root)
	if err != nil {
		return err
	}
	owned := []string{}
	var dependencies, runtimeInputs []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P25" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-go-worker-sdk" {
				return fmt.Errorf("invalid P25 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p25" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
			runtimeInputs = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	sort.Strings(dependencies)
	if !reflect.DeepEqual(owned, requiredOwned) || !reflect.DeepEqual(dependencies, requiredOwned) || len(runtimeInputs) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func discoverTransition(root, endpoint string) (discovered, error) {
	manifest, err := loadManifest(root)
	if err != nil {
		return discovered{}, err
	}
	diff := run(root, nil, "git", "diff", "--name-status", "--no-renames", "-z", baseline+".."+endpoint, "--")
	if diff.err != nil {
		return discovered{}, diff.err
	}
	fields := bytes.Split(diff.output, []byte{0})
	sources := []string{}
	for index := 0; index+1 < len(fields); index += 2 {
		status, path := string(fields[index]), filepath.ToSlash(string(fields[index+1]))
		if status == "" || path == "" {
			continue
		}
		if !strings.Contains("ACMDT", status[:1]) {
			return discovered{}, fmt.Errorf("unsupported Git status %q for %s", status, path)
		}
		sources = append(sources, path)
	}
	sort.Strings(sources)
	artifacts, acceptance := map[string]bool{}, map[string]bool{}
	for _, source := range sources {
		matches := longestOwners(manifest, source)
		if len(matches) == 0 && source == "Makefile" {
			contents, readErr := os.ReadFile(filepath.Join(root, "Makefile"))
			if readErr != nil || !bytes.Contains(contents, []byte("test-go-worker-sdk:")) || !bytes.Contains(contents, []byte("./reference/agents/pull-worker/testdata/harness")) {
				return discovered{}, errors.New("Makefile does not bind exact P25 checker")
			}
			acceptance[command] = true
			continue
		}
		if len(matches) == 0 {
			return discovered{}, fmt.Errorf("changed source has no concrete manifest owner: %s", source)
		}
		for _, artifact := range matches {
			artifacts[artifact.ID] = true
			if artifact.AcceptanceTest != "" {
				acceptance[strings.Replace(artifact.AcceptanceTest, "make-", "make ", 1)] = true
			}
		}
	}
	return discovered{Artifacts: mapKeys(artifacts), Sources: sources, Acceptance: mapKeys(acceptance)}, nil
}

func longestOwners(manifest blueprint.Manifest, source string) []blueprint.Artifact {
	best := -1
	matches := []blueprint.Artifact{}
	for _, artifact := range manifest.Artifacts {
		if artifact.PathRole != "concrete" || artifact.Path == "" || artifact.Path == "build/reports/P25" {
			continue
		}
		path := strings.TrimSuffix(filepath.ToSlash(artifact.Path), "/")
		if source != path && !strings.HasPrefix(source, path+"/") {
			continue
		}
		if len(path) > best {
			best, matches = len(path), []blueprint.Artifact{artifact}
		} else if len(path) == best {
			matches = append(matches, artifact)
		}
	}
	return matches
}

func validateTransition(root string, want discovered) error {
	_, endpoint, err := immutableEndpoint(root)
	if err != nil {
		return err
	}
	archived := run(root, nil, "git", "show", endpoint+":"+waiver)
	if archived.err != nil {
		return archived.err
	}
	data := archived.output
	var value transition
	if err := strictJSON(data, &value); err != nil {
		return err
	}
	return compareTransition(value, want)
}

func compareTransition(value transition, want discovered) error {
	if value.SchemaVersion != 1 || value.TransitionID != "P25-BASELINE-TRANSITION-001" || value.Status != "validated" || value.BaselineCommit != baseline || value.CarrierCommit != carrier {
		return errors.New("transition identity or lifecycle is not exact")
	}
	if !reflect.DeepEqual(value.AffectedArtifacts, want.Artifacts) || !reflect.DeepEqual(value.SourceClosure, want.Sources) || !reflect.DeepEqual(value.AcceptanceClosure, want.Acceptance) || !reflect.DeepEqual(value.Constraints, requiredConstraints) {
		return fmt.Errorf("transition closure mismatch")
	}
	return nil
}

func validateCarrier(root, successor, endpoint string) error {
	if run(root, nil, "git", "merge-base", "--is-ancestor", carrier, "HEAD").err != nil {
		return errors.New("P25 carrier is not an ancestor")
	}
	parent := run(root, nil, "git", "rev-parse", carrier+"^")
	if parent.err != nil || strings.TrimSpace(string(parent.output)) != baseline {
		return errors.New("P25 carrier parent is not final P24")
	}
	changed := run(root, nil, "git", "diff-tree", "--no-commit-id", "--name-only", "-r", carrier)
	if changed.err != nil || strings.TrimSpace(string(changed.output)) != waiver {
		return fmt.Errorf("P25 carrier changed unexpected paths: %s", changed.output)
	}
	if successor == "" || endpoint == "" || run(root, nil, "git", "diff", "--quiet", endpoint, successor, "--").err != nil {
		return errors.New("P26 carrier is not tree-identical to final P25")
	}
	return nil
}

func immutableEndpoint(root string) (string, string, error) {
	commits := run(root, nil, "git", "rev-list", "--reverse", "--ancestry-path", carrier+"..HEAD")
	if commits.err != nil {
		return "", "", commits.err
	}
	for _, commit := range strings.Fields(string(commits.output)) {
		parent := run(root, nil, "git", "rev-parse", commit+"^")
		if parent.err != nil {
			return "", "", parent.err
		}
		endpoint := strings.TrimSpace(string(parent.output))
		if run(root, nil, "git", "diff", "--quiet", endpoint, commit, "--").err == nil {
			return commit, endpoint, nil
		}
	}
	return "", "", errors.New("P26 tree-identical carrier is unavailable")
}

func validateNoPersistence(root string) error {
	diff := run(root, nil, "git", "diff", "--name-only", baseline+"..HEAD", "--")
	if diff.err != nil {
		return diff.err
	}
	for _, path := range strings.Fields(string(diff.output)) {
		if strings.HasSuffix(path, ".sql") || strings.Contains(path, "/migrations/") || strings.Contains(path, "/storage/") {
			return fmt.Errorf("P25 changed persistence path %s", path)
		}
	}
	return nil
}

func validateImports(root string, result commandResult) error {
	if result.err != nil {
		return result.err
	}
	for _, path := range []string{"sdk/go/worker/client.go", "sdk/go/worker/runner.go", "sdk/go/worker/helpers.go"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("/reference/")) {
			return fmt.Errorf("public SDK imports reference path: %s", path)
		}
	}
	if bytes.Contains(result.output, []byte("/reference/control-plane/internal/")) {
		return errors.New("reference worker imports Control Plane internal package")
	}
	return nil
}

func validateTestTerminals(output []byte) error {
	required := map[string]bool{
		"TestClientClaimCompleteAndNoRedirect":                  false,
		"TestClientRejectsAmbiguousAndLeakyResponses":           false,
		"TestClientCancellationAndConfiguration":                false,
		"TestRunnerRetriesIdenticalCompletionAndRenews":         false,
		"TestRunnerBackpressureAndDrainCancellation":            false,
		"TestRunnerDrainCancelsBlockingClaim":                   false,
		"TestRunnerReleasesPanickingHandlerWithoutProcessCrash": false,
		"TestEffectIDStableAcrossAttempts":                      false,
		"TestEventHelperConcurrentSequenceAndFencing":           false,
		"TestEventHelperResumesDurableProducerSequence":         false,
		"TestSecureIdentifiersAreUUIDv7":                        false,
		"TestNewRunnerRejectsInvalidWireConfiguration":          false,
		"TestEchoHandlerProducesTerminalSnapshot":               false,
		"TestFileCredentialSourceRotationAndSafety":             false,
		"TestParseOptions":                                      false,
	}
	packagePass := map[string]bool{}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct{ Action, Package, Test string }
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		if event.Action == "fail" || event.Action == "skip" {
			return fmt.Errorf("forbidden test terminal %s %s", event.Package, event.Test)
		}
		if event.Action == "pass" && event.Test == "" {
			packagePass[event.Package] = true
		}
		if event.Action == "pass" {
			if _, ok := required[event.Test]; ok {
				required[event.Test] = true
			}
		}
	}
	for name, passed := range required {
		if !passed {
			return fmt.Errorf("required test did not pass: %s", name)
		}
	}
	if len(packagePass) != 3 {
		return fmt.Errorf("expected three package terminals, got %v", packagePass)
	}
	return nil
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	values := []string{}
	for _, value := range bytes.Split(result.output, []byte{0}) {
		if len(value) > 0 {
			values = append(values, string(value))
		}
	}
	sort.Strings(values)
	return values, nil
}

func loadManifest(root string) (blueprint.Manifest, error) {
	var manifest blueprint.Manifest
	err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest)
	return manifest, err
}

func mapKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func run(directory string, overrides map[string]string, name string, args ...string) commandResult {
	process := exec.Command(name, args...)
	process.Dir = directory
	process.Env = cleanEnvironment(overrides)
	output, err := process.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment(overrides map[string]string) []string {
	banned := []string{"GOFLAGS=", "GOENV=", "GOWORK=", "GOCACHE=", "GOCACHEPROG=", "GOMODCACHE=", "GOTMPDIR=", "GOROOT=", "GOTOOLCHAIN=", "GOEXPERIMENT=", "CGO_ENABLED=", "NODE_OPTIONS=", "NODE_PATH=", "NPM_CONFIG_NODE_OPTIONS=", "PYTHONHOME=", "PYTHONPATH=", "PYTHONSTARTUP=", "PYTHONINSPECT=", "PYTHONWARNINGS=", "PYTHONUSERBASE=", "PGHOST=", "PGHOSTADDR=", "PGPORT=", "PGDATABASE=", "PGUSER=", "PGPASSWORD=", "PGSERVICE=", "PGSERVICEFILE=", "PGPASSFILE=", "PGOPTIONS="}
	values := []string{}
	for _, entry := range os.Environ() {
		reject := false
		for _, prefix := range banned {
			if strings.HasPrefix(entry, prefix) {
				reject = true
				break
			}
		}
		if !reject {
			values = append(values, entry)
		}
	}
	defaults := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC"}
	for key, value := range overrides {
		defaults[key] = value
	}
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values = append(values, key+"="+defaults[key])
	}
	return values
}

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), filepath.Clean(os.TempDir()), "<tmp>")
	text = strings.ReplaceAll(text, "\n", " ")
	if len(text) > 6000 {
		text = text[len(text)-6000:]
	}
	return text
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
