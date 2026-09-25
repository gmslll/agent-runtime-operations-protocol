package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPublishAndGetContracts(t *testing.T) {
	t.Parallel()
	publish := PublishRequest{AgentID: "example.agent", IdempotencyKey: "request-1", Bundle: []byte("zip"), Caller: validCaller()}
	publish.Metadata.TraceFlags = "01"
	if err := publish.Validate(); err != nil {
		t.Fatalf("publish request: %v", err)
	}
	if publish.Metadata.TraceFlags != "01" {
		t.Fatal("platform request metadata lost trace flags")
	}
	publish.Bundle = make([]byte, MaxBundleBytes+1)
	if err := publish.Validate(); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if err := (GetRequest{AgentID: "example.agent", Version: "1.2.3-rc.1", Caller: validCaller()}).Validate(); err != nil {
		t.Fatalf("get request: %v", err)
	}
	result := PublishResult{AgentID: "example.agent", Version: "1.2.3", ManifestDigest: digest("a"), Location: "/v1/agent-definitions/example.agent/versions/1.2.3", ETag: `"` + digest("a") + `"`}
	if err := result.Validate(); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	result.ETag = `W/"` + result.ManifestDigest + `"`
	if err := result.Validate(); err == nil {
		t.Fatal("weak ETag accepted")
	}
}

func TestPublishFingerprintBindsCallerOperationAndSemanticDigests(t *testing.T) {
	t.Parallel()
	request := PublishRequest{AgentID: "example.agent", IdempotencyKey: "request-1", Bundle: []byte("zip"), Caller: validCaller()}
	bundle := validBundle()
	fingerprint := NewPublishFingerprint(request, bundle)
	if err := fingerprint.Validate(); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fingerprint.TenantID != request.Caller.TenantID || fingerprint.PrincipalID != request.Caller.PrincipalID || fingerprint.CredentialID != request.Caller.CredentialID || len(fingerprint.Scopes) != len(request.Caller.Scopes) || fingerprint.Operation != OperationPublish || fingerprint.AgentID != request.AgentID || fingerprint.Version != bundle.Version || fingerprint.ManifestDigest != bundle.ManifestDigest || fingerprint.BundleSemanticDigest != bundle.BundleSemanticDigest {
		t.Fatalf("incomplete fingerprint: %#v", fingerprint)
	}
	fingerprint.Operation = OperationGet
	if err := fingerprint.Validate(); err == nil {
		t.Fatal("caller-selected publish operation accepted")
	}
}

func TestValidatedBundleRejectsNetworkAndUnsafeHostClasses(t *testing.T) {
	t.Parallel()
	bundle := validBundle()
	if err := bundle.Validate(); err != nil {
		t.Fatalf("valid bundle: %v", err)
	}
	for _, class := range []ReferenceClass{ReferenceNetwork, ReferenceAuthority, ReferenceAbsolutePath, ReferenceFileScheme, ReferenceParentEscape, ReferenceNonPortable} {
		candidate := bundle
		candidate.References = []BundleReference{{SourcePath: "agent-manifest.json", Kind: ReferenceSchemaRef, Class: class, TargetPath: "schema.json"}}
		if err := candidate.Validate(); err == nil {
			t.Fatalf("reference class %q accepted", class)
		}
	}
	for _, class := range []HostClass{HostLoopback, HostLinkLocal, HostPrivate, HostMetadata, HostInvalid} {
		candidate := bundle
		candidate.AllowedHosts = []AllowedHost{{Declared: "denied.invalid", Canonical: "denied.invalid", Class: class}}
		if err := candidate.Validate(); err == nil {
			t.Fatalf("host class %q accepted", class)
		}
	}
	brokenReference := bundle
	brokenReference.References = []BundleReference{{SourcePath: "agent-manifest.json", Kind: ReferenceSchemaRef, Class: ReferenceDocumentFragment, TargetPath: "schema.json", Fragment: "#/$defs/x"}}
	if err := brokenReference.Validate(); err == nil {
		t.Fatal("fragment classification with target path accepted")
	}
	brokenReference.References = []BundleReference{{SourcePath: "agent-manifest.json", Kind: ReferenceSchemaRef, Class: ReferenceDocumentFragment, Fragment: "#bad%20anchor"}}
	if err := brokenReference.Validate(); err == nil {
		t.Fatal("percent-encoded fragment accepted")
	}
	brokenHost := bundle
	brokenHost.AllowedHosts = []AllowedHost{{Declared: "*.Example.Invalid", Canonical: "example.invalid", Class: HostWildcardName}}
	if err := brokenHost.Validate(); err == nil {
		t.Fatal("wildcard classification without canonical wildcard accepted")
	}
}

func TestManifestDigestRecomputedAtBundleAndRecordBoundaries(t *testing.T) {
	t.Parallel()
	bundle := validBundle()
	digester := stubDigester{digest: bundle.ManifestDigest}
	if err := bundle.ValidateWithDigest(context.Background(), digester); err != nil {
		t.Fatalf("bundle digest: %v", err)
	}
	digester.digest = digest("f")
	if err := bundle.ValidateWithDigest(context.Background(), digester); err == nil {
		t.Fatal("bundle manifest digest mismatch accepted")
	}
	record := validRecord()
	if err := record.ValidateWithDigest(context.Background(), stubDigester{digest: record.ManifestDigest}); err != nil {
		t.Fatalf("record digest: %v", err)
	}
	if err := record.ValidateWithDigest(context.Background(), stubDigester{digest: digest("f")}); err == nil {
		t.Fatal("stored manifest digest mismatch accepted")
	}
	created, err := NewRecord(context.Background(), stubDigester{digest: bundle.ManifestDigest}, validCaller(), bundle, strings.Repeat("d", 64), strings.Repeat("e", 64), time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatalf("new record: %v", err)
	}
	bundle.CanonicalManifest[0] = '['
	if created.CanonicalManifest[0] != '{' {
		t.Fatal("record aliases validator manifest bytes")
	}
}

