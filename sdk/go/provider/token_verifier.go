package provider

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// VerificationKey is one overlap-retained ES256 public signing key.
type VerificationKey struct {
	KeyID       string
	X, Y        string
	NotBefore   time.Time
	SignUntil   time.Time
	VerifyUntil time.Time
}

// KeySource returns a key by the JWT kid. Sources may cache only until their
// signed/public metadata permits and must fail closed on refresh failure.
type KeySource interface {
	VerificationKey(context.Context, string, time.Time) (VerificationKey, error)
}

// StaticKeys is useful for pinned public-key deployments and tests.
type StaticKeys map[string]VerificationKey

func (keys StaticKeys) VerificationKey(_ context.Context, keyID string, now time.Time) (VerificationKey, error) {
	key, ok := keys[keyID]
	if !ok || now.Before(key.NotBefore) || !now.Before(key.VerifyUntil) {
		return VerificationKey{}, errors.New("verification key unavailable")
	}
	return key, nil
}

// ES256VerifierConfig freezes runtime identity and ticket bindings.
type ES256VerifierConfig struct {
	Keys                                       KeySource
	Issuer, Audience, Endpoint                 string
	DeploymentID, InstanceID, TransportProfile string
	Generation                                 uint64
}

// ES256Verifier validates Control Plane Run Tokens locally.
type ES256Verifier struct{ config ES256VerifierConfig }

func NewES256Verifier(config ES256VerifierConfig) (*ES256Verifier, error) {
	if config.Keys == nil || config.Issuer == "" || config.Audience == "" || config.Endpoint == "" || !validPrefixed("dep_", config.DeploymentID) || !identifierPattern.MatchString(config.InstanceID) || config.Generation == 0 || config.TransportProfile != "direct" && config.TransportProfile != "proxy" {
		return nil, errors.New("invalid ES256 verifier configuration")
	}
	for _, raw := range []string{config.Issuer, config.Audience, config.Endpoint} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("invalid ES256 verifier URL")
		}
	}
	return &ES256Verifier{config: config}, nil
}

