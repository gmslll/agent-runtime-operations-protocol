package publish

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	manifestsdk "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/protocol/manifest"
)

const (
	testCredential    = "test-publisher-credential-do-not-log"
	validManifestJSON = `{"protocol":"arop/v1","kind":"AgentManifest","identity":{"id":"hello.agent","version":"1.2.3","name":"Hello","summary":"Test publication","owner":{"team":"example-team"}},"skills":[{"id":"default","name":"Echo","invoke_modes":["params"],"input_schema":{"type":"object","additionalProperties":false},"output_schema":{"type":"object","additionalProperties":false}}],"execution":{"default_timeout_seconds":30,"max_timeout_seconds":60,"effects":{"level":"none","idempotency":"supported","human_confirmation":false},"capabilities":{"streaming":false,"cancellation":true,"status_query":true}}}`
)

var testETag = func() string {
	digest, err := manifestsdk.Digest([]byte(validManifestJSON))
	if err != nil {
		panic(err)
	}
	return `"` + digest + `"`
}()

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
		if err != nil || len(body) < 4 || string(body[:2]) != "PK" {
			t.Errorf("body is not the validated ZIP, len=%d err=%v", len(body), err)
		}
		writer.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
		writer.Header().Set("ETag", testETag)
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	command, options := testCommand(t, server.URL+"/base")
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
	command, options := testCommand(t, server.URL)
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
			command, options := testCommand(t, server.URL)
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
		{"wrong-strong-etag", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
			w.Header().Set("ETag", `"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`)
			w.WriteHeader(201)
		}},
		{"wrong-version-location", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/9.9.9")
			w.Header().Set("ETag", testETag)
			w.WriteHeader(201)
		}},
		{"duplicate-location", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
			w.Header().Add("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
			w.Header().Set("ETag", testETag)
			w.WriteHeader(201)
		}},
		{"duplicate-etag", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/v1/agent-definitions/hello.agent/versions/1.2.3")
			w.Header().Add("ETag", testETag)
			w.Header().Add("ETag", testETag)
			w.WriteHeader(201)
		}},
		{"wrong-error-media", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(400)
			io.WriteString(w, `{}`)
		}},
		{"duplicate-error-media", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Content-Type", jsonMediaType)
			w.Header().Add("Content-Type", jsonMediaType)
			w.WriteHeader(400)
			io.WriteString(w, typedError("INVALID_PUBLICATION_REQUEST", "validation", false, ""))
		}},
		{"basic-challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Set("WWW-Authenticate", "Basic")
			w.WriteHeader(401)
			io.WriteString(w, typedError("AUTHENTICATION_REQUIRED", "authentication", false, ""))
		}},
		{"duplicate-challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Add("WWW-Authenticate", "Bearer")
			w.Header().Add("WWW-Authenticate", `Bearer realm="second"`)
			w.WriteHeader(401)
			io.WriteString(w, typedError("AUTHENTICATION_REQUIRED", "authentication", false, ""))
		}},
		{"unexpected-challenge", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(403)
			io.WriteString(w, typedError("PUBLICATION_FORBIDDEN", "authorization", false, ""))
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
		{"duplicate-retry-after", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Add("Retry-After", "30")
			w.Header().Add("Retry-After", "31")
			w.WriteHeader(429)
			io.WriteString(w, typedError("RATE_LIMITED", "capacity", true, `,"retry_after_seconds":30`))
		}},
		{"noncanonical-retry-after", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Set("Retry-After", "+30")
			w.WriteHeader(429)
			io.WriteString(w, typedError("RATE_LIMITED", "capacity", true, `,"retry_after_seconds":30`))
		}},
		{"unexpected-retry-after", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", jsonMediaType)
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(400)
			io.WriteString(w, typedError("INVALID_PUBLICATION_REQUEST", "validation", false, ""))
		}},
		{"unexpected-status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(item.handler)
			defer server.Close()
			command, options := testCommand(t, server.URL)
			if _, err := command.Run(context.Background(), options); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func TestRunRejectsRedirectWithoutSecondHop(t *testing.T) {
	t.Parallel()
	var secondHop atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { secondHop.Add(1) }))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		status := status
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assertHeader(t, request, "Authorization", "Bearer "+testCredential)
				assertHeader(t, request, "Idempotency-Key", "publish-request-0001")
				writer.Header().Set("Location", destination.URL)
				writer.WriteHeader(status)
			}))
			defer server.Close()
			command, options := testCommand(t, server.URL)
			command.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return http.DefaultTransport.RoundTrip(request)
			})
			command.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return nil }
			if _, err := command.Run(context.Background(), options); err == nil {
				t.Fatal("redirect accepted")
			}
		})
	}
	if secondHop.Load() != 0 {
		t.Fatalf("redirect made %d second-hop requests", secondHop.Load())
	}
}

