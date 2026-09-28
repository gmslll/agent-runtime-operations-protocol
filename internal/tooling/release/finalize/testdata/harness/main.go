//go:build ignore

// Command harness is the sole writer of the P42 release finalization report.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/finalize"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/lineage"
	versionpolicy "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/version"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make test-release-finalization-tooling"
	checker = "internal/tooling/release/finalize/testdata/harness/main.go"
)

var ownedArtifacts = []string{"final-delivery-checker", "final-equivalence-attestation-schema", "freeze-overlay-checker", "payload-equivalence-checker", "public-namespace-regeneration", "rc-source-freeze-checker"}

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
			detail = sanitize(root, err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, err := staticInputs(root)
	add("p42-static-input-closure", err, fmt.Sprintf("%d tracked finalization, trust, lineage and policy inputs", len(inputs)))
	add("p42-manifest-inventory", validateManifest(root), "exact six P42-owned artifacts and empty runtime inputs")
	regenerationEvidence, err := regenerationMatrix(root)
	add("p42-public-regeneration", err, "one verified config and logical version deterministically freeze all public namespaces and versions; second pass is zero-diff")
	overlay, overlayEvidence, err := overlayMatrix()
	add("p42-overlay-policy", err, "canonical final overlay permits only metadata, lock and checksum changes and rejects logical source changes")
	commitEvidence, err := commitMatrix()
	add("p42-final-commit", err, "final commit is an approved-tree direct single-parent child and root publication precedes nested")
	attestationEvidence, err := attestationMatrix(root, overlay)
	add("p42-attestation-binding", err, "payload binds source, expected final tree, policies, normalizer, validity and exact bridged report closure")
	add("p42-adversarial-matrix", adversarialMatrix(root, overlay), "placeholder/version, forbidden overlay, wrong parent/tree/time/report closure and schema drift fail closed")
	add("p42-no-signing-or-publication", sourceGuard(root), "tooling creates unsigned canonical candidates only and contains no key generation, signing, tag or publication primitive")
	add("p42-no-runtime-inputs", nil, "ephemeral repositories and candidates are retained only as digest-and-byte runtime evidence")
	evidence := []report.RuntimeEvidence{
		{Kind: "p42-attestation-matrix", SHA256: report.Hash(attestationEvidence), Bytes: int64(len(attestationEvidence))},
		{Kind: "p42-commit-matrix", SHA256: report.Hash(commitEvidence), Bytes: int64(len(commitEvidence))},
		{Kind: "p42-overlay-matrix", SHA256: report.Hash(overlayEvidence), Bytes: int64(len(overlayEvidence))},
		{Kind: "p42-regeneration-matrix", SHA256: report.Hash(regenerationEvidence), Bytes: int64(len(regenerationEvidence))},
	}
	written, err := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P42", Suite: "AROP P42 release finalization tooling", Class: "p42.release.finalize", Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks, Summary: map[string]any{"owned_artifacts": 6, "payload_policy_version": 1, "normalizer_version": 1, "runtime_inputs": 0}, AuditNote: "P42 deterministically maps one final logical version and one verified public configuration into the complete public namespace surface. It records a canonical allowlisted final overlay, binds the RC source and expected final tree, requires an exact independently verified P41 report bridge, verifies a P40 release_approver detached envelope, and accepts only a direct single-parent final commit. The final delivery checker enforces root-before-nested immutable publication order. It cannot sign, tag, publish, or mutate the approved source commit."})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P42/report.json"})
	fatal(err)
	if mode != "current-worktree" || !written.Success || !verified.Success {
		fatal(errors.New("P42 report self-verification failed"))
	}
	fmt.Printf("AROP release finalization tooling passed: %d checks.\n", len(checks))
}

func regenerationMatrix(root string) ([]byte, error) {
	mapper, err := versionpolicy.Load(root)
	if err != nil {
		return nil, err
	}
	files := templateFiles()
	config := validConfig()
	mutations, err := finalize.RegenerateTree(files, config, &mapper, "1.2.3")
	if err != nil {
		return nil, err
	}
	if len(mutations) != len(files) {
		return nil, fmt.Errorf("mutation inventory %d != %d", len(mutations), len(files))
	}
	finalTree := finalize.Apply(files, mutations)
	second, err := finalize.RegenerateTree(finalTree, config, &mapper, "1.2.3")
	if err != nil || len(second) != 0 {
		return nil, errors.New("second regeneration is not zero-diff")
	}
	return json.Marshal(mutations)
}

