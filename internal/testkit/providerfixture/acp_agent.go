package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

const maxACPLineBytes = 128 << 10

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

type acpAgent struct {
	store     *fixtureStore
	sessionID string
}

func runACPAgent(store *fixtureStore) error {
	agent := &acpAgent{store: store}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), maxACPLineBytes)
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		var message acpMessage
		if len(raw) == 0 || json.Unmarshal(raw, &message) != nil || message.JSONRPC != "2.0" ||
			message.Method == "" || !json.Valid(raw) {
			return errors.New("invalid ACP frame")
		}
		_ = store.appendTrace(spec.TraceEvent{
			Event: spec.EventACPRequest, Method: message.Method,
			SessionID: agent.sessionID, Params: cloneRaw(message.Params),
		})
		if len(message.ID) == 0 {
			if message.Method == "session/cancel" {
				continue
			}
			return errors.New("unsupported ACP notification")
		}
		result, err := agent.handleRequest(message.Method, message.Params)
		response := acpMessage{JSONRPC: "2.0", ID: cloneRaw(message.ID)}
		if err != nil {
			response.Error = &acpError{Code: -32000, Message: "synthetic ACP request failed"}
		} else {
			response.Result = result
		}
		if err := writeACP(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (agent *acpAgent) handleRequest(method string, raw json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"loadSession":true},"agentInfo":{"name":"taskai-codex-acp-fixture","version":"1"}}`), nil
	case "session/new":
		return agent.newSession(raw)
	case "session/resume":
		return agent.resumeSession(raw)
	case "session/prompt":
		return agent.prompt(raw)
	default:
		return nil, errors.New("unsupported ACP method")
	}
}

func (agent *acpAgent) newSession(raw json.RawMessage) (json.RawMessage, error) {
	var params struct {
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &params) != nil || params.CWD == "" || !filepath.IsAbs(params.CWD) ||
		params.MCPServers == nil || len(params.MCPServers) != 0 {
		return nil, errors.New("invalid session/new")
	}
	id, err := agent.store.newID("child")
	if err != nil {
		return nil, err
	}
	thread := nativeThread{ID: id, Kind: "subordinate", Turns: []nativeTurn{}}
	if err := agent.store.writeThread(thread); err != nil {
		return nil, err
	}
	agent.sessionID = id
	_ = agent.store.appendTrace(spec.TraceEvent{
		Event: spec.EventSessionNew, SessionID: id, Params: cloneRaw(raw),
	})
	result, _ := json.Marshal(map[string]any{"sessionId": id})
	return result, nil
}

func (agent *acpAgent) resumeSession(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	var params struct {
		SessionID  string            `json:"sessionId"`
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &params) != nil || len(fields) != 3 ||
		fields["sessionId"] == nil || fields["cwd"] == nil || fields["mcpServers"] == nil ||
		params.CWD == "" || !filepath.IsAbs(params.CWD) || params.MCPServers == nil || len(params.MCPServers) != 0 {
		return nil, errors.New("invalid exact session/resume")
	}
	thread, _, err := agent.store.readThread(params.SessionID)
	if err != nil || thread.Kind != "subordinate" {
		return nil, errors.New("exact subordinate unavailable")
	}
	agent.sessionID = params.SessionID
	_ = agent.store.appendTrace(spec.TraceEvent{
		Event: spec.EventSessionResume, SessionID: params.SessionID, Params: cloneRaw(raw),
	})
	return json.RawMessage(`{}`), nil
}

func (agent *acpAgent) prompt(raw json.RawMessage) (json.RawMessage, error) {
	var params struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if json.Unmarshal(raw, &params) != nil || params.SessionID == "" || params.SessionID != agent.sessionID ||
		len(params.Prompt) != 1 || params.Prompt[0].Type != "text" {
		return nil, errors.New("invalid session/prompt")
	}
	if err := agent.waitOnFirstSubordinate(); err != nil {
		return nil, err
	}
	turnID := agent.store.nextID("child-turn")
	thread, _, err := agent.store.readThread(agent.sessionID)
	if err != nil {
		return nil, err
	}
	thread.Turns = append(thread.Turns, nativeTurn{
		ID: turnID, Status: "completed",
		Items: []nativeItem{
			{Type: "userMessage", ID: agent.store.nextID("child-user"), Text: params.Prompt[0].Text},
			{Type: "commandExecution", ID: agent.store.nextID("child-tool"), Command: spec.ToolCanary},
			{Type: "toolResult", ID: agent.store.nextID("child-result"), Text: spec.ReplayCanary},
			{Type: "agentMessage", ID: agent.store.nextID("child-answer"), Text: spec.AnswerCanary},
		},
	})
	// Native completion is persisted before the public completion response, so
	// an explicit subsequent Replay reads the exact completed range in place.
	if err := agent.store.writeThread(thread); err != nil {
		return nil, err
	}
	if err := writeACPNotification("session/update", map[string]any{
		"sessionId": agent.sessionID,
		"update": map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": agent.store.nextID("public-tool"),
			"status": "completed", "title": spec.ToolCanary,
		},
	}); err != nil {
		return nil, err
	}
	if err := writeACPNotification("session/update", map[string]any{
		"sessionId": agent.sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "messageId": agent.store.nextID("public-answer"),
			"content": map[string]any{"type": "text", "text": spec.AnswerCanary},
		},
	}); err != nil {
		return nil, err
	}
	_ = agent.store.appendTrace(spec.TraceEvent{
		Event: spec.EventSubordinateAnswer, SessionID: agent.sessionID, TurnID: turnID,
		Params: json.RawMessage(strconvJSON(spec.AnswerCanary)),
	})
	return json.RawMessage(`{"stopReason":"end_turn"}`), nil
}

func (agent *acpAgent) waitOnFirstSubordinate() error {
	fifo := os.Getenv(spec.ReleaseFIFOEnvironment)
	if fifo == "" {
		return nil
	}
	if !filepath.IsAbs(fifo) || filepath.Clean(fifo) != fifo || strings.IndexByte(fifo, 0) >= 0 ||
		!pathWithin(agent.store.root, fifo) {
		return errors.New("invalid release fifo")
	}
	claimPath := filepath.Join(agent.store.root, "first-subordinate-hold.claimed")
	claim, err := os.OpenFile(claimPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = claim.Close()
	_ = agent.store.appendTrace(spec.TraceEvent{
		Event: spec.EventSubordinateWaiting, SessionID: agent.sessionID,
	})
	reader, err := os.OpenFile(fifo, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	var release [1]byte
	_, readErr := io.ReadFull(reader, release[:])
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || release[0] != 1 {
		return errors.New("invalid subordinate release")
	}
	_ = agent.store.appendTrace(spec.TraceEvent{
		Event: spec.EventSubordinateReleased, SessionID: agent.sessionID,
	})
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func writeACPNotification(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return writeACP(acpMessage{JSONRPC: "2.0", Method: method, Params: raw})
}

func writeACP(message acpMessage) error {
	raw, err := json.Marshal(message)
	if err != nil || len(raw) == 0 || len(raw) > maxACPLineBytes || bytes.Contains(raw, []byte{'\n'}) {
		return errors.New("invalid ACP output")
	}
	raw = append(raw, '\n')
	for len(raw) > 0 {
		written, writeErr := os.Stdout.Write(raw)
		if writeErr != nil || written <= 0 || written > len(raw) {
			return io.ErrClosedPipe
		}
		raw = raw[written:]
	}
	return nil
}
