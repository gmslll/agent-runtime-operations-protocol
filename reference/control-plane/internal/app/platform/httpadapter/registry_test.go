package httpadapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type registryCoreStub struct {
	query registry.DiscoveryQuery
}

func (*registryCoreStub) Register(context.Context, registry.RegisterRequest) (registry.Registration, error) {
	return registry.Registration{}, registry.NewError(registry.ReasonDependencyUnavailable)
}
func (*registryCoreStub) Keepalive(context.Context, registry.KeepaliveRequest) (registry.Instance, error) {
	return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
}
func (*registryCoreStub) CompareAndSwap(context.Context, registry.CASRequest) (registry.Instance, error) {
	return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
}
func (*registryCoreStub) Drain(context.Context, registry.DrainRequest) (registry.Instance, error) {
	return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
}
func (*registryCoreStub) Deregister(context.Context, registry.Fence) (registry.Instance, error) {
	return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
}
func (stub *registryCoreStub) Snapshot(_ context.Context, query registry.DiscoveryQuery) (registry.Snapshot, error) {
	stub.query = query
	return registry.Snapshot{Revision: 0, CompactionWatermark: 0}, nil
}

type registryAuthorizerStub struct{}

func (registryAuthorizerStub) Authorize(_ context.Context, request registryapi.AuthorizationRequest) error {
	want := map[registryapi.Operation]string{
		registryapi.OperationRegister: "registry:register", registryapi.OperationOperate: "registry:operate",
		registryapi.OperationKeepalive: "registry:write", registryapi.OperationDrain: "registry:write",
		registryapi.OperationDeregister: "registry:write", registryapi.OperationDiscover: "registry:discover",
	}[request.Operation]
	if want == "" || !request.Caller.HasScope(want) {
		return registryapi.ErrForbidden
	}
	return nil
}

type assetServiceStub struct{}

func (assetServiceStub) IssueGrant(context.Context, assets.IssueRequest) (assets.IssuedGrant, error) {
	return assets.IssuedGrant{}, errors.New("unused")
}
func (assetServiceStub) ReceiveUpload(context.Context, assets.UploadReceipt) (assets.Asset, error) {
	return assets.Asset{}, errors.New("unused")
}
func (assetServiceStub) Download(context.Context, assets.DownloadReceipt) (assets.DownloadResult, error) {
	return assets.DownloadResult{}, errors.New("unused")
}

func TestRegistryRoutesEnforceAuthenticationScopeTenantAndWatchBoundary(t *testing.T) {
	clock := platform.RealClock{}
	store, err := memory.New(100, 100)
	if err != nil {
		t.Fatal(err)
	}
	application, err := platform.New(platform.DefaultConfig(), platform.Dependencies{Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{}, UoW: &platform.SerialUnitOfWork{}, Observability: store}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}
	core := &registryCoreStub{}
	registryService, err := registryapi.New(registryapi.Dependencies{Core: core, UoW: &platform.SerialUnitOfWork{}, Clock: clock, Authorizer: registryAuthorizerStub{}, LeaseTTL: 30 * time.Second, KeepaliveInterval: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var requestedScopes []string
	authenticate := func(ctx context.Context, credential string, _ platform.RequestMetadata) (AuthenticatedPrincipal, error) {
		requestedScopes = RequiredScopesFromContext(ctx)
		switch credential {
		case "valid-token":
			return AuthenticatedPrincipal{TenantID: "tenant-a", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000002", Scopes: slices.Clone(requestedScopes)}, nil
		case "wrong-scope":
			return AuthenticatedPrincipal{TenantID: "tenant-a", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001", CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000002", Scopes: []string{"agent:read"}}, nil
		case "dependency-down":
			return AuthenticatedPrincipal{}, ErrAuthenticationUnavailable
		default:
			return AuthenticatedPrincipal{}, errors.New("rejected")
		}
	}
	publicationService := publicationStub{
		publish: func(context.Context, publication.PublishRequest) (publication.PublishResult, error) {
			return publication.PublishResult{}, errors.New("unused")
		},
		get: func(context.Context, publication.GetRequest) (publication.GetResult, error) {
			return publication.GetResult{}, errors.New("unused")
		},
	}
	handler, err := NewRegistryApplicationHandler(application, authenticate, publicationService, assetServiceStub{}, registryService)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/discovery/agents/image.generate/instances?version=1.0.0&skill_id=default&protocol_version=1.0", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !slices.Equal(requestedScopes, []string{"registry:discover"}) || core.query.TenantID != "tenant-a" || core.query.AgentID != "image.generate" {
		t.Fatalf("discovery boundary status=%d scopes=%v query=%+v body=%s", response.Code, requestedScopes, core.query, response.Body.String())
	}

	for credential, status := range map[string]int{"rejected": http.StatusUnauthorized, "dependency-down": http.StatusServiceUnavailable, "wrong-scope": http.StatusForbidden} {
		probe := httptest.NewRequest(http.MethodGet, "/v1/discovery/agents/image.generate/instances?version=1.0.0&skill_id=default&protocol_version=1.0", nil)
		probe.Header.Set("Authorization", "Bearer "+credential)
		probeResponse := httptest.NewRecorder()
		handler.ServeHTTP(probeResponse, probe)
		if probeResponse.Code != status || strings.Contains(probeResponse.Body.String(), credential) {
			t.Fatalf("credential=%s status=%d want=%d body=%s", credential, probeResponse.Code, status, probeResponse.Body.String())
		}
		if status != http.StatusForbidden {
			assertPublicationWireError(t, probeResponse, status)
		}
	}

	watch := httptest.NewRequest(http.MethodGet, "/v1/discovery/changes?after_revision=0&wait_seconds=1", nil)
	watch.Header.Set("Authorization", "Bearer valid-token")
	watchResponse := httptest.NewRecorder()
	handler.ServeHTTP(watchResponse, watch)
	if watchResponse.Code != http.StatusNotFound || !slices.Equal(requestedScopes, []string{"registry:discover"}) {
		t.Fatalf("P15 Watch boundary status=%d scopes=%v body=%s", watchResponse.Code, requestedScopes, watchResponse.Body.String())
	}
}
