package streaming

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

type CallerProvider interface {
	Caller(*http.Request) (run.Caller, platform.RequestMetadata, bool)
}

func NewHTTPHandler(service *Service, callers CallerProvider) (http.Handler, error) {
	if service == nil || callers == nil {
		return nil, errors.New("streaming HTTP dependencies are required")
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/v1/agent-runs/") || !strings.HasSuffix(request.URL.Path, "/events") || request.URL.RawQuery != "" {
			http.NotFound(response, request)
			return
		}
		if !singleHeader(request.Header, "Accept", "text/event-stream") {
			writeError(response, http.StatusNotAcceptable, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		runID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/agent-runs/"), "/events")
		after, ok := cursor(request.Header)
		if !ok || !prefixedUUID("run_", runID) {
			writeError(response, http.StatusBadRequest, NewError(CategoryValidation, ReasonInvalidRequest))
			return
		}
		caller, metadata, ok := callers.Caller(request)
		if !ok {
			writeError(response, http.StatusUnauthorized, NewError(CategoryAuthentication, ReasonAuthentication))
			return
		}
		sink := &httpSink{response: response, controller: http.NewResponseController(response), writeTimeout: 5 * time.Second}
		err := service.Stream(request.Context(), Request{Caller: caller, RunID: runID, After: after, Metadata: metadata}, sink)
		if err == nil || sink.started || errors.Is(err, request.Context().Err()) {
			return
		}
		writeStreamingError(response, err)
	}), nil
}

type httpSink struct {
	response     http.ResponseWriter
	controller   *http.ResponseController
	writeTimeout time.Duration
	started      bool
}

func (sink *httpSink) Start() error {
	if sink.started {
		return errors.New("stream already started")
	}
	if _, ok := sink.response.(http.Flusher); !ok {
		return errors.New("streaming unsupported")
	}
	sink.response.Header().Set("Content-Type", "text/event-stream")
	sink.response.Header().Set("Cache-Control", "no-store")
	sink.response.Header().Set("X-Accel-Buffering", "no")
	sink.response.WriteHeader(http.StatusOK)
	sink.started = true
	return sink.flush()
}

func (sink *httpSink) Event(record Record) error {
	if !sink.started {
		return errors.New("stream not started")
	}
	if err := sink.controller.SetWriteDeadline(time.Now().Add(sink.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprintf(sink.response, "id: %d\nevent: %s\ndata: %s\n\n", record.Sequence, record.EventType, record.Envelope); err != nil {
		return err
	}
	return sink.flush()
}

func (sink *httpSink) Heartbeat() error {
	if err := sink.controller.SetWriteDeadline(time.Now().Add(sink.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprint(sink.response, ": heartbeat\n\n"); err != nil {
		return err
	}
	return sink.flush()
}

func (sink *httpSink) flush() error {
	return sink.controller.Flush()
}

func cursor(header http.Header) (uint64, bool) {
	values, exists := header[http.CanonicalHeaderKey("Last-Event-ID")]
	if !exists {
		return 0, true
	}
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.HasPrefix(values[0], "+") || len(values[0]) > 16 {
		return 0, false
	}
	value, err := strconv.ParseUint(values[0], 10, 64)
	return value, err == nil && value <= MaxSafeInteger
}

func singleHeader(header http.Header, name, want string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == want
}

func writeStreamingError(response http.ResponseWriter, err error) {
	typed, ok := AsError(err)
	if !ok {
		writeError(response, http.StatusServiceUnavailable, NewError(CategoryDependency, ReasonDependencyUnavailable))
		return
	}
	status := http.StatusServiceUnavailable
	switch typed.Category {
	case CategoryValidation:
		status = http.StatusBadRequest
	case CategoryAuthentication:
		status = http.StatusUnauthorized
	case CategoryAuthorization:
		status = http.StatusForbidden
	case CategoryNotFound:
		status = http.StatusNotFound
	case CategoryCursor:
		if typed.Reason == ReasonCursorExpired {
			status = http.StatusGone
		} else {
			status = http.StatusConflict
		}
	}
	writeError(response, status, typed)
}

func writeError(response http.ResponseWriter, status int, err error) {
	typed, ok := AsError(err)
	if !ok {
		typed = &Error{Category: CategoryDependency, Reason: ReasonDependencyUnavailable}
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	payload := map[string]any{"code": typed.Reason}
	if typed.Reason == ReasonCursorExpired {
		payload["snapshot_url"] = typed.SnapshotURL
		payload["latest_sequence"] = typed.LatestSequence
	}
	_ = json.NewEncoder(response).Encode(payload)
}
