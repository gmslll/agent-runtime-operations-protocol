package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	eventwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/event"
)

const (
	eventBatchMaxEvents = 100
	eventBatchMaxBytes  = 262144
)

// EventBatchPlan is an immutable delivery attempt assembled from the durable
// provider outbox. Payload is safe to send to the Event Session batch URL with
// BatchID as the Idempotency-Key. A plan is deliberately secret-free.
type EventBatchPlan struct {
	BatchID                     string
	Payload                     json.RawMessage
	FirstSequence, LastSequence uint64
	eventIDs                    map[string]uint64
	payloadDigest               [sha256.Size]byte
}

// BuildEventBatch returns the first unacknowledged, contiguous outbox suffix.
// Rebuilding before an ACK produces the same BatchID and bytes. This makes an
// ambiguous HTTP outcome safe to retry with the same idempotency key.
func (runtime *Runtime) BuildEventBatch(ctx context.Context, runID, attemptID string) (EventBatchPlan, error) {
	if !validPrefixed("run_", runID) || !validPrefixed("att_", attemptID) {
		return EventBatchPlan{}, errors.New("invalid event batch identity")
	}
	inbox, err := runtime.config.Store.GetInbox(ctx, runID, attemptID)
	if err != nil || inbox.RunID != runID || inbox.AttemptID != attemptID {
		return EventBatchPlan{}, errors.New("event batch inbox unavailable")
	}
	records, err := runtime.firstUndelivered(ctx, runID, attemptID)
	if err != nil {
		return EventBatchPlan{}, err
	}
	if len(records) == 0 {
		return EventBatchPlan{}, ErrNotFound
	}
	events := make([]eventwire.EventEnvelope, 0, eventBatchMaxEvents)
	eventIDs := make(map[string]uint64, eventBatchMaxEvents)
	first := records[0].Sequence
	for index, record := range records {
		if len(events) == eventBatchMaxEvents {
			break
		}
		if record.DeliveredAt != nil || record.Sequence != first+uint64(index) {
			return EventBatchPlan{}, errors.New("provider outbox is not a contiguous unacknowledged suffix")
		}
		direct, envelopeErr := runtime.directEnvelope(inbox, record)
		if envelopeErr != nil {
			return EventBatchPlan{}, errors.New("provider outbox contains an invalid event")
		}
		event, decodeErr := eventwire.DecodeEventEnvelope(direct.Bytes)
		if decodeErr != nil || uint64(event.Producersequence) != record.Sequence || event.Runsequence != nil {
			return EventBatchPlan{}, errors.New("provider outbox event is not batch-safe")
		}
		events = append(events, event)
		eventIDs[string(event.ID)] = record.Sequence
		candidate, encodeErr := encodeEventBatch(runID, attemptID, inbox.FencingToken, events)
		if encodeErr != nil {
			return EventBatchPlan{}, encodeErr
		}
		if len(candidate.Payload) > eventBatchMaxBytes {
			events = events[:len(events)-1]
			delete(eventIDs, string(event.ID))
			break
		}
	}
	if len(events) == 0 {
		return EventBatchPlan{}, errors.New("first provider event exceeds batch byte limit")
	}
	plan, err := encodeEventBatch(runID, attemptID, inbox.FencingToken, events)
	if err != nil {
		return EventBatchPlan{}, err
	}
	plan.FirstSequence = first
	plan.LastSequence = first + uint64(len(events)) - 1
	plan.eventIDs = eventIDs
	plan.payloadDigest = sha256.Sum256(plan.Payload)
	return plan, nil
}

func (runtime *Runtime) firstUndelivered(ctx context.Context, runID, attemptID string) ([]OutboxRecord, error) {
	var after uint64
	for {
		page, err := runtime.config.Store.ListOutbox(ctx, runID, attemptID, after, maxStreamBatch)
		if err != nil {
			return nil, errors.New("list provider outbox for batch delivery")
		}
		for index, record := range page {
			if record.Sequence != after+uint64(index)+1 {
				return nil, errors.New("provider outbox sequence gap")
			}
			if record.DeliveredAt == nil {
				return page[index:], nil
			}
		}
		if len(page) < maxStreamBatch {
			return nil, nil
		}
		after = page[len(page)-1].Sequence
	}
}

