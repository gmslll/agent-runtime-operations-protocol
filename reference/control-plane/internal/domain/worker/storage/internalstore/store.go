package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/event"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/worker"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/storage/migrate"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Store struct {
	db      *sql.DB
	lookup  durable.TransactionLookup
	dialect Dialect
}

func New(db *sql.DB, lookup durable.TransactionLookup, dialect Dialect) (*Store, error) {
	if db == nil || lookup == nil || dialect != SQLite && dialect != Postgres {
		return nil, errors.New("worker store dependencies are required")
	}
	return &Store{db: db, lookup: lookup, dialect: dialect}, nil
}

type registryView struct {
	WorkerID, SessionID, ServiceID, Endpoint string
	Generation, ResourceVersion              uint64
	LeaseExpiresAt                           time.Time
	AvailableSlots                           uint64
	Bindings                                 []worker.Binding
}

type attemptView struct {
	TenantID, RunID, AttemptID, TokenID, DeploymentID            string
	InstanceID, SessionID, ServiceID                             string
	Generation, ResourceVersion, AttemptNumber, FencingToken     uint64
	State, Endpoint, Audience, SigningKeyID                      string
	LeaseExpiresAt, TicketExpiresAt, CreatedAt                   time.Time
	Traceparent, Tracestate, IdempotencyKeyDigest, RequestDigest string
	Agent                                                        worker.Binding
	Input, Labels                                                json.RawMessage
	ConversationRef, EffectLevel, EffectID                       string
	DeadlineAt                                                   time.Time
}

