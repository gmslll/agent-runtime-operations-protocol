// Package evidence validates planning and Gate evidence without a Node runtime.
package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/controlledinput"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

var PlanningInputs = []struct{ Path, Field string }{{"spec/requirements.yaml", "requirements_sha256"}, {"docs/DEVELOPMENT_PLAN.md", "plan_sha256"}, {"docs/IMPLEMENTATION_BLUEPRINT.md", "blueprint_sha256"}, {"spec/artifact-manifest.yaml", "artifact_manifest_sha256"}}
var SummaryFields = map[string][]string{"arop-planning-audit": {"schema_version", "kind", "subject", "reviewer", "result", "attested_at"}, "arop-user-gate": {"schema_version", "kind", "gate", "subject", "approver", "result", "attested_at"}}

const P03Command = "make planning-audit EVIDENCE=<external>"

var P03RequiredChecks = []string{
	"external-evidence-location",
	"external-evidence-readable",
	"planning-audit-evidence-schema",
	"subject-commit-lineage",
	"audit-subject-binding",
	"complete-audit-results",
	"canonical-summary-content-binding",
	"reviewer-authenticity-gate",
	"canonical-summary-candidate-schema",
	"canonical-summary-component-digests",
	"report-provenance",
}

var P03StaticInputs = []string{
	"go.mod",
	"go.sum",
	"spec/requirements.yaml",
	"docs/DEVELOPMENT_PLAN.md",
	"docs/IMPLEMENTATION_BLUEPRINT.md",
	"spec/artifact-manifest.yaml",
	"spec/schemas/planning-audit-evidence.schema.json",
	"spec/schemas/canonical-evidence-summary.schema.json",
	"spec/schemas/trusted-key-registry.schema.json",
	"spec/schemas/check-report.schema.json",
	"internal/tooling/cmd/arop-planning-audit/main.go",
	"internal/tooling/controlledinput/manifest.go",
	"internal/tooling/evidence/validation.go",
	"internal/tooling/report/writer.go",
	"internal/tooling/report/verifier.go",
	"internal/tooling/schema/validator.go",
	"internal/tooling/structuredfile/files.go",
}

var P03RuntimeEvidenceKinds = []string{
	"canonical-summary-candidate",
	"external-authentication-material",
	"external-planning-audit-evidence",
}

func hash(v []byte) string { s := sha256.Sum256(v); return hex.EncodeToString(s[:]) }
func Hash(v []byte) string { return hash(v) }
func git(root string, args ...string) ([]byte, error) {
	c := controlledinput.GitCommand(root, args...)
	return c.Output()
}
func gitText(root string, args ...string) (string, error) {
	b, e := git(root, args...)
	return strings.TrimSpace(string(b)), e
}
func gitBlob(root, commit, path string) ([]byte, error) {
	b, e := git(root, "show", commit+":"+path)
	if e != nil {
		return nil, fmt.Errorf("planning input %s is absent at evidence subject commit %s", path, commit)
	}
	return b, nil
}

type SubjectLineage struct {
	SubjectCommit, CurrentHead, Mode string
	Digests                          map[string]string
}

