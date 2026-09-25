package httpadapter

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/memory"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	assetwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/asset"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

type assetBrokerStub struct {
	issue    func(context.Context, assets.IssueRequest) (assets.IssuedGrant, error)
	upload   func(context.Context, assets.UploadReceipt) (assets.Asset, error)
	download func(context.Context, assets.DownloadReceipt) (assets.DownloadResult, error)
}

func (stub assetBrokerStub) IssueGrant(ctx context.Context, request assets.IssueRequest) (assets.IssuedGrant, error) {
	return stub.issue(ctx, request)
}

func (stub assetBrokerStub) ReceiveUpload(ctx context.Context, request assets.UploadReceipt) (assets.Asset, error) {
	return stub.upload(ctx, request)
}

func (stub assetBrokerStub) Download(ctx context.Context, request assets.DownloadReceipt) (assets.DownloadResult, error) {
	return stub.download(ctx, request)
}

func TestAssetRoutesSeparateIdentityFromGrantAuthentication(t *testing.T) {
	clock := platform.RealClock{}
	store, err := memory.New(100, 100)
	if err != nil {
		t.Fatal(err)
	}
	config := platform.DefaultConfig()
	config.MaxBodyBytes = assets.MaxAssetBytes
	application, err := platform.New(config, platform.Dependencies{
		Clock: clock, IDs: platform.SystemIDSource{Clock: clock}, Faults: platform.NoopFaultHook{},
		UoW: &platform.SerialUnitOfWork{}, Observability: store,
	}, "arop-reference-control-plane", "test")
	if err != nil {
		t.Fatal(err)
	}

	var authenticationCalls int
	var requiredScopes []string
	authenticate := func(ctx context.Context, credential string, _ platform.RequestMetadata) (AuthenticatedPrincipal, error) {
		authenticationCalls++
		requiredScopes = RequiredScopesFromContext(ctx)
		if credential != "identity-token" {
			return AuthenticatedPrincipal{}, context.Canceled
		}
		return AuthenticatedPrincipal{
			TenantID: "tenant-a", PrincipalID: "prn_01956e7b-9abc-7def-8abc-000000000001",
			CredentialID: "cred_01956e7b-9abc-7def-8abc-000000000002", Scopes: slices.Clone(requiredScopes),
		}, nil
	}
	publicationService := publicationStub{
		publish: func(context.Context, publication.PublishRequest) (publication.PublishResult, error) {
			return publication.PublishResult{}, publication.NewError(publication.CategoryAuthorization, publication.ReasonPublicationForbidden)
		},
		get: func(context.Context, publication.GetRequest) (publication.GetResult, error) {
			return publication.GetResult{}, publication.NewError(publication.CategoryAuthorization, publication.ReasonPublicationForbidden)
		},
	}

	const (
		runID   = "run_018f1b5a-7c3d-7a11-8b22-6d4e5f708192"
		assetID = "asset_018f1b5a-7c3d-7a11-8b22-6d4e5f708193"
		grantID = "grant_018f1b5a-7c3d-7a11-8b22-6d4e5f708194"
		token   = "agt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		digest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	var issued assets.IssueRequest
	var uploaded assets.UploadReceipt
	var downloaded assets.DownloadReceipt
	assetService := assetBrokerStub{
		issue: func(_ context.Context, request assets.IssueRequest) (assets.IssuedGrant, error) {
			issued = request
			binding := request.Binding()
			binding.AssetID = assetID
			return assets.IssuedGrant{Grant: assets.Grant{
				GrantID: grantID, Binding: binding, NotBefore: request.NotBefore, ExpiresAt: request.ExpiresAt,
				MaxUses: 1, Audience: assets.AssetTokenAudience, TokenKeyID: "atk_test",
				IdempotencyKeyDigest: strings.Repeat("b", 64), OpaqueDigest: strings.Repeat("c", 64),
			}, Token: token}, nil
		},
		upload: func(_ context.Context, request assets.UploadReceipt) (assets.Asset, error) {
			uploaded = request
			uploaded.Content = append([]byte(nil), request.Content...)
			return assets.Asset{}, nil
		},
		download: func(_ context.Context, request assets.DownloadReceipt) (assets.DownloadResult, error) {
			downloaded = request
			return assets.DownloadResult{Content: []byte("asset-content"), Name: "product.png", MediaType: "image/png", Digest: digest}, nil
		},
	}
	handler, err := NewApplicationHandler(application, authenticate, publicationService, assetService)
	if err != nil {
		t.Fatal(err)
	}

	exchange := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runID+"/assets:exchange", strings.NewReader(`{"kind":"upload_request","run_id":"`+runID+`","name":"product.png","media_type":"image/png","size_bytes":12,"digest":"`+digest+`"}`))
	exchange.Header.Set("Authorization", "Bearer identity-token")
	exchange.Header.Set("Content-Type", "application/json")
	exchange.Header.Set("Idempotency-Key", "asset-request-0001")
	exchangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(exchangeResponse, exchange)
	if exchangeResponse.Code != http.StatusOK || authenticationCalls != 1 || !slices.Equal(requiredScopes, []string{"asset:exchange"}) {
		t.Fatalf("asset exchange boundary failed: status=%d auth=%d scopes=%v body=%s", exchangeResponse.Code, authenticationCalls, requiredScopes, exchangeResponse.Body.String())
	}
	if issued.RunID != runID || issued.Caller.TenantID != "tenant-a" || issued.Operation != assets.OperationUpload || issued.Metadata.ParentSpanID == "" || issued.Metadata.SpanID == "" || issued.Metadata.ParentSpanID == issued.Metadata.SpanID {
		t.Fatalf("asset exchange did not bind caller/run/child span: %+v", issued)
	}
	wire, err := assetwire.DecodeGrantResponse(exchangeResponse.Body.Bytes())
	if err != nil || string(wire.GrantID) != grantID || string(wire.Asset.AssetID) != assetID || string(wire.BearerToken) != token || string(wire.BrokerPath) != "/v1/asset-content/"+grantID {
		t.Fatalf("asset grant response is not the generated contract: wire=%+v err=%v", wire, err)
	}

	upload := httptest.NewRequest(http.MethodPut, "/v1/asset-content/"+grantID, bytes.NewBufferString("asset-content"))
	upload.Header.Set("Authorization", "Bearer "+token)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusNoContent || authenticationCalls != 1 || uploaded.GrantID != grantID || uploaded.GrantToken != token || string(uploaded.Content) != "asset-content" || uploaded.Metadata.ParentSpanID == uploaded.Metadata.SpanID {
		t.Fatalf("asset upload boundary failed: status=%d auth=%d request=%+v body=%s", uploadResponse.Code, authenticationCalls, uploaded, uploadResponse.Body.String())
	}

	download := httptest.NewRequest(http.MethodGet, "/v1/asset-content/"+grantID, nil)
	download.Header.Set("Authorization", "Bearer "+token)
	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, download)
	if downloadResponse.Code != http.StatusOK || authenticationCalls != 1 || downloaded.GrantID != grantID || downloaded.GrantToken != token || downloadResponse.Body.String() != "asset-content" || downloadResponse.Header().Get("Digest") != digest || downloadResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("asset download boundary failed: status=%d auth=%d request=%+v headers=%v body=%s", downloadResponse.Code, authenticationCalls, downloaded, downloadResponse.Header(), downloadResponse.Body.String())
	}

	missingGrant := httptest.NewRequest(http.MethodGet, "/v1/asset-content/"+grantID, nil)
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingGrant)
	if missingResponse.Code != http.StatusUnauthorized || missingResponse.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("asset bearer rejection is not typed: status=%d headers=%v body=%s", missingResponse.Code, missingResponse.Header(), missingResponse.Body.String())
	}
	missingWire, err := controlplane.DecodeAROPError(missingResponse.Body.Bytes())
	if err != nil || missingWire.Code != "ASSET_GRANT_AUTHENTICATION_REQUIRED" || missingWire.Category != "authentication" || missingWire.Retryable {
		t.Fatalf("asset bearer rejection is not P11 AROPError: wire=%+v err=%v", missingWire, err)
	}
	if authenticationCalls != 1 {
		t.Fatal("asset content route invoked control-plane identity authentication")
	}
}
