package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func TestActivityRendersChannelFramesWithoutTranscriptProjection(t *testing.T) {
	tests := []struct {
		line sessionlog.Line
		want string
	}{
		{sessionlog.Line{Seq: 1, Type: "connection", Data: json.RawMessage(`{"state":"connected"}`)}, "connection: connected"},
		{sessionlog.Line{Seq: 2, RequestID: "req", Type: "client_command", Data: json.RawMessage(`{"command":"prompt","text":"hello"}`)}, "user: hello"},
		{sessionlog.Line{Seq: 3, RequestID: "req", Type: "provider_event", Data: json.RawMessage(`{"type":"text_delta","stream":"output","text":"world"}`)}, "world"},
		{sessionlog.Line{Seq: 4, RequestID: "req", Type: "provider_event", Data: json.RawMessage(`{"type":"tool_call","title":"go test ./...","status":"in_progress"}`)}, "tool in progress: go test ./..."},
		{sessionlog.Line{Seq: 5, RequestID: "req", Type: "permission_requested", Data: json.RawMessage(`{"id":"p1","title":"write config","options":[{"id":"yes","name":"Allow once"}]}`)}, "permission requested: write config (Allow once)"},
		{sessionlog.Line{Seq: 6, RequestID: "req", Type: "provider_result", Data: json.RawMessage(`{"status":"completed","stop_reason":"end_turn"}`)}, "provider result: completed (end_turn)"},
	}
	for _, test := range tests {
		if got := Activity(test.line, false); !strings.Contains(got, test.want) {
			t.Fatalf("Activity(%s) = %q, want substring %q", test.line.Type, got, test.want)
		}
	}
}

func TestActivityRawRoundTripsFrame(t *testing.T) {
	line := sessionlog.Line{Seq: 4, Timestamp: time.Date(2026, 1, 1, 1, 2, 3, 0, time.Local), RequestID: "req", Type: "provider_event", Data: json.RawMessage(`{"type":"text_delta","text":"hello"}`)}
	var decoded sessionlog.Line
	if err := json.Unmarshal([]byte(Activity(line, true)), &decoded); err != nil || decoded.Seq != line.Seq || decoded.Type != line.Type {
		t.Fatalf("raw output did not round trip: %#v, %v", decoded, err)
	}
}

func TestActivityNonRawCannotEmitTerminalControlsOrForgeLines(t *testing.T) {
	line := sessionlog.Line{
		Seq:       9,
		RequestID: "REQ-\x1b[2J\u202e",
		Type:      "provider_event",
		Data:      json.RawMessage(`{"type":"text_delta","stream":"output","text":"first\r\nsecond\u001b]0;owned\u0007\u009b31m\u202ethird"}`),
	}
	got := Activity(line, false)
	if strings.ContainsAny(got, "\r\n\t\x1b\a") || strings.ContainsRune(got, '\u009b') || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("non-raw activity retained terminal control data: %q", got)
	}
	if !strings.Contains(got, "first  second") || !strings.Contains(got, "third") {
		t.Fatalf("non-raw activity lost printable content: %q", got)
	}
}
