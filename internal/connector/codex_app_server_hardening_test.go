package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

type cancellableDynamicHandler struct {
	calls     atomic.Int64
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func newCancellableDynamicHandler() *cancellableDynamicHandler {
	return &cancellableDynamicHandler{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1)}
}

func (h *cancellableDynamicHandler) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	h.calls.Add(1)
	h.started <- struct{}{}
	<-ctx.Done()
	h.once.Do(func() { h.cancelled <- struct{}{} })
	return nil, ctx.Err()
}

type countDynamicHandler struct {
	calls atomic.Int64
	err   error
}

func (h *countDynamicHandler) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	h.calls.Add(1)
	if h.err != nil {
		return nil, h.err
	}
	return json.RawMessage(`{"more":false}`), nil
}

func startActiveCodexTurn(
	t *testing.T,
	client *CodexAppServerClient,
	server *appHarness,
	reference replay.OpaqueRef,
	threadID string,
	turnID string,
	sink CodexTurnSink,
) *CodexTurnHandle {
	t.Helper()
	done := make(chan struct {
		handle *CodexTurnHandle
		err    error
	}, 1)
	go func() {
		handle, err := client.TurnStart(context.Background(), reference, "opaque", sink)
		done <- struct {
			handle *CodexTurnHandle
			err    error
		}{handle, err}
	}()
	request := server.read(t)
	if request.Method != appMethodTurnStart {
		t.Fatalf("turn start method = %q", request.Method)
	}
	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": appMethodTurnStarted,
		"params": map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
		},
	}))
	server.respond(t, request, mustJSON(t, map[string]any{
		"turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}},
	}))
	started := <-done
	if started.err != nil || started.handle == nil {
		t.Fatalf("turn start handle=%v error=%v", started.handle, started.err)
	}
	return started.handle
}

func sendDynamicRequest(t *testing.T, server *appHarness, id json.RawMessage, params map[string]any) appServerMessage {
	t.Helper()
	server.send(t, appServerMessage{ID: id, Method: appMethodDynamicTool, Params: mustJSON(t, params)})
	return server.read(t)
}

func assertDynamicFailure(t *testing.T, response appServerMessage, canary string) {
	t.Helper()
	var result struct {
		Success bool `json:"success"`
	}
	if response.kind() != appResponse || json.Unmarshal(response.Result, &result) != nil || result.Success {
		t.Fatalf("dynamic failure response = %s", response.Raw)
	}
	if canary != "" && bytes.Contains(response.Raw, []byte(canary)) {
		t.Fatalf("dynamic failure leaked private canary: %s", response.Raw)
	}
}

func waitInboundCount(t *testing.T, peer *appServerPeer, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		peer.mu.Lock()
		got := len(peer.inbound)
		peer.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	peer.mu.Lock()
	got := len(peer.inbound)
	peer.mu.Unlock()
	t.Fatalf("live inbound requests = %d, want %d", got, want)
}

func waitManagerTurn(t *testing.T, connection ManagerConnection) {
	t.Helper()
	live, ok := connection.(*codexManagerConnection)
	if !ok {
		t.Fatal("test connection is not the Codex live port")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		live.mu.Lock()
		active := live.active != nil
		live.mu.Unlock()
		if active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("live manager turn was not published")
}

func (s *appHarness) assertNoMessage(t *testing.T) {
	t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(75 * time.Millisecond))
	_, err := s.reader.Peek(1)
	_ = s.conn.SetReadDeadline(time.Time{})
	if err == nil {
		t.Fatal("unexpected App Server client message")
	}
	if networkErr, ok := err.(net.Error); ok && !networkErr.Timeout() {
		t.Fatalf("unexpected read error: %v", err)
	}
}

