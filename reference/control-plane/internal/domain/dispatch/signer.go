package dispatch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math/big"
	"sync"
	"time"
)

// ProcessSigner is a reference Signer port adapter. Private scalars remain
// encapsulated in this process-only adapter and are never returned or stored.
// Production deployments can replace it with a KMS/HSM-backed implementation.
type ProcessSigner struct {
	mutex    sync.RWMutex
	keys     map[string]*ecdsa.PrivateKey
	meta     map[string]KeyMetadata
	active   string
	lifetime time.Duration
	maxTTL   time.Duration
}

func NewProcessSigner(now time.Time, keyID string, lifetime, maxTTL time.Duration) (*ProcessSigner, error) {
	if !utc(now) || lifetime < 2*maxTTL || maxTTL < time.Minute || maxTTL > 5*time.Minute || !keyIDPattern.MatchString(keyID) {
		return nil, errors.New("invalid process signer policy")
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, errors.New("generate ES256 key")
	}
	metadata := metadataFromPrivate(keyID, KeyActive, private, now, now.Add(lifetime), now.Add(lifetime+2*maxTTL))
	return &ProcessSigner{keys: map[string]*ecdsa.PrivateKey{keyID: private}, meta: map[string]KeyMetadata{keyID: metadata}, active: keyID, lifetime: lifetime, maxTTL: maxTTL}, nil
}

func (signer *ProcessSigner) ActiveKey(ctx context.Context, now time.Time) (KeyMetadata, error) {
	if err := ctx.Err(); err != nil || !utc(now) {
		return KeyMetadata{}, errors.New("signer unavailable")
	}
	signer.mutex.Lock()
	defer signer.mutex.Unlock()
	metadata, ok := signer.meta[signer.active]
	if !ok || metadata.Status != KeyActive || now.Before(metadata.NotBefore) {
		return KeyMetadata{}, errors.New("active signer unavailable")
	}
	// The reference process adapter owns its lifecycle: rotate before the
	// current key can expire so a long-lived server never silently stops
	// dispatching. Production KMS/HSM adapters can implement the same Signer
	// contract with their own coordinated rotation policy.
	if !now.Before(metadata.SignUntil.Add(-signer.maxTTL)) {
		keyID, err := randomKeyID()
		if err != nil {
			return KeyMetadata{}, errors.New("rotate ES256 key")
		}
		private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return KeyMetadata{}, errors.New("rotate ES256 key")
		}
		if _, exists := signer.keys[keyID]; exists {
			return KeyMetadata{}, errors.New("rotate ES256 key")
		}
		metadata = signer.applyRotationLocked(now, keyID, private, signer.lifetime, signer.maxTTL)
	}
	if !now.Before(metadata.SignUntil) {
		return KeyMetadata{}, errors.New("active signer unavailable")
	}
	return metadata, nil
}

func (signer *ProcessSigner) VerificationKeys(ctx context.Context, now time.Time) ([]KeyMetadata, error) {
	if err := ctx.Err(); err != nil || !utc(now) {
		return nil, errors.New("signer unavailable")
	}
	signer.mutex.RLock()
	defer signer.mutex.RUnlock()
	result := make([]KeyMetadata, 0, len(signer.meta))
	for _, metadata := range signer.meta {
		if metadata.VerifyUntil.After(now) {
			result = append(result, metadata)
		}
	}
	return normalizeKeys(result)
}

func (signer *ProcessSigner) Sign(ctx context.Context, keyID string, input []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil || len(input) == 0 {
		return nil, errors.New("signer unavailable")
	}
	signer.mutex.RLock()
	private, metadata := signer.keys[keyID], signer.meta[keyID]
	signer.mutex.RUnlock()
	if private == nil || metadata.Status != KeyActive {
		return nil, errors.New("unknown signing key")
	}
	digest := sha256.Sum256(input)
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		return nil, errors.New("ES256 sign")
	}
	return fixedSignature(r, s), nil
}

func (signer *ProcessSigner) Rotate(now time.Time, keyID string, lifetime, maxTTL time.Duration) (KeyMetadata, error) {
	if !utc(now) || lifetime < 2*maxTTL || maxTTL < time.Minute || maxTTL > 5*time.Minute || !keyIDPattern.MatchString(keyID) {
		return KeyMetadata{}, errors.New("invalid rotation")
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return KeyMetadata{}, err
	}
	signer.mutex.Lock()
	defer signer.mutex.Unlock()
	if _, exists := signer.keys[keyID]; exists {
		return KeyMetadata{}, errors.New("duplicate signing key")
	}
	metadata := signer.applyRotationLocked(now, keyID, private, lifetime, maxTTL)
	signer.lifetime, signer.maxTTL = lifetime, maxTTL
	return metadata, nil
}

func (signer *ProcessSigner) applyRotationLocked(now time.Time, keyID string, private *ecdsa.PrivateKey, lifetime, maxTTL time.Duration) KeyMetadata {
	if old, exists := signer.meta[signer.active]; exists {
		old.Status = KeyRetiring
		old.SignUntil = now
		minimum := now.Add(2 * maxTTL)
		if old.VerifyUntil.Before(minimum) {
			old.VerifyUntil = minimum
		}
		signer.meta[signer.active] = old
	}
	metadata := metadataFromPrivate(keyID, KeyActive, private, now, now.Add(lifetime), now.Add(lifetime+2*maxTTL))
	signer.keys[keyID], signer.meta[keyID], signer.active = private, metadata, keyID
	return metadata
}

func randomKeyID() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "key_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func metadataFromPrivate(keyID string, status KeyStatus, private *ecdsa.PrivateKey, notBefore, signUntil, verifyUntil time.Time) KeyMetadata {
	encode := func(value *big.Int) string {
		bytes := value.FillBytes(make([]byte, 32))
		return base64.RawURLEncoding.EncodeToString(bytes)
	}
	return KeyMetadata{KeyID: keyID, Status: status, X: encode(private.PublicKey.X), Y: encode(private.PublicKey.Y), NotBefore: notBefore, SignUntil: signUntil, VerifyUntil: verifyUntil, CreatedAt: notBefore}
}

func fixedSignature(r, s *big.Int) []byte {
	result := make([]byte, 64)
	r.FillBytes(result[:32])
	s.FillBytes(result[32:])
	return result
}
