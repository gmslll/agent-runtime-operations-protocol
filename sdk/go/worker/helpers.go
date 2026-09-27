package worker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	workerwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/worker"
)

// EffectID derives an Attempt-independent effect id from the stable Run id and
// a caller-defined operation key. The operation key must identify one logical
// side effect, not one retry.
func EffectID(runID, operation string) (string, error) {
	if runID == "" || len(operation) < 1 || len(operation) > 512 || strings.TrimSpace(operation) != operation {
		return "", errors.New("invalid effect identity")
	}
	digest := sha256.Sum256([]byte("arop-effect-v1\x00" + runID + "\x00" + operation))
	return "eff_" + base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// EventHelper supplies monotonic per-Attempt producer sequence numbers and
// UUIDv7 event ids. It is safe for concurrent handler goroutines.
type EventHelper struct {
	claim    workerwire.WorkerClaim
	clock    func() time.Time
	mu       sync.Mutex
	sequence uint64
}

type EventMetadata struct {
	EventID          string
	RunID            string
	AttemptID        string
	FencingToken     uint64
	ProducerSequence uint64
	OccurredAt       time.Time
}

func NewEventHelper(claim workerwire.WorkerClaim, clock func() time.Time) (*EventHelper, error) {
	if string(claim.RunID) == "" || string(claim.AttemptID) == "" || uint64(claim.FencingToken) == 0 {
		return nil, errors.New("invalid event claim")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &EventHelper{claim: claim, clock: clock}, nil
}

func (helper *EventHelper) Next() (EventMetadata, error) {
	helper.mu.Lock()
	defer helper.mu.Unlock()
	if helper.sequence == 9007199254740991 {
		return EventMetadata{}, errors.New("event producer sequence exhausted")
	}
	now := helper.clock().UTC()
	id, err := newUUIDv7("evt_", now)
	if err != nil {
		return EventMetadata{}, err
	}
	helper.sequence++
	return EventMetadata{EventID: id, RunID: string(helper.claim.RunID), AttemptID: string(helper.claim.AttemptID), FencingToken: uint64(helper.claim.FencingToken), ProducerSequence: helper.sequence, OccurredAt: now}, nil
}

// SecureCompletionSource uses crypto/rand and UUIDv7. Its idempotency key is
// derived once from the completion id and is safe to reuse for only that
// completion request.
type SecureCompletionSource struct {
	Clock func() time.Time
}

func (source SecureCompletionSource) Completion(_ context.Context, _ workerwire.WorkerClaim) (string, string, error) {
	clock := source.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	id, err := newUUIDv7("cmp_", clock().UTC())
	if err != nil {
		return "", "", err
	}
	return id, "complete-" + strings.TrimPrefix(id, "cmp_"), nil
}

func NewSessionID(now time.Time) (string, error) {
	return newUUIDv7("ses_", now.UTC())
}

func newUUIDv7(prefix string, now time.Time) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("secure random unavailable")
	}
	millis := now.UnixMilli()
	if millis < 0 || millis > 1<<48-1 {
		return "", errors.New("time outside UUIDv7 range")
	}
	binary.BigEndian.PutUint64(raw[:8], uint64(millis)<<16|uint64(raw[6])<<8|uint64(raw[7]))
	raw[6] = raw[6]&0x0f | 0x70
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return prefix + encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
