package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

const (
	delegateToolName = "taskai_delegate"
	appCallTimeout   = 30 * time.Second
	maxAppLineBytes  = 16 << 20
)

type appError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type appMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *appError       `json:"error,omitempty"`
}

type appServer struct {
	store *fixtureStore

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan appMessage
	active  map[string]string
	done    chan struct{}
	closed  bool
}

func runAppServer(store *fixtureStore) error {
	server := &appServer{
		store: store, pending: make(map[string]chan appMessage),
		active: make(map[string]string), done: make(chan struct{}),
	}
	defer server.close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), maxAppLineBytes)
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		var message appMessage
		if len(raw) == 0 || bytes.Contains(raw, []byte(`"jsonrpc"`)) ||
			json.Unmarshal(raw, &message) != nil || !json.Valid(raw) {
			return errors.New("invalid headerless app-server frame")
		}
		switch {
		case message.Method != "" && len(message.ID) != 0:
			server.handleRequest(message)
		case message.Method != "" && len(message.ID) == 0:
			if message.Method != "initialized" {
				_ = store.appendTrace(spec.TraceEvent{
					Event: spec.EventAppRequest, Method: message.Method,
					Params: cloneRaw(message.Params),
				})
			}
		case message.Method == "" && len(message.ID) != 0:
			server.deliver(message)
		default:
			return errors.New("invalid app-server envelope")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (server *appServer) handleRequest(message appMessage) {
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventAppRequest, Method: message.Method,
		Params: cloneRaw(message.Params),
	})
	result, after, err := server.requestResult(message.Method, message.Params)
	response := appMessage{ID: cloneRaw(message.ID)}
	if err != nil {
		response.Error = &appError{Code: -32000, Message: "synthetic provider request failed"}
	} else {
		response.Result = result
	}
	if writeErr := server.write(response); writeErr != nil {
		server.close()
		return
	}
	if err == nil && after != nil {
		go after()
	}
}

func (server *appServer) requestResult(method string, raw json.RawMessage) (json.RawMessage, func(), error) {
	switch method {
	case "initialize":
		return json.RawMessage(`{"userAgent":"codex-cli/0.150.1"}`), nil, nil
	case "thread/start":
		return server.threadStart(raw)
	case "thread/resume":
		return server.threadResume(raw)
	case "thread/read":
		return server.threadRead(raw)
	case "turn/start":
		return server.turnStart(raw)
	case "turn/interrupt":
		return server.turnInterrupt(raw)
	default:
		return nil, nil, errors.New("unsupported method")
	}
}

func (server *appServer) threadStart(raw json.RawMessage) (json.RawMessage, func(), error) {
	var params struct {
		CWD                   string            `json:"cwd"`
		DeveloperInstructions string            `json:"developerInstructions"`
		DynamicTools          []json.RawMessage `json:"dynamicTools"`
	}
	if json.Unmarshal(raw, &params) != nil || params.CWD == "" || params.DeveloperInstructions == "" ||
		len(params.DynamicTools) != 1 {
		return nil, nil, errors.New("invalid thread start")
	}
	threadID, err := server.store.newID("manager")
	if err != nil {
		return nil, nil, err
	}
	thread := nativeThread{
		ID: threadID, Kind: "manager", DeveloperInstructions: params.DeveloperInstructions,
		DynamicTools:           cloneRaws(params.DynamicTools),
		DiscoveredInstructions: projectInstructionItems(params.CWD), Turns: []nativeTurn{},
	}
	if err := server.store.writeThread(thread); err != nil {
		return nil, nil, err
	}
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventThreadStart, ThreadID: threadID, Params: cloneRaw(raw),
	})
	result, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": threadID}})
	return result, nil, nil
}

func (server *appServer) threadResume(raw json.RawMessage) (json.RawMessage, func(), error) {
	var fields map[string]json.RawMessage
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &params) != nil ||
		len(fields) != 1 || fields["threadId"] == nil {
		return nil, nil, errors.New("resume must contain only threadId")
	}
	thread, _, err := server.store.readThread(params.ThreadID)
	if err != nil || thread.Kind != "manager" || thread.DeveloperInstructions == "" ||
		len(thread.DynamicTools) != 1 || !dynamicToolNamed(thread.DynamicTools[0], delegateToolName) {
		return nil, nil, errors.New("manager capability was not retained")
	}
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventThreadResume, ThreadID: params.ThreadID, Params: cloneRaw(raw),
	})
	result, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": params.ThreadID}})
	return result, nil, nil
}