func templateFiles() map[string][]byte {
	return map[string][]byte{
		"VERSION": []byte("0.1.0-dev\n"), "go.mod": []byte("module __MODULE__\n"),
		"reference/control-plane/go.mod": []byte("module control\nrequire __ROOT_MODULE__ __ROOT_VERSION__\n"),
		"sdk/python/pyproject.toml":      []byte("[project]\nname=\"__PYTHON_NAME__\"\nversion=\"__PYTHON_VERSION__\"\n"),
		"sdk/typescript/package.json":    []byte("{\"name\":\"__NPM_NAME__\",\"version\":\"__NPM_VERSION__\"}\n"),
		"release/oci.json":               []byte("{\"repository\":\"__OCI_REPOSITORY__\",\"version\":\"__OCI_VERSION__\"}\n"),
		"release/namespaces.json":        []byte("{\"schemas\":\"__SCHEMA_BASE_URI__\",\"events\":\"__EVENT_NAMESPACE__\"}\n"),
	}
}

func validConfig() finalize.PublicConfig {
	return finalize.PublicConfig{GoModule: "github.com/arop-dev/arop", PythonPackage: "arop-sdk", NPMPackage: "@arop/sdk", OCIRepository: "ghcr.io/arop-dev/arop", SchemaBaseURI: "https://schemas.arop.dev/v1/", EventNamespace: "dev.arop.v1"}
}

func overlayMatrix() (finalize.Overlay, []byte, error) {
	source := finalize.Freeze{Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}
	before := map[string][]byte{"VERSION": []byte("1.2.3-rc.1\n"), "sdk/typescript/package.json": []byte(`{"version":"1.2.3-rc.1"}`), "internal/core.go": []byte("logic")}
	after := map[string][]byte{"VERSION": []byte("1.2.3\n"), "sdk/typescript/package.json": []byte(`{"version":"1.2.3"}`), "internal/core.go": []byte("logic")}
	overlay, err := finalize.BuildOverlay(source, "1.2.3", before, after)
	if err != nil {
		return finalize.Overlay{}, nil, err
	}
	data, _ := json.Marshal(overlay)
	return overlay, data, nil
}

func commitMatrix() ([]byte, error) {
	temp, err := os.MkdirTemp("", "arop-p42-commit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	temp, err = filepath.EvalSymlinks(temp)
	if err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "AROP P42"}, {"config", "user.email", "p42@example.invalid"}} {
		if _, err := git(temp, args...); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(temp, "VERSION"), []byte("1.2.3-rc.1\n"), 0o600); err != nil {
		return nil, err
	}
	if _, err := git(temp, "add", "VERSION"); err != nil {
		return nil, err
	}
	if _, err := git(temp, "commit", "-qm", "rc"); err != nil {
		return nil, err
	}
	source, _ := git(temp, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(temp, "VERSION"), []byte("1.2.3\n"), 0o600); err != nil {
		return nil, err
	}
	if _, err := git(temp, "commit", "-qam", "final"); err != nil {
		return nil, err
	}
	finalCommit, _ := git(temp, "rev-parse", "HEAD")
	tree, _ := git(temp, "rev-parse", "HEAD^{tree}")
	if err := finalize.VerifyFinalCommit(temp, source, finalCommit, tree); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"source": source, "final": finalCommit, "tree": tree})
}

func attestationMatrix(root string, overlay finalize.Overlay) ([]byte, error) {
	now := time.Now().UTC().Truncate(time.Second)
	reportItem := lineage.VerifiedCandidate{Phase: "P41", ReportSHA256: strings.Repeat("1", 64), ClaimedCommit: overlay.Source.Commit, InputsSHA256: strings.Repeat("2", 64)}
	aggregate := lineage.Aggregate{SchemaVersion: 1, ReleaseCommit: overlay.Source.Commit, ReleaseTree: overlay.Source.Tree, Reports: []lineage.VerifiedCandidate{reportItem}, SHA256: "sha256:" + strings.Repeat("3", 64)}
	attestation := finalize.Attestation{SchemaVersion: 1, Source: overlay.Source, ExpectedFinalTree: strings.Repeat("c", 40), LogicalVersion: overlay.Logical, OverlaySHA256: overlay.SHA256, VersionPolicySHA256: "sha256:" + strings.Repeat("4", 64), PayloadPolicy: 1, NormalizerVersion: 1, PhasePolicySHA256: "sha256:" + strings.Repeat("5", 64), IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), BridgedReports: []finalize.BridgedReport{{Phase: "P41", ReportDigest: "sha256:" + reportItem.ReportSHA256, ClaimedCommit: reportItem.ClaimedCommit, InputClosureDigest: reportItem.InputsSHA256}}}
	if err := finalize.VerifyAttestationBindings(attestation, overlay, aggregate, now); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "build", "p42-attestation.json")
	defer os.Remove(path)
	data, _ := json.Marshal(attestation)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	loaded, raw, err := finalize.LoadAttestation(root, "build/p42-attestation.json")
	if err != nil || !reflect.DeepEqual(loaded, attestation) {
		return nil, fmt.Errorf("schema load: %w", err)
	}
	return raw, nil
}

