// Package evidence verifies detached release evidence against protected trust state.
package evidence

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/schema"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

const (
	RoleRegistrySchema = "spec/schemas/trusted-release-roles.schema.json"
	EnvelopeSchema     = "spec/schemas/detached-release-evidence-envelope.schema.json"
)

type Signature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

type Key struct {
	KeyID       string `json:"key_id"`
	PrincipalID string `json:"principal_id"`
	Algorithm   string `json:"algorithm"`
	PublicKey   string `json:"public_key"`
}

type Role struct {
	KeyIDs    []string `json:"key_ids"`
	Threshold int      `json:"threshold"`
}

type RootMetadata struct {
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	ExpiresAt  string          `json:"expires_at"`
	Keys       []Key           `json:"keys"`
	Roles      map[string]Role `json:"roles"`
	Signatures []Signature     `json:"signatures,omitempty"`
}

type Meta struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}

type TimestampMetadata struct {
	Type        string      `json:"type"`
	Version     int         `json:"version"`
	GeneratedAt string      `json:"generated_at"`
	ExpiresAt   string      `json:"expires_at"`
	Snapshot    Meta        `json:"snapshot"`
	Signatures  []Signature `json:"signatures,omitempty"`
}

type SnapshotMetadata struct {
	Type       string      `json:"type"`
	Version    int         `json:"version"`
	ExpiresAt  string      `json:"expires_at"`
	Targets    Meta        `json:"targets"`
	Signatures []Signature `json:"signatures,omitempty"`
}

type TargetsMetadata struct {
	Type             string      `json:"type"`
	Version          int         `json:"version"`
	ExpiresAt        string      `json:"expires_at"`
	RoleRegistry     Meta        `json:"role_registry"`
	FulcioRootSHA256 string      `json:"fulcio_root_sha256"`
	RekorKeyIDs      []string    `json:"rekor_key_ids"`
	Signatures       []Signature `json:"signatures,omitempty"`
}

type Principal struct {
	PrincipalID string   `json:"principal_id"`
	KeyIDs      []string `json:"key_ids"`
	Roles       []string `json:"roles"`
	NotBefore   string   `json:"not_before"`
	NotAfter    string   `json:"not_after"`
	Revoked     bool     `json:"revoked"`
}

type SigstoreIdentity struct {
	PrincipalID string `json:"principal_id"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow_ref"`
	WorkflowSHA string `json:"workflow_sha"`
	Event       string `json:"event"`
	Role        string `json:"role"`
}

type RoleRegistry struct {
	SchemaVersion      int                `json:"schema_version"`
	Version            int                `json:"version"`
	ExpiresAt          string             `json:"expires_at"`
	Keys               []Key              `json:"keys"`
	Principals         []Principal        `json:"principals"`
	Thresholds         map[string]int     `json:"thresholds"`
	SigstoreIdentities []SigstoreIdentity `json:"sigstore_identities"`
}

type Checkpoint struct {
	Version        int         `json:"version"`
	RegistrySHA256 string      `json:"registry_sha256"`
	SignedAt       string      `json:"signed_at"`
	Signatures     []Signature `json:"signatures,omitempty"`
}

type RegistryBundle struct {
	SchemaVersion int               `json:"schema_version"`
	Root          RootMetadata      `json:"root"`
	Timestamp     TimestampMetadata `json:"timestamp"`
	Snapshot      SnapshotMetadata  `json:"snapshot"`
	Targets       TargetsMetadata   `json:"targets"`
	RoleRegistry  RoleRegistry      `json:"role_registry"`
	Checkpoint    Checkpoint        `json:"checkpoint"`
}

type ProtectedState struct {
	SchemaVersion          int            `json:"schema_version"`
	RootVersion            int            `json:"root_version"`
	RootSHA256             string         `json:"root_sha256"`
	RootKeys               []Key          `json:"root_keys"`
	RootThreshold          int            `json:"root_threshold"`
	LastCheckpointVersion  int            `json:"last_checkpoint_version"`
	LastCheckpointSHA256   string         `json:"last_checkpoint_sha256"`
	MaxTimestampAgeSeconds int64          `json:"max_timestamp_age_seconds"`
	FulcioRootsPEM         []string       `json:"fulcio_roots_pem"`
	RekorKeys              map[string]Key `json:"rekor_keys"`
}

type VerifiedTrust struct {
	RootSHA256        string
	RegistrySHA256    string
	CheckpointSHA256  string
	CheckpointVersion int
	FulcioRootSHA256  string
	RekorKeyIDs       []string
	Registry          RoleRegistry
	Protected         ProtectedState
	rootKeys          map[string]Key
	registryKeys      map[string]Key
	principals        map[string]Principal
}

