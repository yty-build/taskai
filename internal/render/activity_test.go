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
