//go:build ignore

// Command harness is the sole writer of the P37 production reference report.
package main

import (
	"archive/tar"
	"bytes"
	"context"
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

	"github.com/gmslll/agent-runtime-operations-protocol/cmd/arop-conformance/runner"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
	"go.yaml.in/yaml/v3"
)

const (
	command = "make production-reference-smoke"
	checker = "deployments/production-reference/testdata/harness/main.go"
)

type commandResult struct {
	output []byte
	err    error
}
type descriptor struct {
	MediaType, Digest string
	Size              int64
	Platform          map[string]string
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
	add("p37-static-input-closure", inputErr, fmt.Sprintf("%d tracked deployment, profile, container, migration, and dependency inputs", len(inputs)))
	add("p37-manifest-inventory", validateManifest(root), "exact four P37-owned artifacts and report closure")
	manifestEvidence, manifestErr := validateDeploymentManifests(root)
	add("p37-deployment-security", manifestErr, "nine production objects enforce immutable image inputs, restricted non-root pods, probes, rolling availability, resources, and default-deny networking")
	add("p37-upgrade-recovery-runbook", validateRunbook(root), "runbook requires verified backup restore, migration lock/readiness, rolling upgrade, and restore-based rollback")
	profileEvidence, profileErr := validateProductionProfile(root)
	add("p37-production-profile", profileErr, "production profile closes over three Control Plane and six required fault/HA scenarios")
	containerEvidence, containerErr := exerciseContainerBuild(root)
	add("p37-deterministic-container", containerErr, "amd64 and arm64 scratch OCI archives reproduce exactly and run as non-root with fixed metadata")
	dependencyEvidence, dependencyErr := exerciseDeploymentDependencies(root)
	add("p37-live-postgres-and-ha", dependencyErr, "fresh P34 and P35 suites pass real PostgreSQL 16 server, two-node failover, SQLite, profile, and fault matrices")
	add("p37-no-runtime-inputs", nil, "deployment templates are tracked and external credentials/images remain protected runtime substitutions")
	evidence := []report.RuntimeEvidence{
		{Kind: "p37-container-build", SHA256: report.Hash(containerEvidence), Bytes: int64(len(containerEvidence))},
		{Kind: "p37-dependency-suites", SHA256: report.Hash(dependencyEvidence), Bytes: int64(len(dependencyEvidence))},
		{Kind: "p37-manifests", SHA256: report.Hash(manifestEvidence), Bytes: int64(len(manifestEvidence))},
		{Kind: "p37-production-profile", SHA256: report.Hash(profileEvidence), Bytes: int64(len(profileEvidence))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P37", Suite: "AROP P37 PostgreSQL production reference", Class: "p37.deployment.production", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 4, "deployment_objects": 9, "container_architectures": 2, "production_scenarios": 9, "runtime_inputs": 0}, AuditNote: "P37 supplies a reference PostgreSQL deployment with immutable render-time image digests, Secret-manager placeholders, restricted non-root workloads, exact probes, rolling availability, resources, and default-deny networking. Its deterministic scratch OCI primitive contains only the trimpath aropd binary, tracked migrations, and fixed metadata. Fresh P34/P35 executions prove the same protocol profile against durable SQLite/PostgreSQL and deterministic multi-node failover. The runbook requires backup restore verification before upgrade and restore-based rollback after forward-only migrations. No credential, DSN, image secret, path, database content, or process log enters this report."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P37/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P37 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P37 checks failed; see build/reports/P37/report.json"))
	}
	fmt.Printf("AROP production reference passed: %d checks.\n", len(checks))
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtimeInputs := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P37" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-production-reference-smoke" {
				return fmt.Errorf("invalid P37 artifact %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "phase-report-p37" {
			deps = append(deps, artifact.DerivesFrom...)
			runtimeInputs = append(runtimeInputs, artifact.RuntimeInputs...)
		}
	}
	wantOwned := []string{"container-build-primitive", "production-reference-manifests", "production-upgrade-recovery-runbook", "production-conformance-profile"}
	wantDeps := []string{"production-reference-manifests", "production-upgrade-recovery-runbook", "production-conformance-profile", "container-build-primitive", "conformance-profiles"}
	if !reflect.DeepEqual(owned, wantOwned) || !reflect.DeepEqual(deps, wantDeps) || len(runtimeInputs) != 0 {
		return fmt.Errorf("owned=%v deps=%v runtime=%v", owned, deps, runtimeInputs)
	}
	return nil
}

