package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
)

// PermissionResponder is implemented by ACP transports that can keep a real
// session/request_permission call pending while a disposable viewer chooses an
// offered option.
type PermissionResponder interface {
	DecidePermission(context.Context, string, string, bool) error
}

// RuntimeClient is one long-lived public acpx/runtime connection per Session
// worker. It does not use acpx's detached queue-owner CLI, because that process
// cannot receive permission decisions from an attached Task Console viewer.
type RuntimeClient struct {
	binary         string
	stateDirectory string
	verified       atomic.Bool

	mu     sync.Mutex
	bridge *runtimeBridge
	target *Target
	closed bool
}

func NewRuntimeClient(binary, stateDirectory string) *RuntimeClient {
	return &RuntimeClient{binary: binary, stateDirectory: stateDirectory}
}

func (c *RuntimeClient) Probe(ctx context.Context) error {
	probe := NewClient(c.binary)
	if err := probe.Probe(ctx); err != nil {
		return err
	}
	if _, err := installedPackageRoot(c.binary); err != nil {
		return err
	}
	c.verified.Store(true)
	return nil
}

func (c *RuntimeClient) Start(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	return c.ensure(ctx, spec)
}

func (c *RuntimeClient) Resume(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	if strings.TrimSpace(spec.ResumeACPSessionID) == "" {
		return SessionInfo{}, fmt.Errorf("ACP Session ID is required to resume")
	}
	info, err := c.ensure(ctx, spec)
	if err != nil {
		return SessionInfo{}, err
	}
	if info.ACPSessionID == "" || info.ACPSessionID != spec.ResumeACPSessionID {
		return SessionInfo{}, &ProtocolError{
			Operation: "runtime resume",
			Message:   fmt.Sprintf("expected ACP Session ID %q, received %q", spec.ResumeACPSessionID, info.ACPSessionID),
		}
	}
	return info, nil
}

func (c *RuntimeClient) ensure(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return SessionInfo{}, err
	}
	bridge, err := c.bridgeFor(ctx, target)
	if err != nil {
		return SessionInfo{}, err
	}
	agentName, useBuiltin := runtimeAgentConfiguration(target.Profile)
	var payload bridgeSession
	err = bridge.call(ctx, bridgeCommand{
		Type: "ensure", SessionKey: target.ConsoleSessionID, Agent: agentName, UseBuiltin: useBuiltin,
		Argv: bridgeAgentArgv(target.Profile), Environment: cloneProfileEnvironment(target.Profile.Environment),
		CWD: target.WorkingDirectory, PermissionMode: target.Permission, ResumeSessionID: spec.ResumeACPSessionID,
	}, &payload)
	if err != nil {
		return SessionInfo{}, err
	}
	return bridgeSessionInfo(target, payload)
}

// runtimeAgentConfiguration distinguishes the friendly acpx built-in names in
// embedded profiles from user-supplied raw ACP server commands. The public
// runtime registry, rather than a native provider CLI, must expand a friendly
// name such as "codex" into its pinned ACP adapter command.
func runtimeAgentConfiguration(profile agents.Profile) (string, bool) {
	if isBuiltIn(profile) {
		return strings.TrimSpace(profile.Command), true
	}
	return profile.Name, false
}

func (c *RuntimeClient) Prompt(ctx context.Context, spec PromptSpec, emit func(Event) error) (PromptReceipt, error) {
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return PromptReceipt{}, err
	}
	if strings.TrimSpace(spec.Message) == "" {
		return PromptReceipt{}, fmt.Errorf("prompt message is required")
	}
	if spec.RequestID == "" {
		spec.RequestID, err = NewRequestID()
		if err != nil {
			return PromptReceipt{}, err
		}
	}
	if emit == nil {
		emit = func(Event) error { return nil }
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return PromptReceipt{}, err
	}
	channel, cleanup, err := bridge.start(bridgeCommand{Type: "turn", RequestID: spec.RequestID, Text: spec.Message})
	if err != nil {
		return PromptReceipt{}, err
	}
	defer cleanup()
	receipt := PromptReceipt{RequestID: spec.RequestID}
	for {
		select {
		case <-ctx.Done():
			cancelContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = bridge.call(cancelContext, bridgeCommand{Type: "cancel", RequestID: spec.RequestID, Reason: "prompt context cancelled"}, nil)
			cancel()
			return receipt, ctx.Err()
		case <-bridge.closed:
			return receipt, bridge.closedError()
		case message := <-channel:
			switch message.Type {
			case "turn_started":
				continue
			case "runtime_event":
				receipt.LastSeq++
				event, eventErr := runtimeEvent(spec, receipt.LastSeq, message.Event)
				if eventErr != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "invalid runtime event")
					return receipt, eventErr
				}
				if err := emit(event); err != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "event consumer failed")
					return receipt, fmt.Errorf("consume acpx runtime event: %w", err)
				}
			case "permission_request":
				receipt.LastSeq++
				event, eventErr := runtimePermissionEvent(spec, receipt.LastSeq, message)
				if eventErr != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "invalid permission event")
					return receipt, eventErr
				}
				if err := emit(event); err != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "permission consumer failed")
					return receipt, fmt.Errorf("consume acpx permission request: %w", err)
				}
			case "permission_abandoned":
				continue
			case "turn_result":
				return runtimeTurnResult(receipt, message.Result)
			case "error":
				return receipt, bridgeRemoteError(message)
			}
		}
	}
}