func TestAppServerResolvedCancelsOnlyExactInboundAndLeavesZeroRequests(t *testing.T) {
	for _, requestID := range []json.RawMessage{json.RawMessage(`"request-string"`), json.RawMessage(`42`)} {
		requestID := requestID
		t.Run(string(requestID), func(t *testing.T) {
			handler := newCancellableDynamicHandler()
			client, server := openTestAppServer(t, handler)
			reference := startTestThread(t, client, server, "thread-cancel")
			handle := startActiveCodexTurn(t, client, server, reference, "thread-cancel", "turn-cancel", func(context.Context, json.RawMessage) error { return nil })

			server.send(t, appServerMessage{
				ID: requestID, Method: appMethodDynamicTool,
				Params: mustJSON(t, map[string]any{
					"arguments": json.RawMessage(`{"operation":"next_answer"}`),
					"callId":    "call-cancel", "threadId": "thread-cancel", "tool": DelegateToolName, "turnId": "turn-cancel",
				}),
			})
			select {
			case <-handler.started:
			case <-time.After(3 * time.Second):
				t.Fatal("dynamic handler did not start")
			}
			waitInboundCount(t, client.peer, 1)

			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": appMethodRequestDone,
				"params": map[string]any{"requestId": json.RawMessage(requestID), "threadId": "thread-foreign"},
			}))
			select {
			case <-handler.cancelled:
				t.Fatal("foreign thread cancelled owned server request")
			case <-time.After(50 * time.Millisecond):
			}

			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": appMethodRequestDone,
				"params": map[string]any{"requestId": json.RawMessage(requestID), "threadId": "thread-cancel"},
			}))
			select {
			case <-handler.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("exact serverRequest/resolved did not cancel handler")
			}
			waitInboundCount(t, client.peer, 0)
			server.assertNoMessage(t)
			if handler.calls.Load() != 1 {
				t.Fatalf("handler calls = %d, want 1", handler.calls.Load())
			}

			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": appMethodTurnCompleted,
				"params": map[string]any{
					"threadId": "thread-cancel",
					"turn":     map[string]any{"id": "turn-cancel", "status": "completed", "items": []any{}},
				},
			}))
			if _, err := handle.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAppServerDuplicateLiveRequestIDFailsClosedAndCancelsAllInbound(t *testing.T) {
	handler := newCancellableDynamicHandler()
	client, server := openTestAppServer(t, handler)
	reference := startTestThread(t, client, server, "thread-duplicate")
	handle := startActiveCodexTurn(t, client, server, reference, "thread-duplicate", "turn-duplicate", func(context.Context, json.RawMessage) error { return nil })
	params := mustJSON(t, map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer"}`),
		"callId":    "call-first", "threadId": "thread-duplicate", "tool": DelegateToolName, "turnId": "turn-duplicate",
	})
	server.send(t, appServerMessage{ID: json.RawMessage(`7`), Method: appMethodDynamicTool, Params: params})
	select {
	case <-handler.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first handler did not start")
	}
	waitInboundCount(t, client.peer, 1)
	server.send(t, appServerMessage{ID: json.RawMessage(`7`), Method: appMethodDynamicTool, Params: params})
	select {
	case <-client.peer.done:
	case <-time.After(3 * time.Second):
		t.Fatal("duplicate live request ID did not close unsafe transport")
	}
	select {
	case <-handler.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("transport close did not cancel live handler")
	}
	waitInboundCount(t, client.peer, 0)
	if handler.calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", handler.calls.Load())
	}
	if _, err := handle.Wait(context.Background()); !errors.Is(err, ErrTransport) {
		t.Fatalf("turn transport error = %v", err)
	}
}