func VerifySubjectCommit(root, commit string, subject map[string]any) (SubjectLineage, error) {
	if len(commit) != 40 {
		return SubjectLineage{}, fmt.Errorf("evidence subject commit is invalid: %s", commit)
	}
	head, e := gitText(root, "rev-parse", "HEAD")
	if e != nil {
		return SubjectLineage{}, e
	}
	if _, e = gitText(root, "cat-file", "-e", commit+"^{commit}"); e != nil {
		return SubjectLineage{}, fmt.Errorf("evidence subject commit does not exist: %s", commit)
	}
	if commit != head {
		c := controlledinput.GitCommand(root, "merge-base", "--is-ancestor", commit, head)
		if c.Run() != nil {
			return SubjectLineage{}, fmt.Errorf("evidence subject commit %s is not an ancestor of current HEAD %s", commit, head)
		}
	}
	digests := map[string]string{}
	for _, item := range PlanningInputs {
		blob, e := gitBlob(root, commit, item.Path)
		if e != nil {
			return SubjectLineage{}, e
		}
		digest := hash(blob)
		digests[item.Field] = digest
		if subject[item.Field] != digest {
			return SubjectLineage{}, fmt.Errorf("signed %s does not match %s blob at subject commit %s", item.Field, item.Path, commit)
		}
		current, e := os.ReadFile(filepath.Join(root, item.Path))
		if e != nil {
			return SubjectLineage{}, fmt.Errorf("current planning input %s is absent", item.Path)
		}
		if hash(current) != digest {
			return SubjectLineage{}, fmt.Errorf("current planning input %s differs from signed subject blob", item.Path)
		}
	}
	if commit != head {
		list, e := gitText(root, "rev-list", "--reverse", "--ancestry-path", commit+".."+head)
		if e != nil {
			return SubjectLineage{}, e
		}
		for _, rev := range strings.Fields(list) {
			for _, item := range PlanningInputs {
				blob, e := gitBlob(root, rev, item.Path)
				if e != nil {
					return SubjectLineage{}, fmt.Errorf("planning input history deletes %s at %s; rollback replay is forbidden", item.Path, rev)
				}
				if hash(blob) != digests[item.Field] {
					return SubjectLineage{}, fmt.Errorf("planning input history changes %s at %s; changed-then-reverted ancestry cannot reuse evidence", item.Path, rev)
				}
			}
		}
	}
	mode := "current"
	if commit != head {
		mode = "ancestor"
	}
	return SubjectLineage{commit, head, mode, digests}, nil
}
func CanonicalSummary(e map[string]any) ([]byte, error) {
	kind, _ := e["kind"].(string)
	fields := SummaryFields[kind]
	if len(fields) == 0 {
		return nil, fmt.Errorf("unsupported evidence kind: %s", kind)
	}
	s := map[string]any{}
	for _, f := range fields {
		s[f] = e[f]
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(raw)
}
func CanonicalSummaryDigest(e map[string]any) (string, error) {
	b, err := CanonicalSummary(e)
	if err != nil {
		return "", err
	}
	return "sha256:" + hash(b), nil
}

func JCSDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", err
	}
	return "sha256:" + hash(canonical), nil
}

func ValidateCoverage(result map[string]any, requirements, phases []string) []string {
	p := []string{}
	for _, entry := range []struct {
		name     string
		expected []string
	}{{"requirements", requirements}, {"phases", phases}} {
		items, _ := result[entry.name].([]any)
		ids := []string{}
		for _, raw := range items {
			m, _ := raw.(map[string]any)
			id, _ := m["id"].(string)
			ids = append(ids, id)
			if m["result"] != "PASS" {
				p = append(p, id+" result is not PASS")
			}
		}
		if strings.Join(ids, "\x00") != strings.Join(entry.expected, "\x00") {
			p = append(p, entry.name+" must be exact and ordered")
		}
	}
	must, _ := result["must_fix"].([]any)
	if count(result["must_fix_count"]) != 0 || len(must) != 0 {
		p = append(p, "must_fix_count and must_fix details must both be zero/empty")
	}
	should, _ := result["should_fix"].([]any)
	if count(result["should_fix_count"]) != len(should) {
		p = append(p, "should_fix_count does not match should_fix details")
	}
	if result["verdict"] != "PASS" {
		p = append(p, "verdict is not PASS")
	}
	return p
}
func count(v any) int {
	max := uint64(^uint(0) >> 1)
	switch n := v.(type) {
	case int:
		if n >= 0 {
			return n
		}
	case int64:
		if n >= 0 && uint64(n) <= max {
			return int(n)
		}
	case json.Number:
		value, err := n.Int64()
		if err == nil && value >= 0 && uint64(value) <= max {
			return int(value)
		}
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && math.Trunc(n) == n && n <= float64(max) && uint64(n) <= max {
			return int(n)
		}
	}
	return -1
}

type Authentication struct {
	Candidate      map[string]any
	MaterialSHA256 string
	MaterialBytes  int64
}

// RequireExternalFile applies the same lexical and symlink-resolved repository
// boundary to raw evidence, trust registries and trusted-channel records.
func RequireExternalFile(root, candidate, label string) (string, error) {
	return structuredfile.RequireOutsideFile(root, candidate, label)
}

