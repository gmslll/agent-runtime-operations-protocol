//go:build ignore

// Command harness is the sole writer of the P40 release evidence tooling report.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/blueprint"
	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	command = "make test-release-evidence-tooling"
	checker = "internal/tooling/release/evidence/testdata/harness/main.go"
)

var ownedArtifacts = []string{"external-config-evidence-schema", "external-config-validator", "external-conformance-evidence-schema", "external-conformance-validator", "detached-release-evidence-envelope-schema", "detached-release-evidence-verifier", "trusted-release-role-registry-schema", "trusted-release-role-registry"}

type material struct {
	key     releaseevidence.Key
	private ed25519.PrivateKey
}

type fixture struct {
	now          time.Time
	bundle       releaseevidence.RegistryBundle
	protected    releaseevidence.ProtectedState
	trust        releaseevidence.VerifiedTrust
	keys         map[string]material
	caPEM        string
	leafPEM      string
	leafDER      []byte
	leafPrivate  ed25519.PrivateKey
	rekorPrivate ed25519.PrivateKey
	identity     releaseevidence.SigstoreIdentity
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
			detail = sanitize(root, err)
		}
		checks = append(checks, report.Check{Name: name, Passed: err == nil, Detail: detail})
	}
	inputs, inputErr := staticInputs(root)
	add("p40-static-input-closure", inputErr, fmt.Sprintf("%d tracked release-evidence, schema, report, and governance inputs", len(inputs)))
	add("p40-manifest-inventory", validateManifest(root), "exact eight P40-owned artifacts and empty runtime inputs")
	add("p40-identity-lockstep", validateIdentityLockstep(root), "schema consts and patterns bind the one canonical repository identity and release event")

	fx, fixtureErr := buildFixture(root)
	add("p40-tuf-chain", fixtureErr, "protected root verifies exact root-to-timestamp-to-snapshot-to-targets registry chain and monotonic checkpoint")
	var positiveEvidence, negativeEvidence, cliEvidence []byte
	if fixtureErr == nil {
		positiveEvidence, err = exercisePositive(root, fx)
		add("p40-detached-evidence-positive", err, "Ed25519 threshold and DSSE Sigstore/Fulcio/Rekor envelopes bind exact payload, subject, role, policy, validator, and trust checkpoint")
		negativeEvidence, err = exerciseNegatives(root, fx)
		add("p40-adversarial-matrix", err, "root substitution, rollback/freeze, same-principal threshold, expiry/revocation, role mixing, Sigstore chain/tlog/time/digest, and secret leakage fail closed")
		cliEvidence, err = exerciseCLI(root, fx)
		add("p40-protected-cli", err, "CLI accepts trust only from private out-of-repository TRUST_ROOT and writes a digest-only verified summary")
	}
	add("p40-no-approval-generation", validateNoApprovalGeneration(root), "validators contain no signing key, approval synthesis, insecure tlog bypass, or manual trust fallback")
	add("p40-no-runtime-inputs", nil, "test roots and keys are ephemeral; reports retain only digest-and-byte runtime evidence")

	evidence := []report.RuntimeEvidence{
		{Kind: "p40-cli-verification", SHA256: report.Hash(cliEvidence), Bytes: int64(len(cliEvidence))},
		{Kind: "p40-negative-matrix", SHA256: report.Hash(negativeEvidence), Bytes: int64(len(negativeEvidence))},
		{Kind: "p40-positive-envelopes", SHA256: report.Hash(positiveEvidence), Bytes: int64(len(positiveEvidence))},
	}
	written, err := report.Write(report.WriteOptions{
		Root: root, Directory: "build/reports/P40", Suite: "AROP P40 release evidence tooling", Class: "p40.release.evidence",
		Command: command, CheckerPath: checker, InputPaths: inputs, RuntimeInputPaths: []string{}, RuntimeEvidence: evidence, Checks: checks,
		Summary:   map[string]any{"owned_artifacts": 8, "tuf_roles": 4, "verification_methods": 2, "external_evidence_kinds": 2, "runtime_inputs": 0},
		AuditNote: "P40 bootstraps trust only from a private protected TRUST_ROOT whose root digest, root threshold, monotonic checkpoint, Fulcio roots, and Rekor keys are pinned outside the repository. It verifies the complete signed root-to-timestamp-to-snapshot-to-targets chain, freshness, exact role-registry target, distinct-principal thresholds, validity/revocation and release-approver separation. Detached envelopes bind repository object identity, subject commit/tree, schema/policy/validator, role, trust checkpoint and payload. Sigstore mode verifies DSSE, Fulcio chain and exact SAN identity, Rekor inclusion and signed checkpoint, and integrated time. Validators never create approvals. Test keys and raw evidence remain ephemeral; no key, certificate, payload, host path, or process output enters reports.",
	})
	fatal(err)
	verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P40/report.json"})
	fatal(err)
	if mode != "current-worktree" || verified.Success != written.Success {
		fatal(errors.New("P40 self-verification mismatch"))
	}
	if !written.Success {
		fatal(errors.New("P40 checks failed; see build/reports/P40/report.json"))
	}
	fmt.Printf("AROP release evidence tooling passed: %d checks.\n", len(checks))
}