func validateDeploymentManifests(root string) ([]byte, error) {
	directory := filepath.Join(root, "deployments/production-reference/manifests")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	wantFiles := []string{"control-plane.yaml", "namespace.yaml", "network-policy.yaml", "postgres.yaml", "secret-template.yaml"}
	gotFiles, kinds, evidence := []string{}, []string{}, bytes.Buffer{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			return nil, fmt.Errorf("unexpected manifest entry %s", entry.Name())
		}
		gotFiles = append(gotFiles, entry.Name())
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var document map[string]any
			err := decoder.Decode(&document)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			kind, _ := document["kind"].(string)
			if kind == "" {
				return nil, fmt.Errorf("manifest %s has no kind", entry.Name())
			}
			kinds = append(kinds, kind)
		}
		fmt.Fprintf(&evidence, "%s sha256:%s %d\n", entry.Name(), report.Hash(data), len(data))
		lower := strings.ToLower(string(data))
		if strings.Contains(lower, "latest") || strings.Contains(lower, "privileged: true") || strings.Contains(lower, "hostnetwork: true") {
			return nil, fmt.Errorf("unsafe production manifest %s", entry.Name())
		}
	}
	sort.Strings(gotFiles)
	sort.Strings(wantFiles)
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		return nil, fmt.Errorf("manifest files=%v", gotFiles)
	}
	sort.Strings(kinds)
	wantKinds := []string{"Deployment", "Namespace", "NetworkPolicy", "NetworkPolicy", "PodDisruptionBudget", "Secret", "Service", "Service", "StatefulSet"}
	sort.Strings(wantKinds)
	if !reflect.DeepEqual(kinds, wantKinds) {
		return nil, fmt.Errorf("manifest kinds=%v", kinds)
	}
	control, _ := os.ReadFile(filepath.Join(directory, "control-plane.yaml"))
	postgres, _ := os.ReadFile(filepath.Join(directory, "postgres.yaml"))
	network, _ := os.ReadFile(filepath.Join(directory, "network-policy.yaml"))
	for _, required := range []string{"@${AROP_CONTROL_PLANE_IMAGE_DIGEST}", "runAsNonRoot: true", "runAsUser: 65532", "readOnlyRootFilesystem: true", "allowPrivilegeEscalation: false", "drop: [ALL]", "startupProbe:", "readinessProbe:", "livenessProbe:", "maxUnavailable: 0", "maxSurge: 1", "minAvailable: 1"} {
		if !bytes.Contains(control, []byte(required)) {
			return nil, fmt.Errorf("control-plane manifest lacks %q", required)
		}
	}
	for _, required := range []string{"postgres@${POSTGRES_16_IMAGE_DIGEST}", "secretKeyRef:", "readinessProbe:", "livenessProbe:", "ReadWriteOnce"} {
		if !bytes.Contains(postgres, []byte(required)) {
			return nil, fmt.Errorf("postgres manifest lacks %q", required)
		}
	}
	if !bytes.Contains(network, []byte("arop-default-deny")) || !bytes.Contains(network, []byte("policyTypes: [Ingress, Egress]")) {
		return nil, errors.New("network policy is not default deny")
	}
	return evidence.Bytes(), nil
}

func validateRunbook(root string) error {
	data, err := os.ReadFile(filepath.Join(root, "deployments/production-reference/RUNBOOK.md"))
	if err != nil {
		return err
	}
	for _, required := range []string{"pg_dump", "restore", "migration lock", "maxUnavailable", "Production profile", "Scale the Control Plane to zero", "Never fall back to memory"} {
		if !bytes.Contains(bytes.ToLower(data), bytes.ToLower([]byte(required))) {
			return fmt.Errorf("runbook lacks %q", required)
		}
	}
	return nil
}

func validateProductionProfile(root string) ([]byte, error) {
	catalog, err := runner.LoadCatalog(root)
	if err != nil {
		return nil, err
	}
	resolved, digest, err := catalog.Resolve("production", nil)
	if err != nil {
		return nil, err
	}
	if len(resolved) != 9 || digest == "" {
		return nil, fmt.Errorf("production scenario closure=%d", len(resolved))
	}
	data, err := os.ReadFile(filepath.Join(root, "conformance/profiles/v1/production.yaml"))
	if err != nil {
		return nil, err
	}
	return append([]byte(digest+"\n"), data...), nil
}

func exerciseContainerBuild(root string) ([]byte, error) {
	scratch, err := secureScratch()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	evidence := bytes.Buffer{}
	for _, arch := range []string{"amd64", "arm64"} {
		first, second := filepath.Join(scratch, arch+"-a.tar"), filepath.Join(scratch, arch+"-b.tar")
		for _, output := range []string{first, second} {
			result := run(root, 5*time.Minute, "go", "run", "./internal/tooling/cmd/arop-build-container", "--root", root, "--output", output, "--goarch", arch)
			if result.err != nil {
				return nil, result.err
			}
		}
		left, err := os.ReadFile(first)
		if err != nil {
			return nil, err
		}
		right, err := os.ReadFile(second)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(left, right) {
			return nil, fmt.Errorf("OCI archive for %s is not deterministic", arch)
		}
		manifestDigest, err := inspectOCI(left, arch)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&evidence, "linux/%s %s sha256:%s %d\n", arch, manifestDigest, report.Hash(left), len(left))
	}
	return evidence.Bytes(), nil
}