func TestRecordCopiesCanonicalManifest(t *testing.T) {
	t.Parallel()
	record := validRecord()
	if err := record.Validate(); err != nil {
		t.Fatalf("record: %v", err)
	}
	result := record.GetResult()
	result.CanonicalManifest[0] = '['
	if record.CanonicalManifest[0] != '{' {
		t.Fatal("read result aliases persisted manifest")
	}
}

func TestTypedErrorsAreStableAndRedacted(t *testing.T) {
	t.Parallel()
	secret := errors.New("database DSN password=do-not-emit")
	err := NewError(CategoryDependency, ReasonDependencyUnavailable, secret)
	joined := errors.Join(err, NewError(CategoryValidation, ReasonBundleInvalid, secret))
	jsonWire, jsonErr := json.Marshal(err)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}
	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Error("publication", "error", joined)
	outputs := []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), string(jsonWire), joined.Error(), logOutput.String()}
	for _, output := range outputs {
		if strings.Contains(output, "password") || strings.Contains(output, "do-not-emit") {
			t.Fatalf("error leaked cause: %q", output)
		}
	}
	failure, ok := AsError(err)
	if !ok || !failure.Retryable || failure.Category != CategoryDependency || errors.Is(err, secret) {
		t.Fatalf("typed error mismatch: %#v, %v", failure, ok)
	}
	authn, _ := AsError(NewError(CategoryAuthentication, ReasonAuthenticationRequired))
	authz, _ := AsError(NewError(CategoryAuthorization, ReasonPublicationForbidden))
	if authn.Retryable || authz.Retryable {
		t.Fatal("authentication or authorization error marked retryable")
	}
}

func TestDependenciesAndInterfacesStayInternal(t *testing.T) {
	t.Parallel()
	if err := (Dependencies{}).Validate(); err == nil {
		t.Fatal("empty dependencies accepted")
	}
	var _ PublicationService = stubService{}
	var _ Repository = stubRepository{}
	var _ BundleValidator = stubValidator{}
	var _ Authorizer = stubAuthorizer{}
	var _ ManifestDigester = stubDigester{}
	var _ RequestFingerprinter = stubFingerprinter{}
}

func validBundle() ValidatedBundle {
	return ValidatedBundle{
		AgentID: "example.agent", Version: "1.0.0", ManifestDigest: digest("a"), BundleSemanticDigest: digest("b"), CanonicalManifest: []byte(`{"kind":"AgentManifest"}`),
		References:   []BundleReference{{SourcePath: "agent-manifest.json", Kind: ReferenceSchemaRef, Class: ReferenceBundleRelative, TargetPath: "schemas/input.json"}},
		AllowedHosts: []AllowedHost{{Declared: "api.example.invalid", Canonical: "api.example.invalid", Class: HostPublicName}},
	}
}

func validCaller() Caller {
	return Caller{TenantID: "tenant-a", PrincipalID: "prn_018f0000-0000-7000-8000-000000000001", CredentialID: "cred_018f0000-0000-7000-8000-000000000002", Scopes: []string{"agent:publish", "agent:read"}}
}

func validRecord() Record {
	return Record{TenantID: "tenant-a", PublisherPrincipalID: validCaller().PrincipalID, AgentID: "example.agent", Version: "1.0.0", ManifestDigest: digest("b"), BundleSemanticDigest: digest("c"), CanonicalManifest: []byte(`{"kind":"AgentManifest"}`), IdempotencyKeyDigest: strings.Repeat("d", 64), IdempotencyRequestDigest: strings.Repeat("e", 64), PublishedAt: time.Unix(1, 0).UTC(), Revision: 1}
}

func digest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

type stubService struct{}

func (stubService) Publish(context.Context, PublishRequest) (PublishResult, error) {
	return PublishResult{}, nil
}
func (stubService) Get(context.Context, GetRequest) (GetResult, error) { return GetResult{}, nil }

type stubRepository struct{}

func (stubRepository) Create(context.Context, *Record) error { return nil }
func (stubRepository) Get(context.Context, string, string, string) (Record, error) {
	return Record{}, nil
}
func (stubRepository) GetByIdempotencyDigest(context.Context, string, string) (Record, error) {
	return Record{}, nil
}

type stubAuthorizer struct{}

func (stubAuthorizer) Authorize(context.Context, AuthorizationRequest) error { return nil }

type stubDigester struct{ digest string }

func (digester stubDigester) DigestManifest(context.Context, []byte) (string, error) {
	return digester.digest, nil
}

type stubFingerprinter struct{}

func (stubFingerprinter) DigestRequest(context.Context, RequestFingerprint) (string, error) {
	return strings.Repeat("f", 64), nil
}

type stubValidator struct{}

func (stubValidator) ValidateBundle(context.Context, []byte) (ValidatedBundle, error) {
	return ValidatedBundle{}, nil
}