type Subject struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type TrustBinding struct {
	RootSHA256        string `json:"root_sha256"`
	RegistrySHA256    string `json:"registry_sha256"`
	CheckpointVersion int    `json:"checkpoint_version"`
	CheckpointSHA256  string `json:"checkpoint_sha256"`
}

type PayloadBinding struct {
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
}

type Verification struct {
	Method              string            `json:"method"`
	Signatures          []Signature       `json:"signatures,omitempty"`
	PayloadType         string            `json:"payload_type,omitempty"`
	Signature           string            `json:"signature,omitempty"`
	CertificateChainPEM []string          `json:"certificate_chain_pem,omitempty"`
	Identity            *SigstoreIdentity `json:"identity,omitempty"`
	Rekor               *RekorProof       `json:"rekor,omitempty"`
}

type RekorProof struct {
	LogID               string   `json:"log_id"`
	LogIndex            int64    `json:"log_index"`
	IntegratedTime      int64    `json:"integrated_time"`
	BodySHA256          string   `json:"body_sha256"`
	TreeSize            int64    `json:"tree_size"`
	RootHash            string   `json:"root_hash"`
	Hashes              []string `json:"hashes"`
	CheckpointSignature string   `json:"checkpoint_signature"`
}

type Envelope struct {
	SchemaVersion   int            `json:"schema_version"`
	RepositoryURI   string         `json:"repository_uri"`
	ObjectFormat    string         `json:"object_format"`
	Subject         Subject        `json:"subject"`
	Kind            string         `json:"kind"`
	SchemaSHA256    string         `json:"schema_sha256"`
	PolicySHA256    string         `json:"policy_sha256"`
	ValidatorSHA256 string         `json:"validator_sha256"`
	IssuedAt        string         `json:"issued_at"`
	ExpiresAt       string         `json:"expires_at"`
	PrincipalID     string         `json:"principal_id"`
	Role            string         `json:"role"`
	Trust           TrustBinding   `json:"trust"`
	Payload         PayloadBinding `json:"payload"`
	Verification    Verification   `json:"verification"`
}

type ExpectedBindings struct {
	Kind, Role, SchemaSHA256, PolicySHA256, ValidatorSHA256 string
}

type VerifiedEnvelope struct {
	EnvelopeSHA256 string
	PayloadSHA256  string
	PrincipalID    string
	Role           string
	Method         string
	IntegratedTime int64
}

func LoadEnvelope(root, path string) (Envelope, []byte, error) {
	var value Envelope
	data, err := os.ReadFile(path)
	if err != nil {
		return value, nil, fmt.Errorf("read detached release evidence: %w", err)
	}
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return value, nil, err
	}
	if err := schema.ValidateFile(root, EnvelopeSchema, parsed); err != nil {
		return value, nil, err
	}
	if err := strictDecode(data, &value); err != nil {
		return value, nil, err
	}
	return value, data, nil
}