func encodeEventBatch(runID, attemptID string, fencing uint64, events []eventwire.EventEnvelope) (EventBatchPlan, error) {
	if fencing == 0 || fencing > maxSafeInteger || len(events) == 0 || len(events) > eventBatchMaxEvents {
		return EventBatchPlan{}, errors.New("invalid event batch limits")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(runID + "\x00" + attemptID + "\x00" + strconv.FormatUint(fencing, 10)))
	for _, event := range events {
		encoded, err := eventwire.EncodeEventEnvelope(event)
		if err != nil {
			return EventBatchPlan{}, errors.New("encode provider event")
		}
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(encoded)
	}
	identifier := hash.Sum(nil)[:16]
	identifier[6] = identifier[6]&0x0f | 0x70
	identifier[8] = identifier[8]&0x3f | 0x80
	value := hex.EncodeToString(identifier)
	batchID := "batch_" + value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:]
	batch := eventwire.EventBatch{BatchID: batchID, AttemptID: eventwire.AttemptId(attemptID), FencingToken: eventwire.EventSessionPositiveSafeInteger(fencing), Events: events}
	payload, err := eventwire.EncodeEventBatch(batch)
	if err != nil {
		return EventBatchPlan{}, errors.New("encode event batch")
	}
	return EventBatchPlan{BatchID: batchID, Payload: payload}, nil
}

// ApplyEventBatchAck durably marks only the acknowledged prefix delivered.
// A partial ACK therefore leaves the suffix available for the next plan. An
// already-applied ACK is idempotent; conflicting or over-broad ACKs fail closed.
func (runtime *Runtime) ApplyEventBatchAck(ctx context.Context, plan EventBatchPlan, raw []byte) error {
	if plan.FirstSequence == 0 || plan.LastSequence < plan.FirstSequence || len(plan.Payload) == 0 || sha256.Sum256(plan.Payload) != plan.payloadDigest {
		return errors.New("invalid or mutated event batch plan")
	}
	ack, err := eventwire.DecodeEventBatchAck(raw)
	if err != nil {
		return errors.New("invalid event batch acknowledgement")
	}
	accepted := uint64(ack.AcceptedThroughProducerSequence)
	if accepted < plan.FirstSequence || accepted > plan.LastSequence {
		return errors.New("event batch acknowledgement is outside the submitted range")
	}
	seenDuplicates := map[string]bool{}
	for _, identifier := range ack.DuplicateEventIDs {
		sequence, exists := plan.eventIDs[string(identifier)]
		if !exists || sequence > accepted || seenDuplicates[string(identifier)] {
			return errors.New("event batch acknowledgement contains an invalid duplicate id")
		}
		seenDuplicates[string(identifier)] = true
	}
	batch, decodeErr := eventwire.DecodeEventBatch(plan.Payload)
	if decodeErr != nil || batch.BatchID != plan.BatchID {
		return errors.New("event batch plan payload mismatch")
	}
	records, err := runtime.config.Store.ListOutbox(ctx, string(batch.Events[0].Runid), string(batch.AttemptID), plan.FirstSequence-1, int(accepted-plan.FirstSequence+1))
	if err != nil || len(records) != int(accepted-plan.FirstSequence+1) {
		return errors.New("acknowledged provider outbox prefix unavailable")
	}
	for index, record := range records {
		if record.Sequence != plan.FirstSequence+uint64(index) {
			return errors.New("acknowledged provider outbox prefix changed")
		}
	}
	acknowledgedAt := runtime.now()
	return runtime.config.Store.Within(ctx, func(txctx context.Context, transaction Transaction) error {
		for _, record := range records {
			if record.DeliveredAt != nil {
				continue
			}
			if markErr := transaction.MarkOutboxDelivered(txctx, record.RunID, record.AttemptID, record.Sequence, acknowledgedAt); markErr != nil {
				return markErr
			}
		}
		return nil
	})
}

// EventBatchPayloadEqual is a small diagnostic helper for adapters that keep
// an in-flight plan in memory while retrying an ambiguous transport outcome.
func EventBatchPayloadEqual(left, right EventBatchPlan) bool {
	return left.BatchID == right.BatchID && bytes.Equal(left.Payload, right.Payload)
}