func (server *appServer) threadRead(raw json.RawMessage) (json.RawMessage, func(), error) {
	var fields map[string]json.RawMessage
	var params struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}
	if json.Unmarshal(raw, &fields) != nil || json.Unmarshal(raw, &params) != nil ||
		len(fields) != 2 || fields["threadId"] == nil || fields["includeTurns"] == nil || !params.IncludeTurns {
		return nil, nil, errors.New("thread read must name one exact thread")
	}
	_, threadRaw, err := server.store.readThread(params.ThreadID)
	if err != nil {
		return nil, nil, err
	}
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventThreadRead, ThreadID: params.ThreadID, Params: cloneRaw(raw),
	})
	result, _ := json.Marshal(struct {
		Thread json.RawMessage `json:"thread"`
	}{Thread: threadRaw})
	return result, nil, nil
}

func (server *appServer) turnStart(raw json.RawMessage) (json.RawMessage, func(), error) {
	var params struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if json.Unmarshal(raw, &params) != nil || !safeNativeID(params.ThreadID) ||
		len(params.Input) != 1 || params.Input[0].Type != "text" || params.Input[0].Text == "" {
		return nil, nil, errors.New("invalid turn start")
	}
	thread, _, err := server.store.readThread(params.ThreadID)
	if err != nil || thread.Kind != "manager" || len(thread.DynamicTools) != 1 ||
		!dynamicToolNamed(thread.DynamicTools[0], delegateToolName) {
		return nil, nil, errors.New("manager thread unavailable")
	}
	turnID := server.store.nextID("manager-turn")
	server.mu.Lock()
	if server.closed || server.active[params.ThreadID] != "" {
		server.mu.Unlock()
		return nil, nil, errors.New("manager turn already active")
	}
	server.active[params.ThreadID] = turnID
	server.mu.Unlock()
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventTurnStart, ThreadID: params.ThreadID, TurnID: turnID,
		Params: cloneRaw(raw),
	})
	result, _ := json.Marshal(map[string]any{
		"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	})
	after := func() {
		_ = server.notify("turn/started", map[string]any{
			"threadId": params.ThreadID,
			"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		})
		server.runManagerTurn(params.ThreadID, turnID, params.Input[0].Text)
	}
	return result, after, nil
}

func (server *appServer) turnInterrupt(raw json.RawMessage) (json.RawMessage, func(), error) {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if json.Unmarshal(raw, &params) != nil || !safeNativeID(params.ThreadID) || !safeNativeID(params.TurnID) {
		return nil, nil, errors.New("invalid interrupt")
	}
	server.mu.Lock()
	owned := server.active[params.ThreadID] == params.TurnID
	if owned {
		delete(server.active, params.ThreadID)
	}
	server.mu.Unlock()
	if !owned {
		return nil, nil, errors.New("stale interrupt")
	}
	after := func() {
		_ = server.notify("turn/completed", map[string]any{
			"threadId": params.ThreadID,
			"turn":     map[string]any{"id": params.TurnID, "status": "interrupted", "items": []any{}},
		})
	}
	return json.RawMessage(`{}`), after, nil
}

func (server *appServer) runManagerTurn(threadID, turnID, prompt string) {
	status := "completed"
	items := []nativeItem{{Type: "userMessage", ID: server.store.nextID("manager-user"), Text: prompt}}
	output := spec.ManagerOutputCanary
	var ownedChild string
	if native, _, readErr := server.store.readThread(threadID); readErr == nil && len(native.Turns) != 0 {
		// The synthetic provider selects its second public completion from its
		// own native turn boundary. Prompt bytes remain opaque and are never
		// interpreted to drive fixture behavior.
		output = spec.ManagerResumeOutputCanary
		ownedChild = priorOwnedChild(native)
	}

	startInput := map[string]any{
		"operation": "start", "provider": "codex",
		"prompt": spec.SubordinatePromptCanary, "mode": "answer",
	}
	if ownedChild != "" {
		delete(startInput, "provider")
		startInput["child_session_id"] = ownedChild
	}
	startArguments, _ := json.Marshal(startInput)
	startResult, err := server.dynamicTool(threadID, turnID, startArguments)
	if err == nil {
		items = append(items,
			nativeItem{Type: "dynamicToolCall", ID: server.store.nextID("tool-call"), Name: delegateToolName, Result: cloneRaw(startArguments)},
			nativeItem{Type: "dynamicToolResult", ID: server.store.nextID("tool-result"), Name: delegateToolName, Result: cloneRaw(startResult)},
		)
	}
	var start struct {
		ChildSessionID string `json:"child_session_id"`
		More           bool   `json:"more"`
		NextOperation  string `json:"next_operation"`
		NextArguments  struct {
			Operation string `json:"operation"`
		} `json:"next_arguments"`
	}
	if err == nil {
		err = json.Unmarshal(startResult, &start)
	}
	if err == nil && (start.ChildSessionID == "" || !start.More || start.NextOperation != "next_answer" ||
		start.NextArguments.Operation != "next_answer") {
		err = errors.New("invalid start continuation")
	}

	var answerResult json.RawMessage
	if err == nil {
		answerArguments := json.RawMessage(`{"operation":"next_answer"}`)
		answerResult, err = server.dynamicTool(threadID, turnID, answerArguments)
		if err == nil {
			items = append(items,
				nativeItem{Type: "dynamicToolCall", ID: server.store.nextID("tool-call"), Name: delegateToolName, Result: cloneRaw(answerArguments)},
				nativeItem{Type: "dynamicToolResult", ID: server.store.nextID("tool-result"), Name: delegateToolName, Result: cloneRaw(answerResult)},
			)
		}
	}

	var replayResult json.RawMessage
	if err == nil {
		replayArguments, _ := json.Marshal(map[string]any{
			"operation": "replay", "child_session_ids": []string{start.ChildSessionID}, "n": 1,
		})
		replayResult, err = server.dynamicTool(threadID, turnID, replayArguments)
		if err == nil || len(replayResult) != 0 {
			items = append(items,
				nativeItem{Type: "dynamicToolCall", ID: server.store.nextID("tool-call"), Name: delegateToolName, Result: cloneRaw(replayArguments)},
				nativeItem{Type: "dynamicToolResult", ID: server.store.nextID("tool-result"), Name: delegateToolName, Result: cloneRaw(replayResult)},
			)
		}
	}

	if err != nil || !bytes.Contains(answerResult, []byte(spec.AnswerCanary)) ||
		!bytes.Contains(replayResult, []byte(spec.ReplayCanary)) {
		status = "failed"
		output = "TASKAI_ACCEPTANCE_PROVIDER_FLOW_FAILED"
	}
	items = append(items, nativeItem{Type: "agentMessage", ID: server.store.nextID("manager-answer"), Text: output})
	server.appendManagerTurn(threadID, nativeTurn{ID: turnID, Status: status, Items: items})
	_ = server.notify("item/agentMessage/delta", map[string]any{
		"threadId": threadID, "turnId": turnID,
		"itemId": server.store.nextID("manager-message"), "delta": output,
	})
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventManagerOutput, ThreadID: threadID, TurnID: turnID,
		Params: json.RawMessage(strconvJSON(output)),
	})
	_ = server.notify("turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": turnID, "status": status, "items": []any{}},
	})
	server.mu.Lock()
	if server.active[threadID] == turnID {
		delete(server.active, threadID)
	}
	server.mu.Unlock()
}

