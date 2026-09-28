package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDriverRunsTheSameBlackBoxContractForBothDurableBackends(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/health/live":
			_, _ = writer.Write([]byte(`{"status":"ok","service":"arop-reference-control-plane","version":"test","scope":"platform-bootstrap","durability":"durable"}`))
		case "/v1/health/ready":
			_, _ = writer.Write([]byte(`{"status":"ready","service":"arop-reference-control-plane","version":"test","scope":"platform-bootstrap","durability":"durable","checks":[{"name":"database","ready":true,"reason":"ok"}]}`))
		case "/v1/discovery/agents/conformance.agent/instances":
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"category":"authentication","code":"AUTHENTICATION_REQUIRED","message":"AUTHENTICATION_REQUIRED","retryable":false}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	for _, storage := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"core.manifest.valid", "core.event-envelope.valid", "core.trace-context.vectors"} {
			t.Run(storage+"/"+scenario, func(t *testing.T) {
				if err := executeBytes(context.Background(), validInvocation(scenario), server.URL, storage); err != nil {
					t.Fatalf("execute driver: %v", err)
				}
			})
		}
	}
}

func TestDriverRejectsMalformedInvocationFixtureEndpointAndBackend(t *testing.T) {
	valid := validInvocation("core.manifest.valid")
	cases := []struct {
		name     string
		mutate   func(*invocation)
		endpoint string
		backend  string
	}{
		{"protocol", func(value *invocation) { value.Protocol = "other" }, "http://127.0.0.1:1", "sqlite"},
		{"scenario", func(value *invocation) { value.ScenarioID = "unknown" }, "http://127.0.0.1:1", "sqlite"},
		{"base64", func(value *invocation) { value.Fixture.DataBase64 = "Zh==" }, "http://127.0.0.1:1", "sqlite"},
		{"digest", func(value *invocation) { value.Fixture.SHA256 = "sha256:" + strings.Repeat("0", 64) }, "http://127.0.0.1:1", "sqlite"},
		{"backend", func(*invocation) {}, "http://127.0.0.1:1", "memory"},
		{"remote-endpoint", func(*invocation) {}, "http://example.com:80", "sqlite"},
		{"endpoint-secret", func(*invocation) {}, "http://user:secret@127.0.0.1:80", "sqlite"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := valid

			request.Request = map[string]any{"operation": "manifest.validate"}
			test.mutate(&request)
			if err := executeBytes(context.Background(), request, test.endpoint, test.backend); err == nil {
				t.Fatal("invalid invocation was accepted")
			}
		})
	}

	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if err := execute(context.Background(), strings.NewReader(string(data)), "http://127.0.0.1:1", "sqlite"); err == nil {
		t.Fatal("unknown invocation field was accepted")
	}
}

func TestDriverFailsClosedOnHealthOrAuthenticationContractDrift(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{"missing-security-header", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{}`))
		})},
		{"redirect", http.RedirectHandler("http://127.0.0.1:1", http.StatusFound)},
		{"oversized", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(strings.Repeat("x", maxBodyBytes+1)))
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			if err := executeBytes(context.Background(), validInvocation("core.manifest.valid"), server.URL, "sqlite"); err == nil {
				t.Fatal("drifted server contract was accepted")
			}
		})
	}
}

func validInvocation(scenario string) invocation {
	fixture := []byte(`{"schema_version":1}`)
	sum := sha256.Sum256(fixture)
	return invocation{
		Protocol: driverProtocol, ScenarioID: scenario,
		Request: map[string]any{"operation": "manifest.validate"},
		Fixture: invocationFixture{Path: "fixture.json", MediaType: "application/json", Bytes: int64(len(fixture)), SHA256: "sha256:" + hex.EncodeToString(sum[:]), DataBase64: base64.StdEncoding.EncodeToString(fixture)},
	}
}
