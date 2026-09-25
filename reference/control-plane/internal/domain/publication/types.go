// Package publication defines the reference Control Plane's internal
// publication contract. It is deliberately not a public wire model: the root
// CLI and public SDK consume the P11 generated contract instead of importing
// this nested-module internal package.
package publication

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const MaxBundleBytes = 10 * 1024 * 1024

var (
	agentIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	versionPattern   = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	digestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	digestHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Correlation carries only the identifiers needed to create the P08 durable
// observation. It cannot carry request bodies, headers, URLs, or attributes.
type Correlation struct {
	RequestID    string
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// PublishRequest owns Bundle after the call starts. Callers must not mutate or
// reuse the byte slice concurrently. Implementations must validate a private
// copy before publishing any result.
type PublishRequest struct {
	AgentID        string
	IdempotencyKey string
	Bundle         []byte
	Correlation    Correlation
}

func (request PublishRequest) Validate() error {
	if !agentIDPattern.MatchString(request.AgentID) {
		return errors.New("publication agent identifier is invalid")
	}
	if !validIdempotencyKey(request.IdempotencyKey) {
		return errors.New("publication idempotency key is invalid")
	}
	if len(request.Bundle) == 0 {
		return errors.New("publication bundle is empty")
	}
	if len(request.Bundle) > MaxBundleBytes {
		return errors.New("publication bundle exceeds the archive limit")
	}
	return nil
}

type GetRequest struct {
	AgentID     string
	Version     string
	Correlation Correlation
}

func (request GetRequest) Validate() error {
	if !agentIDPattern.MatchString(request.AgentID) || !versionPattern.MatchString(request.Version) {
		return errors.New("publication lookup identity is invalid")
	}
	return nil
}

// PublishResult maps to the P11 201 response. The HTTP adapter emits no body;
// it uses Location and ETag from this result.
type PublishResult struct {
	AgentID        string
	Version        string
	ManifestDigest string
	Location       string
	ETag           string
}

func (result PublishResult) Validate() error {
	if !agentIDPattern.MatchString(result.AgentID) || !versionPattern.MatchString(result.Version) || !digestPattern.MatchString(result.ManifestDigest) {
		return errors.New("publication result identity is invalid")
	}
	wantLocation := "/v1/agent-definitions/" + result.AgentID + "/versions/" + result.Version
	if result.Location != wantLocation || result.ETag != `"`+result.ManifestDigest+`"` {
		return errors.New("publication result validators are invalid")
	}
	return nil
}

// GetResult carries the already validated semantic AgentManifest JSON. It is
// not a second DTO; the HTTP adapter must decode/encode it through the public
// generated P11 AgentManifest model in consumer-compatible mode.
type GetResult struct {
	AgentID           string
	Version           string
	ManifestDigest    string
	CanonicalManifest json.RawMessage
	ETag              string
}

func (result GetResult) Validate() error {
	if !agentIDPattern.MatchString(result.AgentID) || !versionPattern.MatchString(result.Version) || !digestPattern.MatchString(result.ManifestDigest) {
		return errors.New("publication read result identity is invalid")
	}
	if result.ETag != `"`+result.ManifestDigest+`"` || !validJSONObject(result.CanonicalManifest) {
		return errors.New("publication read result payload is invalid")
	}
	return nil
}

type ReferenceKind string

const (
	ReferenceSchemaRef          ReferenceKind = "schema-ref"
	ReferenceSchemaID           ReferenceKind = "schema-id"
	ReferenceExtensionSchemaRef ReferenceKind = "extension-schema-ref"
)

type ReferenceClass string

const (
	ReferenceBundleRelative   ReferenceClass = "bundle-relative"
	ReferenceDocumentFragment ReferenceClass = "document-fragment"
	ReferenceNetwork          ReferenceClass = "network"
	ReferenceAuthority        ReferenceClass = "authority"
	ReferenceAbsolutePath     ReferenceClass = "absolute-path"
	ReferenceFileScheme       ReferenceClass = "file-scheme"
	ReferenceParentEscape     ReferenceClass = "parent-escape"
	ReferenceNonPortable      ReferenceClass = "non-portable"
)

func (class ReferenceClass) Allowed() bool {
	return class == ReferenceBundleRelative || class == ReferenceDocumentFragment
}

// BundleReference is a zero-network classification result. TargetPath is
// archive-root-relative and empty for a document-only fragment. No resolver is
// permitted to perform DNS or network I/O while producing it.
type BundleReference struct {
	SourcePath string
	Kind       ReferenceKind
	Class      ReferenceClass
	TargetPath string
	Fragment   string
}

type HostClass string

const (
	HostPublicName      HostClass = "public-name"
	HostWildcardName    HostClass = "wildcard-public-name"
	HostGlobalIPLiteral HostClass = "global-ip-literal"
	HostLoopback        HostClass = "loopback"
	HostLinkLocal       HostClass = "link-local"
	HostPrivate         HostClass = "private"
	HostMetadata        HostClass = "cloud-metadata"
	HostInvalid         HostClass = "invalid"
)

func (class HostClass) Allowed() bool {
	return class == HostPublicName || class == HostWildcardName || class == HostGlobalIPLiteral
}

// AllowedHost is only a static declaration classification. It is not a DNS,
// connection, certificate, or redirect authorization; those checks remain in
// their later owning phases.
type AllowedHost struct {
	Declared  string
	Canonical string
	Class     HostClass
}

// ValidatedBundle is the exact output of strict bundle validation: offline
// reference closure first, semantic Manifest digest second, publication last.
// Endpoint is intentionally absent because AgentManifest v1 rejects it.
type ValidatedBundle struct {
	AgentID           string
	Version           string
	ManifestDigest    string
	BundleDigest      string
	CanonicalManifest json.RawMessage
	References        []BundleReference
	AllowedHosts      []AllowedHost
}

func (bundle ValidatedBundle) Validate() error {
	if !agentIDPattern.MatchString(bundle.AgentID) || !versionPattern.MatchString(bundle.Version) || !digestPattern.MatchString(bundle.ManifestDigest) || !digestPattern.MatchString(bundle.BundleDigest) {
		return errors.New("validated bundle identity is invalid")
	}
	if !validJSONObject(bundle.CanonicalManifest) {
		return errors.New("validated bundle manifest is invalid")
	}
	for _, reference := range bundle.References {
		if !reference.Class.Allowed() || reference.SourcePath == "" || !reference.Kind.valid() {
			return fmt.Errorf("bundle contains a denied reference classification")
		}
	}
	for _, host := range bundle.AllowedHosts {
		if !host.Class.Allowed() || host.Declared == "" || host.Canonical == "" {
			return errors.New("bundle contains a denied host classification")
		}
	}
	return nil
}

type Record struct {
	AgentID              string
	Version              string
	ManifestDigest       string
	BundleDigest         string
	CanonicalManifest    []byte
	IdempotencyKeyDigest string
	PublishedAt          time.Time
	Revision             int64
}

func (record Record) Validate() error {
	if !agentIDPattern.MatchString(record.AgentID) || !versionPattern.MatchString(record.Version) || !digestPattern.MatchString(record.ManifestDigest) || !digestPattern.MatchString(record.BundleDigest) {
		return errors.New("publication record identity is invalid")
	}
	if !validJSONObject(record.CanonicalManifest) || !digestHexPattern.MatchString(record.IdempotencyKeyDigest) || record.PublishedAt.IsZero() || record.PublishedAt.Location() != time.UTC || record.Revision < 1 {
		return errors.New("publication record metadata is invalid")
	}
	return nil
}

func (record Record) GetResult() GetResult {
	return GetResult{
		AgentID: record.AgentID, Version: record.Version, ManifestDigest: record.ManifestDigest,
		CanonicalManifest: slices.Clone(record.CanonicalManifest), ETag: `"` + record.ManifestDigest + `"`,
	}
}

func validIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 200 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return !strings.ContainsAny(value, "\r\n")
}

func (kind ReferenceKind) valid() bool {
	return kind == ReferenceSchemaRef || kind == ReferenceSchemaID || kind == ReferenceExtensionSchemaRef
}

func validJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}
