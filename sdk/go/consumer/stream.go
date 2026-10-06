package consumer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	dispatchwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

const (
	maxSSELineBytes  = 1 << 20
	maxSSEEventBytes = 8 << 20
)

type StreamMode string

const (
	StreamRelay  StreamMode = "relay"
	StreamDirect StreamMode = "direct"
)

type StreamOptions struct {
	Mode          StreamMode
	After         uint64
	MaxReconnects int
}

type CursorExpiredError struct {
	LatestSequence uint64
	SnapshotURL    string
}

func (failure *CursorExpiredError) Error() string { return "stream cursor expired" }

type EventHandler func(context.Context, streamwire.StreamEvent) error

func (client *Client) Stream(ctx context.Context, ticket dispatchwire.DispatchTicket, options StreamOptions, handler EventHandler) error {
	if handler == nil || !validRunID(string(ticket.RunID)) || options.After > 9007199254740991 || options.MaxReconnects < 0 || options.MaxReconnects > 100 {
		return errors.New("invalid stream request")
	}
	mode := options.Mode
	if mode == "" {
		mode = StreamRelay
	}
	if mode != StreamRelay && mode != StreamDirect {
		return errors.New("unsupported stream mode")
	}
	after := options.After
	var last []byte
	for reconnect := 0; ; reconnect++ {
		terminal, next, wire, err := client.streamOnce(ctx, ticket, mode, after, last, handler)
		if err != nil {
			return err
		}
		after, last = next, wire
		if terminal {
			return nil
		}
		if reconnect >= options.MaxReconnects {
			return errors.New("stream ended before terminal event")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}

func (client *Client) streamOnce(ctx context.Context, ticket dispatchwire.DispatchTicket, mode StreamMode, after uint64, last []byte, handler EventHandler) (bool, uint64, []byte, error) {
	target, token, err := client.streamTarget(ctx, ticket, mode)
	if err != nil {
		return false, after, last, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, after, last, errors.New("build stream request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	if after != 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	}
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, ErrUnsafeEndpoint) {
			return false, after, last, ErrUnsafeEndpoint
		}
		return false, after, last, errors.New("stream request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusGone {
		failure, decodeErr := decodeCursorExpired(response.Body)
		return false, after, last, firstError(decodeErr, failure)
	}
	if response.StatusCode != http.StatusOK || !singleContentType(response.Header, "text/event-stream") || response.Header.Get("Cache-Control") != "no-store" {
		body, _ := bounded(response.Body)
		return false, after, last, decodeRemoteError(response.StatusCode, body)
	}
	parser := newSSEParser(response.Body)
	for {
		frame, parseErr := parser.Next()
		if errors.Is(parseErr, io.EOF) {
			return false, after, last, nil
		}
		if parseErr != nil {
			return false, after, last, errors.New("invalid event stream")
		}
		if frame.heartbeat {
			continue
		}
		sequence, parseErr := strconv.ParseUint(frame.id, 10, 64)
		if parseErr != nil || sequence == 0 || sequence > 9007199254740991 {
			return false, after, last, errors.New("invalid stream sequence")
		}
		if sequence <= after {
			if sequence == after && bytes.Equal(frame.data, last) {
				continue
			}
			return false, after, last, errors.New("conflicting stream replay")
		}
		if sequence != after+1 {
			return false, after, last, errors.New("stream sequence gap")
		}
		event, decodeErr := streamwire.DecodeStreamEvent(frame.data)
		if decodeErr != nil || event.Type != frame.event || string(event.Runid) != string(ticket.RunID) {
			return false, after, last, errors.New("invalid stream event")
		}
		if mode == StreamRelay {
			if event.Runsequence == nil || uint64(*event.Runsequence) != sequence {
				return false, after, last, errors.New("invalid relay sequence domain")
			}
		} else if event.Runsequence != nil || uint64(event.Producersequence) != sequence || string(event.Attemptid) != string(ticket.AttemptID) {
			return false, after, last, errors.New("invalid direct sequence domain")
		}
		if err = handler(ctx, event); err != nil {
			return false, after, last, err
		}
		after, last = sequence, bytes.Clone(frame.data)
		if terminalEvent(event.Type) {
			return true, after, last, nil
		}
	}
}

func (client *Client) streamTarget(ctx context.Context, ticket dispatchwire.DispatchTicket, mode StreamMode) (string, string, error) {
	if mode == StreamDirect {
		if ticket.Delivery.Mode != "direct" || ticket.Delivery.StreamEndpoint == nil || !validBearer(ticket.RunToken) {
			return "", "", errors.New("direct streaming unavailable")
		}
		expires, err := time.Parse(time.RFC3339Nano, string(ticket.Delivery.ExpiresAt))
		if err != nil || !client.clock().UTC().Before(expires) {
			return "", "", ErrTicketExpired
		}
		target := string(*ticket.Delivery.StreamEndpoint)
		parsed, err := parseHTTPSURL(target)
		if err != nil || parsed.RawQuery != "" || parsed.Path != "/v1/runs/"+string(ticket.RunID)+"/events" {
			return "", "", ErrUnsafeEndpoint
		}
		return target, ticket.RunToken, nil
	}
	token, err := client.tokens.Token(ctx)
	if err != nil || !validBearer(token) {
		return "", "", errors.New("control plane authentication unavailable")
	}
	target := *client.base
	target.Path = "/v1/agent-runs/" + string(ticket.RunID) + "/events"
	return target.String(), token, nil
}

func terminalEvent(eventType string) bool {
	switch eventType {
	case "io.arop.run.succeeded.v1", "io.arop.run.failed.v1", "io.arop.run.cancelled.v1", "io.arop.run.timed_out.v1":
		return true
	default:
		return false
	}
}

type sseFrame struct {
	id, event string
	data      []byte
	heartbeat bool
}

type sseParser struct{ reader *bufio.Reader }

func newSSEParser(reader io.Reader) *sseParser {
	return &sseParser{reader: bufio.NewReaderSize(reader, 64<<10)}
}

func (parser *sseParser) Next() (sseFrame, error) {
	frame := sseFrame{}
	var size int
	seen := map[string]bool{}
	for {
		line, err := parser.reader.ReadString('\n')
		if len(line) > maxSSELineBytes || size+len(line) > maxSSEEventBytes {
			return sseFrame{}, errors.New("SSE frame exceeds limit")
		}
		size += len(line)
		if err != nil && !errors.Is(err, io.EOF) {
			return sseFrame{}, err
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			if size == 0 {
				return sseFrame{}, io.EOF
			}
			return sseFrame{}, errors.New("unterminated SSE frame")
		}
		if !strings.HasSuffix(line, "\n") {
			return sseFrame{}, errors.New("unterminated SSE line")
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if frame.heartbeat && len(seen) == 0 {
				return frame, nil
			}
			if frame.id == "" || frame.event == "" || len(frame.data) == 0 || len(seen) != 3 {
				return sseFrame{}, errors.New("incomplete SSE frame")
			}
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			if len(seen) != 0 {
				return sseFrame{}, errors.New("mixed SSE heartbeat")
			}
			frame.heartbeat = true
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok || seen[name] || frame.heartbeat {
			return sseFrame{}, errors.New("invalid SSE field")
		}
		seen[name] = true
		switch name {
		case "id":
			frame.id = value
		case "event":
			frame.event = value
		case "data":
			frame.data = []byte(value)
		default:
			return sseFrame{}, errors.New("unknown SSE field")
		}
	}
}

func decodeCursorExpired(reader io.Reader) (*CursorExpiredError, error) {
	body, err := bounded(reader)
	if err != nil {
		return nil, err
	}
	var value struct {
		Category               string `json:"category,omitempty"`
		Code                   string `json:"code"`
		Message                string `json:"message,omitempty"`
		Retryable              *bool  `json:"retryable,omitempty"`
		LatestSequence         uint64 `json:"latest_sequence"`
		FirstAvailableSequence uint64 `json:"first_available_sequence,omitempty"`
		SnapshotURL            string `json:"snapshot_url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF || value.Code != "STREAM_CURSOR_EXPIRED" || value.LatestSequence > 9007199254740991 || !safeSnapshotURL(value.SnapshotURL) {
		return nil, errors.New("invalid cursor expiry response")
	}
	return &CursorExpiredError{LatestSequence: value.LatestSequence, SnapshotURL: value.SnapshotURL}, nil
}

func safeSnapshotURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && !parsed.IsAbs() && parsed.Host == "" && parsed.User == nil && strings.HasPrefix(parsed.Path, "/v1/") && parsed.RawQuery == "" && parsed.Fragment == ""
}

func firstError(err error, value error) error {
	if err != nil {
		return err
	}
	return value
}

func AssembleUTF8Delta(current []byte, offset uint64, delta string) ([]byte, error) {
	if offset != uint64(len(current)) || !utf8.Valid(current) || !utf8.ValidString(delta) {
		return nil, fmt.Errorf("invalid UTF-8 byte offset")
	}
	return append(bytes.Clone(current), []byte(delta)...), nil
}
