package worker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

func TestWorkerHTTPClaimRenewCompleteRelease(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: &fakeIDs{}, Tokens: fakeTokens{}, UoW: directUoW{}, Observability: &fakeObservations{}, Authorizer: scopedAuthorizer{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHTTPHandler(service, testWorkerCallers{})
	if err != nil {
		t.Fatal(err)
	}
	claimBody := fmt.Sprintf(`{"schema_version":1,"session_id":%q,"generation":7,"available_slots":1,"supported_bindings":[{"id":"image.generate","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:%s"}],"wait_seconds":0,"lease_seconds":60}`, serviceID("ses_", 1), strings.Repeat("a", 64))
	claimResponse := serveWorker(t, handler, "/v1/workers/worker-a/claims:next", claimBody, "")
	if claimResponse.Code != http.StatusOK || claimResponse.Header().Get("Cache-Control") != "no-store" || len(claimResponse.Header().Values("Content-Type")) != 1 {
		t.Fatalf("claim status=%d headers=%v body=%s", claimResponse.Code, claimResponse.Header(), claimResponse.Body.String())
	}
	claim, err := workerwire.DecodeWorkerClaim(claimResponse.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	renewBody := fmt.Sprintf(`{"schema_version":1,"fencing_token":1,"lease_token":%q,"lease_seconds":120}`, claim.LeaseToken)
	renewResponse := serveWorker(t, handler, "/v1/workers/worker-a/claims/"+string(claim.ClaimID)+":renew", renewBody, "")
	if renewResponse.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renewResponse.Code, renewResponse.Body.String())
	}
	result := fmt.Sprintf(`{"schema_version":1,"run_id":%q,"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:%s"},"usage":{"input_tokens":1,"output_tokens":1,"duration_ms":1},"completed_at":%q}`, claim.RunID, strings.Repeat("a", 64), now.Format(time.RFC3339))
	completeBody := fmt.Sprintf(`{"schema_version":1,"completion_id":%q,"claim_id":%q,"attempt_id":%q,"fencing_token":1,"lease_token":%q,"result":%s,"completed_at":%q}`, serviceID("cmp_", 1), claim.ClaimID, claim.AttemptID, claim.LeaseToken, result, now.Format(time.RFC3339))
	completeResponse := serveWorker(t, handler, "/v1/workers/worker-a/claims/"+string(claim.ClaimID)+":complete", completeBody, "worker-completion-0001")
	if completeResponse.Code != http.StatusNoContent || completeResponse.Body.Len() != 0 {
		t.Fatalf("complete status=%d body=%s", completeResponse.Code, completeResponse.Body.String())
	}
	releaseBody := fmt.Sprintf(`{"schema_version":1,"fencing_token":1,"lease_token":%q,"reason":"worker_shutdown"}`, claim.LeaseToken)
	releaseResponse := serveWorker(t, handler, "/v1/workers/worker-a/claims/"+string(claim.ClaimID)+":release", releaseBody, "")
	if releaseResponse.Code != http.StatusNoContent || repository.renews != 1 || repository.completes != 1 || repository.releases != 1 {
		t.Fatalf("release status=%d counts=%d/%d/%d", releaseResponse.Code, repository.renews, repository.completes, repository.releases)
	}
}

func TestWorkerHTTPFailsClosedAndNoWork(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	claimBody := fmt.Sprintf(`{"schema_version":1,"session_id":%q,"generation":7,"available_slots":1,"supported_bindings":[{"id":"image.generate","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:%s"}],"wait_seconds":0}`, serviceID("ses_", 1), strings.Repeat("a", 64))
	repository := &fakeRepository{claimErr: NewError(CategoryCapacity, ReasonNoWork)}
	service, _ := New(Dependencies{Clock: fixedClock{now}, IDs: &fakeIDs{}, Tokens: fakeTokens{}, UoW: directUoW{}, Observability: &fakeObservations{}, Authorizer: scopedAuthorizer{}, Repository: repository})
	handler, _ := NewHTTPHandler(service, testWorkerCallers{})
	response := serveWorker(t, handler, "/v1/workers/worker-a/claims:next", claimBody, "")
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("no work status=%d body=%s", response.Code, response.Body.String())
	}

	handler, _ = NewHTTPHandler(service, testWorkerCallers{failure: NewError(CategoryAuthentication, ReasonAuthentication)})
	response = serveWorker(t, handler, "/v1/workers/worker-a/claims:next", claimBody, "")
	if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != `Bearer realm="arop"` || !validWorkerError(response.Body.Bytes()) {
		t.Fatalf("auth status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}

	handler, _ = NewHTTPHandler(service, testWorkerCallers{})
	request := httptest.NewRequest(http.MethodPost, "/v1/workers/worker-a/claims:next", strings.NewReader(claimBody))
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType || !validWorkerError(response.Body.Bytes()) {
		t.Fatalf("duplicate content type status=%d body=%s", response.Code, response.Body.String())
	}
}

type testWorkerCallers struct{ failure error }

func (provider testWorkerCallers) Authenticate(_ *http.Request, operation Operation) (Caller, platform.RequestMetadata, error) {
	if provider.failure != nil {
		return Caller{}, platform.RequestMetadata{}, provider.failure
	}
	scope := "worker:claim"
	if operation == OperationComplete {
		scope = "worker:complete"
	}
	return Caller{TenantID: "acme", PrincipalID: serviceID("prn_", 1), CredentialID: serviceID("cred_", 1), Scopes: []string{scope}}, platform.RequestMetadata{RequestID: serviceID("req_", 1), TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), TraceFlags: "00"}, nil
}

func serveWorker(t *testing.T, handler http.Handler, target, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func validWorkerError(body []byte) bool {
	var wire map[string]any
	return json.Unmarshal(body, &wire) == nil && wire["code"] != nil && wire["category"] != nil && wire["retryable"] != nil
}

var _ HTTPCallerProvider = testWorkerCallers{}
