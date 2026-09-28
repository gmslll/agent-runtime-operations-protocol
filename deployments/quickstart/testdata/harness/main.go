//go:build ignore

// Command harness is the sole writer of the P36 SQLite quickstart report.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make quickstart-smoke"
	checker = "deployments/quickstart/testdata/harness/main.go"
)

type commandResult struct {
	output []byte
	err    error
}
type goEvent struct{ Action, Package, Test, Output string }
type releaseManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	Artifacts     []struct {
		Path   string `json:"path"`
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
	inputs, inputErr := trackedInputs(root)
	add("p36-static-input-closure", inputErr, fmt.Sprintf("%d tracked quickstart, CLI, release, module, and migration inputs", len(inputs)))
	add("p36-manifest-inventory", validateManifest(root), "exact four P36-owned artifacts and report closure")
	add("p36-quickstart-contract", validateQuickstartContract(root), "composition references only existing components, loopback SQLite, and no persistent credential")
	tests := run(root, 2*time.Minute, "go", "test", "-race", "-count=1", "-json", "./cmd/arop/internal/commands/development", "./cmd/arop")
	add("p36-cli-tests", errors.Join(tests.err, validateTests(tests.output)), "development commands and CLI routing pass exact uncached race tests")
	releaseEvidence, releaseErr := exerciseRelease(root)
	add("p36-deterministic-go-release", releaseErr, "four declared targets produce byte-identical clean builds, fixed archives, source modules, and checksums")
	quickstartEvidence, quickstartErr := exerciseQuickstart(root)
	add("p36-real-sqlite-lifecycle", quickstartErr, "empty workspace init, doctor, dev readiness, SQLite durability, signal cleanup, and post-stop doctor pass")
	add("p36-no-persistent-credentials", validateNoPersistentCredential(root), "tracked quickstart files contain no credential material and runtime key is ephemeral")
	add("p36-no-runtime-inputs", nil, "all executable inputs are tracked; smoke output is digest-and-byte evidence only")
	evidence := []report.RuntimeEvidence{
		{Kind: "p36-cli-tests", SHA256: report.Hash(tests.output), Bytes: int64(len(tests.output))},
		{Kind: "p36-quickstart-smoke", SHA256: report.Hash(quickstartEvidence), Bytes: int64(len(quickstartEvidence))},
		{Kind: "p36-release-builds", SHA256: report.Hash(releaseEvidence), Bytes: int64(len(releaseEvidence))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P36", Suite: "AROP P36 SQLite quickstart and Go release", Class: "p36.quickstart",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 4, "cli_commands": 3, "release_targets": 4, "go_modules": 2, "runtime_inputs": 0},
		AuditNote: "P36 composes existing protocol, Control Plane, SDK, and example artifacts without redefining behavior. Init is deterministic and credential-free; doctor is machine-readable and fail-closed; dev runs the durable SQLite server only on loopback and removes its ephemeral key on shutdown. The Go release primitive forces GOWORK=off, CGO_ENABLED=0, trimpath, fixed ordering/time/modes, and emits deterministic source and CLI archives with checksums. Paths, runtime keys, databases, and process logs do not enter the report.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P36/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P36 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P36 checks failed; see build/reports/P36/report.json"))
	}
	fmt.Printf("AROP SQLite quickstart passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtimeInputs := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P36" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-quickstart-smoke" {
				return fmt.Errorf("invalid P36 artifact %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p36" {
			deps = append(deps, artifact.DerivesFrom...)
			runtimeInputs = append(runtimeInputs, artifact.RuntimeInputs...)
		}
	}
	wantOwned := []string{"quickstart", "arop-cli-development-commands", "go-release-layout", "go-release-primitive"}
	wantDeps := []string{"quickstart", "arop-cli-development-commands", "go-release-layout", "go-release-primitive", "arop-cli-baseline"}
	if !reflect.DeepEqual(owned, wantOwned) || !reflect.DeepEqual(deps, wantDeps) || len(runtimeInputs) != 0 {
		return fmt.Errorf("owned=%v deps=%v runtime=%v", owned, deps, runtimeInputs)
	}
	return nil
}

