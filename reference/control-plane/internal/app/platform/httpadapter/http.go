// Package httpadapter binds the P08 platform application to net/http. It owns
// transport concerns; the parent platform package remains HTTP-independent.
package httpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registryapi"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/registrywatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/assets"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/dispatch"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/publication"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
	assetwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/asset"
	controlplane "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/control-plane"
)

const (
	requestIDHeader   = "X-Request-ID"
	traceparentHeader = "Traceparent"
	tracestateHeader  = "Tracestate"
)

type healthResponse struct {
	Status     string                     `json:"status"`
	Service    string                     `json:"service"`
	Version    string                     `json:"version"`
	Scope      string                     `json:"scope"`
	Durability string                     `json:"durability"`
	Checks     []platform.ReadinessResult `json:"checks,omitempty"`
}

type errorResponse struct {
	Status string `json:"status"`
}

var ErrAuthenticationUnavailable = errors.New("authentication unavailable")

type AuthenticatedPrincipal struct {
	TenantID, PrincipalID, SubjectID, CredentialID string
	Scopes                                         []string
}

type AuthenticateFunc func(context.Context, string, platform.RequestMetadata) (AuthenticatedPrincipal, error)

type principalContextKey struct{}
type metadataContextKey struct{}
type requiredScopesContextKey struct{}

func PrincipalFromContext(ctx context.Context) (AuthenticatedPrincipal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(AuthenticatedPrincipal)
	return principal, ok
}

func MetadataFromContext(ctx context.Context) (platform.RequestMetadata, bool) {
	metadata, ok := ctx.Value(metadataContextKey{}).(platform.RequestMetadata)
	return metadata, ok
}

func RequiredScopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(requiredScopesContextKey{}).([]string)
	return slices.Clone(scopes)
}

func NewHandler(application *platform.Platform) (http.Handler, error) {
	return newHandler(application, nil)
}

func NewAuthenticatedHandler(application *platform.Platform, authenticate AuthenticateFunc) (http.Handler, error) {
	if authenticate == nil {
		return nil, errors.New("authentication function is required")
	}
	return newHandler(application, authenticate)
}

func NewPublicationHandler(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService) (http.Handler, error) {
	if authenticate == nil || service == nil {
		return nil, errors.New("publication authentication and service are required")
	}
	return newHandlerWithPublication(application, authenticate, service)
}

func NewApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil {
		return nil, errors.New("authentication, publication, and asset services are required")
	}
	return newHandlerWithServices(application, authenticate, publicationService, assetService)
}

func NewRegistryApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil || registryService == nil {
		return nil, errors.New("authentication and all application services are required")
	}
	return newHandlerWithAllServices(application, authenticate, publicationService, assetService, registryService, nil, nil, nil, nil)
}

func NewRegistryRecoveryApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service, watchService *registrywatch.Service) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil || registryService == nil || watchService == nil {
		return nil, errors.New("authentication and all registry recovery services are required")
	}
	return newHandlerWithAllServices(application, authenticate, publicationService, assetService, registryService, watchService, nil, nil, nil)
}

func NewRunApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service, watchService *registrywatch.Service, runService *run.Service) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil || registryService == nil || watchService == nil || runService == nil {
		return nil, errors.New("authentication and all run application services are required")
	}
	return newHandlerWithAllServices(application, authenticate, publicationService, assetService, registryService, watchService, runService, nil, nil)
}

func NewDispatchApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service, watchService *registrywatch.Service, runService *run.Service, dispatchService *dispatch.Service) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil || registryService == nil || watchService == nil || runService == nil || dispatchService == nil {
		return nil, errors.New("authentication and all dispatch application services are required")
	}
	return newHandlerWithAllServices(application, authenticate, publicationService, assetService, registryService, watchService, runService, dispatchService, nil)
}

func NewEventApplicationHandler(application *platform.Platform, authenticate AuthenticateFunc, publicationService publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service, watchService *registrywatch.Service, runService *run.Service, dispatchService *dispatch.Service, eventService *event.Service) (http.Handler, error) {
	if authenticate == nil || publicationService == nil || assetService == nil || registryService == nil || watchService == nil || runService == nil || dispatchService == nil || eventService == nil {
		return nil, errors.New("authentication and all event application services are required")
	}
	return newHandlerWithAllServices(application, authenticate, publicationService, assetService, registryService, watchService, runService, dispatchService, eventService)
}

func newHandler(application *platform.Platform, authenticate AuthenticateFunc) (http.Handler, error) {
	return newHandlerWithPublication(application, authenticate, nil)
}

