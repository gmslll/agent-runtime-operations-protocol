package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	registry := map[string]any{"schema_version": 1, "keys": []any{map[string]any{"key_id": "key-1", "algorithm": "Ed25519", "role": "independent_reviewer", "public_key_pem": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}}
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
