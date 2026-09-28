// Command driver is the black-box Reference Control Plane implementation of
// the portable AROP conformance driver protocol. The root runner owns scenario
// selection and report generation; this nested-module binary only verifies an
// invocation and exercises one already-running server over HTTP.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	driverProtocol = "arop-conformance-driver/v1"
	maxInputBytes  = 20 << 20
	maxBodyBytes   = 1 << 20
)

// endpoint and backend are supplied by the P34 harness through -ldflags. They
// deliberately contain no database DSN, credential, key, or filesystem path.
var (
	endpoint string
	backend  string
)

type invocationFixture struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	Bytes      int64  `json:"bytes"`
	DataBase64 string `json:"data_base64"`
}

type invocation struct {
	Protocol   string            `json:"protocol"`
	ScenarioID string            `json:"scenario_id"`
	Request    map[string]any    `json:"request"`
	Fixture    invocationFixture `json:"fixture"`
}

type response struct {
	Protocol   string `json:"protocol"`
	ScenarioID string `json:"scenario_id"`
	Outcome    string `json:"outcome"`
}

type healthResponse struct {
	Status     string          `json:"status"`
	Service    string          `json:"service"`
	Version    string          `json:"version"`
	Scope      string          `json:"scope"`
	Durability string          `json:"durability"`
	Checks     json.RawMessage `json:"checks,omitempty"`
}

type errorResponse struct {
	Category  string `json:"category"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func main() {
	if runErr := runMain(); runErr != nil {
		os.Exit(1)
	}
}

func runMain() error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, maxInputBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxInputBytes {
		return errors.New("invalid conformance invocation")
	}
	var request invocation
	if err := decodeStrict(data, &request); err != nil {
		return err
	}
	result := response{Protocol: driverProtocol, ScenarioID: request.ScenarioID, Outcome: "fail"}
	err = executeBytes(context.Background(), request, endpoint, backend)
	if err == nil {
		result.Outcome = "pass"
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		return encodeErr
	}
	return err
}

// execute is retained as a small test seam for malformed-reader cases.
func execute(ctx context.Context, input io.Reader, serverEndpoint, storageBackend string) error {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxInputBytes {
		return errors.New("invalid conformance invocation")
	}
	var request invocation
	if err := decodeStrict(data, &request); err != nil {
		return err
	}
	return executeBytes(ctx, request, serverEndpoint, storageBackend)
}

func executeBytes(ctx context.Context, request invocation, serverEndpoint, storageBackend string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Protocol != driverProtocol || request.ScenarioID == "" || len(request.Request) == 0 || request.Fixture.Path == "" || request.Fixture.MediaType != "application/json" {
		return errors.New("invalid conformance invocation identity")
	}
	allowed := map[string]bool{"core.manifest.valid": true, "core.event-envelope.valid": true, "core.trace-context.vectors": true}
	if !allowed[request.ScenarioID] {
		return errors.New("unsupported conformance scenario")
	}
	fixture, err := base64.StdEncoding.Strict().DecodeString(request.Fixture.DataBase64)
	if err != nil || request.Fixture.Bytes <= 0 || int64(len(fixture)) != request.Fixture.Bytes || len(fixture) > maxInputBytes {
		return errors.New("invalid conformance fixture")
	}
	sum := sha256.Sum256(fixture)
	if request.Fixture.SHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return errors.New("conformance fixture digest mismatch")
	}
	if storageBackend != "sqlite" && storageBackend != "postgres" {
		return errors.New("unsupported conformance storage backend")
	}
	base, err := validateEndpoint(serverEndpoint)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, probe := range []struct{ path, status string }{{"/v1/health/live", "ok"}, {"/v1/health/ready", "ready"}} {
		if err := probeHealth(ctx, client, base, probe.path, probe.status); err != nil {
			return err
		}
	}
	return probeAuthentication(ctx, client, base)
}

func validateEndpoint(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("conformance endpoint must be a canonical loopback HTTP origin")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || port == "" {
		return nil, errors.New("conformance endpoint must include an explicit port")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("conformance endpoint must be loopback-only")
	}
	return parsed, nil
}

func probeHealth(ctx context.Context, client *http.Client, base *url.URL, path, expected string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+path, nil)
	if err != nil {
		return errors.New("build health probe")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("execute health probe")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return errors.New("read health response")
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Content-Type") != "application/json" {
		return errors.New("health response contract mismatch")
	}
	var health healthResponse
	if err := decodeStrict(body, &health); err != nil {
		return errors.New("decode health response")
	}
	if health.Status != expected || health.Service != "arop-reference-control-plane" || health.Version == "" || health.Scope != "platform-bootstrap" || health.Durability != "durable" {
		return errors.New("health response identity mismatch")
	}
	if path == "/v1/health/ready" && (len(health.Checks) == 0 || bytes.Equal(health.Checks, []byte("null"))) {
		return errors.New("readiness checks are missing")
	}
	return nil
}

func probeAuthentication(ctx context.Context, client *http.Client, base *url.URL) error {
	target := base.String() + "/v1/discovery/agents/conformance.agent/instances?version=1.0.0&skill_id=default&protocol_version=1.0"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("build authentication probe")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("execute authentication probe")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return errors.New("read authentication response")
	}
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("WWW-Authenticate") != "Bearer" {
		return errors.New("authentication boundary mismatch")
	}
	var rejection errorResponse
	if err := decodeStrict(body, &rejection); err != nil || rejection.Category != "authentication" || rejection.Code != "AUTHENTICATION_REQUIRED" || rejection.Message != rejection.Code || rejection.Retryable {
		return errors.New("authentication error contract mismatch")
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON document")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON document has trailing content")
	}
	return nil
}
