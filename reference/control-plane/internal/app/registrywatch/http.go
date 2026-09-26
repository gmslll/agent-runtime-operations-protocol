package registrywatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type HTTPHandler struct {
	service *Service
	caller  registryapi.CallerProvider
}

func NewHTTPHandler(service *Service, caller registryapi.CallerProvider) (*HTTPHandler, error) {
	if service == nil || caller == nil {
		return nil, ErrDependencyUnavailable
	}
	return &HTTPHandler{service: service, caller: caller}, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != "/v1/discovery/changes" {
		http.NotFound(writer, request)
		return
	}
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, ErrDependencyUnavailable)
		return
	}
	query, err := exactQuery(request.URL.Query(), "after_revision", "wait_seconds")
	if err != nil || !emptyBody(request.Body) {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	after, afterErr := strconv.ParseUint(query.Get("after_revision"), 10, 64)
	waitSeconds, waitErr := strconv.ParseUint(query.Get("wait_seconds"), 10, 64)
	if afterErr != nil || waitErr != nil || after > registry.MaxSafeInteger || waitSeconds == 0 || waitSeconds > uint64(MaxWait/time.Second) {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, watchErr := handler.service.Watch(request.Context(), caller, WatchInput{AfterRevision: after, Wait: time.Duration(waitSeconds) * time.Second, Metadata: metadata})
	if watchErr != nil {
		writeError(writer, NormalizeError(watchErr))
		return
	}
	events := make([]eventWire, len(result.Events))
	for index, event := range result.Events {
		events[index] = eventWire{SchemaVersion: 1, EventID: event.EventID, Revision: event.Revision, Type: event.Type, InstanceID: event.InstanceID, SessionID: event.SessionID, Generation: event.Generation, OccurredAt: event.OccurredAt}
	}
	writeJSON(writer, http.StatusOK, changesWire{SchemaVersion: 1, Revision: result.Revision, CompactionWatermark: result.CompactionWatermark, Events: events})
}

type changesWire struct {
	SchemaVersion       int         `json:"schema_version"`
	Revision            uint64      `json:"revision"`
	CompactionWatermark uint64      `json:"compaction_watermark"`
	Events              []eventWire `json:"events"`
}

type eventWire struct {
	SchemaVersion int                `json:"schema_version"`
	EventID       string             `json:"event_id"`
	Revision      uint64             `json:"revision"`
	Type          registry.EventType `json:"type"`
	InstanceID    string             `json:"instance_id"`
	SessionID     string             `json:"session_id"`
	Generation    uint64             `json:"generation"`
	OccurredAt    time.Time          `json:"occurred_at"`
}

type errorWire struct {
	Code              string `json:"code"`
	Category          string `json:"category"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
}

func writeError(writer http.ResponseWriter, err error) {
	status, code, category, retryable := http.StatusServiceUnavailable, "REGISTRY_DEPENDENCY_UNAVAILABLE", "dependency", true
	switch {
	case errors.Is(err, ErrCompacted):
		status, code, category, retryable = http.StatusGone, "REGISTRY_REVISION_COMPACTED", "conflict", false
	case errors.Is(err, ErrForbidden):
		status, code, category, retryable = http.StatusForbidden, "REGISTRY_FORBIDDEN", "authorization", false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code, category, retryable = http.StatusServiceUnavailable, "REGISTRY_DEPENDENCY_UNAVAILABLE", "dependency", true
	case registry.HasReason(err, registry.ReasonInvalidRequest):
		status, code, category, retryable = http.StatusBadRequest, string(registry.ReasonInvalidRequest), "validation", false
	}
	var retryAfter *int
	if status == http.StatusServiceUnavailable {
		value := 1
		retryAfter = &value
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, status, errorWire{Code: code, Category: category, Message: code, Retryable: retryable, RetryAfterSeconds: retryAfter})
}

func exactQuery(query url.Values, names ...string) (url.Values, error) {
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	if len(query) != len(want) {
		return nil, errors.New("query keys are not exact")
	}
	for name, values := range query {
		if _, ok := want[name]; !ok || len(values) != 1 || values[0] == "" {
			return nil, errors.New("query values are not exact")
		}
	}
	return query, nil
}

func emptyBody(body io.ReadCloser) bool {
	defer body.Close()
	buffer := make([]byte, 1)
	count, err := body.Read(buffer)
	return count == 0 && errors.Is(err, io.EOF)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
