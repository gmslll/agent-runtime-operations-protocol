package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform"
	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

const serviceTrace = "00-11111111111111111111111111111111-2222222222222222-00"

func TestServiceWorkerLifecycleAndObservation(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	repository := &fakeRepository{}
	observations := &fakeObservations{}
	service, err := New(Dependencies{Clock: fixedClock{now}, IDs: &fakeIDs{}, Tokens: fakeTokens{}, UoW: directUoW{}, Observability: observations, Authorizer: scopedAuthorizer{}, Repository: repository})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceClaimRequest()
	claim, found, err := service.Claim(context.Background(), request)
	if err != nil || !found || claim.FencingToken != 1 || repository.claims != 1 || len(observations.audit) != 1 || observations.audit[0].HTTPStatus != 200 {
		t.Fatalf("claim found=%v claim=%#v err=%v repo=%d audit=%#v", found, claim, err, repository.claims, observations.audit)
	}
	renewed, err := service.Renew(context.Background(), RenewRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, LeaseToken: claim.LeaseToken, FencingToken: claim.FencingToken, LeaseSeconds: 120, Metadata: request.Metadata})
	if err != nil || !renewed.LeaseExpiresAt.Equal(now.Add(120*time.Second)) || repository.renews != 1 {
		t.Fatalf("renew=%#v err=%v", renewed, err)
	}
	result := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"run_id":%q,"state":"succeeded","snapshot":{"revision":1,"content":[{"type":"text","text":"done"}],"digest":"sha256:%s"},"usage":{"input_tokens":1,"output_tokens":1,"duration_ms":1},"completed_at":%q}`, claim.RunID, strings.Repeat("a", 64), now.Format(time.RFC3339)))
	replay, err := service.Complete(context.Background(), CompleteRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, CompletionID: serviceID("cmp_", 1), AttemptID: claim.AttemptID, LeaseToken: claim.LeaseToken, IdempotencyKey: "worker-completion-0001", FencingToken: claim.FencingToken, Result: result, CompletedAt: now, Metadata: request.Metadata})
	if err != nil || replay || repository.completes != 1 || observations.audit[len(observations.audit)-1].HTTPStatus != 204 {
		t.Fatalf("complete replay=%v err=%v repo=%d", replay, err, repository.completes)
	}
	if err = service.Release(context.Background(), ReleaseRequest{Caller: request.Caller, WorkerID: request.WorkerID, ClaimID: claim.ClaimID, LeaseToken: claim.LeaseToken, FencingToken: claim.FencingToken, Reason: "worker_shutdown", Metadata: request.Metadata}); err != nil || repository.releases != 1 {
		t.Fatalf("release: %v", err)
	}
	for index := range observations.audit {
		if err := observability.ValidateObservationPair(observations.audit[index], observations.spans[index]); err != nil {
			t.Fatalf("observation %d: %v", index, err)
		}
	}
}

func TestServiceFailsClosedForAuthorizationNoWorkAndAuditFailure(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	request := serviceClaimRequest()
	repository := &fakeRepository{claimErr: NewError(CategoryCapacity, ReasonNoWork)}
	observations := &fakeObservations{}
	service, _ := New(Dependencies{Clock: fixedClock{now}, IDs: &fakeIDs{}, Tokens: fakeTokens{}, UoW: directUoW{}, Observability: observations, Authorizer: scopedAuthorizer{}, Repository: repository})
	_, found, err := service.Claim(context.Background(), request)
	if err != nil || found || len(observations.audit) != 1 || observations.audit[0].HTTPStatus != 204 {
		t.Fatalf("no-work result found=%v err=%v audit=%#v", found, err, observations.audit)
	}
	request.Caller.Scopes = []string{"run:read"}
	_, _, err = service.Claim(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryAuthorization || repository.claims != 1 {
		t.Fatalf("authorization failed open: %v claims=%d", err, repository.claims)
	}
	request = serviceClaimRequest()
	observations.failure = errors.New("durable audit unavailable")
	_, _, err = service.Claim(context.Background(), request)
	if failure, ok := AsError(err); !ok || failure.Category != CategoryDependency {
		t.Fatalf("audit failure did not fail closed: %v", err)
	}
	if service.Check(context.Background()) == nil {
		t.Fatal("audit failure did not latch readiness")
	}
}

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type directUoW struct{}

func (directUoW) Within(ctx context.Context, callback func(context.Context) error) error {
	return callback(ctx)
}

type fakeIDs struct{ next int }

func (source *fakeIDs) id(prefix string) string                      { source.next++; return serviceID(prefix, source.next) }
func (source *fakeIDs) NewClaimID(context.Context) (string, error)   { return source.id("clm_"), nil }
func (source *fakeIDs) NewAttemptID(context.Context) (string, error) { return source.id("att_"), nil }
func (source *fakeIDs) NewTokenID(context.Context) (string, error)   { return source.id("tok_"), nil }
func (source *fakeIDs) NewEventID(context.Context) (string, error)   { return source.id("evt_"), nil }
func (source *fakeIDs) NewOutboxID(context.Context) (string, error)  { return source.id("out_"), nil }
func (source *fakeIDs) NewAuditID(context.Context) (string, error)   { return source.id("aud_"), nil }

type fakeTokens struct{}

func (fakeTokens) NewLeaseToken(context.Context) (string, error) {
	return "wlt_" + strings.Repeat("A", 43), nil
}

type scopedAuthorizer struct{}

func (scopedAuthorizer) Authorize(_ context.Context, caller Caller, operation Operation, _ string) error {
	want := "worker:claim"
	if operation == OperationComplete {
		want = "worker:complete"
	}
	for _, scope := range caller.Scopes {
		if scope == want {
			return nil
		}
	}
	return NewError(CategoryAuthorization, ReasonAuthorization)
}

type fakeObservations struct {
	audit   []observability.AuditEntry
	spans   []observability.SpanRecord
	failure error
}

func (store *fakeObservations) AppendObservation(_ context.Context, audit observability.AuditEntry, span observability.SpanRecord) error {
	if store.failure != nil {
		return store.failure
	}
	store.audit = append(store.audit, audit)
	store.spans = append(store.spans, span)
	return nil
}

type fakeRepository struct {
	claims, renews, completes, releases int
	claimErr                            error
	claim                               Claim
}

func (repository *fakeRepository) Claim(_ context.Context, command ClaimCommand) (Claim, error) {
	repository.claims++
	if repository.claimErr != nil {
		return Claim{}, repository.claimErr
	}
	runRequest := json.RawMessage(fmt.Sprintf(`{"schema_version":1,"agent":{"id":"image.generate","version":"1.0.0","skill_id":"default","manifest_digest":"sha256:%s"},"input":[{"type":"text","text":"work"}],"deadline_at":"2026-09-27T11:00:00Z","effects":{"level":"none"},"trace":{"traceparent":%q}}`, strings.Repeat("a", 64), serviceTrace))
	repository.claim = Claim{TenantID: command.Request.Caller.TenantID, ClaimID: command.ClaimID, WorkerID: command.Request.WorkerID, SessionID: command.Request.SessionID, Generation: command.Request.Generation, RunID: serviceID("run_", 1), AttemptID: serviceID("att_", 1), AttemptNumber: 1, FencingToken: 1, LeaseToken: command.LeaseToken, LeaseTokenDigest: command.LeaseTokenDigest, LeaseExpiresAt: command.LeaseExpiresAt, ClaimedAt: command.Now, RunRequest: runRequest}
	return repository.claim, nil
}
func (repository *fakeRepository) Renew(_ context.Context, command RenewCommand) (Claim, error) {
	repository.renews++
	claim := repository.claim
	claim.LeaseExpiresAt = command.ExpiresAt
	return claim, nil
}
func (repository *fakeRepository) Complete(context.Context, CompleteCommand) (bool, error) {
	repository.completes++
	return false, nil
}
func (repository *fakeRepository) Release(context.Context, ReleaseCommand) error {
	repository.releases++
	return nil
}
func (repository *fakeRepository) Check(context.Context) error { return nil }

func serviceClaimRequest() ClaimRequest {
	return ClaimRequest{Caller: Caller{TenantID: "acme", PrincipalID: serviceID("prn_", 1), CredentialID: serviceID("cred_", 1), Scopes: []string{"worker:claim", "worker:complete"}}, WorkerID: "worker-a", SessionID: serviceID("ses_", 1), Generation: 7, AvailableSlots: 1, SupportedBindings: []Binding{{AgentID: "image.generate", Version: "1.0.0", SkillID: "default", ManifestDigest: "sha256:" + strings.Repeat("a", 64)}}, LeaseSeconds: 60, Metadata: platform.RequestMetadata{RequestID: serviceID("req_", 1), TraceID: strings.Repeat("1", 32), SpanID: strings.Repeat("2", 16), TraceFlags: "00"}}
}
func serviceID(prefix string, sequence int) string {
	return fmt.Sprintf("%s01932f13-0cd2-7a82-8fa3-%012x", prefix, sequence)
}

var _ platformports.UnitOfWork = directUoW{}