func newHandlerWithPublication(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService) (http.Handler, error) {
	return newHandlerWithServices(application, authenticate, service, nil)
}

func newHandlerWithServices(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService, assetService assets.AssetBrokerService) (http.Handler, error) {
	return newHandlerWithAllServices(application, authenticate, service, assetService, nil, nil, nil, nil, nil)
}

func newHandlerWithAllServices(application *platform.Platform, authenticate AuthenticateFunc, service publication.PublicationService, assetService assets.AssetBrokerService, registryService *registryapi.Service, watchService *registrywatch.Service, runService *run.Service, dispatchService *dispatch.Service, eventService *event.Service) (http.Handler, error) {
	if application == nil {
		return nil, errors.New("platform application is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Service: application.Service(), Version: application.Version(), Scope: platform.ReadinessScope, Durability: application.Durability()})
	})
	mux.HandleFunc("GET /v1/health/ready", func(writer http.ResponseWriter, request *http.Request) {
		snapshot := application.Readiness(request.Context())
		status := http.StatusOK
		state := "ready"
		if !snapshot.Ready {
			status = http.StatusServiceUnavailable
			state = "not_ready"
		}
		writeJSON(writer, status, healthResponse{Status: state, Service: application.Service(), Version: application.Version(), Scope: snapshot.Scope, Durability: snapshot.Durability, Checks: snapshot.Checks})
	})
	if service != nil {
		mux.HandleFunc("POST /v1/agent-definitions/{agent_id}/versions", publicationPublish(application, service))
		mux.HandleFunc("GET /v1/agent-definitions/{agent_id}/versions/{version}", publicationGet(application, service))
	}
	if assetService != nil {
		mux.HandleFunc("POST /v1/runs/{run_id}/assets:exchange", assetExchange(application, assetService))
		mux.HandleFunc("PUT /v1/asset-content/{grant_id}", assetUpload(application, assetService))
		mux.HandleFunc("GET /v1/asset-content/{grant_id}", assetDownload(application, assetService))
	}
	if registryService != nil {
		callerProvider := func(request *http.Request) (registryapi.Caller, platform.RequestMetadata, bool) {
			principal, principalOK := PrincipalFromContext(request.Context())
			metadata, metadataOK := MetadataFromContext(request.Context())
			if !principalOK || !metadataOK {
				return registryapi.Caller{}, platform.RequestMetadata{}, false
			}
			return registryapi.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes)}, metadata, true
		}
		registryHandler, err := registryapi.NewHTTPHandler(registryService, callerProvider)
		if err != nil {
			return nil, err
		}
		mux.Handle("/v1/registry/", registryHandler)
		mux.Handle("/v1/discovery/", registryHandler)
		if watchService != nil {
			watchHandler, watchErr := registrywatch.NewHTTPHandler(watchService, callerProvider)
			if watchErr != nil {
				return nil, watchErr
			}
			mux.Handle("GET /v1/discovery/changes", watchHandler)
		}
	}
	if runService != nil {
		runHandler, err := run.NewHTTPHandler(runService, runContextCallerProvider{})
		if err != nil {
			return nil, err
		}
		mux.Handle("POST /v1/agent-runs", runHandler)
		mux.Handle("GET /v1/agent-runs/{run_id}", runHandler)
		mux.Handle("POST /v1/agent-runs/{run_id}/commands", runHandler)
	}
	if dispatchService != nil {
		dispatchHandler, err := dispatch.NewHTTPHandler(dispatchService, dispatchContextCallerProvider{application: application})
		if err != nil {
			return nil, err
		}
		mux.Handle("POST /v1/agent-runs/", dispatchHandler)
		mux.Handle("GET /.well-known/arop-jwks.json", dispatchHandler)
	}
	if eventService != nil {
		eventHandler, err := event.NewHTTPHandler(eventService, eventContextMetadataProvider{application: application})
		if err != nil {
			return nil, err
		}
		mux.Handle("POST /v1/agent-runs/{run_id}/event-session", eventHandler)
		mux.Handle("POST /v1/agent-runs/{run_id}/events:batch", eventHandler)
	}
	return instrument(application, mux, authenticate), nil
}

type eventContextMetadataProvider struct{ application *platform.Platform }

func (provider eventContextMetadataProvider) Metadata(request *http.Request) (platform.RequestMetadata, bool) {
	metadata, ok := MetadataFromContext(request.Context())
	if !ok || provider.application == nil {
		return platform.RequestMetadata{}, false
	}
	childSpan, err := provider.application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return metadata, true
}

