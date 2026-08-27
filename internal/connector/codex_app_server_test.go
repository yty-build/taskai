package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

func TestCodexAppServerPinnedInitializeStartResumeAndRestoredToolHandler(t *testing.T) {
	t.Parallel()
	delegate := &recordingDynamicHandler{result: json.RawMessage(`{"operation":"next_answer","more":false}`)}
	client, server := openTestAppServer(t, delegate)

	startDone := make(chan struct {
		session NativeManagerSession
		err     error
	}, 1)
	go func() {
		session, err := startCodexManagerSession(context.Background(), client, "/tmp/taskai-project")
		startDone <- struct {
			session NativeManagerSession
			err     error
		}{session, err}
	}()
	startRequest := server.read(t)
	if startRequest.Method != appMethodThreadStart {
		t.Fatalf("method = %q, want thread/start", startRequest.Method)
	}
	assertNoJSONRPCHeader(t, startRequest.Raw)
	var startParams struct {
		CWD                   string        `json:"cwd"`
		DeveloperInstructions string        `json:"developerInstructions"`
		DynamicTools          []DynamicTool `json:"dynamicTools"`
	}
	if json.Unmarshal(startRequest.Params, &startParams) != nil {
		t.Fatal("invalid thread/start params")
	}
	if startParams.CWD != "/tmp/taskai-project" || startParams.DeveloperInstructions != ManagerPrefix ||
		len(startParams.DynamicTools) != 1 || startParams.DynamicTools[0].Type != "function" ||
		startParams.DynamicTools[0].Name != DelegateToolName {
		t.Fatalf("thread/start params = %+v", startParams)
	}
	server.respond(t, startRequest, json.RawMessage(`{"thread":{"id":"thread-owned"}}`))
	started := <-startDone
	if started.err != nil {
		t.Fatal(started.err)
	}
	wantPublicSession := managerPublicSessionID(started.session.NativeReference)
	if got := started.session.Connection.PublicSessionID(); got != wantPublicSession || !boundedCorrelation(got) {
		t.Fatalf("public Manager Session identity = %q, want bounded %q", got, wantPublicSession)
	}

	resumeDone := make(chan struct {
		session NativeManagerSession
		err     error
	}, 1)
	go func() {
		session, err := resumeCodexManagerSession(context.Background(), client, started.session.NativeReference)
		resumeDone <- struct {
			session NativeManagerSession
			err     error
		}{session, err}
	}()
	resumeRequest := server.read(t)
	if resumeRequest.Method != appMethodThreadResume {
		t.Fatalf("method = %q, want thread/resume", resumeRequest.Method)
	}
	var resumeFields map[string]json.RawMessage
	if json.Unmarshal(resumeRequest.Params, &resumeFields) != nil || len(resumeFields) != 1 || resumeFields["threadId"] == nil {
		t.Fatalf("Resume fields = %v, want only threadId", reflect.ValueOf(resumeFields).MapKeys())
	}
	if resumeFields["dynamicTools"] != nil || resumeFields["developerInstructions"] != nil {
		t.Fatalf("Resume reinjected configuration: %s", resumeRequest.Params)
	}
	server.respond(t, resumeRequest, json.RawMessage(`{"thread":{"id":"thread-owned"}}`))
	resumed := <-resumeDone
	if resumed.err != nil {
		t.Fatal(resumed.err)
	}
	if !bytes.Equal(resumed.session.NativeReference.Bytes(), started.session.NativeReference.Bytes()) {
		t.Fatal("Resume changed opaque reference")
	}
	if resumed.session.Connection == nil || resumed.session.Connection != started.session.Connection {
		t.Fatal("same-client exact Resume did not retain the live manager connection")
	}
	if got := resumed.session.Connection.PublicSessionID(); got != wantPublicSession {
		t.Fatalf("Resume changed public Manager Session identity to %q", got)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, err := resumed.session.Connection.Prompt(context.Background(), "invoke restored capability", func(json.RawMessage) error { return nil })
		promptDone <- err
	}()
	turnStart := server.read(t)
	if turnStart.Method != appMethodTurnStart {
		t.Fatalf("post-Resume method = %q, want turn/start", turnStart.Method)
	}
	server.sendRaw(t, json.RawMessage(`{"method":"turn/started","params":{"threadId":"thread-owned","turn":{"id":"turn-1","status":"inProgress","items":[]}}}`))
	server.respond(t, turnStart, json.RawMessage(`{"turn":{"id":"turn-1","status":"inProgress","items":[]}}`))

	toolArguments := json.RawMessage(`{"operation":"next_answer"}`)
	toolRequest := appServerMessage{
		ID: json.RawMessage(`"tool-rpc-id"`), Method: appMethodDynamicTool,
		Params: mustJSON(t, map[string]any{
			"arguments": json.RawMessage(toolArguments), "callId": "call-1", "threadId": "thread-owned",
			"tool": DelegateToolName, "turnId": "turn-1",
		}),
	}
	server.send(t, toolRequest)
	toolResponse := server.read(t)
	if toolResponse.kind() != appResponse || !bytes.Equal(toolResponse.ID, toolRequest.ID) || toolResponse.Error != nil {
		t.Fatalf("tool response correlation = %+v", toolResponse)
	}
	var nativeResult struct {
		Success      bool `json:"success"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentItems"`
	}
	if json.Unmarshal(toolResponse.Result, &nativeResult) != nil || !nativeResult.Success || len(nativeResult.ContentItems) != 1 ||
		nativeResult.ContentItems[0].Type != "inputText" || nativeResult.ContentItems[0].Text != string(delegate.result) {
		t.Fatalf("dynamic tool result = %s", toolResponse.Result)
	}
	delegate.mu.Lock()
	if delegate.calls != 1 || !bytes.Equal(delegate.argument, toolArguments) {
		t.Fatalf("delegate calls=%d arguments=%s", delegate.calls, delegate.argument)
	}
	delegate.mu.Unlock()
	server.sendRaw(t, json.RawMessage(`{"method":"turn/completed","params":{"threadId":"thread-owned","turn":{"id":"turn-1","status":"completed","items":[]}}}`))
	if err := <-promptDone; err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerTurnStreamCompletionInterruptAndThreadRead(t *testing.T) {
	t.Parallel()
	client, server := openTestAppServer(t, &recordingDynamicHandler{result: json.RawMessage(`{"more":false}`)})
	thread := startTestThread(t, client, server, "thread-stream")

	var framesMu sync.Mutex
	var frames []json.RawMessage
	startDone := make(chan struct {
		handle *CodexTurnHandle
		err    error
	}, 1)
	go func() {
		handle, err := client.TurnStart(context.Background(), thread, "opaque manager prompt", func(_ context.Context, raw json.RawMessage) error {
			framesMu.Lock()
			frames = append(frames, append(json.RawMessage(nil), raw...))
			framesMu.Unlock()
			return nil
		})
		startDone <- struct {
			handle *CodexTurnHandle
			err    error
		}{handle, err}
	}()
	turnRequest := server.read(t)
	if turnRequest.Method != appMethodTurnStart {
		t.Fatalf("method = %q, want turn/start", turnRequest.Method)
	}
	var turnParams struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if json.Unmarshal(turnRequest.Params, &turnParams) != nil || turnParams.ThreadID != "thread-stream" ||
		len(turnParams.Input) != 1 || turnParams.Input[0].Type != "text" || turnParams.Input[0].Text != "opaque manager prompt" {
		t.Fatalf("turn/start params = %+v", turnParams)
	}
	startedFrame := json.RawMessage(`{"method":"turn/started", "params":{"threadId":"thread-stream","turn":{"id":"turn-live","status":"inProgress","items":[]}}}`)
	unknownFrame := json.RawMessage(`{"method":"item/future/public", "params":{"threadId":"thread-stream","turnId":"turn-live","opaque":[3,2,1]}}`)
	publicFirst := json.RawMessage(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-stream","turnId":"turn-live","itemId":"message-live","delta":"public-first"}}`)
	publicSecond := json.RawMessage(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-stream","turnId":"turn-live","itemId":"message-live","delta":"public-second"}}`)
	server.sendRaw(t, startedFrame)
	server.sendRaw(t, unknownFrame)
	server.sendRaw(t, publicFirst)
	server.sendRaw(t, publicSecond)
	server.respond(t, turnRequest, json.RawMessage(`{"turn":{"id":"turn-live","status":"inProgress","items":[]}}`))
	started := <-startDone
	if started.err != nil || started.handle.TurnID() != "turn-live" {
		t.Fatalf("TurnStart handle=%v error=%v", started.handle, started.err)
	}
	if !bytes.Equal(started.handle.NativeReference().Bytes(), thread.Bytes()) {
		t.Fatal("turn handle changed native reference")
	}

	completedFrame := json.RawMessage(`{"method":"turn/completed", "params":{"threadId":"thread-stream","turn":{"id":"turn-live","status":"completed","items":[]}}}`)
	server.sendRaw(t, completedFrame)
	completion, err := started.handle.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(completion, json.RawMessage(`{"stopReason":"end_turn"}`)) {
		t.Fatalf("public completion = %s", completion)
	}
	framesMu.Lock()
	gotFrames := append([]json.RawMessage(nil), frames...)
	framesMu.Unlock()
	if len(gotFrames) != 2 {
		t.Fatalf("stream frames=%d, want 2 nonterminal ACP frames: %s", len(gotFrames), gotFrames)
	}
	for index, frame := range gotFrames {
		parsed, parseErr := acp.ParseMessage(frame)
		if parseErr != nil || parsed.Method != acp.MethodSessionUpdate || parsed.JSONRPC != "2.0" {
			t.Fatalf("frame %d is not public ACP: %s error=%v", index, frame, parseErr)
		}
		if bytes.Contains(frame, []byte(`"threadId"`)) || bytes.Contains(frame, []byte(`"jsonrpc":null`)) {
			t.Fatalf("frame %d leaked private envelope mechanics: %s", index, frame)
		}
	}
	if !bytes.Contains(gotFrames[0], []byte("public-first")) || !bytes.Contains(gotFrames[1], []byte("public-second")) ||
		bytes.Contains(gotFrames[0], []byte("item/future/public")) || bytes.Contains(gotFrames[1], []byte("item/future/public")) {
		t.Fatalf("mapped public ACP order/filtering failed: %s", gotFrames)
	}

	interruptStart := make(chan *CodexTurnHandle, 1)
	go func() {
		handle, _ := client.TurnStart(context.Background(), thread, "interrupt me", func(context.Context, json.RawMessage) error { return nil })
		interruptStart <- handle
	}()
	secondStart := server.read(t)
	server.respond(t, secondStart, json.RawMessage(`{"turn":{"id":"turn-interrupt","status":"inProgress","items":[]}}`))
	interruptHandle := <-interruptStart
	interruptDone := make(chan error, 1)
	go func() { interruptDone <- interruptHandle.Interrupt(context.Background()) }()
	interruptRequest := server.read(t)
	if interruptRequest.Method != appMethodTurnInterrupt || !bytes.Equal(interruptRequest.Params, json.RawMessage(`{"threadId":"thread-stream","turnId":"turn-interrupt"}`)) {
		t.Fatalf("turn/interrupt request = %s %s", interruptRequest.Method, interruptRequest.Params)
	}
	server.respond(t, interruptRequest, json.RawMessage(`{}`))
	if err := <-interruptDone; err != nil {
		t.Fatal(err)
	}
	server.sendRaw(t, json.RawMessage(`{"method":"turn/completed","params":{"threadId":"thread-stream","turn":{"id":"turn-interrupt","status":"interrupted","items":[]}}}`))
	if _, err := interruptHandle.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	readDone := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := client.ThreadRead(context.Background(), "thread-stream")
		readDone <- struct {
			raw json.RawMessage
			err error
		}{raw, err}
	}()
	readRequest := server.read(t)
	if readRequest.Method != appMethodThreadRead || !bytes.Equal(readRequest.Params, json.RawMessage(`{"threadId":"thread-stream","includeTurns":true}`)) {
		t.Fatalf("thread/read request = %s %s", readRequest.Method, readRequest.Params)
	}
	threadRaw := json.RawMessage(`{ "id":"thread-stream", "turns":[{"id":"turn-live","status":"completed","items":[{"type":"future_item", "opaque":true}]}] }`)
	server.respondRaw(t, readRequest.ID, append(append([]byte(`{"thread":`), threadRaw...), '}'))
	read := <-readDone
	if read.err != nil || !bytes.Equal(read.raw, threadRaw) {
		t.Fatalf("ThreadRead raw=%s error=%v", read.raw, read.err)
	}
}

func TestCodexAppServerThreadReadFeedsRestartedOwnedReplayWithoutTransientCounter(t *testing.T) {
	t.Parallel()
	client, server := openTestAppServer(t, &recordingDynamicHandler{result: json.RawMessage(`{"more":false}`)})
	service, err := replay.NewService(replay.Dependencies{Codex: client}, replay.Limits{
		MaxTargets: 2, MaxCount: 2, MaxSourceBytes: 1 << 20, MaxRecordBytes: 64 << 10,
		MaxRecords: 16, MaxSelectedBytes: 64 << 10, MaxBatchBytes: 128 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	versions := Versions{ACPProtocol: 1, Connector: "app-server-replay-test"}
	profile := NewConformedProfile(
		"codex-test", "Codex test", versions,
		Conformance{
			Exact: versions, Proven: Capabilities{Subordinate: true, Replay: true},
			ReplayProvider: replay.ProviderCodex, ReplayVersion: CodexAppServerVersion,
		},
		nil, unusedChildFactory{}, nil, nil, nil,
		ReplayBinding{Provider: replay.ProviderCodex, InstalledVersion: CodexAppServerVersion, Port: service},
	)
	registry := NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	delegator, err := NewDelegator(context.Background(), "manager", registry, ChildEventSinkFunc(func(context.Context, ChildEvent) error { return nil }), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delegator.Close() })
	reference, _ := replay.NewCodexReference("thread-restored")
	if err := delegator.RestoreOwnedChild("child-restored", "codex-test", reference); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct {
		raw json.RawMessage
		err error
	}, 1)
	go func() {
		raw, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-restored"]}`))
		done <- struct {
			raw json.RawMessage
			err error
		}{raw, err}
	}()
	readRequest := server.read(t)
	if readRequest.Method != appMethodThreadRead || !bytes.Equal(readRequest.Params, json.RawMessage(`{"threadId":"thread-restored","includeTurns":true}`)) {
		t.Fatalf("native Replay read = %s %s", readRequest.Method, readRequest.Params)
	}
	server.respond(t, readRequest, json.RawMessage(`{"thread":{"id":"thread-restored","turns":[{"id":"older","status":"completed","items":[{"type":"native","sequence":1}]},{"id":"latest","status":"completed","items":[{"type":"native","sequence":2}]}]}}`))
	replayed := <-done
	if replayed.err != nil {
		t.Fatal(replayed.err)
	}
	if bytes.Contains(replayed.raw, []byte(`"sequence":1`)) || !bytes.Contains(replayed.raw, []byte(`"sequence":2`)) {
		t.Fatalf("Replay 1 did not select exact final completed range: %s", replayed.raw)
	}
}

func TestCodexAppServerFailsClosedOnVersionMismatchAndForeignToolThread(t *testing.T) {
	t.Parallel()
	if _, err := openCodexAppServer(context.Background(), bytes.NewReader(nil), ioDiscardWriter{}, "0.151.0", &recordingDynamicHandler{}); !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("version mismatch error = %v", err)
	}
	if _, err := LaunchCodexAppServer(context.Background(), CodexAppServerConfig{
		Command:          CommandSpec{Executable: "codex", Args: []string{"wrong-subcommand"}},
		InstalledVersion: CodexAppServerVersion,
		Delegate:         &recordingDynamicHandler{},
	}); !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("wrong literal App Server command error = %v", err)
	}
	client, server := openTestAppServer(t, &recordingDynamicHandler{result: json.RawMessage(`{"more":false}`)})
	request := appServerMessage{
		ID: json.RawMessage(`91`), Method: appMethodDynamicTool,
		Params: mustJSON(t, map[string]any{
			"arguments": json.RawMessage(`{"operation":"next_answer"}`), "callId": "call", "threadId": "foreign",
			"tool": DelegateToolName, "turnId": "turn",
		}),
	}
	server.send(t, request)
	response := server.read(t)
	var result struct {
		Success bool `json:"success"`
	}
	if json.Unmarshal(response.Result, &result) != nil || result.Success {
		t.Fatalf("foreign tool response = %s", response.Result)
	}
	if bytes.Contains(response.Raw, []byte("foreign")) || bytes.Contains(response.Raw, []byte(`"arguments"`)) {
		t.Fatalf("tool ownership failure echoed private request content: %s", response.Raw)
	}
	_ = client
}

