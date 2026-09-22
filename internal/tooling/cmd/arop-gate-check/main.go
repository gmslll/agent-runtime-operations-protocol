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
	record("supported-gate", os.Getenv("GATE") == "P04", "only P04 is supported")
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
	record("external-evidence-location", outside, "raw Gate evidence must remain outside repository")
	record("external-evidence-readable", err == nil && len(raw) > 0, fmt.Sprint(err))
	record("user-gate-evidence-schema", doc["schema_version"] != nil && doc["kind"] == "arop-user-gate" && doc["gate"] == "P04", "required evidence shape, kind and gate")
	audit, mode, aerr := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P03/report.json"})
	record("planning-audit-report-integrity", aerr == nil, detail(aerr, "P03 report verified in "+mode))
	record("planning-audit-prerequisite", aerr == nil && audit.Success, "P03 report must be successful")
	promotedPath := filepath.Join(root, "spec/evidence/P03-planning-audit-summary.json")
	var promoted map[string]any
	perr := structuredfile.Load(promotedPath, &promoted)
	record("promoted-planning-audit-summary", perr == nil && promoted["kind"] == "arop-planning-audit-summary", detail(perr, "promoted P03 canonical summary loaded from spec/evidence"))
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
	if subject["planning_audit_summary_sha256"] != promoted["summary_sha256"] {
		mismatch = append(mismatch, "planning_audit_summary_sha256 must bind promoted P03 canonical summary")
	}
	record("gate-subject-binding", lerr == nil && perr == nil && len(mismatch) == 0, strings.Join(mismatch, "; "))
	result, _ := doc["result"].(map[string]any)
	coverage := evidence.ValidateCoverage(result, requirements, phases)
	record("complete-gate-results", len(coverage) == 0, strings.Join(coverage, "; "))
	digest, derr := evidence.CanonicalSummaryDigest(doc)
	record("canonical-summary-content-binding", derr == nil && doc["summary_sha256"] == digest, detail(derr, "canonical summary digest matches"))
	auth, authErr := evidence.Authenticate(root, doc, "project_owner", os.Getenv("TRUSTED_KEYS"), os.Getenv("TRUSTED_CHANNEL_CONFIRMATION"))
	record("owner-authenticity-gate", authErr == nil, detail(authErr, fmt.Sprint(auth["mode"])))
	command := os.Getenv("AROP_CHECK_COMMAND")
	if command == "" {
		command = "go run ./internal/tooling/cmd/arop-gate-check"
	}
	r, werr := report.Write(report.WriteOptions{Root: root, Directory: "build/reports/P04", Suite: "arop-user-gate", Class: "arop.user-gate", Command: command, CheckerPath: "internal/tooling/cmd/arop-gate-check/main.go", InputPaths: []string{"spec/requirements.yaml", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md", "spec/artifact-manifest.yaml", "spec/schemas/user-gate-evidence.schema.json", "spec/schemas/check-report.schema.json", "internal/tooling/cmd/arop-gate-check/main.go", "internal/tooling/evidence/validation.go", "internal/tooling/report/writer.go", "internal/tooling/report/verifier.go", "internal/tooling/structuredfile/files.go"}, Checks: checks, Summary: map[string]any{"gate": "P04", "evidence_sha256": report.Hash(raw), "canonical_summary_sha256": digest, "authentication_mode": auth["mode"], "promoted_planning_audit_summary_sha256": promoted["summary_sha256"], "raw_evidence_committed": false}, AuditNote: "P04 consumes and verifies the promoted P03 canonical summary; it does not create or infer approval."})
	fatal(werr)
	if r.Success {
		summary := map[string]any{"schema_version": 1, "kind": "arop-user-gate-summary", "gate": "P04", "subject": subject, "approver_id": nested(doc, "approver", "id"), "result": result, "attested_at": doc["attested_at"], "summary_sha256": doc["summary_sha256"], "authentication": auth, "raw_evidence_sha256": "sha256:" + report.Hash(raw)}
		data, _ := json.MarshalIndent(summary, "", "  ")
		fatal(os.MkdirAll(filepath.Join(root, "build/reports/P04"), 0755))
		fatal(os.WriteFile(filepath.Join(root, "build/reports/P04/canonical-summary.json"), append(data, '\n'), 0644))
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
func detail(e error, s string) string {
	if e != nil {
		return e.Error()
	}
	return s
}
func nested(m map[string]any, a, b string) any { x, _ := m[a].(map[string]any); return x[b] }
func fatal(e error) {
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