func (eventContextMetadataProvider) TenantID(request *http.Request) (string, bool) {
	principal, ok := PrincipalFromContext(request.Context())
	return principal.TenantID, ok && principal.TenantID != ""
}

type runContextCallerProvider struct{}

type dispatchContextCallerProvider struct{ application *platform.Platform }

func (provider dispatchContextCallerProvider) Authenticate(request *http.Request) (dispatch.Caller, error) {
	principal, ok := PrincipalFromContext(request.Context())
	if !ok {
		return dispatch.Caller{}, dispatch.NewError(dispatch.CategoryAuthentication, dispatch.ReasonAuthenticationRequired)
	}
	return dispatch.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes)}, nil
}

func (provider dispatchContextCallerProvider) Metadata(request *http.Request) (platform.RequestMetadata, bool) {
	metadata, ok := MetadataFromContext(request.Context())
	if !ok || provider.application == nil {
		return platform.RequestMetadata{}, false
	}
	childSpan, err := provider.application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return metadata, true
}

func (runContextCallerProvider) Authenticate(request *http.Request, _ run.Operation) (run.Caller, error) {
	principal, ok := PrincipalFromContext(request.Context())
	if !ok {
		return run.Caller{}, run.NewError(run.CategoryAuthentication, run.ReasonAuthenticationRequired)
	}
	return run.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes)}, nil
}

func (runContextCallerProvider) Metadata(request *http.Request) (platform.RequestMetadata, bool) {
	return MetadataFromContext(request.Context())
}

func assetExchange(application *platform.Platform, service assets.AssetBrokerService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != "application/json" || len(request.Header.Values("Content-Type")) != 1 {
			writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
			return
		}
		keys := request.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
		if err != nil {
			writeAssetError(writer, assets.NewError(assets.CategoryCapacity, assets.ReasonAssetTooLarge))
			return
		}
		if len(body) > 1<<20 {
			clear(body)
			writeAssetError(writer, assets.NewError(assets.CategoryCapacity, assets.ReasonAssetTooLarge))
			return
		}
		caller, metadata, ok := assetCallerContext(application, request)
		if !ok {
			writeAssetError(writer, assets.NewError(assets.CategoryDependency, assets.ReasonDependencyUnavailable))
			return
		}
		now := application.Now()
		issue := assets.IssueRequest{Metadata: metadata, Caller: caller, IdempotencyKey: keys[0], NotBefore: now, ExpiresAt: now.Add(5 * time.Minute), MaxUses: 1}
		var discriminator struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(body, &discriminator); err != nil {
			writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
			return
		}
		switch discriminator.Kind {
		case "upload_request":
			model, decodeErr := assetwire.DecodeUploadRequest(body)
			if decodeErr != nil || string(model.RunID) != request.PathValue("run_id") {
				writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
				return
			}
			issue.RunID, issue.Operation = string(model.RunID), assets.OperationUpload
			issue.Name, issue.MediaType, issue.SizeBytes, issue.Digest = string(model.Name), string(model.MediaType), int64(model.SizeBytes), string(model.Digest)
		case "download_request":
			model, decodeErr := assetwire.DecodeDownloadRequest(body)
			if decodeErr != nil || string(model.RunID) != request.PathValue("run_id") {
				writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
				return
			}
			issue.RunID, issue.Operation, issue.AssetID = string(model.RunID), assets.OperationDownload, string(model.AssetID)
		default:
			writeAssetError(writer, assets.NewError(assets.CategoryValidation, assets.ReasonInvalidRequest))
			return
		}
		issued, err := service.IssueGrant(request.Context(), issue)
		if err != nil {
			writeAssetError(writer, err)
			return
		}
		digest := assetwire.Sha256Digest(issued.Grant.Binding.Digest)
		expires := assetwire.DateTime(issued.Grant.ExpiresAt.Format(time.RFC3339Nano))
		response := assetwire.GrantResponse{
			Kind: "grant", GrantID: assetwire.GrantId(issued.Grant.GrantID), Method: strings.TrimPrefix(string(issued.Grant.Binding.Operation), "asset."),
			BrokerPath: assetwire.BrokerPath("/v1/asset-content/" + issued.Grant.GrantID), BearerToken: assetwire.BearerToken(issued.Token), ExpiresAt: expires, MaxUses: assetwire.MaxUses(issued.Grant.MaxUses),
			Asset: assetwire.AROPV1AssetRef{AssetID: assetwire.AssetId(issued.Grant.Binding.AssetID), Name: issued.Grant.Binding.Name, MediaType: issued.Grant.Binding.MediaType, SizeBytes: assetwire.SafeInteger(issued.Grant.Binding.SizeBytes), Digest: &digest, Access: assetwire.AROPV1AssetRefAccess{Mode: "brokered", ExpiresAt: &expires}},
		}
		encoded, err := assetwire.EncodeGrantResponse(response)
		if err != nil {
			writeAssetError(writer, assets.NewError(assets.CategoryDependency, assets.ReasonDependencyUnavailable))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(encoded)
	}
}

