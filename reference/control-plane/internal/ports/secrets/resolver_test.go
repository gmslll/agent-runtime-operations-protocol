package secrets

import (
	"errors"
	"testing"
	"time"
)

func TestResolveRequestValidate(t *testing.T) {
	valid := ResolveRequest{
		Reference: "billing-api", Operation: "read", SubjectID: "svc:runtime",
		CredentialID: "cred_01", Scopes: []string{"secret.read"},
		Deadline:  time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC),
		RequestID: "req_01890f28-70d2-7cc0-98c4-dc0c0c07398f",
		TraceID:   "80e1afed08e019fc1110464cfa66635c",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	cases := map[string]ResolveRequest{
		"empty reference":    withRequest(valid, func(r *ResolveRequest) { r.Reference = "" }),
		"invalid operation":  withRequest(valid, func(r *ResolveRequest) { r.Operation = "read secret" }),
		"missing subject":    withRequest(valid, func(r *ResolveRequest) { r.SubjectID = "" }),
		"missing credential": withRequest(valid, func(r *ResolveRequest) { r.CredentialID = "" }),
		"missing scope":      withRequest(valid, func(r *ResolveRequest) { r.Scopes = nil }),
		"duplicate scope":    withRequest(valid, func(r *ResolveRequest) { r.Scopes = []string{"secret.read", "secret.read"} }),
		"non UTC deadline":   withRequest(valid, func(r *ResolveRequest) { r.Deadline = r.Deadline.In(time.FixedZone("test", 3600)) }),
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			if err := request.Validate(); !errors.Is(err, ErrDenied) {
				t.Fatalf("got %v, want generic denial", err)
			}
		})
	}
}

func TestSecretViewUsesProvidedStorage(t *testing.T) {
	storage := []byte("not-a-real-secret")
	view := NewView(storage)
	view.Bytes()[0] = 'N'
	if storage[0] != 'N' {
		t.Fatal("view unexpectedly copied callback storage")
	}
}

func withRequest(base ResolveRequest, change func(*ResolveRequest)) ResolveRequest {
	copy := base
	copy.Scopes = append([]string(nil), base.Scopes...)
	change(&copy)
	return copy
}