func buildFixture(root string) (fixture, error) {
	fx := fixture{now: time.Now().UTC().Truncate(time.Second), keys: map[string]material{}}
	for _, item := range []struct{ id, principal string }{
		{"root-a", "root-a"}, {"root-b", "root-b"}, {"timestamp", "timestamp"}, {"snapshot", "snapshot"}, {"targets", "targets"},
		{"owner-a-1", "owner-a"}, {"owner-a-2", "owner-a"}, {"owner-b", "owner-b"}, {"reviewer", "reviewer-ci"}, {"approver", "approver"}, {"revoked", "revoked-reviewer"}, {"rekor-log", "rekor"},
	} {
		fx.keys[item.id] = newMaterial(item.id, item.principal)
	}
	subject := releaseevidence.RepositoryURI + "/.github/workflows/release.yml@refs/heads/main"
	caPEM, leafPEM, leafDER, leafPrivate, err := certificates(fx.now, "https://token.actions.githubusercontent.com", subject)
	if err != nil {
		return fx, err
	}
	fx.caPEM, fx.leafPEM, fx.leafDER, fx.leafPrivate = caPEM, leafPEM, leafDER, leafPrivate
	fx.rekorPrivate = fx.keys["rekor-log"].private
	workflowSHA := fmt.Sprintf("%x", sha256.Sum256([]byte("arop-p40-test-workflow-sha")))[:40]
	fx.identity = releaseevidence.SigstoreIdentity{PrincipalID: "reviewer-ci", Issuer: "https://token.actions.githubusercontent.com", Subject: subject, Repository: releaseevidence.Repository, WorkflowRef: releaseevidence.Repository + "/.github/workflows/release.yml@refs/heads/main", WorkflowSHA: workflowSHA, Event: "workflow_dispatch", Role: "independent_reviewer"}
	start, end := fx.now.Add(-time.Hour).Format(time.RFC3339), fx.now.Add(24*time.Hour).Format(time.RFC3339)
	registryKeys := []releaseevidence.Key{fx.keys["owner-a-1"].key, fx.keys["owner-a-2"].key, fx.keys["owner-b"].key, fx.keys["reviewer"].key, fx.keys["approver"].key, fx.keys["revoked"].key}
	registry := releaseevidence.RoleRegistry{SchemaVersion: 1, Version: 7, ExpiresAt: end, Keys: registryKeys, Principals: []releaseevidence.Principal{
		{PrincipalID: "approver", KeyIDs: []string{"approver"}, Roles: []string{"release_approver"}, NotBefore: start, NotAfter: end},
		{PrincipalID: "owner-a", KeyIDs: []string{"owner-a-1", "owner-a-2"}, Roles: []string{"project_owner"}, NotBefore: start, NotAfter: end},
		{PrincipalID: "owner-b", KeyIDs: []string{"owner-b"}, Roles: []string{"project_owner"}, NotBefore: start, NotAfter: end},
		{PrincipalID: "reviewer-ci", KeyIDs: []string{"reviewer"}, Roles: []string{"independent_reviewer"}, NotBefore: start, NotAfter: end},
		{PrincipalID: "revoked-reviewer", KeyIDs: []string{"revoked"}, Roles: []string{"independent_reviewer"}, NotBefore: start, NotAfter: end, Revoked: true},
	}, Thresholds: map[string]int{"independent_reviewer": 1, "project_owner": 2, "release_approver": 1}, SigstoreIdentities: []releaseevidence.SigstoreIdentity{fx.identity}}
	rootKeys := []releaseevidence.Key{fx.keys["root-a"].key, fx.keys["root-b"].key, fx.keys["timestamp"].key, fx.keys["snapshot"].key, fx.keys["targets"].key}
	rootMetadata := releaseevidence.RootMetadata{Type: "root", Version: 3, ExpiresAt: end, Keys: rootKeys, Roles: map[string]releaseevidence.Role{
		"root": {KeyIDs: []string{"root-a", "root-b"}, Threshold: 2}, "timestamp": {KeyIDs: []string{"timestamp"}, Threshold: 1}, "snapshot": {KeyIDs: []string{"snapshot"}, Threshold: 1}, "targets": {KeyIDs: []string{"targets"}, Threshold: 1},
	}}
	rootMetadata.Signatures = sign(rootMetadata, fx.keys, "root-a", "root-b")
	registryMeta := meta(registry)
	targets := releaseevidence.TargetsMetadata{Type: "targets", Version: 9, ExpiresAt: end, RoleRegistry: registryMeta, FulcioRootSHA256: releaseevidence.HashBytes([]byte(caPEM)), RekorKeyIDs: []string{"rekor-log"}}
	targets.Signatures = sign(targets, fx.keys, "targets")
	snapshot := releaseevidence.SnapshotMetadata{Type: "snapshot", Version: 11, ExpiresAt: end, Targets: meta(targets)}
	snapshot.Signatures = sign(snapshot, fx.keys, "snapshot")
	timestamp := releaseevidence.TimestampMetadata{Type: "timestamp", Version: 13, GeneratedAt: fx.now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: fx.now.Add(time.Hour).Format(time.RFC3339), Snapshot: meta(snapshot)}
	timestamp.Signatures = sign(timestamp, fx.keys, "timestamp")
	registryDigest, _, _ := releaseevidence.DigestValue(registry)
	checkpoint := releaseevidence.Checkpoint{Version: 17, RegistrySHA256: registryDigest, SignedAt: fx.now.Add(-30 * time.Second).Format(time.RFC3339)}
	checkpoint.Signatures = sign(checkpoint, fx.keys, "root-a", "root-b")
	fx.bundle = releaseevidence.RegistryBundle{SchemaVersion: 1, Root: rootMetadata, Timestamp: timestamp, Snapshot: snapshot, Targets: targets, RoleRegistry: registry, Checkpoint: checkpoint}
	rootDigest, _, _ := releaseevidence.DigestValue(rootMetadata)
	checkpointDigest, _, _ := releaseevidence.DigestValue(checkpoint)
	fx.protected = releaseevidence.ProtectedState{SchemaVersion: 1, RootVersion: rootMetadata.Version, RootSHA256: rootDigest, RootKeys: []releaseevidence.Key{fx.keys["root-a"].key, fx.keys["root-b"].key}, RootThreshold: 2, LastCheckpointVersion: checkpoint.Version, LastCheckpointSHA256: checkpointDigest, MaxTimestampAgeSeconds: 600, FulcioRootsPEM: []string{caPEM}, RekorKeys: map[string]releaseevidence.Key{"rekor-log": fx.keys["rekor-log"].key}}
	trust, err := releaseevidence.VerifyRegistry(fx.bundle, fx.protected, fx.now)
	if err != nil {
		return fx, err
	}
	fx.trust = trust
	return fx, nil
}

