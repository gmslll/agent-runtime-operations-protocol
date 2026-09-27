// Command verification is the read-only P26 operations and security verifier
// pre-positioned by P25.
package main

import (
	"bytes"
	"errors"
	"fmt"
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
	command = "make verify-operations-security"
	checker = "reference/agents/pull-worker/testdata/verification/main.go"
	// Every P08-P22 archived input was last refreshed at the tree-identical P23
	// carrier. P23-P25 are rerun at the tree-identical P26 carrier.
	archiveCommit = "5bada508fa5e7035abd1d33817b4de7abf3ee49d"
	p25Carrier    = "13147594ab2258b4bf57553abe03f7543b4b7c51"
)

var phases = []string{"P08", "P10", "P12", "P13", "P18", "P19", "P20", "P21", "P22", "P23", "P24", "P25"}
var archivedPhases = []string{"P08", "P10", "P12", "P13", "P18", "P19", "P20", "P21", "P22"}

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

	p26Carrier, endpoint, carrierErr := immutableCarrier(root)
	add("p26-tree-identical-carrier", carrierErr, "P26 is a tree-identical read-only commit whose parent is final P25")
	add("p26-no-owned-artifacts", validateManifest(root), "P26 owns no artifacts and consumes exact P08/P10/P12/P13/P18-P25 reports")
	archiveEvidence, archiveErr := validateArchivedReports(root)
	add("p26-archived-report-chain", archiveErr, "nine predecessor reports reverify in an isolated immutable archive")
	currentEvidence, currentErr := validateCurrentReports(root, p26Carrier)
	add("p26-current-report-chain", currentErr, "P23-P25 reports are successful and bound to the P26 carrier")

	rootTests := run(root, nil, "go", "test", "-race", "-count=1", "./sdk/go/consumer", "./sdk/go/provider", "./sdk/go/worker", "./reference/agents/pull-worker", "./reference/agents/pull-worker/cmd/arop-pull-worker", "./cmd/arop/internal/commands/publish")
	add("p26-root-security-tests", rootTests.err, "credential, redirect, SSRF, provider and Worker safety tests pass under race detection")
	nestedTests := runNested(root)
	add("p26-control-plane-security-tests", nestedTests.err, "identity, SecretRef, URL/Asset, audit/trace, delivery and migration security tests pass under race detection")
	add("p26-test-terminals", rejectIncompleteTests(append(append([]byte(nil), rootTests.output...), nestedTests.output...)), "security suites contain no fail, skip, cache or no-test terminal")
	storage := run(root, nil, "make", "test-storage-migrations")
	add("p26-backup-restore-regression", storage.err, "live dual-store migration, backup/restore, hostile credential and readiness matrix passes")
	add("p26-sensitive-output-scan", scanEvidence(rootTests.output, nestedTests.output, storage.output), "runtime evidence contains no credential, lease token, DSN or password sentinel")

	runtimeInputs := []string{}
	for _, phase := range phases {
		runtimeInputs = append(runtimeInputs, "build/reports/"+phase+"/report.json", "build/reports/"+phase+"/junit.xml")
	}
	inputs := []string{"Makefile", "spec/artifact-manifest.yaml", checker, "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go"}
	evidence := []report.RuntimeEvidence{
		{Kind: "archived-report-reverification", SHA256: report.Hash(archiveEvidence), Bytes: int64(len(archiveEvidence))},
		{Kind: "current-report-reverification", SHA256: report.Hash(currentEvidence), Bytes: int64(len(currentEvidence))},
		{Kind: "root-security-tests", SHA256: report.Hash(rootTests.output), Bytes: int64(len(rootTests.output))},
		{Kind: "control-plane-security-tests", SHA256: report.Hash(nestedTests.output), Bytes: int64(len(nestedTests.output))},
		{Kind: "backup-restore-regression", SHA256: report.Hash(storage.output), Bytes: int64(len(storage.output))},
		{Kind: "p25-endpoint", SHA256: report.Hash([]byte(endpoint)), Bytes: int64(len(endpoint))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P26", Suite: "AROP P26 operations and security verification", Class: "p26.operations.security",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: runtimeInputs, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 0, "logical_inputs": phases, "runtime_input_files": len(runtimeInputs)},
		AuditNote: "P26 changes no tracked tree bytes. It revalidates P08/P10/P12/P13/P18-P25 evidence, reruns credential and SecretRef isolation, redirect/SSRF, Asset, Provider, Worker, audit/trace, limit and live dual-store backup/restore suites, and records only digests and byte counts. Any defect returns to its first implementation owner rather than being patched here.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P26/report.json"})
	fatal(err)
	if mode != "current-worktree" || !written.Success || !verified.Success {
		fatal(errors.New("P26 verification did not produce a current successful report"))
	}
	fmt.Printf("AROP operations and security verification passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned := []string{}
	var dependencies, runtimeInputs []string
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P26" {
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p26" {
			dependencies = append([]string(nil), artifact.DerivesFrom...)
			runtimeInputs = append([]string(nil), artifact.RuntimeInputs...)
		}
	}
	want := make([]string, len(phases))
	for index, phase := range phases {
		want[index] = "phase-report-" + strings.ToLower(phase)
	}
	if len(owned) != 0 || len(dependencies) != 0 || !reflect.DeepEqual(runtimeInputs, want) {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func immutableCarrier(root string) (string, string, error) {
	commits := run(root, nil, "git", "rev-list", "--reverse", "--ancestry-path", p25Carrier+"..HEAD")
	if commits.err != nil {
		return "", "", commits.err
	}
	var carrier, endpoint string
	for _, commit := range strings.Fields(string(commits.output)) {
		parent := run(root, nil, "git", "rev-parse", commit+"^")
		if parent.err != nil {
			return "", "", parent.err
		}
		candidateEndpoint := strings.TrimSpace(string(parent.output))
		if run(root, nil, "git", "diff", "--quiet", candidateEndpoint, commit, "--").err == nil {
			carrier, endpoint = commit, candidateEndpoint
		}
	}
	if carrier == "" {
		return "", "", errors.New("P26 tree-identical carrier is unavailable")
	}
	return carrier, endpoint, nil
}

func validateArchivedReports(root string) ([]byte, error) {
	temporary, err := os.MkdirTemp("/tmp", "arop-p26-archive-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(temporary)
	added := run(root, nil, "git", "worktree", "add", "--detach", temporary, archiveCommit)
	if added.err != nil {
		return added.output, added.err
	}
	defer func() { _ = run(root, nil, "git", "worktree", "remove", "--force", temporary).err }()
	var evidence bytes.Buffer
	for _, phase := range archivedPhases {
		destination := filepath.Join(temporary, "build", "reports", phase)
		if err := os.MkdirAll(destination, 0o700); err != nil {
			return evidence.Bytes(), err
		}
		for _, name := range []string{"report.json", "junit.xml"} {
			data, readErr := os.ReadFile(filepath.Join(root, "build", "reports", phase, name))
			if readErr != nil {
				return evidence.Bytes(), readErr
			}
			if writeErr := os.WriteFile(filepath.Join(destination, name), data, 0o600); writeErr != nil {
				return evidence.Bytes(), writeErr
			}
		}
		value, mode, verifyErr := report.Verify(report.VerifyOptions{Root: temporary, ReportPath: "build/reports/" + phase + "/report.json"})
		if verifyErr != nil || mode != "current-worktree" || !value.Success || value.Provenance.Git.Head != archiveCommit {
			return evidence.Bytes(), fmt.Errorf("archived %s verification failed in mode %s: %w", phase, mode, verifyErr)
		}
		fmt.Fprintf(&evidence, "%s:%s:%d\n", phase, value.Provenance.Git.Head, len(value.Checks))
	}
	return evidence.Bytes(), nil
}

func validateCurrentReports(root, carrier string) ([]byte, error) {
	var evidence bytes.Buffer
	for _, phase := range []string{"P23", "P24", "P25"} {
		value, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/" + phase + "/report.json"})
		if err != nil || mode != "current-worktree" || !value.Success || value.Provenance.Git.Head != carrier {
			return evidence.Bytes(), fmt.Errorf("current %s verification failed in mode %s: %w", phase, mode, err)
		}
		fmt.Fprintf(&evidence, "%s:%s:%d\n", phase, value.Provenance.Git.Head, len(value.Checks))
	}
	return evidence.Bytes(), nil
}

func runNested(root string) commandResult {
	temporary, err := os.MkdirTemp("/tmp", "arop-p26-go-work-")
	if err != nil {
		return commandResult{err: err}
	}
	defer os.RemoveAll(temporary)
	work := filepath.Join(temporary, "go.work")
	body := "go 1.24.0\n\nuse " + filepath.Join(root, "reference/control-plane") + "\n\nreplace github.com/gmslll/agent-runtime-operations-protocol => " + root + "\n"
	if err := os.WriteFile(work, []byte(body), 0o600); err != nil {
		return commandResult{err: err}
	}
	packages := []string{"./internal/adapters/secrets", "./internal/identity", "./internal/domain/assets", "./internal/domain/publication", "./internal/domain/run", "./internal/domain/dispatch", "./internal/domain/event", "./internal/app/delivery", "./internal/app/streaming", "./internal/app/platform/httpadapter", "./internal/storage/migrate"}
	arguments := append([]string{"test", "-json", "-race", "-count=1"}, packages...)
	return run(filepath.Join(root, "reference/control-plane"), map[string]string{"GOWORK": work, "TMPDIR": temporary}, "go", arguments...)
}

func rejectIncompleteTests(output []byte) error {
	for _, marker := range []string{`"Action":"skip"`, `"Action":"fail"`, "[no test files]", "(cached)"} {
		if bytes.Contains(output, []byte(marker)) {
			return fmt.Errorf("test output contains forbidden terminal %q", marker)
		}
	}
	return nil
}

func scanEvidence(outputs ...[]byte) error {
	combined := bytes.Join(outputs, nil)
	for _, forbidden := range [][]byte{[]byte("wlt_"), []byte("Bearer ey"), []byte("postgresql://"), []byte("password=")} {
		if bytes.Contains(combined, forbidden) {
			return fmt.Errorf("runtime evidence contains forbidden sentinel %q", forbidden)
		}
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