func TestAppServerTransportCloseCancelsInboundWithoutLeak(t *testing.T) {
	handler := newCancellableDynamicHandler()
	client, server := openTestAppServer(t, handler)
	reference := startTestThread(t, client, server, "thread-close")
	handle := startActiveCodexTurn(t, client, server, reference, "thread-close", "turn-close", func(context.Context, json.RawMessage) error { return nil })
	server.send(t, appServerMessage{
		ID: json.RawMessage(`"closing"`), Method: appMethodDynamicTool,
		Params: mustJSON(t, map[string]any{
			"arguments": json.RawMessage(`{"operation":"next_answer"}`),
			"callId":    "call-close", "threadId": "thread-close", "tool": DelegateToolName, "turnId": "turn-close",
		}),
	})
	select {
	case <-handler.started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	waitInboundCount(t, client.peer, 1)
	_ = server.conn.Close()
	select {
	case <-handler.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("transport close did not cancel handler")
	}
	waitInboundCount(t, client.peer, 0)
	if _, err := handle.Wait(context.Background()); !errors.Is(err, ErrTransport) {
		t.Fatalf("turn transport error = %v", err)
	}
}

func TestDynamicToolRejectsUntrustedShapeAndCorrelationsBeforeDelegation(t *testing.T) {
	handler := &countDynamicHandler{}
	client, server := openTestAppServer(t, handler)
	reference := startTestThread(t, client, server, "thread-validate")

	turnDone := make(chan struct {
		handle *CodexTurnHandle
		err    error
	}, 1)
	go func() {
		handle, err := client.TurnStart(context.Background(), reference, "opaque", func(context.Context, json.RawMessage) error { return nil })
		turnDone <- struct {
			handle *CodexTurnHandle
			err    error
		}{handle, err}
	}()
	turnRequest := server.read(t)
	canary := "PRIVATE_DELEGATION_CANARY"
	base := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer","opaque":"` + canary + `"}`),
		"callId":    "call-before-identity", "threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate",
	}
	response := sendDynamicRequest(t, server, json.RawMessage(`1`), base)
	assertDynamicFailure(t, response, canary)
	if handler.calls.Load() != 0 {
		t.Fatal("tool request established an unknown active turn identity")
	}

	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": appMethodTurnStarted,
		"params": map[string]any{
			"threadId": "thread-validate",
			"turn":     map[string]any{"id": "turn-validate", "status": "inProgress", "items": []any{}},
		},
	}))
	server.respond(t, turnRequest, json.RawMessage(`{"turn":{"id":"turn-validate","status":"inProgress","items":[]}}`))
	started := <-turnDone
	if started.err != nil || started.handle == nil {
		t.Fatalf("turn start error = %v", started.err)
	}

	invalid := []struct {
		name   string
		id     json.RawMessage
		params map[string]any
	}{
		{
			name: "namespace string", id: json.RawMessage(`2`),
			params: map[string]any{
				"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-namespace",
				"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate", "namespace": "forbidden",
			},
		},
		{
			name: "oversized arguments", id: json.RawMessage(`3`),
			params: map[string]any{
				"arguments": json.RawMessage(`{"opaque":"` + strings.Repeat("x", acp.MaxFrameBytes) + `"}`),
				"callId":    "call-oversized", "threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate",
			},
		},
		{
			name: "wrong tool", id: json.RawMessage(`4`),
			params: map[string]any{
				"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-wrong-tool",
				"threadId": "thread-validate", "tool": "foreign_tool", "turnId": "turn-validate",
			},
		},
		{
			name: "wrong thread", id: json.RawMessage(`5`),
			params: map[string]any{
				"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-wrong-thread",
				"threadId": "thread-foreign", "tool": DelegateToolName, "turnId": "turn-validate",
			},
		},
		{
			name: "wrong turn", id: json.RawMessage(`6`),
			params: map[string]any{
				"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-wrong-turn",
				"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-foreign",
			},
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			response := sendDynamicRequest(t, server, test.id, test.params)
			assertDynamicFailure(t, response, canary)
		})
	}
	if handler.calls.Load() != 0 {
		t.Fatalf("invalid requests reached Delegator: %d", handler.calls.Load())
	}

	valid := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-valid",
		"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate", "namespace": nil,
	}
	validResponse := sendDynamicRequest(t, server, json.RawMessage(`10`), valid)
	var success struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(validResponse.Result, &success) != nil || !success.Success || handler.calls.Load() != 1 {
		t.Fatalf("valid null-namespace response=%s calls=%d", validResponse.Raw, handler.calls.Load())
	}
	waitInboundCount(t, client.peer, 0)

	duplicateRequest := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-new",
		"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate",
	}
	assertDynamicFailure(t, sendDynamicRequest(t, server, json.RawMessage(`10`), duplicateRequest), "")
	duplicateCall := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-valid",
		"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate",
	}
	assertDynamicFailure(t, sendDynamicRequest(t, server, json.RawMessage(`11`), duplicateCall), "")
	if handler.calls.Load() != 1 {
		t.Fatalf("duplicate correlations reached Delegator: %d", handler.calls.Load())
	}

	server.sendRaw(t, json.RawMessage(`{"method":"turn/completed","params":{"threadId":"thread-validate","turn":{"id":"turn-validate","status":"completed","items":[]}}}`))
	if _, err := started.handle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call-stale",
		"threadId": "thread-validate", "tool": DelegateToolName, "turnId": "turn-validate",
	}
	assertDynamicFailure(t, sendDynamicRequest(t, server, json.RawMessage(`12`), stale), "")
	if handler.calls.Load() != 1 {
		t.Fatal("stale request reached Delegator")
	}
}