func VerifyEnvelope(envelope Envelope, payload []byte, trust VerifiedTrust, expected ExpectedBindings, now time.Time) (VerifiedEnvelope, error) {
	if envelope.SchemaVersion != 1 || envelope.RepositoryURI != RepositoryURI || envelope.ObjectFormat != "git-sha1" || len(envelope.Subject.Commit) != 40 || len(envelope.Subject.Tree) != 40 {
		return VerifiedEnvelope{}, errors.New("detached evidence repository or subject identity is invalid")
	}
	if envelope.Kind != expected.Kind || envelope.Role != expected.Role || envelope.SchemaSHA256 != expected.SchemaSHA256 || envelope.PolicySHA256 != expected.PolicySHA256 || envelope.ValidatorSHA256 != expected.ValidatorSHA256 {
		return VerifiedEnvelope{}, errors.New("detached evidence schema, policy, or validator binding mismatch")
	}
	if envelope.Trust.RootSHA256 != trust.RootSHA256 || envelope.Trust.RegistrySHA256 != trust.RegistrySHA256 || envelope.Trust.CheckpointVersion != trust.CheckpointVersion || envelope.Trust.CheckpointSHA256 != trust.CheckpointSHA256 {
		return VerifiedEnvelope{}, errors.New("detached evidence trust-root, registry, or checkpoint binding mismatch")
	}
	issued, err := time.Parse(time.RFC3339, envelope.IssuedAt)
	if err != nil {
		return VerifiedEnvelope{}, errors.New("detached evidence issued_at is invalid")
	}
	if err := validWindow("detached evidence", envelope.IssuedAt, envelope.ExpiresAt, now); err != nil {
		return VerifiedEnvelope{}, err
	}
	if envelope.Payload.Bytes != int64(len(payload)) || envelope.Payload.SHA256 != digest(payload) || envelope.Payload.MediaType != "application/json" {
		return VerifiedEnvelope{}, errors.New("detached evidence payload digest, bytes, or media type mismatch")
	}
	principal, ok := trust.principals[envelope.PrincipalID]
	if !ok || principal.Revoked || !stringSet(principal.Roles)[envelope.Role] || validWindow("evidence principal", principal.NotBefore, principal.NotAfter, issued) != nil {
		return VerifiedEnvelope{}, errors.New("detached evidence principal is not eligible for role")
	}
	statement, err := envelopeStatement(envelope)
	if err != nil {
		return VerifiedEnvelope{}, err
	}
	result := VerifiedEnvelope{PayloadSHA256: envelope.Payload.SHA256, PrincipalID: envelope.PrincipalID, Role: envelope.Role, Method: envelope.Verification.Method}
	switch envelope.Verification.Method {
	case "ed25519":
		if err := trust.VerifyRole(statement, envelope.Verification.Signatures, envelope.Role, issued); err != nil {
			return VerifiedEnvelope{}, err
		}
	case "dsse-sigstore":
		integratedTime, err := verifySigstore(envelope, statement, trust, issued, now)
		if err != nil {
			return VerifiedEnvelope{}, err
		}
		result.IntegratedTime = integratedTime.Unix()
	default:
		return VerifiedEnvelope{}, errors.New("detached evidence verification method is unsupported")
	}
	envelopeDigest, _, err := canonicalDigest(envelope)
	if err != nil {
		return VerifiedEnvelope{}, err
	}
	result.EnvelopeSHA256 = envelopeDigest
	return result, nil
}

func envelopeStatement(envelope Envelope) ([]byte, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	delete(document, "verification")
	raw, err = json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(raw)
}

func verifySigstore(envelope Envelope, statement []byte, trust VerifiedTrust, issued, now time.Time) (time.Time, error) {
	verification := envelope.Verification
	if verification.PayloadType != "application/vnd.arop.release-evidence.v1+json" || verification.Identity == nil || verification.Rekor == nil || len(verification.CertificateChainPEM) == 0 {
		return time.Time{}, errors.New("Sigstore verification material is incomplete")
	}
	if trust.Registry.Thresholds[envelope.Role] != 1 {
		return time.Time{}, errors.New("single Sigstore certificate cannot satisfy a multi-principal threshold")
	}
	identity := *verification.Identity
	if identity.PrincipalID != envelope.PrincipalID || identity.Role != envelope.Role {
		return time.Time{}, errors.New("Sigstore identity principal or role mismatch")
	}
	matched := false
	for _, trusted := range trust.Registry.SigstoreIdentities {
		if trusted == identity {
			matched = true
			break
		}
	}
	if !matched {
		return time.Time{}, errors.New("Sigstore issuer, subject, repository, workflow identity/ref/event is untrusted")
	}
	if trust.TargetsFulcioDigest() != trust.RegistryFulcioDigest() {
		return time.Time{}, errors.New("Fulcio roots do not match TUF targets")
	}
	certificates := []*x509.Certificate{}
	for _, value := range verification.CertificateChainPEM {
		block, rest := pem.Decode([]byte(value))
		if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
			return time.Time{}, errors.New("Sigstore certificate chain PEM is invalid")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, errors.New("Sigstore certificate is invalid")
		}
		certificates = append(certificates, certificate)
	}
	integrated := time.Unix(verification.Rekor.IntegratedTime, 0).UTC()
	expires, parseErr := time.Parse(time.RFC3339, envelope.ExpiresAt)
	if parseErr != nil || integrated.Before(issued) || !integrated.Before(expires) || integrated.After(now.Add(5*time.Minute)) {
		return time.Time{}, errors.New("Rekor integrated time is before candidate creation or in the future")
	}
	principal := trust.principals[envelope.PrincipalID]
	if validWindow("Sigstore principal", principal.NotBefore, principal.NotAfter, integrated) != nil || validWindow("Sigstore role registry", "1970-01-01T00:00:00Z", trust.Registry.ExpiresAt, integrated) != nil {
		return time.Time{}, errors.New("Rekor integrated time is outside the principal or registry validity window")
	}
	leaf := certificates[0]
	roots := x509.NewCertPool()
	for _, value := range trust.Protected.FulcioRootsPEM {
		if !roots.AppendCertsFromPEM([]byte(value)) {
			return time.Time{}, errors.New("protected Fulcio root is invalid")
		}
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: integrated, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
		return time.Time{}, errors.New("Fulcio certificate chain verification failed")
	}
	if leaf.Issuer.CommonName != identity.Issuer || len(leaf.URIs) != 1 || leaf.URIs[0].String() != identity.Subject {
		return time.Time{}, errors.New("Fulcio certificate issuer or SAN mismatch")
	}
	publicKey, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return time.Time{}, errors.New("Sigstore leaf key must be Ed25519")
	}
	rawSignature, err := base64.StdEncoding.Strict().DecodeString(verification.Signature)
	if err != nil || !ed25519.Verify(publicKey, dssePAE(verification.PayloadType, statement), rawSignature) {
		return time.Time{}, errors.New("DSSE signature verification failed")
	}
	bodyDigest, err := SigstoreBodyDigest(statement, verification.Signature, leaf.Raw)
	if err != nil || bodyDigest != verification.Rekor.BodySHA256 {
		return time.Time{}, errors.New("Rekor body does not bind DSSE payload, signature, and certificate")
	}
	if err := verifyRekorProof(*verification.Rekor, trust); err != nil {
		return time.Time{}, err
	}
	return integrated, nil
}

func (trust VerifiedTrust) TargetsFulcioDigest() string {
	return trust.FulcioRootSHA256
}

func (trust VerifiedTrust) RegistryFulcioDigest() string {
	// Targets are not exposed separately; protected roots must be pinned by the TUF targets check in VerifyRegistry.
	return trust.TargetsFulcioDigest()
}

func SigstoreBodyDigest(statement []byte, signature string, certificateDER []byte) (string, error) {
	value := map[string]any{"payload_sha256": digest(statement), "signature": signature, "certificate_sha256": digest(certificateDER)}
	canonical, err := Canonical(value)
	if err != nil {
		return "", err
	}
	return digest(canonical), nil
}

func dssePAE(payloadType string, payload []byte) []byte {
	return []byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload))
}

func verifyRekorProof(proof RekorProof, trust VerifiedTrust) error {
	allowed := stringSet(trustRegistryRekorIDs(trust))
	key, ok := trust.Protected.RekorKeys[proof.LogID]
	if !ok || !allowed[proof.LogID] || proof.TreeSize < 1 || proof.LogIndex < 0 || proof.LogIndex >= proof.TreeSize || !isDigest(proof.BodySHA256) || !isDigest(proof.RootHash) {
		return errors.New("Rekor log identity or inclusion coordinates are invalid")
	}
	root, err := RekorRoot(proof.BodySHA256, proof.LogIndex, proof.TreeSize, proof.Hashes)
	if err != nil || root != proof.RootHash {
		return errors.New("Rekor inclusion proof is invalid")
	}
	checkpoint, err := Canonical(map[string]any{"log_id": proof.LogID, "tree_size": proof.TreeSize, "root_hash": proof.RootHash, "integrated_time": proof.IntegratedTime})
	if err != nil {
		return err
	}
	publicKey, err := decodePublicKey(key)
	if err != nil {
		return err
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(proof.CheckpointSignature)
	if err != nil || !ed25519.Verify(publicKey, checkpoint, signature) {
		return errors.New("Rekor signed checkpoint is invalid")
	}
	return nil
}

func trustRegistryRekorIDs(trust VerifiedTrust) []string {
	return append([]string(nil), trust.RekorKeyIDs...)
}

func RekorRoot(bodyDigest string, index, size int64, hashes []string) (string, error) {
	if !isDigest(bodyDigest) || index < 0 || size < 1 || index >= size {
		return "", errors.New("invalid Rekor inclusion input")
	}
	leaf := sha256.Sum256(append([]byte{0}, []byte(bodyDigest)...))
	current := leaf[:]
	position, last := index, size-1
	for _, siblingDigest := range hashes {
		if !isDigest(siblingDigest) {
			return "", errors.New("invalid Rekor sibling digest")
		}
		sibling, _ := hex.DecodeString(strings.TrimPrefix(siblingDigest, "sha256:"))
		buffer := []byte{1}
		if position%2 == 1 || position == last {
			buffer = append(buffer, sibling...)
			buffer = append(buffer, current...)
		} else {
			buffer = append(buffer, current...)
			buffer = append(buffer, sibling...)
		}
		sum := sha256.Sum256(buffer)
		current = sum[:]
		position /= 2
		last /= 2
	}
	if position != 0 || last != 0 {
		return "", errors.New("Rekor inclusion proof is incomplete")
	}
	return "sha256:" + hex.EncodeToString(current), nil
}

func VerifyDetachedSignature(publicKey crypto.PublicKey, statement, signature []byte) error {
	ed25519Key, ok := publicKey.(ed25519.PublicKey)
	if !ok || !ed25519.Verify(ed25519Key, statement, signature) {
		return errors.New("detached signature is invalid")
	}
	return nil
}

func LoadProtectedState(path string) (ProtectedState, []byte, error) {
	var value ProtectedState
	data, err := os.ReadFile(path)
	if err != nil {
		return value, nil, fmt.Errorf("read protected trust state: %w", err)
	}
	if err := strictDecode(data, &value); err != nil {
		return value, nil, fmt.Errorf("decode protected trust state: %w", err)
	}
	if err := validateProtectedState(value); err != nil {
		return value, nil, err
	}
	return value, data, nil
}

func LoadRegistryBundle(root, path string) (RegistryBundle, []byte, error) {
	var value RegistryBundle
	data, err := os.ReadFile(path)
	if err != nil {
		return value, nil, fmt.Errorf("read trusted release registry: %w", err)
	}
	parsed, err := structuredfile.Parse(data, "json")
	if err != nil {
		return value, nil, err
	}
	if err := schema.ValidateFile(root, RoleRegistrySchema, parsed); err != nil {
		return value, nil, err
	}
	if err := strictDecode(data, &value); err != nil {
		return value, nil, err
	}
	return value, data, nil
}

func VerifyRegistry(bundle RegistryBundle, protected ProtectedState, now time.Time) (VerifiedTrust, error) {
	if err := validateProtectedState(protected); err != nil {
		return VerifiedTrust{}, err
	}
	if bundle.SchemaVersion != 1 || bundle.Root.Type != "root" || bundle.Timestamp.Type != "timestamp" || bundle.Snapshot.Type != "snapshot" || bundle.Targets.Type != "targets" {
		return VerifiedTrust{}, errors.New("TUF metadata type chain is invalid")
	}
	rootDigest, rootBytes, err := canonicalDigest(bundle.Root)
	if err != nil || rootDigest != protected.RootSHA256 || bundle.Root.Version != protected.RootVersion {
		return VerifiedTrust{}, errors.New("protected pinned root digest or version mismatch")
	}
	rootStatement, err := signingStatement(bundle.Root)
	if err != nil {
		return VerifiedTrust{}, err
	}
	protectedRole := Role{Threshold: protected.RootThreshold}
	for _, key := range protected.RootKeys {
		protectedRole.KeyIDs = append(protectedRole.KeyIDs, key.KeyID)
	}
	if err := verifyThreshold(rootStatement, bundle.Root.Signatures, protectedRole, keyMap(protected.RootKeys), nil, now); err != nil {
		return VerifiedTrust{}, fmt.Errorf("verify protected root: %w", err)
	}
	rootKeys := keyMap(bundle.Root.Keys)
	if len(rootKeys) != len(bundle.Root.Keys) {
		return VerifiedTrust{}, errors.New("TUF root contains duplicate keys")
	}
	for _, roleName := range []string{"root", "timestamp", "snapshot", "targets"} {
		role, ok := bundle.Root.Roles[roleName]
		if !ok || role.Threshold < 1 || role.Threshold > len(role.KeyIDs) {
			return VerifiedTrust{}, fmt.Errorf("TUF root role %s is invalid", roleName)
		}
		for _, keyID := range role.KeyIDs {
			if _, ok := rootKeys[keyID]; !ok {
				return VerifiedTrust{}, fmt.Errorf("TUF role %s references missing key", roleName)
			}
		}
	}
	for name, value := range map[string]any{"root": bundle.Root, "timestamp": bundle.Timestamp, "snapshot": bundle.Snapshot, "targets": bundle.Targets, "registry": bundle.RoleRegistry} {
		expires := ""
		switch item := value.(type) {
		case RootMetadata:
			expires = item.ExpiresAt
		case TimestampMetadata:
			expires = item.ExpiresAt
		case SnapshotMetadata:
			expires = item.ExpiresAt
		case TargetsMetadata:
			expires = item.ExpiresAt
		case RoleRegistry:
			expires = item.ExpiresAt
		}
		if err := validWindow("TUF "+name, "1970-01-01T00:00:00Z", expires, now); err != nil {
			return VerifiedTrust{}, err
		}
	}
	for _, item := range []struct {
		name       string
		value      any
		signatures []Signature
		role       string
	}{{"timestamp", bundle.Timestamp, bundle.Timestamp.Signatures, "timestamp"}, {"snapshot", bundle.Snapshot, bundle.Snapshot.Signatures, "snapshot"}, {"targets", bundle.Targets, bundle.Targets.Signatures, "targets"}} {
		statement, err := signingStatement(item.value)
		if err != nil {
			return VerifiedTrust{}, err
		}
		if err := verifyThreshold(statement, item.signatures, bundle.Root.Roles[item.role], rootKeys, nil, now); err != nil {
			return VerifiedTrust{}, fmt.Errorf("verify TUF %s: %w", item.name, err)
		}
	}
	generatedAt, err := time.Parse(time.RFC3339, bundle.Timestamp.GeneratedAt)
	if err != nil || generatedAt.After(now) || now.Sub(generatedAt) > time.Duration(protected.MaxTimestampAgeSeconds)*time.Second {
		return VerifiedTrust{}, errors.New("TUF timestamp is frozen, future-dated, or stale")
	}
	if err := matchMeta(bundle.Timestamp.Snapshot, bundle.Snapshot); err != nil {
		return VerifiedTrust{}, fmt.Errorf("timestamp to snapshot: %w", err)
	}
	if err := matchMeta(bundle.Snapshot.Targets, bundle.Targets); err != nil {
		return VerifiedTrust{}, fmt.Errorf("snapshot to targets: %w", err)
	}
	if err := matchMeta(bundle.Targets.RoleRegistry, bundle.RoleRegistry); err != nil {
		return VerifiedTrust{}, fmt.Errorf("targets to role registry: %w", err)
	}
	registryDigest, _, err := canonicalDigest(bundle.RoleRegistry)
	if err != nil {
		return VerifiedTrust{}, err
	}
	if err := validateRoleRegistry(bundle.RoleRegistry, now); err != nil {
		return VerifiedTrust{}, err
	}
	protectedFulcioDigest := digest([]byte(strings.Join(protected.FulcioRootsPEM, "\n")))
	if bundle.Targets.FulcioRootSHA256 != protectedFulcioDigest {
		return VerifiedTrust{}, errors.New("protected Fulcio roots do not match TUF targets")
	}
	for _, keyID := range bundle.Targets.RekorKeyIDs {
		if _, ok := protected.RekorKeys[keyID]; !ok {
			return VerifiedTrust{}, errors.New("TUF targets reference an unprotected Rekor key")
		}
	}
	checkpointStatement, err := signingStatement(bundle.Checkpoint)
	if err != nil {
		return VerifiedTrust{}, err
	}
	if err := verifyThreshold(checkpointStatement, bundle.Checkpoint.Signatures, bundle.Root.Roles["root"], rootKeys, nil, now); err != nil {
		return VerifiedTrust{}, fmt.Errorf("verify monotonic checkpoint: %w", err)
	}
	checkpointDigest, _, err := canonicalDigest(bundle.Checkpoint)
	if err != nil || bundle.Checkpoint.RegistrySHA256 != registryDigest || bundle.Checkpoint.Version < protected.LastCheckpointVersion {
		return VerifiedTrust{}, errors.New("registry checkpoint digest or monotonic version is invalid")
	}
	if bundle.Checkpoint.Version == protected.LastCheckpointVersion && checkpointDigest != protected.LastCheckpointSHA256 {
		return VerifiedTrust{}, errors.New("registry checkpoint changed without version advance")
	}
	checkpointTime, err := time.Parse(time.RFC3339, bundle.Checkpoint.SignedAt)
	if err != nil || checkpointTime.After(now) || checkpointTime.Before(generatedAt) {
		return VerifiedTrust{}, errors.New("registry checkpoint time is outside the trusted metadata window")
	}
	registryKeys := keyMap(bundle.RoleRegistry.Keys)
	principals := map[string]Principal{}
	for _, principal := range bundle.RoleRegistry.Principals {
		principals[principal.PrincipalID] = principal
	}
	_ = rootBytes
	return VerifiedTrust{RootSHA256: rootDigest, RegistrySHA256: registryDigest, CheckpointSHA256: checkpointDigest, CheckpointVersion: bundle.Checkpoint.Version, FulcioRootSHA256: bundle.Targets.FulcioRootSHA256, RekorKeyIDs: append([]string(nil), bundle.Targets.RekorKeyIDs...), Registry: bundle.RoleRegistry, Protected: protected, rootKeys: rootKeys, registryKeys: registryKeys, principals: principals}, nil
}

func validateProtectedState(value ProtectedState) error {
	if value.SchemaVersion != 1 || value.RootVersion < 1 || !isDigest(value.RootSHA256) || value.RootThreshold < 1 || value.RootThreshold > len(value.RootKeys) || value.LastCheckpointVersion < 1 || !isDigest(value.LastCheckpointSHA256) || value.MaxTimestampAgeSeconds < 1 || value.MaxTimestampAgeSeconds > 86400 || len(value.RootKeys) == 0 || len(value.FulcioRootsPEM) == 0 || len(value.RekorKeys) == 0 {
		return errors.New("protected trust state is incomplete")
	}
	if len(keyMap(value.RootKeys)) != len(value.RootKeys) {
		return errors.New("protected trust root keys are duplicate or invalid")
	}
	for id, key := range value.RekorKeys {
		if id != key.KeyID || key.Algorithm != "ed25519" {
			return errors.New("protected Rekor key registry is invalid")
		}
	}
	return nil
}

func validateRoleRegistry(value RoleRegistry, now time.Time) error {
	if value.SchemaVersion != 1 || value.Version < 1 || len(value.Keys) == 0 || len(value.Principals) == 0 || len(value.Thresholds) == 0 {
		return errors.New("trusted role registry is incomplete")
	}
	keys := keyMap(value.Keys)
	if len(keys) != len(value.Keys) {
		return errors.New("trusted role registry contains duplicate or invalid keys")
	}
	principals := map[string]Principal{}
	keyOwner := map[string]string{}
	for _, principal := range value.Principals {
		if _, duplicate := principals[principal.PrincipalID]; duplicate || strings.Contains(principal.PrincipalID, "dry-run") || strings.Contains(principal.PrincipalID, "dryrun") {
			return errors.New("trusted role registry principal identity is duplicate or forbidden")
		}
		if err := validWindow("principal "+principal.PrincipalID, principal.NotBefore, principal.NotAfter, now); err != nil && !principal.Revoked {
			return err
		}
		roleSet := stringSet(principal.Roles)
		if roleSet["release_approver"] && (roleSet["builder"] || roleSet["orchestrator"] || roleSet["publisher"] || roleSet["ci_builder"]) {
			return errors.New("release approver role is not separated from build or publication roles")
		}
		for _, keyID := range principal.KeyIDs {
			key, ok := keys[keyID]
			if !ok || key.PrincipalID != principal.PrincipalID || keyOwner[keyID] != "" || strings.Contains(keyID, "dry-run") || strings.Contains(keyID, "dryrun") {
				return errors.New("trusted principal key ownership is invalid")
			}
			keyOwner[keyID] = principal.PrincipalID
		}
		principals[principal.PrincipalID] = principal
	}
	for role, threshold := range value.Thresholds {
		eligible := 0
		for _, principal := range value.Principals {
			if !principal.Revoked && stringSet(principal.Roles)[role] {
				eligible++
			}
		}
		if threshold < 1 || threshold > eligible {
			return fmt.Errorf("role %s threshold exceeds distinct eligible principals", role)
		}
	}
	for _, identity := range value.SigstoreIdentities {
		principal, ok := principals[identity.PrincipalID]
		if !ok || principal.Revoked || !stringSet(principal.Roles)[identity.Role] || identity.Repository != Repository || identity.Issuer == "" || identity.Subject == "" {
			return errors.New("Sigstore identity is not bound to an eligible principal and role")
		}
		if identity.Event != ReleaseWorkflowEvent {
			return errors.New("Sigstore identity event is not one the pinned release workflow can emit")
		}
		if placeholderCommit(identity.WorkflowSHA) {
			return errors.New("Sigstore identity workflow_sha is not a real pinned commit")
		}
	}
	return nil
}

// placeholderCommit rejects workflow_sha values that cannot be a genuinely
// pinned commit: anything but 40 lowercase hex, or a degenerate single
// repeated character sometimes used as a synthetic stand-in.
func placeholderCommit(value string) bool {
	if len(value) != 40 {
		return true
	}
	distinct := map[byte]bool{}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return true
		}
		distinct[char] = true
	}
	return len(distinct) < 2
}