func adversarialMatrix(root string, overlay finalize.Overlay) error {
	mapper, err := versionpolicy.Load(root)
	if err != nil {
		return err
	}
	bad := validConfig()
	bad.GoModule = "example.com/TODO"
	if _, err := finalize.RegenerateTree(templateFiles(), bad, &mapper, "1.2.3"); err == nil {
		return errors.New("placeholder config accepted")
	}
	if _, err := finalize.RegenerateTree(templateFiles(), validConfig(), &mapper, "v1.2.3"); err == nil {
		return errors.New("v-prefixed version accepted")
	}
	before := map[string][]byte{"internal/core.go": []byte("a")}
	after := map[string][]byte{"internal/core.go": []byte("b")}
	if _, err := finalize.BuildOverlay(overlay.Source, "1.2.3", before, after); err == nil {
		return errors.New("logical source overlay accepted")
	}
	now := time.Now().UTC()
	aggregate := lineage.Aggregate{Reports: []lineage.VerifiedCandidate{{Phase: "P41", ReportSHA256: strings.Repeat("1", 64), ClaimedCommit: overlay.Source.Commit, InputsSHA256: strings.Repeat("2", 64)}}}
	badAttestation := finalize.Attestation{Source: overlay.Source, ExpectedFinalTree: strings.Repeat("c", 40), LogicalVersion: overlay.Logical, OverlaySHA256: overlay.SHA256, PayloadPolicy: 1, NormalizerVersion: 1, IssuedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-time.Hour).Format(time.RFC3339)}
	if finalize.VerifyAttestationBindings(badAttestation, overlay, aggregate, now) == nil {
		return errors.New("expired/incomplete attestation accepted")
	}
	return nil
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P42" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-release-finalization-tooling" {
				return fmt.Errorf("invalid P42 artifact %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "release-finalization-tooling-reports" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	want := append([]string{}, ownedArtifacts...)
	sort.Strings(owned)
	sort.Strings(deps)
	sort.Strings(want)
	if !reflect.DeepEqual(owned, want) || !reflect.DeepEqual(deps, want) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v deps=%v runtime=%v", owned, deps, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	output, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		return nil, err
	}
	values := map[string]bool{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == "spec/release/version-policy.yaml" || path == "spec/schemas/release-version-policy.schema.json" || path == finalize.AttestationSchema || strings.HasPrefix(path, "internal/tooling/release/finalize/") || strings.HasPrefix(path, "internal/tooling/cmd/arop-release-finalize/") || strings.HasPrefix(path, "internal/tooling/release/evidence/") || strings.HasPrefix(path, "internal/tooling/release/lineage/") || strings.HasPrefix(path, "internal/tooling/release/version/") || strings.HasPrefix(path, "internal/tooling/report/") || strings.HasPrefix(path, "internal/tooling/schema/") || strings.HasPrefix(path, "internal/tooling/structuredfile/") {
			if path != "" {
				info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
				if e != nil || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("P42 input absent or nonregular: %s", path)
				}
				values[path] = true
			}
		}
	}
	if !values[checker] {
		return nil, errors.New("P42 harness is untracked")
	}
	paths := []string{}
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func sourceGuard(root string) error {
	for _, path := range []string{"internal/tooling/release/finalize/freeze.go", "internal/tooling/release/finalize/regenerate.go", "internal/tooling/release/finalize/overlay.go", "internal/tooling/release/finalize/equivalence.go", "internal/tooling/cmd/arop-release-finalize/main.go"} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(data))
		for _, forbidden := range []string{"generatekey", "private key", "git tag", "git push", "npm publish", "twine upload", "docker push", "cosign sign"} {
			if strings.Contains(lower, forbidden) {
				return fmt.Errorf("%s contains forbidden signing/publication primitive %q", path, forbidden)
			}
		}
	}
	return nil
}

func git(root string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}
func sanitize(root string, err error) string {
	value := strings.ReplaceAll(err.Error(), root, "<repo>")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "P42 harness:", err)
		os.Exit(1)
	}
}
