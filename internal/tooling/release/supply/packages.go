// Package supply orchestrates existing package/container primitives and durable publication.
package supply

import (
	"archive/zip"
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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/journal"
	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
)

const (
	EcosystemGo           = "go"
	EcosystemPython       = "python"
	EcosystemNPM          = "npm"
	EcosystemOCI          = "oci"
	EcosystemCLI          = "cli"
	EcosystemSchemaBundle = "schema_bundle"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type Artifact struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	LocalPath string `json:"-"`
}

type Primitive interface {
	ID() string
	Build(context.Context, versionpolicy.Versions) ([]Artifact, error)
}

type RemoteState struct {
	Exists bool
	Digest string
}

type Publisher interface {
	Reconcile(context.Context, string) (RemoteState, error)
	PutImmutable(context.Context, string, Artifact) (string, error)
	CompareAndSwapChannel(context.Context, string, string, string) (string, error)
}

type Signature struct {
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
}

type Signer interface {
	Sign(context.Context, []byte) (Signature, error)
}

type FaultHook interface{ Hit(string) error }

type Request struct {
	LogicalVersion        string
	SourceCommit          string
	SourceTree            string
	Destination           string
	ExpectedChannelDigest string
	WorkflowDigest        string
	WorkflowLockDigest    string
	WorkflowIdentity      string
}

