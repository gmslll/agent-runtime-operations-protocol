package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testCredential = "test-publisher-credential-do-not-log"
	testETag       = `"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`
)

func TestRunPublishesExactRequestAndStableReplay(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.EscapedPath() != "/base/v1/agent-definitions/hello.agent/versions" {
			t.Errorf("request = %s %s", request.Method, request.URL.EscapedPath())
		}
		assertHeader(t, request, "Authorization", "Bearer "+testCredential)
		assertHeader(t, request, "Content-Type", bundleMediaType)
		assertHeader(t, request, "Accept", jsonMediaType)
		assertHeader(t, request, "Idempotency-Key", "publish-request-0001")
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != "PK-test-bundle" {
			t.Errorf("body = %q, err = %v", body, err)
		}
		writer.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
		writer.Header().Set("ETag", testETag)
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	command, options := testCommand(t, server.URL+"/base", []byte("PK-test-bundle"))
	for range 2 {
		result, err := command.Run(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if result.Location != "/v1/agent-definitions/hello.agent/versions/1.2.3" || result.ETag != testETag {
			t.Fatalf("result = %#v", result)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestExecuteWritesOnlySafeResult(t *testing.T) {
	t.Parallel()
	server := successServer()
	defer server.Close()
	command, options := testCommand(t, server.URL, []byte("bundle"))
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Execute(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	if strings.Contains(output, testCredential) || !strings.Contains(output, testETag) {
		t.Fatalf("unsafe or incomplete output %q", output)
	}
}

func TestRunDecodesTypedPublicationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status         int
		code, category string
		retryable      bool
		headers        map[string]string
	}{
		{400, "INVALID_PUBLICATION_REQUEST", "validation", false, nil},
		{401, "AUTHENTICATION_REQUIRED", "authentication", false, map[string]string{"WWW-Authenticate": "Bearer"}},
		{403, "PUBLICATION_FORBIDDEN", "authorization", false, nil},
		{409, "AGENT_VERSION_CONFLICT", "conflict", false, nil},
		{413, "BUNDLE_TOO_LARGE", "capacity", false, nil},
		{415, "UNSUPPORTED_MEDIA_TYPE", "validation", false, nil},
		{429, "RATE_LIMITED", "capacity", true, map[string]string{"Retry-After": "30"}},
		{503, "DEPENDENCY_UNAVAILABLE", "dependency", true, map[string]string{"Retry-After": "10"}},
	}
	for _, item := range cases {
		item := item
		t.Run(fmt.Sprint(item.status), func(t *testing.T) {
			t.Parallel()
			retry := ""
			if value := item.headers["Retry-After"]; value != "" {
				retry = `,"retry_after_seconds":` + value
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", jsonMediaType)
				for name, value := range item.headers {
					writer.Header().Set(name, value)
				}
				writer.WriteHeader(item.status)
				fmt.Fprintf(writer, `{"code":%q,"category":%q,"message":"server detail %s","retryable":%t%s}`, item.code, item.category, testCredential, item.retryable, retry)
			}))
			defer server.Close()
			command, options := testCommand(t, server.URL, []byte("bundle"))
			_, err := command.Run(context.Background(), options)
			var remote *RemoteError
			if !errors.As(err, &remote) || remote.StatusCode != item.status || remote.Wire.Code != item.code {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), testCredential) || strings.Contains(RedactedError(err), testCredential) {
				t.Fatal("remote message leaked through error output")
			}
		})
	}
}

func TestRunRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"success-body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/agent-definitions/a/versions/1.0.0")
			w.Header().Set("ETag", testETag)
			w.WriteHeader(201)
			io.WriteString(w, "x")
		}},
		{"absolute-location", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.invalid/v1/agent-definitions/a/versions/1.0.0")
			w.Header().Set("ETag", testETag)
			w.WriteHeader(201)
		}},
		{"weak-etag", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/agent-definitions/a/versions/1.0.0")
			w.Header().Set("ETag", `W/`+testETag)
			w.WriteHeader(201)
		}},
		{"wrong-error-media", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(400)
			io.WriteString(w, `{}`)
		}},
		{"mismatched-error", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"AGENT_VERSION_CONFLICT","category":"conflict","message":"x","retryable":false}`)
		}},
		{"missing-retry-after", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.WriteHeader(429)
			io.WriteString(w, `{"code":"RATE_LIMITED","category":"capacity","message":"x","retryable":true,"retry_after_seconds":30}`)
		}},
		{"unexpected-status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(item.handler)
			defer server.Close()
			command, options := testCommand(t, server.URL, []byte("bundle"))
			if _, err := command.Run(context.Background(), options); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestRunHonorsCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	command, options := testCommand(t, "https://control.example.invalid", []byte("bundle"))
	command.Client = doerFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := command.Run(cancelled, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}

	timed, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := command.Run(timed, options); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (do doerFunc) Do(request *http.Request) (*http.Response, error) { return do(request) }

func TestRunRejectsCredentialAndInputInjection(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://control.example.invalid")
	path := writeBundle(t, []byte("bundle"))
	command := Command{BaseURL: base, Client: http.DefaultClient, Stdout: io.Discard, Credential: CredentialSourceFunc(func(context.Context) (string, error) { return "secret\r\nInjected: true", nil })}
	options := Options{AgentID: "hello.agent", BundlePath: path, IdempotencyKey: "publish-request-0001"}
	if _, err := command.Run(context.Background(), options); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("credential failure = %v", err)
	}
	command.Credential = CredentialSourceFunc(func(context.Context) (string, error) { return testCredential, nil })
	for _, mutate := range []func(*Options){
		func(o *Options) { o.AgentID = "../admin" },
		func(o *Options) { o.IdempotencyKey = "bad key" },
	} {
		candidate := options
		mutate(&candidate)
		if _, err := command.Run(context.Background(), candidate); err == nil {
			t.Fatal("expected validation failure")
		}
	}
}

func TestRunRedactsCredentialSourceFailure(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://control.example.invalid")
	command := Command{
		BaseURL: base,
		Client:  http.DefaultClient,
		Stdout:  io.Discard,
		Credential: CredentialSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("provider exposed " + testCredential)
		}),
	}
	options := Options{AgentID: "hello.agent", BundlePath: writeBundle(t, []byte("bundle")), IdempotencyKey: "publish-request-0001"}
	_, err := command.Run(context.Background(), options)
	if err == nil || strings.Contains(err.Error(), testCredential) || strings.Contains(RedactedError(err), testCredential) {
		t.Fatalf("credential source error was not redacted: %v", err)
	}
}

func testCommand(t *testing.T, rawURL string, bundle []byte) (Command, Options) {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	path := writeBundle(t, bundle)
	command := Command{
		BaseURL:    base,
		Client:     http.DefaultClient,
		Credential: CredentialSourceFunc(func(context.Context) (string, error) { return testCredential, nil }),
		Stdout:     io.Discard,
	}
	return command, Options{AgentID: "hello.agent", BundlePath: path, IdempotencyKey: "publish-request-0001"}
}

func writeBundle(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func successServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
		writer.Header().Set("ETag", testETag)
		writer.WriteHeader(http.StatusCreated)
	}))
}

func assertHeader(t *testing.T, request *http.Request, name, want string) {
	t.Helper()
	if got := request.Header.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}
