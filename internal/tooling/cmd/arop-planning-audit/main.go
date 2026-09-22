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
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	root, err := structuredfile.FindRoot(".")
	fatal(err)
	checks := []report.Check{}
	record := func(n string, ok bool, d string) { checks = append(checks, report.Check{n, ok, d}) }
	arg := os.Getenv("EVIDENCE")
	if arg == "" && len(os.Args) > 1 {
		arg = os.Args[1]
	}
	var doc map[string]any
	raw := []byte{}
	outside := false
	if arg != "" {
		abs, _ := filepath.Abs(arg)
		rel, _ := filepath.Rel(root, abs)
		outside = strings.HasPrefix(rel, "..")
		raw, err = os.ReadFile(abs)
		if err == nil {
			err = structuredfile.Load(abs, &doc)
		}
	}
	record("external-evidence-location", outside, "raw review evidence must remain outside the Git repository")
	record("external-evidence-readable", err == nil && len(raw) > 0, fmt.Sprint(err))
	valid := doc["schema_version"] != nil && doc["kind"] == "arop-planning-audit" && doc["subject"] != nil && doc["result"] != nil
	record("planning-audit-evidence-schema", valid, "required evidence shape and kind")
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
	record("reviewer-authenticity-gate", aerr == nil, detail(aerr, fmt.Sprint(auth["mode"])))
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-planning-audit"
	}
	r, werr := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P03", Suite: "arop-planning-audit", Class: "arop.planning-audit", Command: command, CheckerPath: "internal/tooling/cmd/arop-planning-audit/main.go", InputPaths: []string{"spec/requirements.yaml", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "spec/artifact-manifest.yaml", "spec/schemas/planning-audit-evidence.schema.json", "spec/schemas/check-report.schema.json", "internal/tooling/cmd/arop-planning-audit/main.go", "internal/tooling/evidence/validation.go", "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go", "internal/tooling/structuredfile/files.go"}, Checks: checks, Summary: map[string]any{"evidence_sha256": report.Hash(raw), "canonical_summary_sha256": digest, "authentication_mode": auth["mode"], "raw_evidence_committed": false}, AuditNote: "Reviewer authenticity comes from an external trusted role/signature or a human-owned trusted channel; this tool never infers independence."})
	fatal(werr)
	if r.Success {
		candidate := map[string]any{"schema_version": 1, "kind": "arop-planning-audit-summary", "subject": subject, "reviewer_id": nested(doc, "reviewer", "id"), "result": result, "attested_at": doc["attested_at"], "summary_sha256": doc["summary_sha256"], "authentication": auth, "raw_evidence_sha256": "sha256:" + report.Hash(raw)}
		data, _ := json.MarshalIndent(candidate, "", "  ")
		fatal(os.WriteFile(filepath.Join(root, "build/reports/P03/canonical-summary.json"), append(data, '\n'), 0644))
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
			ID string `yaml:"id"`
		} `yaml:"requirements"`
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
func nested(m map[string]any, a, b string) any { x, _ := m[a].(map[string]any); return x[b] }
func fatal(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