func (store *Store) Claim(ctx context.Context, command worker.ClaimCommand) (worker.Claim, error) {
	if command.Request.Validate() != nil || !prefixed("clm_", command.ClaimID) || !prefixed("att_", command.ReplacementAttemptID) || !prefixed("tok_", command.TokenID) || worker.TokenDigest(command.LeaseToken) != command.LeaseTokenDigest || !utc(command.Now) || !utc(command.LeaseExpiresAt) || !command.LeaseExpiresAt.After(command.Now) {
		return worker.Claim{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return worker.Claim{}, err
	}
	if err = store.lock(ctx, tx, "worker/claim/"+command.Request.Caller.TenantID); err != nil {
		return worker.Claim{}, err
	}
	registry, err := store.worker(ctx, tx, command.Request, command.Now)
	if err != nil {
		return worker.Claim{}, err
	}
	var active uint64
	if err = tx.QueryRowContext(ctx, store.query(`SELECT count(*) FROM arop_worker_claims WHERE tenant_id=? AND worker_id=? AND worker_session_id=? AND worker_generation=? AND claim_state='active' AND lease_expires_at>?`), command.Request.Caller.TenantID, command.Request.WorkerID, command.Request.SessionID, command.Request.Generation, formatTime(command.Now)).Scan(&active); err != nil {
		return worker.Claim{}, store.dependency()
	}
	limit := command.Request.AvailableSlots
	if registry.AvailableSlots < limit {
		limit = registry.AvailableSlots
	}
	if active >= limit {
		return worker.Claim{}, worker.NewError(worker.CategoryCapacity, worker.ReasonNoWork)
	}
	attempts, err := store.claimable(ctx, tx, command.Request.Caller.TenantID, command.Request.WorkerID, command.Request.SessionID, command.Request.Generation, command.Now)
	if err != nil {
		return worker.Claim{}, err
	}
	var selected attemptView
	for _, candidate := range attempts {
		if bindingAllowed(candidate.Agent, command.Request.SupportedBindings) && bindingAllowed(candidate.Agent, registry.Bindings) {
			selected = candidate
			break
		}
	}
	if selected.AttemptID == "" {
		return worker.Claim{}, worker.NewError(worker.CategoryCapacity, worker.ReasonNoWork)
	}
	if err = store.lock(ctx, tx, "worker/run/"+selected.TenantID+"/"+selected.RunID, "worker/attempt/"+selected.TenantID+"/"+selected.AttemptID); err != nil {
		return worker.Claim{}, err
	}
	leaseExpiry := minimum(command.LeaseExpiresAt, selected.DeadlineAt, registry.LeaseExpiresAt)
	if !leaseExpiry.After(command.Now) {
		return worker.Claim{}, worker.NewError(worker.CategoryCapacity, worker.ReasonNoWork)
	}
	replace := selected.State == "accepted" || !selected.LeaseExpiresAt.After(command.Now) || selected.InstanceID != command.Request.WorkerID || selected.SessionID != command.Request.SessionID || selected.Generation != command.Request.Generation
	if replace {
		if err = store.expireAttempt(ctx, tx, selected, command.Now); err != nil {
			return worker.Claim{}, err
		}
		selected, err = store.replacement(ctx, tx, selected, registry, command.ReplacementAttemptID, command.TokenID, command.Now, leaseExpiry)
		if err != nil {
			return worker.Claim{}, err
		}
	} else {
		result, updateErr := tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='accepted',accepted_at=?,lease_expires_at=?,ticket_expires_at=? WHERE tenant_id=? AND attempt_id=? AND attempt_state='issued' AND fencing_token=?`), formatTime(command.Now), formatTime(leaseExpiry), formatTime(leaseExpiry), selected.TenantID, selected.AttemptID, selected.FencingToken)
		if updateErr != nil {
			return worker.Claim{}, store.classify(updateErr)
		}
		rows, _ := result.RowsAffected()
		if rows != 1 {
			return worker.Claim{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
		}
		selected.State = "accepted"
		selected.LeaseExpiresAt = leaseExpiry
		selected.TicketExpiresAt = leaseExpiry
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_worker_claims(tenant_id,claim_id,worker_id,worker_session_id,worker_generation,run_id,attempt_id,fencing_token,lease_token_digest,claim_state,lease_expires_at,claimed_at,renewed_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,'active',?,?,NULL,NULL)`), selected.TenantID, command.ClaimID, command.Request.WorkerID, command.Request.SessionID, command.Request.Generation, selected.RunID, selected.AttemptID, selected.FencingToken, command.LeaseTokenDigest, formatTime(leaseExpiry), formatTime(command.Now))
	if err != nil {
		return worker.Claim{}, store.classify(err)
	}
	if _, err = tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='running',state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state IN ('queued','dispatching') AND state_version<9007199254740991`), formatTime(command.Now), selected.TenantID, selected.RunID); err != nil {
		return worker.Claim{}, store.classify(err)
	}
	runRequest, err := selected.runRequest()
	if err != nil {
		return worker.Claim{}, store.dependency()
	}
	claim := worker.Claim{TenantID: selected.TenantID, ClaimID: command.ClaimID, WorkerID: command.Request.WorkerID, SessionID: command.Request.SessionID, Generation: command.Request.Generation, RunID: selected.RunID, AttemptID: selected.AttemptID, AttemptNumber: selected.AttemptNumber, FencingToken: selected.FencingToken, LeaseToken: command.LeaseToken, LeaseTokenDigest: command.LeaseTokenDigest, LeaseExpiresAt: leaseExpiry, ClaimedAt: command.Now, RunRequest: runRequest}
	if claim.Validate() != nil {
		return worker.Claim{}, store.dependency()
	}
	return claim, nil
}

func (store *Store) Renew(ctx context.Context, command worker.RenewCommand) (worker.Claim, error) {
	if command.Request.Validate() != nil || command.LeaseTokenDigest != worker.TokenDigest(command.Request.LeaseToken) || !utc(command.Now) || !utc(command.ExpiresAt) || !command.ExpiresAt.After(command.Now) {
		return worker.Claim{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return worker.Claim{}, err
	}
	if err = store.lock(ctx, tx, "worker/claim/"+command.Request.Caller.TenantID+"/"+command.Request.ClaimID); err != nil {
		return worker.Claim{}, err
	}
	claim, attempt, err := store.activeClaim(ctx, tx, command.Request.Caller.TenantID, command.Request.WorkerID, command.Request.ClaimID, command.LeaseTokenDigest, command.Request.FencingToken, command.Now)
	if err != nil {
		return worker.Claim{}, err
	}
	registry, err := store.currentWorker(ctx, tx, claim.TenantID, claim.WorkerID, claim.SessionID, claim.Generation, command.Now)
	if err != nil {
		return worker.Claim{}, err
	}
	expires := minimum(command.ExpiresAt, attempt.DeadlineAt, registry.LeaseExpiresAt)
	if !expires.After(command.Now) {
		return worker.Claim{}, worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	result, err := tx.ExecContext(ctx, store.query(`UPDATE arop_worker_claims SET lease_expires_at=?,renewed_at=? WHERE tenant_id=? AND claim_id=? AND claim_state='active' AND lease_expires_at>?`), formatTime(expires), formatTime(command.Now), claim.TenantID, claim.ClaimID, formatTime(command.Now))
	if err != nil {
		return worker.Claim{}, store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return worker.Claim{}, worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	result, err = tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET lease_expires_at=?,ticket_expires_at=? WHERE tenant_id=? AND attempt_id=? AND attempt_state='accepted' AND fencing_token=?`), formatTime(expires), formatTime(expires), claim.TenantID, claim.AttemptID, claim.FencingToken)
	if err != nil {
		return worker.Claim{}, store.classify(err)
	}
	rows, _ = result.RowsAffected()
	if rows != 1 {
		return worker.Claim{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	claim.LeaseExpiresAt = expires
	claim.LeaseToken = command.Request.LeaseToken
	return claim, nil
}

func (store *Store) Complete(ctx context.Context, command worker.CompleteCommand) (bool, error) {
	if command.Request.Validate() != nil || command.LeaseTokenDigest != worker.TokenDigest(command.Request.LeaseToken) || !validHex(command.KeyDigest) || !validDigest(command.RequestDigest) || !prefixed("evt_", command.EventID) || !prefixed("out_", command.OutboxID) || !utc(command.Now) {
		return false, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return false, err
	}
	if err = store.lock(ctx, tx, "worker/complete/"+command.Request.Caller.TenantID+"/"+command.KeyDigest, "worker/claim/"+command.Request.Caller.TenantID+"/"+command.Request.ClaimID); err != nil {
		return false, err
	}
	var storedDigest, claimID, attemptID, workerID, leaseTokenDigest string
	var fencingToken uint64
	err = tx.QueryRowContext(ctx, store.query(`SELECT completion.request_digest,completion.claim_id,completion.attempt_id,claim.worker_id,claim.lease_token_digest,claim.fencing_token FROM arop_worker_completions completion JOIN arop_worker_claims claim ON claim.tenant_id=completion.tenant_id AND claim.claim_id=completion.claim_id WHERE completion.tenant_id=? AND completion.idempotency_key_digest=?`), command.Request.Caller.TenantID, command.KeyDigest).Scan(&storedDigest, &claimID, &attemptID, &workerID, &leaseTokenDigest, &fencingToken)
	if err == nil {
		if storedDigest != command.RequestDigest || claimID != command.Request.ClaimID || attemptID != command.Request.AttemptID || workerID != command.Request.WorkerID || leaseTokenDigest != command.LeaseTokenDigest || fencingToken != command.Request.FencingToken {
			return false, worker.NewError(worker.CategoryConflict, worker.ReasonIdempotencyConflict)
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, store.dependency()
	}
	claim, attempt, err := store.activeClaim(ctx, tx, command.Request.Caller.TenantID, command.Request.WorkerID, command.Request.ClaimID, command.LeaseTokenDigest, command.Request.FencingToken, command.Now)
	if err != nil {
		return false, err
	}
	if claim.AttemptID != command.Request.AttemptID || attempt.AttemptID != command.Request.AttemptID {
		return false, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	if _, err = store.currentWorker(ctx, tx, claim.TenantID, claim.WorkerID, claim.SessionID, claim.Generation, command.Now); err != nil {
		return false, err
	}
	resultDigest, terminal, envelope, err := store.terminal(command, attempt)
	if err != nil {
		return false, err
	}
	if err = store.applyEffects(ctx, tx, attempt, command.Request.EffectIDs, resultDigest, command.Now); err != nil {
		return false, err
	}
	if err = store.appendTerminal(ctx, tx, attempt, envelope, terminal, command.Now); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, store.query(`UPDATE arop_worker_claims SET claim_state='completed',closed_at=? WHERE tenant_id=? AND claim_id=? AND claim_state='active'`), formatTime(command.Now), claim.TenantID, claim.ClaimID)
	if err != nil {
		return false, store.classify(err)
	}
	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return false, store.dependency()
	}
	if rows != 1 {
		return false, worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	_, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_worker_completions(tenant_id,completion_id,claim_id,attempt_id,idempotency_key_digest,request_digest,result_digest,terminal_event_id,completed_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`), claim.TenantID, command.Request.CompletionID, claim.ClaimID, claim.AttemptID, command.KeyDigest, command.RequestDigest, resultDigest, command.EventID, formatTime(command.Request.CompletedAt), formatTime(command.Now))
	if err != nil {
		return false, store.classify(err)
	}
	payload, _ := json.Marshal(map[string]any{"claim_id": claim.ClaimID, "attempt_id": claim.AttemptID, "completion_id": command.Request.CompletionID, "event_id": command.EventID, "result_digest": resultDigest})
	if _, err = tx.ExecContext(ctx, store.query(`INSERT INTO arop_worker_outbox(tenant_id,outbox_id,claim_id,attempt_id,event_kind,payload_json,created_at,published_at) VALUES(?,?,?,?,'worker.completed',?,?,NULL)`), claim.TenantID, command.OutboxID, claim.ClaimID, claim.AttemptID, string(payload), formatTime(command.Now)); err != nil {
		return false, store.classify(err)
	}
	return false, nil
}

func (store *Store) Release(ctx context.Context, command worker.ReleaseCommand) error {
	if command.Request.Validate() != nil || command.LeaseTokenDigest != worker.TokenDigest(command.Request.LeaseToken) || !prefixed("att_", command.ReplacementAttemptID) || !prefixed("tok_", command.TokenID) || !utc(command.Now) {
		return worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	tx, err := store.writeTx(ctx)
	if err != nil {
		return err
	}
	if err = store.lock(ctx, tx, "worker/claim/"+command.Request.Caller.TenantID+"/"+command.Request.ClaimID); err != nil {
		return err
	}
	claim, attempt, err := store.activeClaim(ctx, tx, command.Request.Caller.TenantID, command.Request.WorkerID, command.Request.ClaimID, command.LeaseTokenDigest, command.Request.FencingToken, command.Now)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='fenced',closed_at=?,failure_code='WORKER_RELEASED' WHERE tenant_id=? AND attempt_id=? AND attempt_state='accepted' AND fencing_token=?`), formatTime(command.Now), attempt.TenantID, attempt.AttemptID, attempt.FencingToken)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	result, err = tx.ExecContext(ctx, store.query(`UPDATE arop_worker_claims SET claim_state='released',closed_at=? WHERE tenant_id=? AND claim_id=? AND claim_state='active'`), formatTime(command.Now), claim.TenantID, claim.ClaimID)
	if err != nil {
		return store.classify(err)
	}
	rows, _ = result.RowsAffected()
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	registry, err := store.currentWorker(ctx, tx, claim.TenantID, claim.WorkerID, claim.SessionID, claim.Generation, command.Now)
	if err != nil {
		return err
	}
	expires := minimum(command.Now.Add(time.Minute), attempt.DeadlineAt, registry.LeaseExpiresAt)
	if !expires.After(command.Now) {
		return worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	if _, err = store.replacement(ctx, tx, attempt, registry, command.ReplacementAttemptID, command.TokenID, command.Now, expires); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='issued',accepted_at=NULL WHERE tenant_id=? AND attempt_id=? AND attempt_state='accepted'`), claim.TenantID, command.ReplacementAttemptID); err != nil {
		return store.classify(err)
	}
	if _, err = tx.ExecContext(ctx, store.query(`UPDATE arop_runs SET state='dispatching',state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state IN ('running','dispatching') AND state_version<9007199254740991`), formatTime(command.Now), claim.TenantID, claim.RunID); err != nil {
		return store.classify(err)
	}
	return nil
}

func (store *Store) Check(ctx context.Context) error {
	var value int
	if err := store.db.QueryRowContext(ctx, `SELECT 1`).Scan(&value); err != nil || value != 1 {
		return errors.New("worker repository unavailable")
	}
	return nil
}

func (store *Store) worker(ctx context.Context, q migrate.Queryer, request worker.ClaimRequest, now time.Time) (registryView, error) {
	view, err := store.currentWorker(ctx, q, request.Caller.TenantID, request.WorkerID, request.SessionID, request.Generation, now)
	if err != nil {
		return registryView{}, err
	}
	if request.AvailableSlots > view.AvailableSlots {
		return registryView{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	return view, nil
}

func (store *Store) currentWorker(ctx context.Context, q migrate.Queryer, tenantID, workerID, sessionID string, generation uint64, now time.Time) (registryView, error) {
	var view registryView
	var leaseText, runtimeJSON, bindingsJSON, status string
	var draining bool
	err := q.QueryRowContext(ctx, store.query(`SELECT session_id,service_id,generation,resource_version,lease_expires_at,endpoint_base_url,bindings_json,runtime_json,draining,status FROM arop_registry_instances WHERE tenant_id=? AND instance_id=?`), tenantID, workerID).Scan(&view.SessionID, &view.ServiceID, &view.Generation, &view.ResourceVersion, &leaseText, &view.Endpoint, &bindingsJSON, &runtimeJSON, &draining, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return registryView{}, worker.NewError(worker.CategoryAuthentication, worker.ReasonAuthentication)
	}
	if err != nil {
		return registryView{}, store.dependency()
	}
	view.WorkerID = workerID
	view.LeaseExpiresAt, err = parseTime(leaseText)
	var runtime struct {
		Healthy, Ready    bool
		TransportProfiles []string `json:"transport_profiles"`
		Capacity          struct {
			AvailableSlots uint64 `json:"available_slots"`
		} `json:"capacity"`
	}
	var bindings []struct {
		AgentID        string   `json:"agent_id"`
		AgentVersion   string   `json:"agent_version"`
		ManifestDigest string   `json:"manifest_digest"`
		SkillIDs       []string `json:"skill_ids"`
	}
	if err != nil || json.Unmarshal([]byte(runtimeJSON), &runtime) != nil || json.Unmarshal([]byte(bindingsJSON), &bindings) != nil || view.SessionID != sessionID || view.Generation != generation || status != "registered" || draining || !runtime.Healthy || !runtime.Ready || !slices.Contains(runtime.TransportProfiles, "worker_pull") || runtime.Capacity.AvailableSlots == 0 || !view.LeaseExpiresAt.After(now) {
		return registryView{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	view.AvailableSlots = runtime.Capacity.AvailableSlots
	for _, binding := range bindings {
		for _, skill := range binding.SkillIDs {
			view.Bindings = append(view.Bindings, worker.Binding{AgentID: binding.AgentID, Version: binding.AgentVersion, SkillID: skill, ManifestDigest: binding.ManifestDigest})
		}
	}
	return view, nil
}

func (store *Store) claimable(ctx context.Context, q migrate.Queryer, tenantID, workerID, sessionID string, generation uint64, now time.Time) ([]attemptView, error) {
	rows, err := q.QueryContext(ctx, store.query(`SELECT a.tenant_id,a.run_id,a.attempt_id,a.token_id,a.deployment_id,a.instance_id,a.session_id,a.service_id,a.generation,a.registry_resource_version,a.attempt_number,a.fencing_token,a.attempt_state,a.endpoint,a.audience,a.signing_key_id,a.lease_expires_at,a.ticket_expires_at,a.created_at,a.traceparent,coalesce(a.tracestate,''),a.idempotency_key_digest,a.idempotency_request_digest,r.agent_id,r.agent_version,r.skill_id,r.manifest_digest,r.input_json,r.labels_json,coalesce(r.conversation_ref,''),r.effect_level,coalesce(r.effect_id,''),r.deadline_at FROM arop_dispatch_attempts a JOIN arop_runs r ON r.tenant_id=a.tenant_id AND r.run_id=a.run_id WHERE a.tenant_id=? AND a.transport_profile='worker_pull' AND r.state IN ('dispatching','running') AND r.deadline_at>? AND ((a.attempt_state='issued' AND NOT EXISTS (SELECT 1 FROM arop_worker_claims c WHERE c.tenant_id=a.tenant_id AND c.attempt_id=a.attempt_id AND c.claim_state='active')) OR (a.attempt_state='accepted' AND EXISTS (SELECT 1 FROM arop_worker_claims c WHERE c.tenant_id=a.tenant_id AND c.attempt_id=a.attempt_id AND c.claim_state='active' AND (c.lease_expires_at<=? OR c.worker_id<>? OR c.worker_session_id<>? OR c.worker_generation<>?)))) ORDER BY a.created_at,a.attempt_id`), tenantID, formatTime(now), formatTime(now), workerID, sessionID, generation)
	if err != nil {
		return nil, store.dependency()
	}
	defer rows.Close()
	var result []attemptView
	for rows.Next() {
		var item attemptView
		var lease, ticket, created, deadline, input, labels string
		if rows.Scan(&item.TenantID, &item.RunID, &item.AttemptID, &item.TokenID, &item.DeploymentID, &item.InstanceID, &item.SessionID, &item.ServiceID, &item.Generation, &item.ResourceVersion, &item.AttemptNumber, &item.FencingToken, &item.State, &item.Endpoint, &item.Audience, &item.SigningKeyID, &lease, &ticket, &created, &item.Traceparent, &item.Tracestate, &item.IdempotencyKeyDigest, &item.RequestDigest, &item.Agent.AgentID, &item.Agent.Version, &item.Agent.SkillID, &item.Agent.ManifestDigest, &input, &labels, &item.ConversationRef, &item.EffectLevel, &item.EffectID, &deadline) != nil {
			return nil, store.dependency()
		}
		item.LeaseExpiresAt, err = parseTime(lease)
		if err == nil {
			item.TicketExpiresAt, err = parseTime(ticket)
		}
		if err == nil {
			item.CreatedAt, err = parseTime(created)
		}
		if err == nil {
			item.DeadlineAt, err = parseTime(deadline)
		}
		item.Input, item.Labels = json.RawMessage(input), json.RawMessage(labels)
		if err != nil || item.Agent.Validate() != nil || !json.Valid(item.Input) || !json.Valid(item.Labels) {
			return nil, store.dependency()
		}
		result = append(result, item)
	}
	if rows.Err() != nil {
		return nil, store.dependency()
	}
	return result, nil
}

func (store *Store) activeClaim(ctx context.Context, q migrate.Queryer, tenantID, workerID, claimID, tokenDigest string, fence uint64, now time.Time) (worker.Claim, attemptView, error) {
	var claim worker.Claim
	var expires, claimed string
	err := q.QueryRowContext(ctx, store.query(`SELECT tenant_id,claim_id,worker_id,worker_session_id,worker_generation,run_id,attempt_id,fencing_token,lease_token_digest,lease_expires_at,claimed_at FROM arop_worker_claims WHERE tenant_id=? AND claim_id=? AND claim_state='active'`), tenantID, claimID).Scan(&claim.TenantID, &claim.ClaimID, &claim.WorkerID, &claim.SessionID, &claim.Generation, &claim.RunID, &claim.AttemptID, &claim.FencingToken, &claim.LeaseTokenDigest, &expires, &claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return worker.Claim{}, attemptView{}, worker.NewError(worker.CategoryNotFound, worker.ReasonClaimNotFound)
	}
	if err != nil {
		return worker.Claim{}, attemptView{}, store.dependency()
	}
	claim.LeaseExpiresAt, err = parseTime(expires)
	if err == nil {
		claim.ClaimedAt, err = parseTime(claimed)
	}
	if err != nil || claim.WorkerID != workerID || claim.LeaseTokenDigest != tokenDigest || claim.FencingToken != fence || !now.Before(claim.LeaseExpiresAt) {
		return worker.Claim{}, attemptView{}, worker.NewError(worker.CategoryConflict, worker.ReasonClaimExpired)
	}
	attempt, err := store.attempt(ctx, q, tenantID, claim.AttemptID)
	if err != nil {
		return worker.Claim{}, attemptView{}, err
	}
	if attempt.State != "accepted" || attempt.RunID != claim.RunID || attempt.FencingToken != claim.FencingToken || !now.Before(attempt.LeaseExpiresAt) {
		return worker.Claim{}, attemptView{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	claim.AttemptNumber = attempt.AttemptNumber
	claim.RunRequest, err = attempt.runRequest()
	return claim, attempt, err
}

func (store *Store) attempt(ctx context.Context, q migrate.Queryer, tenantID, attemptID string) (attemptView, error) {
	rows, err := store.claimableByID(ctx, q, tenantID, attemptID)
	if err != nil || len(rows) != 1 {
		if err != nil {
			return attemptView{}, err
		}
		return attemptView{}, worker.NewError(worker.CategoryNotFound, worker.ReasonClaimNotFound)
	}
	return rows[0], nil
}

func (store *Store) claimableByID(ctx context.Context, q migrate.Queryer, tenantID, attemptID string) ([]attemptView, error) {
	// Use the same strict hydration as claimable without applying queue-state predicates.
	rows, err := q.QueryContext(ctx, store.query(`SELECT a.tenant_id,a.run_id,a.attempt_id,a.token_id,a.deployment_id,a.instance_id,a.session_id,a.service_id,a.generation,a.registry_resource_version,a.attempt_number,a.fencing_token,a.attempt_state,a.endpoint,a.audience,a.signing_key_id,a.lease_expires_at,a.ticket_expires_at,a.created_at,a.traceparent,coalesce(a.tracestate,''),a.idempotency_key_digest,a.idempotency_request_digest,r.agent_id,r.agent_version,r.skill_id,r.manifest_digest,r.input_json,r.labels_json,coalesce(r.conversation_ref,''),r.effect_level,coalesce(r.effect_id,''),r.deadline_at FROM arop_dispatch_attempts a JOIN arop_runs r ON r.tenant_id=a.tenant_id AND r.run_id=a.run_id WHERE a.tenant_id=? AND a.attempt_id=?`), tenantID, attemptID)
	if err != nil {
		return nil, store.dependency()
	}
	defer rows.Close()
	var result []attemptView
	for rows.Next() {
		var item attemptView
		var lease, ticket, created, deadline, input, labels string
		if rows.Scan(&item.TenantID, &item.RunID, &item.AttemptID, &item.TokenID, &item.DeploymentID, &item.InstanceID, &item.SessionID, &item.ServiceID, &item.Generation, &item.ResourceVersion, &item.AttemptNumber, &item.FencingToken, &item.State, &item.Endpoint, &item.Audience, &item.SigningKeyID, &lease, &ticket, &created, &item.Traceparent, &item.Tracestate, &item.IdempotencyKeyDigest, &item.RequestDigest, &item.Agent.AgentID, &item.Agent.Version, &item.Agent.SkillID, &item.Agent.ManifestDigest, &input, &labels, &item.ConversationRef, &item.EffectLevel, &item.EffectID, &deadline) != nil {
			return nil, store.dependency()
		}
		item.LeaseExpiresAt, err = parseTime(lease)
		if err == nil {
			item.TicketExpiresAt, err = parseTime(ticket)
		}
		if err == nil {
			item.CreatedAt, err = parseTime(created)
		}
		if err == nil {
			item.DeadlineAt, err = parseTime(deadline)
		}
		item.Input, item.Labels = json.RawMessage(input), json.RawMessage(labels)
		if err != nil {
			return nil, store.dependency()
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (attempt attemptView) runRequest() (json.RawMessage, error) {
	effects := map[string]any{"level": attempt.EffectLevel}
	if attempt.EffectID != "" {
		effects["effect_id"] = attempt.EffectID
	}
	trace := map[string]any{"traceparent": attempt.Traceparent}
	if attempt.Tracestate != "" {
		trace["tracestate"] = attempt.Tracestate
	}
	value := map[string]any{"schema_version": 1, "agent": map[string]any{"id": attempt.Agent.AgentID, "version": attempt.Agent.Version, "skill_id": attempt.Agent.SkillID, "manifest_digest": attempt.Agent.ManifestDigest}, "input": attempt.Input, "deadline_at": attempt.DeadlineAt.Format(time.RFC3339Nano), "effects": effects, "trace": trace, "labels": attempt.Labels}
	if attempt.ConversationRef != "" {
		value["conversation_ref"] = attempt.ConversationRef
	}
	encoded, err := json.Marshal(value)
	return json.RawMessage(encoded), err
}

func (store *Store) expireAttempt(ctx context.Context, q migrate.Queryer, attempt attemptView, now time.Time) error {
	if _, err := q.ExecContext(ctx, store.query(`UPDATE arop_worker_claims SET claim_state='expired',closed_at=? WHERE tenant_id=? AND attempt_id=? AND claim_state='active'`), formatTime(now), attempt.TenantID, attempt.AttemptID); err != nil {
		return store.classify(err)
	}
	result, err := q.ExecContext(ctx, store.query(`UPDATE arop_dispatch_attempts SET attempt_state='fenced',closed_at=?,failure_code='WORKER_LEASE_EXPIRED' WHERE tenant_id=? AND attempt_id=? AND attempt_state IN ('issued','accepted') AND fencing_token=?`), formatTime(now), attempt.TenantID, attempt.AttemptID, attempt.FencingToken)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	return nil
}

func (store *Store) replacement(ctx context.Context, q migrate.Queryer, previous attemptView, registry registryView, attemptID, tokenID string, now, expires time.Time) (attemptView, error) {
	number := previous.AttemptNumber + 1
	fence := previous.FencingToken + 1
	if number > worker.MaxSafeInteger || fence > worker.MaxSafeInteger {
		return attemptView{}, worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	var deploymentID string
	if err := q.QueryRowContext(ctx, store.query(`SELECT deployment_id FROM arop_dispatch_deployments WHERE tenant_id=? AND service_id=?`), previous.TenantID, registry.ServiceID).Scan(&deploymentID); err != nil {
		return attemptView{}, store.dependency()
	}
	endpoint := previous.Endpoint
	if strings.HasPrefix(registry.Endpoint, "https://") {
		endpoint = strings.TrimSuffix(registry.Endpoint, "/") + "/v1/runs"
	}
	audience := previous.Audience
	if deploymentID != previous.DeploymentID {
		parsed := strings.Split(previous.Audience, "/deployments/")
		if len(parsed) == 2 {
			audience = parsed[0] + "/deployments/" + deploymentID
		}
	}
	_, err := q.ExecContext(ctx, store.query(`INSERT INTO arop_dispatch_attempts(tenant_id,run_id,attempt_id,attempt_number,fencing_token,token_id,deployment_id,instance_id,session_id,service_id,generation,registry_resource_version,attempt_state,transport_profile,endpoint,audience,signing_key_id,lease_expires_at,ticket_expires_at,created_at,accepted_at,closed_at,failure_code,traceparent,tracestate,idempotency_key_digest,idempotency_request_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'accepted','worker_pull',?,?,?,?,?,?,?,NULL,NULL,?,?,?,?)`), previous.TenantID, previous.RunID, attemptID, number, fence, tokenID, deploymentID, registry.WorkerID, registry.SessionID, registry.ServiceID, registry.Generation, registry.ResourceVersion, endpoint, audience, previous.SigningKeyID, formatTime(expires), formatTime(expires), formatTime(now), formatTime(now), previous.Traceparent, nullable(previous.Tracestate), previous.IdempotencyKeyDigest, previous.RequestDigest)
	if err != nil {
		return attemptView{}, store.classify(err)
	}
	previous.AttemptID, previous.TokenID, previous.DeploymentID = attemptID, tokenID, deploymentID
	previous.AttemptNumber, previous.FencingToken = number, fence
	previous.InstanceID, previous.SessionID, previous.ServiceID, previous.Generation, previous.ResourceVersion = registry.WorkerID, registry.SessionID, registry.ServiceID, registry.Generation, registry.ResourceVersion
	previous.State, previous.Endpoint, previous.Audience = "accepted", endpoint, audience
	previous.LeaseExpiresAt, previous.TicketExpiresAt, previous.CreatedAt = expires, expires, now
	return previous, nil
}

func (store *Store) terminal(command worker.CompleteCommand, attempt attemptView) (string, event.TerminalProjection, event.Envelope, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(command.Request.Result, &raw) != nil {
		return "", event.TerminalProjection{}, event.Envelope{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	delete(raw, "schema_version")
	delete(raw, "run_id")
	data, err := json.Marshal(raw)
	if err != nil {
		return "", event.TerminalProjection{}, event.Envelope{}, store.dependency()
	}
	var state string
	if json.Unmarshal(raw["state"], &state) != nil {
		return "", event.TerminalProjection{}, event.Envelope{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	envelope := event.Envelope{SpecVersion: "1.0", ID: command.EventID, Source: "https://control-plane.invalid/workers/" + command.Request.WorkerID, Type: "io.arop.run." + state + ".v1", Subject: "runs/" + attempt.RunID, Time: command.Request.CompletedAt, DataContentType: "application/json", DataSchema: "https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json", RunID: attempt.RunID, AttemptID: attempt.AttemptID, ProducerSequence: 1, Traceparent: attempt.Traceparent, Data: data}
	terminal, ok, err := event.Terminal(envelope)
	if err != nil || !ok {
		return "", event.TerminalProjection{}, event.Envelope{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	digest, err := worker.RequestDigest(json.RawMessage(command.Request.Result))
	if err != nil {
		return "", event.TerminalProjection{}, event.Envelope{}, worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	return digest, terminal, envelope, nil
}

func (store *Store) applyEffects(ctx context.Context, q migrate.Queryer, attempt attemptView, effectIDs []string, digest string, now time.Time) error {
	for _, id := range effectIDs {
		var runID, stored string
		err := q.QueryRowContext(ctx, store.query(`SELECT run_id,semantic_digest FROM arop_run_effects WHERE tenant_id=? AND effect_id=?`), attempt.TenantID, id).Scan(&runID, &stored)
		if err == nil {
			if runID != attempt.RunID || stored != digest {
				return worker.NewError(worker.CategoryConflict, worker.ReasonIdempotencyConflict)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.dependency()
		}
		if _, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_run_effects(tenant_id,run_id,effect_id,semantic_digest,created_at) VALUES(?,?,?,?,?)`), attempt.TenantID, attempt.RunID, id, digest, formatTime(now)); err != nil {
			return store.classify(err)
		}
	}
	return nil
}