func assetUpload(application *platform.Platform, service assets.AssetBrokerService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		metadata, ok := assetMetadata(application, request)
		if !ok {
			writeAssetError(writer, assets.NewError(assets.CategoryDependency, assets.ReasonDependencyUnavailable))
			return
		}
		token, err := assetGrantBearer(request.Header.Values("Authorization"))
		if err != nil {
			writeAssetError(writer, err)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writeAssetError(writer, assets.NewError(assets.CategoryCapacity, assets.ReasonAssetTooLarge))
			return
		}
		if _, err := service.ReceiveUpload(request.Context(), assets.UploadReceipt{Metadata: metadata, GrantID: request.PathValue("grant_id"), GrantToken: token, Content: body}); err != nil {
			clear(body)
			writeAssetError(writer, err)
			return
		}
		clear(body)
		writer.WriteHeader(http.StatusNoContent)
	}
}

func assetDownload(application *platform.Platform, service assets.AssetBrokerService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		metadata, ok := assetMetadata(application, request)
		if !ok {
			writeAssetError(writer, assets.NewError(assets.CategoryDependency, assets.ReasonDependencyUnavailable))
			return
		}
		token, err := assetGrantBearer(request.Header.Values("Authorization"))
		if err != nil {
			writeAssetError(writer, err)
			return
		}
		result, err := service.Download(request.Context(), assets.DownloadReceipt{Metadata: metadata, GrantID: request.PathValue("grant_id"), GrantToken: token})
		if err != nil {
			writeAssetError(writer, err)
			return
		}
		defer clear(result.Content)
		writer.Header().Set("Content-Type", result.MediaType)
		writer.Header().Set("Digest", result.Digest)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(result.Content)
	}
}

func publicationPublish(application *platform.Platform, service publication.PublicationService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Content-Type") != "application/vnd.arop.agent-version-bundle+zip" || len(request.Header.Values("Content-Type")) != 1 {
			writePublicationError(writer, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "validation", false)
			return
		}
		keys := request.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writePublicationError(writer, http.StatusBadRequest, "INVALID_PUBLICATION_REQUEST", "validation", false)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writePublicationError(writer, http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE", "capacity", false)
			return
		}
		caller, metadata, ok := publicationContext(application, request)
		if !ok {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		result, err := service.Publish(request.Context(), publication.PublishRequest{AgentID: request.PathValue("agent_id"), IdempotencyKey: keys[0], Bundle: body, Caller: caller, Metadata: metadata})
		if err != nil {
			writeDomainError(writer, err)
			return
		}
		writer.Header().Set("Location", result.Location)
		writer.Header().Set("ETag", result.ETag)
		writer.WriteHeader(http.StatusCreated)
	}
}

func publicationGet(application *platform.Platform, service publication.PublicationService) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		caller, metadata, ok := publicationContext(application, request)
		if !ok {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		result, err := service.Get(request.Context(), publication.GetRequest{AgentID: request.PathValue("agent_id"), Version: request.PathValue("version"), Caller: caller, Metadata: metadata})
		if err != nil {
			writeDomainError(writer, err)
			return
		}
		manifest, err := controlplane.DecodeAgentManifest(result.CanonicalManifest)
		if err != nil {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		encoded, err := controlplane.EncodeAgentManifest(manifest)
		if err != nil {
			writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("ETag", result.ETag)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(encoded)
	}
}

func publicationContext(application *platform.Platform, request *http.Request) (publication.Caller, platform.RequestMetadata, bool) {
	principal, principalOK := PrincipalFromContext(request.Context())
	metadata, metadataOK := MetadataFromContext(request.Context())
	if !principalOK || !metadataOK {
		return publication.Caller{}, platform.RequestMetadata{}, false
	}
	childSpan, err := application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return publication.Caller{}, platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return publication.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID, Scopes: slices.Clone(principal.Scopes)}, metadata, true
}

