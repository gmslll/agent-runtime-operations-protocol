package platform

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	platformports "github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/app/platform/ports"
)

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type SystemIDSource struct{ Clock platformports.Clock }

func (source SystemIDSource) NewID(ctx context.Context, kind platformports.IDKind) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := kind.Validate(); err != nil {
		return "", err
	}
	if source.Clock == nil {
		return "", errors.New("ID source clock is required")
	}
	switch kind {
	case platformports.IDRequest, platformports.IDAudit:
		value, err := uuidV7(source.Clock.Now(), rand.Read)
		if err != nil {
			return "", err
		}
		if kind == platformports.IDRequest {
			return "req_" + value, nil
		}
		return "aud_" + value, nil
	case platformports.IDTrace:
		return randomHex(16)
	case platformports.IDSpan:
		return randomHex(8)
	default:
		return "", fmt.Errorf("unsupported ID kind %q", kind)
	}
}

func uuidV7(now time.Time, read func([]byte) (int, error)) (string, error) {
	if now.IsZero() {
		return "", errors.New("UUIDv7 time is zero")
	}
	var value [16]byte
	if _, err := read(value[:]); err != nil {
		return "", fmt.Errorf("generate UUIDv7 entropy: %w", err)
	}
	milliseconds := uint64(now.UnixMilli())
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], milliseconds)
	copy(value[0:6], timestamp[2:8])
	value[6] = value[6]&0x0f | 0x70
	value[8] = value[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(value[:])
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32], nil
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	allZero := true
	for _, item := range value {
		allZero = allZero && item == 0
	}
	if allZero {
		return "", errors.New("generated an all-zero identifier")
	}
	return hex.EncodeToString(value), nil
}

type NoopFaultHook struct{}

func (NoopFaultHook) Check(ctx context.Context, checkpoint platformports.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return checkpoint.Validate()
}

// SerialUnitOfWork is an ephemeral bootstrap boundary. It provides callback
// isolation and exactly-once invocation, but deliberately makes no rollback or
// durability claim; P09 replaces it with database transactions.
type SerialUnitOfWork struct{ mutex sync.Mutex }

func (unit *SerialUnitOfWork) Within(ctx context.Context, callback func(context.Context) error) error {
	if callback == nil {
		return errors.New("unit of work callback is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	unit.mutex.Lock()
	defer unit.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return callback(ctx)
}

var _ platformports.Clock = RealClock{}
var _ platformports.IDSource = SystemIDSource{}
var _ platformports.FaultHook = NoopFaultHook{}
var _ platformports.UnitOfWork = (*SerialUnitOfWork)(nil)
