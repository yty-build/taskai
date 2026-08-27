package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

const (
	appMethodAgentMessageDelta = "item/agentMessage/delta"
)

// ConnectManager binds the client's sole owned thread to the live,
// provider-agnostic runtime port. It performs no provider request and cannot
// create, resume, or replace a thread.
func (c *CodexAppServerClient) ConnectManager(reference replay.OpaqueRef) (ManagerConnection, error) {
	if c == nil {
		return nil, ErrProviderUnsupported
	}
	threadID, err := replay.ResolveCodexReference(reference)
	if err != nil {
		return nil, ErrProviderUnsupported
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized || c.closed || c.ownedThread != threadID {
		return nil, ErrOwnership
	}
	if c.connection != nil {
		if !bytes.Equal(c.connection.reference.Bytes(), reference.Bytes()) {
			return nil, ErrOwnership
		}
		return c.connection, nil
	}
	c.connection = &codexManagerConnection{
		client:    c,
		reference: replay.OpaqueRefFromBytes(reference.Bytes()),
	}
	return c.connection, nil
}

type codexManagerConnection struct {
	client    *CodexAppServerClient
	reference replay.OpaqueRef

	mu     sync.Mutex
	active *CodexTurnHandle
}

func (c *codexManagerConnection) NativeReference() replay.OpaqueRef {
	if c == nil {
		return replay.OpaqueRef{}
	}
	return replay.OpaqueRefFromBytes(c.reference.Bytes())
}

func (c *codexManagerConnection) Prompt(
	ctx context.Context,
	prompt string,
	onFrame func(json.RawMessage) error,
) (json.RawMessage, error) {
	if c == nil || c.client == nil || onFrame == nil {
		return nil, ErrInvalidOperation
	}
	handle, err := c.client.TurnStart(ctx, c.reference, prompt, func(_ context.Context, frame json.RawMessage) error {
		return onFrame(append(json.RawMessage(nil), frame...))
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.active != nil {
		c.mu.Unlock()
		return nil, ErrChildBusy
	}
	c.active = handle
	c.mu.Unlock()
	result, waitErr := handle.Wait(ctx)
	if waitErr != nil {
		select {
		case <-handle.state.done:
			c.clearActive(handle)
		case <-c.client.peer.done:
			c.clearActive(handle)
		default:
			go func() {
				_, _ = handle.Wait(context.Background())
				c.clearActive(handle)
			}()
		}
		return nil, waitErr
	}
	c.clearActive(handle)
	return result, nil
}

func (c *codexManagerConnection) clearActive(handle *CodexTurnHandle) {
	c.mu.Lock()
	if c.active == handle {
		c.active = nil
	}
	c.mu.Unlock()
}

func (c *codexManagerConnection) Interrupt(ctx context.Context) error {
	if c == nil {
		return ErrStaleTurn
	}
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return ErrStaleTurn
	}
	return active.Interrupt(ctx)
}

func (c *codexManagerConnection) ReadNativeSession(ctx context.Context) (json.RawMessage, error) {
	if c == nil || c.client == nil {
		return nil, ErrProviderUnsupported
	}
	threadID, err := replay.ResolveCodexReference(c.reference)
	if err != nil {
		return nil, ErrProviderUnsupported
	}
	return c.client.ThreadRead(ctx, threadID)
}

func (c *codexManagerConnection) Done() <-chan struct{} {
	if c == nil || c.client == nil || c.client.peer == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return c.client.peer.done
}

func (c *codexManagerConnection) Err() error {
	if c == nil || c.client == nil || c.client.peer == nil {
		return &BoundaryError{Stage: "manager transport", Err: ErrTransport}
	}
	if c.client.peer.errValue() == nil {
		return nil
	}
	return &BoundaryError{Stage: "manager transport", Err: ErrTransport}
}

func (c *codexManagerConnection) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

func managerPublicSessionID(reference replay.OpaqueRef) string {
	digest := sha256.Sum256(reference.Bytes())
	return "manager_" + hex.EncodeToString(digest[:16])
}

func managerCompletion(status string) (json.RawMessage, error) {
	switch status {
	case "completed":
		return json.RawMessage(`{"stopReason":"end_turn"}`), nil
	case "interrupted":
		return json.RawMessage(`{"stopReason":"cancelled"}`), nil
	case "failed":
		return nil, &BoundaryError{Stage: "manager completion", Err: ErrProcessFailed}
	default:
		return nil, &BoundaryError{Stage: "manager completion", Err: ErrMalformedCompletion}
	}
}

// appNotificationToACP translates the private headerless transport into one
// bounded public ACP notification. It performs only provider-boundary
// projection; no shared semantic event model is introduced.
func appNotificationToACP(publicSession string, message appServerMessage) (json.RawMessage, bool, error) {
	if publicSession == "" || message.Method == appMethodDynamicTool || message.Method == appMethodRequestDone {
		return nil, false, nil
	}
	// App Server notifications are not ACP frames. Only an exact, conformed
	// mapping may cross the public port; unknown/private native events stay
	// inside the provider adapter rather than becoming a custom ACP protocol.
	if message.Method != appMethodAgentMessageDelta {
		return nil, false, nil
	}
	params, dynamic, err := sanitizePublicAppParams(message.Params)
	if err != nil {
		return nil, false, err
	}
	if dynamic {
		return nil, false, nil
	}

	var delta struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
	}
	if json.Unmarshal(params, &delta) != nil || delta.Delta == "" {
		return nil, false, ErrTransport
	}
	update, err := json.Marshal(struct {
		SessionUpdate string `json:"sessionUpdate"`
		MessageID     string `json:"messageId,omitempty"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		SessionUpdate: "agent_message_chunk",
		MessageID:     delta.ItemID,
		Content: struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: delta.Delta},
	})
	if err != nil {
		return nil, false, ErrTransport
	}
	publicParams, err := json.Marshal(struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}{SessionID: publicSession, Update: update})
	if err != nil {
		return nil, false, ErrTransport
	}
	frame, err := acp.Notification(acp.MethodSessionUpdate, publicParams).Bytes()
	if err != nil || len(frame) > acp.MaxFrameBytes {
		return nil, false, ErrTransport
	}
	return json.RawMessage(frame), true, nil
}

func sanitizePublicAppParams(raw json.RawMessage) (json.RawMessage, bool, error) {
	if !validJSONObject(raw) {
		return nil, false, ErrTransport
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false, ErrTransport
	}
	if containsDynamicTool(value) {
		return nil, true, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false, ErrTransport
	}
	stripPrivateThreadIDs(object)
	sanitized, err := json.Marshal(object)
	if err != nil {
		return nil, false, ErrTransport
	}
	return sanitized, false, nil
}

func containsDynamicTool(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if kind, ok := typed["type"].(string); ok && kind == "dynamicToolCall" {
			return true
		}
		if tool, ok := typed["tool"].(string); ok && tool == DelegateToolName {
			return true
		}
		if name, ok := typed["name"].(string); ok && name == DelegateToolName {
			return true
		}
		for _, child := range typed {
			if containsDynamicTool(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsDynamicTool(child) {
				return true
			}
		}
	}
	return false
}

func stripPrivateThreadIDs(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "threadId")
		for _, child := range typed {
			stripPrivateThreadIDs(child)
		}
	case []any:
		for _, child := range typed {
			stripPrivateThreadIDs(child)
		}
	}
}

var _ ManagerConnection = (*codexManagerConnection)(nil)