func assetCallerContext(application *platform.Platform, request *http.Request) (assets.Caller, platform.RequestMetadata, bool) {
	principal, principalOK := PrincipalFromContext(request.Context())
	metadata, metadataOK := MetadataFromContext(request.Context())
	if !principalOK || !metadataOK {
		return assets.Caller{}, platform.RequestMetadata{}, false
	}
	childSpan, err := application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return assets.Caller{}, platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return assets.Caller{TenantID: principal.TenantID, PrincipalID: principal.PrincipalID, CredentialID: principal.CredentialID}, metadata, true
}

func assetMetadata(application *platform.Platform, request *http.Request) (platform.RequestMetadata, bool) {
	metadata, ok := MetadataFromContext(request.Context())
	if !ok {
		return platform.RequestMetadata{}, false
	}
	childSpan, err := application.NewID(request.Context(), platformports.IDSpan)
	if err != nil {
		return platform.RequestMetadata{}, false
	}
	metadata.ParentSpanID, metadata.SpanID = metadata.SpanID, childSpan
	return metadata, true
}

func assetGrantBearer(values []string) (string, error) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", assets.NewError(assets.CategoryAuthentication, assets.ReasonAuthenticationRequired)
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if len(token) != 47 || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n,") {
		return "", assets.NewError(assets.CategoryAuthentication, assets.ReasonAuthenticationRequired)
	}
	return token, nil
}

func writeAssetError(writer http.ResponseWriter, err error) {
	typed, ok := assets.AsError(err)
	if !ok {
		typed, _ = assets.AsError(assets.NewError(assets.CategoryDependency, assets.ReasonDependencyUnavailable))
	}
	status, code := http.StatusBadRequest, "INVALID_ASSET_REQUEST"
	switch typed.Category {
	case assets.CategoryAuthentication:
		status, code = http.StatusUnauthorized, "ASSET_GRANT_AUTHENTICATION_REQUIRED"
	case assets.CategoryAuthorization:
		status, code = http.StatusForbidden, "ASSET_GRANT_FORBIDDEN"
	case assets.CategoryNotFound:
		status, code = http.StatusNotFound, "ASSET_NOT_FOUND"
	case assets.CategoryConflict:
		status, code = http.StatusConflict, "ASSET_CONFLICT"
	case assets.CategoryCapacity:
		status, code = http.StatusRequestEntityTooLarge, "ASSET_TOO_LARGE"
	case assets.CategoryNetwork:
		status, code = http.StatusForbidden, "ASSET_NETWORK_DENIED"
	case assets.CategoryDependency:
		status, code = http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"
	}
	writePublicationError(writer, status, code, string(typed.Category), typed.Retryable)
}

