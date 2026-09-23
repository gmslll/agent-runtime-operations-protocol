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
	record := func(n string, ok bool, d string) {
		checks = append(checks, report.Check{Name: n, Passed: ok, Detail: d})
	}
	arg := os.Getenv("EVIDENCE")
	if arg == "" && len(os.Args) > 1 {
		arg = os.Args[1]
	}
	doc := map[string]any{}
	raw := []byte{}
	outside := false
	var loadErr error
	if arg != "" {
		resolved, boundaryErr := evidence.RequireExternalFile(root, arg, "EVIDENCE")
		if boundaryErr != nil {
			loadErr = boundaryErr
		} else {
			outside = true
			value, bytes, strictErr := structuredfile.LoadAny(resolved)
			loadErr = strictErr
			raw = bytes
			if strictErr == nil {
				loadErr = schema.ValidateFile(root, "spec/schemas/planning-audit-evidence.schema.json", value)
				doc, _ = value.(map[string]any)
			}
		}
	} else {
		loadErr = fmt.Errorf("EVIDENCE is required")
	}
	record("external-evidence-location", outside, "raw review evidence must remain outside the Git repository")
	record("external-evidence-readable", loadErr == nil && len(raw) > 0, detail(loadErr, "strict external evidence loaded"))
	record("planning-audit-evidence-schema", loadErr == nil, detail(loadErr, "Draft 2020-12 schema and format assertions passed"))
	subject, _ := doc["subject"].(map[string]any)
	commit, _ := subject["commit"].(string)
	lineage, lerr := evidence.VerifySubjectCommit(root, commit, subject)
	record("subject-commit-lineage", lerr == nil, detail(lerr, fmt.Sprintf("%s commit %s", lineage.Mode, lineage.SubjectCommit)))
	phases, requirements := ids(root)
	expected := map[string]string{"plan_last_phase": last(phases)}
	for k, v := range lineage.Digests {
		expected[k] = v
	}
	mismatch := []string{}
	for k, v := range expected {
		if subject[k] != v {
			mismatch = append(mismatch, fmt.Sprintf("%s expected %s got %v", k, v, subject[k]))
		}
	}
	record("audit-subject-binding", lerr == nil && len(mismatch) == 0, strings.Join(mismatch, "; "))
	result, _ := doc["result"].(map[string]any)
	coverage := evidence.ValidateCoverage(result, requirements, phases)
	record("complete-audit-results", len(coverage) == 0, strings.Join(coverage, "; "))
	digest, derr := evidence.CanonicalSummaryDigest(doc)
	record("canonical-summary-content-binding", derr == nil && doc["summary_sha256"] == digest, detail(derr, "canonical summary digest matches"))
	auth, aerr := evidence.Authenticate(root, doc, "independent_reviewer", os.Getenv("TRUSTED_KEYS"), os.Getenv("TRUSTED_CHANNEL_CONFIRMATION"))
	record("reviewer-authenticity-gate", aerr == nil, detail(aerr, fmt.Sprint(auth.Candidate["mode"])))
	candidate := evidence.PlanningAuditCandidate(doc, auth, raw)
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
	authJCS, authErr := evidence.JCSDigest(auth.Candidate)
	candidateJCS, candidateJCSErr := evidence.JCSDigest(candidate)
	record("canonical-summary-component-digests", subjectErr == nil && resultErr == nil && authErr == nil && candidateJCSErr == nil, detail(first(subjectErr, resultErr, authErr, candidateJCSErr), "subject/result/auth/candidate JCS digests computed"))
	if allPassed(checks) {
		fatal(os.MkdirAll(filepath.Join(root, "build/reports/P03"), 0o755))
		fatal(os.WriteFile(filepath.Join(root, "build/reports/P03/canonical-summary.json"), candidateData, 0o644))
	}
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = evidence.P03Command
	}
	runtimeEvidence := []report.RuntimeEvidence{{Kind: "canonical-summary-candidate", SHA256: report.Hash(candidateData), Bytes: int64(len(candidateData))}, {Kind: "external-planning-audit-evidence", SHA256: report.Hash(raw), Bytes: int64(len(raw))}}
	if auth.MaterialSHA256 != "" {
		runtimeEvidence = append(runtimeEvidence, report.RuntimeEvidence{Kind: "external-authentication-material", SHA256: auth.MaterialSHA256, Bytes: auth.MaterialBytes})
	}
	r, werr := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P03", Suite: "arop-planning-audit", Class: "arop.planning-audit", Command: command, CheckerPath: "internal/tooling/cmd/arop-planning-audit/main.go", InputPaths: evidence.P03StaticInputs, RuntimeEvidence: runtimeEvidence, Checks: checks, Summary: map[string]any{"candidate_file_sha256": "sha256:" + report.Hash(candidateData), "candidate_jcs_sha256": candidateJCS, "canonical_summary_sha256": digest, "subject_jcs": subjectJCS, "result_jcs": resultJCS, "raw_evidence": map[string]any{"sha256": "sha256:" + report.Hash(raw), "bytes": len(raw)}, "auth_jcs": authJCS, "authentication_mode": auth.Candidate["mode"], "raw_evidence_committed": false}, AuditNote: "Reviewer authenticity comes from external trusted material recorded only by digest and byte count; this tool never infers independence."})
	fatal(werr)
	if r.Success {
		fmt.Println("AROP planning audit evidence passed; canonical summary candidate is in build/reports/P03/.")
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
func detail(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}
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
