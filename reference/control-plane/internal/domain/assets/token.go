package assets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const opaqueTokenBytes = 32

// OpaqueToken is the caller-held secret. Persistence stores only its digest.
type OpaqueToken struct {
	Value  string
	Digest string
}

func newOpaqueToken() (OpaqueToken, error) {
	raw := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return OpaqueToken{}, NewError(CategoryDependency, ReasonDependencyUnavailable, err)
	}
	encoded := hex.EncodeToString(raw)
	return OpaqueToken{Value: encoded, Digest: digestOpaque(encoded)}, nil
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
	return "agnt_" + id, nil
}

func newAssetID(now time.Time) (string, error) {
	id, err := newUUIDv7(now)
	if err != nil {
		return "", err
	}
	return "asset_" + id, nil
}
