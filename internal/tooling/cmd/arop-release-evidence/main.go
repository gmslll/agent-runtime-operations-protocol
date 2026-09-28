// Command arop-release-evidence verifies external evidence without accepting a caller-supplied trust root.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

type summary struct {
	SchemaVersion      int    `json:"schema_version"`
	Kind               string `json:"kind"`
	SubjectCommit      string `json:"subject_commit"`
	SubjectTree        string `json:"subject_tree"`
	EnvelopeSHA256     string `json:"envelope_sha256"`
	PayloadSHA256      string `json:"payload_sha256"`
	RootSHA256         string `json:"root_sha256"`
	RegistrySHA256     string `json:"registry_sha256"`
	CheckpointVersion  int    `json:"checkpoint_version"`
	CheckpointSHA256   string `json:"checkpoint_sha256"`
	PrincipalID        string `json:"principal_id"`
	Role               string `json:"role"`
	VerificationMethod string `json:"verification_method"`
}

func main() {
	rootFlag := flag.String("root", ".", "repository root")
	kindFlag := flag.String("kind", "", "external_config or external_conformance")
	envelopeFlag := flag.String("envelope", "", "detached evidence envelope JSON")
	payloadFlag := flag.String("payload", "", "detached payload JSON")
	registryFlag := flag.String("registry", "", "untrusted TUF registry bundle JSON")
	outputFlag := flag.String("output", "", "verified summary output path")
	flag.Parse()
	if flag.NArg() != 0 || *envelopeFlag == "" || *payloadFlag == "" || *registryFlag == "" || *outputFlag == "" {
		fatal(errors.New("--kind, --envelope, --payload, --registry, and --output are required"))
	}
	root, err := structuredfile.FindRoot(*rootFlag)
	fatal(err)
	trustRoot := os.Getenv("TRUST_ROOT")
	if trustRoot == "" {
		fatal(errors.New("TRUST_ROOT protected configuration is required; no fallback is allowed"))
	}
	trustRoot, err = releaseevidence.ProtectedPathOutsideRepository(root, trustRoot)
	fatal(err)
	protected, _, err := releaseevidence.LoadProtectedState(trustRoot)
	fatal(err)
	bundle, _, err := releaseevidence.LoadRegistryBundle(root, *registryFlag)
	fatal(err)
	trust, err := releaseevidence.VerifyRegistry(bundle, protected, time.Now().UTC())
	fatal(err)
	envelope, _, err := releaseevidence.LoadEnvelope(root, *envelopeFlag)
	fatal(err)
	payload, err := os.ReadFile(*payloadFlag)
	fatal(err)
	expected, err := bindings(root, *kindFlag)
	fatal(err)
	verified, err := releaseevidence.VerifyEnvelope(envelope, payload, trust, expected, time.Now().UTC())
	fatal(err)
	switch *kindFlag {
	case "external_config":
		workflowLock, err := digestFile(filepath.Join(root, ".github/workflows/release.lock.json"))
		fatal(err)
		_, err = releaseevidence.ValidateExternalConfig(root, payload, workflowLock)
		fatal(err)
	case "external_conformance":
		_, err = releaseevidence.ValidateExternalConformance(root, payload, []string{"production"})
		fatal(err)
	default:
		fatal(errors.New("unsupported external evidence kind"))
	}
	tree, err := git(root, "rev-parse", envelope.Subject.Commit+"^{tree}")
	fatal(err)
	if tree != envelope.Subject.Tree {
		fatal(errors.New("detached evidence subject tree does not match Git commit"))
	}
	result := summary{SchemaVersion: 1, Kind: *kindFlag, SubjectCommit: envelope.Subject.Commit, SubjectTree: tree, EnvelopeSHA256: verified.EnvelopeSHA256, PayloadSHA256: verified.PayloadSHA256, RootSHA256: trust.RootSHA256, RegistrySHA256: trust.RegistrySHA256, CheckpointVersion: trust.CheckpointVersion, CheckpointSHA256: trust.CheckpointSHA256, PrincipalID: verified.PrincipalID, Role: verified.Role, VerificationMethod: verified.Method}
	fatal(writeJSON(*outputFlag, result))
	fmt.Printf("verified detached %s evidence for %s at checkpoint %d\n", *kindFlag, envelope.Subject.Commit, trust.CheckpointVersion)
}

func bindings(root, kind string) (releaseevidence.ExpectedBindings, error) {
	var schemaPath, validatorPath, policyPath, role string
	switch kind {
	case "external_config":
		schemaPath, validatorPath, policyPath, role = releaseevidence.ExternalConfigSchema, "internal/tooling/release/evidence/config.go", ".github/workflows/release.lock.json", "project_owner"
	case "external_conformance":
		schemaPath, validatorPath, policyPath, role = releaseevidence.ExternalConformanceSchema, "internal/tooling/release/evidence/conformance.go", "conformance/profiles/v1/production.yaml", "independent_reviewer"
	default:
		return releaseevidence.ExpectedBindings{}, errors.New("unsupported external evidence kind")
	}
	schemaDigest, err := digestFile(filepath.Join(root, filepath.FromSlash(schemaPath)))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	validatorDigest, err := digestFile(filepath.Join(root, filepath.FromSlash(validatorPath)))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	policyDigest, err := digestFile(filepath.Join(root, filepath.FromSlash(policyPath)))
	if err != nil {
		return releaseevidence.ExpectedBindings{}, err
	}
	return releaseevidence.ExpectedBindings{Kind: kind, Role: role, SchemaSHA256: schemaDigest, PolicySHA256: policyDigest, ValidatorSHA256: validatorDigest}, nil
}

func digestFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeJSON(path string, value any) error {
	abs, err := filepath.Abs(path)
	if err != nil || abs == string(filepath.Separator) {
		return errors.New("verified summary output path is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("verified summary output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(abs, append(data, '\n'), 0o600)
}

func git(root string, arguments ...string) (string, error) {
	command := exec.Command("git", arguments...)
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s failed", strings.Join(arguments, " "))
	}
	return strings.TrimSpace(string(output)), nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "arop-release-evidence:", err)
		os.Exit(1)
	}
}