func TestPrivateDynamicToolContentNeverCrossesPublicManagerBoundary(t *testing.T) {
	handler := &countDynamicHandler{err: ErrInvalidOperation}
	client, server := openTestAppServer(t, handler)
	reference := startTestThread(t, client, server, "thread-private")
	var mu sync.Mutex
	var frames []json.RawMessage
	handle := startActiveCodexTurn(t, client, server, reference, "thread-private", "turn-private", func(_ context.Context, frame json.RawMessage) error {
		mu.Lock()
		frames = append(frames, append(json.RawMessage(nil), frame...))
		mu.Unlock()
		return nil
	})
	canary := "CHILD_PROMPT_REF_CANARY_9f4d"

	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"threadId": "thread-private", "turnId": "turn-private",
			"item": map[string]any{"type": "dynamicToolCall", "status": "inProgress", "arguments": canary},
		},
	}))
	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": "thread-private", "turnId": "turn-private",
			"item": map[string]any{
				"type": "dynamicToolCall", "status": "completed", "contentItems": []any{map[string]any{"type": "inputText", "text": canary}},
			},
		},
	}))
	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": "error",
		"params": map[string]any{"threadId": "thread-private", "turnId": "turn-private", "message": canary},
	}))
	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": appMethodAgentMessageDelta,
		"params": map[string]any{
			"threadId": "thread-private", "turnId": "turn-private", "itemId": "message-public", "delta": "PUBLIC_MANAGER_TEXT",
		},
	}))

	privateParams := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer","private":"` + canary + `"}`),
		"callId":    "call-private", "threadId": "thread-private", "tool": DelegateToolName, "turnId": "turn-private",
	}
	failed := sendDynamicRequest(t, server, json.RawMessage(`20`), privateParams)
	assertDynamicFailure(t, failed, canary)
	foreignParams := map[string]any{
		"arguments": json.RawMessage(`{"operation":"next_answer","private":"` + canary + `"}`),
		"callId":    "call-foreign", "threadId": "thread-foreign", "tool": DelegateToolName, "turnId": "turn-private",
	}
	foreign := sendDynamicRequest(t, server, json.RawMessage(`21`), foreignParams)
	assertDynamicFailure(t, foreign, canary)

	server.sendRaw(t, mustJSON(t, map[string]any{
		"method": appMethodTurnCompleted,
		"params": map[string]any{
			"threadId": "thread-private",
			"turn": map[string]any{
				"id": "turn-private", "status": "completed",
				"items": []any{map[string]any{
					"type": "dynamicToolCall", "arguments": canary, "output": canary,
				}},
			},
		},
	}))
	completion, err := handle.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(completion, []byte(canary)) || !bytes.Equal(completion, json.RawMessage(`{"stopReason":"end_turn"}`)) {
		t.Fatalf("completion exposed private tool items: %s", completion)
	}

	mu.Lock()
	got := append([]json.RawMessage(nil), frames...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("public frames=%d, want only the mapped public agent delta: %s", len(got), got)
	}
	for index, frame := range got {
		if _, parseErr := acp.ParseMessage(frame); parseErr != nil {
			t.Fatalf("frame %d is not ACP: %s error=%v", index, frame, parseErr)
		}
		if bytes.Contains(frame, []byte(canary)) || bytes.Contains(frame, []byte("dynamicToolCall")) ||
			bytes.Contains(frame, []byte(`"arguments"`)) || bytes.Contains(frame, []byte(`"contentItems"`)) {
			t.Fatalf("frame %d leaked delegation content: %s", index, frame)
		}
	}
	if !bytes.Contains(got[0], []byte("PUBLIC_MANAGER_TEXT")) {
		t.Fatalf("public manager output was not translated: %s", got[0])
	}
	if handler.calls.Load() != 1 {
		t.Fatalf("foreign call reached Delegator: calls=%d", handler.calls.Load())
	}
}

