package internalstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/adapters/observability/durable"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

type Repository struct {
	db      *sql.DB
	lookup  durable.TransactionLookup
	dialect Dialect
}

func New(db *sql.DB, lookup durable.TransactionLookup, dialect Dialect) (*Repository, error) {
	if db == nil || lookup == nil || dialect != SQLite && dialect != Postgres {
		return nil, errors.New("registry repository dependencies are required")
	}
	return &Repository{db: db, lookup: lookup, dialect: dialect}, nil
}

func (repository *Repository) Register(ctx context.Context, command registry.RegisterCommand) (registry.Registration, error) {
	if err := command.Request.Validate(); err != nil || !utc(command.Now) || command.TTL <= 0 || command.Keepalive <= 0 || command.Keepalive >= command.TTL {
		return registry.Registration{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return registry.Registration{}, err
	}
	if err := repository.lockKeys(ctx, tx, "registry/idempotency/"+command.Request.TenantID+"/"+command.Request.IdempotencyKeyDigest, "registry/instance/"+command.Request.TenantID+"/"+command.Request.InstanceID); err != nil {
		return registry.Registration{}, err
	}
	storedRequest, storedResult, found, err := repository.loadIdempotency(ctx, tx, command.Request.TenantID, command.Request.IdempotencyKeyDigest)
	if err != nil {
		return registry.Registration{}, err
	}
	if found {
		if storedRequest != command.Request.IdempotencyRequestDigest {
			return registry.Registration{}, registry.NewError(registry.ReasonIdempotencyConflict)
		}
		var result registry.Registration
		if json.Unmarshal(storedResult, &result) != nil || result.Validate() != nil {
			return registry.Registration{}, registry.NewError(registry.ReasonDependencyUnavailable)
		}
		result.Replay = true
		return result, nil
	}

	current, exists, err := repository.loadInstance(ctx, tx, command.Request.TenantID, command.Request.InstanceID, true)
	if err != nil {
		return registry.Registration{}, err
	}
	generation, resourceVersion := uint64(1), uint64(1)
	operator := registry.OperatorState{Enabled: true, Weight: 100}
	draining := false
	var deadline *time.Time
	createdAt := command.Now
	if exists {
		if current.SessionID == command.Request.SessionID {
			return registry.Registration{}, registry.NewError(registry.ReasonSessionReused)
		}
		if current.Generation >= registry.MaxSafeInteger {
			return registry.Registration{}, registry.NewError(registry.ReasonGenerationOverflow)
		}
		generation = current.Generation + 1
		if current.ResourceVersion >= registry.MaxSafeInteger {
			return registry.Registration{}, registry.NewError(registry.ReasonRevisionOverflow)
		}
		resourceVersion = current.ResourceVersion + 1
		operator, createdAt = current.Operator, current.CreatedAt
		if current.Status == registry.StatusRegistered {
			draining, deadline = current.Draining, current.DrainDeadlineAt
		}
	}
	used, err := repository.sessionUsed(ctx, tx, command.Request.TenantID, command.Request.InstanceID, command.Request.SessionID)
	if err != nil {
		return registry.Registration{}, err
	}
	if used {
		return registry.Registration{}, registry.NewError(registry.ReasonSessionReused)
	}
	revision, err := repository.allocateRevision(ctx, tx)
	if err != nil {
		return registry.Registration{}, err
	}
	instance := registry.Instance{
		TenantID: command.Request.TenantID, InstanceID: command.Request.InstanceID, SessionID: command.Request.SessionID, ServiceID: command.Request.ServiceID,
		Environment: command.Request.Environment, Generation: generation, ResourceVersion: resourceVersion, RegistryRevision: revision,
		LeaseID: command.LeaseID, LeaseExpiresAt: command.Now.Add(command.TTL), Endpoint: command.Request.Endpoint,
		Bindings: cloneBindings(command.Request.Bindings), Runtime: cloneRuntime(command.Request.Runtime), Operator: operator,
		Draining: draining, DrainDeadlineAt: cloneTime(deadline), Status: registry.StatusRegistered, CreatedAt: createdAt, UpdatedAt: command.Now,
	}
	if err := instance.Validate(); err != nil {
		return registry.Registration{}, err
	}
	if err := repository.saveInstance(ctx, tx, instance, exists); err != nil {
		return registry.Registration{}, err
	}
	if err := repository.insertSession(ctx, tx, instance); err != nil {
		return registry.Registration{}, err
	}
	if err := repository.insertEvent(ctx, tx, registry.Event{EventID: command.EventID, TenantID: instance.TenantID, Revision: revision, Type: registry.EventRegistered, InstanceID: instance.InstanceID, SessionID: instance.SessionID, Generation: generation, OccurredAt: command.Now}); err != nil {
		return registry.Registration{}, err
	}
	result := registry.Registration{Instance: instance, LeaseTTLSeconds: uint64(command.TTL / time.Second), KeepaliveIntervalSeconds: uint64(command.Keepalive / time.Second)}
	encoded, err := json.Marshal(result)
	if err != nil {
		return registry.Registration{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	if _, err = tx.ExecContext(ctx, repository.query(`INSERT INTO arop_registry_idempotency(tenant_id,operation,key_digest,request_digest,result_json,result_revision,created_at) VALUES(?,?,?,?,?,?,?)`), command.Request.TenantID, "register", command.Request.IdempotencyKeyDigest, command.Request.IdempotencyRequestDigest, string(encoded), revision, formatTime(command.Now)); err != nil {
		return registry.Registration{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return result, nil
}

func (repository *Repository) Keepalive(ctx context.Context, command registry.KeepaliveCommand) (registry.Instance, error) {
	if err := command.Request.Validate(); err != nil || !utc(command.Now) || command.TTL <= 0 {
		return registry.Instance{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return registry.Instance{}, err
	}
	if err := repository.lockKeys(ctx, tx, "registry/instance/"+command.Request.TenantID+"/"+command.Request.InstanceID); err != nil {
		return registry.Instance{}, err
	}
	instance, err := repository.loadFenced(ctx, tx, registry.Fence{TenantID: command.Request.TenantID, InstanceID: command.Request.InstanceID, SessionID: command.Request.SessionID, LeaseID: command.Request.LeaseID, Generation: command.Request.Generation}, command.Now)
	if err != nil {
		return registry.Instance{}, err
	}
	if command.Request.HeartbeatSequence <= instance.HeartbeatSequence {
		return registry.Instance{}, registry.NewError(registry.ReasonHeartbeatStale)
	}
	capacity := instance.Runtime.Capacity
	capacity.ActiveRuns, capacity.AvailableSlots, capacity.QueueDepth = command.Request.ActiveRuns, command.Request.AvailableSlots, command.Request.QueueDepth
	if err := capacity.Validate(); err != nil {
		return registry.Instance{}, err
	}
	instance.Runtime.Healthy, instance.Runtime.Ready, instance.Runtime.Capacity = command.Request.Healthy, command.Request.Ready, capacity
	instance.HeartbeatSequence = command.Request.HeartbeatSequence
	instance.LeaseExpiresAt = command.Now.Add(command.TTL)
	return repository.commitMutation(ctx, tx, instance, command.EventID, registry.EventKeepalive, command.Now)
}

func (repository *Repository) CompareAndSwap(ctx context.Context, command registry.CASCommand) (registry.Instance, error) {
	if err := command.Request.Validate(); err != nil || !utc(command.Now) {
		return registry.Instance{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return registry.Instance{}, err
	}
	if err := repository.lockKeys(ctx, tx, "registry/instance/"+command.Request.TenantID+"/"+command.Request.InstanceID); err != nil {
		return registry.Instance{}, err
	}
	instance, exists, err := repository.loadInstance(ctx, tx, command.Request.TenantID, command.Request.InstanceID, true)
	if err != nil {
		return registry.Instance{}, err
	}
	if !exists {
		return registry.Instance{}, registry.NewError(registry.ReasonNotFound)
	}
	if instance.ResourceVersion != command.Request.ExpectedResourceVersion {
		return registry.Instance{}, registry.NewError(registry.ReasonResourceConflict)
	}
	instance.Operator = registry.OperatorState{Enabled: command.Request.Enabled, Weight: command.Request.Weight, Priority: command.Request.Priority, MaintenanceReason: command.Request.MaintenanceReason}
	return repository.commitMutation(ctx, tx, instance, command.EventID, registry.EventUpdated, command.Now)
}

func (repository *Repository) Drain(ctx context.Context, command registry.DrainCommand) (registry.Instance, error) {
	if err := command.Request.Validate(); err != nil || !utc(command.Now) || !command.Request.DeadlineAt.After(command.Now) {
		return registry.Instance{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return registry.Instance{}, err
	}
	if err := repository.lockKeys(ctx, tx, "registry/instance/"+command.Request.TenantID+"/"+command.Request.InstanceID); err != nil {
		return registry.Instance{}, err
	}
	instance, err := repository.loadFenced(ctx, tx, command.Request.Fence, command.Now)
	if err != nil {
		return registry.Instance{}, err
	}
	instance.Draining = true
	deadline := command.Request.DeadlineAt
	instance.DrainDeadlineAt = &deadline
	return repository.commitMutation(ctx, tx, instance, command.EventID, registry.EventDraining, command.Now)
}

func (repository *Repository) Deregister(ctx context.Context, command registry.DeregisterCommand) (registry.Instance, error) {
	if err := command.Fence.Validate(); err != nil || !utc(command.Now) {
		return registry.Instance{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return registry.Instance{}, err
	}
	if err := repository.lockKeys(ctx, tx, "registry/instance/"+command.Fence.TenantID+"/"+command.Fence.InstanceID); err != nil {
		return registry.Instance{}, err
	}
	instance, err := repository.loadFenced(ctx, tx, command.Fence, command.Now)
	if err != nil {
		return registry.Instance{}, err
	}
	instance.Status, instance.Draining, instance.DrainDeadlineAt, instance.LeaseExpiresAt = registry.StatusDeregistered, false, nil, command.Now
	return repository.commitMutation(ctx, tx, instance, command.EventID, registry.EventDeregistered, command.Now)
}

func (repository *Repository) Expire(ctx context.Context, command registry.ExpireCommand) ([]registry.Instance, error) {
	if !utc(command.Now) || command.Limit == 0 || uint64(len(command.EventIDs)) < command.Limit {
		return nil, registry.NewError(registry.ReasonInvalidRequest)
	}
	tx, err := repository.writeTx(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, repository.query(`SELECT tenant_id,instance_id FROM arop_registry_instances WHERE tenant_id=? AND status='registered' AND lease_expires_at<=? ORDER BY lease_expires_at,instance_id LIMIT ?`), command.TenantID, formatTime(command.Now), command.Limit)
	if err != nil {
		return nil, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	var keys [][2]string
	for rows.Next() {
		var tenantID, instanceID string
		if rows.Scan(&tenantID, &instanceID) != nil {
			_ = rows.Close()
			return nil, registry.NewError(registry.ReasonDependencyUnavailable)
		}
		keys = append(keys, [2]string{tenantID, instanceID})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	instances := make([]registry.Instance, 0, len(keys))
	for index, key := range keys {
		instance, exists, loadErr := repository.loadInstance(ctx, tx, key[0], key[1], true)
		if loadErr != nil {
			return nil, loadErr
		}
		if !exists || instance.Status != registry.StatusRegistered || command.Now.Before(instance.LeaseExpiresAt) {
			continue
		}
		instance.Status, instance.LeaseExpiresAt = registry.StatusExpired, command.Now
		updated, updateErr := repository.commitMutation(ctx, tx, instance, command.EventIDs[index], registry.EventExpired, command.Now)
		if updateErr != nil {
			return nil, updateErr
		}
		instances = append(instances, updated)
	}
	return instances, nil
}

func (repository *Repository) Snapshot(ctx context.Context, query registry.DiscoveryQuery, now time.Time) (registry.Snapshot, error) {
	if err := query.Validate(); err != nil || !utc(now) {
		return registry.Snapshot{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	var revision uint64
	if err := repository.queryer(ctx).QueryRowContext(ctx, `SELECT revision FROM arop_registry_meta WHERE singleton=1`).Scan(&revision); err != nil || revision > registry.MaxSafeInteger {
		return registry.Snapshot{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	rows, err := repository.queryer(ctx).QueryContext(ctx, repository.query(`SELECT `+instanceColumns+` FROM arop_registry_instances WHERE tenant_id=? AND status='registered' AND lease_expires_at>? ORDER BY instance_id`), query.TenantID, formatTime(now))
	if err != nil {
		return registry.Snapshot{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	defer rows.Close()
	result := registry.Snapshot{Revision: revision, Instances: []registry.Instance{}}
	for rows.Next() {
		instance, scanErr := scanInstance(rows)
		if scanErr != nil {
			return registry.Snapshot{}, scanErr
		}
		if instance.DiscoverableAt(now, query) {
			result.Instances = append(result.Instances, instance)
		}
	}
	if rows.Err() != nil {
		return registry.Snapshot{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return result, nil
}

func (repository *Repository) Events(ctx context.Context, tenantID string, afterRevision, limit uint64) ([]registry.Event, error) {
	if afterRevision > registry.MaxSafeInteger || limit == 0 || limit > 10000 {
		return nil, registry.NewError(registry.ReasonInvalidRequest)
	}
	rows, err := repository.queryer(ctx).QueryContext(ctx, repository.query(`SELECT event_id,tenant_id,revision,event_type,instance_id,session_id,generation,occurred_at FROM arop_registry_events WHERE tenant_id=? AND revision>? ORDER BY revision LIMIT ?`), tenantID, afterRevision, limit)
	if err != nil {
		return nil, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	defer rows.Close()
	result := []registry.Event{}
	for rows.Next() {
		var event registry.Event
		var eventType, occurred string
		if rows.Scan(&event.EventID, &event.TenantID, &event.Revision, &eventType, &event.InstanceID, &event.SessionID, &event.Generation, &occurred) != nil {
			return nil, registry.NewError(registry.ReasonDependencyUnavailable)
		}
		event.Type = registry.EventType(eventType)
		event.OccurredAt, err = parseTime(occurred)
		if err != nil || event.Validate() != nil {
			return nil, registry.NewError(registry.ReasonDependencyUnavailable)
		}
		result = append(result, event)
	}
	if rows.Err() != nil {
		return nil, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return result, nil
}

func (repository *Repository) CompactionWatermark(ctx context.Context, _ string) (uint64, error) {
	var watermark uint64
	if err := repository.queryer(ctx).QueryRowContext(ctx, `SELECT compaction_watermark FROM arop_registry_meta WHERE singleton=1`).Scan(&watermark); err != nil || watermark > registry.MaxSafeInteger {
		return 0, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return watermark, nil
}

const instanceColumns = `tenant_id,instance_id,session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at`

type scanner interface{ Scan(...any) error }

func scanInstance(row scanner) (registry.Instance, error) {
	var instance registry.Instance
	var bindingsJSON, runtimeJSON, operatorJSON, leaseExpires, createdAt, updatedAt, status string
	var drain sql.NullString
	if err := row.Scan(&instance.TenantID, &instance.InstanceID, &instance.SessionID, &instance.ServiceID, &instance.Environment, &instance.Generation, &instance.ResourceVersion, &instance.RegistryRevision, &instance.LeaseID, &leaseExpires, &instance.HeartbeatSequence, &instance.Endpoint.BaseURL, &instance.Endpoint.HealthPath, &bindingsJSON, &runtimeJSON, &operatorJSON, &instance.Draining, &drain, &status, &createdAt, &updatedAt); err != nil {
		return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	var err error
	instance.LeaseExpiresAt, err = parseTime(leaseExpires)
	if err == nil {
		instance.CreatedAt, err = parseTime(createdAt)
	}
	if err == nil {
		instance.UpdatedAt, err = parseTime(updatedAt)
	}
	if err == nil && drain.Valid {
		value, parseErr := parseTime(drain.String)
		err, instance.DrainDeadlineAt = parseErr, &value
	}
	if err != nil || json.Unmarshal([]byte(bindingsJSON), &instance.Bindings) != nil || json.Unmarshal([]byte(runtimeJSON), &instance.Runtime) != nil || json.Unmarshal([]byte(operatorJSON), &instance.Operator) != nil {
		return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	instance.Status = registry.InstanceStatus(status)
	if instance.Validate() != nil {
		return registry.Instance{}, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return instance, nil
}

func (repository *Repository) loadInstance(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, instanceID string, lock bool) (registry.Instance, bool, error) {
	query := `SELECT ` + instanceColumns + ` FROM arop_registry_instances WHERE tenant_id=? AND instance_id=?`
	if lock && repository.dialect == Postgres {
		query += ` FOR UPDATE`
	}
	instance, err := scanInstance(queryer.QueryRowContext(ctx, repository.query(query), tenantID, instanceID))
	if err != nil {
		// scanInstance intentionally hides database errors, so probe existence to
		// distinguish absence without exposing driver details.
		var one int
		probe := queryer.QueryRowContext(ctx, repository.query(`SELECT 1 FROM arop_registry_instances WHERE tenant_id=? AND instance_id=?`), tenantID, instanceID).Scan(&one)
		if errors.Is(probe, sql.ErrNoRows) {
			return registry.Instance{}, false, nil
		}
		return registry.Instance{}, false, err
	}
	return instance, true, nil
}

func (repository *Repository) loadFenced(ctx context.Context, tx *sql.Tx, fence registry.Fence, now time.Time) (registry.Instance, error) {
	instance, exists, err := repository.loadInstance(ctx, tx, fence.TenantID, fence.InstanceID, true)
	if err != nil {
		return registry.Instance{}, err
	}
	if !exists {
		return registry.Instance{}, registry.NewError(registry.ReasonNotFound)
	}
	if instance.Generation != fence.Generation || instance.SessionID != fence.SessionID || instance.LeaseID != fence.LeaseID {
		return registry.Instance{}, registry.NewError(registry.ReasonGenerationFenced)
	}
	if instance.Status != registry.StatusRegistered || !now.Before(instance.LeaseExpiresAt) {
		return registry.Instance{}, registry.NewError(registry.ReasonLeaseExpired)
	}
	return instance, nil
}

func (repository *Repository) commitMutation(ctx context.Context, tx *sql.Tx, instance registry.Instance, eventID string, eventType registry.EventType, now time.Time) (registry.Instance, error) {
	if instance.ResourceVersion >= registry.MaxSafeInteger {
		return registry.Instance{}, registry.NewError(registry.ReasonRevisionOverflow)
	}
	revision, err := repository.allocateRevision(ctx, tx)
	if err != nil {
		return registry.Instance{}, err
	}
	instance.ResourceVersion++
	instance.RegistryRevision, instance.UpdatedAt = revision, now
	if instance.Validate() != nil {
		return registry.Instance{}, registry.NewError(registry.ReasonInvalidRequest)
	}
	if err := repository.saveInstance(ctx, tx, instance, true); err != nil {
		return registry.Instance{}, err
	}
	if err := repository.insertEvent(ctx, tx, registry.Event{EventID: eventID, TenantID: instance.TenantID, Revision: revision, Type: eventType, InstanceID: instance.InstanceID, SessionID: instance.SessionID, Generation: instance.Generation, OccurredAt: now}); err != nil {
		return registry.Instance{}, err
	}
	return instance, nil
}

func (repository *Repository) saveInstance(ctx context.Context, tx *sql.Tx, instance registry.Instance, exists bool) error {
	bindings, err := json.Marshal(instance.Bindings)
	if err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	runtime, err := json.Marshal(instance.Runtime)
	if err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	operator, err := json.Marshal(instance.Operator)
	if err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	var deadline any
	if instance.DrainDeadlineAt != nil {
		deadline = formatTime(*instance.DrainDeadlineAt)
	}
	values := []any{instance.SessionID, instance.ServiceID, instance.Environment, instance.Generation, instance.ResourceVersion, instance.RegistryRevision, instance.LeaseID, formatTime(instance.LeaseExpiresAt), instance.HeartbeatSequence, instance.Endpoint.BaseURL, instance.Endpoint.HealthPath, string(bindings), string(runtime), string(operator), instance.Draining, deadline, string(instance.Status), formatTime(instance.CreatedAt), formatTime(instance.UpdatedAt), instance.TenantID, instance.InstanceID}
	if exists {
		result, execErr := tx.ExecContext(ctx, repository.query(`UPDATE arop_registry_instances SET session_id=?,service_id=?,environment=?,generation=?,resource_version=?,registry_revision=?,lease_id=?,lease_expires_at=?,heartbeat_sequence=?,endpoint_base_url=?,endpoint_health_path=?,bindings_json=?,runtime_json=?,operator_json=?,draining=?,drain_deadline_at=?,status=?,created_at=?,updated_at=? WHERE tenant_id=? AND instance_id=?`), values...)
		if execErr != nil {
			return registry.NewError(registry.ReasonDependencyUnavailable)
		}
		affected, execErr := result.RowsAffected()
		if execErr != nil || affected != 1 {
			return registry.NewError(registry.ReasonResourceConflict)
		}
		return nil
	}
	_, err = tx.ExecContext(ctx, repository.query(`INSERT INTO arop_registry_instances(session_id,service_id,environment,generation,resource_version,registry_revision,lease_id,lease_expires_at,heartbeat_sequence,endpoint_base_url,endpoint_health_path,bindings_json,runtime_json,operator_json,draining,drain_deadline_at,status,created_at,updated_at,tenant_id,instance_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), values...)
	if err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return nil
}

func (repository *Repository) insertSession(ctx context.Context, tx *sql.Tx, instance registry.Instance) error {
	if _, err := tx.ExecContext(ctx, repository.query(`INSERT INTO arop_registry_sessions(tenant_id,instance_id,session_id,generation,created_at) VALUES(?,?,?,?,?)`), instance.TenantID, instance.InstanceID, instance.SessionID, instance.Generation, formatTime(instance.UpdatedAt)); err != nil {
		return registry.NewError(registry.ReasonSessionReused)
	}
	return nil
}

func (repository *Repository) insertEvent(ctx context.Context, tx *sql.Tx, event registry.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, repository.query(`INSERT INTO arop_registry_events(revision,event_id,tenant_id,event_type,instance_id,session_id,generation,occurred_at) VALUES(?,?,?,?,?,?,?,?)`), event.Revision, event.EventID, event.TenantID, string(event.Type), event.InstanceID, event.SessionID, event.Generation, formatTime(event.OccurredAt)); err != nil {
		return registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return nil
}

func (repository *Repository) allocateRevision(ctx context.Context, tx *sql.Tx) (uint64, error) {
	query := `SELECT revision FROM arop_registry_meta WHERE singleton=1`
	if repository.dialect == Postgres {
		query += ` FOR UPDATE`
	}
	var revision uint64
	if err := tx.QueryRowContext(ctx, query).Scan(&revision); err != nil {
		return 0, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	if revision >= registry.MaxSafeInteger {
		return 0, registry.NewError(registry.ReasonRevisionOverflow)
	}
	result, err := tx.ExecContext(ctx, repository.query(`UPDATE arop_registry_meta SET revision=? WHERE singleton=1 AND revision=?`), revision+1, revision)
	if err != nil {
		return 0, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return 0, registry.NewError(registry.ReasonResourceConflict)
	}
	return revision + 1, nil
}

func (repository *Repository) loadIdempotency(ctx context.Context, tx *sql.Tx, tenantID, keyDigest string) (string, []byte, bool, error) {
	var requestDigest, result string
	err := tx.QueryRowContext(ctx, repository.query(`SELECT request_digest,result_json FROM arop_registry_idempotency WHERE tenant_id=? AND operation='register' AND key_digest=?`), tenantID, keyDigest).Scan(&requestDigest, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return requestDigest, []byte(result), true, nil
}

func (repository *Repository) sessionUsed(ctx context.Context, tx *sql.Tx, tenantID, instanceID, sessionID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, repository.query(`SELECT 1 FROM arop_registry_sessions WHERE tenant_id=? AND instance_id=? AND session_id=?`), tenantID, instanceID, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, registry.NewError(registry.ReasonDependencyUnavailable)
	}
	return true, nil
}

func (repository *Repository) writeTx(ctx context.Context) (*sql.Tx, error) {
	if tx, ok := repository.lookup(ctx); ok && tx != nil {
		return tx, nil
	}
	return nil, registry.NewError(registry.ReasonDependencyUnavailable)
}

func (repository *Repository) queryer(ctx context.Context) interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
} {
	if tx, ok := repository.lookup(ctx); ok && tx != nil {
		return tx
	}
	return repository.db
}

func (repository *Repository) lockKeys(ctx context.Context, tx *sql.Tx, keys ...string) error {
	if repository.dialect != Postgres {
		return nil
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return registry.NewError(registry.ReasonDependencyUnavailable)
		}
	}
	return nil
}

func (repository *Repository) query(query string) string {
	if repository.dialect != Postgres {
		return query
	}
	var builder strings.Builder
	index := 1
	for _, character := range query {
		if character == '?' {
			fmt.Fprintf(&builder, "$%d", index)
			index++
		} else {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || formatTime(parsed) != value {
		return time.Time{}, errors.New("invalid registry timestamp")
	}
	return parsed, nil
}

func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }

func cloneBindings(value []registry.Binding) []registry.Binding {
	encoded, _ := json.Marshal(value)
	var cloned []registry.Binding
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func cloneRuntime(value registry.RuntimeState) registry.RuntimeState {
	encoded, _ := json.Marshal(value)
	var cloned registry.RuntimeState
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
