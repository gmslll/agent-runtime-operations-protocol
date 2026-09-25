package publication

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPublishAndGetContracts(t *testing.T) {
	t.Parallel()
	publish := PublishRequest{AgentID: "example.agent", IdempotencyKey: "request-1", Bundle: []byte("zip")}
	if err := publish.Validate(); err != nil {
		t.Fatalf("publish request: %v", err)
	}
	publish.Bundle = make([]byte, MaxBundleBytes+1)
	if err := publish.Validate(); err == nil {
		t.Fatal("oversized bundle accepted")
	}
	if err := (GetRequest{AgentID: "example.agent", Version: "1.2.3-rc.1"}).Validate(); err != nil {
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
}

func TestRecordCopiesCanonicalManifest(t *testing.T) {
	t.Parallel()
	record := Record{AgentID: "example.agent", Version: "1.0.0", ManifestDigest: digest("b"), BundleDigest: digest("c"), CanonicalManifest: []byte(`{"kind":"AgentManifest"}`), IdempotencyKeyDigest: strings.Repeat("d", 64), PublishedAt: time.Unix(1, 0).UTC(), Revision: 1}
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
	if strings.Contains(err.Error(), "password") || err.Error() != string(ReasonDependencyUnavailable) {
		t.Fatalf("error leaked cause: %q", err)
	}
	failure, ok := AsError(err)
	if !ok || !failure.Retryable || failure.Category != CategoryDependency || !errors.Is(err, secret) {
		t.Fatalf("typed error mismatch: %#v, %v", failure, ok)
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
}

func validBundle() ValidatedBundle {
	return ValidatedBundle{
		AgentID: "example.agent", Version: "1.0.0", ManifestDigest: digest("a"), BundleDigest: digest("b"), CanonicalManifest: []byte(`{"kind":"AgentManifest"}`),
		References:   []BundleReference{{SourcePath: "agent-manifest.json", Kind: ReferenceSchemaRef, Class: ReferenceBundleRelative, TargetPath: "schemas/input.json"}},
		AllowedHosts: []AllowedHost{{Declared: "api.example.invalid", Canonical: "api.example.invalid", Class: HostPublicName}},
	}
}

func digest(character string) string { return "sha256:" + strings.Repeat(character, 64) }

type stubService struct{}

func (stubService) Publish(context.Context, PublishRequest) (PublishResult, error) {
	return PublishResult{}, nil
}
func (stubService) Get(context.Context, GetRequest) (GetResult, error) { return GetResult{}, nil }

type stubRepository struct{}

func (stubRepository) Create(context.Context, *Record) error               { return nil }
func (stubRepository) Get(context.Context, string, string) (Record, error) { return Record{}, nil }
func (stubRepository) GetByIdempotencyDigest(context.Context, string) (Record, error) {
	return Record{}, nil
}

type stubValidator struct{}

func (stubValidator) ValidateBundle(context.Context, []byte) (ValidatedBundle, error) {
	return ValidatedBundle{}, nil
}
