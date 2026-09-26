package httpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registrywatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type watchReaderStub struct{}

func (watchReaderStub) EventWindow(context.Context, string, uint64, uint64) (registry.EventWindow, error) {
	return registry.EventWindow{Revision: 1, Events: []registry.Event{}}, nil
}

type watchCoordinatorStub struct{}

func (watchCoordinatorStub) Acquire(context.Context, string) (registrywatch.Leadership, error) {
	return nil, registrywatch.ErrDependencyUnavailable
}
func (watchCoordinatorStub) Check(context.Context) error { return nil }

func TestRegistryRecoveryRouteIsAuthenticatedScopedAndOutsideGenericUoW(t *testing.T) {
	clock := platform.RealClock{}
	store, err := memory.New(100, 100)
	if err != nil {
		t.Fatal(err)
	}
	uow := &platform.SerialUnitOfWork{}
	application, err := platform.New(platform.DefaultConfig(), platform.Dependencies{Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{}, UoW: uow, Observability: store}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	core := &registryCoreStub{}
	registryService, err := registryapi.New(registryapi.Dependencies{Core: core, UoW: uow, Clock: clock, Authorizer: registryAuthorizerStub{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	hub := registrywatch.NewHub()
	watchService, err := registrywatch.New(registrywatch.Dependencies{Reader: watchReaderStub{}, Authorizer: registryAuthorizerStub{}, Notifier: hub, Coordinator: watchCoordinatorStub{}, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var requestedScopes []string
	authenticate := func(ctx context.Context, _ string, _ platform.RequestMetadata) (AuthenticatedPrincipal, error) {
		requestedScopes = RequiredScopesFromContext(ctx)
		return AuthenticatedPrincipal{TenantID: "tenant-a", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000002", Scopes: slices.Clone(requestedScopes)}, nil
	}
	handler, err := NewRegistryRecoveryApplicationHandler(application, authenticate, publicationStub{}, assetServiceStub{}, registryService, watchService)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/discovery/changes?after_revision=0&wait_seconds=1", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !slices.Equal(requestedScopes, []string{"registry:discover"}) {
		t.Fatalf("status=%d scopes=%v body=%s", response.Code, requestedScopes, response.Body.String())
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("instrumentation metadata missing")
	}
}
