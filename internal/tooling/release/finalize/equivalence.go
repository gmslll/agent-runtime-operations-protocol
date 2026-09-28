package finalize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	releaseevidence "github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/release/lineage"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const AttestationSchema = "spec/schemas/final-equivalence-attestation.schema.json"

type BridgedReport struct {
	Phase              string `json:"phase"`
	ReportDigest       string `json:"report_digest"`
	ClaimedCommit      string `json:"claimed_commit"`
	InputClosureDigest string `json:"input_closure_digest"`
}

type Attestation struct {
	SchemaVersion       int             `json:"schema_version"`
	Source              Freeze          `json:"source"`
	ExpectedFinalTree   string          `json:"expected_final_tree"`
	LogicalVersion      string          `json:"logical_version"`
	OverlaySHA256       string          `json:"overlay_sha256"`
	VersionPolicySHA256 string          `json:"version_policy_sha256"`
	PayloadPolicy       int             `json:"payload_policy_version"`
	NormalizerVersion   int             `json:"normalizer_version"`
	PhasePolicySHA256   string          `json:"phase_policy_sha256"`
	IssuedAt            string          `json:"issued_at"`
	ExpiresAt           string          `json:"expires_at"`
	BridgedReports      []BridgedReport `json:"bridged_reports"`
}

func LoadAttestation(root, path string) (Attestation, []byte, error) {
	rel, err := structuredfile.SafeRelative(root, path)
	if err != nil {
		return Attestation{}, nil, err
	}
	abs, err := structuredfile.RequireInsideFile(root, root+string(os.PathSeparator)+rel, "attestation")
	if err != nil {
		return Attestation{}, nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Attestation{}, nil, err
	}
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return Attestation{}, nil, err
	}
	if err := schema.ValidateFile(root, AttestationSchema, parsed); err != nil {
		return Attestation{}, nil, err
	}
	normalized, _ := json.Marshal(parsed)
	var value Attestation
	decoder := json.NewDecoder(bytes.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Attestation{}, nil, err
	}
	return value, data, nil
}

func VerifyEquivalence(root string, attestation Attestation, payload []byte, envelope releaseevidence.Envelope, trust releaseevidence.VerifiedTrust, overlay Overlay, aggregate lineage.Aggregate, expected releaseevidence.ExpectedBindings, now time.Time) error {
	if err := VerifyAttestationBindings(attestation, overlay, aggregate, now); err != nil {
		return err
	}
	if expected.Kind != "final_equivalence" || expected.Role != "release_approver" {
		return errors.New("final equivalence requires release_approver policy")
	}
	if _, err := releaseevidence.VerifyEnvelope(envelope, payload, trust, expected, now); err != nil {
		return err
	}
	if envelope.Subject.Commit != attestation.Source.Commit || envelope.Subject.Tree != attestation.Source.Tree {
		return errors.New("envelope subject does not bind RC source")
	}
	return nil
}

func VerifyAttestationBindings(attestation Attestation, overlay Overlay, aggregate lineage.Aggregate, now time.Time) error {
	if attestation.Source != overlay.Source || attestation.LogicalVersion != overlay.Logical || attestation.OverlaySHA256 != overlay.SHA256 || attestation.ExpectedFinalTree == "" || attestation.PayloadPolicy != 1 || attestation.NormalizerVersion != 1 {
		return errors.New("attestation does not exactly bind source, overlay, version and policies")
	}
	issued, err := time.Parse(time.RFC3339, attestation.IssuedAt)
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339, attestation.ExpiresAt)
	if err != nil || !expires.After(issued) || now.Before(issued) || !now.Before(expires) {
		return errors.New("attestation validity window is invalid")
	}
	want := make([]BridgedReport, 0, len(aggregate.Reports))
	for _, item := range aggregate.Reports {
		want = append(want, BridgedReport{item.Phase, "sha256:" + item.ReportSHA256, item.ClaimedCommit, item.InputsSHA256})
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Phase < want[j].Phase })
	got := append([]BridgedReport{}, attestation.BridgedReports...)
	sort.Slice(got, func(i, j int) bool { return got[i].Phase < got[j].Phase })
	if !bytes.Equal(mustJSON(got), mustJSON(want)) {
		return fmt.Errorf("bridged report closure is not exact")
	}
	return nil
}

func VerifyFinalCommit(root, sourceCommit, finalCommit, expectedTree string) error {
	parents, err := git(root, "rev-list", "--parents", "-n", "1", finalCommit)
	if err != nil {
		return err
	}
	fields := bytes.Fields([]byte(parents))
	if len(fields) != 2 || string(fields[1]) != sourceCommit {
		return errors.New("final commit must be a direct single-parent child of RC source")
	}
	tree, err := git(root, "rev-parse", finalCommit+"^{tree}")
	if err != nil || tree != expectedTree {
		return errors.New("final commit tree does not match approved expected tree")
	}
	return nil
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }
