package assets

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const opaqueTokenBytes = 32

// OpaqueToken is the caller-held secret. Persistence stores only its digest.
type OpaqueToken struct {
	Value  string
	Digest string
}

// HMACTokenIssuer derives stable, non-reversible opaque tokens. The key comes
// from deployment secret configuration; it is copied on construction and is
// never exposed by this package.
type HMACTokenIssuer struct {
	keyID string
	key   [32]byte
}

func NewHMACTokenIssuer(keyID string, key []byte) (*HMACTokenIssuer, error) {
	if !tokenKeyPattern.MatchString(keyID) || len(key) != sha256.Size {
		return nil, errors.New("asset token key must be exactly 32 bytes")
	}
	issuer := &HMACTokenIssuer{keyID: keyID}
	copy(issuer.key[:], key)
	return issuer, nil
}

func (issuer *HMACTokenIssuer) KeyID() string {
	if issuer == nil {
		return ""
	}
	return issuer.keyID
}

func (issuer *HMACTokenIssuer) IssueToken(ctx context.Context, grant Grant) (OpaqueToken, error) {
	if issuer == nil || ctx.Err() != nil || validateTokenSubject(grant) != nil {
		return OpaqueToken{}, NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	mac := hmac.New(sha256.New, issuer.key[:])
	writeMACField(mac, grant.GrantID)
	writeMACField(mac, grant.Binding.TenantID)
	writeMACField(mac, grant.Binding.PrincipalID)
	writeMACField(mac, grant.Binding.CredentialID)
	writeMACField(mac, grant.Binding.RunID)
	writeMACField(mac, grant.Binding.AssetID)
	writeMACField(mac, string(grant.Binding.Operation))
	writeMACField(mac, grant.Binding.Name)
	writeMACField(mac, grant.Binding.MediaType)
	writeMACField(mac, grant.Binding.Digest)
	writeMACInt64(mac, grant.Binding.SizeBytes)
	writeMACInt64(mac, grant.NotBefore.UnixNano())
	writeMACInt64(mac, grant.ExpiresAt.UnixNano())
	writeMACInt64(mac, int64(grant.MaxUses))
	writeMACField(mac, grant.Audience)
	writeMACField(mac, grant.TokenKeyID)
	value := "agt_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return OpaqueToken{Value: value, Digest: digestOpaque(value)}, nil
}

func validateTokenSubject(grant Grant) error {
	if err := grant.Binding.Validate(); err != nil {
		return err
	}
	if !grantPattern.MatchString(grant.GrantID) ||
		grant.NotBefore.IsZero() || grant.ExpiresAt.IsZero() ||
		grant.NotBefore.Location() != time.UTC || grant.ExpiresAt.Location() != time.UTC ||
		!grant.ExpiresAt.After(grant.NotBefore) || grant.ExpiresAt.Sub(grant.NotBefore) > MaxGrantTTL ||
		grant.MaxUses < 1 || grant.MaxUses > MaxGrantUses ||
		grant.Audience != AssetTokenAudience || !tokenKeyPattern.MatchString(grant.TokenKeyID) ||
		len(grant.IdempotencyKeyDigest) != 64 || strings.Trim(grant.IdempotencyKeyDigest, "0123456789abcdef") != "" {
		return NewError(CategoryValidation, ReasonInvalidRequest)
	}
	return nil
}

func writeMACField(mac interface{ Write([]byte) (int, error) }, value string) {
	writeMACInt64(mac, int64(len(value)))
	_, _ = mac.Write([]byte(value))
}

func writeMACInt64(mac interface{ Write([]byte) (int, error) }, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = mac.Write(encoded[:])
}

func digestOpaque(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func digestIdempotency(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newUUIDv7(now time.Time) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	millis := now.UnixMilli()
	raw[0] = byte(millis >> 40)
	raw[1] = byte(millis >> 32)
	raw[2] = byte(millis >> 24)
	raw[3] = byte(millis >> 16)
	raw[4] = byte(millis >> 8)
	raw[5] = byte(millis)
	raw[6] = 0x70 | (raw[6] & 0x0f)
	raw[8] = 0x80 | (raw[8] & 0x3f)
	return hex.EncodeToString(raw[0:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" + hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:16]), nil
}

func newGrantID(now time.Time) (string, error) {
	id, err := newUUIDv7(now)
	if err != nil {
		return "", err
	}
	return "grant_" + id, nil
}

func newAssetID(now time.Time) (string, error) {
	id, err := newUUIDv7(now)
	if err != nil {
		return "", err
	}
	return "asset_" + id, nil
}
