// Package registryapi binds the P14 registry domain to authenticated P15 use
// cases. It is transport-neutral except for the sibling HTTP adapter.
package registryapi

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type Operation string

const (
	OperationRegister   Operation = "registry.register"
	OperationKeepalive  Operation = "registry.keepalive"
	OperationOperate    Operation = "registry.operate"
	OperationDrain      Operation = "registry.drain"
	OperationDeregister Operation = "registry.deregister"
	OperationDiscover   Operation = "registry.discover"
	OperationWatch      Operation = "registry.watch"
)

var (
	tenantPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	principalPattern  = regexp.MustCompile(`^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	credentialPattern = regexp.MustCompile(`^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	scopePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
)

var ErrForbidden = errors.New("registry operation forbidden")

type Caller struct {
	TenantID     string
	PrincipalID  string
	CredentialID string
	Scopes       []string
}

func (caller Caller) Validate() error {
	if !tenantPattern.MatchString(caller.TenantID) || len(caller.TenantID) > 128 || !principalPattern.MatchString(caller.PrincipalID) || !credentialPattern.MatchString(caller.CredentialID) || len(caller.Scopes) == 0 {
		return ErrForbidden
	}
	seen := map[string]struct{}{}
	for _, scope := range caller.Scopes {
		if !scopePattern.MatchString(scope) {
			return ErrForbidden
		}
		if _, exists := seen[scope]; exists {
			return ErrForbidden
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// Valid exposes the already-frozen caller validation to later internal
// application phases without duplicating the identity grammar.
func (caller Caller) Valid() bool { return caller.Validate() == nil }

func (caller Caller) HasScope(scope string) bool { return slices.Contains(caller.Scopes, scope) }

type AuthorizationRequest struct {
	Caller     Caller
	Operation  Operation
	InstanceID string
	AgentID    string
}

type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

type RegisterInput struct {
	InstanceID     string
	SessionID      string
	ServiceID      string
	Environment    string
	Endpoint       registry.Endpoint
	Bindings       []registry.Binding
	Runtime        registry.RuntimeState
	IdempotencyKey string
	Metadata       platform.RequestMetadata
}

type KeepaliveInput struct {
	InstanceID        string
	SessionID         string
	LeaseID           string
	Generation        uint64
	HeartbeatSequence uint64
	ReportedAt        time.Time
	Ready             bool
	ActiveRuns        uint64
	AvailableSlots    uint64
	QueueDepth        uint64
	Metadata          platform.RequestMetadata
}

type OperateInput struct {
	InstanceID              string
	ExpectedResourceVersion uint64
	Enabled                 bool
	Weight                  uint64
	Priority                uint64
	MaintenanceReason       string
	Metadata                platform.RequestMetadata
}

type DrainInput struct {
	InstanceID string
	SessionID  string
	LeaseID    string
	Generation uint64
	DeadlineAt time.Time
	Metadata   platform.RequestMetadata
}

type DeregisterInput struct {
	InstanceID string
	SessionID  string
	LeaseID    string
	Generation uint64
	Metadata   platform.RequestMetadata
}

type DiscoverInput struct {
	AgentID         string
	AgentVersion    string
	SkillID         string
	ProtocolVersion string
	Metadata        platform.RequestMetadata
}

type Snapshot struct {
	Revision            uint64
	CompactionWatermark uint64
	ServerTime          time.Time
	Instances           []registry.Instance
}
