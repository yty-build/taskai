package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func TestActivityRawAndNormalized(t *testing.T) {
	event, _ := json.Marshal(acp.Event{
		EventVersion: 1, RequestID: "req", Seq: 1, Type: "agent_message_chunk",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}}`),
	})
	line := sessionlog.Line{Seq: 4, Timestamp: time.Date(2026, 1, 1, 1, 2, 3, 0, time.Local), RequestID: "req", Type: "acp", Data: event}
	if got := Activity(line, false); !strings.Contains(got, "[req] hello") {
		t.Fatalf("normalized = %q", got)
	}
	var decoded sessionlog.Line
	if err := json.Unmarshal([]byte(Activity(line, true)), &decoded); err != nil || decoded.Seq != 4 {
		t.Fatalf("raw output did not round trip: %#v, %v", decoded, err)
	}
}

func TestActivityNormalizesACPVisibleKinds(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		raw       string
		want      string
	}{
		{
			name: "reasoning", eventType: "agent_thought_chunk",
			raw:  `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"checking constraints"}}}}`,
			want: "reasoning: checking constraints",
		},
		{
			name: "command", eventType: "tool_call",
			raw:  `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"go test ./...","kind":"execute","status":"in_progress"}}}`,
			want: "tool execute in progress: go test ./...",
		},
		{
			name: "result", eventType: "tool_call_update",
			raw:  `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","title":"tests","status":"completed","content":[{"type":"content","content":{"type":"text","text":"PASS"}}]}}}`,
			want: "tool completed: tests — PASS",
		},
		{
			name: "diff", eventType: "tool_call_update",
			raw:  `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-2","title":"edit file","status":"completed","content":[{"type":"diff","path":"main.go","oldText":"a","newText":"b"}]}}}`,
			want: "diff main.go",
		},
		{
			name: "permission", eventType: "session_request_permission",
			raw:  `{"jsonrpc":"2.0","id":8,"method":"session/request_permission","params":{"toolCall":{"toolCallId":"call-3","title":"write config"},"options":[{"optionId":"yes","name":"Allow once","kind":"allow_once"}]}}`,
			want: "permission requested: write config (Allow once)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, _ := json.Marshal(acp.Event{EventVersion: 1, RequestID: "req", Type: test.eventType, Raw: json.RawMessage(test.raw)})
			line := sessionlog.Line{Seq: 1, Type: "acp", Data: event}
			if got := Activity(line, false); !strings.Contains(got, test.want) {
				t.Fatalf("Activity() = %q, want substring %q", got, test.want)
			}
		})
	}
}
