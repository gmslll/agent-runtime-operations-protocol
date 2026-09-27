package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOptionsRequireTLSOrExplicitLoopback(t *testing.T) {
	base := options{listen: "127.0.0.1:8443", database: "/tmp/provider.db", issuer: "https://control.example.invalid", audience: "https://control.example.invalid/deployments/dep_x", endpoint: "https://runtime.example.invalid/v1/runs", deployment: "dep_x", instance: "runtime-a", transport: "direct", keyFile: "/tmp/keys.json", generation: 1}
	if err := base.validate(); err == nil {
		t.Fatal("missing TLS configuration accepted")
	}
	base.insecureLoopback = true
	if err := base.validate(); err != nil {
		t.Fatalf("explicit loopback mode rejected: %v", err)
	}
	base.listen = "0.0.0.0:8443"
	if err := base.validate(); err == nil {
		t.Fatal("cleartext non-loopback listener accepted")
	}
}

func TestLoadKeysRejectsSymlinkAndParsesStrictMetadata(t *testing.T) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value *big.Int) string {
		buffer := make([]byte, 32)
		value.FillBytes(buffer)
		return base64.RawURLEncoding.EncodeToString(buffer)
	}
	now := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	document := map[string]any{"keys": []map[string]any{{"kid": "dispatch-key-01", "x": encode(private.X), "y": encode(private.Y), "not_before": now.Add(-time.Hour).Format(time.RFC3339Nano), "sign_until": now.Add(time.Hour).Format(time.RFC3339Nano), "verify_until": now.Add(2 * time.Hour).Format(time.RFC3339Nano)}}}
	encoded, _ := json.Marshal(document)
	path := filepath.Join(t.TempDir(), "keys.json")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := loadKeys(path)
	if err != nil || len(keys) != 1 {
		t.Fatalf("valid public keys rejected: %#v %v", keys, err)
	}
	link := filepath.Join(filepath.Dir(path), "keys-link.json")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = loadKeys(link); err == nil {
		t.Fatal("symlinked key file accepted")
	}
}
