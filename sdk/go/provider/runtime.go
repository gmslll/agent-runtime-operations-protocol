package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	runwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/run"
)

const maxBodyBytes = 8 << 20

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	scopePattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.:_-][a-z0-9]+)*$`)
	uuidPattern       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Claims are the already authenticated invocation facts. A verifier must
// validate the signature, issuer, audience, time window and revocation/fencing
// policy before returning them.
type Claims struct {
	Issuer, Audience, Subject, AuthorizedParty, TokenID  string
	RunID, AttemptID, AgentID, AgentVersion, SkillID     string
	DeploymentID, InstanceID, TransportProfile, Endpoint string
	Generation, FencingToken                             uint64
	Scopes                                               []string
	IssuedAt, ExpiresAt                                  time.Time
}

// VerifyRequest binds token authentication to the concrete HTTP operation.
type VerifyRequest struct {
	Method, Path, RequiredScope string
	Now                         time.Time
}

// TokenVerifier keeps key discovery and token algorithms outside the runtime
// HTTP layer. It must never log or persist RawToken.
type TokenVerifier interface {
	Verify(context.Context, string, VerifyRequest) (Claims, error)
}

// Handler executes one immutable attempt. It may use Execution.Effect for
// stable external effects and Execution.Emit for durable outbox events.
type Handler interface {
	Execute(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, *Execution, runwire.RunRequest) (runwire.RunResult, error)

func (function HandlerFunc) Execute(ctx context.Context, execution *Execution, request runwire.RunRequest) (runwire.RunResult, error) {
	return function(ctx, execution, request)
}

// Config contains mandatory provider runtime dependencies.
type Config struct {
	Store        DurableStore
	Verifier     TokenVerifier
	Handler      Handler
	Clock        func() time.Time
	MaxBodyBytes int64
}

// Runtime is an http.Handler for the non-streaming Agent Runtime v1 surface.
type Runtime struct {
	config  Config
	cancelM sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewRuntime constructs a fail-closed provider runtime.
func NewRuntime(config Config) (*Runtime, error) {
	if config.Store == nil || config.Verifier == nil || config.Handler == nil {
		return nil, errors.New("provider runtime dependencies are required")
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = maxBodyBytes
	}
	if config.MaxBodyBytes < 1024 || config.MaxBodyBytes > maxBodyBytes {
		return nil, errors.New("provider runtime body limit is invalid")
	}
	return &Runtime{config: config, cancels: map[string]context.CancelFunc{}}, nil
}

// Recover restarts durable accepted/running work after a process restart.
// Attempts whose deadlines elapsed are finalized as timed_out instead.
func (runtime *Runtime) Recover(ctx context.Context, limit int) error {
	if limit < 1 || limit > 1000 {
		return errors.New("invalid recovery limit")
	}
	now := runtime.now()
	records, err := runtime.config.Store.ListRecoverable(ctx, now, limit)
	if err != nil {
		return err
	}
	for _, record := range records {
		if !now.Before(record.DeadlineAt) {
			if err = runtime.finalize(ctx, record, timedOutResult(record.RunID, now), InboxTimedOut); err != nil {
				return err
			}
			continue
		}
		runtime.start(record)
	}
	return nil
}

func (runtime *Runtime) now() time.Time { return runtime.config.Clock().UTC() }

func (runtime *Runtime) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	if request.URL.RawQuery != "" || request.URL.Fragment != "" {
		writeProblem(response, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/health/live":
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && request.URL.Path == "/health/ready":
		if runtime.config.Store.Ready(request.Context()) != nil {
			writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
			return
		}
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/runs":
		runtime.create(response, request)
	case strings.HasPrefix(request.URL.Path, "/v1/runs/"):
		runtime.routeRun(response, request)
	default:
		writeProblem(response, http.StatusNotFound, "NOT_FOUND")
	}
}

func (runtime *Runtime) routeRun(response http.ResponseWriter, request *http.Request) {
	remainder := strings.TrimPrefix(request.URL.Path, "/v1/runs/")
	if strings.HasSuffix(remainder, "/commands") {
		runID := strings.TrimSuffix(remainder, "/commands")
		if request.Method != http.MethodPost || strings.Contains(runID, "/") || !validPrefixed("run_", runID) {
			writeProblem(response, http.StatusNotFound, "NOT_FOUND")
			return
		}
		runtime.command(response, request, runID)
		return
	}
	if request.Method != http.MethodGet || strings.Contains(remainder, "/") || !validPrefixed("run_", remainder) {
		writeProblem(response, http.StatusNotFound, "NOT_FOUND")
		return
	}
	runtime.status(response, request, remainder)
}

func (runtime *Runtime) create(response http.ResponseWriter, request *http.Request) {
	if !singleMediaType(request.Header, "Content-Type", "application/json") {
		writeProblem(response, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
		return
	}
	token, ok := bearer(request.Header)
	if !ok {
		writeUnauthorized(response)
		return
	}
	key, ok := singleHeader(request.Header, "Idempotency-Key")
	if !ok || !validPrefixed("att_", key) {
		writeProblem(response, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY")
		return
	}
	claims, err := runtime.verify(request.Context(), token, request.Method, request.URL.Path)
	if err != nil || claims.AttemptID != key {
		writeUnauthorized(response)
		return
	}
	body, err := readBody(request.Body, runtime.config.MaxBodyBytes)
	if err != nil {
		writeProblem(response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
		return
	}
	wire, err := runwire.DecodeRunRequest(body)
	if err != nil {
		writeProblem(response, http.StatusBadRequest, "INVALID_RUN_REQUEST")
		return
	}
	normalized, err := runwire.EncodeRunRequest(wire)
	deadline, deadlineErr := time.Parse(time.RFC3339Nano, string(wire.DeadlineAt))
	if err != nil || deadlineErr != nil || !deadline.UTC().Equal(deadline) || !runtime.now().Before(deadline) || !claimsBindRequest(claims, wire) {
		writeProblem(response, http.StatusBadRequest, "INVALID_RUN_REQUEST")
		return
	}
	digest := sha256Digest(normalized)
	authDigest := claimsDigest(claims)
	now := runtime.now()
	record := InboxRecord{
		RunID: claims.RunID, AttemptID: claims.AttemptID, RequestDigest: digest, AuthorizationDigest: authDigest,
		RequestJSON: normalized, AgentID: claims.AgentID, AgentVersion: claims.AgentVersion, SkillID: claims.SkillID,
		DeploymentID: claims.DeploymentID, InstanceID: claims.InstanceID, Generation: claims.Generation,
		FencingToken: claims.FencingToken, StateVersion: 1, Traceparent: wire.Trace.Traceparent,
		State: InboxAccepted, DeadlineAt: deadline, CreatedAt: now, UpdatedAt: now,
	}
	if wire.Trace.Tracestate != nil {
		record.Tracestate = *wire.Trace.Tracestate
	}
	created := false
	err = runtime.config.Store.Within(request.Context(), func(ctx context.Context, transaction Transaction) error {
		existing, findErr := transaction.GetInbox(ctx, record.RunID, record.AttemptID)
		if findErr == nil {
			if existing.RequestDigest != record.RequestDigest || existing.AuthorizationDigest != record.AuthorizationDigest {
				return ErrConflict
			}
			record = existing
			return nil
		}
		if !errors.Is(findErr, ErrNotFound) {
			return findErr
		}
		if createErr := transaction.CreateInbox(ctx, record); createErr != nil {
			return createErr
		}
		_, appendErr := transaction.AppendOutbox(ctx, lifecycleEvent(record, "arop.run.accepted", now))
		created = appendErr == nil
		return appendErr
	})
	if errors.Is(err, ErrConflict) {
		writeProblem(response, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
		return
	}
	if err != nil {
		writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
		return
	}
	if created {
		runtime.start(record)
	}
	response.Header().Set("Location", "/v1/runs/"+record.RunID)
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(runtime.statusValue(record))
}

func (runtime *Runtime) status(response http.ResponseWriter, request *http.Request, runID string) {
	token, ok := bearer(request.Header)
	if !ok {
		writeUnauthorized(response)
		return
	}
	claims, err := runtime.verify(request.Context(), token, request.Method, request.URL.Path)
	if err != nil || claims.RunID != runID {
		writeUnauthorized(response)
		return
	}
	record, err := runtime.config.Store.GetInbox(request.Context(), runID, claims.AttemptID)
	if errors.Is(err, ErrNotFound) {
		writeProblem(response, http.StatusNotFound, "RUN_NOT_FOUND")
		return
	}
	if err != nil {
		writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
		return
	}
	_ = json.NewEncoder(response).Encode(runtime.statusValue(record))
}

func (runtime *Runtime) command(response http.ResponseWriter, request *http.Request, runID string) {
	if !singleMediaType(request.Header, "Content-Type", "application/json") {
		writeProblem(response, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
		return
	}
	token, ok := bearer(request.Header)
	if !ok {
		writeUnauthorized(response)
		return
	}
	claims, err := runtime.verify(request.Context(), token, request.Method, request.URL.Path)
	if err != nil || claims.RunID != runID {
		writeUnauthorized(response)
		return
	}
	body, err := readBody(request.Body, runtime.config.MaxBodyBytes)
	if err != nil {
		writeProblem(response, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE")
		return
	}
	command, err := runwire.DecodeRunCommand(body)
	if err != nil || command.Type != "run.cancel" {
		writeProblem(response, http.StatusBadRequest, "INVALID_RUN_COMMAND")
		return
	}
	now := runtime.now()
	var record InboxRecord
	err = runtime.config.Store.Within(request.Context(), func(ctx context.Context, transaction Transaction) error {
		current, findErr := transaction.GetInbox(ctx, runID, claims.AttemptID)
		if findErr != nil {
			return findErr
		}
		record = current
		if current.State.Terminal() || current.State == InboxCancelRequested {
			return nil
		}
		if uint64(command.ExpectedStateVersion) != current.StateVersion {
			return ErrConflict
		}
		current.State = InboxCancelRequested
		current.StateVersion++
		current.UpdatedAt = now
		current.CancelRequestedAt = &now
		if updateErr := transaction.UpdateInbox(ctx, current, record.StateVersion); updateErr != nil {
			return updateErr
		}
		if _, appendErr := transaction.AppendOutbox(ctx, lifecycleEvent(current, "arop.run.cancel_requested", now)); appendErr != nil {
			return appendErr
		}
		record = current
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		writeProblem(response, http.StatusNotFound, "RUN_NOT_FOUND")
		return
	}
	if errors.Is(err, ErrConflict) {
		writeProblem(response, http.StatusConflict, "STATE_VERSION_CONFLICT")
		return
	}
	if err != nil {
		writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
		return
	}
	runtime.cancelM.Lock()
	cancel := runtime.cancels[record.RunID+"/"+record.AttemptID]
	runtime.cancelM.Unlock()
	if cancel != nil {
		cancel()
	}
	response.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(response).Encode(runtime.statusValue(record))
}

func (runtime *Runtime) start(record InboxRecord) {
	key := record.RunID + "/" + record.AttemptID
	runtime.cancelM.Lock()
	if _, exists := runtime.cancels[key]; exists {
		runtime.cancelM.Unlock()
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), record.DeadlineAt)
	runtime.cancels[key] = cancel
	runtime.cancelM.Unlock()
	go func() {
		defer func() {
			cancel()
			runtime.cancelM.Lock()
			delete(runtime.cancels, key)
			runtime.cancelM.Unlock()
		}()
		runtime.execute(ctx, record)
	}()
}

func (runtime *Runtime) execute(ctx context.Context, record InboxRecord) {
	if err := runtime.markRunning(ctx, &record); err != nil {
		if current, loadErr := runtime.config.Store.GetInbox(context.Background(), record.RunID, record.AttemptID); loadErr == nil && current.State == InboxCancelRequested {
			_ = runtime.finalize(context.Background(), current, cancelledResult(current.RunID, runtime.now()), InboxCancelled)
		}
		return
	}
	request, err := runwire.DecodeRunRequest(record.RequestJSON)
	if err != nil {
		_ = runtime.finalize(context.Background(), record, failedResult(record.RunID, runtime.now()), InboxFailed)
		return
	}
	execution := &Execution{store: runtime.config.Store, record: record, clock: runtime.config.Clock}
	result, handlerErr := runtime.config.Handler.Execute(ctx, execution, request)
	now := runtime.now()
	state := InboxSucceeded
	if handlerErr != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			result, state = timedOutResult(record.RunID, now), InboxTimedOut
		case errors.Is(ctx.Err(), context.Canceled):
			result, state = cancelledResult(record.RunID, now), InboxCancelled
		default:
			result, state = failedResult(record.RunID, now), InboxFailed
		}
	}
	encoded, encodeErr := runwire.EncodeRunResult(result)
	if encodeErr != nil || string(result.RunID) != record.RunID || result.State != string(state) {
		result, state = failedResult(record.RunID, now), InboxFailed
		encoded, _ = runwire.EncodeRunResult(result)
	}
	record.ResultJSON = encoded
	_ = runtime.finalize(context.Background(), record, result, state)
}

func (runtime *Runtime) markRunning(ctx context.Context, record *InboxRecord) error {
	now := runtime.now()
	return runtime.config.Store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		current, err := transaction.GetInbox(txctx, record.RunID, record.AttemptID)
		if err != nil {
			return err
		}
		if current.State.Terminal() || current.State == InboxCancelRequested {
			return ErrConflict
		}
		previous := current.StateVersion
		current.State, current.StateVersion, current.UpdatedAt = InboxRunning, previous+1, now
		if err = transaction.UpdateInbox(txctx, current, previous); err != nil {
			return err
		}
		if _, err = transaction.AppendOutbox(txctx, lifecycleEvent(current, "arop.run.running", now)); err != nil {
			return err
		}
		*record = current
		return nil
	})
}

func (runtime *Runtime) finalize(ctx context.Context, record InboxRecord, result runwire.RunResult, state InboxState) error {
	encoded, err := runwire.EncodeRunResult(result)
	if err != nil {
		return err
	}
	now := runtime.now()
	return runtime.config.Store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		current, findErr := transaction.GetInbox(txctx, record.RunID, record.AttemptID)
		if findErr != nil {
			return findErr
		}
		if current.State.Terminal() {
			return nil
		}
		previous := current.StateVersion
		current.State, current.StateVersion, current.UpdatedAt, current.ResultJSON = state, previous+1, now, encoded
		if updateErr := transaction.UpdateInbox(txctx, current, previous); updateErr != nil {
			return updateErr
		}
		_, appendErr := transaction.AppendOutbox(txctx, lifecycleEvent(current, "arop.run."+string(state), now))
		return appendErr
	})
}

// Execution supplies durable effect and outbox helpers to a Handler.
type Execution struct {
	store  DurableStore
	record InboxRecord
	clock  func() time.Time
}

func (execution *Execution) RunID() string     { return execution.record.RunID }
func (execution *Execution) AttemptID() string { return execution.record.AttemptID }

// Effect executes callback at most once automatically. A previously-started
// effect is reported as uncertain and never repeated blindly.
func (execution *Execution) Effect(ctx context.Context, effectID string, request json.RawMessage, callback func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if !regexp.MustCompile(`^eff_[A-Za-z0-9._:-]{4,196}$`).MatchString(effectID) || callback == nil || !json.Valid(request) {
		return nil, errors.New("invalid durable effect")
	}
	digest := sha256Digest(request)
	now := execution.clock().UTC()
	claim := EffectRecord{EffectID: effectID, RunID: execution.record.RunID, AttemptID: execution.record.AttemptID, RequestDigest: digest, State: EffectStarted, StartedAt: now, UpdatedAt: now}
	err := execution.store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		existing, findErr := transaction.GetEffect(txctx, effectID)
		if findErr == nil {
			if existing.RequestDigest != digest || existing.RunID != claim.RunID {
				return ErrConflict
			}
			if existing.State == EffectCompleted {
				claim = existing
				return nil
			}
			return ErrEffectUncertain
		}
		if !errors.Is(findErr, ErrNotFound) {
			return findErr
		}
		return transaction.CreateEffect(txctx, claim)
	})
	if err != nil {
		return nil, err
	}
	if claim.State == EffectCompleted {
		return bytes.Clone(claim.Result), nil
	}
	result, err := callback(ctx)
	if err != nil {
		return nil, err
	}
	if !json.Valid(result) {
		return nil, errors.New("effect result is not JSON")
	}
	now = execution.clock().UTC()
	err = execution.store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		return transaction.CompleteEffect(txctx, effectID, digest, result, now)
	})
	return bytes.Clone(result), err
}

// Emit durably appends one provider event before any delivery attempt.
func (execution *Execution) Emit(ctx context.Context, eventID, eventType string, envelope json.RawMessage) (uint64, error) {
	if eventID == "" || !identifierPattern.MatchString(eventType) || !json.Valid(envelope) {
		return 0, errors.New("invalid outbox event")
	}
	record := OutboxRecord{RunID: execution.record.RunID, AttemptID: execution.record.AttemptID, EventID: eventID, EventType: eventType, Envelope: bytes.Clone(envelope), CreatedAt: execution.clock().UTC()}
	var sequence uint64
	err := execution.store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		var appendErr error
		sequence, appendErr = transaction.AppendOutbox(txctx, record)
		return appendErr
	})
	return sequence, err
}

func (runtime *Runtime) verify(ctx context.Context, token, method, path string) (Claims, error) {
	now := runtime.now()
	claims, err := runtime.config.Verifier.Verify(ctx, token, VerifyRequest{Method: method, Path: path, RequiredScope: "agent:invoke", Now: now})
	if err != nil || claims.Validate(now, "agent:invoke") != nil {
		return Claims{}, errors.New("authentication failed")
	}
	return claims, nil
}

// Validate verifies the non-cryptographic invariants of authenticated claims.
func (claims Claims) Validate(now time.Time, scope string) error {
	issuer, issuerErr := url.Parse(claims.Issuer)
	audience, audienceErr := url.Parse(claims.Audience)
	endpoint, endpointErr := url.Parse(claims.Endpoint)
	if issuerErr != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.Path != "" || issuer.RawQuery != "" || issuer.Fragment != "" || audienceErr != nil || audience.Scheme != "https" || audience.Host == "" || audience.User != nil || audience.Path != "/deployments/"+claims.DeploymentID || audience.RawQuery != "" || audience.Fragment != "" || endpointErr != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "/v1/runs" || endpoint.RawQuery != "" || endpoint.Fragment != "" || !validPrefixed("prn_", claims.Subject) || !validPrefixed("cred_", claims.AuthorizedParty) || !validPrefixed("tok_", claims.TokenID) || !validPrefixed("run_", claims.RunID) || !validPrefixed("att_", claims.AttemptID) || !identifierPattern.MatchString(claims.AgentID) || claims.AgentVersion == "" || !identifierPattern.MatchString(claims.SkillID) || !validPrefixed("dep_", claims.DeploymentID) || !identifierPattern.MatchString(claims.InstanceID) || claims.Generation == 0 || claims.Generation > 9007199254740991 || claims.FencingToken == 0 || claims.FencingToken > 9007199254740991 || claims.TransportProfile != "direct" && claims.TransportProfile != "proxy" || !claims.IssuedAt.Equal(claims.IssuedAt.UTC()) || !claims.ExpiresAt.Equal(claims.ExpiresAt.UTC()) || now.Before(claims.IssuedAt) || !now.Before(claims.ExpiresAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > 5*time.Minute || len(claims.Scopes) == 0 || len(claims.Scopes) > 16 {
		return errors.New("invalid claims")
	}
	seen := map[string]struct{}{}
	for _, value := range claims.Scopes {
		if !scopePattern.MatchString(value) {
			return errors.New("invalid scope")
		}
		if _, duplicate := seen[value]; duplicate {
			return errors.New("duplicate scope")
		}
		seen[value] = struct{}{}
		if value == scope {
			scope = ""
		}
	}
	if scope != "" {
		return errors.New("required scope missing")
	}
	return nil
}

func (runtime *Runtime) statusValue(record InboxRecord) runwire.RunStatus {
	request, _ := runwire.DecodeRunRequest(record.RequestJSON)
	value := runwire.RunStatus{
		SchemaVersion: 1, RunID: runwire.RunId(record.RunID), State: statusState(record.State), StateVersion: runwire.SafeInteger(record.StateVersion),
		Agent: request.Agent, AuthorizationSnapshotDigest: runwire.Sha256Digest(record.AuthorizationDigest),
		CreatedAt: runwire.DateTime(record.CreatedAt.Format(time.RFC3339Nano)), UpdatedAt: runwire.DateTime(record.UpdatedAt.Format(time.RFC3339Nano)), DeadlineAt: runwire.DateTime(record.DeadlineAt.Format(time.RFC3339Nano)),
		Trace: runwire.AROPV1W3CTraceContext{Traceparent: record.Traceparent},
	}
	if record.Tracestate != "" {
		value.Trace.Tracestate = &record.Tracestate
	}
	if record.CancelRequestedAt != nil {
		cancelled := runwire.DateTime(record.CancelRequestedAt.Format(time.RFC3339Nano))
		value.CancelRequestedAt = &cancelled
	}
	if len(record.ResultJSON) != 0 {
		result, err := runwire.DecodeRunResult(record.ResultJSON)
		if err == nil {
			value.Result = &result
		}
	}
	return value
}

func statusState(state InboxState) string {
	switch state {
	case InboxAccepted:
		return "dispatching"
	default:
		return string(state)
	}
}

func failedResult(runID string, now time.Time) runwire.RunResult {
	return terminalResult(runID, "failed", now, &runwire.AROPV1Error{Category: "internal", Code: "AGENT_EXECUTION_FAILED", Message: "agent execution failed", Retryable: false})
}

func cancelledResult(runID string, now time.Time) runwire.RunResult {
	return terminalResult(runID, "cancelled", now, &runwire.AROPV1Error{Category: "cancelled", Code: "RUN_CANCELLED", Message: "run was cancelled", Retryable: false})
}

func timedOutResult(runID string, now time.Time) runwire.RunResult {
	return terminalResult(runID, "timed_out", now, &runwire.AROPV1Error{Category: "timeout", Code: "RUN_TIMED_OUT", Message: "run deadline elapsed", Retryable: false})
}

func terminalResult(runID, state string, now time.Time, failure *runwire.AROPV1Error) runwire.RunResult {
	return runwire.RunResult{SchemaVersion: 1, RunID: runwire.RunId(runID), State: state, CompletedAt: runwire.DateTime(now.UTC().Format(time.RFC3339Nano)), Error: failure, Usage: runwire.Usage{InputTokens: 0, OutputTokens: 0, DurationMs: 0}}
}

func lifecycleEvent(record InboxRecord, eventType string, now time.Time) OutboxRecord {
	payload, _ := json.Marshal(map[string]any{"run_id": record.RunID, "attempt_id": record.AttemptID, "state": statusState(record.State), "state_version": record.StateVersion, "time": now.Format(time.RFC3339Nano)})
	sum := sha256.Sum256(append([]byte(eventType+":"), payload...))
	return OutboxRecord{RunID: record.RunID, AttemptID: record.AttemptID, EventID: "evt_" + hex.EncodeToString(sum[:16]), EventType: eventType, Envelope: payload, CreatedAt: now}
}

func claimsBindRequest(claims Claims, request runwire.RunRequest) bool {
	return claims.AgentID == string(request.Agent.ID) && claims.AgentVersion == string(request.Agent.Version) && claims.SkillID == string(request.Agent.SkillID)
}

func claimsDigest(claims Claims) string {
	value := struct {
		Issuer, Audience, Subject, AuthorizedParty, TokenID, RunID, AttemptID string
		AgentID, AgentVersion, SkillID, DeploymentID, InstanceID              string
		Generation, FencingToken                                              uint64
		Scopes                                                                []string
	}{claims.Issuer, claims.Audience, claims.Subject, claims.AuthorizedParty, claims.TokenID, claims.RunID, claims.AttemptID, claims.AgentID, claims.AgentVersion, claims.SkillID, claims.DeploymentID, claims.InstanceID, claims.Generation, claims.FencingToken, claims.Scopes}
	encoded, _ := json.Marshal(value)
	return sha256Digest(encoded)
}

func sha256Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validPrefixed(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && uuidPattern.MatchString(strings.TrimPrefix(value, prefix))
}

func readBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()
	reader := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("body exceeds limit")
	}
	return data, nil
}

func bearer(header http.Header) (string, bool) {
	value, ok := singleHeader(header, "Authorization")
	if !ok || !strings.HasPrefix(value, "Bearer ") || len(value) <= len("Bearer ") || strings.ContainsAny(value[len("Bearer "):], " \t\r\n") {
		return "", false
	}
	return strings.TrimPrefix(value, "Bearer "), true
}

func singleMediaType(header http.Header, name, expected string) bool {
	value, ok := singleHeader(header, name)
	return ok && strings.EqualFold(value, expected)
}

func singleHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = strings.TrimSpace(values[0])
	}
	return returnValue, len(values) == 1 && returnValue != "" && !strings.ContainsAny(returnValue, "\r\n")
}

func writeUnauthorized(response http.ResponseWriter) {
	response.Header().Set("WWW-Authenticate", `Bearer realm="arop-agent", error="invalid_token"`)
	writeProblem(response, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
}

func writeProblem(response http.ResponseWriter, status int, code string) {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		response.Header().Set("Retry-After", "1")
	}
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{"category": categoryFor(status), "code": code, "message": http.StatusText(status), "retryable": status == 429 || status == 503})
}

func categoryFor(status int) string {
	switch status {
	case 401:
		return "authentication"
	case 403:
		return "authorization"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "capacity"
	case 503:
		return "dependency"
	default:
		return "validation"
	}
}

var _ http.Handler = (*Runtime)(nil)
