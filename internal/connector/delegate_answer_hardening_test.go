package connector

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestAnswerCollectorFailsClosedWithoutConformedMessageIdentity(t *testing.T) {
	frames := []json.RawMessage{
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"owned","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ambiguous"}}}}`),
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"owned","update":{"sessionUpdate":"agent_message_chunk","messageId":"","content":{"type":"text","text":"ambiguous"}}}}`),
	}
	for index, frame := range frames {
		collector := answerCollector{}
		if err := collector.add(frame); !errors.Is(err, ErrMalformedCompletion) {
			t.Fatalf("frame %d error = %v, want malformed completion", index, err)
		}
		if len(collector.frames()) != 0 {
			t.Fatalf("frame %d retained ambiguous answer", index)
		}
	}
}
