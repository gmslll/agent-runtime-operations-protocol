package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

const (
	maxStreamBatch = 256
	maxSafeInteger = uint64(9007199254740991)
)

func (runtime *Runtime) stream(response http.ResponseWriter, request *http.Request, runID string) {
	if !singleMediaType(request.Header, "Accept", "text/event-stream") {
		writeProblem(response, http.StatusNotAcceptable, "INVALID_ACCEPT")
		return
	}
	after, ok := streamCursor(request.Header)
	if !ok {
		writeProblem(response, http.StatusBadRequest, "INVALID_STREAM_CURSOR")
		return
	}
	token, ok := bearer(request.Header)
	if !ok {
		writeUnauthorized(response)
		return
	}
	claims, err := runtime.verifyScope(request.Context(), token, request.Method, request.URL.Path, "run:stream")
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
	first, err := runtime.config.Store.ListOutbox(request.Context(), runID, claims.AttemptID, after, maxStreamBatch)
	if err != nil {
		writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
		return
	}
	if len(first) != 0 && first[0].Sequence != after+1 {
		writeCursorExpired(response, runID, first[0].Sequence)
		return
	}
	if len(first) == 0 && after != 0 && record.State.Terminal() {
		latest, latestErr := runtime.latestOutboxSequence(request.Context(), runID, claims.AttemptID)
		if latestErr != nil {
			writeProblem(response, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
			return
		}
		if after > latest {
			writeProblem(response, http.StatusConflict, "STREAM_CURSOR_AHEAD")
			return
		}
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(response)
	if err = controller.Flush(); err != nil {
		return
	}
	ctx, cancel := streamContext(request.Context(), runtime.now(), claims.ExpiresAt)
	defer cancel()
	page := first
	lastHeartbeat := runtime.now()
	for {
		for _, outbox := range page {
			if outbox.Sequence != after+1 {
				return
			}
			envelope, encodeErr := runtime.directEnvelope(record, outbox)
			if encodeErr != nil || setStreamWriteDeadline(controller) != nil {
				return
			}
			if _, err = fmt.Fprintf(response, "id: %d\nevent: %s\ndata: %s\n\n", outbox.Sequence, envelope.Type, envelope.Bytes); err != nil || controller.Flush() != nil {
				return
			}
			after = outbox.Sequence
		}
		current, currentErr := runtime.config.Store.GetInbox(ctx, runID, claims.AttemptID)
		if currentErr != nil {
			return
		}
		record = current
		if record.State.Terminal() && len(page) < maxStreamBatch {
			return
		}
		if len(page) == maxStreamBatch {
			page, err = runtime.config.Store.ListOutbox(ctx, runID, claims.AttemptID, after, maxStreamBatch)
			if err != nil {
				return
			}
			continue
		}
		timer := time.NewTimer(runtime.config.StreamPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if runtime.now().Sub(lastHeartbeat) >= runtime.config.StreamHeartbeatInterval {
			if setStreamWriteDeadline(controller) != nil {
				return
			}
			if _, err = fmt.Fprint(response, ": heartbeat\n\n"); err != nil || controller.Flush() != nil {
				return
			}
			lastHeartbeat = runtime.now()
		}
		page, err = runtime.config.Store.ListOutbox(ctx, runID, claims.AttemptID, after, maxStreamBatch)
		if err != nil {
			return
		}
	}
}

type directWireEvent struct {
	Type  string
	Bytes []byte
}

func (runtime *Runtime) directEnvelope(inbox InboxRecord, record OutboxRecord) (directWireEvent, error) {
	if record.Sequence == 0 || record.Sequence > maxSafeInteger || record.RunID != inbox.RunID || record.AttemptID != inbox.AttemptID || !jsonObject(record.Envelope) {
		return directWireEvent{}, errors.New("invalid outbox event")
	}
	if decoded, err := streamwire.DecodeStreamEvent(record.Envelope); err == nil {
		if uint64(decoded.Producersequence) != record.Sequence || decoded.Runsequence != nil || string(decoded.Runid) != inbox.RunID || string(decoded.Attemptid) != inbox.AttemptID || decoded.Type != record.EventType {
			return directWireEvent{}, errors.New("outbox envelope mismatch")
		}
		encoded, encodeErr := streamwire.EncodeStreamEvent(decoded)
		return directWireEvent{Type: decoded.Type, Bytes: encoded}, encodeErr
	}
	eventType := canonicalEventType(record.EventType)
	schema := eventSchema(eventType)
	if eventType == "" || schema == "" {
		return directWireEvent{}, errors.New("invalid outbox event type")
	}
	var data map[string]json.RawMessage
	if err := decodeStrict(record.Envelope, &data); err != nil || data == nil {
		return directWireEvent{}, errors.New("invalid outbox event data")
	}
	event := streamwire.StreamEvent{
		Specversion: "1.0", ID: streamwire.EventId(canonicalEventID(record)), Source: streamwire.URIReference("https://runtime.arop.invalid/instances/" + inbox.InstanceID),
		Type: eventType, Subject: "runs/" + inbox.RunID, Time: streamwire.DateTime(record.CreatedAt.UTC().Format(time.RFC3339Nano)),
		Datacontenttype: "application/json", Dataschema: streamwire.URIReference(schema), Runid: streamwire.RunId(inbox.RunID), Attemptid: streamwire.AttemptId(inbox.AttemptID),
		Producersequence: streamwire.PositiveSafeInteger(record.Sequence), Traceparent: streamwire.Traceparent(inbox.Traceparent), Data: data,
	}
	encoded, err := streamwire.EncodeStreamEvent(event)
	return directWireEvent{Type: eventType, Bytes: encoded}, err
}

func canonicalEventType(value string) string {
	if strings.HasPrefix(value, "io.kinglucky.arop.") {
		return value
	}
	if strings.HasPrefix(value, "arop.run.") {
		return "io.kinglucky.arop.run." + strings.TrimPrefix(value, "arop.run.") + ".v1"
	}
	return ""
}

func eventSchema(eventType string) string {
	for _, category := range []string{"run", "output", "progress", "usage"} {
		if strings.HasPrefix(eventType, "io.kinglucky.arop."+category+".") {
			name := category
			if category == "run" {
				name = "lifecycle"
			}
			return "https://arop.invalid/schemas/v1/events/" + name + "-events-v1.schema.json"
		}
	}
	return ""
}

func canonicalEventID(record OutboxRecord) string {
	if validPrefixed("evt_", record.EventID) {
		return record.EventID
	}
	digest := sha256.Sum256([]byte(record.RunID + "\x00" + record.AttemptID + "\x00" + strconv.FormatUint(record.Sequence, 10) + "\x00" + record.EventID))
	value := digest[:16]
	value[6] = value[6]&0x0f | 0x70
	value[8] = value[8]&0x3f | 0x80
	hexValue := hex.EncodeToString(value)
	return "evt_" + hexValue[:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:]
}

func (runtime *Runtime) latestOutboxSequence(ctx context.Context, runID, attemptID string) (uint64, error) {
	var after uint64
	for {
		page, err := runtime.config.Store.ListOutbox(ctx, runID, attemptID, after, maxStreamBatch)
		if err != nil {
			return 0, err
		}
		for _, record := range page {
			if record.Sequence != after+1 {
				return 0, errors.New("outbox sequence gap")
			}
			after = record.Sequence
		}
		if len(page) < maxStreamBatch {
			return after, nil
		}
	}
}

func streamCursor(header http.Header) (uint64, bool) {
	values, exists := header[http.CanonicalHeaderKey("Last-Event-ID")]
	if !exists {
		return 0, true
	}
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.HasPrefix(values[0], "+") || len(values[0]) > 16 {
		return 0, false
	}
	value, err := strconv.ParseUint(values[0], 10, 64)
	return value, err == nil && value <= maxSafeInteger
}

func streamContext(parent context.Context, now, expires time.Time) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && !expires.Before(deadline) {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, expires.Sub(now))
}

func setStreamWriteDeadline(controller *http.ResponseController) error {
	err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeCursorExpired(response http.ResponseWriter, runID string, first uint64) {
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(response).Encode(map[string]any{"category": "conflict", "code": "STREAM_CURSOR_EXPIRED", "message": "stream cursor expired", "retryable": false, "first_available_sequence": first, "snapshot_url": "/v1/runs/" + runID})
}

func jsonObject(value []byte) bool {
	var object map[string]json.RawMessage
	return decodeStrict(value, &object) == nil && object != nil
}

func decodeStrict(value []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err == nil {
		return errors.New("trailing JSON value")
	} else {
		return err
	}
}