func exercisePositive(root string, fx fixture) ([]byte, error) {
	configPayload, configExpected, err := config(root)
	if err != nil {
		return nil, err
	}
	configEnvelope := baseEnvelope(root, fx, "external_config", "project_owner", "owner-a", configPayload, configExpected)
	statement, _ := releaseevidence.EnvelopeSigningStatement(configEnvelope)
	configEnvelope.Verification = releaseevidence.Verification{Method: "ed25519", Signatures: []releaseevidence.Signature{signature("owner-a-1", fx.keys["owner-a-1"].private, statement), signature("owner-b", fx.keys["owner-b"].private, statement)}}
	verifiedConfig, err := releaseevidence.VerifyEnvelope(configEnvelope, configPayload, fx.trust, configExpected, fx.now)
	if err != nil {
		return nil, err
	}
	if _, err := releaseevidence.ValidateExternalConfig(root, configPayload, configExpected.PolicySHA256); err != nil {
		return nil, err
	}
	conformancePayload, conformanceExpected, err := conformance(root)
	if err != nil {
		return nil, err
	}
	conformanceEnvelope := baseEnvelope(root, fx, "external_conformance", "independent_reviewer", "reviewer-ci", conformancePayload, conformanceExpected)
	statement, _ = releaseevidence.EnvelopeSigningStatement(conformanceEnvelope)
	proof, signatureValue, err := sigstoreVerification(fx, statement)
	if err != nil {
		return nil, err
	}
	conformanceEnvelope.Verification = releaseevidence.Verification{Method: "dsse-sigstore", PayloadType: "application/vnd.arop.release-evidence.v1+json", Signature: signatureValue, CertificateChainPEM: []string{fx.leafPEM}, Identity: &fx.identity, Rekor: &proof}
	verifiedConformance, err := releaseevidence.VerifyEnvelope(conformanceEnvelope, conformancePayload, fx.trust, conformanceExpected, fx.now)
	if err != nil {
		return nil, err
	}
	if _, err := releaseevidence.ValidateExternalConformance(root, conformancePayload, []string{"production"}); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("config=%s method=%s conformance=%s method=%s integrated=%d\n", verifiedConfig.EnvelopeSHA256, verifiedConfig.Method, verifiedConformance.EnvelopeSHA256, verifiedConformance.Method, verifiedConformance.IntegratedTime)), nil
}