func (store *Store) appendTerminal(ctx context.Context, q migrate.Queryer, attempt attemptView, envelope event.Envelope, terminal event.TerminalProjection, now time.Time) error {
	var producer, runSequence uint64
	err := q.QueryRowContext(ctx, store.query(`SELECT last_producer_sequence FROM arop_event_attempt_projections WHERE tenant_id=? AND attempt_id=?`), attempt.TenantID, attempt.AttemptID).Scan(&producer)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := q.ExecContext(ctx, store.query(`INSERT INTO arop_event_attempt_projections(tenant_id,run_id,attempt_id,last_producer_sequence,updated_at) VALUES(?,?,?,?,?)`), attempt.TenantID, attempt.RunID, attempt.AttemptID, 0, formatTime(now)); err != nil {
			return store.classify(err)
		}
		producer = 0
	} else if err != nil {
		return store.dependency()
	}
	var terminalState sql.NullString
	err = q.QueryRowContext(ctx, store.query(`SELECT last_run_sequence,terminal_state FROM arop_event_run_projections WHERE tenant_id=? AND run_id=?`), attempt.TenantID, attempt.RunID).Scan(&runSequence, &terminalState)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_run_projections(tenant_id,run_id,last_run_sequence,terminal_state,terminal_result_json,terminal_event_id,updated_at) VALUES(?,?,0,NULL,NULL,NULL,?)`), attempt.TenantID, attempt.RunID, formatTime(now)); err != nil {
			return store.classify(err)
		}
		runSequence = 0
	} else if err != nil {
		return store.dependency()
	}
	if terminalState.Valid || producer >= worker.MaxSafeInteger || runSequence >= worker.MaxSafeInteger {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	producer++
	runSequence++
	envelope.ProducerSequence = producer
	encoded, digest, err := envelope.Canonical()
	if err != nil {
		return worker.NewError(worker.CategoryValidation, worker.ReasonInvalidRequest)
	}
	result, err := q.ExecContext(ctx, store.query(`UPDATE arop_runs SET state=?,usage_input_tokens=?,usage_output_tokens=?,usage_duration_ms=?,usage_billable_units=?,state_version=state_version+1,updated_at=? WHERE tenant_id=? AND run_id=? AND state NOT IN ('succeeded','failed','cancelled','timed_out') AND state_version<9007199254740991`), string(terminal.State), terminal.Usage.InputTokens, terminal.Usage.OutputTokens, terminal.Usage.DurationMS, terminal.Usage.BillableUnits, formatTime(now), attempt.TenantID, attempt.RunID)
	if err != nil {
		return store.classify(err)
	}
	rows, _ := result.RowsAffected()
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	if _, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_ledger(tenant_id,run_id,run_sequence,attempt_id,producer_sequence,event_id,source,event_type,event_digest,envelope_json,occurred_at,received_at,projection_applied) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`), attempt.TenantID, attempt.RunID, runSequence, attempt.AttemptID, producer, envelope.ID, envelope.Source, envelope.Type, digest, string(encoded), formatTime(envelope.Time), formatTime(now), true); err != nil {
		return store.classify(err)
	}
	result, err = q.ExecContext(ctx, store.query(`UPDATE arop_event_attempt_projections SET last_producer_sequence=?,updated_at=? WHERE tenant_id=? AND attempt_id=?`), producer, formatTime(now), attempt.TenantID, attempt.AttemptID)
	if err != nil {
		return store.classify(err)
	}
	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return store.dependency()
	}
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	result, err = q.ExecContext(ctx, store.query(`UPDATE arop_event_run_projections SET last_run_sequence=?,terminal_state=?,terminal_result_json=?,terminal_event_id=?,updated_at=? WHERE tenant_id=? AND run_id=? AND terminal_state IS NULL`), runSequence, string(terminal.State), string(terminal.Result), envelope.ID, formatTime(now), attempt.TenantID, attempt.RunID)
	if err != nil {
		return store.classify(err)
	}
	rows, rowsErr = result.RowsAffected()
	if rowsErr != nil {
		return store.dependency()
	}
	if rows != 1 {
		return worker.NewError(worker.CategoryConflict, worker.ReasonAttemptFenced)
	}
	if _, err = q.ExecContext(ctx, store.query(`INSERT INTO arop_event_capacity_releases(tenant_id,attempt_id,run_id,run_sequence,terminal_event_id,released_at) VALUES(?,?,?,?,?,?)`), attempt.TenantID, attempt.AttemptID, attempt.RunID, runSequence, envelope.ID, formatTime(now)); err != nil {
		return store.classify(err)
	}
	return nil
}

