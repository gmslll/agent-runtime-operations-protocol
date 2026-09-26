package registry

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	MaxSafeInteger           uint64 = 9007199254740991
	DefaultLeaseTTL                 = 30 * time.Second
	DefaultKeepaliveInterval        = 10 * time.Second
	MaxLeaseTTL                     = 10 * time.Minute
	MaxBindings                     = 256
	MaxLabels                       = 128
	MaxEnvironmentLength            = 64
)

var (
	tenantPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	instancePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	servicePattern  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	agentPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	skillPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	versionPattern  = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexDigest       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuid7Pattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	labelPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
)

type InstanceStatus string

const (
	StatusRegistered   InstanceStatus = "registered"
	StatusExpired      InstanceStatus = "expired"
	StatusDeregistered InstanceStatus = "deregistered"
)

type EventType string

const (
	EventRegistered   EventType = "registered"
	EventKeepalive    EventType = "keepalive"
	EventUpdated      EventType = "updated"
	EventDraining     EventType = "draining"
	EventDeregistered EventType = "deregistered"
	EventExpired      EventType = "expired"
)

type Binding struct {
	AgentID        string   `json:"agent_id"`
	AgentVersion   string   `json:"agent_version"`
	SkillIDs       []string `json:"skill_ids"`
	ManifestDigest string   `json:"manifest_digest"`
}

func (binding Binding) Validate() error {
	if !agentPattern.MatchString(binding.AgentID) || len(binding.AgentID) > 128 || !versionPattern.MatchString(binding.AgentVersion) || !digestPattern.MatchString(binding.ManifestDigest) || len(binding.SkillIDs) == 0 || len(binding.SkillIDs) > 128 {
		return NewError(ReasonInvalidRequest)
	}
	if !sort.StringsAreSorted(binding.SkillIDs) {
		return NewError(ReasonInvalidRequest)
	}
	for index, skill := range binding.SkillIDs {
		if !skillPattern.MatchString(skill) || len(skill) > 128 || index > 0 && skill == binding.SkillIDs[index-1] {
			return NewError(ReasonInvalidRequest)
		}
	}
	return nil
}

type Capabilities struct {
	Streaming    bool   `json:"streaming"`
	StreamResume bool   `json:"stream_resume"`
	Cancellation bool   `json:"cancellation"`
	StatusQuery  bool   `json:"status_query"`
	EventOutbox  string `json:"event_outbox"`
}

func (capabilities Capabilities) Validate() error {
	if capabilities.EventOutbox != "durable" && capabilities.EventOutbox != "best-effort" {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type Capacity struct {
	MaxConcurrency uint64 `json:"max_concurrency"`
	MaxQueueDepth  uint64 `json:"max_queue_depth"`
	ActiveRuns     uint64 `json:"active_runs"`
	AvailableSlots uint64 `json:"available_slots"`
	QueueDepth     uint64 `json:"queue_depth"`
}

func (capacity Capacity) Validate() error {
	if capacity.MaxConcurrency == 0 || capacity.MaxConcurrency > MaxSafeInteger || capacity.MaxQueueDepth > MaxSafeInteger || capacity.ActiveRuns > MaxSafeInteger || capacity.AvailableSlots > capacity.MaxConcurrency || capacity.QueueDepth > capacity.MaxQueueDepth || capacity.ActiveRuns+capacity.AvailableSlots > capacity.MaxConcurrency {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type Endpoint struct {
	BaseURL    string `json:"base_url"`
	HealthPath string `json:"health_path"`
}

func (endpoint Endpoint) Validate() error {
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || len(endpoint.BaseURL) > 2048 || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || len(endpoint.HealthPath) > 512 || !strings.HasPrefix(endpoint.HealthPath, "/") || strings.Contains(endpoint.HealthPath, "..") || strings.ContainsAny(endpoint.HealthPath, "?#") {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type RuntimeState struct {
	Healthy           bool              `json:"healthy"`
	Ready             bool              `json:"ready"`
	Capacity          Capacity          `json:"capacity"`
	RuntimeVersion    string            `json:"runtime_version"`
	ProtocolVersions  []string          `json:"protocol_versions"`
	TransportProfiles []string          `json:"transport_profiles"`
	Capabilities      Capabilities      `json:"capabilities"`
	Labels            map[string]string `json:"labels"`
}

func (state RuntimeState) Validate() error {
	if strings.TrimSpace(state.RuntimeVersion) == "" || len(state.RuntimeVersion) > 128 || len(state.ProtocolVersions) == 0 || len(state.ProtocolVersions) > 32 || len(state.TransportProfiles) == 0 || len(state.TransportProfiles) > 16 || len(state.Labels) > MaxLabels {
		return NewError(ReasonInvalidRequest)
	}
	if err := state.Capacity.Validate(); err != nil {
		return err
	}
	if err := state.Capabilities.Validate(); err != nil {
		return err
	}
	if !canonicalStrings(state.ProtocolVersions) || !canonicalStrings(state.TransportProfiles) {
		return NewError(ReasonInvalidRequest)
	}
	for key, value := range state.Labels {
		if !labelPattern.MatchString(key) || len(key) > 64 || strings.TrimSpace(value) == "" || len(value) > 256 {
			return NewError(ReasonInvalidRequest)
		}
	}
	return nil
}

type OperatorState struct {
	Enabled           bool   `json:"enabled"`
	Weight            uint64 `json:"weight"`
	Priority          uint64 `json:"priority"`
	MaintenanceReason string `json:"maintenance_reason,omitempty"`
}

func (state OperatorState) Validate() error {
	if state.Weight > 1000 || state.Priority > MaxSafeInteger || len(state.MaintenanceReason) > 512 {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type Instance struct {
	TenantID          string         `json:"tenant_id"`
	InstanceID        string         `json:"instance_id"`
	SessionID         string         `json:"session_id"`
	ServiceID         string         `json:"service_id"`
	Environment       string         `json:"environment"`
	Generation        uint64         `json:"generation"`
	ResourceVersion   uint64         `json:"resource_version"`
	RegistryRevision  uint64         `json:"registry_revision"`
	LeaseID           string         `json:"lease_id"`
	LeaseExpiresAt    time.Time      `json:"lease_expires_at"`
	HeartbeatSequence uint64         `json:"heartbeat_sequence"`
	Endpoint          Endpoint       `json:"endpoint"`
	Bindings          []Binding      `json:"bindings"`
	Runtime           RuntimeState   `json:"runtime"`
	Operator          OperatorState  `json:"operator"`
	Draining          bool           `json:"draining"`
	DrainDeadlineAt   *time.Time     `json:"drain_deadline_at,omitempty"`
	Status            InstanceStatus `json:"status"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

func (instance Instance) Validate() error {
	if !tenantPattern.MatchString(instance.TenantID) || len(instance.TenantID) > 128 || !instancePattern.MatchString(instance.InstanceID) || len(instance.InstanceID) > 128 || !validPrefixedUUID(instance.SessionID, "ses_") || !servicePattern.MatchString(instance.ServiceID) || len(instance.ServiceID) > 128 || !labelPattern.MatchString(instance.Environment) || len(instance.Environment) > MaxEnvironmentLength || instance.Generation == 0 || instance.Generation > MaxSafeInteger || instance.ResourceVersion == 0 || instance.ResourceVersion > MaxSafeInteger || instance.RegistryRevision == 0 || instance.RegistryRevision > MaxSafeInteger || !validPrefixedUUID(instance.LeaseID, "lease_") || !utc(instance.LeaseExpiresAt) || instance.HeartbeatSequence > MaxSafeInteger || len(instance.Bindings) == 0 || len(instance.Bindings) > MaxBindings || !utc(instance.CreatedAt) || !utc(instance.UpdatedAt) || instance.UpdatedAt.Before(instance.CreatedAt) {
		return NewError(ReasonInvalidRequest)
	}
	if instance.Status != StatusRegistered && instance.Status != StatusExpired && instance.Status != StatusDeregistered {
		return NewError(ReasonInvalidRequest)
	}
	if err := instance.Endpoint.Validate(); err != nil {
		return err
	}
	if err := instance.Runtime.Validate(); err != nil {
		return err
	}
	if err := instance.Operator.Validate(); err != nil {
		return err
	}
	previous := ""
	for _, binding := range instance.Bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		key := binding.AgentID + "\x00" + binding.AgentVersion + "\x00" + strings.Join(binding.SkillIDs, "\x00")
		if previous != "" && key <= previous {
			return NewError(ReasonInvalidRequest)
		}
		previous = key
	}
	if instance.DrainDeadlineAt != nil && !utc(*instance.DrainDeadlineAt) || instance.Draining != (instance.DrainDeadlineAt != nil) {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type Event struct {
	EventID    string    `json:"event_id"`
	TenantID   string    `json:"tenant_id"`
	Revision   uint64    `json:"revision"`
	Type       EventType `json:"type"`
	InstanceID string    `json:"instance_id"`
	SessionID  string    `json:"session_id"`
	Generation uint64    `json:"generation"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (event Event) Validate() error {
	if !validPrefixedUUID(event.EventID, "evt_") || !tenantPattern.MatchString(event.TenantID) || len(event.TenantID) > 128 || event.Revision == 0 || event.Revision > MaxSafeInteger || !validEventType(event.Type) || !instancePattern.MatchString(event.InstanceID) || len(event.InstanceID) > 128 || !validPrefixedUUID(event.SessionID, "ses_") || event.Generation == 0 || event.Generation > MaxSafeInteger || !utc(event.OccurredAt) {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type Registration struct {
	Instance                 Instance `json:"instance"`
	LeaseTTLSeconds          uint64   `json:"lease_ttl_seconds"`
	KeepaliveIntervalSeconds uint64   `json:"keepalive_interval_seconds"`
	Replay                   bool     `json:"replay"`
}

func (registration Registration) Validate() error {
	if err := registration.Instance.Validate(); err != nil {
		return err
	}
	if registration.LeaseTTLSeconds == 0 || registration.LeaseTTLSeconds > uint64(MaxLeaseTTL/time.Second) || registration.KeepaliveIntervalSeconds == 0 || registration.KeepaliveIntervalSeconds >= registration.LeaseTTLSeconds {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type RegisterRequest struct {
	TenantID                 string
	InstanceID               string
	SessionID                string
	ServiceID                string
	Environment              string
	Endpoint                 Endpoint
	Bindings                 []Binding
	Runtime                  RuntimeState
	IdempotencyKeyDigest     string
	IdempotencyRequestDigest string
}

func (request RegisterRequest) Validate() error {
	if !tenantPattern.MatchString(request.TenantID) || len(request.TenantID) > 128 || !instancePattern.MatchString(request.InstanceID) || len(request.InstanceID) > 128 || !validPrefixedUUID(request.SessionID, "ses_") || !servicePattern.MatchString(request.ServiceID) || len(request.ServiceID) > 128 || !labelPattern.MatchString(request.Environment) || len(request.Environment) > MaxEnvironmentLength || !hexDigest.MatchString(request.IdempotencyKeyDigest) || !hexDigest.MatchString(request.IdempotencyRequestDigest) || len(request.Bindings) == 0 || len(request.Bindings) > MaxBindings {
		return NewError(ReasonInvalidRequest)
	}
	if err := request.Endpoint.Validate(); err != nil {
		return err
	}
	if err := request.Runtime.Validate(); err != nil {
		return err
	}
	previous := ""
	for _, binding := range request.Bindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		key := binding.AgentID + "\x00" + binding.AgentVersion + "\x00" + strings.Join(binding.SkillIDs, "\x00")
		if previous != "" && key <= previous {
			return NewError(ReasonInvalidRequest)
		}
		previous = key
	}
	return nil
}

type KeepaliveRequest struct {
	TenantID          string
	InstanceID        string
	SessionID         string
	LeaseID           string
	Generation        uint64
	HeartbeatSequence uint64
	ReportedAt        time.Time
	Healthy           bool
	Ready             bool
	ActiveRuns        uint64
	AvailableSlots    uint64
	QueueDepth        uint64
}

func (request KeepaliveRequest) Fence() Fence {
	return Fence{TenantID: request.TenantID, InstanceID: request.InstanceID, SessionID: request.SessionID, LeaseID: request.LeaseID, Generation: request.Generation}
}

func (request KeepaliveRequest) Validate() error {
	if !tenantPattern.MatchString(request.TenantID) || len(request.TenantID) > 128 || !instancePattern.MatchString(request.InstanceID) || len(request.InstanceID) > 128 || !validPrefixedUUID(request.SessionID, "ses_") || !validPrefixedUUID(request.LeaseID, "lease_") || request.Generation == 0 || request.Generation > MaxSafeInteger || request.HeartbeatSequence == 0 || request.HeartbeatSequence > MaxSafeInteger || !utc(request.ReportedAt) || request.ActiveRuns > MaxSafeInteger || request.AvailableSlots > MaxSafeInteger || request.QueueDepth > MaxSafeInteger {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type CASRequest struct {
	TenantID                string
	InstanceID              string
	ExpectedResourceVersion uint64
	Enabled                 bool
	Weight                  uint64
	Priority                uint64
	MaintenanceReason       string
}

func (request CASRequest) Validate() error {
	if !tenantPattern.MatchString(request.TenantID) || len(request.TenantID) > 128 || !instancePattern.MatchString(request.InstanceID) || len(request.InstanceID) > 128 || request.ExpectedResourceVersion == 0 || request.ExpectedResourceVersion > MaxSafeInteger {
		return NewError(ReasonInvalidRequest)
	}
	return (OperatorState{Enabled: request.Enabled, Weight: request.Weight, Priority: request.Priority, MaintenanceReason: request.MaintenanceReason}).Validate()
}

type Fence struct {
	TenantID   string
	InstanceID string
	SessionID  string
	LeaseID    string
	Generation uint64
}

func (fence Fence) Validate() error {
	if !tenantPattern.MatchString(fence.TenantID) || len(fence.TenantID) > 128 || !instancePattern.MatchString(fence.InstanceID) || len(fence.InstanceID) > 128 || !validPrefixedUUID(fence.SessionID, "ses_") || !validPrefixedUUID(fence.LeaseID, "lease_") || fence.Generation == 0 || fence.Generation > MaxSafeInteger {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type DrainRequest struct {
	Fence
	DeadlineAt time.Time
}

func (request DrainRequest) Validate() error {
	if err := request.Fence.Validate(); err != nil || !utc(request.DeadlineAt) {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

type DiscoveryQuery struct {
	TenantID        string
	AgentID         string
	AgentVersion    string
	SkillID         string
	ProtocolVersion string
}

func (query DiscoveryQuery) Validate() error {
	if !tenantPattern.MatchString(query.TenantID) || len(query.TenantID) > 128 || !agentPattern.MatchString(query.AgentID) || len(query.AgentID) > 128 || !versionPattern.MatchString(query.AgentVersion) || !skillPattern.MatchString(query.SkillID) || len(query.SkillID) > 128 || strings.TrimSpace(query.ProtocolVersion) == "" || len(query.ProtocolVersion) > 64 {
		return NewError(ReasonInvalidRequest)
	}
	return nil
}

func (instance Instance) DiscoverableAt(now time.Time, query DiscoveryQuery) bool {
	if query.Validate() != nil || !utc(now) || instance.Status != StatusRegistered || !now.Before(instance.LeaseExpiresAt) || !instance.Runtime.Healthy || !instance.Runtime.Ready || !instance.Operator.Enabled || instance.Draining || instance.Runtime.Capacity.AvailableSlots == 0 || !slices.Contains(instance.Runtime.ProtocolVersions, query.ProtocolVersion) {
		return false
	}
	for _, binding := range instance.Bindings {
		if binding.AgentID == query.AgentID && binding.AgentVersion == query.AgentVersion && slices.Contains(binding.SkillIDs, query.SkillID) {
			return true
		}
	}
	return false
}

func canonicalStrings(values []string) bool {
	if len(values) == 0 || !sort.StringsAreSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 64 || index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}

func validPrefixedUUID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && uuid7Pattern.MatchString(strings.TrimPrefix(value, prefix))
}

func validEventType(value EventType) bool {
	switch value {
	case EventRegistered, EventKeepalive, EventUpdated, EventDraining, EventDeregistered, EventExpired:
		return true
	default:
		return false
	}
}

func utc(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func cloneBindings(bindings []Binding) []Binding {
	result := make([]Binding, len(bindings))
	for index, binding := range bindings {
		result[index] = binding
		result[index].SkillIDs = slices.Clone(binding.SkillIDs)
	}
	return result
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func validateLeasePolicy(ttl, keepalive time.Duration) error {
	if ttl <= 0 || ttl > MaxLeaseTTL || ttl%time.Second != 0 || keepalive <= 0 || keepalive >= ttl || keepalive%time.Second != 0 {
		return errors.New("registry lease policy is invalid")
	}
	return nil
}
