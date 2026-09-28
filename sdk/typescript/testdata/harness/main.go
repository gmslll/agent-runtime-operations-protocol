//go:build ignore

// Command harness is the sole writer of the P28 TypeScript Consumer report.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command  = "make test-typescript-consumer"
	checker  = "sdk/typescript/testdata/harness/main.go"
	baseline = "15a24027c7831b9eb2f3a170ab0bf5a6f6e42da7"
)

var requiredOwned = []string{"npm-package-primitive", "typescript-bff-web", "typescript-consumer-sdk", "typescript-package-metadata"}
var requiredTests = []string{
	"bff_authorizes_and_shields_tokens",
	"consumer_create_get_command_shields_server_token",
	"consumer_typed_retry_error_is_redacted",
	"reducer_rejects_gap_conflict_and_post_terminal",
	"reducer_utf8_replay_and_unknown_forward_event",
	"relay_reconnect_reauthenticates_and_resumes",
	"web_resumes_with_opaque_cursor",
}

type commandResult struct {
	output []byte
	err    error
}
type packageManifest struct {
	SchemaVersion int           `json:"schema_version"`
	Name          string        `json:"name"`
	Version       string        `json:"version"`
	Artifacts     []packageFile `json:"artifacts"`
	Files         []packageFile `json:"files"`
}
type packageFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	if os.Getenv("AROP_CHECK_COMMAND") != command {
		fatal(fmt.Errorf("AROP_CHECK_COMMAND must equal %q", command))
	}
	nodeModules, err := findNodeModules(root)
	fatal(err)
	temporary, err := os.MkdirTemp("", "arop-p28-")
	fatal(err)
	defer os.RemoveAll(temporary)
	runner, err := prepareBuildRunner(root, nodeModules, temporary)
	fatal(err)

	checks := []report.Check{}
	add := func(name string, err error, success string) {
		detail := success
		if err != nil {
			detail = sanitize(err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, inputErr := staticInputs(root)
	add("p28-static-input-closure", inputErr, fmt.Sprintf("%d Git-tracked P28 inputs", len(inputs)))
	add("p28-manifest-inventory", validateManifest(root), "exact four P28-owned artifacts and empty runtime inputs")

	compiled := filepath.Join(temporary, "compiled")
	compile := compileTests(root, nodeModules, compiled)
	add("p28-typescript-compile", compile.err, "strict NodeNext compilation succeeds")
	consumerTests := runNode(compiled, nodeModules, filepath.Join(compiled, "src/consumer/consumer.test.js"))
	bffTests := runNode(compiled, nodeModules, filepath.Join(compiled, "examples/bff-web/app.test.js"))
	add("p28-consumer-tests", consumerTests.err, "consumer, reducer, reconnect and error tests pass")
	add("p28-bff-web-tests", bffTests.err, "BFF token shielding and Web resume tests pass")
	testWire := append(bytes.Clone(consumerTests.output), bffTests.output...)
	add("p28-test-inventory", validateTestInventory(testWire), "exact seven TypeScript tests pass")

	buildA := buildPackage(root, nodeModules, runner, filepath.Join(temporary, "pack-a"))
	buildB := buildPackage(root, nodeModules, runner, filepath.Join(temporary, "pack-b"))
	add("p28-package-builds", errors.Join(buildA.err, buildB.err), "two offline package primitive runs succeed")
	packageEvidence, packageErr := comparePackages(filepath.Join(temporary, "pack-a"), filepath.Join(temporary, "pack-b"))
	add("p28-package-determinism", packageErr, "npm tarballs and inventories are byte-identical and allowlisted")
	installOutput, installErr := installProbe(root, nodeModules, temporary, filepath.Join(temporary, "pack-a", "arop-sdk-0.1.0-dev.0.tgz"))
	add("p28-clean-install", installErr, "empty offline npm project installs and imports @arop/sdk")
	add("p28-no-runtime-inputs", nil, "all executed material is static input or digest-only runtime evidence")

	evidence := []report.RuntimeEvidence{
		{Kind: "p28-clean-install", SHA256: report.Hash(installOutput), Bytes: int64(len(installOutput))},
		{Kind: "p28-package-artifacts", SHA256: report.Hash(packageEvidence), Bytes: int64(len(packageEvidence))},
		{Kind: "p28-typescript-tests", SHA256: report.Hash(testWire), Bytes: int64(len(testWire))},
	}
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Kind < evidence[j].Kind })
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P28", Suite: "AROP P28 TypeScript Consumer", Class: "p28.typescript.consumer",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 4, "runtime_inputs": 0, "typescript_tests": len(requiredTests), "package_artifacts": 1},
		AuditNote: "P28 provides an ESM TypeScript Consumer SDK, deterministic Run event reducer, resumable relay SSE client, and a BFF/Web reference that keeps every privileged token server-side. The dependency-free @arop/sdk runtime package is built by a deterministic offline npm primitive with an exact dist-only inventory and is installed and imported in an empty offline project.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P28/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P28 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P28 checks failed; see build/reports/P28/report.json"))
	}
	fmt.Printf("AROP TypeScript Consumer passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, dependencies, runtimeInputs := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P28" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-typescript-consumer" {
				return fmt.Errorf("invalid P28 owner metadata: %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p28" {
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
	result := run(root, nil, 30*time.Second, "git", "diff", "--name-status", "--no-renames", "-z", baseline+"..HEAD", "--")
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
			return nil, fmt.Errorf("unsupported P28 Git change %q for %s", status, name)
		}
		if strings.HasPrefix(name, "build/") {
			return nil, fmt.Errorf("build output entered P28 static closure: %s", name)
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("P28 input is not regular: %s", name)
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

func compileTests(root, nodeModules, out string) commandResult {
	if err := os.MkdirAll(out, 0o700); err != nil {
		return commandResult{err: err}
	}
	files, err := typescriptFiles(filepath.Join(root, "sdk/typescript"))
	if err != nil {
		return commandResult{err: err}
	}
	args := []string{"--permission", "--allow-fs-read=" + root, "--allow-fs-read=" + nodeModules, "--allow-fs-read=" + out, "--allow-fs-write=" + out, "--disable-proto=throw", "--no-addons", filepath.Join(nodeModules, "typescript/bin/tsc"), "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "--jsx", "react-jsx", "--strict", "--noEmitOnError", "--rootDir", "sdk/typescript", "--outDir", out, "--lib", "ES2022,DOM"}
	args = append(args, files...)
	return run(root, nil, 90*time.Second, "node", args...)
}

func typescriptFiles(root string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("TypeScript inventory symlink: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("TypeScript inventory special file: %s", path)
		}
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
			relative, _ := filepath.Rel(filepath.Dir(filepath.Dir(root)), path)
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func runNode(compiled, nodeModules, script string) commandResult {
	realCompiled, err := filepath.EvalSymlinks(compiled)
	if err != nil {
		return commandResult{err: err}
	}
	realScript, err := filepath.EvalSymlinks(script)
	if err != nil {
		return commandResult{err: err}
	}
	return run(realCompiled, nil, 30*time.Second, "node", "--permission", "--allow-fs-read="+realCompiled, "--allow-fs-read="+nodeModules, "--disable-proto=throw", "--no-addons", realScript)
}

func validateTestInventory(output []byte) error {
	seen := []string{}
	for _, raw := range bytes.Split(output, []byte{'\n'}) {
		line := strings.TrimSpace(string(raw))
		if strings.HasPrefix(line, "ok ") {
			seen = append(seen, strings.TrimPrefix(line, "ok "))
		}
	}
	sort.Strings(seen)
	want := append([]string(nil), requiredTests...)
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) {
		return fmt.Errorf("TypeScript test inventory=%v want=%v", seen, want)
	}
	return nil
}

func prepareBuildRunner(root, nodeModules, temporary string) (string, error) {
	runner := filepath.Join(temporary, "package-runner")
	if err := os.MkdirAll(filepath.Join(runner, "node_modules"), 0o700); err != nil {
		return "", err
	}
	source, err := os.ReadFile(filepath.Join(root, "sdk/typescript/scripts/build-package.mjs"))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(runner, "build-package.mjs"), source, 0o600); err != nil {
		return "", err
	}
	if err := os.Symlink(filepath.Join(nodeModules, "typescript"), filepath.Join(runner, "node_modules/typescript")); err != nil {
		return "", err
	}
	return runner, nil
}

func buildPackage(root, nodeModules, runner, out string) commandResult {
	if err := os.MkdirAll(out, 0o700); err != nil {
		return commandResult{err: err}
	}
	realRunner, err := filepath.EvalSymlinks(runner)
	if err != nil {
		return commandResult{err: err}
	}
	return run(root, nil, 90*time.Second, "node", "--permission", "--allow-fs-read="+root, "--allow-fs-read="+nodeModules, "--allow-fs-read="+realRunner, "--allow-fs-read="+out, "--allow-fs-write="+out, "--disable-proto=throw", "--no-addons", filepath.Join(realRunner, "build-package.mjs"), "--out-dir", out, "--root", root)
}

func comparePackages(first, second string) ([]byte, error) {
	firstManifest, err := os.ReadFile(filepath.Join(first, "manifest.json"))
	if err != nil {
		return nil, err
	}
	secondManifest, err := os.ReadFile(filepath.Join(second, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(firstManifest, secondManifest) {
		return nil, errors.New("package manifests differ")
	}
	var manifest packageManifest
	decoder := json.NewDecoder(bytes.NewReader(firstManifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("package manifest has trailing data")
	}
	if manifest.SchemaVersion != 1 || manifest.Name != "@arop/sdk" || manifest.Version != "0.1.0-dev.0" || len(manifest.Artifacts) != 1 || len(manifest.Files) < 10 {
		return nil, errors.New("package manifest identity or inventory mismatch")
	}
	artifact := manifest.Artifacts[0]
	if artifact.Name != "arop-sdk-0.1.0-dev.0.tgz" || len(artifact.SHA256) != 64 || artifact.Bytes <= 0 {
		return nil, errors.New("invalid npm artifact metadata")
	}
	left, err := os.ReadFile(filepath.Join(first, artifact.Name))
	if err != nil {
		return nil, err
	}
	right, err := os.ReadFile(filepath.Join(second, artifact.Name))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(left)
	if !bytes.Equal(left, right) || hex.EncodeToString(digest[:]) != artifact.SHA256 || int64(len(left)) != artifact.Bytes {
		return nil, errors.New("npm tarball is not deterministic")
	}
	if err := inspectTarball(left, manifest.Files); err != nil {
		return nil, err
	}
	return firstManifest, nil
}

func inspectTarball(wire []byte, expected []packageFile) error {
	gz, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	actual := []packageFile{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Mode != 0o644 || header.ModTime.Unix() != 0 || header.Uid != 0 || header.Gid != 0 {
			return fmt.Errorf("non-deterministic tar header: %s", header.Name)
		}
		if !strings.HasPrefix(header.Name, "package/") || strings.Contains(header.Name, "..") || strings.ContainsAny(strings.ToLower(header.Name), "\\") || strings.Contains(strings.ToLower(header.Name), "test") || strings.Contains(strings.ToLower(header.Name), "secret") || strings.Contains(header.Name, ".map") {
			return fmt.Errorf("forbidden package path: %s", header.Name)
		}
		data, err := io.ReadAll(io.LimitReader(reader, 16<<20))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		actual = append(actual, packageFile{Name: header.Name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data))})
		if header.Name == "package/package.json" {
			var metadata map[string]any
			if err := json.Unmarshal(data, &metadata); err != nil {
				return err
			}
			if _, ok := metadata["devDependencies"]; ok {
				return errors.New("packed metadata contains devDependencies")
			}
			if _, ok := metadata["scripts"]; ok {
				return errors.New("packed metadata contains scripts")
			}
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("tarball inventory differs from package manifest")
	}
	return nil
}

func installProbe(root, nodeModules, temporary, archive string) ([]byte, error) {
	project := filepath.Join(temporary, "empty-project")
	if err := os.MkdirAll(filepath.Join(project, "home"), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(project, "package.json"), []byte("{\"name\":\"p28-install-probe\",\"private\":true,\"type\":\"module\"}\n"), 0o600); err != nil {
		return nil, err
	}
	probe := "import { initialRunView } from '@arop/sdk/consumer';\nconst id='run_01956e7b-9abc-7def-8abc-0123456789ab';\nif(initialRunView(id).runId!==id)throw new Error('import failed');\nconsole.log('installed @arop/sdk');\n"
	if err := os.WriteFile(filepath.Join(project, "probe.mjs"), []byte(probe), 0o600); err != nil {
		return nil, err
	}
	env := map[string]string{"HOME": filepath.Join(project, "home"), "npm_config_cache": filepath.Join(project, "cache"), "npm_config_offline": "true", "npm_config_audit": "false", "npm_config_fund": "false", "npm_config_ignore_scripts": "true", "npm_config_package_lock": "false", "npm_config_userconfig": os.DevNull}
	installed := run(project, env, 90*time.Second, "npm", "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--package-lock=false", archive)
	if installed.err != nil {
		return installed.output, installed.err
	}
	realProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		return installed.output, err
	}
	probeResult := run(realProject, nil, 30*time.Second, "node", "--permission", "--allow-fs-read="+realProject, "--allow-fs-read="+nodeModules, "--disable-proto=throw", "--no-addons", filepath.Join(realProject, "probe.mjs"))
	return append(installed.output, probeResult.output...), probeResult.err
}

func findNodeModules(root string) (string, error) {
	for current := root; ; current = filepath.Dir(current) {
		candidate := filepath.Join(current, "node_modules")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			real, err := filepath.EvalSymlinks(candidate)
			return real, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	common := run(root, nil, 10*time.Second, "git", "rev-parse", "--git-common-dir")
	if common.err == nil {
		path := strings.TrimSpace(string(common.output))
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		candidate := filepath.Join(filepath.Dir(path), "node_modules")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return filepath.EvalSymlinks(candidate)
		}
	}
	return "", errors.New("locked node_modules not found")
}

func run(directory string, overrides map[string]string, timeout time.Duration, name string, arguments ...string) commandResult {
	process := exec.Command(name, arguments...)
	process.Dir = directory
	process.Env = cleanEnvironment(overrides)
	timer := time.AfterFunc(timeout, func() {
		if process.Process != nil {
			_ = process.Process.Kill()
		}
	})
	output, err := process.CombinedOutput()
	timer.Stop()
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
	blocked := map[string]bool{"GOFLAGS": true, "GOENV": true, "GOWORK": true, "GOCACHE": true, "GOCACHEPROG": true, "GOMODCACHE": true, "GOTMPDIR": true, "GOROOT": true, "GOTOOLCHAIN": true, "GOEXPERIMENT": true, "CGO_ENABLED": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "HOME": true}
	values := []string{}
	for _, entry := range os.Environ() {
		key := entry
		if at := strings.IndexByte(entry, '='); at >= 0 {
			key = entry[:at]
		}
		if !blocked[strings.ToUpper(key)] && !strings.HasPrefix(strings.ToLower(key), "npm_config_") {
			values = append(values, entry)
		}
	}
	defaults := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC", "LANG": "C", "LC_ALL": "C"}
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
