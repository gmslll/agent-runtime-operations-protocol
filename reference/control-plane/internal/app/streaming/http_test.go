package streaming

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

type callerProvider struct{ ok bool }

func (provider callerProvider) Caller(*http.Request) (run.Caller, platform.RequestMetadata, bool) {
	return request(0).Caller, platform.RequestMetadata{}, provider.ok
}

func TestHTTPRelayAndCursorExpired(t *testing.T) {
	reader := &fakeReader{binding: binding(), pages: []Page{{FirstAvailable: 1, Latest: 1, Terminal: true, Records: []Record{record(1)}}}}
	service, _ := New(Dependencies{Reader: reader, Authorizer: &fakeAuthorizer{}, Waiter: &fakeWaiter{}, PollInterval: 10 * time.Millisecond, HeartbeatEvery: 1})
	handler, err := NewHTTPHandler(service, callerProvider{ok: true})
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodGet, "/v1/agent-runs/"+testRunID+"/events", nil)
	httpRequest.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	if response.Code != 200 || response.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(response.Body.String(), "id: 1\n") || !strings.Contains(response.Body.String(), `"runsequence":1`) {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}

	reader.pages = []Page{{FirstAvailable: 7, Latest: 9, Records: []Record{record(7)}}}
	httpRequest = httptest.NewRequest(http.MethodGet, "/v1/agent-runs/"+testRunID+"/events", nil)
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Last-Event-ID", "2")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httpRequest)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"code":"STREAM_CURSOR_EXPIRED"`) || !strings.Contains(response.Body.String(), `"latest_sequence":9`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHTTPRejectsUnauthenticatedAndAmbiguousHeaders(t *testing.T) {
	service, _ := New(Dependencies{Reader: &fakeReader{}, Authorizer: &fakeAuthorizer{}, Waiter: &fakeWaiter{}, PollInterval: 10 * time.Millisecond, HeartbeatEvery: 1})
	for _, test := range []struct {
		name     string
		provider callerProvider
		headers  http.Header
		status   int
	}{
		{name: "unauthenticated", provider: callerProvider{}, headers: http.Header{"Accept": {"text/event-stream"}}, status: 401},
		{name: "duplicate accept", provider: callerProvider{ok: true}, headers: http.Header{"Accept": {"text/event-stream", "text/event-stream"}}, status: 406},
		{name: "duplicate cursor", provider: callerProvider{ok: true}, headers: http.Header{"Accept": {"text/event-stream"}, "Last-Event-Id": {"1", "2"}}, status: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := NewHTTPHandler(service, test.provider)
			r := httptest.NewRequest(http.MethodGet, "/v1/agent-runs/"+testRunID+"/events", nil)
			r.Header = test.headers
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
