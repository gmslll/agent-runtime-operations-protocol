// Package memory provides the explicitly ephemeral P08 observability bootstrap.
// It is bounded and reports any eviction as unhealthy so data loss is visible.
package memory

import (
	"context"
	"errors"
	"sync"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/ports/observability"
)

type Store struct {
	mutex              sync.RWMutex
	capacity           int
	audits             []observability.AuditEntry
	traces             []observability.SpanRecord
	observationEvicted uint64
}

func New(auditCapacity, traceCapacity int) (*Store, error) {
	if auditCapacity < 1 || traceCapacity < 1 {
		return nil, errors.New("memory observability capacities must be positive")
	}
	if auditCapacity != traceCapacity {
		return nil, errors.New("memory audit and trace capacities must be equal")
	}
	return &Store{capacity: auditCapacity}, nil
}

func (store *Store) AppendObservation(ctx context.Context, entry observability.AuditEntry, span observability.SpanRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := observability.ValidateObservationPair(entry, span); err != nil {
		return err
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if len(store.audits) != len(store.traces) {
		return errors.New("memory observation store invariant violated")
	}
	if len(store.audits) == store.capacity {
		copy(store.audits, store.audits[1:])
		copy(store.traces, store.traces[1:])
		store.audits[len(store.audits)-1] = entry
		store.traces[len(store.traces)-1] = span
		store.observationEvicted++
	} else {
		store.audits = append(store.audits, entry)
		store.traces = append(store.traces, span)
	}
	return nil
}

func (store *Store) QueryAudit(ctx context.Context, query observability.AuditQuery) ([]observability.AuditEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	entries := make([]observability.AuditEntry, 0, min(limit, len(store.audits)))
	for _, entry := range store.audits {
		if query.RequestID != "" && entry.RequestID != query.RequestID ||
			query.TraceID != "" && entry.TraceID != query.TraceID ||
			query.Operation != "" && entry.Operation != query.Operation ||
			query.Outcome != "" && entry.Outcome != query.Outcome {
			continue
		}
		entries = append(entries, entry)
		if len(entries) == limit {
			break
		}
	}
	return entries, nil
}

func (store *Store) AuditHealth(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	if store.observationEvicted != 0 {
		return errors.New("ephemeral audit store has evicted records")
	}
	return nil
}

func (store *Store) QueryTrace(ctx context.Context, query observability.TraceQuery) ([]observability.SpanRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	spans := make([]observability.SpanRecord, 0, min(limit, len(store.traces)))
	for _, span := range store.traces {
		if query.RequestID != "" && span.RequestID != query.RequestID ||
			query.TraceID != "" && span.TraceID != query.TraceID ||
			query.Operation != "" && span.Operation != query.Operation {
			continue
		}
		spans = append(spans, span)
		if len(spans) == limit {
			break
		}
	}
	return spans, nil
}

func (store *Store) TraceHealth(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	if store.observationEvicted != 0 {
		return errors.New("ephemeral trace store has evicted records")
	}
	return nil
}

var _ observability.Store = (*Store)(nil)
