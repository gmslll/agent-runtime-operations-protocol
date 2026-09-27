package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestES256VerifierBindsEveryRuntimeFact(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := verificationKey(private, now)
	configuration := ES256VerifierConfig{
		Keys: StaticKeys{key.KeyID: key}, Issuer: "https://control.example.invalid", Audience: "https://control.example.invalid/deployments/dep_01999999-9999-7999-8999-999999999997",
		Endpoint: "https://runtime.example.invalid/v1/runs", DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a", Generation: 7, TransportProfile: "direct",
	}
	verifier, err := NewES256Verifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	claims := validJWTClaims(now)
	token := signToken(t, private, key.KeyID, claims)
	verified, err := verifier.Verify(context.Background(), token, VerifyRequest{Method: "POST", Path: "/v1/runs", RequiredScope: "agent:invoke", Now: now})
	if err != nil || verified.RunID != claims.RunID || verified.FencingToken != claims.FencingToken {
		t.Fatalf("valid token rejected: %#v %v", verified, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*jwtClaims)
		path   string
	}{
		{name: "audience", mutate: func(value *jwtClaims) {
			value.Audience = "https://wrong.example.invalid/deployments/" + value.DeploymentID
		}},
		{name: "endpoint", mutate: func(value *jwtClaims) { value.Endpoint = "https://wrong.example.invalid/v1/runs" }},
		{name: "generation", mutate: func(value *jwtClaims) { value.Generation++ }},
		{name: "fencing", mutate: func(value *jwtClaims) { value.FencingToken = 0 }},
		{name: "not-before", mutate: func(value *jwtClaims) { value.NotBefore++ }},
		{name: "scope", mutate: func(value *jwtClaims) { value.Scopes = []string{"run:stream"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := claims
			candidate.Scopes = append([]string(nil), claims.Scopes...)
			test.mutate(&candidate)
			if _, verifyErr := verifier.Verify(context.Background(), signToken(t, private, key.KeyID, candidate), VerifyRequest{Method: "POST", Path: "/v1/runs", RequiredScope: "agent:invoke", Now: now}); verifyErr == nil {
				t.Fatal("mutated token accepted")
			}
		})
	}
	if _, err = verifier.Verify(context.Background(), token, VerifyRequest{Method: "GET", Path: "/v1/runs/run_01999999-9999-7999-8999-999999999990", RequiredScope: "agent:invoke", Now: now}); err == nil {
		t.Fatal("token accepted for a different run path")
	}
	tamperedParts := strings.Split(token, ".")
	tamperedSignature, decodeErr := base64.RawURLEncoding.Strict().DecodeString(tamperedParts[2])
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	tamperedSignature[0] ^= 1
	tamperedParts[2] = base64.RawURLEncoding.EncodeToString(tamperedSignature)
	tampered := strings.Join(tamperedParts, ".")
	if _, err = verifier.Verify(context.Background(), tampered, VerifyRequest{Method: "POST", Path: "/v1/runs", RequiredScope: "agent:invoke", Now: now}); err == nil {
		t.Fatal("tampered signature accepted")
	}
}

func TestES256VerifierRejectsDuplicateAndNonCanonicalBase64(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	key := verificationKey(private, now)
	verifier, _ := NewES256Verifier(ES256VerifierConfig{Keys: StaticKeys{key.KeyID: key}, Issuer: "https://control.example.invalid", Audience: "https://control.example.invalid/deployments/dep_01999999-9999-7999-8999-999999999997", Endpoint: "https://runtime.example.invalid/v1/runs", DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a", Generation: 7, TransportProfile: "direct"})
	claimsJSON, _ := json.Marshal(validJWTClaims(now))
	duplicate := strings.TrimSuffix(string(claimsJSON), "}") + `,"run_id":"run_01999999-9999-7999-8999-999999999999"}`
	token := signRawToken(t, private, key.KeyID, []byte(duplicate))
	if _, err := verifier.Verify(context.Background(), token, VerifyRequest{Method: "POST", Path: "/v1/runs", RequiredScope: "agent:invoke", Now: now}); err == nil {
		t.Fatal("duplicate claim accepted")
	}
	parts := strings.Split(signToken(t, private, key.KeyID, validJWTClaims(now)), ".")
	parts[0] += "="
	if _, err := verifier.Verify(context.Background(), strings.Join(parts, "."), VerifyRequest{Method: "POST", Path: "/v1/runs", RequiredScope: "agent:invoke", Now: now}); err == nil {
		t.Fatal("padded base64url accepted")
	}
}

func validJWTClaims(now time.Time) jwtClaims {
	return jwtClaims{
		Issuer: "https://control.example.invalid", Audience: "https://control.example.invalid/deployments/dep_01999999-9999-7999-8999-999999999997",
		Subject: "prn_01999999-9999-7999-8999-999999999995", AuthorizedParty: "cred_01999999-9999-7999-8999-999999999994", TokenID: "tok_01999999-9999-7999-8999-999999999993",
		RunID: "run_01999999-9999-7999-8999-999999999999", AttemptID: "att_01999999-9999-7999-8999-999999999998", AgentID: "test.agent", AgentVersion: "1.0.0", SkillID: "default",
		DeploymentID: "dep_01999999-9999-7999-8999-999999999997", InstanceID: "runtime-a", TransportProfile: "direct", Endpoint: "https://runtime.example.invalid/v1/runs",
		Generation: 7, FencingToken: 9, Scopes: []string{"agent:invoke", "run:stream"}, IssuedAt: now.Add(-time.Minute).Unix(), NotBefore: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(),
	}
}

func verificationKey(private *ecdsa.PrivateKey, now time.Time) VerificationKey {
	pad := func(value *big.Int) []byte {
		result := make([]byte, 32)
		value.FillBytes(result)
		return result
	}
	return VerificationKey{KeyID: "dispatch-key-01", X: base64.RawURLEncoding.EncodeToString(pad(private.X)), Y: base64.RawURLEncoding.EncodeToString(pad(private.Y)), NotBefore: now.Add(-time.Hour), SignUntil: now.Add(time.Hour), VerifyUntil: now.Add(2 * time.Hour)}
}

func signToken(t *testing.T, private *ecdsa.PrivateKey, keyID string, claims jwtClaims) string {
	t.Helper()
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return signRawToken(t, private, keyID, encoded)
}

func signRawToken(t *testing.T, private *ecdsa.PrivateKey, keyID string, claims []byte) string {
	t.Helper()
	header, _ := json.Marshal(jwtHeader{Algorithm: "ES256", KeyID: keyID, Type: "JWT"})
	first := base64.RawURLEncoding.EncodeToString(header)
	second := base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(first + "." + second))
	r, s, err := ecdsa.Sign(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return first + "." + second + "." + base64.RawURLEncoding.EncodeToString(signature)
}
