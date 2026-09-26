// Package registry is the public Go client for the AROP v1 Registry and
// Discovery APIs. It never follows redirects and never logs bearer material.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
	registrywire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/registry"
)

const maxResponseBytes = 2 << 20

var (
	slugPattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	sessionIDPattern  = regexp.MustCompile(`^ses_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	leaseIDPattern    = regexp.MustCompile(`^lease_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	semanticVersion   = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	protocolVersionRE = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
)

type CredentialSource interface {
	Credential(context.Context) (string, error)
}
type CredentialSourceFunc func(context.Context) (string, error)

func (source CredentialSourceFunc) Credential(ctx context.Context) (string, error) {
	return source(ctx)
}

type Client struct {
	BaseURL    *url.URL
	HTTPClient *http.Client
	Credential CredentialSource
}

type RegisterRequest struct {
	InstanceID     string
	SessionID      string
	ServiceID      string
	Environment    string
	Endpoint       registrywire.Endpoint
	Bindings       []registrywire.Binding
	Runtime        registrywire.Runtime
	IdempotencyKey string
}

type Registration struct {
	Instance                 registrywire.RuntimeInstance
	LeaseTTLSeconds          uint64
	KeepaliveIntervalSeconds uint64
	Replay                   bool
}

type KeepaliveRequest struct {
	InstanceID, SessionID, LeaseID         string
	Generation, HeartbeatSequence          uint64
	ReportedAt                             time.Time
	Ready                                  bool
	ActiveRuns, AvailableSlots, QueueDepth uint64
}

type OperateRequest struct {
	InstanceID              string
	ExpectedResourceVersion uint64
	Enabled                 bool
	Weight, Priority        uint64
	MaintenanceReason       string
}

type DrainRequest struct {
	InstanceID, SessionID, LeaseID string
	Generation                     uint64
	DeadlineAt                     time.Time
}

type DeregisterRequest struct {
	InstanceID, SessionID, LeaseID string
	Generation                     uint64
}

type DiscoveryQuery struct {
	AgentID, AgentVersion, SkillID, ProtocolVersion string
}

type RemoteError struct {
	StatusCode        int
	Code              string
	Category          string
	Retryable         bool
	RetryAfterSeconds *uint64
}

func (failure *RemoteError) Error() string {
	return fmt.Sprintf("registry request failed: status=%d code=%s", failure.StatusCode, failure.Code)
}

func (client Client) Register(ctx context.Context, request RegisterRequest) (Registration, error) {
	if !validSlug(request.InstanceID, 128) || !sessionIDPattern.MatchString(request.SessionID) || !validIdempotencyKey(request.IdempotencyKey) {
		return Registration{}, errors.New("registry registration request is invalid")
	}
	body := struct {
		SchemaVersion int                    `json:"schema_version"`
		SessionID     string                 `json:"session_id"`
		ServiceID     string                 `json:"service_id"`
		Environment   string                 `json:"environment"`
		Endpoint      registrywire.Endpoint  `json:"endpoint"`
		Bindings      []registrywire.Binding `json:"bindings"`
		Runtime       registrywire.Runtime   `json:"runtime"`
	}{1, request.SessionID, request.ServiceID, request.Environment, request.Endpoint, request.Bindings, request.Runtime}
	response, err := client.do(ctx, http.MethodPut, "/v1/registry/instances/"+request.InstanceID, nil, body, map[string]string{"Idempotency-Key": request.IdempotencyKey})
	if err != nil {
		return Registration{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return Registration{}, decodeRemoteError(response)
	}
	data, err := readJSON(response)
	if err != nil {
		return Registration{}, err
	}
	var envelope struct {
		SchemaVersion            int             `json:"schema_version"`
		Instance                 json.RawMessage `json:"instance"`
		LeaseTTLSeconds          uint64          `json:"lease_ttl_seconds"`
		KeepaliveIntervalSeconds uint64          `json:"keepalive_interval_seconds"`
		Replay                   bool            `json:"replay"`
	}
	if err := decodeExact(data, &envelope); err != nil || envelope.SchemaVersion != 1 || envelope.LeaseTTLSeconds == 0 || envelope.KeepaliveIntervalSeconds == 0 || envelope.KeepaliveIntervalSeconds >= envelope.LeaseTTLSeconds || envelope.Replay != (response.StatusCode == http.StatusOK) {
		return Registration{}, errors.New("invalid registry registration response")
	}
	instance, err := registrywire.DecodeRuntimeInstance(envelope.Instance)
	if err != nil || string(instance.InstanceID) != request.InstanceID || string(instance.SessionID) != request.SessionID {
		return Registration{}, errors.New("invalid registry registration response")
	}
	return Registration{Instance: instance, LeaseTTLSeconds: envelope.LeaseTTLSeconds, KeepaliveIntervalSeconds: envelope.KeepaliveIntervalSeconds, Replay: envelope.Replay}, nil
}

func (client Client) Keepalive(ctx context.Context, request KeepaliveRequest) (registrywire.RegistryLease, error) {
	if !validFence(request.InstanceID, request.SessionID, request.LeaseID, request.Generation) || request.HeartbeatSequence == 0 || request.HeartbeatSequence > 9007199254740991 || request.ReportedAt.IsZero() || request.ReportedAt.Location() != time.UTC || request.ActiveRuns > 9007199254740991 || request.AvailableSlots > 9007199254740991 || request.QueueDepth > 9007199254740991 {
		return registrywire.RegistryLease{}, errors.New("registry keepalive request is invalid")
	}
	body := struct {
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
	}{1, request.InstanceID, request.SessionID, request.Generation, request.HeartbeatSequence, request.ReportedAt, request.Ready, request.ActiveRuns, request.AvailableSlots, request.QueueDepth}
	response, err := client.do(ctx, http.MethodPost, "/v1/registry/leases/"+request.LeaseID+"/keepalive", nil, body, nil)
	if err != nil {
		return registrywire.RegistryLease{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return registrywire.RegistryLease{}, decodeRemoteError(response)
	}
	data, err := readJSON(response)
	if err != nil {
		return registrywire.RegistryLease{}, err
	}
	lease, err := registrywire.DecodeRegistryLease(data)
	if err != nil || string(lease.InstanceID) != request.InstanceID || string(lease.SessionID) != request.SessionID || string(lease.LeaseID) != request.LeaseID || uint64(lease.Generation) != request.Generation || uint64(lease.HeartbeatSequence) != request.HeartbeatSequence {
		return registrywire.RegistryLease{}, errors.New("invalid registry keepalive response")
	}
	return lease, nil
}

func (client Client) Operate(ctx context.Context, request OperateRequest) (registrywire.RuntimeInstance, error) {
	if !validSlug(request.InstanceID, 128) || request.ExpectedResourceVersion == 0 || request.ExpectedResourceVersion > 9007199254740991 || request.Weight > 1000 || request.Priority > 9007199254740991 || len(request.MaintenanceReason) > 512 {
		return registrywire.RuntimeInstance{}, errors.New("registry operator request is invalid")
	}
	body := struct {
		SchemaVersion           int    `json:"schema_version"`
		ExpectedResourceVersion uint64 `json:"expected_resource_version"`
		Enabled                 bool   `json:"enabled"`
		Weight                  uint64 `json:"weight"`
		Priority                uint64 `json:"priority"`
		MaintenanceReason       string `json:"maintenance_reason,omitempty"`
	}{1, request.ExpectedResourceVersion, request.Enabled, request.Weight, request.Priority, request.MaintenanceReason}
	return client.instanceMutation(ctx, http.MethodPatch, "/v1/registry/instances/"+request.InstanceID, nil, body, request.InstanceID)
}

func (client Client) Drain(ctx context.Context, request DrainRequest) (registrywire.RuntimeInstance, error) {
	if !validFence(request.InstanceID, request.SessionID, request.LeaseID, request.Generation) || request.DeadlineAt.IsZero() || request.DeadlineAt.Location() != time.UTC {
		return registrywire.RuntimeInstance{}, errors.New("registry drain request is invalid")
	}
	body := struct {
		SchemaVersion int       `json:"schema_version"`
		SessionID     string    `json:"session_id"`
		LeaseID       string    `json:"lease_id"`
		Generation    uint64    `json:"generation"`
		DeadlineAt    time.Time `json:"deadline_at"`
	}{1, request.SessionID, request.LeaseID, request.Generation, request.DeadlineAt}
	return client.instanceMutation(ctx, http.MethodPost, "/v1/registry/instances/"+request.InstanceID+"/drain", nil, body, request.InstanceID)
}

func (client Client) Deregister(ctx context.Context, request DeregisterRequest) (registrywire.RuntimeInstance, error) {
	if !validFence(request.InstanceID, request.SessionID, request.LeaseID, request.Generation) {
		return registrywire.RuntimeInstance{}, errors.New("registry deregistration request is invalid")
	}
	query := url.Values{"session_id": {request.SessionID}, "lease_id": {request.LeaseID}, "generation": {strconv.FormatUint(request.Generation, 10)}}
	return client.instanceMutation(ctx, http.MethodDelete, "/v1/registry/instances/"+request.InstanceID, query, nil, request.InstanceID)
}

func (client Client) Discover(ctx context.Context, query DiscoveryQuery) (registrywire.DiscoverySnapshot, error) {
	if !validSlug(query.AgentID, 128) || !semanticVersion.MatchString(query.AgentVersion) || !validSlug(query.SkillID, 128) || !protocolVersionRE.MatchString(query.ProtocolVersion) {
		return registrywire.DiscoverySnapshot{}, errors.New("registry discovery query is invalid")
	}
	values := url.Values{"version": {query.AgentVersion}, "skill_id": {query.SkillID}, "protocol_version": {query.ProtocolVersion}}
	response, err := client.do(ctx, http.MethodGet, "/v1/discovery/agents/"+query.AgentID+"/instances", values, nil, nil)
	if err != nil {
		return registrywire.DiscoverySnapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return registrywire.DiscoverySnapshot{}, decodeRemoteError(response)
	}
	data, err := readJSON(response)
	if err != nil {
		return registrywire.DiscoverySnapshot{}, err
	}
	return registrywire.DecodeDiscoverySnapshot(data)
}

func (client Client) instanceMutation(ctx context.Context, method, path string, query url.Values, body any, instanceID string) (registrywire.RuntimeInstance, error) {
	response, err := client.do(ctx, method, path, query, body, nil)
	if err != nil {
		return registrywire.RuntimeInstance{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return registrywire.RuntimeInstance{}, decodeRemoteError(response)
	}
	data, err := readJSON(response)
	if err != nil {
		return registrywire.RuntimeInstance{}, err
	}
	instance, err := registrywire.DecodeRuntimeInstance(data)
	if err != nil || string(instance.InstanceID) != instanceID {
		return registrywire.RuntimeInstance{}, errors.New("invalid registry instance response")
	}
	return instance, nil
}

func (client Client) do(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (*http.Response, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode registry request")
		}
	}
	return client.doRaw(ctx, method, path, query, encoded, headers)
}

func (client Client) doRaw(ctx context.Context, method, path string, query url.Values, body []byte, headers map[string]string) (*http.Response, error) {
	if err := client.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	credential, err := client.Credential.Credential(ctx)
	if err != nil || credential == "" || strings.TrimSpace(credential) != credential || strings.ContainsAny(credential, " \t\r\n,") {
		return nil, errors.New("registry credential unavailable")
	}
	endpoint := *client.BaseURL
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", "", ""
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + path
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}
	requestContext := ctx
	cancel := func() {}
	if client.HTTPClient.Timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, client.HTTPClient.Timeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create registry request")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	transport := client.HTTPClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := requestContext.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("registry transport unavailable")
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("invalid registry response")
	}
	return response, nil
}

func (client Client) validate() error {
	if client.BaseURL == nil || client.BaseURL.Scheme == "" || client.BaseURL.Host == "" || client.BaseURL.User != nil || client.BaseURL.RawQuery != "" || client.BaseURL.Fragment != "" || client.HTTPClient == nil || client.Credential == nil {
		return errors.New("registry client configuration is invalid")
	}
	if client.BaseURL.Scheme != "http" && client.BaseURL.Scheme != "https" {
		return errors.New("registry base URL must use http or https")
	}
	return nil
}

func readJSON(response *http.Response) ([]byte, error) {
	if len(response.Header.Values("Content-Type")) != 1 || response.Header.Get("Content-Type") != "application/json" {
		return nil, errors.New("invalid registry response content type")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, errors.New("invalid registry response body")
	}
	return data, nil
}

func decodeRemoteError(response *http.Response) error {
	data, err := readJSON(response)
	if err != nil {
		return err
	}
	wire, err := controlplane.DecodeAROPError(data)
	if err != nil {
		return errors.New("invalid registry error response")
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenges := response.Header.Values("WWW-Authenticate")
		if len(challenges) != 1 || (challenges[0] != "Bearer" && !strings.HasPrefix(challenges[0], "Bearer ")) {
			return errors.New("invalid registry authentication response")
		}
	}
	var retryAfter *uint64
	if wire.RetryAfterSeconds != nil {
		value := uint64(*wire.RetryAfterSeconds)
		retryAfter = &value
	}
	if response.StatusCode == http.StatusServiceUnavailable && wire.Retryable {
		values := response.Header.Values("Retry-After")
		if len(values) != 1 || retryAfter == nil || values[0] != strconv.FormatUint(*retryAfter, 10) {
			return errors.New("invalid registry retry response")
		}
	}
	return &RemoteError{StatusCode: response.StatusCode, Code: wire.Code, Category: wire.Category, Retryable: wire.Retryable, RetryAfterSeconds: retryAfter}
}

func decodeExact(data []byte, destination any) error {
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

func validSlug(value string, maximum int) bool {
	return len(value) <= maximum && slugPattern.MatchString(value)
}

func validFence(instanceID, sessionID, leaseID string, generation uint64) bool {
	return validSlug(instanceID, 128) && sessionIDPattern.MatchString(sessionID) && leaseIDPattern.MatchString(leaseID) && generation > 0 && generation <= 9007199254740991
}
