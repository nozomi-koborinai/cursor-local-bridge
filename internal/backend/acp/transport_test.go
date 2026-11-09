package acp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

func TestTransportErrorChunkIsItsOwnEvent(t *testing.T) {
	sink := &recorder{}
	s := New(fakeAgent, sink)
	t.Cleanup(func() { _ = s.Close() })

	const text = "RetriableError: http/2 stream closed"
	err := client{s}.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.UpdateAgentMessageText(text),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	ev := events[0]
	if ev.Type != backend.EventTransportError || ev.Text != "" {
		t.Fatalf("event = %+v, want transport_error with empty text", ev)
	}
	if !strings.Contains(string(ev.Data), text) || !strings.Contains(string(ev.Data), "agent_message_chunk") {
		t.Fatalf("data = %s, want the original chunk", ev.Data)
	}
	if !json.Valid(ev.Data) {
		t.Fatalf("data is not JSON: %s", ev.Data)
	}
}

func TestOrdinaryMessageChunkKeepsItsText(t *testing.T) {
	sink := &recorder{}
	s := New(fakeAgent, sink)
	t.Cleanup(func() { _ = s.Close() })

	const text = "I'll add the flag"
	err := (client{s}).SessionUpdate(context.Background(), acpsdk.SessionNotification{
		Update: acpsdk.UpdateAgentMessageText(text),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	if len(events) != 1 || events[0].Type != "agent_message_chunk" || events[0].Text != text {
		t.Fatalf("events = %+v", events)
	}
}