func Authenticate(root string, e map[string]any, role, keysPath, confirmationPath string) (Authentication, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Authentication{}, err
	}
	canonical, err := CanonicalSummary(e)
	if err != nil {
		return Authentication{}, err
	}
	if keysPath != "" {
		abs, err := RequireExternalFile(root, keysPath, "TRUSTED_KEYS registry")
		if err != nil {
			return Authentication{}, err
		}
		registryValue, registryRaw, err := structuredfile.LoadAny(abs)
		if err != nil {
			return Authentication{}, err
		}
		if err := schema.ValidateFile(root, "spec/schemas/trusted-key-registry.schema.json", registryValue); err != nil {
			return Authentication{}, err
		}
		registry, ok := registryValue.(map[string]any)
		if !ok {
			return Authentication{}, fmt.Errorf("trusted key registry is not an object")
		}
		att, _ := e["attestation"].(map[string]any)
		if att["algorithm"] != "Ed25519" {
			return Authentication{}, fmt.Errorf("evidence attestation algorithm must be Ed25519")
		}
		keyID, _ := att["key_id"].(string)
		if keyID == "" {
			return Authentication{}, fmt.Errorf("evidence attestation key_id is required")
		}
		var selected map[string]any
		seen := map[string]bool{}
		keys, _ := registry["keys"].([]any)
		for _, raw := range keys {
			k, _ := raw.(map[string]any)
			id, _ := k["key_id"].(string)
			if seen[id] {
				return Authentication{}, fmt.Errorf("trusted key_id %s is duplicated", id)
			}
			seen[id] = true
			if k["key_id"] == keyID {
				if selected != nil {
					return Authentication{}, fmt.Errorf("trusted key %s must resolve exactly once", keyID)
				}
				selected = k
			}
		}
		if selected == nil {
			return Authentication{}, fmt.Errorf("trusted key %s must resolve exactly once", keyID)
		}
		if selected["role"] != role {
			return Authentication{}, fmt.Errorf("trusted key role must be %s", role)
		}
		if selected["algorithm"] != "Ed25519" {
			return Authentication{}, fmt.Errorf("trusted key algorithm must be Ed25519")
		}
		block, rest := pem.Decode([]byte(fmt.Sprint(selected["public_key_pem"])))
		if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
			return Authentication{}, fmt.Errorf("trusted key public key is not one PKIX PUBLIC KEY PEM block")
		}
		pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return Authentication{}, err
		}
		pub, ok := pubAny.(ed25519.PublicKey)
		if !ok || len(pub) != ed25519.PublicKeySize {
			return Authentication{}, fmt.Errorf("trusted key is not Ed25519")
		}
		sig, err := base64.StdEncoding.Strict().DecodeString(fmt.Sprint(att["signature"]))
		if err != nil || len(sig) != ed25519.SignatureSize {
			return Authentication{}, fmt.Errorf("attestation signature must be strict Base64 for a 64-byte Ed25519 signature")
		}
		if !ed25519.Verify(pub, canonical, sig) {
			return Authentication{}, fmt.Errorf("Ed25519 signature does not verify against canonical summary")
		}
		return Authentication{Candidate: map[string]any{"mode": "trusted-key-attestation", "key_id": keyID, "key_role": role, "trusted_keys_sha256": "sha256:" + hash(registryRaw)}, MaterialSHA256: hash(registryRaw), MaterialBytes: int64(len(registryRaw))}, nil
	}
	if confirmationPath == "" {
		return Authentication{}, fmt.Errorf("authenticity requires TRUSTED_KEYS or TRUSTED_CHANNEL_CONFIRMATION")
	}
	abs, err := RequireExternalFile(root, confirmationPath, "TRUSTED_CHANNEL_CONFIRMATION")
	if err != nil {
		return Authentication{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil || len(raw) == 0 {
		return Authentication{}, fmt.Errorf("trusted-channel confirmation record is empty or unreadable")
	}
	return Authentication{Candidate: map[string]any{"mode": "manual-trusted-channel", "confirmation_sha256": "sha256:" + hash(raw), "limitation": "manual identity and independence verification is not cryptographically replayable"}, MaterialSHA256: hash(raw), MaterialBytes: int64(len(raw))}, nil
}

func PlanningAuditCandidate(doc map[string]any, auth Authentication, raw []byte) map[string]any {
	reviewer, _ := doc["reviewer"].(map[string]any)
	candidate := map[string]any{
		"schema_version":      1,
		"kind":                "arop-planning-audit-summary",
		"subject":             doc["subject"],
		"reviewer_id":         reviewer["id"],
		"result":              doc["result"],
		"attested_at":         doc["attested_at"],
		"summary_sha256":      doc["summary_sha256"],
		"authentication":      auth.Candidate,
		"raw_evidence_sha256": "sha256:" + hash(raw),
	}
	if auth.Candidate["mode"] == "trusted-key-attestation" {
		candidate["attestation"] = doc["attestation"]
	}
	return candidate
}

func ValidateP03ReportPolicy(audit *report.Report) []string {
	problems := []string{}
	if audit == nil {
		return []string{"P03 report is absent"}
	}
	if audit.Provenance.Command != P03Command {
		problems = append(problems, "command must be "+P03Command)
	}
	if audit.Provenance.Checker.Path != "internal/tooling/cmd/arop-planning-audit/main.go" {
		problems = append(problems, "checker path is not the fixed P03 checker")
	}
	checkNames := make([]string, 0, len(audit.Checks))
	for _, check := range audit.Checks {
		checkNames = append(checkNames, check.Name)
		if !check.Passed {
			problems = append(problems, "required P03 check failed: "+check.Name)
		}
	}
	if strings.Join(checkNames, "\x00") != strings.Join(P03RequiredChecks, "\x00") {
		problems = append(problems, "P03 required checks are not exact and ordered")
	}
	inputPaths := make([]string, 0, len(audit.Provenance.Inputs.Files))
	for _, input := range audit.Provenance.Inputs.Files {
		inputPaths = append(inputPaths, input.Path)
	}
	expectedInputs := append([]string{}, P03StaticInputs...)
	sort.Strings(expectedInputs)
	if strings.Join(inputPaths, "\x00") != strings.Join(expectedInputs, "\x00") {
		problems = append(problems, "P03 static input closure does not match fixed policy")
	}
	if len(audit.Provenance.RuntimeInputs.Files) != 0 {
		problems = append(problems, "P03 runtime_inputs must be empty")
	}
	kinds := make([]string, 0, len(audit.Provenance.RuntimeEvidence))
	for _, item := range audit.Provenance.RuntimeEvidence {
		kinds = append(kinds, item.Kind)
	}
	if strings.Join(kinds, "\x00") != strings.Join(P03RuntimeEvidenceKinds, "\x00") {
		problems = append(problems, "P03 runtime evidence kinds do not match fixed policy")
	}
	return problems
}

type P03Revalidation struct {
	Raw                        []byte
	Authentication             Authentication
	Promoted                   map[string]any
	PromotedRaw                []byte
	CanonicalSummarySHA256     string
	PromotedEnvelopeJCSSHA256  string
	PromotedEnvelopeFileSHA256 string
}

func RevalidateP03(root, evidencePath, keysPath, confirmationPath string, requirements, phases []string, audit *report.Report, promotedPath string) (P03Revalidation, error) {
	resolved, err := RequireExternalFile(root, evidencePath, "P03_EVIDENCE")
	if err != nil {
		return P03Revalidation{}, err
	}
	value, raw, err := structuredfile.LoadAny(resolved)
	if err != nil {
		return P03Revalidation{}, fmt.Errorf("strict P03 evidence load: %w", err)
	}
	if err := schema.ValidateFile(root, "spec/schemas/planning-audit-evidence.schema.json", value); err != nil {
		return P03Revalidation{}, fmt.Errorf("P03 evidence schema: %w", err)
	}
	doc, ok := value.(map[string]any)
	if !ok {
		return P03Revalidation{}, fmt.Errorf("P03 evidence is not an object")
	}
	subject, _ := doc["subject"].(map[string]any)
	commit, _ := subject["commit"].(string)
	lineage, err := VerifySubjectCommit(root, commit, subject)
	if err != nil {
		return P03Revalidation{}, err
	}
	if subject["plan_last_phase"] != lastString(phases) {
		return P03Revalidation{}, fmt.Errorf("P03 plan_last_phase does not match fixed phase closure")
	}
	for field, digest := range lineage.Digests {
		if subject[field] != digest {
			return P03Revalidation{}, fmt.Errorf("P03 subject %s does not match planning closure", field)
		}
	}
	result, _ := doc["result"].(map[string]any)
	if problems := ValidateCoverage(result, requirements, phases); len(problems) != 0 {
		return P03Revalidation{}, fmt.Errorf("P03 coverage: %s", strings.Join(problems, "; "))
	}
	canonicalSummary, err := CanonicalSummaryDigest(doc)
	if err != nil || doc["summary_sha256"] != canonicalSummary {
		return P03Revalidation{}, fmt.Errorf("P03 canonical summary digest mismatch")
	}
	auth, err := Authenticate(root, doc, "independent_reviewer", keysPath, confirmationPath)
	if err != nil {
		return P03Revalidation{}, fmt.Errorf("P03 reviewer authentication: %w", err)
	}
	candidate := PlanningAuditCandidate(doc, auth, raw)
	if err := schema.ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", candidate); err != nil {
		return P03Revalidation{}, fmt.Errorf("reconstructed P03 promoted envelope: %w", err)
	}
	promotedValue, promotedRaw, err := structuredfile.LoadAny(promotedPath)
	if err != nil {
		return P03Revalidation{}, fmt.Errorf("strict promoted P03 envelope load: %w", err)
	}
	if err := schema.ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", promotedValue); err != nil {
		return P03Revalidation{}, fmt.Errorf("promoted P03 envelope schema: %w", err)
	}
	promoted, ok := promotedValue.(map[string]any)
	if !ok || promoted["kind"] != "arop-planning-audit-summary" {
		return P03Revalidation{}, fmt.Errorf("promoted P03 envelope has the wrong kind")
	}
	candidateJCS, err := JCSDigest(candidate)
	if err != nil {
		return P03Revalidation{}, err
	}
	promotedJCS, err := JCSDigest(promoted)
	if err != nil || candidateJCS != promotedJCS {
		return P03Revalidation{}, fmt.Errorf("promoted P03 envelope differs from revalidated raw evidence and authentication")
	}
	if problems := ValidateP03ReportPolicy(audit); len(problems) != 0 {
		return P03Revalidation{}, fmt.Errorf("P03 report policy: %s", strings.Join(problems, "; "))
	}
	if err := matchP03ReportBindings(audit, doc, auth, raw, promotedRaw, canonicalSummary, promotedJCS); err != nil {
		return P03Revalidation{}, err
	}
	return P03Revalidation{
		Raw:                        raw,
		Authentication:             auth,
		Promoted:                   promoted,
		PromotedRaw:                promotedRaw,
		CanonicalSummarySHA256:     canonicalSummary,
		PromotedEnvelopeJCSSHA256:  promotedJCS,
		PromotedEnvelopeFileSHA256: "sha256:" + hash(promotedRaw),
	}, nil
}

