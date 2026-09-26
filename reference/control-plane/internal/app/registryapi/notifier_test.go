package registryapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type commitAwareUoW struct{ active bool }

func (unit *commitAwareUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	unit.active = true
	err := callback(ctx)
	unit.active = false
	return err
}

type captureNotifier struct {
	unit     *commitAwareUoW
	tenantID string
	revision uint64
	calls    int
	duringTx bool
}

func (notifier *captureNotifier) Notify(tenantID string, revision uint64) {
	notifier.calls++
	notifier.tenantID, notifier.revision = tenantID, revision
	notifier.duringTx = notifier.unit.active
}

func TestRevisionNotificationOccursOnlyAfterSuccessfulCommit(t *testing.T) {
	core := &testCore{instance: validInstance()}
	core.instance.RegistryRevision = 12
	unit := &commitAwareUoW{}
	notifier := &captureNotifier{unit: unit}
	service, err := New(Dependencies{Core: core, UoW: unit, Clock: testClock{}, Authorizer: testAuthorizer{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second, RevisionNotifier: notifier})
	if err != nil {
		t.Fatal(err)
	}
	caller := Caller{TenantID: "reference-dev", PrincipalID: "prn_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", CredentialID: "cred_018f6b6e-8a2e-7c3a-8b2a-6d1e2f3a4b5c", Scopes: []string{"registry:write", "registry:register"}}
	input := KeepaliveInput{InstanceID: core.instance.InstanceID, SessionID: core.instance.SessionID, LeaseID: core.instance.LeaseID, Generation: core.instance.Generation, HeartbeatSequence: core.instance.HeartbeatSequence + 1, ReportedAt: testNow, Ready: true, ActiveRuns: 1, AvailableSlots: 1, QueueDepth: 0}
	if _, err := service.Keepalive(context.Background(), caller, input); err != nil {
		t.Fatal(err)
	}
	if notifier.calls != 1 || notifier.tenantID != caller.TenantID || notifier.revision != 12 || notifier.duringTx {
		t.Fatalf("notification=%+v", notifier)
	}

	core.registerErr = errors.New("database unavailable")
	_, _ = service.Register(context.Background(), caller, RegisterInput{InstanceID: "runtime-a", SessionID: core.instance.SessionID, ServiceID: core.instance.ServiceID, Environment: core.instance.Environment, Endpoint: core.instance.Endpoint, Bindings: core.instance.Bindings, Runtime: core.instance.Runtime, IdempotencyKey: "register-0001"})
	if notifier.calls != 1 {
		t.Fatalf("failed mutation notified: calls=%d", notifier.calls)
	}
	if !registry.HasReason(normalize(core.registerErr), registry.ReasonDependencyUnavailable) {
		t.Fatal("test dependency error did not normalize")
	}
}
