package registryapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

const maxJSONBody = 1 << 20

type CallerProvider func(*http.Request) (Caller, platform.RequestMetadata, bool)

type HTTPHandler struct {
	service *Service
	caller  CallerProvider
	mux     *http.ServeMux
}

func NewHTTPHandler(service *Service, caller CallerProvider) (*HTTPHandler, error) {
	if service == nil || caller == nil {
		return nil, errors.New("registry API service and caller provider are required")
	}
	handler := &HTTPHandler{service: service, caller: caller, mux: http.NewServeMux()}
	handler.mux.HandleFunc("PUT /v1/registry/instances/{instance_id}", handler.register)
	handler.mux.HandleFunc("PATCH /v1/registry/instances/{instance_id}", handler.operate)
	handler.mux.HandleFunc("DELETE /v1/registry/instances/{instance_id}", handler.deregister)
	handler.mux.HandleFunc("POST /v1/registry/instances/{instance_id}/drain", handler.drain)
	handler.mux.HandleFunc("POST /v1/registry/leases/{lease_id}/keepalive", handler.keepalive)
	handler.mux.HandleFunc("GET /v1/discovery/agents/{agent_id}/instances", handler.discover)
	// /v1/discovery/changes is intentionally not registered until P16.
	return handler, nil
}

func (handler *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

type registerBody struct {
	SchemaVersion int                   `json:"schema_version"`
	SessionID     string                `json:"session_id"`
	ServiceID     string                `json:"service_id"`
	Environment   string                `json:"environment"`
	Endpoint      registry.Endpoint     `json:"endpoint"`
	Bindings      []registry.Binding    `json:"bindings"`
	Runtime       registry.RuntimeState `json:"runtime"`
}

type keepaliveBody struct {
	SchemaVersion     int       `json:"schema_version"`
	InstanceID        string    `json:"instance_id"`
	SessionID         string    `json:"session_id"`
	Generation        uint64    `json:"generation"`
	HeartbeatSequence uint64    `json:"heartbeat_sequence"`
	ReportedAt        time.Time `json:"reported_at"`
	Ready             bool      `json:"ready"`
	ActiveRuns        uint64    `json:"active_runs"`
	AvailableSlots    uint64    `json:"available_slots"`
	QueueDepth        uint64    `json:"queue_depth"`
}

type operateBody struct {
	SchemaVersion           int    `json:"schema_version"`
	ExpectedResourceVersion uint64 `json:"expected_resource_version"`
	Enabled                 bool   `json:"enabled"`
	Weight                  uint64 `json:"weight"`
	Priority                uint64 `json:"priority"`
	MaintenanceReason       string `json:"maintenance_reason,omitempty"`
}

type drainBody struct {
	SchemaVersion int       `json:"schema_version"`
	SessionID     string    `json:"session_id"`
	LeaseID       string    `json:"lease_id"`
	Generation    uint64    `json:"generation"`
	DeadlineAt    time.Time `json:"deadline_at"`
}

func (handler *HTTPHandler) register(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	keys := request.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !validIdempotencyKey(keys[0]) {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	var body registerBody
	if decodeErr := decodeBody(request, &body); decodeErr != nil || body.SchemaVersion != 1 {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, err := handler.service.Register(request.Context(), caller, RegisterInput{InstanceID: request.PathValue("instance_id"), SessionID: body.SessionID, ServiceID: body.ServiceID, Environment: body.Environment, Endpoint: body.Endpoint, Bindings: body.Bindings, Runtime: body.Runtime, IdempotencyKey: keys[0], Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	status := http.StatusCreated
	if result.Replay {
		status = http.StatusOK
	}
	writeJSON(writer, status, registrationWire{SchemaVersion: 1, Instance: mapInstance(result.Instance), LeaseTTLSeconds: result.LeaseTTLSeconds, KeepaliveIntervalSeconds: result.KeepaliveIntervalSeconds, Replay: result.Replay})
}

func (handler *HTTPHandler) keepalive(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	var body keepaliveBody
	if decodeErr := decodeBody(request, &body); decodeErr != nil || body.SchemaVersion != 1 {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	leaseID := request.PathValue("lease_id")
	result, err := handler.service.Keepalive(request.Context(), caller, KeepaliveInput{InstanceID: body.InstanceID, SessionID: body.SessionID, LeaseID: leaseID, Generation: body.Generation, HeartbeatSequence: body.HeartbeatSequence, ReportedAt: body.ReportedAt, Ready: body.Ready, ActiveRuns: body.ActiveRuns, AvailableSlots: body.AvailableSlots, QueueDepth: body.QueueDepth, Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, leaseWire{SchemaVersion: 1, LeaseID: result.LeaseID, InstanceID: result.InstanceID, SessionID: result.SessionID, Generation: result.Generation, HeartbeatSequence: result.HeartbeatSequence, ReportedAt: &body.ReportedAt, ServerTime: result.LeaseExpiresAt.Add(-handler.service.LeaseTTL()), LeaseExpiresAt: result.LeaseExpiresAt, RegistryRevision: result.RegistryRevision, LeaseTTLSeconds: uint64(handler.service.LeaseTTL() / time.Second), KeepaliveIntervalSeconds: uint64(handler.service.KeepaliveInterval() / time.Second)})
}

func (handler *HTTPHandler) operate(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	var body operateBody
	if decodeErr := decodeBody(request, &body); decodeErr != nil || body.SchemaVersion != 1 {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, err := handler.service.Operate(request.Context(), caller, OperateInput{InstanceID: request.PathValue("instance_id"), ExpectedResourceVersion: body.ExpectedResourceVersion, Enabled: body.Enabled, Weight: body.Weight, Priority: body.Priority, MaintenanceReason: body.MaintenanceReason, Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, mapInstance(result))
}

func (handler *HTTPHandler) drain(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	var body drainBody
	if decodeErr := decodeBody(request, &body); decodeErr != nil || body.SchemaVersion != 1 {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, err := handler.service.Drain(request.Context(), caller, DrainInput{InstanceID: request.PathValue("instance_id"), SessionID: body.SessionID, LeaseID: body.LeaseID, Generation: body.Generation, DeadlineAt: body.DeadlineAt, Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, mapInstance(result))
}

func (handler *HTTPHandler) deregister(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	query, err := exactQuery(request, "session_id", "lease_id", "generation")
	if err != nil {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	generation, err := strconv.ParseUint(query.Get("generation"), 10, 64)
	if err != nil || generation == 0 || generation > registry.MaxSafeInteger || !emptyBody(request.Body) {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, err := handler.service.Deregister(request.Context(), caller, DeregisterInput{InstanceID: request.PathValue("instance_id"), SessionID: query.Get("session_id"), LeaseID: query.Get("lease_id"), Generation: generation, Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, mapInstance(result))
}

func (handler *HTTPHandler) discover(writer http.ResponseWriter, request *http.Request) {
	caller, metadata, ok := handler.caller(request)
	if !ok {
		writeError(writer, registry.NewError(registry.ReasonDependencyUnavailable))
		return
	}
	query, err := exactQuery(request, "version", "skill_id", "protocol_version")
	if err != nil || !emptyBody(request.Body) {
		writeError(writer, registry.NewError(registry.ReasonInvalidRequest))
		return
	}
	result, err := handler.service.Discover(request.Context(), caller, DiscoverInput{AgentID: request.PathValue("agent_id"), AgentVersion: query.Get("version"), SkillID: query.Get("skill_id"), ProtocolVersion: query.Get("protocol_version"), Metadata: metadata})
	if err != nil {
		writeError(writer, err)
		return
	}
	instances := make([]runtimeInstanceWire, len(result.Instances))
	for index, instance := range result.Instances {
		instances[index] = mapInstance(instance)
	}
	writeJSON(writer, http.StatusOK, snapshotWire{SchemaVersion: 1, Revision: result.Revision, CompactionWatermark: result.CompactionWatermark, ServerTime: result.ServerTime, Instances: instances})
}

type runtimeInstanceWire struct {
	SchemaVersion    int                     `json:"schema_version"`
	InstanceID       string                  `json:"instance_id"`
	SessionID        string                  `json:"session_id"`
	ServiceID        string                  `json:"service_id"`
	Environment      string                  `json:"environment"`
	Generation       uint64                  `json:"generation"`
	ResourceVersion  uint64                  `json:"resource_version"`
	RegistryRevision uint64                  `json:"registry_revision"`
	LeaseID          string                  `json:"lease_id"`
	LeaseExpiresAt   time.Time               `json:"lease_expires_at"`
	Endpoint         registry.Endpoint       `json:"endpoint"`
	Bindings         []registry.Binding      `json:"bindings"`
	Runtime          registry.RuntimeState   `json:"runtime"`
	Operator         registry.OperatorState  `json:"operator"`
	Draining         bool                    `json:"draining"`
	DrainDeadlineAt  *time.Time              `json:"drain_deadline_at,omitempty"`
	Status           registry.InstanceStatus `json:"status"`
}

type registrationWire struct {
	SchemaVersion            int                 `json:"schema_version"`
	Instance                 runtimeInstanceWire `json:"instance"`
	LeaseTTLSeconds          uint64              `json:"lease_ttl_seconds"`
	KeepaliveIntervalSeconds uint64              `json:"keepalive_interval_seconds"`
	Replay                   bool                `json:"replay"`
}

type leaseWire struct {
	SchemaVersion                                               int `json:"schema_version"`
	LeaseID, InstanceID, SessionID                              string
	Generation, HeartbeatSequence                               uint64
	ReportedAt                                                  *time.Time
	ServerTime, LeaseExpiresAt                                  time.Time
	RegistryRevision, LeaseTTLSeconds, KeepaliveIntervalSeconds uint64
}

func (value leaseWire) MarshalJSON() ([]byte, error) {
	type wire struct {
		SchemaVersion            int        `json:"schema_version"`
		LeaseID                  string     `json:"lease_id"`
		InstanceID               string     `json:"instance_id"`
		SessionID                string     `json:"session_id"`
		Generation               uint64     `json:"generation"`
		HeartbeatSequence        uint64     `json:"heartbeat_sequence"`
		ReportedAt               *time.Time `json:"reported_at,omitempty"`
		ServerTime               time.Time  `json:"server_time"`
		LeaseExpiresAt           time.Time  `json:"lease_expires_at"`
		RegistryRevision         uint64     `json:"registry_revision"`
		LeaseTTLSeconds          uint64     `json:"lease_ttl_seconds"`
		KeepaliveIntervalSeconds uint64     `json:"keepalive_interval_seconds"`
	}
	return json.Marshal(wire(value))
}

type snapshotWire struct {
	SchemaVersion       int                   `json:"schema_version"`
	Revision            uint64                `json:"revision"`
	CompactionWatermark uint64                `json:"compaction_watermark"`
	ServerTime          time.Time             `json:"server_time"`
	Instances           []runtimeInstanceWire `json:"instances"`
}

func mapInstance(instance registry.Instance) runtimeInstanceWire {
	return runtimeInstanceWire{SchemaVersion: 1, InstanceID: instance.InstanceID, SessionID: instance.SessionID, ServiceID: instance.ServiceID, Environment: instance.Environment, Generation: instance.Generation, ResourceVersion: instance.ResourceVersion, RegistryRevision: instance.RegistryRevision, LeaseID: instance.LeaseID, LeaseExpiresAt: instance.LeaseExpiresAt, Endpoint: instance.Endpoint, Bindings: instance.Bindings, Runtime: instance.Runtime, Operator: instance.Operator, Draining: instance.Draining, DrainDeadlineAt: instance.DrainDeadlineAt, Status: instance.Status}
}

type errorWire struct {
	Code              string `json:"code"`
	Category          string `json:"category"`
	Message           string `json:"message"`
	Retryable         bool   `json:"retryable"`
	RetryAfterSeconds *int   `json:"retry_after_seconds,omitempty"`
}

func writeError(writer http.ResponseWriter, err error) {
	status, code, category, retryable := http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true
	switch {
	case errors.Is(err, ErrForbidden):
		status, code, category, retryable = http.StatusForbidden, "REGISTRY_FORBIDDEN", "authorization", false
	case registry.HasReason(err, registry.ReasonInvalidRequest):
		status, code, category, retryable = http.StatusBadRequest, string(registry.ReasonInvalidRequest), "validation", false
	case registry.HasReason(err, registry.ReasonNotFound):
		status, code, category, retryable = http.StatusNotFound, string(registry.ReasonNotFound), "not_found", false
	case registry.HasReason(err, registry.ReasonSessionReused), registry.HasReason(err, registry.ReasonGenerationFenced), registry.HasReason(err, registry.ReasonLeaseExpired), registry.HasReason(err, registry.ReasonHeartbeatStale), registry.HasReason(err, registry.ReasonResourceConflict), registry.HasReason(err, registry.ReasonIdempotencyConflict):
		status, code, category, retryable = http.StatusConflict, registryReason(err), "conflict", false
	case registry.HasReason(err, registry.ReasonRevisionOverflow), registry.HasReason(err, registry.ReasonResourceOverflow), registry.HasReason(err, registry.ReasonGenerationOverflow):
		status, code, category, retryable = http.StatusServiceUnavailable, registryReason(err), "capacity", false
	}
	var retryAfter *int
	if status == http.StatusServiceUnavailable && retryable {
		value := 1
		retryAfter = &value
		writer.Header().Set("Retry-After", "1")
	}
	writeJSON(writer, status, errorWire{Code: code, Category: category, Message: code, Retryable: retryable, RetryAfterSeconds: retryAfter})
}

func registryReason(err error) string {
	for _, reason := range []registry.ErrorReason{registry.ReasonSessionReused, registry.ReasonGenerationFenced, registry.ReasonLeaseExpired, registry.ReasonHeartbeatStale, registry.ReasonResourceConflict, registry.ReasonIdempotencyConflict, registry.ReasonRevisionOverflow, registry.ReasonResourceOverflow, registry.ReasonGenerationOverflow} {
		if registry.HasReason(err, reason) {
			return string(reason)
		}
	}
	return "DEPENDENCY_UNAVAILABLE"
}

func decodeBody(request *http.Request, destination any) error {
	if len(request.Header.Values("Content-Type")) != 1 || request.Header.Get("Content-Type") != "application/json" {
		return errors.New("content type is invalid")
	}
	if request.ContentLength > maxJSONBody {
		return errors.New("request body is too large")
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, maxJSONBody+1))
	if err != nil || len(data) > maxJSONBody {
		return errors.New("request body is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func exactQuery(request *http.Request, keys ...string) (mapQuery, error) {
	query := request.URL.Query()
	if len(query) != len(keys) {
		return nil, errors.New("query keys mismatch")
	}
	for _, key := range keys {
		values, ok := query[key]
		if !ok || len(values) != 1 || values[0] == "" {
			return nil, errors.New("query value mismatch")
		}
	}
	return mapQuery(query), nil
}

type mapQuery map[string][]string

func (query mapQuery) Get(key string) string {
	if len(query[key]) != 1 {
		return ""
	}
	return query[key][0]
}

func emptyBody(body io.Reader) bool {
	if body == nil {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(body, 1))
	return err == nil && len(data) == 0
}

func validIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 200 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