type ArtifactRecord struct {
	Name        string    `json:"name"`
	Ecosystem   string    `json:"ecosystem"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	Bytes       int64     `json:"bytes"`
	Destination string    `json:"destination"`
	Signature   Signature `json:"signature"`
}

type Manifest struct {
	SchemaVersion       int              `json:"schema_version"`
	LogicalVersion      string           `json:"logical_version"`
	SourceCommit        string           `json:"source_commit"`
	SourceTree          string           `json:"source_tree"`
	VersionPolicyDigest string           `json:"version_policy_digest"`
	WorkflowDigest      string           `json:"workflow_digest"`
	WorkflowLockDigest  string           `json:"workflow_lock_digest"`
	WorkflowIdentity    string           `json:"workflow_identity"`
	Artifacts           []ArtifactRecord `json:"artifacts"`
}

type SBOM struct {
	SchemaVersion       int      `json:"schema_version"`
	Format              string   `json:"format"`
	LogicalVersion      string   `json:"logical_version"`
	VersionPolicyDigest string   `json:"version_policy_digest"`
	WorkflowDigest      string   `json:"workflow_digest"`
	WorkflowLockDigest  string   `json:"workflow_lock_digest"`
	WorkflowIdentity    string   `json:"workflow_identity"`
	ArtifactDigests     []string `json:"artifact_digests"`
}

type Provenance struct {
	SchemaVersion       int    `json:"schema_version"`
	PredicateType       string `json:"predicate_type"`
	SourceCommit        string `json:"source_commit"`
	SourceTree          string `json:"source_tree"`
	LogicalVersion      string `json:"logical_version"`
	VersionPolicyDigest string `json:"version_policy_digest"`
	WorkflowDigest      string `json:"workflow_digest"`
	WorkflowLockDigest  string `json:"workflow_lock_digest"`
	WorkflowIdentity    string `json:"workflow_identity"`
	ManifestDigest      string `json:"manifest_digest"`
}

type Result struct {
	Versions       versionpolicy.Versions `json:"versions"`
	Manifest       Manifest               `json:"manifest"`
	ManifestDigest string                 `json:"manifest_digest"`
	SBOM           SBOM                   `json:"sbom"`
	Provenance     Provenance             `json:"provenance"`
	ChannelDigest  string                 `json:"channel_digest"`
}

type Coordinator struct {
	Mapper     versionpolicy.Mapper
	Primitives []Primitive
	Publisher  Publisher
	Signer     Signer
	Journal    *journal.Store
	Faults     FaultHook
}

func (coordinator Coordinator) Run(ctx context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	if coordinator.Publisher == nil || coordinator.Signer == nil || coordinator.Journal == nil {
		return Result{}, errors.New("release dependencies are unavailable")
	}
	versions, err := coordinator.Mapper.Map(request.LogicalVersion)
	if err != nil {
		return Result{}, err
	}
	primitives, err := exactPrimitives(coordinator.Primitives)
	if err != nil {
		return Result{}, err
	}
	artifacts := []Artifact{}
	for _, id := range []string{"go", "python", "npm", "oci"} {
		built, buildErr := primitives[id].Build(ctx, versions)
		if buildErr != nil {
			return Result{}, fmt.Errorf("build %s primitive: %w", id, buildErr)
		}
		artifacts = append(artifacts, built...)
	}
	if err := validateArtifacts(artifacts, versions); err != nil {
		return Result{}, err
	}
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].Ecosystem != artifacts[j].Ecosystem {
			return artifacts[i].Ecosystem < artifacts[j].Ecosystem
		}
		return artifacts[i].Name < artifacts[j].Name
	})
	records := make([]ArtifactRecord, 0, len(artifacts))
	for _, artifact := range artifacts {
		destination := strings.TrimRight(request.Destination, "/") + "/" + artifact.Ecosystem + "/" + artifact.Version + "/" + artifact.Name
		key := journal.Key{SourceCommit: request.SourceCommit, SourceTree: request.SourceTree, LogicalVersion: request.LogicalVersion, VersionPolicyDigest: versions.PolicyDigest, Destination: destination, Operation: "publish-immutable", ArtifactDigest: artifact.SHA256}
		if _, err := coordinator.Journal.Append(releaseEntry(request, key, journal.StatusPrepared)); err != nil {
			return Result{}, err
		}
		if err := hit(coordinator.Faults, "before:"+artifact.Ecosystem); err != nil {
			return Result{}, err
		}
		remote, err := coordinator.Publisher.Reconcile(ctx, destination)
		if err != nil {
			return Result{}, fmt.Errorf("reconcile immutable artifact: %w", err)
		}
		remoteDigest := remote.Digest
		if remote.Exists {
			if remoteDigest != artifact.SHA256 {
				return Result{}, errors.New("immutable destination conflict")
			}
		} else {
			remoteDigest, err = coordinator.Publisher.PutImmutable(ctx, destination, artifact)
			if err != nil || remoteDigest != artifact.SHA256 {
				return Result{}, errors.New("immutable publication failed closed")
			}
		}
		if err := hit(coordinator.Faults, "after-remote:"+artifact.Ecosystem); err != nil {
			return Result{}, err
		}
		remoteEntry := releaseEntry(request, key, journal.StatusRemoteSuccess)
		remoteEntry.RemoteDigest = remoteDigest
		if _, err := coordinator.Journal.Append(remoteEntry); err != nil {
			return Result{}, err
		}
		statement := mustJSON(map[string]any{"artifact": artifact.SHA256, "destination": destination, "logical_version": request.LogicalVersion, "policy": versions.PolicyDigest, "source_commit": request.SourceCommit, "source_tree": request.SourceTree, "workflow": request.WorkflowDigest, "workflow_lock": request.WorkflowLockDigest, "workflow_identity": request.WorkflowIdentity})
		signature, err := coordinator.Signer.Sign(ctx, statement)
		if err != nil || signature.Identity == "" || !digestPattern.MatchString(signature.Digest) {
			return Result{}, errors.New("artifact signing failed closed")
		}
		committedEntry := releaseEntry(request, key, journal.StatusCommitted)
		committedEntry.RemoteDigest, committedEntry.ResultingDigest = remoteDigest, artifact.SHA256
		if _, err := coordinator.Journal.Append(committedEntry); err != nil {
			return Result{}, err
		}
		records = append(records, ArtifactRecord{Name: artifact.Name, Ecosystem: artifact.Ecosystem, Version: artifact.Version, SHA256: artifact.SHA256, Bytes: artifact.Bytes, Destination: destination, Signature: signature})
	}
	manifest := Manifest{SchemaVersion: 1, LogicalVersion: request.LogicalVersion, SourceCommit: request.SourceCommit, SourceTree: request.SourceTree, VersionPolicyDigest: versions.PolicyDigest, WorkflowDigest: request.WorkflowDigest, WorkflowLockDigest: request.WorkflowLockDigest, WorkflowIdentity: request.WorkflowIdentity, Artifacts: records}
	manifestDigest := digest(mustJSON(manifest))
	channelDestination := strings.TrimRight(request.Destination, "/") + "/channels/stable"
	channelKey := journal.Key{SourceCommit: request.SourceCommit, SourceTree: request.SourceTree, LogicalVersion: request.LogicalVersion, VersionPolicyDigest: versions.PolicyDigest, Destination: channelDestination, Operation: "update-channel", ArtifactDigest: manifestDigest}
	channelPrepared := releaseEntry(request, channelKey, journal.StatusPrepared)
	channelPrepared.ExpectedOldDigest = request.ExpectedChannelDigest
	if _, err := coordinator.Journal.Append(channelPrepared); err != nil {
		return Result{}, err
	}
	remoteChannel, err := coordinator.Publisher.Reconcile(ctx, channelDestination)
	if err != nil {
		return Result{}, err
	}
	if remoteChannel.Exists && remoteChannel.Digest == manifestDigest {
		// Remote success/local crash: reconcile before attempting CAS again.
	} else {
		if remoteChannel.Exists && remoteChannel.Digest != request.ExpectedChannelDigest {
			return Result{}, errors.New("mutable channel compare-and-swap conflict")
		}
		if err := hit(coordinator.Faults, "before:channel"); err != nil {
			return Result{}, err
		}
		result, casErr := coordinator.Publisher.CompareAndSwapChannel(ctx, channelDestination, request.ExpectedChannelDigest, manifestDigest)
		if casErr != nil || result != manifestDigest {
			return Result{}, errors.New("mutable channel update failed closed")
		}
	}
	if err := hit(coordinator.Faults, "after-remote:channel"); err != nil {
		return Result{}, err
	}
	channelRemote := releaseEntry(request, channelKey, journal.StatusRemoteSuccess)
	channelRemote.RemoteDigest, channelRemote.ExpectedOldDigest = manifestDigest, request.ExpectedChannelDigest
	if _, err := coordinator.Journal.Append(channelRemote); err != nil {
		return Result{}, err
	}
	channelCommitted := releaseEntry(request, channelKey, journal.StatusCommitted)
	channelCommitted.RemoteDigest, channelCommitted.ExpectedOldDigest, channelCommitted.ResultingDigest = manifestDigest, request.ExpectedChannelDigest, manifestDigest
	if _, err := coordinator.Journal.Append(channelCommitted); err != nil {
		return Result{}, err
	}
	digests := make([]string, 0, len(records))
	for _, record := range records {
		digests = append(digests, record.SHA256)
	}
	sort.Strings(digests)
	sbom := SBOM{SchemaVersion: 1, Format: "spdx-json-digest-inventory-v1", LogicalVersion: request.LogicalVersion, VersionPolicyDigest: versions.PolicyDigest, WorkflowDigest: request.WorkflowDigest, WorkflowLockDigest: request.WorkflowLockDigest, WorkflowIdentity: request.WorkflowIdentity, ArtifactDigests: digests}
	provenance := Provenance{SchemaVersion: 1, PredicateType: "https://slsa.dev/provenance/v1", SourceCommit: request.SourceCommit, SourceTree: request.SourceTree, LogicalVersion: request.LogicalVersion, VersionPolicyDigest: versions.PolicyDigest, WorkflowDigest: request.WorkflowDigest, WorkflowLockDigest: request.WorkflowLockDigest, WorkflowIdentity: request.WorkflowIdentity, ManifestDigest: manifestDigest}
	return Result{Versions: versions, Manifest: manifest, ManifestDigest: manifestDigest, SBOM: sbom, Provenance: provenance, ChannelDigest: manifestDigest}, nil
}

func exactPrimitives(values []Primitive) (map[string]Primitive, error) {
	result := map[string]Primitive{}
	for _, value := range values {
		if value == nil || value.ID() == "" || result[value.ID()] != nil {
			return nil, errors.New("release primitive inventory contains nil, empty, or duplicate ID")
		}
		result[value.ID()] = value
	}
	if len(result) != 4 {
		return nil, errors.New("release primitive inventory must contain exactly go/python/npm/oci")
	}
	for _, id := range []string{"go", "python", "npm", "oci"} {
		if result[id] == nil {
			return nil, fmt.Errorf("release primitive %s is missing", id)
		}
	}
	return result, nil
}

func validateRequest(request Request) error {
	if !commitPattern.MatchString(request.SourceCommit) || !commitPattern.MatchString(request.SourceTree) || !digestPattern.MatchString(request.WorkflowDigest) || !digestPattern.MatchString(request.WorkflowLockDigest) || request.WorkflowIdentity == "" || request.Destination == "" {
		return errors.New("release request identity is invalid")
	}
	if request.ExpectedChannelDigest != "" && !digestPattern.MatchString(request.ExpectedChannelDigest) {
		return errors.New("expected channel digest is invalid")
	}
	if strings.ContainsAny(request.Destination+request.WorkflowIdentity, "\r\n\x00") {
		return errors.New("release request contains unsafe text")
	}
	return nil
}

func releaseEntry(request Request, key journal.Key, status string) journal.Entry {
	return journal.Entry{
		Key: key, Status: status, WorkflowDigest: request.WorkflowDigest,
		WorkflowLockDigest: request.WorkflowLockDigest, WorkflowIdentity: request.WorkflowIdentity,
	}
}

func validateArtifacts(artifacts []Artifact, versions versionpolicy.Versions) error {
	if len(artifacts) < 6 {
		return errors.New("release primitive output does not cover all ecosystems")
	}
	wantVersion := map[string]string{EcosystemGo: versions.Go, EcosystemPython: versions.Python, EcosystemNPM: versions.NPM, EcosystemOCI: versions.OCI, EcosystemCLI: versions.CLI, EcosystemSchemaBundle: versions.SchemaBundle}
	seenName, seenEcosystem := map[string]bool{}, map[string]bool{}
	for _, artifact := range artifacts {
		if artifact.Name == "" || strings.ContainsAny(artifact.Name, "/\\\r\n\x00") || seenName[artifact.Name] || artifact.Bytes <= 0 || !digestPattern.MatchString(artifact.SHA256) || artifact.Version != wantVersion[artifact.Ecosystem] {
			return errors.New("release artifact inventory is invalid")
		}
		if artifact.LocalPath != "" {
			data, err := os.ReadFile(artifact.LocalPath)
			if err != nil || int64(len(data)) != artifact.Bytes || digest(data) != artifact.SHA256 {
				return errors.New("release artifact bytes do not match descriptor")
			}
		}
		seenName[artifact.Name], seenEcosystem[artifact.Ecosystem] = true, true
	}
	for ecosystem := range wantVersion {
		if !seenEcosystem[ecosystem] {
			return fmt.Errorf("release artifact ecosystem %s is missing", ecosystem)
		}
	}
	return nil
}

func hit(hook FaultHook, point string) error {
	if hook == nil {
		return nil
	}
	return hook.Hit(point)
}

// LocalPrimitives invokes only the existing P36/P27/P28/P37 build primitives.
func LocalPrimitives(root, output string) ([]Primitive, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return nil, err
	}
	return []Primitive{
		localPrimitive{"go", func(ctx context.Context, versions versionpolicy.Versions) ([]Artifact, error) {
			return buildGo(ctx, root, filepath.Join(output, "go"), versions)
		}},
		localPrimitive{"python", func(ctx context.Context, versions versionpolicy.Versions) ([]Artifact, error) {
			return buildPython(ctx, root, filepath.Join(output, "python"), versions)
		}},
		localPrimitive{"npm", func(ctx context.Context, versions versionpolicy.Versions) ([]Artifact, error) {
			return buildNPM(ctx, root, filepath.Join(output, "npm"), versions)
		}},
		localPrimitive{"oci", func(ctx context.Context, versions versionpolicy.Versions) ([]Artifact, error) {
			return buildOCIAndSchema(ctx, root, filepath.Join(output, "oci"), versions)
		}},
	}, nil
}

type localPrimitive struct {
	id    string
	build func(context.Context, versionpolicy.Versions) ([]Artifact, error)
}

func (primitive localPrimitive) ID() string { return primitive.id }
func (primitive localPrimitive) Build(ctx context.Context, versions versionpolicy.Versions) ([]Artifact, error) {
	return primitive.build(ctx, versions)
}

type primitiveManifest struct {
	Artifacts []struct {
		Name, Path, SHA256 string
		Bytes              int64
	} `json:"artifacts"`
}

func buildGo(ctx context.Context, root, output string, versions versionpolicy.Versions) ([]Artifact, error) {
	if err := prepareAbsentDirectory(output); err != nil {
		return nil, err
	}
	if err := run(ctx, root, nil, "go", "run", "./internal/tooling/cmd/arop-build-go-release", "--root", root, "--output", output, "--version", versions.Logical, "--goos", "linux", "--goarch", "amd64"); err != nil {
		return nil, err
	}
	var manifest primitiveManifest
	if err := decodeFile(filepath.Join(output, "checksums.json"), &manifest); err != nil {
		return nil, err
	}
	artifacts := []Artifact{}
	for _, item := range manifest.Artifacts {
		name := item.Path
		ecosystem, version := EcosystemGo, versions.Go
		if strings.HasPrefix(name, "arop-v") {
			ecosystem, version = EcosystemCLI, versions.CLI
		}
		artifacts = append(artifacts, Artifact{Name: name, Ecosystem: ecosystem, Version: version, SHA256: normalizeDigest(item.SHA256), Bytes: item.Bytes, LocalPath: filepath.Join(output, name)})
	}
	return artifacts, nil
}

func prepareAbsentDirectory(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("release primitive output path must not exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func buildPython(ctx context.Context, root, output string, versions versionpolicy.Versions) ([]Artifact, error) {
	if err := prepareDirectory(output); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(output, "manifest.json")
	if err := run(ctx, root, map[string]string{"PYTHONDONTWRITEBYTECODE": "1", "PYTHONHASHSEED": "0", "PIP_NO_INDEX": "1"}, "python3", "-B", "sdk/python/scripts/build_package.py", "--out-dir", output, "--manifest", manifestPath); err != nil {
		return nil, err
	}
	return artifactsFromManifest(manifestPath, output, EcosystemPython, versions.Python)
}

func buildNPM(ctx context.Context, root, output string, versions versionpolicy.Versions) ([]Artifact, error) {
	if err := prepareDirectory(output); err != nil {
		return nil, err
	}
	arguments := []string{"--permission", "--allow-fs-read=" + root, "--allow-fs-write=" + output, "--disable-proto=throw", "--no-addons", "sdk/typescript/scripts/build-package.mjs", "--root", root, "--out-dir", output}
	if err := run(ctx, root, nil, "node", arguments...); err != nil {
		return nil, err
	}
	return artifactsFromManifest(filepath.Join(output, "manifest.json"), output, EcosystemNPM, versions.NPM)
}

func buildOCIAndSchema(ctx context.Context, root, output string, versions versionpolicy.Versions) ([]Artifact, error) {
	if err := prepareDirectory(output); err != nil {
		return nil, err
	}
	image := filepath.Join(output, "arop-control-plane-"+versions.OCI+"-linux-amd64.oci.tar")
	if err := run(ctx, root, nil, "go", "run", "./internal/tooling/cmd/arop-build-container", "--root", root, "--output", image, "--goarch", "amd64"); err != nil {
		return nil, err
	}
	imageArtifact, err := fileArtifact(image, EcosystemOCI, versions.OCI)
	if err != nil {
		return nil, err
	}
	schemaPath := filepath.Join(output, "arop-schema-bundle-"+versions.SchemaBundle+".zip")
	if err := buildSchemaBundle(root, schemaPath); err != nil {
		return nil, err
	}
	schemaArtifact, err := fileArtifact(schemaPath, EcosystemSchemaBundle, versions.SchemaBundle)
	return []Artifact{imageArtifact, schemaArtifact}, err
}

func artifactsFromManifest(path, directory, ecosystem, version string) ([]Artifact, error) {
	var manifest primitiveManifest
	if err := decodeFile(path, &manifest); err != nil {
		return nil, err
	}
	result := make([]Artifact, 0, len(manifest.Artifacts))
	for _, item := range manifest.Artifacts {
		name := item.Name
		result = append(result, Artifact{Name: name, Ecosystem: ecosystem, Version: version, SHA256: normalizeDigest(item.SHA256), Bytes: item.Bytes, LocalPath: filepath.Join(directory, name)})
	}
	return result, nil
}

func buildSchemaBundle(root, destination string) error {
	entries, err := filepath.Glob(filepath.Join(root, "spec", "schemas", "*.json"))
	if err != nil || len(entries) == 0 {
		return errors.New("schema bundle inventory is empty")
	}
	sort.Strings(entries)
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(file)
	for _, path := range entries {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			_ = file.Close()
			return readErr
		}
		header := &zip.FileHeader{Name: "schemas/" + filepath.Base(path), Method: zip.Deflate}
		header.SetModTime(time.Unix(315532800, 0).UTC())
		header.SetMode(0o644)
		writer, createErr := archive.CreateHeader(header)
		if createErr != nil {
			_ = file.Close()
			return createErr
		}
		if _, writeErr := writer.Write(data); writeErr != nil {
			_ = file.Close()
			return writeErr
		}
	}
	if err := archive.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func prepareDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("release primitive output directory must be empty")
	}
	return nil
}

func fileArtifact(path, ecosystem, version string) (Artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Name: filepath.Base(path), Ecosystem: ecosystem, Version: version, SHA256: digest(data), Bytes: int64(len(data)), LocalPath: path}, nil
}

func decodeFile(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("primitive manifest has trailing JSON")
	}
	return nil
}

func run(ctx context.Context, directory string, additions map[string]string, name string, args ...string) error {
	process := exec.CommandContext(ctx, name, args...)
	process.Dir = directory
	environment := map[string]string{"GOENV": "off", "GOFLAGS": "-mod=readonly", "GOWORK": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0", "TZ": "UTC", "LANG": "C", "LC_ALL": "C"}
	for _, key := range []string{"HOME", "PATH", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			environment[key] = value
		}
	}
	for key, value := range additions {
		environment[key] = value
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		process.Env = append(process.Env, key+"="+environment[key])
	}
	output, err := process.CombinedOutput()
	if err != nil {
		if len(output) > 2000 {
			output = output[len(output)-2000:]
		}
		return fmt.Errorf("primitive %s failed: %w: %s", filepath.Base(name), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func normalizeDigest(value string) string {
	if strings.HasPrefix(value, "sha256:") {
		return value
	}
	return "sha256:" + value
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