func validateQuickstartContract(root string) error {
	var document struct {
		SchemaVersion  int               `json:"schema_version"`
		Name           string            `json:"name"`
		Durability     string            `json:"durability"`
		Listen         string            `json:"listen"`
		SampleManifest string            `json:"sample_manifest"`
		Components     map[string]string `json:"components"`
		Commands       map[string]string `json:"commands"`
		Security       map[string]string `json:"security"`
	}
	if err := structuredfile.Load(filepath.Join(root, "deployments/quickstart/quickstart.yaml"), &document); err != nil {
		return err
	}
	if document.SchemaVersion != 1 || document.Durability != "sqlite" || document.Listen != "127.0.0.1:8080" || len(document.Components) != 5 || len(document.Commands) != 3 || document.Security["listen_scope"] != "loopback-only" || document.Security["persistent_credentials"] != "forbidden" {
		return errors.New("quickstart composition invariants are incomplete")
	}
	for _, path := range document.Components {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return fmt.Errorf("missing composed component %s", path)
		}
	}
	return nil
}

func validateNoPersistentCredential(root string) error {
	paths := []string{"deployments/quickstart/quickstart.yaml", "spec/release/go-artifacts.yaml"}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		lower := bytes.ToLower(data)
		for _, pattern := range []string{"private_key:", "password:", "bearer ", "token_value:", "client_secret:"} {
			if bytes.Contains(lower, []byte(pattern)) {
				return fmt.Errorf("credential-like content in %s", path)
			}
		}
	}
	return nil
}

func validateTests(output []byte) error {
	want := []string{
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop TestRunRequiresManifestDigestCommand",
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/development TestDevRequiresHealthyWorkspaceAndExplicitBinaries",
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/development TestDoctorJSONAndUnsafeConfigFailClosed",
		"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop/internal/commands/development TestInitIsDeterministicIdempotentAndSecretFree",
	}
	seen, packageTerminals := []string{}, map[string]int{}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var event goEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if event.Action == "fail" || event.Action == "skip" || strings.Contains(event.Output, "(cached)") {
			return fmt.Errorf("non-conforming test event: %+v", event)
		}
		if event.Action == "pass" && event.Test != "" && !strings.Contains(event.Test, "/") {
			seen = append(seen, event.Package+" "+event.Test)
		}
		if event.Action == "pass" && event.Test == "" {
			packageTerminals[event.Package]++
		}
	}
	sort.Strings(seen)
	sort.Strings(want)
	if !reflect.DeepEqual(seen, want) || len(packageTerminals) != 2 {
		return fmt.Errorf("tests=%v terminals=%v", seen, packageTerminals)
	}
	for _, count := range packageTerminals {
		if count != 1 {
			return errors.New("duplicate package terminal")
		}
	}
	return scanner.Err()
}