func TestBundleCentralAndLocalMetadataMustMatch(t *testing.T) {
	t.Parallel()
	path := writeBundle(t)
	archive, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	forged := append([]byte(nil), archive...)
	eocd := bytes.LastIndex(forged, []byte{'P', 'K', 5, 6})
	if eocd < 0 {
		t.Fatal("EOCD absent")
	}
	central := int(binary.LittleEndian.Uint32(forged[eocd+16 : eocd+20]))
	if central+28 > eocd {
		t.Fatal("central entry absent")
	}
	binary.LittleEndian.PutUint32(forged[central+24:central+28], binary.LittleEndian.Uint32(forged[central+24:central+28])+1)
	if _, err := validateBundleArchive(context.Background(), forged); err == nil {
		t.Fatal("forged central/local size mismatch accepted")
	}
}

func TestBundleRejectsEveryCentralLocalCorruption(t *testing.T) {
	t.Parallel()
	one := bundleBytes(t, map[string]string{"agent-manifest.json": validManifestJSON})
	two := bundleBytes(t, map[string]string{"agent-manifest.json": validManifestJSON, "schemas/unused.json": `{}`})
	tests := []struct {
		name   string
		base   []byte
		mutate func(*testing.T, []byte) []byte
	}{
		{"filename", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			value[layout.local+30] ^= 1
			return value
		}},
		{"flags", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			binary.LittleEndian.PutUint16(value[layout.local+6:layout.local+8], layout.flags^1)
			return value
		}},
		{"method", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			binary.LittleEndian.PutUint16(value[layout.local+8:layout.local+10], layout.method^1)
			return value
		}},
		{"crc", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			binary.LittleEndian.PutUint32(value[layout.central+16:layout.central+20], layout.crc+1)
			return value
		}},
		{"compressed-size", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			binary.LittleEndian.PutUint32(value[layout.central+20:layout.central+24], layout.compressed+1)
			return value
		}},
		{"data-descriptor", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			descriptor := layout.localData + int(layout.compressed)
			if binary.LittleEndian.Uint32(value[descriptor:descriptor+4]) == 0x08074b50 {
				descriptor += 4
			}
			value[descriptor] ^= 1
			return value
		}},
		{"overlap", two, forgeOverlap},
		{"gap", one, addCentralGap},
		{"prefix", one, addZIPPrefix},
		{"trailing", one, func(_ *testing.T, value []byte) []byte { return append(value, 0) }},
		{"central-range", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			size := binary.LittleEndian.Uint32(value[layout.eocd+12 : layout.eocd+16])
			binary.LittleEndian.PutUint32(value[layout.eocd+12:layout.eocd+16], size+1)
			return value
		}},
		{"entry-count", one, func(t *testing.T, value []byte) []byte {
			layout := firstZIPLayout(t, value)
			binary.LittleEndian.PutUint16(value[layout.eocd+8:layout.eocd+10], maxArchiveEntries+1)
			binary.LittleEndian.PutUint16(value[layout.eocd+10:layout.eocd+12], maxArchiveEntries+1)
			return value
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			candidate := test.mutate(t, append([]byte(nil), test.base...))
			if _, err := validateBundleArchive(context.Background(), candidate); err == nil {
				t.Fatalf("%s corruption accepted", test.name)
			}
		})
	}
}

type zipLayout struct {
	eocd, central, local, localData int
	flags, method                   uint16
	crc, compressed                 uint32
}

