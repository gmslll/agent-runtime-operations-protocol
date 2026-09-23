package platform

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"sync/atomic"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

const (
	ReadinessScope      = "platform-bootstrap"
	DurabilityEphemeral = "ephemeral"
	DurabilityDurable   = "durable"
	// DurabilityMode is retained for the P08 development-memory compatibility
	// surface. Runtime responses use Platform.Durability instead.
	DurabilityMode = DurabilityEphemeral
)

var checkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type Dependencies struct {
	Clock         platformports.Clock
	IDs           platformports.IDSource
	Faults        platformports.FaultHook
	UoW           platformports.UnitOfWork
	Observability observability.Store
	Checks        []platformports.ReadinessCheck
}

type ReadinessCheck = platformports.ReadinessCheck

type ReadinessCheckFunc struct {
	CheckName string
	Func      func(context.Context) error
}

func (check ReadinessCheckFunc) Name() string { return check.CheckName }
func (check ReadinessCheckFunc) Check(ctx context.Context) error {
	if check.Func == nil {
		return errors.New("readiness function is required")
	}
	return check.Func(ctx)
}

type Platform struct {
	config      Config
	service     string
	version     string
	deps        Dependencies
	checks      []platformports.ReadinessCheck
	draining    atomic.Bool
	initialized atomic.Bool
}

type ReadinessResult struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason"`
}

type ReadinessSnapshot struct {
	Ready      bool              `json:"ready"`
	Scope      string            `json:"scope"`
	Durability string            `json:"durability"`
	Checks     []ReadinessResult `json:"checks"`
}

func New(config Config, dependencies Dependencies, service, version string) (*Platform, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate platform config: %w", err)
	}
	if service == "" || version == "" {
		return nil, errors.New("service and version are required")
	}
	for name, dependency := range map[string]any{
		"clock": dependencies.Clock, "id source": dependencies.IDs,
		"fault hook": dependencies.Faults, "unit of work": dependencies.UoW,
		"observability store": dependencies.Observability,
	} {
		if isNil(dependency) {
			return nil, fmt.Errorf("%s dependency is required", name)
		}
	}
	now := dependencies.Clock.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return nil, errors.New("clock must return non-zero UTC time")
	}
	for _, kind := range []platformports.IDKind{platformports.IDRequest, platformports.IDAudit, platformports.IDTrace, platformports.IDSpan} {
		identifier, err := dependencies.IDs.NewID(context.Background(), kind)
		if err != nil || identifier == "" {
			return nil, fmt.Errorf("ID source self-check failed for %s", kind)
		}
	}
	called := 0
	if err := dependencies.UoW.Within(context.Background(), func(context.Context) error { called++; return nil }); err != nil || called != 1 {
		return nil, errors.New("unit of work self-check failed")
	}
	checks := append([]platformports.ReadinessCheck(nil), dependencies.Checks...)
	seen := map[string]bool{"audit": true, "trace": true, "draining": true}
	for _, check := range checks {
		if isNil(check) {
			return nil, errors.New("nil readiness check")
		}
		name := check.Name()
		if len(name) > 100 || !checkNamePattern.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate readiness check name %q", name)
		}
		seen[name] = true
	}
	sort.Slice(checks, func(left, right int) bool { return checks[left].Name() < checks[right].Name() })
	result := &Platform{config: config, service: service, version: version, deps: dependencies, checks: checks}
	result.initialized.Store(true)
	return result, nil
}

func (platform *Platform) Config() Config  { return platform.config }
func (platform *Platform) Service() string { return platform.service }
func (platform *Platform) Version() string { return platform.version }
func (platform *Platform) Durability() string {
	if platform != nil && platform.config.Mode != ModeDevelopmentMemory {
		return DurabilityDurable
	}
	return DurabilityEphemeral
}

func (platform *Platform) Now() time.Time { return platform.deps.Clock.Now().UTC() }

func (platform *Platform) NewID(ctx context.Context, kind platformports.IDKind) (string, error) {
	if err := kind.Validate(); err != nil {
		return "", err
	}
	return platform.deps.IDs.NewID(ctx, kind)
}

func (platform *Platform) CheckFault(ctx context.Context, checkpoint platformports.Checkpoint) error {
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	return platform.deps.Faults.Check(ctx, checkpoint)
}

func (platform *Platform) Within(ctx context.Context, callback func(context.Context) error) error {
	if callback == nil {
		return errors.New("unit of work callback is required")
	}
	return platform.deps.UoW.Within(ctx, callback)
}

func (platform *Platform) QueryAudit(ctx context.Context, query observability.AuditQuery) ([]observability.AuditEntry, error) {
	return platform.deps.Observability.QueryAudit(ctx, query)
}

func (platform *Platform) QueryTrace(ctx context.Context, query observability.TraceQuery) ([]observability.SpanRecord, error) {
	return platform.deps.Observability.QueryTrace(ctx, query)
}

func (platform *Platform) AuditForRequest(ctx context.Context, requestID string) ([]observability.AuditEntry, error) {
	return platform.QueryAudit(ctx, observability.AuditQuery{RequestID: requestID})
}

func (platform *Platform) TracesForRequest(ctx context.Context, requestID string) ([]observability.SpanRecord, error) {
	return platform.QueryTrace(ctx, observability.TraceQuery{RequestID: requestID})
}

func (platform *Platform) SetDraining(draining bool) { platform.draining.Store(draining) }

func (platform *Platform) Readiness(ctx context.Context) ReadinessSnapshot {
	if platform == nil || !platform.initialized.Load() {
		return ReadinessSnapshot{Ready: false, Scope: ReadinessScope, Durability: DurabilityMode, Checks: []ReadinessResult{{Name: "initialized", Ready: false, Reason: "not_initialized"}}}
	}
	results := []ReadinessResult{{Name: "initialized", Ready: true, Reason: "ok"}, {Name: "draining", Ready: !platform.draining.Load(), Reason: "ok"}}
	if platform.draining.Load() {
		results[1].Reason = "draining"
	}
	results = append(results,
		readinessResult(ctx, "audit", platform.deps.Observability.AuditHealth),
		readinessResult(ctx, "trace", platform.deps.Observability.TraceHealth),
	)
	for _, check := range platform.checks {
		results = append(results, readinessResult(ctx, check.Name(), check.Check))
	}
	ready := true
	for _, result := range results {
		ready = ready && result.Ready
	}
	return ReadinessSnapshot{Ready: ready, Scope: ReadinessScope, Durability: platform.Durability(), Checks: results}
}

func readinessResult(ctx context.Context, name string, check func(context.Context) error) ReadinessResult {
	if ctx.Err() != nil || check(ctx) != nil {
		return ReadinessResult{Name: name, Ready: false, Reason: "check_failed"}
	}
	return ReadinessResult{Name: name, Ready: true, Reason: "ok"}
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