func (verifier *ES256Verifier) Verify(ctx context.Context, token string, request VerifyRequest) (Claims, error) {
	if err := ctx.Err(); err != nil || len(token) < 96 || len(token) > 8192 || !request.Now.Equal(request.Now.UTC()) {
		return Claims{}, errors.New("invalid run token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("invalid run token")
	}
	var header jwtHeader
	var wire jwtClaims
	if decodeJWTJSON(parts[0], &header) != nil || decodeJWTJSON(parts[1], &wire) != nil || header.Algorithm != "ES256" || header.Type != "JWT" || header.KeyID == "" {
		return Claims{}, errors.New("invalid run token")
	}
	key, err := verifier.config.Keys.VerificationKey(ctx, header.KeyID, request.Now)
	if err != nil || key.KeyID != header.KeyID || request.Now.Before(key.NotBefore) || !request.Now.Before(key.VerifyUntil) {
		return Claims{}, errors.New("invalid run token")
	}
	public, err := key.publicKey()
	signature, signatureErr := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || signatureErr != nil || len(signature) != 64 {
		return Claims{}, errors.New("invalid run token")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		return Claims{}, errors.New("invalid run token")
	}
	claims := wire.claims()
	if wire.NotBefore != wire.IssuedAt || claims.Validate(request.Now, request.RequiredScope) != nil || claims.Issuer != verifier.config.Issuer || claims.Audience != verifier.config.Audience || claims.Endpoint != verifier.config.Endpoint || claims.DeploymentID != verifier.config.DeploymentID || claims.InstanceID != verifier.config.InstanceID || claims.Generation != verifier.config.Generation || claims.TransportProfile != verifier.config.TransportProfile || claims.IssuedAt.Before(key.NotBefore) || !claims.IssuedAt.Before(key.SignUntil) {
		return Claims{}, errors.New("invalid run token")
	}
	if strings.HasPrefix(request.Path, "/v1/runs/") {
		remainder := strings.TrimPrefix(request.Path, "/v1/runs/")
		runID := strings.TrimSuffix(remainder, "/commands")
		if strings.Contains(runID, "/") || runID != claims.RunID {
			return Claims{}, errors.New("run token path mismatch")
		}
	}
	return claims, nil
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type jwtClaims struct {
	Issuer           string   `json:"iss"`
	Audience         string   `json:"aud"`
	Subject          string   `json:"sub"`
	AuthorizedParty  string   `json:"azp"`
	TokenID          string   `json:"jti"`
	RunID            string   `json:"run_id"`
	AttemptID        string   `json:"attempt_id"`
	AgentID          string   `json:"agent_id"`
	AgentVersion     string   `json:"agent_version"`
	SkillID          string   `json:"skill_id"`
	DeploymentID     string   `json:"deployment_id"`
	InstanceID       string   `json:"instance_id"`
	TransportProfile string   `json:"transport_profile"`
	Endpoint         string   `json:"endpoint"`
	Generation       uint64   `json:"generation"`
	FencingToken     uint64   `json:"fencing_token"`
	Scopes           []string `json:"scope"`
	IssuedAt         int64    `json:"iat"`
	NotBefore        int64    `json:"nbf"`
	ExpiresAt        int64    `json:"exp"`
}

func (wire jwtClaims) claims() Claims {
	return Claims{
		Issuer: wire.Issuer, Audience: wire.Audience, Subject: wire.Subject, AuthorizedParty: wire.AuthorizedParty, TokenID: wire.TokenID,
		RunID: wire.RunID, AttemptID: wire.AttemptID, AgentID: wire.AgentID, AgentVersion: wire.AgentVersion, SkillID: wire.SkillID,
		DeploymentID: wire.DeploymentID, InstanceID: wire.InstanceID, TransportProfile: wire.TransportProfile, Endpoint: wire.Endpoint,
		Generation: wire.Generation, FencingToken: wire.FencingToken, Scopes: append([]string(nil), wire.Scopes...),
		IssuedAt: time.Unix(wire.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(wire.ExpiresAt, 0).UTC(),
	}
}

func (key VerificationKey) publicKey() (*ecdsa.PublicKey, error) {
	if key.KeyID == "" || !key.NotBefore.Equal(key.NotBefore.UTC()) || !key.SignUntil.Equal(key.SignUntil.UTC()) || !key.VerifyUntil.Equal(key.VerifyUntil.UTC()) || key.SignUntil.Before(key.NotBefore) || key.VerifyUntil.Before(key.SignUntil) {
		return nil, errors.New("invalid verification key")
	}
	x, xErr := base64.RawURLEncoding.Strict().DecodeString(key.X)
	y, yErr := base64.RawURLEncoding.Strict().DecodeString(key.Y)
	if xErr != nil || yErr != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("invalid verification key")
	}
	public := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !public.Curve.IsOnCurve(public.X, public.Y) {
		return nil, errors.New("invalid verification key")
	}
	return public, nil
}

func decodeJWTJSON(encoded string, destination any) error {
	data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || !utf8.Valid(data) || len(data) == 0 || len(data) > 8192 {
		return errors.New("invalid JWT JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = walkUniqueJSON(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return err
	}
	if err = decoder.Decode(destination); err != nil {
		return err
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JWT JSON")
	}
	return nil
}

func walkUniqueJSON(decoder *json.Decoder) error {
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
			return errors.New("trailing JWT JSON")
		}
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return errors.New("invalid JWT JSON")
	}
	if err = walkCompound(decoder, delimiter); err != nil {
		return err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JWT JSON")
	}
	return nil
}

func walkCompound(decoder *json.Decoder, delimiter json.Delim) error {
	seen := map[string]struct{}{}
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return errors.New("invalid JWT object")
			}
			if _, exists := seen[name]; exists {
				return errors.New("duplicate JWT field")
			}
			seen[name] = struct{}{}
		}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if nested, ok := value.(json.Delim); ok {
			if nested != '{' && nested != '[' {
				return errors.New("invalid JWT JSON")
			}
			if err = walkCompound(decoder, nested); err != nil {
				return err
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != matchingDelimiter(delimiter) {
		return errors.New("invalid JWT JSON close")
	}
	return nil
}

func matchingDelimiter(open json.Delim) json.Delim {
	if open == '{' {
		return '}'
	}
	return ']'
}

var _ TokenVerifier = (*ES256Verifier)(nil)