func TestCodexAppServerInitializeRequiresExactObservedVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  json.RawMessage
		want    string
		wantErr bool
	}{
		{name: "exact public identity", result: json.RawMessage(`{"userAgent":"codex-cli/0.150.1","platformFamily":"unix"}`), want: CodexAppServerVersion},
		{name: "missing", result: json.RawMessage(`{"platformFamily":"unix"}`), wantErr: true},
		{name: "wrong type", result: json.RawMessage(`{"userAgent":1501}`), wantErr: true},
		{name: "wrong provider", result: json.RawMessage(`{"userAgent":"codex/0.150.1"}`), wantErr: true},
		{name: "mismatch", result: json.RawMessage(`{"userAgent":"codex-cli/0.151.0"}`), wantErr: true},
		{name: "suffix", result: json.RawMessage(`{"userAgent":"codex-cli/0.150.1-dev"}`), wantErr: true},
		{name: "duplicate", result: json.RawMessage(`{"userAgent":"codex-cli/0.151.0","userAgent":"codex-cli/0.150.1"}`), wantErr: true},
		{name: "non object", result: json.RawMessage(`[]`), wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := observedCodexAppServerVersion(test.result)
			if test.wantErr {
				if !errors.Is(err, ErrProviderUnsupported) || got != "" {
					t.Fatalf("observed version = %q, %v", got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("observed version = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestCodexAppServerRejectsHandshakeMismatchBeforeInitialized(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	server := &appHarness{conn: serverConn, reader: bufio.NewReaderSize(serverConn, 64<<10)}
	done := make(chan error, 1)
	go func() {
		client, err := openCodexAppServer(
			context.Background(), clientConn, clientConn, CodexAppServerVersion, &recordingDynamicHandler{},
		)
		if client != nil {
			_ = client.Close()
		}
		done <- err
	}()
	initialize := server.read(t)
	if initialize.Method != appMethodInitialize {
		t.Fatalf("first method = %q, want initialize", initialize.Method)
	}
	server.respond(t, initialize, json.RawMessage(`{"userAgent":"codex-cli/0.151.0"}`))
	if err := <-done; !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("mismatched handshake error = %v", err)
	}
	_ = serverConn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if raw, err := readAppServerLine(server.reader); err == nil {
		t.Fatalf("mismatched handshake sent a post-initialize frame: %s", raw)
	}
}

func TestCodexAppServerSilentInitializeIsFinitelyBounded(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	server := &appHarness{conn: serverConn, reader: bufio.NewReaderSize(serverConn, 64<<10)}
	done := make(chan error, 1)
	go func() {
		client, err := openCodexAppServerWithin(
			context.Background(), clientConn, clientConn, CodexAppServerVersion,
			&recordingDynamicHandler{}, 20*time.Millisecond,
		)
		if client != nil {
			_ = client.Close()
		}
		done <- err
	}()
	initialize := server.read(t)
	if initialize.Method != appMethodInitialize {
		t.Fatalf("first method = %q, want initialize", initialize.Method)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrTransport) {
			t.Fatalf("silent initialize error = %v, want transport", err)
		}
	case <-time.After(time.Second):
		t.Fatal("silent initialize exceeded its finite handshake deadline")
	}
}

func TestDynamicToolResultBudgetIncludesEscapingAndFullRPCHeadroom(t *testing.T) {
	t.Parallel()
	result := json.RawMessage(`{"opaque":"` + strings.Repeat("x", (128<<10)-DynamicToolRPCHeadroom-(2<<10)) + `"}`)
	toolWire, err := marshalDynamicToolResponse(result, true)
	if err != nil {
		t.Fatalf("bounded tool result error = %v", err)
	}
	id, _ := json.Marshal(strings.Repeat("i", 240))
	response, err := json.Marshal(appServerMessage{ID: id, Result: toolWire})
	if err != nil {
		t.Fatal(err)
	}
	if len(response) > 128<<10 || len(response)-len(toolWire) >= DynamicToolRPCHeadroom {
		t.Fatalf("RPC response=%d wrapper=%d headroom=%d", len(response), len(toolWire), DynamicToolRPCHeadroom)
	}
	escaped := json.RawMessage(`{"opaque":"` + strings.Repeat(`\\`, 40<<10) + `"}`)
	if _, err := marshalDynamicToolResponse(escaped, true); !errors.Is(err, ErrCapacity) {
		t.Fatalf("escape-heavy result error = %v, want ErrCapacity", err)
	}
}

func startTestThread(t *testing.T, client *CodexAppServerClient, server *appHarness, threadID string) replay.OpaqueRef {
	t.Helper()
	done := make(chan struct {
		session CodexThreadSession
		err     error
	}, 1)
	go func() {
		session, err := client.ThreadStart(context.Background(), ThreadStartParams{
			CWD: "/tmp/taskai-project", DeveloperInstructions: ManagerPrefix,
			DynamicTools: []DynamicTool{DelegateCapability()},
		})
		done <- struct {
			session CodexThreadSession
			err     error
		}{session, err}
	}()
	request := server.read(t)
	server.respond(t, request, mustJSON(t, map[string]any{"thread": map[string]any{"id": threadID}}))
	started := <-done
	if started.err != nil {
		t.Fatal(started.err)
	}
	reference, err := replay.NewCodexReference(started.session.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

type recordingDynamicHandler struct {
	mu       sync.Mutex
	calls    int
	argument json.RawMessage
	result   json.RawMessage
}

func (h *recordingDynamicHandler) Invoke(_ context.Context, argument json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.argument = append(json.RawMessage(nil), argument...)
	if len(h.result) == 0 {
		return nil, ErrInvalidOperation
	}
	return append(json.RawMessage(nil), h.result...), nil
}

type appHarness struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

func openTestAppServer(t *testing.T, delegate DynamicToolHandler) (*CodexAppServerClient, *appHarness) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := &appHarness{conn: serverConn, reader: bufio.NewReaderSize(serverConn, 64<<10)}
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	done := make(chan struct {
		client *CodexAppServerClient
		err    error
	}, 1)
	go func() {
		client, err := openCodexAppServer(context.Background(), clientConn, clientConn, CodexAppServerVersion, delegate)
		done <- struct {
			client *CodexAppServerClient
			err    error
		}{client, err}
	}()
	initialize := server.read(t)
	if initialize.Method != appMethodInitialize {
		t.Fatalf("first method = %q, want initialize", initialize.Method)
	}
	assertNoJSONRPCHeader(t, initialize.Raw)
	var params struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if json.Unmarshal(initialize.Params, &params) != nil || params.ClientInfo.Name != "taskai" || !params.Capabilities.ExperimentalAPI {
		t.Fatalf("initialize params = %s", initialize.Params)
	}
	server.respond(t, initialize, json.RawMessage(`{"userAgent":"codex-cli/0.150.1","platformFamily":"unix","platformOs":"macos","codexHome":"/opaque"}`))
	initialized := server.read(t)
	if initialized.Method != appMethodInitialized || initialized.kind() != appNotification || len(initialized.Params) != 0 {
		t.Fatalf("second message = %s", initialized.Raw)
	}
	opened := <-done
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	t.Cleanup(func() { _ = opened.client.Close() })
	return opened.client, server
}

func (s *appHarness) read(t *testing.T) appServerMessage {
	t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	raw, err := readAppServerLine(s.reader)
	_ = s.conn.SetReadDeadline(time.Time{})
	if err != nil {
		t.Fatalf("server read error = %v", err)
	}
	message, err := parseAppServerMessage(raw)
	if err != nil {
		t.Fatalf("server parse error = %v: %s", err, raw)
	}
	return message
}

func (s *appHarness) send(t *testing.T, message appServerMessage) {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	s.sendRaw(t, raw)
}

func (s *appHarness) sendRaw(t *testing.T, raw json.RawMessage) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	framed := append(append([]byte(nil), raw...), '\n')
	_ = s.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	for len(framed) != 0 {
		written, err := s.conn.Write(framed)
		if err != nil || written == 0 {
			t.Fatalf("server write error = %v", err)
		}
		framed = framed[written:]
	}
	_ = s.conn.SetWriteDeadline(time.Time{})
}

func (s *appHarness) respond(t *testing.T, request appServerMessage, result json.RawMessage) {
	t.Helper()
	s.send(t, appServerMessage{ID: append(json.RawMessage(nil), request.ID...), Result: result})
}

func (s *appHarness) respondRaw(t *testing.T, id json.RawMessage, result json.RawMessage) {
	t.Helper()
	wire := append([]byte(`{"id":`), id...)
	wire = append(wire, []byte(`,"result":`)...)
	wire = append(wire, result...)
	wire = append(wire, '}')
	s.sendRaw(t, wire)
}

func assertNoJSONRPCHeader(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatalf("invalid wire: %s", raw)
	}
	if fields["jsonrpc"] != nil {
		t.Fatalf("App Server wire added JSON-RPC header: %s", raw)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type ioDiscardWriter struct{}

func (ioDiscardWriter) Write(value []byte) (int, error) { return len(value), nil }

type unusedChildFactory struct{}

func (unusedChildFactory) New(context.Context) (ChildPort, error) {
	return nil, errors.New("unused test child factory")
}