func TestCodexManagerReturnsLiveACPConnectionWithExactReadInterruptAndDeath(t *testing.T) {
	client, server := openTestAppServer(t, &countDynamicHandler{})
	started := make(chan struct {
		session NativeManagerSession
		err     error
	}, 1)
	go func() {
		session, err := startCodexManagerSession(context.Background(), client, "/tmp/taskai-live")
		started <- struct {
			session NativeManagerSession
			err     error
		}{session, err}
	}()
	startRequest := server.read(t)
	server.respond(t, startRequest, json.RawMessage(`{"thread":{"id":"thread-live-port"}}`))
	managerSession := <-started
	if managerSession.err != nil || managerSession.session.Connection == nil {
		t.Fatalf("manager session=%+v error=%v", managerSession.session, managerSession.err)
	}
	connection := managerSession.session.Connection
	if !bytes.Equal(connection.NativeReference().Bytes(), managerSession.session.NativeReference.Bytes()) {
		t.Fatal("live connection changed exact opaque reference")
	}

	frames := make(chan json.RawMessage, 8)
	promptDone := make(chan struct {
		completion json.RawMessage
		err        error
	}, 1)
	go func() {
		completion, err := connection.Prompt(context.Background(), "opaque user prompt", func(frame json.RawMessage) error {
			frames <- append(json.RawMessage(nil), frame...)
			return nil
		})
		promptDone <- struct {
			completion json.RawMessage
			err        error
		}{completion, err}
	}()
	turnStart := server.read(t)
	server.sendRaw(t, json.RawMessage(`{"method":"turn/started","params":{"threadId":"thread-live-port","turn":{"id":"turn-live-port","status":"inProgress","items":[]}}}`))
	server.respond(t, turnStart, json.RawMessage(`{"turn":{"id":"turn-live-port","status":"inProgress","items":[]}}`))
	server.sendRaw(t, json.RawMessage(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-live-port","turnId":"turn-live-port","itemId":"msg-live","delta":"public-live"}}`))
	server.sendRaw(t, json.RawMessage(`{"method":"error","params":{"threadId":"thread-live-port","turnId":"turn-live-port","message":"private diagnostic"}}`))
	select {
	case finished := <-promptDone:
		t.Fatalf("nonterminal error completed prompt: %+v", finished)
	case <-time.After(50 * time.Millisecond):
	}
	server.sendRaw(t, json.RawMessage(`{"method":"turn/completed","params":{"threadId":"thread-live-port","turn":{"id":"turn-live-port","status":"completed","items":[]}}}`))
	finished := <-promptDone
	if finished.err != nil || !bytes.Equal(finished.completion, json.RawMessage(`{"stopReason":"end_turn"}`)) {
		t.Fatalf("prompt completion=%s error=%v", finished.completion, finished.err)
	}
	close(frames)
	var sawPublic bool
	for frame := range frames {
		message, err := acp.ParseMessage(frame)
		if err != nil || message.JSONRPC != "2.0" || message.Method != acp.MethodSessionUpdate {
			t.Fatalf("manager frame is not ACP: %s error=%v", frame, err)
		}
		if bytes.Contains(frame, []byte(`"threadId"`)) || bytes.Contains(frame, []byte("private diagnostic")) {
			t.Fatalf("manager frame leaked private transport content: %s", frame)
		}
		sawPublic = sawPublic || bytes.Contains(frame, []byte("public-live"))
	}
	if !sawPublic {
		t.Fatal("live public manager content was not streamed")
	}

	readDone := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := connection.ReadNativeSession(context.Background())
		readDone <- struct {
			raw json.RawMessage
			err error
		}{raw, err}
	}()
	readRequest := server.read(t)
	if readRequest.Method != appMethodThreadRead || !bytes.Equal(readRequest.Params, json.RawMessage(`{"threadId":"thread-live-port","includeTurns":true}`)) {
		t.Fatalf("exact native read = %s %s", readRequest.Method, readRequest.Params)
	}
	server.respond(t, readRequest, json.RawMessage(`{"thread":{"id":"thread-live-port","turns":[]}}`))
	if read := <-readDone; read.err != nil || !bytes.Equal(read.raw, json.RawMessage(`{"id":"thread-live-port","turns":[]}`)) {
		t.Fatalf("native read=%s error=%v", read.raw, read.err)
	}

	interruptDone := make(chan struct {
		completion json.RawMessage
		err        error
	}, 1)
	go func() {
		completion, err := connection.Prompt(context.Background(), "interrupt exact", func(json.RawMessage) error { return nil })
		interruptDone <- struct {
			completion json.RawMessage
			err        error
		}{completion, err}
	}()
	interruptStart := server.read(t)
	server.respond(t, interruptStart, json.RawMessage(`{"turn":{"id":"turn-interrupt-port","status":"inProgress","items":[]}}`))
	waitManagerTurn(t, connection)
	interruptResult := make(chan error, 1)
	go func() { interruptResult <- connection.Interrupt(context.Background()) }()
	interruptRequest := server.read(t)
	if interruptRequest.Method != appMethodTurnInterrupt ||
		!bytes.Equal(interruptRequest.Params, json.RawMessage(`{"threadId":"thread-live-port","turnId":"turn-interrupt-port"}`)) {
		t.Fatalf("exact interrupt = %s %s", interruptRequest.Method, interruptRequest.Params)
	}
	server.respond(t, interruptRequest, json.RawMessage(`{}`))
	if err := <-interruptResult; err != nil {
		t.Fatal(err)
	}
	server.sendRaw(t, json.RawMessage(`{"method":"turn/completed","params":{"threadId":"thread-live-port","turn":{"id":"turn-interrupt-port","status":"interrupted","items":[]}}}`))
	interrupted := <-interruptDone
	if interrupted.err != nil || !bytes.Equal(interrupted.completion, json.RawMessage(`{"stopReason":"cancelled"}`)) {
		t.Fatalf("interrupted completion=%s error=%v", interrupted.completion, interrupted.err)
	}

	_ = server.conn.Close()
	select {
	case <-connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("live connection did not report process/transport death")
	}
	if !errors.Is(connection.Err(), ErrTransport) {
		t.Fatalf("connection death error = %v", connection.Err())
	}
}

func TestAppServerEnforcesExactStartAndOneOwnedThreadPerClient(t *testing.T) {
	client, server := openTestAppServer(t, &countDynamicHandler{})
	valid := ThreadStartParams{
		CWD: "/tmp/taskai-owned", DeveloperInstructions: ManagerPrefix,
		DynamicTools: []DynamicTool{DelegateCapability()},
	}
	invalid := []ThreadStartParams{
		{CWD: "relative", DeveloperInstructions: ManagerPrefix, DynamicTools: []DynamicTool{DelegateCapability()}},
		{CWD: "/tmp/taskai-owned", DeveloperInstructions: ManagerPrefix + " extra", DynamicTools: []DynamicTool{DelegateCapability()}},
		{CWD: "/tmp/taskai-owned", DeveloperInstructions: ManagerPrefix, DynamicTools: []DynamicTool{}},
		{CWD: "/tmp/taskai-owned", DeveloperInstructions: ManagerPrefix, DynamicTools: []DynamicTool{{Type: "function", Name: DelegateToolName, Description: "changed", InputSchema: DelegateCapability().InputSchema}}},
	}
	for _, params := range invalid {
		if _, err := client.ThreadStart(context.Background(), params); !errors.Is(err, ErrProviderUnsupported) {
			t.Fatalf("invalid Start error = %v", err)
		}
		server.assertNoMessage(t)
	}

	started := make(chan struct {
		session CodexThreadSession
		err     error
	}, 1)
	go func() {
		session, err := client.ThreadStart(context.Background(), valid)
		started <- struct {
			session CodexThreadSession
			err     error
		}{session, err}
	}()
	request := server.read(t)
	server.respond(t, request, json.RawMessage(`{"thread":{"id":"thread-only"}}`))
	if result := <-started; result.err != nil || result.session.ThreadID != "thread-only" {
		t.Fatalf("valid Start=%+v error=%v", result.session, result.err)
	}
	if _, err := client.ThreadStart(context.Background(), valid); !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("second Start error = %v", err)
	}
	server.assertNoMessage(t)
	if _, err := client.ThreadResume(context.Background(), ThreadResumeParams{ThreadID: "thread-foreign"}); !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("foreign Resume error = %v", err)
	}
	server.assertNoMessage(t)
	foreignRef, _ := replay.NewCodexReference("thread-foreign")
	if _, err := client.ConnectManager(foreignRef); !errors.Is(err, ErrOwnership) {
		t.Fatalf("foreign manager binding error = %v", err)
	}
}