func priorOwnedChild(thread nativeThread) string {
	for turnIndex := len(thread.Turns) - 1; turnIndex >= 0; turnIndex-- {
		items := thread.Turns[turnIndex].Items
		for itemIndex := len(items) - 1; itemIndex >= 0; itemIndex-- {
			item := items[itemIndex]
			if item.Type != "dynamicToolResult" || item.Name != delegateToolName || len(item.Result) == 0 {
				continue
			}
			var result struct {
				Operation      string `json:"operation"`
				ChildSessionID string `json:"child_session_id"`
			}
			if json.Unmarshal(item.Result, &result) == nil && result.Operation == "start" &&
				result.ChildSessionID != "" {
				return result.ChildSessionID
			}
		}
	}
	return ""
}

func (server *appServer) appendManagerTurn(threadID string, turn nativeTurn) {
	thread, _, err := server.store.readThread(threadID)
	if err != nil {
		return
	}
	thread.Turns = append(thread.Turns, turn)
	_ = server.store.writeThread(thread)
}

func (server *appServer) dynamicTool(threadID, turnID string, arguments json.RawMessage) (json.RawMessage, error) {
	var head struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(arguments, &head) != nil || head.Operation == "" {
		return nil, errors.New("invalid dynamic tool arguments")
	}
	requestID := server.store.nextID("tool-rpc")
	callID := server.store.nextID("tool-call")
	idRaw, _ := json.Marshal(requestID)
	params, _ := json.Marshal(map[string]any{
		"arguments": json.RawMessage(arguments), "callId": callID,
		"threadId": threadID, "tool": delegateToolName, "turnId": turnID,
	})
	result := make(chan appMessage, 1)
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return nil, io.EOF
	}
	server.pending[correlationKey(idRaw)] = result
	server.mu.Unlock()
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventToolCall, ThreadID: threadID, TurnID: turnID,
		Operation: head.Operation, Params: cloneRaw(arguments),
	})
	if err := server.write(appMessage{
		ID: idRaw, Method: "item/tool/call", Params: params,
	}); err != nil {
		server.removePending(idRaw)
		return nil, err
	}
	timer := time.NewTimer(appCallTimeout)
	defer timer.Stop()
	var response appMessage
	select {
	case response = <-result:
	case <-server.done:
		server.removePending(idRaw)
		return nil, io.EOF
	case <-timer.C:
		server.removePending(idRaw)
		return nil, errors.New("dynamic tool timeout")
	}
	_ = server.notify("serverRequest/resolved", map[string]any{
		"requestId": json.RawMessage(idRaw), "threadId": threadID,
	})
	if response.Error != nil || len(response.Result) == 0 {
		return nil, errors.New("dynamic tool rpc failed")
	}
	var native struct {
		Success      bool `json:"success"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentItems"`
	}
	if json.Unmarshal(response.Result, &native) != nil || !native.Success ||
		len(native.ContentItems) != 1 || native.ContentItems[0].Type != "inputText" ||
		!json.Valid([]byte(native.ContentItems[0].Text)) {
		return nil, errors.New("dynamic tool result failed")
	}
	capabilityResult := json.RawMessage(native.ContentItems[0].Text)
	_ = server.store.appendTrace(spec.TraceEvent{
		Event: spec.EventToolResult, ThreadID: threadID, TurnID: turnID,
		Operation: head.Operation, Params: cloneRaw(capabilityResult),
	})
	return cloneRaw(capabilityResult), nil
}