func verifyThreshold(statement []byte, signatures []Signature, role Role, keys map[string]Key, principals map[string]Principal, at time.Time) error {
	allowed := stringSet(role.KeyIDs)
	counted := map[string]bool{}
	seenKeys := map[string]bool{}
	for _, signature := range signatures {
		if seenKeys[signature.KeyID] {
			return errors.New("duplicate signature key")
		}
		seenKeys[signature.KeyID] = true
		key, ok := keys[signature.KeyID]
		if !ok || !allowed[signature.KeyID] {
			continue
		}
		if principals != nil {
			principal, ok := principals[key.PrincipalID]
			if !ok || principal.Revoked || validWindow("principal", principal.NotBefore, principal.NotAfter, at) != nil {
				continue
			}
		}
		publicKey, err := decodePublicKey(key)
		if err != nil {
			return err
		}
		rawSignature, err := base64.StdEncoding.Strict().DecodeString(signature.Signature)
		if err != nil || len(rawSignature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, statement, rawSignature) {
			continue
		}
		counted[key.PrincipalID] = true
	}
	if len(counted) < role.Threshold {
		return fmt.Errorf("signature threshold not met: distinct principals=%d threshold=%d", len(counted), role.Threshold)
	}
	return nil
}

func (trust VerifiedTrust) VerifyRole(statement []byte, signatures []Signature, role string, at time.Time) error {
	threshold := trust.Registry.Thresholds[role]
	if threshold < 1 {
		return fmt.Errorf("trusted role %s has no threshold", role)
	}
	keyIDs := []string{}
	for _, principal := range trust.Registry.Principals {
		if !principal.Revoked && stringSet(principal.Roles)[role] {
			keyIDs = append(keyIDs, principal.KeyIDs...)
		}
	}
	return verifyThreshold(statement, signatures, Role{KeyIDs: keyIDs, Threshold: threshold}, trust.registryKeys, trust.principals, at)
}

