package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

func TestClientClaimCompleteAndNoRedirect(t *testing.T) {
	claimJSON, err := os.ReadFile("../../../conformance/fixtures/worker/claim.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	completeJSON, err := os.ReadFile("../../../conformance/fixtures/worker/complete.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := workerwire.DecodeWorkerComplete(completeJSON)
	if err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token-value" || len(request.Header.Values("Authorization")) != 1 {
			t.Errorf("credential header mismatch")
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "claims:next"):
			response.Header().Set("Content-Type", "application/json")
			response.Header().Set("Cache-Control", "no-store")
			_, _ = response.Write(claimJSON)
		case strings.HasSuffix(request.URL.Path, ":complete"):
			if request.Header.Get("Idempotency-Key") != "complete-test-key" {
				t.Errorf("idempotency key mismatch")
			}
			data, _ := io.ReadAll(request.Body)
			if _, err := workerwire.DecodeWorkerComplete(data); err != nil {
				t.Errorf("completion body: %v", err)
			}
			response.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(request.URL.Path, ":release"):
			response.Header().Set("Location", target.URL)
			response.WriteHeader(http.StatusTemporaryRedirect)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, HTTPClient: server.Client(), Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "token-value", nil })})
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := client.Claim(context.Background(), "worker-a", workerwire.WorkerClaimRequest{SchemaVersion: 1, SessionID: "ses_018f0c00-0000-7000-8000-000000000002", Generation: 7, AvailableSlots: 1, SupportedBindings: []workerwire.AgentBinding{claimBinding()}, WaitSeconds: 0})
	if err != nil || !found || string(claim.ClaimID) == "" {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := client.Complete(context.Background(), "worker-a", string(complete.ClaimID), "complete-test-key", complete); err != nil {
		t.Fatal(err)
	}
	err = client.Release(context.Background(), "worker-a", string(claim.ClaimID), workerwire.WorkerReleaseRequest{SchemaVersion: 1, LeaseToken: claim.LeaseToken, FencingToken: claim.FencingToken, Reason: "worker_shutdown"})
	if err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect followed or accepted: calls=%d err=%v", redirected.Load(), err)
	}
}

func TestClientRejectsAmbiguousAndLeakyResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		head   http.Header
		body   string
	}{
		{name: "duplicate content type", status: 200, head: http.Header{"Content-Type": {"application/json", "application/json"}, "Cache-Control": {"no-store"}}, body: `{}`},
		{name: "oversize", status: 200, head: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, body: strings.Repeat("x", maxWorkerResponseBytes+1)},
		{name: "bad challenge", status: 401, head: http.Header{"Content-Type": {"application/json"}, "WWW-Authenticate": {"Basic"}}, body: validErrorJSON(false, 0)},
		{name: "retry mismatch", status: 503, head: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"2"}}, body: validErrorJSON(true, 3)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				for key, values := range test.head {
					for _, value := range values {
						response.Header().Add(key, value)
					}
				}
				response.WriteHeader(test.status)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewClient(ClientConfig{BaseURL: server.URL, HTTPClient: server.Client(), Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "top-secret", nil })})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = client.Claim(context.Background(), "worker-a", workerwire.WorkerClaimRequest{SchemaVersion: 1, SessionID: "ses_018f0c00-0000-7000-8000-000000000002", Generation: 7, AvailableSlots: 1, SupportedBindings: []workerwire.AgentBinding{claimBinding()}, WaitSeconds: 0})
			if err == nil || strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "remote secret") {
				t.Fatalf("response not rejected safely: %v", err)
			}
		})
	}
}

func TestClientCancellationAndConfiguration(t *testing.T) {
	if _, err := NewClient(ClientConfig{BaseURL: "http://example.com", Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "x", nil })}); err == nil {
		t.Fatal("insecure base URL accepted")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(time.Second) }))
	defer server.Close()
	client, err := NewClient(ClientConfig{BaseURL: server.URL, HTTPClient: server.Client(), Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "token", nil }), RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = client.Claim(ctx, "worker-a", workerwire.WorkerClaimRequest{})
	if err == nil {
		t.Fatal("cancelled claim accepted")
	}
}

func claimBinding() workerwire.AgentBinding {
	return workerwire.AgentBinding{ID: "image.generate", Version: "1.0.0", SkillID: "default", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

func validErrorJSON(retryable bool, retryAfter uint64) string {
	value := map[string]any{"category": "dependency", "code": "DEPENDENCY_UNAVAILABLE", "message": "remote secret", "retryable": retryable}
	if retryAfter != 0 {
		value["retry_after_seconds"] = retryAfter
	}
	data, _ := json.Marshal(value)
	return string(data)
}