func exerciseNegatives(root string, fx fixture) ([]byte, error) {
	configPayload, expected, err := config(root)
	if err != nil {
		return nil, err
	}
	base := baseEnvelope(root, fx, "external_config", "project_owner", "owner-a", configPayload, expected)
	statement, _ := releaseevidence.EnvelopeSigningStatement(base)
	base.Verification = releaseevidence.Verification{Method: "ed25519", Signatures: []releaseevidence.Signature{signature("owner-a-1", fx.keys["owner-a-1"].private, statement), signature("owner-b", fx.keys["owner-b"].private, statement)}}
	rejected := 0
	reject := func(name string, envelope releaseevidence.Envelope, payload []byte, trust releaseevidence.VerifiedTrust, binding releaseevidence.ExpectedBindings) error {
		if _, err := releaseevidence.VerifyEnvelope(envelope, payload, trust, binding, fx.now); err == nil {
			return fmt.Errorf("negative %s was accepted", name)
		}
		rejected++
		return nil
	}
	rootSwap := fx.protected
	rootSwap.RootSHA256 = releaseevidence.HashBytes([]byte("attacker-root"))
	if _, err := releaseevidence.VerifyRegistry(fx.bundle, rootSwap, fx.now); err == nil {
		return nil, errors.New("root substitution accepted")
	}
	rejected++
	rollback := fx.protected
	rollback.LastCheckpointVersion++
	if _, err := releaseevidence.VerifyRegistry(fx.bundle, rollback, fx.now); err == nil {
		return nil, errors.New("checkpoint rollback accepted")
	}
	rejected++
	frozen := fx.bundle
	frozen.Timestamp.GeneratedAt = fx.now.Add(-time.Hour).Format(time.RFC3339)
	frozen.Timestamp.Signatures = sign(frozen.Timestamp, fx.keys, "timestamp")
	if _, err := releaseevidence.VerifyRegistry(frozen, fx.protected, fx.now); err == nil {
		return nil, errors.New("frozen timestamp accepted")
	}
	rejected++
	if _, err := releaseevidence.VerifyRegistry(rechain(fx, fx.bundle.RoleRegistry), fx.protected, fx.now); err != nil {
		return nil, fmt.Errorf("re-signed registry baseline failed: %w", err)
	}
	pushIdentity := fx.bundle.RoleRegistry
	pushIdentity.SigstoreIdentities = []releaseevidence.SigstoreIdentity{fx.identity}
	pushIdentity.SigstoreIdentities[0].Event = "push"
	if _, err := releaseevidence.VerifyRegistry(rechain(fx, pushIdentity), fx.protected, fx.now); err == nil {
		return nil, errors.New("push-event Sigstore identity accepted")
	}
	rejected++
	placeholderIdentity := fx.bundle.RoleRegistry
	placeholderIdentity.SigstoreIdentities = []releaseevidence.SigstoreIdentity{fx.identity}
	placeholderIdentity.SigstoreIdentities[0].WorkflowSHA = strings.Repeat("d", 40)
	if _, err := releaseevidence.VerifyRegistry(rechain(fx, placeholderIdentity), fx.protected, fx.now); err == nil {
		return nil, errors.New("placeholder workflow_sha Sigstore identity accepted")
	}
	rejected++
	samePrincipal := base
	samePrincipal.Verification.Signatures = []releaseevidence.Signature{signature("owner-a-1", fx.keys["owner-a-1"].private, statement), signature("owner-a-2", fx.keys["owner-a-2"].private, statement)}
	if err := reject("same-principal-threshold", samePrincipal, configPayload, fx.trust, expected); err != nil {
		return nil, err
	}
	expired := base
	expired.ExpiresAt = fx.now.Add(-time.Second).Format(time.RFC3339)
	if err := reject("expired-envelope", expired, configPayload, fx.trust, expected); err != nil {
		return nil, err
	}
	revoked := base
	revoked.PrincipalID, revoked.Role = "revoked-reviewer", "independent_reviewer"
	if err := reject("revoked-principal", revoked, configPayload, fx.trust, releaseevidence.ExpectedBindings{Kind: "external_config", Role: "independent_reviewer", SchemaSHA256: expected.SchemaSHA256, PolicySHA256: expected.PolicySHA256, ValidatorSHA256: expected.ValidatorSHA256}); err != nil {
		return nil, err
	}
	digestMismatch := base
	digestMismatch.Payload.SHA256 = releaseevidence.HashBytes([]byte("wrong"))
	if err := reject("payload-digest", digestMismatch, configPayload, fx.trust, expected); err != nil {
		return nil, err
	}
	conformancePayload, conformanceExpected, _ := conformance(root)
	sigstoreEnvelope := baseEnvelope(root, fx, "external_conformance", "independent_reviewer", "reviewer-ci", conformancePayload, conformanceExpected)
	sigstoreStatement, _ := releaseevidence.EnvelopeSigningStatement(sigstoreEnvelope)
	proof, signatureValue, _ := sigstoreVerification(fx, sigstoreStatement)
	sigstoreEnvelope.Verification = releaseevidence.Verification{Method: "dsse-sigstore", PayloadType: "application/vnd.arop.release-evidence.v1+json", Signature: signatureValue, CertificateChainPEM: []string{fx.leafPEM}, Identity: &fx.identity, Rekor: &proof}
	for _, mutation := range []struct {
		name  string
		apply func(*releaseevidence.Envelope)
	}{
		{"sigstore-signature", func(e *releaseevidence.Envelope) {
			e.Verification.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		}},
		{"sigstore-identity", func(e *releaseevidence.Envelope) {
			copy := *e.Verification.Identity
			copy.Event = "push"
			e.Verification.Identity = &copy
		}},
		{"rekor-body", func(e *releaseevidence.Envelope) {
			copy := *e.Verification.Rekor
			copy.BodySHA256 = releaseevidence.HashBytes([]byte("wrong"))
			e.Verification.Rekor = &copy
		}},
		{"rekor-checkpoint", func(e *releaseevidence.Envelope) {
			copy := *e.Verification.Rekor
			copy.CheckpointSignature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
			e.Verification.Rekor = &copy
		}},
		{"integrated-time", func(e *releaseevidence.Envelope) {
			copy := *e.Verification.Rekor
			copy.IntegratedTime = fx.now.Add(-time.Hour).Unix()
			e.Verification.Rekor = &copy
		}},
		{"missing-chain", func(e *releaseevidence.Envelope) { e.Verification.CertificateChainPEM = nil }},
	} {
		candidate := sigstoreEnvelope
		mutation.apply(&candidate)
		if err := reject(mutation.name, candidate, conformancePayload, fx.trust, conformanceExpected); err != nil {
			return nil, err
		}
	}
	badConfig := bytes.Replace(configPayload, []byte("\"public_namespace\":\"arop.dev\""), []byte("\"public_namespace\":\"bearer secret\""), 1)
	if _, err := releaseevidence.ValidateExternalConfig(root, badConfig, expected.PolicySHA256); err == nil {
		return nil, errors.New("secret sentinel accepted")
	}
	rejected++
	return []byte(fmt.Sprintf("rejected=%d categories=root,rollback,freeze,identity,threshold,expiry,revocation,digest,sigstore,secret\n", rejected)), nil
}