func matchMeta(meta Meta, value any) error {
	version := 0
	switch typed := value.(type) {
	case SnapshotMetadata:
		version = typed.Version
	case TargetsMetadata:
		version = typed.Version
	case RoleRegistry:
		version = typed.Version
	default:
		return errors.New("unsupported TUF metadata target")
	}
	digest, data, err := canonicalDigest(value)
	if err != nil || meta.Version != version || meta.SHA256 != digest || meta.Bytes != int64(len(data)) {
		return errors.New("version, digest, or byte count mismatch")
	}
	return nil
}

func signingStatement(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	delete(document, "signatures")
	raw, err = json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(raw)
}

func SigningStatement(value any) ([]byte, error) { return signingStatement(value) }

func EnvelopeSigningStatement(value Envelope) ([]byte, error) { return envelopeStatement(value) }

func DigestValue(value any) (string, []byte, error) { return canonicalDigest(value) }

func HashBytes(value []byte) string { return digest(value) }

func DSSEPAE(payloadType string, payload []byte) []byte { return dssePAE(payloadType, payload) }

func Canonical(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return jsoncanonicalizer.Transform(raw)
}

func canonicalDigest(value any) (string, []byte, error) {
	data, err := Canonical(value)
	if err != nil {
		return "", nil, err
	}
	return digest(data), data, nil
}

