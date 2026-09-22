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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

var PlanningInputs = []struct{ Path, Field string }{{"spec/requirements.yaml", "requirements_sha256"}, {"docs/DEVELOPMENT_PLAN.md", "plan_sha256"}, {"docs/IMPLEMENTATION_BLUEPRINT.md", "blueprint_sha256"}, {"spec/artifact-manifest.yaml", "artifact_manifest_sha256"}}
var SummaryFields = map[string][]string{"arop-planning-audit": {"schema_version", "kind", "subject", "reviewer", "result", "attested_at"}, "arop-user-gate": {"schema_version", "kind", "gate", "subject", "approver", "result", "attested_at"}}

func hash(v []byte) string { s := sha256.Sum256(v); return hex.EncodeToString(s[:]) }
func git(root string, args ...string) ([]byte, error) {
	c := exec.Command("git", args...)
	c.Dir = root
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
		c := exec.Command("git", "merge-base", "--is-ancestor", commit, head)
		c.Dir = root
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
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func Authenticate(root string, e map[string]any, role, keysPath, confirmationPath string) (map[string]any, error) {
	canonical, err := CanonicalSummary(e)
	if err != nil {
		return nil, err
	}
	if keysPath != "" {
		abs, _ := filepath.Abs(keysPath)
		rel, _ := filepath.Rel(root, abs)
		if !strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("TRUSTED_KEYS registry must remain outside the Git repository")
		}
		var registry map[string]any
		if err := structuredfile.Load(abs, &registry); err != nil {
			return nil, err
		}
		att, _ := e["attestation"].(map[string]any)
		keyID, _ := att["key_id"].(string)
		var selected map[string]any
		keys, _ := registry["keys"].([]any)
		for _, raw := range keys {
			k, _ := raw.(map[string]any)
			if k["key_id"] == keyID {
				if selected != nil {
					return nil, fmt.Errorf("trusted key %s must resolve exactly once", keyID)
				}
				selected = k
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("trusted key %s must resolve exactly once", keyID)
		}
		if selected["role"] != role {
			return nil, fmt.Errorf("trusted key role must be %s", role)
		}
		block, _ := pem.Decode([]byte(fmt.Sprint(selected["public_key_pem"])))
		if block == nil {
			return nil, fmt.Errorf("trusted key public key is invalid")
		}
		pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := pubAny.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("trusted key is not Ed25519")
		}
		sig, err := base64.StdEncoding.DecodeString(fmt.Sprint(att["signature"]))
		if err != nil || !ed25519.Verify(pub, canonical, sig) {
			return nil, fmt.Errorf("Ed25519 signature does not verify against canonical summary")
		}
		return map[string]any{"mode": "trusted-key-attestation", "key_id": keyID, "key_role": role}, nil
	}
	if confirmationPath == "" {
		return nil, fmt.Errorf("authenticity requires TRUSTED_KEYS or TRUSTED_CHANNEL_CONFIRMATION")
	}
	abs, _ := filepath.Abs(confirmationPath)
	rel, _ := filepath.Rel(root, abs)
	if !strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("TRUSTED_CHANNEL_CONFIRMATION must remain outside the Git repository")
	}
	raw, err := os.ReadFile(abs)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("trusted-channel confirmation record is empty or unreadable")
	}
	return map[string]any{"mode": "manual-trusted-channel", "confirmation_sha256": "sha256:" + hash(raw)}, nil
}