func cancelRuntimeTurn(bridge *runtimeBridge, requestID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = bridge.call(ctx, bridgeCommand{Type: "cancel", RequestID: requestID, Reason: reason}, nil)
}

func (c *RuntimeClient) Cancel(ctx context.Context, target Target) error {
	target, err := c.readyTarget(target)
	if err != nil {
		return err
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return err
	}
	return bridge.call(ctx, bridgeCommand{Type: "cancel", Reason: "cancelled by Task Console"}, nil)
}

func (c *RuntimeClient) Inspect(ctx context.Context, target Target) (SessionInfo, error) {
	target, err := c.readyTarget(target)
	if err != nil {
		return SessionInfo{}, err
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return SessionInfo{}, err
	}
	var payload bridgeSession
	if err := bridge.call(ctx, bridgeCommand{Type: "inspect"}, &payload); err != nil {
		return SessionInfo{}, err
	}
	return bridgeSessionInfo(target, payload)
}

func (c *RuntimeClient) DecidePermission(ctx context.Context, permissionID, optionID string, cancelled bool) error {
	if strings.TrimSpace(permissionID) == "" {
		return fmt.Errorf("permission ID is required")
	}
	if !cancelled && strings.TrimSpace(optionID) == "" {
		return fmt.Errorf("permission option ID is required")
	}
	c.mu.Lock()
	bridge := c.bridge
	closed := c.closed
	c.mu.Unlock()
	if closed || bridge == nil {
		return fmt.Errorf("acpx runtime bridge is unavailable")
	}
	return bridge.call(ctx, bridgeCommand{
		Type: "permission_decision", PermissionID: permissionID, OptionID: optionID, Cancelled: cancelled,
	}, nil)
}

func (c *RuntimeClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	bridge := c.bridge
	c.mu.Unlock()
	if bridge == nil {
		return nil
	}
	return bridge.closeProcess()
}

func (c *RuntimeClient) readyTarget(target Target) (Target, error) {
	if !c.verified.Load() {
		return Target{}, fmt.Errorf("acpx runtime compatibility has not been probed")
	}
	if err := target.validate(); err != nil {
		return Target{}, err
	}
	workingDirectory, err := gitrepo.CanonicalPath(target.WorkingDirectory)
	if err != nil {
		return Target{}, fmt.Errorf("resolve agent working directory %q: %w", target.WorkingDirectory, err)
	}
	target.WorkingDirectory = workingDirectory
	return target, nil
}

func (c *RuntimeClient) bridgeFor(ctx context.Context, target Target) (*runtimeBridge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("acpx runtime client is closed")
	}
	if c.target != nil && !sameRuntimeTarget(*c.target, target) {
		return nil, fmt.Errorf("acpx runtime client is already bound to Session %s", c.target.ConsoleSessionID)
	}
	if c.bridge == nil {
		bridge, err := startRuntimeBridge(ctx, c.binary, c.stateDirectory)
		if err != nil {
			return nil, err
		}
		c.bridge = bridge
		copyTarget := target
		c.target = &copyTarget
	}
	return c.bridge, nil
}

func (c *RuntimeClient) existingBridge(target Target) (*runtimeBridge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.bridge == nil || c.target == nil {
		return nil, fmt.Errorf("acpx runtime Session is not connected")
	}
	if !sameRuntimeTarget(*c.target, target) {
		return nil, fmt.Errorf("acpx runtime client is bound to a different Session")
	}
	return c.bridge, nil
}

func sameRuntimeTarget(left, right Target) bool {
	return left.ConsoleSessionID == right.ConsoleSessionID &&
		left.WorkingDirectory == right.WorkingDirectory &&
		left.Profile.Name == right.Profile.Name &&
		left.Permission == right.Permission
}

