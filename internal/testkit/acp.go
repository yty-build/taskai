package testkit

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/yty-build/acp-gateway/internal/acp"
)

// ACPAgent is a deterministic protocol-conforming fake agent. It records only
// method names and Session IDs unless the owning test explicitly retains its
// synthetic in-memory prompt fixture.
type ACPAgent struct {
	Agent *acp.Agent

	SessionID string
	Frames    []json.RawMessage
	Terminal  json.RawMessage

	mu      sync.Mutex
	Methods []string
	cancel  chan struct{}
}

func NewACPAgent(r interface{ Read([]byte) (int, error) }, w interface{ Write([]byte) (int, error) }, sessionID string, frames []json.RawMessage, terminal json.RawMessage) *ACPAgent {
	fake := &ACPAgent{
		SessionID: sessionID,
		Frames:    cloneRaw(frames),
		Terminal:  append(json.RawMessage(nil), terminal...),
		cancel:    make(chan struct{}),
	}
	fake.Agent = acp.NewAgent(r, w, acp.HandlerFuncs{
		Request:      fake.request,
		Notification: fake.notification,
	})
	return fake
}

func (f *ACPAgent) request(_ context.Context, message acp.Message) (json.RawMessage, *acp.ErrorObject) {
	f.record(message.Method)
	switch message.Method {
	case acp.MethodInitialize:
		return json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"resume":true}}}`), nil
	case acp.MethodSessionNew:
		value, _ := json.Marshal(map[string]string{"sessionId": f.SessionID})
		return value, nil
	case acp.MethodSessionResume:
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(message.Params, &params) != nil || params.SessionID != f.SessionID {
			return nil, &acp.ErrorObject{Code: -32602, Message: "invalid resume target"}
		}
		return json.RawMessage(`{}`), nil
	case acp.MethodSessionPrompt:
		for _, frame := range f.Frames {
			parsed, err := acp.ParseMessage(frame)
			if err != nil {
				return nil, &acp.ErrorObject{Code: -32603, Message: "invalid fixture frame"}
			}
			if err := f.Agent.Notify(parsed.Method, parsed.Params); err != nil {
				return nil, &acp.ErrorObject{Code: -32000, Message: "fixture transport"}
			}
		}
		select {
		case <-f.cancel:
			return json.RawMessage(`{"stopReason":"cancelled"}`), nil
		default:
		}
		if len(f.Terminal) == 0 {
			return json.RawMessage(`{"stopReason":"end_turn"}`), nil
		}
		return append(json.RawMessage(nil), f.Terminal...), nil
	default:
		return nil, &acp.ErrorObject{Code: -32601, Message: "method not supported"}
	}
}

func (f *ACPAgent) notification(_ context.Context, message acp.Message) error {
	f.record(message.Method)
	if message.Method == acp.MethodSessionCancel {
		select {
		case <-f.cancel:
		default:
			close(f.cancel)
		}
	}
	return nil
}

func (f *ACPAgent) record(method string) {
	f.mu.Lock()
	f.Methods = append(f.Methods, method)
	f.mu.Unlock()
}

func (f *ACPAgent) MethodSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Methods...)
}

func cloneRaw(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for i, value := range values {
		result[i] = append(json.RawMessage(nil), value...)
	}
	return result
}
