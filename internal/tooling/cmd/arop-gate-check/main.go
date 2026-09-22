package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/evidence"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	checks := []report.Check{}
	record := func(n string, ok bool, d string) { checks = append(checks, report.Check{n, ok, d}) }
	record("supported-gate", os.Getenv("GATE") == "P04", "only P04 is supported")
	arg := os.Getenv("EVIDENCE")
	if arg == "" && len(os.Args) > 1 {
		arg = os.Args[1]
	}
	doc := map[string]any{}
	raw := []byte{}
	outside := false
	var loadErr error
	if arg != "" {
		abs, absErr := filepath.Abs(arg)
		if absErr != nil {
			loadErr = absErr
		} else {
			rel, relErr := filepath.Rel(root, abs)
			outside = relErr == nil && strings.HasPrefix(rel, "..")
			value, bytes, strictErr := structuredfile.LoadAny(abs)
			loadErr = strictErr
			raw = bytes
			if strictErr == nil {
				loadErr = schema.ValidateFile(root, "spec/schemas/user-gate-evidence.schema.json", value)
				doc, _ = value.(map[string]any)
			}
		}
	} else {
		loadErr = fmt.Errorf("EVIDENCE is required")
	}
	record("external-evidence-location", outside, "raw Gate evidence must remain outside repository")
	record("external-evidence-readable", loadErr == nil && len(raw) > 0, detail(loadErr, "strict external evidence loaded"))
	record("user-gate-evidence-schema", loadErr == nil, detail(loadErr, "Draft 2020-12 schema and format assertions passed"))
	audit, mode, aerr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P03/report.json"})
	record("planning-audit-report-integrity", aerr == nil, detail(aerr, "P03 report verified in "+mode))
	record("planning-audit-prerequisite", aerr == nil && audit.Success, "P03 report must be successful")
	promotedPath := filepath.Join(root, "spec/evidence/P03-planning-audit-summary.json")
	promoted := map[string]any{}
	promotedValue, promotedRaw, perr := structuredfile.LoadAny(promotedPath)
	if perr == nil {
		perr = schema.ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", promotedValue)
		promoted, _ = promotedValue.(map[string]any)
	}
	record("promoted-planning-audit-summary", perr == nil && promoted["kind"] == "arop-planning-audit-summary", detail(perr, "promoted P03 canonical summary loaded and schema validated"))
	promotedExternal := map[string]any{"schema_version": promoted["schema_version"], "kind": "arop-planning-audit", "subject": promoted["subject"], "reviewer": map[string]any{"id": promoted["reviewer_id"]}, "result": promoted["result"], "attested_at": promoted["attested_at"]}
	promotedExternal["summary_sha256"] = promoted["summary_sha256"]
	if promoted["attestation"] != nil {
		promotedExternal["attestation"] = promoted["attestation"]
	}
	if perr == nil {
		perr = schema.ValidateFile(root, "spec/schemas/planning-audit-evidence.schema.json", promotedExternal)
	}
	promotedCanonical, promotedCanonicalErr := evidence.CanonicalSummaryDigest(promotedExternal)
	promotedSubjectJCS, promotedSubjectErr := evidence.JCSDigest(promoted["subject"])
	promotedResultJCS, promotedResultErr := evidence.JCSDigest(promoted["result"])
	promotedAuthJCS, promotedAuthErr := evidence.JCSDigest(promoted["authentication"])
	p03Binding := []string{}
	if aerr == nil {
		if audit.Summary["candidate_file_sha256"] != "sha256:"+report.Hash(promotedRaw) {
			p03Binding = append(p03Binding, "candidate_file_sha256")
		}
		if promotedCanonicalErr != nil || promoted["summary_sha256"] != promotedCanonical || audit.Summary["canonical_summary_sha256"] != promotedCanonical {
			p03Binding = append(p03Binding, "canonical_summary_sha256")
		}
		if promotedSubjectErr != nil || audit.Summary["subject_jcs"] != promotedSubjectJCS {
			p03Binding = append(p03Binding, "subject_jcs")
		}
		if promotedResultErr != nil || audit.Summary["result_jcs"] != promotedResultJCS {
			p03Binding = append(p03Binding, "result_jcs")
		}
		if promotedAuthErr != nil || audit.Summary["auth_jcs"] != promotedAuthJCS || audit.Summary["authentication_mode"] != nested(promoted, "authentication", "mode") {
			p03Binding = append(p03Binding, "authentication")
		}
		rawBinding, _ := audit.Summary["raw_evidence"].(map[string]any)
		if rawBinding["sha256"] != promoted["raw_evidence_sha256"] {
			p03Binding = append(p03Binding, "raw_evidence_sha256")
		}
	}
	record("planning-audit-promoted-summary-binding", aerr == nil && perr == nil && len(p03Binding) == 0, strings.Join(p03Binding, "; "))
	subject, _ := doc["subject"].(map[string]any)
	commit, _ := subject["commit"].(string)
	lineage, lerr := evidence.VerifySubjectCommit(root, commit, subject)
	record("subject-commit-lineage", lerr == nil, detail(lerr, lineage.Mode))
	phases, requirements := ids(root)
	mismatch := []string{}
	for k, v := range lineage.Digests {
		if subject[k] != v {
			mismatch = append(mismatch, k)
		}
	}
	if subject["plan_last_phase"] != last(phases) {
		mismatch = append(mismatch, "plan_last_phase")
	}
	if subject["planning_audit_summary_sha256"] != promotedCanonical {
		mismatch = append(mismatch, "planning_audit_summary_sha256 must bind promoted P03 canonical summary")
	}
	record("gate-subject-binding", lerr == nil && perr == nil && len(mismatch) == 0, strings.Join(mismatch, "; "))
	result, _ := doc["result"].(map[string]any)
	coverage := evidence.ValidateCoverage(result, requirements, phases)
	record("complete-gate-results", len(coverage) == 0, strings.Join(coverage, "; "))
	digest, derr := evidence.CanonicalSummaryDigest(doc)
	record("canonical-summary-content-binding", derr == nil && doc["summary_sha256"] == digest, detail(derr, "canonical summary digest matches"))
	auth, authErr := evidence.Authenticate(root, doc, "project_owner", os.Getenv("TRUSTED_KEYS"), os.Getenv("TRUSTED_CHANNEL_CONFIRMATION"))
	record("owner-authenticity-gate", authErr == nil, detail(authErr, fmt.Sprint(auth.Candidate["mode"])))
	candidate := map[string]any{"schema_version": 1, "kind": "arop-user-gate-summary", "gate": "P04", "subject": subject, "approver_id": nested(doc, "approver", "id"), "result": result, "attested_at": doc["attested_at"], "summary_sha256": doc["summary_sha256"], "authentication": auth.Candidate, "raw_evidence_sha256": "sha256:" + report.Hash(raw)}
	if auth.Candidate["mode"] == "trusted-key-attestation" {
		candidate["attestation"] = doc["attestation"]
	}
	candidateData, candidateErr := json.MarshalIndent(candidate, "", "  ")
	if candidateErr == nil {
		candidateErr = schema.ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", candidate)
	}
	if candidateErr == nil {
		candidateData = append(candidateData, '\n')
	}
	record("canonical-summary-candidate-schema", candidateErr == nil, detail(candidateErr, "candidate preserves authentication material and passes schema"))
	subjectJCS, subjectErr := evidence.JCSDigest(subject)
	resultJCS, resultErr := evidence.JCSDigest(result)
	authJCS, authJCSErr := evidence.JCSDigest(auth.Candidate)
	record("canonical-summary-component-digests", subjectErr == nil && resultErr == nil && authJCSErr == nil, detail(first(subjectErr, resultErr, authJCSErr), "subject/result/auth JCS digests computed"))
	if allPassed(checks) {
		fatal(os.MkdirAll(filepath.Join(root, "build/reports/P04"), 0o755))
		fatal(os.WriteFile(filepath.Join(root, "build/reports/P04/canonical-summary.json"), candidateData, 0o644))
	}
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-gate-check"
	}
	runtimeEvidence := []report.RuntimeEvidence{{Kind: "canonical-summary-candidate", SHA256: report.Hash(candidateData), Bytes: int64(len(candidateData))}, {Kind: "external-user-gate-evidence", SHA256: report.Hash(raw), Bytes: int64(len(raw))}}
	if auth.MaterialSHA256 != "" {
		runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "external-authentication-material", SHA256: auth.MaterialSHA256, Bytes: auth.MaterialBytes})
	}
	r, werr := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P04", Suite: "arop-user-gate", Class: "arop.user-gate", Command: command, CheckerPath: "internal/tooling/cmd/arop-gate-check/main.go", InputPaths: []string{"go.mod", "go.sum", "spec/requirements.yaml", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "spec/artifact-manifest.yaml", "spec/schemas/user-gate-evidence.schema.json", "spec/schemas/canonical-evidence-summary.schema.json", "spec/schemas/trusted-key-registry.schema.json", "spec/schemas/check-report.schema.json", "internal/tooling/cmd/arop-gate-check/main.go", "internal/tooling/evidence/validation.go", "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go", "internal/tooling/schema/validator.go", "internal/tooling/structuredfile/files.go"}, RuntimeInputPaths: []string{"build/reports/P03/report.json", "build/reports/P03/junit.xml", "spec/evidence/P03-planning-audit-summary.json"}, RuntimeEvidence: runtimeEvidence, Checks: checks, Summary: map[string]any{"gate": "P04", "candidate_file_sha256": "sha256:" + report.Hash(candidateData), "canonical_summary_sha256": digest, "subject_jcs": subjectJCS, "result_jcs": resultJCS, "raw_evidence": map[string]any{"sha256": "sha256:" + report.Hash(raw), "bytes": len(raw)}, "auth_jcs": authJCS, "authentication_mode": auth.Candidate["mode"], "promoted_planning_audit_summary_sha256": promotedCanonical, "raw_evidence_committed": false}, AuditNote: "P04 consumes and cross-checks the P03 report, JUnit, and promoted summary; external authentication material is recorded only by digest and byte count."})
	fatal(werr)
	if r.Success {
		fmt.Println("AROP P04 Gate passed; canonical summary candidate is in build/reports/P04/.")
		return
	}
	os.Exit(1)
}
func ids(root string) ([]string, []string) {
	plan, _ := os.ReadFile(filepath.Join(root, "docs/DEVELOPMENT_PLAN.md"))
	m := regexp.MustCompile(`(?m)^## (P[0-9]{2}) —`).FindAllStringSubmatch(string(plan), -1)
	p := []string{}
	for _, x := range m {
		p = append(p, x[1])
	}
	var req struct {
		Requirements []struct {
			ID string `json:"id" yaml:"id"`
		} `json:"requirements" yaml:"requirements"`
	}
	_ = structuredfile.Load(filepath.Join(root, "spec/requirements.yaml"), &req)
	r := []string{}
	for _, x := range req.Requirements {
		r = append(r, x.ID)
	}
	return p, r
}
func last(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
func detail(e error, s string) string {
	if e != nil {
		return e.Error()
	}
	return s
}
func nested(m map[string]any, a, b string) any { x, _ := m[a].(map[string]any); return x[b] }
func allPassed(checks []report.Check) bool {
	for _, check := range checks {
		if !check.Passed {
			return false
		}
	}
	return true
}
func first(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
func fatal(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
