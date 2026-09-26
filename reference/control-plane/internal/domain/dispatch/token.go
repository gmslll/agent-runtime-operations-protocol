package dispatch

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/run"
)

type tokenHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type TokenClaims struct {
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

func (claims TokenClaims) Validate() error {
	endpoint, endpointErr := url.Parse(claims.Endpoint)
	audience, audienceErr := url.Parse(claims.Audience)
	if _, err := urlParseIssuer(claims.Issuer); err != nil || !prefixedUUID("prn_", claims.Subject) || !prefixedUUID("cred_", claims.AuthorizedParty) || !prefixedUUID("tok_", claims.TokenID) || !prefixedUUID("run_", claims.RunID) || !prefixedUUID("att_", claims.AttemptID) || (run.AgentBinding{ID: claims.AgentID, Version: claims.AgentVersion, SkillID: claims.SkillID, ManifestDigest: "sha256:" + strings.Repeat("0", 64)}).Validate() != nil || !prefixedUUID("dep_", claims.DeploymentID) || !slugPattern.MatchString(claims.InstanceID) || len(claims.InstanceID) > 128 || claims.Generation == 0 || claims.Generation > MaxSafeInteger || claims.FencingToken == 0 || claims.FencingToken > MaxSafeInteger || claims.TransportProfile != "direct" && claims.TransportProfile != "proxy" && claims.TransportProfile != "worker_pull" || endpointErr != nil || len(claims.Endpoint) > 2048 || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/v1/runs" || audienceErr != nil || len(claims.Audience) > 2048 || audience.Scheme != "https" || audience.Host == "" || audience.User != nil || audience.RawQuery != "" || audience.Fragment != "" || audience.Path != "/deployments/"+claims.DeploymentID || claims.IssuedAt <= 0 || claims.NotBefore != claims.IssuedAt || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64((5*time.Minute)/time.Second) || len(claims.Scopes) == 0 || len(claims.Scopes) > 16 {
		return NewError(CategoryValidation, ReasonTicketInvalid)
	}
	seen := map[string]struct{}{}
	for _, scope := range claims.Scopes {
		if !scopePattern.MatchString(scope) {
			return NewError(CategoryValidation, ReasonTicketInvalid)
		}
		if _, exists := seen[scope]; exists {
			return NewError(CategoryValidation, ReasonTicketInvalid)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

func issueToken(ctx context.Context, signer Signer, key KeyMetadata, claims TokenClaims) (string, error) {
	issuedAt := time.Unix(claims.IssuedAt, 0).UTC()
	if err := ctx.Err(); err != nil || signer == nil || key.Status != KeyActive || key.Validate(5*time.Minute) != nil || issuedAt.Before(key.NotBefore) || !issuedAt.Before(key.SignUntil) || claims.Validate() != nil {
		return "", NewError(CategoryTimeout, ReasonDependencyUnavailable)
	}
	headerBytes, _ := json.Marshal(tokenHeader{Algorithm: "ES256", KeyID: key.KeyID, Type: "JWT"})
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	encoding := base64.RawURLEncoding
	input := encoding.EncodeToString(headerBytes) + "." + encoding.EncodeToString(claimsBytes)
	signature, err := signer.Sign(ctx, key.KeyID, []byte(input))
	if err != nil || len(signature) != 64 {
		return "", NewError(CategoryDependency, ReasonDependencyUnavailable)
	}
	return input + "." + encoding.EncodeToString(signature), nil
}

type VerifyExpectation struct {
	Issuer, Audience, RunID, AttemptID, AgentID, AgentVersion, SkillID string
	DeploymentID, InstanceID, TransportProfile, Endpoint               string
	Generation, FencingToken                                           uint64
	ExpiresAt, Now                                                     time.Time
	RequiredScope                                                      string
}

func VerifyToken(token string, key KeyMetadata, expected VerifyExpectation) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !utc(expected.Now) || !utc(expected.ExpiresAt) || key.Validate(5*time.Minute) != nil || expected.Now.Before(key.NotBefore) || !expected.Now.Before(key.VerifyUntil) {
		return TokenClaims{}, NewError(CategoryValidation, ReasonTicketInvalid)
	}
	decode := func(value string, target any) error {
		data, err := base64.RawURLEncoding.Strict().DecodeString(value)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(target); err != nil {
			return err
		}
		if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("trailing token JSON")
		}
		return nil
	}
	var header tokenHeader
	var claims TokenClaims
	if decode(parts[0], &header) != nil || decode(parts[1], &claims) != nil || header != (tokenHeader{Algorithm: "ES256", KeyID: key.KeyID, Type: "JWT"}) || claims.Validate() != nil {
		return TokenClaims{}, NewError(CategoryValidation, ReasonTicketInvalid)
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	public, publicErr := key.PublicKey()
	if err != nil || publicErr != nil || len(signature) != 64 || !verifyES256(public, []byte(parts[0]+"."+parts[1]), signature) {
		return TokenClaims{}, NewError(CategoryValidation, ReasonTicketInvalid)
	}
	now := expected.Now.Unix()
	if claims.Issuer != expected.Issuer || claims.Audience != expected.Audience || claims.RunID != expected.RunID || claims.AttemptID != expected.AttemptID || claims.AgentID != expected.AgentID || claims.AgentVersion != expected.AgentVersion || claims.SkillID != expected.SkillID || claims.DeploymentID != expected.DeploymentID || claims.InstanceID != expected.InstanceID || claims.TransportProfile != expected.TransportProfile || claims.Endpoint != expected.Endpoint || claims.Generation != expected.Generation || claims.FencingToken != expected.FencingToken || claims.IssuedAt < key.NotBefore.Unix() || claims.IssuedAt >= key.SignUntil.Unix() || claims.IssuedAt > now || claims.NotBefore != claims.IssuedAt || claims.NotBefore > now || claims.ExpiresAt <= now || claims.ExpiresAt != expected.ExpiresAt.Unix() || claims.ExpiresAt-claims.IssuedAt > int64((5*time.Minute)/time.Second) || !slicesContains(claims.Scopes, expected.RequiredScope) {
		return TokenClaims{}, NewError(CategoryAuthorization, ReasonTicketInvalid)
	}
	return claims, nil
}

func verifyES256(public *ecdsa.PublicKey, input, signature []byte) bool {
	digest := sha256.Sum256(input)
	return ecdsa.Verify(public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:]))
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func tokenClaims(attempt Attempt, view RunView, caller Caller, issuer string, issuedAt time.Time) TokenClaims {
	return TokenClaims{
		Issuer: issuer, Audience: attempt.Audience, Subject: caller.PrincipalID, AuthorizedParty: caller.CredentialID,
		TokenID: attempt.TokenID, RunID: attempt.RunID, AttemptID: attempt.AttemptID,
		AgentID: view.Agent.ID, AgentVersion: view.Agent.Version, SkillID: view.Agent.SkillID,
		DeploymentID: attempt.DeploymentID, InstanceID: attempt.InstanceID, TransportProfile: attempt.TransportProfile, Endpoint: attempt.Endpoint, Generation: attempt.Generation, FencingToken: attempt.FencingToken,
		Scopes: []string{"agent:invoke", "run:stream"}, IssuedAt: issuedAt.Unix(), NotBefore: issuedAt.Unix(), ExpiresAt: attempt.TicketExpiresAt.Unix(),
	}
}

func urlParseIssuer(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", fmt.Errorf("invalid issuer")
	}
	return strings.TrimSuffix(value, "/"), nil
}
