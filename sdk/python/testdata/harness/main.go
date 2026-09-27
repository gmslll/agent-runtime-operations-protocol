//go:build ignore

// Command harness is the sole writer of the P27 Python Provider report.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	command  = "make test-python-provider"
	checker  = "sdk/python/testdata/harness/main.go"
	baseline = "6ba1651f0cc376d1b2d749d8ccbba13a8a7c1773"
)

var requiredOwned = []string{
	"python-asgi-runtime",
	"python-package-metadata",
	"python-package-primitive",
	"python-provider-durable-store-port",
	"python-provider-sdk",
	"python-runtime-registration-client",
	"python-worker-client",
	"reference-python-http-agent",
	"reference-python-sqlite-adapter",
	"reference-python-sqlite-migration",
}

var requiredTests = []string{
	"test_asgi_create_and_strict_headers",
	"test_asgi_oversize_and_duplicate_header_fail_closed",
	"test_authentication_error_never_contains_token",
	"test_claim_complete_and_fencing",
	"test_clean_venv_installs_offline_and_imports_public_surfaces",
	"test_completion_retry_reuses_identity_and_clears_outbox",
	"test_create_replay_effect_and_outbox_are_durable",
	"test_deadline_cancels_handler_and_persists_timed_out_result",
	"test_drain_cancels_blocked_claim",
	"test_effect_id_is_attempt_independent",
	"test_expired_claim_releases_without_invoking_handler",
	"test_idempotency_digest_conflict_fails_closed",
	"test_recover_accepted_record_after_restart",
	"test_register_keepalive_drain_deregister",
	"test_registration_error_is_redacted",
	"test_registration_loop_reregisters_after_keepalive_failure",
	"test_renewal_failure_cancels_handler_and_releases_claim",
	"test_sqlite_rejects_symlinked_parent",
	"test_sqlite_rejects_dirty_unversioned_database",
	"test_sqlite_reopen_history_and_schema_tamper_fail_closed",
	"test_sqlite_rollback_and_readiness",
	"test_stream_authentication_fails_before_response_start",
	"test_stream_emits_bound_cloudevent_envelopes",
	"test_two_builds_are_identical_and_inventory_is_exact",
}

type commandResult struct {
	output []byte
	err    error
}

type packageManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	Artifacts     []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"artifacts"`
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

	inputs, inputErr := staticInputs(root)
	add("p27-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P27 inputs", len(inputs)))
	add("p27-manifest-inventory", validateManifest(root), "exact ten P27-owned artifacts and empty runtime inputs")

	pythonEnv := map[string]string{
		"PYTHONPATH":                    filepath.Join(root, "sdk/python/src") + string(os.PathListSeparator) + filepath.Join(root, "reference/agents/python-http"),
		"PYTHONDONTWRITEBYTECODE":       "1",
		"PYTHONHASHSEED":                "0",
		"PIP_CONFIG_FILE":               os.DevNull,
		"PIP_NO_INDEX":                  "1",
		"PIP_DISABLE_PIP_VERSION_CHECK": "1",
	}
	tests := run(root, pythonEnv, "python3", "-B", "-m", "unittest", "discover", "-s", "sdk/python/tests", "-v")
	add("p27-python-tests", tests.err, "Python provider, ASGI, registry, worker, SQLite and package tests pass")
	add("p27-test-inventory", validateTestInventory(tests.output), "exact 24 tests pass without skip or cache")

	temporary, err := os.MkdirTemp("", "arop-p27-package-")
	fatal(err)
	defer os.RemoveAll(temporary)
	buildA := buildPackage(root, pythonEnv, filepath.Join(temporary, "a"))
	buildB := buildPackage(root, pythonEnv, filepath.Join(temporary, "b"))
	add("p27-package-build-a", buildA.err, "first offline package primitive run succeeds")
	add("p27-package-build-b", buildB.err, "second offline package primitive run succeeds")
	packageEvidence, packageErr := comparePackages(filepath.Join(temporary, "a"), filepath.Join(temporary, "b"))
	add("p27-package-determinism", packageErr, "wheel, sdist and manifest are byte-identical with exact metadata")

	codegen := run(root, nil, "make", "test-codegen-pipeline")
	add("p27-p07-codegen-regression", codegen.err, "P07 generator and generated Go/Python/TypeScript outputs remain deterministic")
	add("p27-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")

	evidence := []report.RuntimeEvidence{
		{Kind: "p07-codegen-regression", SHA256: report.Hash(codegen.output), Bytes: int64(len(codegen.output))},
		{Kind: "p27-package-artifacts", SHA256: report.Hash(packageEvidence), Bytes: int64(len(packageEvidence))},
		{Kind: "p27-python-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P27", Suite: "AROP P27 Python Provider", Class: "p27.python.provider",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 10, "runtime_inputs": 0, "python_tests": len(requiredTests), "package_artifacts": 2},
		AuditNote: "P27 supplies a dependency-free Python Provider SDK, strict ASGI HTTP/SSE runtime, RuntimeRegistration and Worker clients, a driver-neutral transactional durability port, and a reference SQLite Agent. Worker completion identity and provider effects survive ambiguous delivery and restart. The offline PEP 517 primitive emits deterministic wheel/sdist artifacts with an exact allowlist, valid RECORD/METADATA, no embedded credential or host path, and clean-venv import coverage.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P27/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P27 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P27 checks failed; see build/reports/P27/report.json"))
	}
	fmt.Printf("AROP Python Provider passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned := []string{}
	dependencies := []string{}
	runtimeInputs := []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P27" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-python-provider" {
				return fmt.Errorf("invalid P27 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p27" {
			dependencies = append(dependencies, artifact.DerivesFrom...)
			runtimeInputs = append(runtimeInputs, artifact.RuntimeInputs...)
		}
	}
	sort.Strings(owned)
	sort.Strings(dependencies)
	want := append([]string(nil), requiredOwned...)
	sort.Strings(want)
	if !reflect.DeepEqual(owned, want) || !reflect.DeepEqual(dependencies, want) || len(runtimeInputs) != 0 {
		return fmt.Errorf("owned=%v dependencies=%v runtime_inputs=%v", owned, dependencies, runtimeInputs)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	result := run(root, nil, "git", "diff", "--name-status", "--no-renames", "-z", baseline+"..HEAD", "--")
	if result.err != nil {
		return nil, result.err
	}
	values := map[string]bool{"spec/artifact-manifest.yaml": true}
	fields := bytes.Split(result.output, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		status, name := string(fields[index]), filepath.ToSlash(string(fields[index+1]))
		if status == "" || name == "" {
			continue
		}
		if status[0] != 'A' && status[0] != 'M' && status[0] != 'T' {
			return nil, fmt.Errorf("unsupported P27 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P27 static closure: %s", name)
		}
		information, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !information.Mode().IsRegular() {
			return nil, fmt.Errorf("P27 input is not regular: %s", name)
		}
		values[name] = true
	}
	paths := make([]string, 0, len(values))
	for value := range values {
		paths = append(paths, value)
	}
	sort.Strings(paths)
	return paths, nil
}

func validateTestInventory(output []byte) error {
	seen := []string{}
	for _, raw := range bytes.Split(output, []byte{'\n'}) {
		line := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(line, "test_") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[len(fields)-1] != "ok" || fields[len(fields)-2] != "..." {
			return fmt.Errorf("non-pass Python test terminal: %s", line)
		}
		seen = append(seen, fields[0])
	}
	sort.Strings(seen)
	want := append([]string(nil), requiredTests...)
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) {
		return fmt.Errorf("Python test inventory=%v want=%v", seen, want)
	}
	return nil
}

func buildPackage(root string, environment map[string]string, directory string) commandResult {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return commandResult{err: err}
	}
	return run(root, environment, "python3", "-B", "sdk/python/scripts/build_package.py", "--out-dir", filepath.Join(directory, "dist"), "--manifest", filepath.Join(directory, "manifest.json"))
}

func comparePackages(first, second string) ([]byte, error) {
	firstData, err := os.ReadFile(filepath.Join(first, "manifest.json"))
	if err != nil {
		return nil, err
	}
	secondData, err := os.ReadFile(filepath.Join(second, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(firstData, secondData) {
		return nil, errors.New("package manifests differ")
	}
	var manifest packageManifest
	decoder := json.NewDecoder(bytes.NewReader(firstData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("package manifest has trailing data")
	}
	if manifest.SchemaVersion != 1 || manifest.Name != "arop-sdk" || manifest.Version != "0.1.0.dev0" || len(manifest.Artifacts) != 2 {
		return nil, errors.New("package manifest identity or inventory mismatch")
	}
	wantNames := []string{"arop_sdk-0.1.0.dev0-py3-none-any.whl", "arop_sdk-0.1.0.dev0.tar.gz"}
	for index, artifact := range manifest.Artifacts {
		if artifact.Name != wantNames[index] || len(artifact.SHA256) != 64 || artifact.Bytes <= 0 {
			return nil, fmt.Errorf("invalid package artifact metadata: %+v", artifact)
		}
		left, err := os.ReadFile(filepath.Join(first, "dist", artifact.Name))
		if err != nil {
			return nil, err
		}
		right, err := os.ReadFile(filepath.Join(second, "dist", artifact.Name))
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(left)
		if !bytes.Equal(left, right) || hex.EncodeToString(digest[:]) != artifact.SHA256 || int64(len(left)) != artifact.Bytes {
			return nil, fmt.Errorf("package artifact mismatch: %s", artifact.Name)
		}
	}
	return firstData, nil
}

func run(directory string, overrides map[string]string, name string, arguments ...string) commandResult {
	process := exec.Command(name, arguments...)
	process.Dir = directory
	process.Env = cleanEnvironment(overrides)
	output, err := process.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output: output, err: err}
}

func cleanEnvironment(overrides map[string]string) []string {
	blocked := map[string]bool{
		"GOFLAGS": true, "GOENV": true, "GOWORK": true, "GOCACHE": true, "GOCACHEPROG": true, "GOMODCACHE": true,
		"GOTMPDIR": true, "GOROOT": true, "GOTOOLCHAIN": true, "GOEXPERIMENT": true, "CGO_ENABLED": true,
		"NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true,
		"PYTHONHOME": true, "PYTHONPATH": true, "PYTHONSTARTUP": true, "PYTHONINSPECT": true, "PYTHONWARNINGS": true,
		"PYTHONUSERBASE": true, "PIP_CONFIG_FILE": true, "PIP_INDEX_URL": true, "PIP_EXTRA_INDEX_URL": true, "PIP_TRUSTED_HOST": true,
	}
	values := []string{}
	for _, entry := range os.Environ() {
		key := entry
		if at := strings.IndexByte(entry, '='); at >= 0 {
			key = entry[:at]
		}
		if !blocked[strings.ToUpper(key)] {
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
	value := strings.ReplaceAll(err.Error(), filepath.Clean(os.TempDir()), "<tmp>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 6000 {
		value = value[len(value)-6000:]
	}
	return value
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