func firstZIPLayout(t *testing.T, archive []byte) zipLayout {
	t.Helper()
	eocd := bytes.LastIndex(archive, []byte{'P', 'K', 5, 6})
	if eocd < 0 {
		t.Fatal("EOCD absent")
	}
	central := int(binary.LittleEndian.Uint32(archive[eocd+16 : eocd+20]))
	local := int(binary.LittleEndian.Uint32(archive[central+42 : central+46]))
	nameLength := int(binary.LittleEndian.Uint16(archive[local+26 : local+28]))
	extraLength := int(binary.LittleEndian.Uint16(archive[local+28 : local+30]))
	return zipLayout{eocd: eocd, central: central, local: local, localData: local + 30 + nameLength + extraLength, flags: binary.LittleEndian.Uint16(archive[central+8 : central+10]), method: binary.LittleEndian.Uint16(archive[central+10 : central+12]), crc: binary.LittleEndian.Uint32(archive[central+16 : central+20]), compressed: binary.LittleEndian.Uint32(archive[central+20 : central+24])}
}

func forgeOverlap(t *testing.T, archive []byte) []byte {
	t.Helper()
	layout := firstZIPLayout(t, archive)
	nameLength := int(binary.LittleEndian.Uint16(archive[layout.central+28 : layout.central+30]))
	extraLength := int(binary.LittleEndian.Uint16(archive[layout.central+30 : layout.central+32]))
	commentLength := int(binary.LittleEndian.Uint16(archive[layout.central+32 : layout.central+34]))
	second := layout.central + 46 + nameLength + extraLength + commentLength
	if second+46 > layout.eocd {
		t.Fatal("second central entry absent")
	}
	binary.LittleEndian.PutUint32(archive[second+42:second+46], uint32(layout.local))
	return archive
}

func addCentralGap(t *testing.T, archive []byte) []byte {
	t.Helper()
	layout := firstZIPLayout(t, archive)
	result := make([]byte, 0, len(archive)+1)
	result = append(result, archive[:layout.central]...)
	result = append(result, 0)
	result = append(result, archive[layout.central:]...)
	newEOCD := layout.eocd + 1
	binary.LittleEndian.PutUint32(result[newEOCD+16:newEOCD+20], uint32(layout.central+1))
	return result
}

func addZIPPrefix(t *testing.T, archive []byte) []byte {
	t.Helper()
	result := append([]byte{0}, archive...)
	old := firstZIPLayout(t, archive)
	newEOCD := old.eocd + 1
	newCentral := old.central + 1
	binary.LittleEndian.PutUint32(result[newEOCD+16:newEOCD+20], uint32(newCentral))
	position := newCentral
	count := int(binary.LittleEndian.Uint16(result[newEOCD+10 : newEOCD+12]))
	for range count {
		local := binary.LittleEndian.Uint32(result[position+42 : position+46])
		binary.LittleEndian.PutUint32(result[position+42:position+46], local+1)
		position += 46 + int(binary.LittleEndian.Uint16(result[position+28:position+30])) + int(binary.LittleEndian.Uint16(result[position+30:position+32])) + int(binary.LittleEndian.Uint16(result[position+32:position+34]))
	}
	return result
}

func TestBoundedReadArchiveLimits(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{maxArchiveBytes - 1, maxArchiveBytes} {
		data, err := readBounded(context.Background(), io.LimitReader(zeroReader{}, size), maxArchiveBytes)
		if err != nil || int64(len(data)) != size {
			t.Fatalf("size %d: len=%d err=%v", size, len(data), err)
		}
	}
	if _, err := readBounded(context.Background(), io.LimitReader(zeroReader{}, maxArchiveBytes+1), maxArchiveBytes); err == nil {
		t.Fatal("archive limit+1 accepted")
	}
}

func TestLoadAndValidateBundleFileSizeBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		size int64
		want error
	}{
		{"limit-minus-one", maxArchiveBytes - 1, errBundleInvalid},
		{"limit", maxArchiveBytes, errBundleInvalid},
		{"limit-plus-one", maxArchiveBytes + 1, errBundleUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bundle.zip")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(test.size); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			_, _, err = loadAndValidateBundle(context.Background(), path)
			if !errors.Is(err, test.want) {
				t.Fatalf("size %d error=%v want=%v", test.size, err, test.want)
			}
		})
	}
}

func TestLoadAndValidateBundleRejectsGrowthAfterOpen(t *testing.T) {
	t.Parallel()
	path := writeBundle(t)
	_, _, err := loadAndValidateBundleWithHook(context.Background(), path, func() error {
		return os.Truncate(path, maxArchiveBytes+1)
	})
	if !errors.Is(err, errBundleUnavailable) {
		t.Fatalf("growing file error=%v", err)
	}
}