// rechain re-signs the TUF chain over a mutated role registry so a rejection
// is attributable to registry content, not to a broken signature chain.
func rechain(fx fixture, registry releaseevidence.RoleRegistry) releaseevidence.RegistryBundle {
	bundle := fx.bundle
	bundle.RoleRegistry = registry
	bundle.Targets.RoleRegistry = meta(registry)
	bundle.Targets.Signatures = sign(bundle.Targets, fx.keys, "targets")
	bundle.Snapshot.Targets = meta(bundle.Targets)
	bundle.Snapshot.Signatures = sign(bundle.Snapshot, fx.keys, "snapshot")
	bundle.Timestamp.Snapshot = meta(bundle.Snapshot)
	bundle.Timestamp.Signatures = sign(bundle.Timestamp, fx.keys, "timestamp")
	registryDigest, _, err := releaseevidence.DigestValue(registry)
	if err != nil {
		panic(err)
	}
	bundle.Checkpoint = releaseevidence.Checkpoint{Version: fx.bundle.Checkpoint.Version + 1, RegistrySHA256: registryDigest, SignedAt: fx.now.Add(-20 * time.Second).Format(time.RFC3339)}
	bundle.Checkpoint.Signatures = sign(bundle.Checkpoint, fx.keys, "root-a", "root-b")
	return bundle
}