func writeDomainError(writer http.ResponseWriter, err error) {
	typed, ok := publication.AsError(err)
	if !ok {
		writePublicationError(writer, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
		return
	}
	status, code := http.StatusBadRequest, "INVALID_PUBLICATION_REQUEST"
	switch typed.Category {
	case publication.CategoryAuthentication:
		status, code = http.StatusUnauthorized, "AUTHENTICATION_REQUIRED"
	case publication.CategoryAuthorization:
		status, code = http.StatusForbidden, "PUBLICATION_FORBIDDEN"
	case publication.CategoryNotFound:
		status, code = http.StatusNotFound, "AGENT_VERSION_NOT_FOUND"
	case publication.CategoryConflict:
		status, code = http.StatusConflict, "AGENT_VERSION_CONFLICT"
	case publication.CategoryCapacity:
		status, code = http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE"
	case publication.CategoryDependency:
		status, code = http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"
	}
	writePublicationError(writer, status, code, string(typed.Category), typed.Retryable)
}

func writePublicationError(writer http.ResponseWriter, status int, code, category string, retryable bool) {
	var retryAfter *controlplane.SafeInteger
	if status == http.StatusUnauthorized {
		writer.Header().Set("WWW-Authenticate", "Bearer")
	}
	if status == http.StatusServiceUnavailable {
		writer.Header().Set("Retry-After", "1")
		value := controlplane.SafeInteger(1)
		retryAfter = &value
	}
	encoded, err := controlplane.EncodeAROPError(controlplane.AROPError{Category: category, Code: code, Message: code, Retryable: retryable, RetryAfterSeconds: retryAfter})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, errorResponse{Status: "error"})
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func NewServer(application *platform.Platform, handler http.Handler) *http.Server {
	config := application.Config()
	return &http.Server{
		Addr: config.ListenAddress, Handler: handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout, ReadTimeout: config.ReadTimeout,
		WriteTimeout: config.WriteTimeout, IdleTimeout: config.IdleTimeout,
		MaxHeaderBytes: config.MaxHeaderBytes,
	}
}

func NewRegistryRecoveryServer(application *platform.Platform, handler http.Handler) (*http.Server, error) {
	server := NewServer(application, handler)
	if server.WriteTimeout <= registrywatch.MaxWait {
		return nil, errors.New("registry recovery write timeout must exceed maximum watch wait")
	}
	return server, nil
}

func instrument(application *platform.Platform, next http.Handler, authenticate AuthenticateFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		config := application.Config()
		operation := classifyOperation(request.Method, request.URL.Path)
		requestTimeout := config.RequestTimeout
		if operation == "registry.watch" && requestTimeout <= registrywatch.MaxWait {
			requestTimeout = registrywatch.MaxWait + time.Second
		}
		ctx, cancel := context.WithTimeout(request.Context(), requestTimeout)
		defer cancel()
		request = request.WithContext(ctx)
		request.Body = http.MaxBytesReader(writer, request.Body, config.MaxBodyBytes)

		startedAt := application.Now()
		metadata, metadataErr := application.BeginRequest(ctx, request.Header.Values(requestIDHeader), request.Header.Values(traceparentHeader), request.Header.Values(tracestateHeader))
		if metadataErr == nil {
			request = request.WithContext(context.WithValue(request.Context(), metadataContextKey{}, metadata))
		}
		capture := newResponseCapture()
		if metadata.RequestID != "" {
			capture.Header().Set(requestIDHeader, metadata.RequestID)
		}
		if metadata.Traceparent() != "" {
			capture.Header().Set(traceparentHeader, metadata.Traceparent())
		}
		setSecurityHeaders(capture.Header())
		emptyHealthBody := true
		var healthBodyErr error
		if operation == "health.live" || operation == "health.ready" {
			emptyHealthBody, healthBodyErr = bodyIsEmpty(request.Body)
		}

		switch {
		case metadataErr != nil:
			writeJSON(capture, http.StatusBadRequest, errorResponse{Status: "rejected"})
		case healthBodyErr != nil || !emptyHealthBody:
			writeJSON(capture, http.StatusBadRequest, errorResponse{Status: "rejected"})
		case request.ContentLength > config.MaxBodyBytes:
			if operation == "publication.publish" {
				writePublicationError(capture, http.StatusRequestEntityTooLarge, "BUNDLE_TOO_LARGE", "capacity", false)
			} else if operation == "asset.upload" {
				writeAssetError(capture, assets.NewError(assets.CategoryCapacity, assets.ReasonAssetTooLarge))
			} else if isRegistryOperation(operation) {
				writePublicationError(capture, http.StatusBadRequest, "REGISTRY_INVALID_REQUEST", "validation", false)
			} else if isRunOperation(operation) {
				writePublicationError(capture, http.StatusRequestEntityTooLarge, "RUN_REQUEST_TOO_LARGE", "capacity", false)
			} else if isDispatchOperation(operation) {
				writePublicationError(capture, http.StatusBadRequest, "INVALID_DISPATCH_REQUEST", "validation", false)
			} else {
				writeJSON(capture, http.StatusRequestEntityTooLarge, errorResponse{Status: "rejected"})
			}
		default:
			if authenticate != nil && requiresControlPlaneAuthentication(operation) {
				requiredScopes := scopesForOperation(operation)
				authenticationContext := context.WithValue(ctx, requiredScopesContextKey{}, requiredScopes)
				principal, authenticationErr := authenticateBearer(authenticationContext, request.Header.Values("Authorization"), metadata, authenticate)
				switch {
				case authenticationErr == nil:
					requestContext := context.WithValue(request.Context(), principalContextKey{}, principal)
					requestContext = context.WithValue(requestContext, metadataContextKey{}, metadata)
					request = request.WithContext(requestContext)
					invoke(application, capture, request, next)
				case errors.Is(authenticationErr, ErrAuthenticationUnavailable):
					if isContractOperation(operation) {
						writePublicationError(capture, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
					} else {
						writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
					}
				default:
					if operation == "asset.exchange" {
						writeAssetError(capture, assets.NewError(assets.CategoryAuthentication, assets.ReasonAuthenticationRequired))
					} else if isPublicationOperation(operation) || isRegistryOperation(operation) || isRunOperation(operation) || isDispatchOperation(operation) || isEventOperation(operation) {
						writePublicationError(capture, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "authentication", false)
					} else {
						writeJSON(capture, http.StatusUnauthorized, errorResponse{Status: "unauthorized"})
					}
				}
			} else {
				invoke(application, capture, request, next)
			}
		}
		if metadata.RequestID != "" {
			capture.Header().Set(requestIDHeader, metadata.RequestID)
		}
		if metadata.Traceparent() != "" {
			capture.Header().Set(traceparentHeader, metadata.Traceparent())
		}

		endedAt := application.Now()
		statusCode := capture.statusCode()
		if metadata.RequestID != "" && metadata.TraceID != "" && metadata.SpanID != "" {
			recordContext, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), config.RequestTimeout)
			recordErr := application.RecordObservation(recordContext, platform.Observation{Metadata: metadata, Operation: operation, StartedAt: startedAt, EndedAt: endedAt, HTTPStatus: statusCode})
			recordCancel()
			if recordErr != nil && statusCode < 500 {
				capture = newResponseCapture()
				setSecurityHeaders(capture.Header())
				capture.Header().Set(requestIDHeader, metadata.RequestID)
				capture.Header().Set(traceparentHeader, metadata.Traceparent())
				if isContractOperation(operation) {
					writePublicationError(capture, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "dependency", true)
				} else {
					writeJSON(capture, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
				}
			}
		}
		copyResponse(writer, capture)
	})
}