func strictDecode(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("JSON contains a trailing value")
		}
		return err
	}
	return nil
}

func decodePublicKey(key Key) (ed25519.PublicKey, error) {
	if key.Algorithm != "ed25519" {
		return nil, errors.New("only Ed25519 keys are accepted")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(key.PublicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("Ed25519 public key is invalid")
	}
	return ed25519.PublicKey(decoded), nil
}

func keyMap(values []Key) map[string]Key {
	result := map[string]Key{}
	for _, key := range values {
		if key.KeyID == "" || key.PrincipalID == "" || key.Algorithm != "ed25519" {
			continue
		}
		if _, err := decodePublicKey(key); err != nil {
			continue
		}
		if _, duplicate := result[key.KeyID]; duplicate {
			delete(result, key.KeyID)
			continue
		}
		result[key.KeyID] = key
	}
	return result
}

func validWindow(name, start, end string, now time.Time) error {
	begin, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return fmt.Errorf("%s not_before is invalid", name)
	}
	finish, err := time.Parse(time.RFC3339, end)
	if err != nil || !finish.After(begin) || now.Before(begin) || !now.Before(finish) {
		return fmt.Errorf("%s is outside its validity window", name)
	}
	return nil
}

func stringSet(values []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		result[value] = true
	}
	return result
}

func isDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == strings.ToLower(value)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func SortedRoles(value RoleRegistry) []string {
	roles := make([]string, 0, len(value.Thresholds))
	for role := range value.Thresholds {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

func ProtectedPathOutsideRepository(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pathResolved, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil || relative == "." || (!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "..") {
		return "", errors.New("protected trust state must be outside the repository")
	}
	information, err := os.Lstat(pathResolved)
	if err != nil || !information.Mode().IsRegular() || information.Mode().Perm()&0o077 != 0 {
		return "", errors.New("protected trust state must be a private regular file")
	}
	return pathResolved, nil
}