func exerciseCLI(root string, fx fixture) ([]byte, error) {
	scratch, err := secureTempDir("arop-p40-cli-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	payload, expected, err := config(root)
	if err != nil {
		return nil, err
	}
	envelope := baseEnvelope(root, fx, "external_config", "project_owner", "owner-a", payload, expected)
	statement, _ := releaseevidence.EnvelopeSigningStatement(envelope)
	envelope.Verification = releaseevidence.Verification{Method: "ed25519", Signatures: []releaseevidence.Signature{signature("owner-a-1", fx.keys["owner-a-1"].private, statement), signature("owner-b", fx.keys["owner-b"].private, statement)}}
	paths := map[string]any{"registry.json": fx.bundle, "protected.json": fx.protected, "envelope.json": envelope}
	for name, value := range paths {
		if err := writeJSON(filepath.Join(scratch, name), value, 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(scratch, "payload.json"), payload, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "run", "./internal/tooling/cmd/arop-release-evidence", "--root", root, "--kind", "external_config", "--envelope", filepath.Join(scratch, "envelope.json"), "--payload", filepath.Join(scratch, "payload.json"), "--registry", filepath.Join(scratch, "registry.json"), "--output", filepath.Join(scratch, "summary.json"))
	command.Dir = root
	command.Env = sanitizedEnvironment(append(os.Environ(), "TRUST_ROOT="+filepath.Join(scratch, "protected.json")))
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("release evidence CLI failed: %w: %s", err, tail(output))
	}
	summaryData, err := os.ReadFile(filepath.Join(scratch, "summary.json"))
	if err != nil {
		return nil, err
	}
	if bytes.Contains(summaryData, []byte("PRIVATE")) || bytes.Contains(summaryData, []byte(scratch)) {
		return nil, errors.New("verified summary leaked private material or path")
	}
	return []byte(fmt.Sprintf("summary=%s/%d stdout=%s/%d\n", releaseevidence.HashBytes(summaryData), len(summaryData), releaseevidence.HashBytes(output), len(output))), nil
}

func config(root string) ([]byte, releaseevidence.ExpectedBindings, error) {
	workflowLock, err := digestFile(filepath.Join(root, ".github/workflows/release.lock.json"))
	if err != nil {
		return nil, releaseevidence.ExpectedBindings{}, err
	}
	value := releaseevidence.ExternalConfig{SchemaVersion: 1, OIDCIssuer: "https://token.actions.githubusercontent.com", Repository: releaseevidence.Repository, WorkflowPath: ".github/workflows/release.yml", Environment: "arop-release", WorkflowLockSHA256: workflowLock, PublicNamespace: "arop.dev", Registries: map[string]string{"go": "https://proxy.golang.org", "pypi": "https://pypi.org", "npm": "https://registry.npmjs.org", "oci": "https://ghcr.io"}}
	data, _ := json.Marshal(value)
	expected, err := bindings(root, "external_config", "project_owner", releaseevidence.ExternalConfigSchema, "internal/tooling/release/evidence/config.go", ".github/workflows/release.lock.json")
	return data, expected, err
}

func conformance(root string) ([]byte, releaseevidence.ExpectedBindings, error) {
	value := releaseevidence.ExternalConformance{SchemaVersion: 1, RunnerSHA256: releaseevidence.HashBytes([]byte("runner")), SuiteSHA256: releaseevidence.HashBytes([]byte("suite")), Profiles: []string{"production"}, Artifacts: []releaseevidence.ExternalArtifact{{Name: "arop-cli.tar.gz", SHA256: releaseevidence.HashBytes([]byte("cli")), Bytes: 3}}, Results: []releaseevidence.ExternalResult{{ScenarioID: "server.production", Result: "PASS"}}}
	data, _ := json.Marshal(value)
	expected, err := bindings(root, "external_conformance", "independent_reviewer", releaseevidence.ExternalConformanceSchema, "internal/tooling/release/evidence/conformance.go", "conformance/profiles/v1/production.yaml")
	return data, expected, err
}

func bindings(root, kind, role, schemaPath, validatorPath, policyPath string) (releaseevidence.ExpectedBindings, error) {
	values := []string{}
	for _, path := range []string{schemaPath, validatorPath, policyPath} {
		value, err := digestFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return releaseevidence.ExpectedBindings{}, err
		}
		values = append(values, value)
	}
	return releaseevidence.ExpectedBindings{Kind: kind, Role: role, SchemaSHA256: values[0], ValidatorSHA256: values[1], PolicySHA256: values[2]}, nil
}

func baseEnvelope(root string, fx fixture, kind, role, principal string, payload []byte, expected releaseevidence.ExpectedBindings) releaseevidence.Envelope {
	head, _ := git(root, "rev-parse", "HEAD")
	tree, _ := git(root, "rev-parse", head+"^{tree}")
	return releaseevidence.Envelope{SchemaVersion: 1, RepositoryURI: releaseevidence.RepositoryURI, ObjectFormat: "git-sha1", Subject: releaseevidence.Subject{Commit: head, Tree: tree}, Kind: kind, SchemaSHA256: expected.SchemaSHA256, PolicySHA256: expected.PolicySHA256, ValidatorSHA256: expected.ValidatorSHA256, IssuedAt: fx.now.Add(-20 * time.Second).Format(time.RFC3339), ExpiresAt: fx.now.Add(time.Hour).Format(time.RFC3339), PrincipalID: principal, Role: role, Trust: releaseevidence.TrustBinding{RootSHA256: fx.trust.RootSHA256, RegistrySHA256: fx.trust.RegistrySHA256, CheckpointVersion: fx.trust.CheckpointVersion, CheckpointSHA256: fx.trust.CheckpointSHA256}, Payload: releaseevidence.PayloadBinding{MediaType: "application/json", SHA256: releaseevidence.HashBytes(payload), Bytes: int64(len(payload))}}
}

func sigstoreVerification(fx fixture, statement []byte) (releaseevidence.RekorProof, string, error) {
	payloadType := "application/vnd.arop.release-evidence.v1+json"
	signatureBytes := ed25519.Sign(fx.leafPrivate, releaseevidence.DSSEPAE(payloadType, statement))
	signatureValue := base64.StdEncoding.EncodeToString(signatureBytes)
	bodyDigest, err := releaseevidence.SigstoreBodyDigest(statement, signatureValue, fx.leafDER)
	if err != nil {
		return releaseevidence.RekorProof{}, "", err
	}
	rootHash, err := releaseevidence.RekorRoot(bodyDigest, 0, 1, nil)
	if err != nil {
		return releaseevidence.RekorProof{}, "", err
	}
	integrated := fx.now.Add(-10 * time.Second).Unix()
	checkpoint, _ := releaseevidence.Canonical(map[string]any{"log_id": "rekor-log", "tree_size": int64(1), "root_hash": rootHash, "integrated_time": integrated})
	checkpointSignature := base64.StdEncoding.EncodeToString(ed25519.Sign(fx.rekorPrivate, checkpoint))
	return releaseevidence.RekorProof{LogID: "rekor-log", LogIndex: 0, IntegratedTime: integrated, BodySHA256: bodyDigest, TreeSize: 1, RootHash: rootHash, Hashes: []string{}, CheckpointSignature: checkpointSignature}, signatureValue, nil
}