func matchP03ReportBindings(audit *report.Report, doc map[string]any, auth Authentication, raw, promotedRaw []byte, canonicalSummary, promotedJCS string) error {
	subjectJCS, subjectErr := JCSDigest(doc["subject"])
	resultJCS, resultErr := JCSDigest(doc["result"])
	authJCS, authErr := JCSDigest(auth.Candidate)
	if subjectErr != nil || resultErr != nil || authErr != nil {
		return fmt.Errorf("cannot canonicalize P03 report binding components")
	}
	expected := map[string]any{
		"candidate_file_sha256":    "sha256:" + hash(promotedRaw),
		"candidate_jcs_sha256":     promotedJCS,
		"canonical_summary_sha256": canonicalSummary,
		"subject_jcs":              subjectJCS,
		"result_jcs":               resultJCS,
		"auth_jcs":                 authJCS,
		"authentication_mode":      auth.Candidate["mode"],
		"raw_evidence_committed":   false,
	}
	for key, value := range expected {
		if audit.Summary[key] != value {
			return fmt.Errorf("P03 report summary %s does not match revalidated material", key)
		}
	}
	rawSummary, _ := audit.Summary["raw_evidence"].(map[string]any)
	if rawSummary["sha256"] != "sha256:"+hash(raw) || strictInt64(rawSummary["bytes"]) != int64(len(raw)) {
		return fmt.Errorf("P03 report raw evidence binding does not match revalidated material")
	}
	evidenceByKind := map[string]report.RuntimeEvidence{}
	for _, item := range audit.Provenance.RuntimeEvidence {
		evidenceByKind[item.Kind] = item
	}
	checks := map[string]struct {
		sha   string
		bytes int64
	}{
		"canonical-summary-candidate":      {hash(promotedRaw), int64(len(promotedRaw))},
		"external-authentication-material": {auth.MaterialSHA256, auth.MaterialBytes},
		"external-planning-audit-evidence": {hash(raw), int64(len(raw))},
	}
	for kind, expected := range checks {
		actual, ok := evidenceByKind[kind]
		if !ok || actual.SHA256 != expected.sha || actual.Bytes != expected.bytes {
			return fmt.Errorf("P03 report runtime evidence %s does not match revalidated material", kind)
		}
	}
	return nil
}

func strictInt64(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		v, err := n.Int64()
		if err == nil && v >= 0 {
			return v
		}
	case int:
		if n >= 0 {
			return int64(n)
		}
	case int64:
		if n >= 0 {
			return n
		}
	case float64:
		if n >= 0 && math.Trunc(n) == n && n <= math.MaxInt64 {
			return int64(n)
		}
	}
	return -1
}

func lastString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}