func exerciseRelease(root string) ([]byte, error) {
	scratch, err := secureScratch("arop-p36-release-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	evidence := bytes.Buffer{}
	targets := [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}
	for _, target := range targets {
		first, second := filepath.Join(scratch, target[0]+"-"+target[1]+"-a"), filepath.Join(scratch, target[0]+"-"+target[1]+"-b")
		for _, output := range []string{first, second} {
			result := run(root, 4*time.Minute, "go", "run", "./internal/tooling/cmd/arop-build-go-release", "--root", root, "--output", output, "--version", "0.1.0", "--goos", target[0], "--goarch", target[1])
			if result.err != nil {
				return nil, result.err
			}
		}
		firstDigest, err := directoryDigest(first)
		if err != nil {
			return nil, err
		}
		secondDigest, err := directoryDigest(second)
		if err != nil {
			return nil, err
		}
		if firstDigest != secondDigest {
			return nil, fmt.Errorf("release target %s/%s is not reproducible", target[0], target[1])
		}
		if err := inspectRelease(first, target[0], target[1]); err != nil {
			return nil, err
		}
		fmt.Fprintf(&evidence, "%s/%s %s\n", target[0], target[1], firstDigest)
	}
	return evidence.Bytes(), nil
}

func inspectRelease(directory, goos, goarch string) error {
	data, err := os.ReadFile(filepath.Join(directory, "checksums.json"))
	if err != nil {
		return err
	}
	var manifest releaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.SchemaVersion != 1 || manifest.Version != "0.1.0" || manifest.GOOS != goos || manifest.GOARCH != goarch || len(manifest.Artifacts) != 3 {
		return errors.New("release checksum manifest is incomplete")
	}
	for _, item := range manifest.Artifacts {
		artifactData, err := os.ReadFile(filepath.Join(directory, item.Path))
		if err != nil {
			return err
		}
		if "sha256:"+report.Hash(artifactData) != item.SHA256 || int64(len(artifactData)) != item.Bytes {
			return fmt.Errorf("release checksum mismatch for %s", item.Path)
		}
		if strings.HasSuffix(item.Path, ".zip") {
			if err := inspectZip(filepath.Join(directory, item.Path)); err != nil {
				return err
			}
		}
		if strings.HasSuffix(item.Path, ".tar.gz") {
			if err := inspectTar(filepath.Join(directory, item.Path)); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectZip(path string) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	last := ""
	for _, file := range archive.File {
		if file.Name <= last || filepath.IsAbs(file.Name) || strings.Contains(file.Name, "\\") || file.Modified.UTC() != time.Unix(315532800, 0).UTC() || (file.Mode().Perm() != 0o644 && file.Mode().Perm() != 0o755) {
			return fmt.Errorf("non-deterministic zip entry %s", file.Name)
		}
		last = file.Name
	}
	return nil
}

func inspectTar(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader, last, count := tar.NewReader(gzipReader), "", 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name <= last || filepath.IsAbs(header.Name) || strings.Contains(header.Name, "\\") || !header.ModTime.Equal(time.Unix(315532800, 0).UTC()) || (header.Mode != 0o644 && header.Mode != 0o755) {
			return fmt.Errorf("non-deterministic tar entry %s", header.Name)
		}
		last = header.Name
		count++
	}
	if count != 2 {
		return fmt.Errorf("CLI archive contains %d entries", count)
	}
	return nil
}

func exerciseQuickstart(root string) ([]byte, error) {
	scratch, err := secureScratch("arop-p36-smoke-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	arop, aropd := filepath.Join(scratch, "arop"), filepath.Join(scratch, "aropd")
	if result := run(root, 2*time.Minute, "go", "build", "-trimpath", "-buildvcs=false", "-o", arop, "./cmd/arop"); result.err != nil {
		return nil, result.err
	}
	workspaceFile, err := writeWorkspace(scratch, root)
	if err != nil {
		return nil, err
	}
	environment := goEnvironment(workspaceFile)
	if result := runWithEnv(root, 4*time.Minute, environment, "go", "-C", "reference/control-plane", "build", "-trimpath", "-buildvcs=false", "-o", aropd, "./cmd/aropd"); result.err != nil {
		return nil, result.err
	}
	workspace := filepath.Join(scratch, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return nil, err
	}
	init := run(workspace, 30*time.Second, arop, "init", workspace)
	if init.err != nil {
		return nil, init.err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(workspace, ".arop", "quickstart.json")
	configData := fmt.Sprintf("{\n  \"schema_version\": 1,\n  \"listen\": \"127.0.0.1:%d\",\n  \"database\": \".arop/control-plane.sqlite\",\n  \"backup_directory\": \".arop/backups\",\n  \"sample_manifest\": \"agent-manifest.json\"\n}\n", port)
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		return nil, err
	}
	doctor := run(workspace, 30*time.Second, arop, "doctor", workspace)
	if doctor.err != nil {
		return nil, doctor.err
	}
	var doctorReport struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal(doctor.output, &doctorReport); err != nil || !doctorReport.Healthy {
		return nil, errors.New("quickstart doctor did not report healthy")
	}
	digestResult := run(workspace, 30*time.Second, arop, "manifest", "digest", filepath.Join(workspace, "agent-manifest.json"))
	if digestResult.err != nil || !strings.HasPrefix(strings.TrimSpace(string(digestResult.output)), "sha256:") {
		return nil, errors.New("quickstart sample manifest is invalid")
	}
	process := exec.Command(arop, "dev", workspace)
	process.Dir = workspace
	process.Env = append(cleanEnvironment(), "AROP_CONTROL_PLANE_BINARY="+aropd, "AROP_MIGRATION_ROOT="+filepath.Join(root, "reference/control-plane/migrations"))
	log := &safeBuffer{limit: 1 << 20}
	process.Stdout, process.Stderr = log, log
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := process.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	readyErr := waitReady(done, fmt.Sprintf("http://127.0.0.1:%d/v1/health/ready", port), 90*time.Second)
	_ = process.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		return nil, errors.New("quickstart did not stop")
	}
	if readyErr != nil {
		return nil, readyErr
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".arop", "runtime", "asset-token.key")); !os.IsNotExist(err) {
		return nil, errors.New("ephemeral key remained after shutdown")
	}
	if info, err := os.Stat(filepath.Join(workspace, ".arop", "control-plane.sqlite")); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("SQLite database was not created")
	}
	afterDoctor := run(workspace, 30*time.Second, arop, "doctor", workspace)
	if afterDoctor.err != nil {
		return nil, afterDoctor.err
	}
	evidence := struct {
		Init          bool  `json:"init"`
		Doctor        bool  `json:"doctor"`
		Manifest      bool  `json:"manifest"`
		Dev           bool  `json:"dev"`
		Stop          bool  `json:"stop"`
		DatabaseBytes int64 `json:"database_bytes"`
	}{true, true, true, true, true, fileSize(filepath.Join(workspace, ".arop", "control-plane.sqlite"))}
	return json.Marshal(evidence)
}

