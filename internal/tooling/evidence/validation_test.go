package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
)

func signedPlanningEvidence(t *testing.T) (map[string]any, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"schema_version": 1,
		"kind":           "arop-planning-audit",
		"subject":        map[string]any{"commit": strings.Repeat("a", 40)},
		"reviewer":       map[string]any{"id": "reviewer-1"},
		"result":         map[string]any{"verdict": "PASS"},
		"attested_at":    "2026-09-22T01:02:03Z",
	}
	canonical, err := CanonicalSummary(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["attestation"] = map[string]any{"algorithm": "Ed25519", "key_id": "key-1", "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical))}
	return doc, pub
}

func writeRegistry(t *testing.T, path string, pub ed25519.PublicKey, mutate func(map[string]any)) {
	writeRegistryRole(t, path, pub, "independent_reviewer", mutate)
}

func writeRegistryRole(t *testing.T, path string, pub ed25519.PublicKey, role string, mutate func(map[string]any)) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	registry := map[string]any{"schema_version": 1, "keys": []any{map[string]any{"key_id": "key-1", "algorithm": "Ed25519", "role": role, "public_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}
	if mutate != nil {
		mutate(registry)
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateStrictEd25519Registry(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	doc, pub := signedPlanningEvidence(t)
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "keys.json")
	writeRegistry(t, registryPath, pub, nil)
	auth, err := Authenticate(root, doc, "independent_reviewer", registryPath, "")
	if err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if auth.Candidate["mode"] != "trusted-key-attestation" || auth.Candidate["trusted_keys_sha256"] == nil || auth.MaterialBytes == 0 {
		t.Fatalf("authentication result omitted replay material: %#v", auth)
	}

	for name, mutate := range map[string]func(map[string]any){
		"evidence algorithm": func(v map[string]any) { v["attestation"].(map[string]any)["algorithm"] = "RSA" },
		"empty key id":       func(v map[string]any) { v["attestation"].(map[string]any)["key_id"] = "" },
		"missing attestation": func(v map[string]any) {
			delete(v, "attestation")
		},
		"short signature": func(v map[string]any) {
			v["attestation"].(map[string]any)["signature"] = base64.StdEncoding.EncodeToString(make([]byte, 63))
		},
		"invalid base64": func(v map[string]any) { v["attestation"].(map[string]any)["signature"] = strings.Repeat("!", 88) },
		"nonzero base64 padding bits": func(v map[string]any) {
			// Both strings decode to the same final byte with permissive RFC 4648
			// decoders. Strict decoding must reject the non-zero unused bits.
			signature := v["attestation"].(map[string]any)["signature"].(string)
			v["attestation"].(map[string]any)["signature"] = signature[:len(signature)-2] + "B="
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyDoc := deepCopy(t, doc)
			mutate(copyDoc)
			if _, err := Authenticate(root, copyDoc, "independent_reviewer", registryPath, ""); err == nil {
				t.Fatalf("invalid authentication accepted")
			}
		})
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaDER}))
	for name, mutate := range map[string]func(map[string]any){
		"registry version":   func(v map[string]any) { v["schema_version"] = 2 },
		"registry algorithm": func(v map[string]any) { v["keys"].([]any)[0].(map[string]any)["algorithm"] = "RSA" },
		"registry role":      func(v map[string]any) { v["keys"].([]any)[0].(map[string]any)["role"] = "project_owner" },
		"registry extra":     func(v map[string]any) { v["keys"].([]any)[0].(map[string]any)["extra"] = true },
		"registry missing":   func(v map[string]any) { delete(v["keys"].([]any)[0].(map[string]any), "public_key_pem") },
		"non Ed25519 key":    func(v map[string]any) { v["keys"].([]any)[0].(map[string]any)["public_key_pem"] = rsaPEM },
		"duplicate key id": func(v map[string]any) {
			keys := v["keys"].([]any)
			v["keys"] = append(keys, deepCopy(t, keys[0].(map[string]any)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			writeRegistry(t, path, pub, mutate)
			if _, err := Authenticate(root, doc, "independent_reviewer", path, ""); err == nil {
				t.Fatalf("invalid registry accepted")
			}
		})
	}

	duplicateJSON := `{"schema_version":1,"schema_version":1,"keys":[]}`
	duplicatePath := filepath.Join(dir, "duplicate-json-key.json")
	if err := os.WriteFile(duplicatePath, []byte(duplicateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(root, doc, "independent_reviewer", duplicatePath, ""); err == nil {
		t.Fatal("duplicate registry JSON key accepted")
	}
}

func TestManualAuthenticationPreservesLimitation(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	doc, _ := signedPlanningEvidence(t)
	path := filepath.Join(t.TempDir(), "confirmation.txt")
	if err := os.WriteFile(path, []byte("verified outside repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := Authenticate(root, doc, "independent_reviewer", "", path)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Candidate["limitation"] == nil || auth.Candidate["confirmation_sha256"] == nil {
		t.Fatalf("manual limitation or digest missing: %#v", auth.Candidate)
	}
}

func TestCountAcceptsOnlyNonnegativeInRangeIntegers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   any
		want int
	}{
		{"json number", json.Number("12"), 12},
		{"native int", 3, 3},
		{"integral float", float64(4), 4},
		{"fractional json number", json.Number("1.5"), -1},
		{"overflow json number", json.Number("9223372036854775808"), -1},
		{"negative json number", json.Number("-1"), -1},
		{"fractional float", 1.5, -1},
		{"negative int", -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := count(tc.in); got != tc.want {
				t.Fatalf("count(%v)=%d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestAllExternalEvidenceMaterialsUseCanonicalContainment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	insideDir := filepath.Join(root, "..inside")
	if err := os.MkdirAll(insideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(insideDir, "material.json")
	if err := os.WriteFile(inside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "material.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(outsideDir, "back-to-repository")
	if err := os.Symlink(inside, back); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"EVIDENCE", "TRUSTED_KEYS registry", "TRUSTED_CHANNEL_CONFIRMATION"} {
		t.Run(label+" valid outside", func(t *testing.T) {
			if _, err := RequireExternalFile(root, outside, label); err != nil {
				t.Fatalf("valid outside path rejected: %v", err)
			}
		})
		t.Run(label+" dot-dot-inside", func(t *testing.T) {
			if _, err := RequireExternalFile(root, inside, label); err == nil {
				t.Fatal("repository path beginning with '..' accepted")
			}
		})
		t.Run(label+" symlink-back", func(t *testing.T) {
			if _, err := RequireExternalFile(root, back, label); err == nil {
				t.Fatal("external symlink resolving into repository accepted")
			}
		})
	}
}

func TestValidP03AndP04EvidenceEndToEnd(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q")
	run(t, root, "config", "user.name", "AROP Evidence E2E")
	run(t, root, "config", "user.email", "evidence-e2e@invalid.example")
	for _, item := range PlanningInputs {
		write(t, root, item.Path, item.Path+" e2e\n")
	}
	sourceRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	for _, path := range []string{"spec/schemas/planning-audit-evidence.schema.json", "spec/schemas/user-gate-evidence.schema.json", "spec/schemas/canonical-evidence-summary.schema.json", "spec/schemas/trusted-key-registry.schema.json"} {
		data, err := os.ReadFile(filepath.Join(sourceRoot, path))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, path, string(data))
	}
	head := commit(t, root, "valid evidence fixture")
	baseSubject := subject(t, root, head)
	baseSubject["plan_last_phase"] = "P53"
	requirementIDs := make([]string, 14)
	requirementResults := make([]any, 14)
	for i := range requirementIDs {
		requirementIDs[i] = fmt.Sprintf("IR-%02d", i+1)
		requirementResults[i] = map[string]any{"id": requirementIDs[i], "result": "PASS"}
	}
	phaseIDs := make([]string, 53)
	phaseResults := make([]any, 53)
	for i := range phaseIDs {
		phaseIDs[i] = fmt.Sprintf("P%02d", i+1)
		phaseResults[i] = map[string]any{"id": phaseIDs[i], "result": "PASS"}
	}
	result := map[string]any{"requirements": requirementResults, "phases": phaseResults, "must_fix_count": json.Number("0"), "must_fix": []any{}, "should_fix_count": json.Number("0"), "should_fix": []any{}, "verdict": "PASS"}

	for _, tc := range []struct {
		name, kind, identityField, identity, role, schemaPath, summaryKind string
		gate                                                               bool
	}{
		{"P03", "arop-planning-audit", "reviewer", "reviewer-e2e", "independent_reviewer", "spec/schemas/planning-audit-evidence.schema.json", "arop-planning-audit-summary", false},
		{"P04", "arop-user-gate", "approver", "owner-e2e", "project_owner", "spec/schemas/user-gate-evidence.schema.json", "arop-user-gate-summary", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subjectCopy := deepCopy(t, baseSubject)
			if tc.gate {
				subjectCopy["planning_audit_summary_sha256"] = "sha256:" + strings.Repeat("c", 64)
				subjectCopy["planning_audit_promoted_jcs_sha256"] = "sha256:" + strings.Repeat("d", 64)
				subjectCopy["planning_audit_promoted_file_sha256"] = "sha256:" + strings.Repeat("e", 64)
			}
			doc := map[string]any{"schema_version": 1, "kind": tc.kind, "subject": subjectCopy, tc.identityField: map[string]any{"id": tc.identity}, "result": deepCopy(t, result), "attested_at": "2026-09-23T01:02:03Z"}
			if tc.gate {
				doc["gate"] = "P04"
			}
			digest, err := CanonicalSummaryDigest(doc)
			if err != nil {
				t.Fatal(err)
			}
			doc["summary_sha256"] = digest
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := CanonicalSummary(doc)
			if err != nil {
				t.Fatal(err)
			}
			doc["attestation"] = map[string]any{"algorithm": "Ed25519", "key_id": "key-1", "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical))}
			if err := schema.ValidateFile(root, tc.schemaPath, doc); err != nil {
				t.Fatalf("valid evidence schema rejected: %v", err)
			}
			if problems := ValidateCoverage(doc["result"].(map[string]any), requirementIDs, phaseIDs); len(problems) != 0 {
				t.Fatalf("valid coverage rejected: %v", problems)
			}
			if _, err := VerifySubjectCommit(root, head, subjectCopy); err != nil {
				t.Fatalf("valid subject rejected: %v", err)
			}
			registryPath := filepath.Join(t.TempDir(), "keys.json")
			writeRegistryRole(t, registryPath, pub, tc.role, nil)
			auth, err := Authenticate(root, doc, tc.role, registryPath, "")
			if err != nil {
				t.Fatalf("valid authentication rejected: %v", err)
			}
			candidate := map[string]any{"schema_version": 1, "kind": tc.summaryKind, "subject": subjectCopy, "result": doc["result"], "attested_at": doc["attested_at"], "summary_sha256": digest, "attestation": doc["attestation"], "authentication": auth.Candidate, "raw_evidence_sha256": "sha256:" + strings.Repeat("d", 64)}
			if tc.gate {
				candidate["gate"] = "P04"
				candidate["approver_id"] = tc.identity
			} else {
				candidate["reviewer_id"] = tc.identity
			}
			if err := schema.ValidateFile(root, "spec/schemas/canonical-evidence-summary.schema.json", candidate); err != nil {
				t.Fatalf("valid candidate rejected: %v", err)
			}
		})
	}
}

func TestP03AndP04CommandsAcceptCompleteValidEvidenceEndToEnd(t *testing.T) {
	sourceRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	root := t.TempDir()
	for _, path := range []string{
		"go.mod", "go.sum", "internal/tooling", "docs/DEVELOPMENT_PLAN.md", "docs/IMPLEMENTATION_BLUEPRINT.md",
		"spec/requirements.yaml", "spec/artifact-manifest.yaml", "spec/schemas/planning-audit-evidence.schema.json",
		"spec/schemas/user-gate-evidence.schema.json", "spec/schemas/canonical-evidence-summary.schema.json",
		"spec/schemas/trusted-key-registry.schema.json", "spec/schemas/check-report.schema.json",
	} {
		copyFixturePath(t, sourceRoot, root, path)
	}
	write(t, root, ".gitignore", "build/\n")
	run(t, root, "init", "-q")
	run(t, root, "config", "user.name", "AROP Command E2E")
	run(t, root, "config", "user.email", "command-e2e@invalid.example")
	subjectCommit := commit(t, root, "command fixture baseline")
	subjectP03 := subject(t, root, subjectCommit)
	subjectP03["plan_last_phase"] = "P53"
	result := completePassingResult()

	p03Doc, p03Pub := signedCompleteEvidence(t, "arop-planning-audit", subjectP03, "reviewer", "reviewer-command", result, false)
	externalDir := t.TempDir()
	p03Registry := filepath.Join(externalDir, "reviewer-keys.json")
	writeRegistryRole(t, p03Registry, p03Pub, "independent_reviewer", nil)
	p03Raw := marshalJSONLine(t, p03Doc)
	p03Evidence := filepath.Join(externalDir, "p03-evidence.json")
	if err := os.WriteFile(p03Evidence, p03Raw, 0o600); err != nil {
		t.Fatal(err)
	}
	registryRaw, err := os.ReadFile(p03Registry)
	if err != nil {
		t.Fatal(err)
	}
	p03Candidate := map[string]any{
		"schema_version": 1, "kind": "arop-planning-audit-summary", "subject": subjectP03,
		"reviewer_id": "reviewer-command", "result": result, "attested_at": p03Doc["attested_at"],
		"summary_sha256": p03Doc["summary_sha256"], "attestation": p03Doc["attestation"],
		"authentication":      map[string]any{"mode": "trusted-key-attestation", "key_id": "key-1", "key_role": "independent_reviewer", "trusted_keys_sha256": "sha256:" + hash(registryRaw)},
		"raw_evidence_sha256": "sha256:" + hash(p03Raw),
	}
	write(t, root, "spec/evidence/P03-planning-audit-summary.json", string(marshalJSONLine(t, p03Candidate)))
	gateCommit := commit(t, root, "promote P03 canonical summary")
	runCommand(t, root, map[string]string{"EVIDENCE": p03Evidence, "TRUSTED_KEYS": p03Registry, "AROP_CHECK_COMMAND": P03Command}, "go", "run", "./internal/tooling/cmd/arop-planning-audit")
	p03Report, _, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/P03/report.json"})
	if err != nil {
		t.Fatalf("P03 report fixture did not verify: %v", err)
	}
	requirementIDs, phaseIDs := completeIDs()
	promotedPath := filepath.Join(root, "spec/evidence/P03-planning-audit-summary.json")
	if _, err := RevalidateP03(root, p03Evidence, p03Registry, "", requirementIDs, phaseIDs, p03Report, promotedPath); err != nil {
		t.Fatalf("valid P03 revalidation rejected: %v", err)
	}

	originalEvidence := append([]byte{}, p03Raw...)
	tamperedEvidence := append([]byte{}, p03Raw...)
	tamperedEvidence[len(tamperedEvidence)-2] = ' '
	if err := os.WriteFile(p03Evidence, tamperedEvidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RevalidateP03(root, p03Evidence, p03Registry, "", requirementIDs, phaseIDs, p03Report, promotedPath); err == nil {
		t.Fatal("tampered P03 raw evidence accepted")
	}
	if err := os.WriteFile(p03Evidence, originalEvidence, 0o600); err != nil {
		t.Fatal(err)
	}
	originalRegistry := append([]byte{}, registryRaw...)
	if err := os.WriteFile(p03Registry, append(originalRegistry, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RevalidateP03(root, p03Evidence, p03Registry, "", requirementIDs, phaseIDs, p03Report, promotedPath); err == nil {
		t.Fatal("P03 trust material digest drift accepted")
	}
	if err := os.WriteFile(p03Registry, originalRegistry, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedReport := *p03Report
	tamperedReport.Summary = cloneAnyMap(t, p03Report.Summary)
	tamperedReport.Summary["candidate_jcs_sha256"] = "sha256:" + strings.Repeat("f", 64)
	if _, err := RevalidateP03(root, p03Evidence, p03Registry, "", requirementIDs, phaseIDs, &tamperedReport, promotedPath); err == nil {
		t.Fatal("tampered P03 report binding accepted")
	}
	originalPromoted, err := os.ReadFile(promotedPath)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPromoted := deepCopy(t, p03Candidate)
	tamperedPromoted["reviewer_id"] = "different-reviewer"
	if err := os.WriteFile(promotedPath, marshalJSONLine(t, tamperedPromoted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RevalidateP03(root, p03Evidence, p03Registry, "", requirementIDs, phaseIDs, p03Report, promotedPath); err == nil {
		t.Fatal("tampered promoted P03 envelope accepted")
	}
	if err := os.WriteFile(promotedPath, originalPromoted, 0o644); err != nil {
		t.Fatal(err)
	}

	subjectP04 := subject(t, root, gateCommit)
	subjectP04["plan_last_phase"] = "P53"
	subjectP04["planning_audit_summary_sha256"] = p03Doc["summary_sha256"]
	promotedJCS, err := JCSDigest(p03Candidate)
	if err != nil {
		t.Fatal(err)
	}
	subjectP04["planning_audit_promoted_jcs_sha256"] = promotedJCS
	subjectP04["planning_audit_promoted_file_sha256"] = "sha256:" + hash(marshalJSONLine(t, p03Candidate))
	p04Doc, p04Pub := signedCompleteEvidence(t, "arop-user-gate", subjectP04, "approver", "owner-command", result, true)
	p04Registry := filepath.Join(externalDir, "owner-keys.json")
	writeRegistryRole(t, p04Registry, p04Pub, "project_owner", nil)
	p04Evidence := filepath.Join(externalDir, "p04-evidence.json")
	if err := os.WriteFile(p04Evidence, marshalJSONLine(t, p04Doc), 0o600); err != nil {
		t.Fatal(err)
	}
	runCommand(t, root, map[string]string{"GATE": "P04", "EVIDENCE": p04Evidence, "TRUSTED_KEYS": p04Registry, "P03_EVIDENCE": p03Evidence, "P03_TRUSTED_KEYS": p03Registry, "AROP_CHECK_COMMAND": "e2e user gate"}, "go", "run", "./internal/tooling/cmd/arop-gate-check")
	for _, phase := range []string{"P03", "P04"} {
		verified, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: "build/reports/" + phase + "/report.json"})
		if err != nil || mode != "current-worktree" || !verified.Success {
			t.Fatalf("%s report did not verify end-to-end mode=%s success=%v err=%v", phase, mode, verified != nil && verified.Success, err)
		}
	}
}

func completePassingResult() map[string]any {
	requirements := make([]any, 14)
	for i := range requirements {
		requirements[i] = map[string]any{"id": fmt.Sprintf("IR-%02d", i+1), "result": "PASS"}
	}
	phases := make([]any, 53)
	for i := range phases {
		phases[i] = map[string]any{"id": fmt.Sprintf("P%02d", i+1), "result": "PASS"}
	}
	return map[string]any{"requirements": requirements, "phases": phases, "must_fix_count": 0, "must_fix": []any{}, "should_fix_count": 0, "should_fix": []any{}, "verdict": "PASS"}
}

func completeIDs() ([]string, []string) {
	requirements := make([]string, 14)
	for i := range requirements {
		requirements[i] = fmt.Sprintf("IR-%02d", i+1)
	}
	phases := make([]string, 53)
	for i := range phases {
		phases[i] = fmt.Sprintf("P%02d", i+1)
	}
	return requirements, phases
}

func signedCompleteEvidence(t *testing.T, kind string, subject map[string]any, identityField, identity string, result map[string]any, gate bool) (map[string]any, ed25519.PublicKey) {
	t.Helper()
	doc := map[string]any{"schema_version": 1, "kind": kind, "subject": subject, identityField: map[string]any{"id": identity}, "result": result, "attested_at": "2026-09-23T01:02:03Z"}
	if gate {
		doc["gate"] = "P04"
	}
	digest, err := CanonicalSummaryDigest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["summary_sha256"] = digest
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalSummary(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["attestation"] = map[string]any{"algorithm": "Ed25519", "key_id": "key-1", "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical))}
	return doc, pub
}

func marshalJSONLine(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func copyFixturePath(t *testing.T, sourceRoot, targetRoot, relative string) {
	t.Helper()
	source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		write(t, targetRoot, relative, string(data))
		return
	}
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetRoot, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
}

func runCommand(t *testing.T, root string, environment map[string]string, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = root
	command.Env = os.Environ()
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, output)
	}
}

func deepCopy(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

func cloneAnyMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var copied map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&copied); err != nil {
		t.Fatal(err)
	}
	return copied
}