func (server *appServer) deliver(message appMessage) {
	key := correlationKey(message.ID)
	server.mu.Lock()
	result := server.pending[key]
	delete(server.pending, key)
	server.mu.Unlock()
	if result != nil {
		result <- message
	}
}

func (server *appServer) removePending(id json.RawMessage) {
	server.mu.Lock()
	delete(server.pending, correlationKey(id))
	server.mu.Unlock()
}

func (server *appServer) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return server.write(appMessage{Method: method, Params: raw})
}

func (server *appServer) write(message appMessage) error {
	raw, err := json.Marshal(message)
	if err != nil || len(raw) == 0 || len(raw) > maxAppLineBytes || bytes.Contains(raw, []byte{'\n'}) {
		return errors.New("invalid app-server output")
	}
	server.writeMu.Lock()
	defer server.writeMu.Unlock()
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

func (server *appServer) close() {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return
	}
	server.closed = true
	server.pending = make(map[string]chan appMessage)
	close(server.done)
	server.mu.Unlock()
}

func dynamicToolNamed(raw json.RawMessage, name string) bool {
	var tool struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(raw, &tool) == nil && tool.Name == name
}

func correlationKey(raw json.RawMessage) string { return string(bytes.TrimSpace(raw)) }

func cloneRaw(raw json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), raw...) }

func cloneRaws(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = cloneRaw(values[index])
	}
	return result
}

func strconvJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