func inspectOCI(data []byte, arch string) (string, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	files := map[string][]byte{}
	last := ""
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if header.Name <= last || !header.ModTime.Equal(time.Unix(315532800, 0).UTC()) || header.Mode != 0o644 {
			return "", fmt.Errorf("non-deterministic OCI entry %s", header.Name)
		}
		content, err := io.ReadAll(io.LimitReader(reader, 128<<20))
		if err != nil {
			return "", err
		}
		files[header.Name] = content
		last = header.Name
	}
	if string(files["oci-layout"]) != "{\"imageLayoutVersion\":\"1.0.0\"}\n" {
		return "", errors.New("invalid OCI layout")
	}
	var index struct {
		SchemaVersion int          `json:"schemaVersion"`
		Manifests     []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(files["index.json"], &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 || index.Manifests[0].Platform["architecture"] != arch {
		return "", errors.New("invalid OCI index")
	}
	manifestDescriptor := index.Manifests[0]
	manifestData := files["blobs/sha256/"+strings.TrimPrefix(manifestDescriptor.Digest, "sha256:")]
	if digest(manifestData) != manifestDescriptor.Digest || int64(len(manifestData)) != manifestDescriptor.Size {
		return "", errors.New("OCI manifest digest mismatch")
	}
	var manifest struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil || len(manifest.Layers) != 1 {
		return "", errors.New("invalid OCI manifest")
	}
	configData := files["blobs/sha256/"+strings.TrimPrefix(manifest.Config.Digest, "sha256:")]
	if digest(configData) != manifest.Config.Digest {
		return "", errors.New("OCI config digest mismatch")
	}
	var config struct {
		Architecture, OS string
		Config           struct {
			User       string
			Entrypoint []string
			WorkingDir string
		}
	}
	if err := json.Unmarshal(configData, &config); err != nil || config.Architecture != arch || config.OS != "linux" || config.Config.User != "65532:65532" || !reflect.DeepEqual(config.Config.Entrypoint, []string{"/usr/local/bin/aropd"}) || config.Config.WorkingDir != "/" {
		return "", errors.New("OCI non-root config drifted")
	}
	layerData := files["blobs/sha256/"+strings.TrimPrefix(manifest.Layers[0].Digest, "sha256:")]
	if digest(layerData) != manifest.Layers[0].Digest {
		return "", errors.New("OCI layer digest mismatch")
	}
	layer := tar.NewReader(bytes.NewReader(layerData))
	lastLayer, binarySeen, migrationCount := "", false, 0
	for {
		header, err := layer.Next()
		if err == io.EOF {
			break
		}
		if err != nil || header.Name <= lastLayer || !header.ModTime.Equal(time.Unix(315532800, 0).UTC()) {
			return "", errors.New("OCI layer ordering or mtime drifted")
		}
		if header.Name == "usr/local/bin/aropd" {
			if header.Mode != 0o755 || header.Size <= 0 {
				return "", errors.New("OCI binary metadata drifted")
			}
			binarySeen = true
		} else if strings.HasPrefix(header.Name, "opt/arop/migrations/") {
			if header.Mode != 0o644 || header.Size <= 0 {
				return "", errors.New("OCI migration metadata drifted")
			}
			migrationCount++
		} else {
			return "", fmt.Errorf("unexpected OCI layer entry %s", header.Name)
		}
		lastLayer = header.Name
	}
	if !binarySeen || migrationCount == 0 {
		return "", errors.New("OCI layer lacks binary or migration closure")
	}
	return manifestDescriptor.Digest, nil
}

func exerciseDeploymentDependencies(root string) ([]byte, error) {
	evidence := bytes.Buffer{}
	for _, target := range []struct{ name, reportPath string }{{"test-server-conformance", "build/reports/P34/report.json"}, {"test-fault-ha-drivers", "build/reports/P35/report.json"}} {
		result := run(root, 8*time.Minute, "make", target.name)
		if result.err != nil {
			return nil, result.err
		}
		verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: target.reportPath})
		if err != nil || mode != "current-worktree" || !verified.Success {
			return nil, fmt.Errorf("dependency %s did not verify current: %v", target.name, err)
		}
		data, err := os.ReadFile(filepath.Join(root, target.reportPath))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&evidence, "%s sha256:%s %d\n", target.name, report.Hash(data), len(data))
	}
	return evidence.Bytes(), nil
}

func trackedInputs(root string) ([]string, error) {
	result := run(root, 30*time.Second, "git", "ls-files", "-z")
	if result.err != nil {
		return nil, result.err
	}
	paths := []string{}
	for _, raw := range bytes.Split(result.output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == "go.mod" || path == "go.sum" || strings.HasPrefix(path, "deployments/production-reference/") || path == "conformance/profiles/v1/production.yaml" || path == "conformance/profiles/schema.json" || strings.HasPrefix(path, "conformance/scenarios/") || path == "internal/tooling/cmd/arop-build-container/main.go" || path == "reference/control-plane/go.mod" || path == "reference/control-plane/go.sum" || strings.HasPrefix(path, "reference/control-plane/migrations/") || strings.HasPrefix(path, "reference/control-plane/tests/conformance/") || strings.HasPrefix(path, "conformance/fault-injection/") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func secureScratch() (string, error) {
	directory, err := os.MkdirTemp("/tmp", "arop-p37-")
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
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func run(directory string, timeout time.Duration, name string, arguments ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	process := exec.CommandContext(ctx, name, arguments...)
	process.Dir = directory
	process.Env = cleanEnvironment()
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