func cloneProfileEnvironment(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func runtimeEvent(spec PromptSpec, sequence uint64, encoded json.RawMessage) (Event, error) {
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return Event{}, &ProtocolError{Operation: "runtime event", Message: err.Error()}
	}
	typeName, _ := payload["type"].(string)
	eventType := "runtime_status"
	var raw any
	switch typeName {
	case "text_delta":
		text, _ := payload["text"].(string)
		stream, _ := payload["stream"].(string)
		eventType = "agent_message_chunk"
		if stream == "thought" {
			eventType = "agent_thought_chunk"
		}
		raw = runtimeSessionUpdate(spec, eventType, map[string]any{
			"content": map[string]any{"type": "text", "text": text},
		})
	case "tool_call":
		eventType, _ = payload["tag"].(string)
		if eventType != "tool_call" && eventType != "tool_call_update" {
			eventType = "tool_call_update"
		}
		update := cloneRuntimeMap(payload, "type", "tag", "text")
		if _, exists := update["title"]; !exists {
			if title, ok := payload["text"].(string); ok {
				update["title"] = title
			}
		}
		raw = runtimeSessionUpdate(spec, eventType, update)
	case "status":
		eventType, _ = payload["tag"].(string)
		if eventType == "" {
			eventType = "runtime_status"
		}
		raw = runtimeSessionUpdate(spec, eventType, cloneRuntimeMap(payload, "type", "tag"))
	case "error":
		eventType = "error"
		raw = map[string]any{"jsonrpc": "2.0", "error": map[string]any{"message": payload["message"], "code": payload["code"]}}
	default:
		return Event{}, &ProtocolError{Operation: "runtime event", Message: fmt.Sprintf("unknown event type %q", typeName)}
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventVersion: 1, SessionID: spec.ConsoleSessionID, RequestID: spec.RequestID,
		Seq: sequence, Stream: "prompt", Type: eventType, Raw: rawJSON,
	}, nil
}

func runtimePermissionEvent(spec PromptSpec, sequence uint64, message bridgeMessage) (Event, error) {
	if strings.TrimSpace(message.PermissionID) == "" || !json.Valid(message.Request) {
		return Event{}, &ProtocolError{Operation: "runtime permission", Message: "invalid permission envelope"}
	}
	var params map[string]any
	if err := json.Unmarshal(message.Request, &params); err != nil {
		return Event{}, err
	}
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": message.PermissionID, "method": "session/request_permission", "params": params,
	})
	if err != nil {
		return Event{}, err
	}
	return Event{
		EventVersion: 1, SessionID: spec.ConsoleSessionID, RequestID: spec.RequestID,
		Seq: sequence, Stream: "prompt", Type: "session_request_permission", Raw: raw,
	}, nil
}

func runtimeTurnResult(receipt PromptReceipt, encoded json.RawMessage) (PromptReceipt, error) {
	var result struct {
		Status     string `json:"status"`
		StopReason string `json:"stopReason"`
		Error      struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return receipt, &ProtocolError{Operation: "runtime turn result", Message: err.Error()}
	}
	switch result.Status {
	case "completed":
		receipt.StopReason = firstNonEmptyString(result.StopReason, "end_turn")
		return receipt, nil
	case "cancelled":
		receipt.StopReason = "cancelled"
		return receipt, nil
	case "failed":
		message := result.Error.Message
		if result.Error.Code != "" {
			message = result.Error.Code + ": " + message
		}
		return receipt, &ProtocolError{Operation: "runtime turn", Message: firstNonEmptyString(message, "provider turn failed")}
	default:
		return receipt, &ProtocolError{Operation: "runtime turn result", Message: fmt.Sprintf("unknown status %q", result.Status)}
	}
}

func runtimeSessionUpdate(spec PromptSpec, eventType string, fields map[string]any) map[string]any {
	update := make(map[string]any, len(fields)+1)
	update["sessionUpdate"] = eventType
	for name, value := range fields {
		update[name] = value
	}
	return map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": spec.ConsoleSessionID, "update": update},
	}
}

func cloneRuntimeMap(source map[string]any, excluded ...string) map[string]any {
	skip := make(map[string]struct{}, len(excluded))
	for _, name := range excluded {
		skip[name] = struct{}{}
	}
	result := make(map[string]any, len(source))
	for name, value := range source {
		if _, omitted := skip[name]; !omitted {
			result[name] = value
		}
	}
	return result
}

var _ PermissionResponder = (*RuntimeClient)(nil)