func TestAppServerAndACPCodecsCannotBeMixed(t *testing.T) {
	headerful := []byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"t","turn":{"id":"x"}}}`)
	if _, err := parseAppServerMessage(headerful); !errors.Is(err, ErrTransport) {
		t.Fatalf("headerful App Server parse error = %v", err)
	}
	headerless := []byte(`{"method":"session/update","params":{"sessionId":"s","update":{}}}`)
	if _, err := acp.ParseMessage(headerless); !errors.Is(err, acp.ErrMalformedFrame) {
		t.Fatalf("headerless ACP parse error = %v", err)
	}
}

func TestTurnStartedAndTurnStartResponseMustAgree(t *testing.T) {
	client, server := openTestAppServer(t, &countDynamicHandler{})
	reference := startTestThread(t, client, server, "thread-agreement")
	done := make(chan error, 1)
	go func() {
		_, err := client.TurnStart(context.Background(), reference, "opaque", func(context.Context, json.RawMessage) error { return nil })
		done <- err
	}()
	request := server.read(t)
	server.sendRaw(t, json.RawMessage(`{"method":"turn/started","params":{"threadId":"thread-agreement","turn":{"id":"turn-a","status":"inProgress","items":[]}}}`))
	server.respond(t, request, json.RawMessage(`{"turn":{"id":"turn-b","status":"inProgress","items":[]}}`))
	if err := <-done; !errors.Is(err, ErrOwnership) {
		t.Fatalf("disagreeing turn identity error = %v", err)
	}
	client.mu.Lock()
	active := client.active["thread-agreement"]
	client.mu.Unlock()
	if active != nil {
		t.Fatal("disagreeing turn identity remained active")
	}
}

func TestManagerConnectionCloseStopsItsExactClient(t *testing.T) {
	client, server := openTestAppServer(t, &countDynamicHandler{})
	done := make(chan struct {
		session NativeManagerSession
		err     error
	}, 1)
	go func() {
		session, err := startCodexManagerSession(context.Background(), client, "/tmp/taskai-close")
		done <- struct {
			session NativeManagerSession
			err     error
		}{session, err}
	}()
	request := server.read(t)
	server.respond(t, request, json.RawMessage(`{"thread":{"id":"thread-stop"}}`))
	started := <-done
	if started.err != nil || started.session.Connection == nil {
		t.Fatalf("Start error = %v", started.err)
	}
	if err := started.session.Connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started.session.Connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not stop exact App Server client")
	}
	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if !closed {
		t.Fatal("manager connection Close left client live")
	}
	waitInboundCount(t, client.peer, 0)
}

func TestOnlyValidatedTurnCompletedIsTerminal(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		completion json.RawMessage
		wantErr    error
	}{
		{name: "completed", status: "completed", completion: json.RawMessage(`{"stopReason":"end_turn"}`)},
		{name: "interrupted", status: "interrupted", completion: json.RawMessage(`{"stopReason":"cancelled"}`)},
		{name: "failed", status: "failed", wantErr: ErrProcessFailed},
		{name: "unknown", status: "future_status", wantErr: ErrMalformedCompletion},
		{name: "missing_id", status: "completed", wantErr: ErrMalformedCompletion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := openTestAppServer(t, &countDynamicHandler{})
			threadID := "thread-terminal-" + test.name
			turnID := "turn-terminal-" + test.name
			reference := startTestThread(t, client, server, threadID)
			handle := startActiveCodexTurn(t, client, server, reference, threadID, turnID, func(context.Context, json.RawMessage) error { return nil })
			done := make(chan struct {
				completion json.RawMessage
				err        error
			}, 1)
			go func() {
				completion, err := handle.Wait(context.Background())
				done <- struct {
					completion json.RawMessage
					err        error
				}{completion, err}
			}()

			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": "item/completed",
				"params": map[string]any{
					"threadId": threadID, "turnId": turnID,
					"item": map[string]any{"type": "agentMessage", "status": "completed"},
				},
			}))
			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": "error",
				"params": map[string]any{"threadId": threadID, "turnId": turnID, "message": "nonterminal"},
			}))
			select {
			case early := <-done:
				t.Fatalf("nonterminal notification ended turn: %+v", early)
			case <-time.After(40 * time.Millisecond):
			}

			terminalTurn := map[string]any{
				"id": turnID, "status": test.status,
				"items": []any{map[string]any{"type": "dynamicToolCall", "arguments": "PRIVATE"}},
			}
			if test.name == "missing_id" {
				delete(terminalTurn, "id")
			}
			server.sendRaw(t, mustJSON(t, map[string]any{
				"method": appMethodTurnCompleted,
				"params": map[string]any{
					"threadId": threadID,
					"turn":     terminalTurn,
				},
			}))
			result := <-done
			if test.wantErr != nil {
				if !errors.Is(result.err, test.wantErr) || len(result.completion) != 0 {
					t.Fatalf("completion=%s error=%v, want %v", result.completion, result.err, test.wantErr)
				}
			} else if result.err != nil || !bytes.Equal(result.completion, test.completion) || bytes.Contains(result.completion, []byte("PRIVATE")) {
				t.Fatalf("completion=%s error=%v", result.completion, result.err)
			}
		})
	}
}