func TestLoadAndValidateBundleRejectsAtomicPathReplacement(t *testing.T) {
	t.Parallel()
	path := writeBundle(t)
	replacement := filepath.Join(filepath.Dir(path), "replacement.zip")
	if err := os.WriteFile(replacement, bundleBytes(t, map[string]string{"agent-manifest.json": validManifestJSON}), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadAndValidateBundleWithHook(context.Background(), path, func() error {
		return os.Rename(replacement, path)
	})
	if !errors.Is(err, errBundleUnavailable) || strings.Contains(err.Error(), path) {
		t.Fatalf("atomic replacement error=%v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = 0
	}
	return len(buffer), nil
}

func TestBundleProcessingHonorsCancellation(t *testing.T) {
	t.Parallel()
	archive, err := os.ReadFile(writeBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateBundleArchive(ctx, archive); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestCentralDirectoryLoopHonorsCancellation(t *testing.T) {
	t.Parallel()
	archive := bundleBytes(t, map[string]string{"agent-manifest.json": validManifestJSON, "schemas/unused.json": `{}`})
	ctx := &cancelOnSecondErrContext{}
	if err := verifyCentralDirectoryExact(ctx, archive); !errors.Is(err, context.Canceled) {
		t.Fatalf("directory loop cancellation error=%v", err)
	}
}

type cancelOnSecondErrContext struct{ checks atomic.Int32 }

func (*cancelOnSecondErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*cancelOnSecondErrContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelOnSecondErrContext) Err() error {
	if ctx.checks.Add(1) >= 2 {
		return context.Canceled
	}
	return nil
}
func (*cancelOnSecondErrContext) Value(any) any { return nil }

func TestActualArchiveReadCancelsMidStream(t *testing.T) {
	t.Parallel()
	file, err := os.Open(writeBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterFirstRead{Reader: file, cancel: cancel}
	if _, err := readBounded(ctx, reader, maxArchiveBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-read cancel error=%v", err)
	}
}

type cancelAfterFirstRead struct {
	io.Reader
	cancel context.CancelFunc
	done   bool
}

func (reader *cancelAfterFirstRead) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	if !reader.done {
		reader.done = true
		reader.cancel()
	}
	return count, err
}

func TestValidateToDigestCancellationBarrier(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "arop-publish-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.WriteFile(filepath.Join(root, "agent-manifest.json"), []byte(validManifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	var digestCalls atomic.Int32
	if _, err := validateAndDigestManifestWith(alwaysCancelledContext{}, root, func(string, string) (string, error) {
		digestCalls.Add(1)
		return "must-not-run", nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("validate-to-digest barrier error=%v", err)
	}
	if digestCalls.Load() != 0 {
		t.Fatalf("digest called %d times after cancellation", digestCalls.Load())
	}
}

type alwaysCancelledContext struct{}

func (alwaysCancelledContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (alwaysCancelledContext) Done() <-chan struct{} {
	channel := make(chan struct{})
	close(channel)
	return channel
}
func (alwaysCancelledContext) Err() error    { return context.Canceled }
func (alwaysCancelledContext) Value(any) any { return nil }

func TestRunRejectsInvalidBundleInputsBeforeNetwork(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	base, _ := url.Parse("https://control.example.invalid")
	command := Command{BaseURL: base, Client: clientFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("called") }), Stdout: io.Discard, Credential: CredentialSourceFunc(func(context.Context) (string, error) { return testCredential, nil })}
	root := t.TempDir()
	regular := filepath.Join(root, "not-a-zip-AROP_LOCAL_PATH_CANARY")
	if err := os.WriteFile(regular, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "bundle-link")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, symlink, regular, filepath.Join(root, "missing-AROP_LOCAL_PATH_CANARY")} {
		_, err := command.Run(context.Background(), Options{AgentID: "hello.agent", BundlePath: path, IdempotencyKey: "publish-request-0001"})
		if err == nil || strings.Contains(err.Error(), "AROP_LOCAL_PATH_CANARY") || strings.Contains(RedactedError(err), "AROP_LOCAL_PATH_CANARY") {
			t.Fatalf("path %q produced unsafe error %v", path, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d", calls.Load())
	}
}

func TestRunRejectsBundleIdentityMismatchBeforeNetwork(t *testing.T) {
	t.Parallel()
	command, options := testCommand(t, "https://control.example.invalid")
	var calls atomic.Int32
	command.Client = clientFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("called") })
	options.AgentID = "another.agent"
	if _, err := command.Run(context.Background(), options); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("identity mismatch reached network")
	}
}

func TestRunRedactsTransportAndHandlesNilResponses(t *testing.T) {
	t.Parallel()
	for _, item := range []struct {
		name string
		do   *http.Client
	}{
		{"transport-error", clientFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("AROP_TRANSPORT_CANARY " + testCredential + " https://secret.invalid")
		})},
		{"nil-response", clientFunc(func(*http.Request) (*http.Response, error) { return nil, nil })},
		{"nil-body", clientFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 201, Header: make(http.Header), Request: request}, nil
		})},
	} {
		t.Run(item.name, func(t *testing.T) {
			command, options := testCommand(t, "https://control.example.invalid")
			command.Client = item.do
			_, err := command.Run(context.Background(), options)
			if err == nil || strings.Contains(err.Error(), "AROP_TRANSPORT_CANARY") || strings.Contains(err.Error(), testCredential) || strings.Contains(err.Error(), "secret.invalid") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestRunClosesResponseReturnedWithTransportError(t *testing.T) {
	t.Parallel()
	closed := &atomic.Bool{}
	command, options := testCommand(t, "https://control.example.invalid")
	command.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: closeTracker{Reader: strings.NewReader("ignored"), closed: closed}, Request: request}, errors.New("AROP_TRANSPORT_CANARY")
	})
	if _, err := command.Run(context.Background(), options); err == nil {
		t.Fatal("transport error accepted")
	}
	if !closed.Load() {
		t.Fatal("response body returned with error was not closed")
	}
}