func isPublicationOperation(operation string) bool {
	return operation == "publication.publish" || operation == "publication.get"
}

func isAssetOperation(operation string) bool {
	return operation == "asset.exchange" || operation == "asset.upload" || operation == "asset.download"
}

func isRegistryOperation(operation string) bool {
	return strings.HasPrefix(operation, "registry.")
}

func isRunOperation(operation string) bool { return strings.HasPrefix(operation, "run.") }

func isDispatchOperation(operation string) bool { return strings.HasPrefix(operation, "dispatch.") }
func isEventOperation(operation string) bool    { return strings.HasPrefix(operation, "event.") }

func isContractOperation(operation string) bool {
	return isPublicationOperation(operation) || isAssetOperation(operation) || isRegistryOperation(operation) || isRunOperation(operation) || isDispatchOperation(operation) || isEventOperation(operation)
}

func requiresControlPlaneAuthentication(operation string) bool {
	return operation != "health.live" && operation != "health.ready" && operation != "asset.upload" && operation != "asset.download" && operation != "dispatch.jwks" && operation != "event.batch"
}

func authenticateBearer(ctx context.Context, values []string, metadata platform.RequestMetadata, authenticate AuthenticateFunc) (AuthenticatedPrincipal, error) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return AuthenticatedPrincipal{}, errors.New("invalid authorization")
	}
	credential := strings.TrimPrefix(values[0], "Bearer ")
	if credential == "" || strings.TrimSpace(credential) != credential || strings.ContainsAny(credential, " \t\r\n,") {
		return AuthenticatedPrincipal{}, errors.New("invalid authorization")
	}
	return authenticate(ctx, credential, metadata)
}

func bodyIsEmpty(body io.Reader) (bool, error) {
	if body == nil {
		return true, nil
	}
	content, err := io.ReadAll(io.LimitReader(body, 1))
	if err != nil {
		return false, err
	}
	return len(content) == 0, nil
}

func invoke(application *platform.Platform, writer http.ResponseWriter, request *http.Request, next http.Handler) {
	defer func() {
		if recover() != nil {
			resetResponse(writer)
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Status: "error"})
		}
	}()
	operation := classifyOperation(request.Method, request.URL.Path)
	if operation == "health.live" || operation == "health.ready" || isContractOperation(operation) {
		next.ServeHTTP(writer, request)
		return
	}
	if err := application.Execute(request.Context(), func(ctx context.Context) error {
		next.ServeHTTP(writer, request.WithContext(ctx))
		return nil
	}); err != nil {
		resetResponse(writer)
		writeJSON(writer, http.StatusServiceUnavailable, errorResponse{Status: "unavailable"})
		return
	}
}