func newMaterial(id, principal string) material {
	seed := sha256.Sum256([]byte("arop-p40-test-key:" + id))
	private := ed25519.NewKeyFromSeed(seed[:])
	public := private.Public().(ed25519.PublicKey)
	return material{key: releaseevidence.Key{KeyID: id, PrincipalID: principal, Algorithm: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(public)}, private: private}
}

func sign(value any, keys map[string]material, ids ...string) []releaseevidence.Signature {
	statement, err := releaseevidence.SigningStatement(value)
	if err != nil {
		panic(err)
	}
	result := []releaseevidence.Signature{}
	for _, id := range ids {
		result = append(result, signature(id, keys[id].private, statement))
	}
	return result
}

func signature(id string, private ed25519.PrivateKey, statement []byte) releaseevidence.Signature {
	return releaseevidence.Signature{KeyID: id, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, statement))}
}
func meta(value any) releaseevidence.Meta {
	digest, data, err := releaseevidence.DigestValue(value)
	if err != nil {
		panic(err)
	}
	version := 0
	switch item := value.(type) {
	case releaseevidence.RoleRegistry:
		version = item.Version
	case releaseevidence.TargetsMetadata:
		version = item.Version
	case releaseevidence.SnapshotMetadata:
		version = item.Version
	}
	return releaseevidence.Meta{Version: version, SHA256: digest, Bytes: int64(len(data))}
}

func certificates(now time.Time, issuer, subject string) (string, string, []byte, ed25519.PrivateKey, error) {
	caSeed := sha256.Sum256([]byte("arop-p40-fulcio-ca"))
	caPrivate := ed25519.NewKeyFromSeed(caSeed[:])
	caTemplate := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: issuer}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, caPrivate.Public(), caPrivate)
	if err != nil {
		return "", "", nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", "", nil, nil, err
	}
	leafSeed := sha256.Sum256([]byte("arop-p40-fulcio-leaf"))
	leafPrivate := ed25519.NewKeyFromSeed(leafSeed[:])
	uri, err := url.Parse(subject)
	if err != nil {
		return "", "", nil, nil, err
	}
	leafTemplate := x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "AROP test workload"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, URIs: []*url.URL{uri}}
	leafDER, err := x509.CreateCertificate(rand.Reader, &leafTemplate, caCert, leafPrivate.Public(), caPrivate)
	if err != nil {
		return "", "", nil, nil, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})), leafDER, leafPrivate, nil
}

func validateManifest(root string) error {
	var manifest blueprint.Manifest
	if err := structuredfile.Load(filepath.Join(root, "spec/artifact-manifest.yaml"), &manifest); err != nil {
		return err
	}
	owned, deps, runtime := []string{}, []string{}, []string{}
	for _, artifact := range manifest.Artifacts {
		if artifact.OwnerPhase == "P40" {
			if artifact.PathRole != "concrete" || artifact.AcceptanceTest != "make-test-release-evidence-tooling" {
				return fmt.Errorf("invalid P40 artifact %s", artifact.ID)
			}
			owned = append(owned, artifact.ID)
		}
		if artifact.ID == "release-evidence-tooling-reports" {
			deps = append(deps, artifact.DerivesFrom...)
			runtime = append(runtime, artifact.RuntimeInputs...)
		}
	}
	want := append([]string(nil), ownedArtifacts...)
	sort.Strings(owned)
	sort.Strings(deps)
	sort.Strings(want)
	if !reflect.DeepEqual(owned, want) || !reflect.DeepEqual(deps, want) || len(runtime) != 0 {
		return fmt.Errorf("owned=%v deps=%v runtime=%v", owned, deps, runtime)
	}
	return nil
}