type closeTracker struct {
	io.Reader
	closed *atomic.Bool
}

func (tracker closeTracker) Close() error { tracker.closed.Store(true); return nil }

func TestRunHonorsCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	command, options := testCommand(t, "https://control.example.invalid")
	command.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
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

	var roundTrips atomic.Int32
	command, options = testCommand(t, "https://control.example.invalid")
	command.Client = &http.Client{Timeout: 20 * time.Millisecond, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		deadline, ok := request.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Millisecond {
			return nil, errors.New("AROP_TIMEOUT_DEADLINE_CANARY")
		}
		<-request.Context().Done()
		return nil, errors.New("AROP_TIMEOUT_TRANSPORT_CANARY")
	})}
	if _, err := command.Run(context.Background(), options); !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), testCredential) {
		t.Fatalf("client timeout error = %v", err)
	}
	if roundTrips.Load() != 1 {
		t.Fatalf("client timeout made %d transport hops", roundTrips.Load())
	}
}

func typedError(code, category string, retryable bool, suffix string) string {
	return fmt.Sprintf(`{"code":%q,"category":%q,"message":"safe","retryable":%t%s}`, code, category, retryable, suffix)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func clientFunc(roundTrip roundTripFunc) *http.Client { return &http.Client{Transport: roundTrip} }

func TestRunRejectsCredentialAndInputInjection(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://control.example.invalid")
	path := writeBundle(t)
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
	options := Options{AgentID: "hello.agent", BundlePath: writeBundle(t), IdempotencyKey: "publish-request-0001"}
	_, err := command.Run(context.Background(), options)
	if err == nil || strings.Contains(err.Error(), testCredential) || strings.Contains(RedactedError(err), testCredential) {
		t.Fatalf("credential source error was not redacted: %v", err)
	}
}

func testCommand(t *testing.T, rawURL string) (Command, Options) {
	t.Helper()
	base, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	path := writeBundle(t)
	command := Command{
		BaseURL:    base,
		Client:     &http.Client{Transport: http.DefaultTransport},
		Credential: CredentialSourceFunc(func(context.Context) (string, error) { return testCredential, nil }),
		Stdout:     io.Discard,
	}
	return command, Options{AgentID: "hello.agent", BundlePath: path, IdempotencyKey: "publish-request-0001"}
}

func writeBundle(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := os.WriteFile(path, bundleBytes(t, map[string]string{"agent-manifest.json": validManifestJSON}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func bundleBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
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
