package register

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registrysdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/registry"
)

const validConfig = `{"schema_version":1,"session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","service_id":"image-runtime","environment":"production","endpoint":{"base_url":"https://runtime.internal.example","health_path":"/v1/health/ready"},"bindings":[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"runtime":{"healthy":true,"ready":true,"runtime_version":"2026.09.26","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":1,"available_slots":1,"queue_depth":0},"labels":{"region":"cn-east"}}}`

const validInstance = `{"schema_version":1,"instance_id":"runtime-a","session_id":"ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","service_id":"image-runtime","environment":"production","generation":1,"resource_version":1,"registry_revision":1,"lease_id":"lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c","lease_expires_at":"2026-09-26T08:00:30Z","endpoint":{"base_url":"https://runtime.internal.example","health_path":"/v1/health/ready"},"bindings":[{"agent_id":"image.generate","agent_version":"1.0.0","skill_ids":["default"],"manifest_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"runtime":{"healthy":true,"ready":true,"runtime_version":"2026.09.26","protocol_versions":["1.0"],"transport_profiles":["direct"],"capabilities":{"streaming":true,"stream_resume":true,"cancellation":true,"status_query":true,"event_outbox":"durable"},"capacity":{"max_concurrency":2,"max_queue_depth":4,"active_runs":1,"available_slots":1,"queue_depth":0},"labels":{"region":"cn-east"}},"operator":{"enabled":true,"weight":100,"priority":10},"draining":false,"status":"registered"}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCommandRegistersAndPrintsOnlyPublicLeaseMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registration.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://control.example.invalid")
	secret := "token-that-must-not-appear"
	client := registrysdk.Client{BaseURL: base, HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer "+secret || request.Header.Get("Idempotency-Key") != "register-0001" {
			t.Fatalf("unexpected registry headers %#v", request.Header)
		}
		body := `{"schema_version":1,"instance":` + validInstance + `,"lease_ttl_seconds":30,"keepalive_interval_seconds":10,"replay":false}`
		return &http.Response{StatusCode: http.StatusCreated, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}, Credential: registrysdk.CredentialSourceFunc(func(context.Context) (string, error) { return secret, nil })}
	var output bytes.Buffer
	err := (Command{Client: client, Stdout: &output}).Execute(context.Background(), Options{InstanceID: "runtime-a", ConfigPath: path, IdempotencyKey: "register-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || output.String() != "registered instance=runtime-a session=ses_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c generation=1 lease=lease_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c ttl=30s keepalive=10s replay=false\n" {
		t.Fatalf("unexpected output %q", output.String())
	}
}

func TestCommandRejectsSymlinkUnknownFieldsAndCancellationBeforeNetwork(t *testing.T) {
	directory := t.TempDir()
	realPath := filepath.Join(directory, "real.json")
	if err := os.WriteFile(realPath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "link.json")
	if err := os.Symlink(realPath, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(symlink); err == nil {
		t.Fatal("symlink config accepted")
	}
	unknownPath := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknownPath, []byte(strings.TrimSuffix(validConfig, "}")+`,"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(unknownPath); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unknown field was accepted or leaked: %v", err)
	}

	base, _ := url.Parse("https://control.example.invalid")
	client := registrysdk.Client{BaseURL: base, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network must not run")
	})}, Credential: registrysdk.CredentialSourceFunc(func(context.Context) (string, error) { return "token", nil })}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (Command{Client: client, Stdout: io.Discard}).Execute(ctx, Options{InstanceID: "runtime-a", ConfigPath: realPath, IdempotencyKey: "register-0001"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled command err=%v", err)
	}
}

func TestReadConfigRejectsOversizeAndTrailingJSON(t *testing.T) {
	directory := t.TempDir()
	oversize := filepath.Join(directory, "oversize.json")
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("x"), maxConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(oversize); err == nil {
		t.Fatal("oversize config accepted")
	}
	trailing := filepath.Join(directory, "trailing.json")
	if err := os.WriteFile(trailing, []byte(validConfig+` {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(trailing); err == nil {
		t.Fatal("trailing document accepted")
	}
}