func bindingAllowed(value worker.Binding, allowed []worker.Binding) bool {
	return slices.Contains(allowed, value)
}
func (store *Store) writeTx(ctx context.Context) (*sql.Tx, error) {
	tx, ok := store.lookup(ctx)
	if !ok || tx == nil {
		return nil, store.dependency()
	}
	return tx, nil
}
func (store *Store) lock(ctx context.Context, tx *sql.Tx, keys ...string) error {
	if store.dialect == SQLite {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return store.dependency()
		}
	}
	return nil
}
func (store *Store) query(value string) string {
	if store.dialect == SQLite {
		return value
	}
	var result strings.Builder
	index := 1
	for _, character := range value {
		if character == '?' {
			result.WriteString(fmt.Sprintf("$%d", index))
			index++
		} else {
			result.WriteRune(character)
		}
	}
	return result.String()
}
func (store *Store) classify(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "unique") || strings.Contains(text, "constraint") || strings.Contains(text, "duplicate") {
		return worker.NewError(worker.CategoryConflict, worker.ReasonIdempotencyConflict)
	}
	return store.dependency()
}
func (store *Store) dependency() error {
	return worker.NewError(worker.CategoryDependency, worker.ReasonDependencyUnavailable)
}
func formatTime(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func parseTime(value string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000000000Z", value)
}
func minimum(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}
func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }

var hexDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validHex(value string) bool { return hexDigestPattern.MatchString(value) }
func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"))
}
func prefixed(prefix, value string) bool {
	return strings.HasPrefix(value, prefix) && len(value) == len(prefix)+36
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