func classifyOperation(method, path string) string {
	switch path {
	case "/v1/health/live":
		return "health.live"
	case "/v1/health/ready":
		return "health.ready"
	case "/.well-known/arop-jwks.json":
		if method == http.MethodGet {
			return "dispatch.jwks"
		}
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "agent-definitions" && segments[3] == "versions" {
		return "publication.publish"
	}
	if method == http.MethodGet && len(segments) == 5 && segments[0] == "v1" && segments[1] == "agent-definitions" && segments[3] == "versions" {
		return "publication.get"
	}
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "runs" && segments[3] == "assets:exchange" {
		return "asset.exchange"
	}
	if len(segments) == 3 && segments[0] == "v1" && segments[1] == "asset-content" {
		if method == http.MethodPut {
			return "asset.upload"
		}
		if method == http.MethodGet {
			return "asset.download"
		}
	}
	if len(segments) == 4 && segments[0] == "v1" && segments[1] == "registry" && segments[2] == "instances" {
		switch method {
		case http.MethodPut:
			return "registry.register"
		case http.MethodPatch:
			return "registry.operate"
		case http.MethodDelete:
			return "registry.deregister"
		}
	}
	if method == http.MethodPost && len(segments) == 5 && segments[0] == "v1" && segments[1] == "registry" && segments[2] == "instances" && segments[4] == "drain" {
		return "registry.drain"
	}
	if method == http.MethodPost && len(segments) == 5 && segments[0] == "v1" && segments[1] == "registry" && segments[2] == "leases" && segments[4] == "keepalive" {
		return "registry.keepalive"
	}
	if method == http.MethodGet && len(segments) == 5 && segments[0] == "v1" && segments[1] == "discovery" && segments[2] == "agents" && segments[4] == "instances" {
		return "registry.discover"
	}
	if method == http.MethodGet && len(segments) == 3 && segments[0] == "v1" && segments[1] == "discovery" && segments[2] == "changes" {
		// P15 freezes the P16 Watch authentication boundary without
		// registering its handler. Authenticated callers therefore receive the
		// mux's 404 until P16, never a fallback scope or an accidental route.
		return "registry.watch"
	}
	if method == http.MethodPost && len(segments) == 2 && segments[0] == "v1" && segments[1] == "agent-runs" {
		return "run.create"
	}
	if method == http.MethodGet && len(segments) == 3 && segments[0] == "v1" && segments[1] == "agent-runs" {
		return "run.read"
	}
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "agent-runs" && segments[3] == "commands" {
		return "run.command"
	}
	if method == http.MethodPost && len(segments) == 3 && segments[0] == "v1" && segments[1] == "agent-runs" && strings.HasSuffix(segments[2], ":dispatch") {
		return "dispatch.issue"
	}
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "agent-runs" && segments[3] == "event-session" {
		return "event.session"
	}
	if method == http.MethodPost && len(segments) == 4 && segments[0] == "v1" && segments[1] == "agent-runs" && segments[3] == "events:batch" {
		return "event.batch"
	}
	return "http.unmatched"
}

func scopesForOperation(operation string) []string {
	switch operation {
	case "publication.publish":
		return []string{"agent:publish"}
	case "publication.get":
		return []string{"agent:read"}
	case "asset.exchange":
		return []string{"asset:exchange"}
	case "registry.register":
		return []string{"registry:register"}
	case "registry.operate":
		return []string{"registry:operate"}
	case "registry.keepalive", "registry.drain", "registry.deregister":
		return []string{"registry:write"}
	case "registry.discover", "registry.watch":
		return []string{"registry:discover"}
	case "run.create":
		return []string{"run:create"}
	case "run.read":
		return []string{"run:read"}
	case "run.command":
		return []string{"run:command"}
	case "dispatch.issue":
		return []string{"run:dispatch"}
	case "event.session":
		return []string{"event:session"}
	case "dispatch.jwks":
		return nil
	default:
		return []string{"secret.read"}
	}
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseCapture() *responseCapture           { return &responseCapture{header: make(http.Header)} }
func (capture *responseCapture) Header() http.Header { return capture.header }
func (capture *responseCapture) WriteHeader(status int) {
	if capture.status == 0 {
		capture.status = status
	}
}
func (capture *responseCapture) Write(data []byte) (int, error) {
	if capture.status == 0 {
		capture.status = http.StatusOK
	}
	return capture.body.Write(data)
}
func (capture *responseCapture) statusCode() int {
	if capture.status == 0 {
		return http.StatusOK
	}
	return capture.status
}

func resetResponse(writer http.ResponseWriter) {
	if capture, ok := writer.(*responseCapture); ok {
		capture.header = make(http.Header)
		capture.status = 0
		capture.body.Reset()
		setSecurityHeaders(capture.header)
	}
}

func copyResponse(writer http.ResponseWriter, capture *responseCapture) {
	for key, values := range capture.Header() {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(capture.statusCode())
	_, _ = io.Copy(writer, &capture.body)
}

func setSecurityHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