func staticInputs(root string) ([]string, error) {
	command := exec.Command("git", "ls-files", "-z")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	values := map[string]bool{}
	for _, raw := range bytes.Split(output, []byte{0}) {
		path := filepath.ToSlash(string(raw))
		if path == "Makefile" || path == "spec/artifact-manifest.yaml" || path == ".github/workflows/release.lock.json" || path == "conformance/profiles/v1/production.yaml" || strings.HasPrefix(path, "internal/tooling/release/evidence/") || strings.HasPrefix(path, "internal/tooling/cmd/arop-release-evidence/") || strings.HasPrefix(path, "internal/tooling/report/") || strings.HasPrefix(path, "internal/tooling/schema/") || strings.HasPrefix(path, "internal/tooling/structuredfile/") || path == releaseevidence.RoleRegistrySchema || path == releaseevidence.EnvelopeSchema || path == releaseevidence.ExternalConfigSchema || path == releaseevidence.ExternalConformanceSchema {
			if path != "" {
				info, e := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
				if e != nil || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("P40 input is absent or nonregular: %s", path)
				}
				values[path] = true
			}
		}
	}
	if !values[checker] {
		return nil, errors.New("P40 harness is untracked")
	}
	paths := []string{}
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// validateIdentityLockstep parses the trust schemas and asserts their identity
// consts and patterns equal the canonical Go constants, so a partial
// repository or event rename across Go and Schema surfaces fails this harness
// instead of surfacing at release time.
func validateIdentityLockstep(root string) error {
	roles, err := jsonDocument(root, releaseevidence.RoleRegistrySchema)
	if err != nil {
		return err
	}
	identity, err := mapPath(roles, "$defs", "sigstore_identity", "properties")
	if err != nil {
		return err
	}
	if schemaConst(identity, "repository") != releaseevidence.Repository || schemaConst(identity, "event") != releaseevidence.ReleaseWorkflowEvent {
		return errors.New("role registry schema identity consts drifted from canonical Go constants")
	}
	workflowRef, err := mapPath(identity, "workflow_ref")
	if err != nil {
		return err
	}
	if pattern, _ := workflowRef["pattern"].(string); !strings.HasPrefix(pattern, "^"+releaseevidence.Repository+"/") {
		return errors.New("role registry schema workflow_ref pattern is not anchored to the canonical repository")
	}
	envelope, err := jsonDocument(root, releaseevidence.EnvelopeSchema)
	if err != nil {
		return err
	}
	properties, err := mapPath(envelope, "properties")
	if err != nil {
		return err
	}
	envelopeIdentity, err := mapPath(envelope, "$defs", "identity", "properties")
	if err != nil {
		return err
	}
	if schemaConst(properties, "repository_uri") != releaseevidence.RepositoryURI || schemaConst(envelopeIdentity, "repository") != releaseevidence.Repository || schemaConst(envelopeIdentity, "event") != releaseevidence.ReleaseWorkflowEvent {
		return errors.New("envelope schema identity consts drifted from canonical Go constants")
	}
	config, err := jsonDocument(root, releaseevidence.ExternalConfigSchema)
	if err != nil {
		return err
	}
	configProperties, err := mapPath(config, "properties")
	if err != nil {
		return err
	}
	if schemaConst(configProperties, "repository") != releaseevidence.Repository {
		return errors.New("external config schema repository const drifted from canonical Go constant")
	}
	return nil
}

func jsonDocument(root, path string) (map[string]any, error) {
	value, _, err := structuredfile.LoadAny(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return nil, err
	}
	document, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON object", path)
	}
	return document, nil
}

func mapPath(document map[string]any, keys ...string) (map[string]any, error) {
	current := document
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema member %q is missing or not an object", key)
		}
		current = next
	}
	return current, nil
}

func schemaConst(properties map[string]any, field string) string {
	value, err := mapPath(properties, field)
	if err != nil {
		return ""
	}
	text, _ := value["const"].(string)
	return text
}

func validateNoApprovalGeneration(root string) error {
	paths := []string{"internal/tooling/release/evidence/config.go", "internal/tooling/release/evidence/conformance.go", "internal/tooling/release/evidence/trust.go", "internal/tooling/cmd/arop-release-evidence/main.go"}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		lower := bytes.ToLower(data)
		for _, forbidden := range []string{"insecure-ignore-tlog", "manual-trusted-channel", "generateapproval", "privatekeypem", "ed25519.generatekey"} {
			if bytes.Contains(lower, []byte(forbidden)) {
				return fmt.Errorf("validator %s contains forbidden approval/trust capability", path)
			}
		}
	}
	return nil
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return releaseevidence.HashBytes(data), nil
}
func writeJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), mode)
}
func secureTempDir(pattern string) (string, error) {
	path, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	return resolved, nil
}
func git(root string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
func sanitizedEnvironment(values []string) []string {
	blocked := map[string]bool{"TRUST_ROOT": true, "NODE_OPTIONS": true, "NODE_PATH": true, "NPM_CONFIG_NODE_OPTIONS": true, "GOFLAGS": true, "GOENV": true, "GOWORK": true, "GOTOOLCHAIN": true}
	result := []string{}
	for _, value := range values {
		key := strings.ToUpper(strings.SplitN(value, "=", 2)[0])
		if !blocked[key] {
			result = append(result, value)
		}
	}
	for index := len(values) - 1; index >= 0; index-- {
		if strings.HasPrefix(values[index], "TRUST_ROOT=") {
			result = append(result, values[index])
			break
		}
	}
	return append(result, "GOFLAGS=-mod=readonly", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0")
}
func tail(value []byte) string {
	if len(value) > 1000 {
		value = value[len(value)-1000:]
	}
	return strings.TrimSpace(string(value))
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
		fmt.Fprintln(os.Stderr, "P40 harness:", err)
		os.Exit(1)
	}
}
