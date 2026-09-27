package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dispatchwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/dispatch"
	streamwire "github.com/gmslll/agent-runtime-operations-protocol/sdk/go/generated/streaming"
)

func TestRelayStreamReconnectsWithRunSequence(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		call := requests.Add(1)
		if request.URL.Path != "/v1/agent-runs/"+testRunID()+"/events" || request.Header.Get("Authorization") != "Bearer control-token-000000" || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-store")
		if call == 1 {
			writeSSE(t, response, 1, relayEvent(t, 1, "io.kinglucky.arop.output.delta.v1"))
			return
		}
		if request.Header.Get("Last-Event-ID") != "1" {
			t.Fatalf("cursor=%q", request.Header.Get("Last-Event-ID"))
		}
		writeSSE(t, response, 2, relayEvent(t, 2, "io.kinglucky.arop.run.succeeded.v1"))
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "relay")
	var sequences []uint64
	err := client.Stream(context.Background(), streamTicket(server.URL, false), StreamOptions{Mode: StreamRelay, MaxReconnects: 1}, func(_ context.Context, event streamwire.StreamEvent) error {
		sequences = append(sequences, uint64(*event.Runsequence))
		return nil
	})
	if err != nil || fmt.Sprint(sequences) != "[1 2]" || requests.Load() != 2 {
		t.Fatalf("sequences=%v requests=%d err=%v", sequences, requests.Load(), err)
	}
}

func TestDirectStreamUsesTicketCapabilityAndRejectsSequenceConfusion(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testRunToken() || request.URL.Path != "/v1/runs/"+testRunID()+"/events" {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-store")
		writeSSE(t, response, 1, directEvent(t, 1, "io.kinglucky.arop.run.succeeded.v1"))
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "direct")
	called := 0
	err := client.Stream(context.Background(), streamTicket(server.URL, true), StreamOptions{Mode: StreamDirect}, func(_ context.Context, _ streamwire.StreamEvent) error { called++; return nil })
	if err != nil || called != 1 {
		t.Fatalf("called=%d err=%v", called, err)
	}

	bad := relayEvent(t, 1, "io.kinglucky.arop.run.succeeded.v1")
	badServer := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-store")
		writeSSE(t, response, 1, bad)
	}))
	defer badServer.Close()
	client = testClient(t, badServer, &countingResolver{}, allowAll{}, "direct")
	err = client.Stream(context.Background(), streamTicket(badServer.URL, true), StreamOptions{Mode: StreamDirect}, func(context.Context, streamwire.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "direct sequence") {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamCursorExpiredAndMalformedFramesFailClosed(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(http.StatusGone)
		_, _ = response.Write([]byte(`{"code":"STREAM_CURSOR_EXPIRED","latest_sequence":9,"snapshot_url":"/v1/agent-runs/` + testRunID() + `"}`))
	}))
	defer server.Close()
	client := testClient(t, server, &countingResolver{}, allowAll{}, "relay")
	err := client.Stream(context.Background(), streamTicket(server.URL, false), StreamOptions{Mode: StreamRelay, After: 2}, func(context.Context, streamwire.StreamEvent) error { return nil })
	var expired *CursorExpiredError
	if !errors.As(err, &expired) || expired.LatestSequence != 9 {
		t.Fatalf("err=%#v", err)
	}

	malformed := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write([]byte("id: 1\nid: 1\nevent: x\ndata: {}\n\n"))
	}))
	defer malformed.Close()
	client = testClient(t, malformed, &countingResolver{}, allowAll{}, "relay")
	err = client.Stream(context.Background(), streamTicket(malformed.URL, false), StreamOptions{}, func(context.Context, streamwire.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "invalid event stream") {
		t.Fatalf("err=%v", err)
	}
}

func TestAssembleUTF8DeltaUsesByteOffsets(t *testing.T) {
	value, err := AssembleUTF8Delta(nil, 0, "商品")
	if err != nil || len(value) != 6 {
		t.Fatalf("value=%q len=%d err=%v", value, len(value), err)
	}
	value, err = AssembleUTF8Delta(value, 6, "🙂e\u0301")
	if err != nil || len(value) != 13 {
		t.Fatalf("value=%q len=%d err=%v", value, len(value), err)
	}
	if _, err = AssembleUTF8Delta(value, 7, "x"); err == nil {
		t.Fatal("UTF-16/code-point style offset accepted")
	}
}

func streamTicket(base string, direct bool) dispatchwire.DispatchTicket {
	ticket := dispatchwire.DispatchTicket{RunID: dispatchwire.RunId(testRunID()), AttemptID: dispatchwire.AttemptId(testAttemptID()), RunToken: testRunToken()}
	ticket.Delivery.ExpiresAt = dispatchwire.DateTime(consumerNow.Add(time.Minute).Format(time.RFC3339Nano))
	if direct {
		ticket.Delivery.Mode = "direct"
		endpoint := dispatchwire.URIReference(base + "/v1/runs/" + testRunID() + "/events")
		ticket.Delivery.StreamEndpoint = &endpoint
	} else {
		ticket.Delivery.Mode = "proxy"
	}
	return ticket
}

func relayEvent(t *testing.T, sequence uint64, eventType string) streamwire.StreamEvent {
	t.Helper()
	value := directEvent(t, sequence, eventType)
	runSequence := streamwire.PositiveSafeInteger(sequence)
	value.Runsequence = &runSequence
	return value
}

func directEvent(t *testing.T, sequence uint64, eventType string) streamwire.StreamEvent {
	t.Helper()
	data := map[string]json.RawMessage{"state": json.RawMessage(`"succeeded"`)}
	return streamwire.StreamEvent{
		Specversion: "1.0", ID: "evt_01999999-9999-7999-8999-999999999996", Source: "https://runtime.example.invalid/instances/runtime-a", Type: eventType,
		Subject: "runs/" + testRunID(), Time: "2026-09-27T07:00:00Z", Datacontenttype: "application/json", Dataschema: "https://arop.invalid/schemas/v1/events/lifecycle-events-v1.schema.json",
		Runid: streamwire.RunId(testRunID()), Attemptid: streamwire.AttemptId(testAttemptID()), Producersequence: streamwire.PositiveSafeInteger(sequence), Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", Data: data,
	}
}

func writeSSE(t *testing.T, response http.ResponseWriter, sequence uint64, event streamwire.StreamEvent) {
	t.Helper()
	encoded, err := streamwire.EncodeStreamEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(response, "id: %d\nevent: %s\ndata: %s\n\n", sequence, event.Type, encoded)
}
