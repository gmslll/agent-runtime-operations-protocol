// Package publication defines the reference Control Plane's internal
// publication contract. It is deliberately not a public wire model: the root
// CLI and public SDK consume the P11 generated contract instead of importing
// this nested-module internal package.
package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
)

const MaxBundleBytes = 10 * 1024 * 1024

var (
	agentIDPattern         = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	versionPattern         = regexp.MustCompile(`^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	digestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	digestHexPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tenantPattern          = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	principalPattern       = regexp.MustCompile(`^prn_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	credentialPattern      = regexp.MustCompile(`^cred_[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	scopePattern           = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
	hostnamePattern        = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	portableSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

type Operation string

const (
	OperationPublish Operation = "publication.publish"
	OperationGet     Operation = "publication.get"
)

func (operation Operation) valid() bool {
	return operation == OperationPublish || operation == OperationGet
}

// Caller is the already authenticated identity presented to the Publication
// service. Authentication and authorization are never inferred from request
// fields, Manifest ownership, or an empty scope set.
type Caller struct {
	TenantID     string
	PrincipalID  string
	CredentialID string
	Scopes       []string
}

func (caller Caller) Validate() error {
	if !tenantPattern.MatchString(caller.TenantID) || !principalPattern.MatchString(caller.PrincipalID) || !credentialPattern.MatchString(caller.CredentialID) || len(caller.Scopes) == 0 {
		return errors.New("publication caller is invalid")
	}
	seen := map[string]struct{}{}
	for _, scope := range caller.Scopes {
		if !scopePattern.MatchString(scope) {
			return errors.New("publication caller scope is invalid")
		}
		if _, exists := seen[scope]; exists {
			return errors.New("publication caller scope is duplicated")
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// PublishRequest owns Bundle after the call starts. Callers must not mutate or
// reuse the byte slice concurrently. Implementations must validate a private
// copy before publishing any result.
type PublishRequest struct {
	AgentID        string
	IdempotencyKey string
	Bundle         []byte
	Caller         Caller
	Metadata       platform.RequestMetadata
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
	if err := request.Caller.Validate(); err != nil {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	return nil
}

type GetRequest struct {
	AgentID  string
	Version  string
	Caller   Caller
	Metadata platform.RequestMetadata
}

func (request GetRequest) Validate() error {
	if !agentIDPattern.MatchString(request.AgentID) || !versionPattern.MatchString(request.Version) {
		return errors.New("publication lookup identity is invalid")
	}
	if err := request.Caller.Validate(); err != nil {
		return NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	return nil
}

// RequestFingerprint is the complete semantic identity of one publish
// attempt. Implementations hash its canonical encoding, never the raw
// credential or idempotency key. Operation is fixed by Publish and cannot be
// supplied by callers.
type RequestFingerprint struct {
	TenantID             string
	PrincipalID          string
	CredentialID         string
	Scopes               []string
	Operation            Operation
	AgentID              string
	Version              string
	ManifestDigest       string
	BundleSemanticDigest string
}

func NewPublishFingerprint(request PublishRequest, bundle ValidatedBundle) RequestFingerprint {
	scopes := slices.Clone(request.Caller.Scopes)
	sort.Strings(scopes)
	return RequestFingerprint{TenantID: request.Caller.TenantID, PrincipalID: request.Caller.PrincipalID, CredentialID: request.Caller.CredentialID, Scopes: scopes, Operation: OperationPublish, AgentID: request.AgentID, Version: bundle.Version, ManifestDigest: bundle.ManifestDigest, BundleSemanticDigest: bundle.BundleSemanticDigest}
}

func (fingerprint RequestFingerprint) Validate() error {
	if !tenantPattern.MatchString(fingerprint.TenantID) || !principalPattern.MatchString(fingerprint.PrincipalID) || !credentialPattern.MatchString(fingerprint.CredentialID) || fingerprint.Operation != OperationPublish || !agentIDPattern.MatchString(fingerprint.AgentID) || !versionPattern.MatchString(fingerprint.Version) || !digestPattern.MatchString(fingerprint.ManifestDigest) || !digestPattern.MatchString(fingerprint.BundleSemanticDigest) {
		return errors.New("publication request fingerprint is invalid")
	}
	if len(fingerprint.Scopes) == 0 || !sort.StringsAreSorted(fingerprint.Scopes) {
		return errors.New("publication request fingerprint scopes are not canonical")
	}
	for index, scope := range fingerprint.Scopes {
		if !scopePattern.MatchString(scope) || index > 0 && scope == fingerprint.Scopes[index-1] {
			return errors.New("publication request fingerprint scope is invalid")
		}
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

func (reference BundleReference) Validate() error {
	if !reference.Kind.valid() || !reference.Class.Allowed() || !validPortablePath(reference.SourcePath) {
		return errors.New("bundle reference classification is invalid")
	}
	if reference.Fragment != "" && !strings.HasPrefix(reference.Fragment, "#") {
		return errors.New("bundle reference fragment is invalid")
	}
	if reference.Fragment != "" && !validPortableFragment(reference.Fragment) {
		return errors.New("bundle reference fragment is not portable")
	}
	switch reference.Class {
	case ReferenceDocumentFragment:
		if reference.TargetPath != "" || reference.Fragment == "" {
			return errors.New("document fragment reference is invalid")
		}
	case ReferenceBundleRelative:
		if !validPortablePath(reference.TargetPath) {
			return errors.New("bundle-relative reference target is invalid")
		}
	}
	return nil
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

func (host AllowedHost) Validate() error {
	if !host.Class.Allowed() || host.Declared == "" || host.Canonical == "" || host.Canonical != strings.ToLower(host.Canonical) {
		return errors.New("allowed host classification is invalid")
	}
	switch host.Class {
	case HostPublicName:
		if strings.ToLower(host.Declared) != host.Canonical || !validPublicHostname(host.Canonical) || net.ParseIP(host.Canonical) != nil {
			return errors.New("public host classification is invalid")
		}
	case HostWildcardName:
		if strings.ToLower(host.Declared) != host.Canonical || !strings.HasPrefix(host.Canonical, "*.") || !validPublicHostname(strings.TrimPrefix(host.Canonical, "*.")) {
			return errors.New("wildcard host classification is invalid")
		}
	case HostGlobalIPLiteral:
		ip := net.ParseIP(host.Declared)
		if ip == nil || ip.To4() == nil || ip.String() != host.Canonical || deniedIPLiteral(ip) || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return errors.New("global IP host classification is invalid")
		}
	}
	return nil
}

// ValidatedBundle is the exact output of strict bundle validation: offline
// reference closure first, semantic Manifest digest second, publication last.
// Endpoint is intentionally absent because AgentManifest v1 rejects it.
type ValidatedBundle struct {
	AgentID              string
	Version              string
	ManifestDigest       string
	BundleSemanticDigest string
	CanonicalManifest    json.RawMessage
	References           []BundleReference
	AllowedHosts         []AllowedHost
}

func (bundle ValidatedBundle) Validate() error {
	if !agentIDPattern.MatchString(bundle.AgentID) || !versionPattern.MatchString(bundle.Version) || !digestPattern.MatchString(bundle.ManifestDigest) || !digestPattern.MatchString(bundle.BundleSemanticDigest) {
		return errors.New("validated bundle identity is invalid")
	}
	if !validJSONObject(bundle.CanonicalManifest) {
		return errors.New("validated bundle manifest is invalid")
	}
	for _, reference := range bundle.References {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("bundle contains a denied reference classification: %w", err)
		}
	}
	for _, host := range bundle.AllowedHosts {
		if err := host.Validate(); err != nil {
			return errors.New("bundle contains a denied host classification")
		}
	}
	return nil
}

type Record struct {
	TenantID                 string
	PublisherPrincipalID     string
	AgentID                  string
	Version                  string
	ManifestDigest           string
	BundleSemanticDigest     string
	CanonicalManifest        []byte
	IdempotencyKeyDigest     string
	IdempotencyRequestDigest string
	PublishedAt              time.Time
	Revision                 int64
}

func NewRecord(ctx context.Context, digester ManifestDigester, caller Caller, bundle ValidatedBundle, idempotencyKeyDigest, idempotencyRequestDigest string, publishedAt time.Time) (Record, error) {
	if err := caller.Validate(); err != nil {
		return Record{}, NewError(CategoryAuthentication, ReasonAuthenticationRequired)
	}
	if err := bundle.ValidateWithDigest(ctx, digester); err != nil {
		return Record{}, NewError(CategoryValidation, ReasonBundleInvalid, err)
	}
	record := Record{
		TenantID: caller.TenantID, PublisherPrincipalID: caller.PrincipalID,
		AgentID: bundle.AgentID, Version: bundle.Version, ManifestDigest: bundle.ManifestDigest, BundleSemanticDigest: bundle.BundleSemanticDigest,
		CanonicalManifest: slices.Clone(bundle.CanonicalManifest), IdempotencyKeyDigest: idempotencyKeyDigest, IdempotencyRequestDigest: idempotencyRequestDigest,
		PublishedAt: publishedAt, Revision: 1,
	}
	if err := record.ValidateWithDigest(ctx, digester); err != nil {
		return Record{}, NewError(CategoryValidation, ReasonBundleInvalid, err)
	}
	return record, nil
}

func (record Record) Validate() error {
	if !tenantPattern.MatchString(record.TenantID) || !principalPattern.MatchString(record.PublisherPrincipalID) || !agentIDPattern.MatchString(record.AgentID) || !versionPattern.MatchString(record.Version) || !digestPattern.MatchString(record.ManifestDigest) || !digestPattern.MatchString(record.BundleSemanticDigest) {
		return errors.New("publication record identity is invalid")
	}
	if !validJSONObject(record.CanonicalManifest) || !digestHexPattern.MatchString(record.IdempotencyKeyDigest) || !digestHexPattern.MatchString(record.IdempotencyRequestDigest) || record.PublishedAt.IsZero() || record.PublishedAt.Location() != time.UTC || record.Revision < 1 {
		return errors.New("publication record metadata is invalid")
	}
	return nil
}

// ValidateWithDigest is the mandatory service/storage boundary check. The
// injected digester must be the repository's pinned RFC 8785 JCS implementation;
// this package deliberately does not invent another canonicalization routine.
func (bundle ValidatedBundle) ValidateWithDigest(ctx context.Context, digester ManifestDigester) error {
	if err := bundle.Validate(); err != nil {
		return err
	}
	if digester == nil {
		return errors.New("manifest digester is required")
	}
	digest, err := digester.DigestManifest(ctx, slices.Clone(bundle.CanonicalManifest))
	if err != nil || digest != bundle.ManifestDigest {
		return errors.New("validated bundle manifest digest mismatch")
	}
	return nil
}

func (record Record) ValidateWithDigest(ctx context.Context, digester ManifestDigester) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if digester == nil {
		return errors.New("manifest digester is required")
	}
	digest, err := digester.DigestManifest(ctx, slices.Clone(record.CanonicalManifest))
	if err != nil || digest != record.ManifestDigest {
		return errors.New("stored publication manifest digest mismatch")
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

func validPortablePath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	value = strings.TrimPrefix(value, "./")
	if value == "" {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || !portableSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

func validJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func validPublicHostname(value string) bool {
	if !hostnamePattern.MatchString(value) || !strings.Contains(value, ".") || strings.Contains(value, "..") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	if value == "localhost" || strings.HasSuffix(value, ".localhost") || value == "metadata" || value == "metadata.google.internal" || value == "metadata.azure.internal" {
		return false
	}
	return true
}

func validPortableFragment(value string) bool {
	if value == "#" {
		return true
	}
	body := strings.TrimPrefix(value, "#")
	if body == "" || strings.Contains(body, "%") {
		return false
	}
	if !strings.HasPrefix(body, "/") {
		return portableSegmentPattern.MatchString(body)
	}
	for index := 0; index < len(body); index++ {
		character := body[index]
		if character >= 0x80 || character < 0x21 || character == '\\' {
			return false
		}
		if character == '~' {
			if index+1 >= len(body) || body[index+1] != '0' && body[index+1] != '1' {
				return false
			}
			index++
		}
	}
	return true
}