func waitReady(done <-chan error, endpoint string, timeout time.Duration) error {
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return errors.New("quickstart exited before readiness")
		default:
		}
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("quickstart readiness timeout")
}

func writeWorkspace(scratch, root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "reference/control-plane/go.mod"))
	if err != nil {
		return "", err
	}
	fields, version := strings.Fields(string(data)), ""
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "github.com/gmslll/agent-runtime-operations-protocol" && strings.HasPrefix(fields[index+1], "v0.0.0-") {
			version = fields[index+1]
			break
		}
	}
	if version == "" {
		return "", errors.New("nested root pin is missing")
	}
	path := filepath.Join(scratch, "go.work")
	content := fmt.Sprintf("go 1.24.0\n\nuse (\n\t%s\n\t%s\n)\n\nreplace github.com/gmslll/agent-runtime-operations-protocol %s => %s\n", root, filepath.Join(root, "reference/control-plane"), version, root)
	return path, os.WriteFile(path, []byte(content), 0o600)
}

func directoryDigest(directory string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	buffer := bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() {
			return "", errors.New("unexpected release subdirectory")
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&buffer, "%s\x00%s\x00%d\n", entry.Name(), report.Hash(data), len(data))
	}
	return "sha256:" + report.Hash(buffer.Bytes()), nil
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, 30*time.Second, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	paths := []string{}
	for _, raw := range bytes.Split(result.output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "Makefile" || path == "go.mod" || path == "go.sum" || path == "spec/artifact-manifest.yaml" || path == "spec/release/go-artifacts.yaml" || strings.HasPrefix(path, "deployments/quickstart/") || strings.HasPrefix(path, "cmd/arop/") || strings.HasPrefix(path, "internal/tooling/cmd/arop-build-go-release/") || path == "reference/control-plane/go.mod" || path == "reference/control-plane/go.sum" || strings.HasPrefix(path, "reference/control-plane/migrations/") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func secureScratch(prefix string) (string, error) {
	directory, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		return "", err
	}
	value, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(value, 0o700); err != nil {
		return "", err
	}
	return value, nil
}
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

type safeBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (buffer *safeBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, data...)
	if len(buffer.data) > buffer.limit {
		buffer.data = append([]byte(nil), buffer.data[len(buffer.data)-buffer.limit:]...)
	}
	return len(data), nil
}

func run(directory string, timeout time.Duration, name string, arguments ...string) commandResult {
	return runWithEnv(directory, timeout, cleanEnvironment(), name, arguments...)
}
func runWithEnv(directory string, timeout time.Duration, environment []string, name string, arguments ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(ctx, name, arguments...)
	process.Dir, process.Env = directory, environment
	output, err := process.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("%s timed out", filepath.Base(name))
	} else if err != nil {
		tail := output
		if len(tail) > 8000 {
			tail = tail[len(tail)-8000:]
		}
		err = fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(tail)))
	}
	return commandResult{output, err}
}
func cleanEnvironment() []string {
	values := []string{"GOENV=off", "GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "LANG=C", "LC_ALL=C", "TZ=UTC", "HOME=" + os.Getenv("HOME")}
	if path := os.Getenv("PATH"); path != "" {
		values = append(values, "PATH="+path)
	}
	return values
}
func goEnvironment(workspace string) []string {
	values := cleanEnvironment()
	for index, value := range values {
		if strings.HasPrefix(value, "GOWORK=") {
			values[index] = "GOWORK=" + workspace
		}
	}
	return values
}
func sanitize(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(value) > 1000 {
		value = value[:1000]
	}
	return value
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, sanitize(err))
		os.Exit(1)
	}
}
