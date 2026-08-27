package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

const (
	appMethodInitialize    = "initialize"
	appMethodInitialized   = "initialized"
	appMethodThreadStart   = "thread/start"
	appMethodThreadResume  = "thread/resume"
	appMethodThreadRead    = "thread/read"
	appMethodTurnStart     = "turn/start"
	appMethodTurnInterrupt = "turn/interrupt"
	appMethodTurnStarted   = "turn/started"
	appMethodTurnCompleted = "turn/completed"
	appMethodDynamicTool   = "item/tool/call"
	appMethodRequestDone   = "serverRequest/resolved"

	// Live App Server traffic is a small conformed public boundary. Only an
	// exact pending thread/read response may use the independently bounded
	// provider-native history allowance below.
	MaxAppServerLiveFrameBytes = acp.MaxFrameBytes
	MaxAppServerHistoryBytes   = 128 << 20
	// Capability JSON is embedded as a JSON string inside a dynamic-tool result
	// and then inside an RPC response. This fixed allowance covers the bounded
	// request ID and both envelopes after exact JSON escaping.
	DynamicToolRPCHeadroom = 1024
	MaxManagerToolCalls    = 64
	MaxAppServerInbound    = 64

	// CodexAppServerHandshakeTimeout bounds only the public initialize
	// exchange. The successfully initialized peer retains its caller-owned
	// lifetime context afterward.
	CodexAppServerHandshakeTimeout = 10 * time.Second
)

// DynamicToolHandler is implemented by the connector-owned Delegator. Its
// arguments and result stay on the manager/provider boundary and never enter
// TaskAI core event or persistence ports.
type DynamicToolHandler interface {
	Invoke(context.Context, json.RawMessage) (json.RawMessage, error)
}

// CodexAppServerConfig launches the pinned public Codex App Server command by
// literal executable and argv. InstalledVersion must be exactly 0.150.1.
type CodexAppServerConfig struct {
	Command          CommandSpec
	InstalledVersion string
	Delegate         DynamicToolHandler
}

// LaunchCodexAppServer starts and initializes one headerless JSONL App Server
// peer. It opts into the pinned experimental API before sending initialized.
func LaunchCodexAppServer(ctx context.Context, config CodexAppServerConfig) (*CodexAppServerClient, error) {
	if config.InstalledVersion != CodexAppServerVersion || config.Delegate == nil ||
		len(config.Command.Args) != 1 || config.Command.Args[0] != "app-server" {
		return nil, ErrProviderUnsupported
	}
	process, err := (Launcher{}).Start(ctx, config.Command)
	if err != nil {
		return nil, err
	}
	client, err := openCodexAppServer(ctx, process.Stdout, process.Stdin, config.InstalledVersion, config.Delegate)
	if err != nil {
		_ = process.Close()
		return nil, err
	}
	client.process = process
	return client, nil
}

// CodexAppServerClient is the live 0.150.1 Start/Resume, turn, tool-call, and
// thread/read port. It does not expose its private JSONL peer to TaskAI core.
type CodexAppServerClient struct {
	version  string
	delegate DynamicToolHandler
	peer     *appServerPeer
	process  *Process

	mu          sync.Mutex
	initialized bool
	closed      bool
	claiming    bool
	ownedThread string
	connection  *codexManagerConnection
	active      map[string]*codexTurnState
	closeOnce   sync.Once
}

func openCodexAppServer(
	ctx context.Context,
	r io.Reader,
	w io.Writer,
	version string,
	delegate DynamicToolHandler,
) (*CodexAppServerClient, error) {
	return openCodexAppServerWithin(ctx, r, w, version, delegate, CodexAppServerHandshakeTimeout)
}

func openCodexAppServerWithin(
	ctx context.Context,
	r io.Reader,
	w io.Writer,
	version string,
	delegate DynamicToolHandler,
	handshakeTimeout time.Duration,
) (*CodexAppServerClient, error) {
	if ctx == nil || version != CodexAppServerVersion || delegate == nil || r == nil || w == nil || handshakeTimeout <= 0 {
		return nil, ErrProviderUnsupported
	}
	client := &CodexAppServerClient{
		delegate: delegate,
		active:   make(map[string]*codexTurnState),
	}
	client.peer = newAppServerPeer(r, w, client.handleRequest, client.handleNotification)
	client.peer.start(ctx)

	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, handshakeTimeout)
	defer cancelHandshake()
	params := json.RawMessage(`{"clientInfo":{"name":"taskai","version":"1"},"capabilities":{"experimentalApi":true}}`)
	result, err := client.peer.call(handshakeCtx, appMethodInitialize, params)
	if err != nil || !validJSONObject(result) {
		_ = client.Close()
		return nil, &BoundaryError{Stage: "app server initialize", Err: ErrTransport}
	}
	observedVersion, err := observedCodexAppServerVersion(result)
	if err != nil || observedVersion != version {
		_ = client.Close()
		return nil, &BoundaryError{Stage: "app server version", Err: ErrProviderUnsupported}
	}
	if err := client.peer.notify(appMethodInitialized, nil); err != nil {
		_ = client.Close()
		return nil, &BoundaryError{Stage: "app server initialized", Err: ErrTransport}
	}
	client.mu.Lock()
	client.version = observedVersion
	client.initialized = true
	client.mu.Unlock()
	return client, nil
}

// observedCodexAppServerVersion accepts only the public pinned user-agent
// identity returned by Codex App Server's initialize method. The configured
// conformance version is an expectation, never evidence about the executable
// that was actually launched.
func observedCodexAppServerVersion(result json.RawMessage) (string, error) {
	if !validJSONObject(result) {
		return "", ErrProviderUnsupported
	}
	decoder := json.NewDecoder(bytes.NewReader(result))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return "", ErrProviderUnsupported
	}
	var userAgent string
	found := false
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return "", ErrProviderUnsupported
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", ErrProviderUnsupported
		}
		if name != "userAgent" {
			continue
		}
		if found || json.Unmarshal(value, &userAgent) != nil {
			return "", ErrProviderUnsupported
		}
		found = true
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !found {
		return "", ErrProviderUnsupported
	}
	const prefix = "codex-cli/"
	if userAgent != prefix+CodexAppServerVersion {
		return "", ErrProviderUnsupported
	}
	return userAgent[len(prefix):], nil
}

func (c *CodexAppServerClient) Version() string {
	if c == nil {
		return ""
	}
	return c.version
}

func (c *CodexAppServerClient) DelegateHandlerRegistered() bool {
	return c != nil && c.version == CodexAppServerVersion && c.delegate != nil
}

func (c *CodexAppServerClient) ThreadStart(ctx context.Context, params ThreadStartParams) (CodexThreadSession, error) {
	if !c.ready() || !validManagerStart(params) || !c.reserveThread("") {
		return CodexThreadSession{}, ErrProviderUnsupported
	}
	committed := false
	defer func() { c.releaseThreadReservation("", committed) }()
	raw, err := json.Marshal(params)
	if err != nil {
		return CodexThreadSession{}, ErrProviderUnsupported
	}
	result, err := c.peer.call(ctx, appMethodThreadStart, raw)
	if err != nil {
		return CodexThreadSession{}, &BoundaryError{Stage: "manager start", Err: ErrTransport}
	}
	threadID, err := appThreadID(result)
	if err != nil {
		return CodexThreadSession{}, &BoundaryError{Stage: "manager start response", Err: ErrMalformedCompletion}
	}
	c.mu.Lock()
	if c.ownedThread != "" && c.ownedThread != threadID {
		c.mu.Unlock()
		return CodexThreadSession{}, &BoundaryError{Stage: "manager start ownership", Err: ErrOwnership}
	}
	c.ownedThread = threadID
	committed = true
	c.mu.Unlock()
	return CodexThreadSession{ThreadID: threadID}, nil
}

func (c *CodexAppServerClient) ThreadResume(ctx context.Context, params ThreadResumeParams) (CodexThreadSession, error) {
	if !c.ready() || params.ThreadID == "" || len(params.ThreadID) > MaxCorrelationIDBytes ||
		!c.reserveThread(params.ThreadID) {
		return CodexThreadSession{}, ErrProviderUnsupported
	}
	committed := false
	defer func() { c.releaseThreadReservation(params.ThreadID, committed) }()
	// ThreadResumeParams contains only threadId. The pinned server restores the
	// stored dynamic tool; this client never resends it.
	raw, err := json.Marshal(params)
	if err != nil {
		return CodexThreadSession{}, ErrProviderUnsupported
	}
	result, err := c.peer.call(ctx, appMethodThreadResume, raw)
	if err != nil {
		return CodexThreadSession{}, &BoundaryError{Stage: "manager resume", Err: ErrTransport}
	}
	threadID, err := appThreadID(result)
	if err != nil || threadID != params.ThreadID {
		return CodexThreadSession{}, &BoundaryError{Stage: "manager resume response", Err: ErrMalformedCompletion}
	}
	c.mu.Lock()
	c.ownedThread = threadID
	committed = true
	c.mu.Unlock()
	return CodexThreadSession{ThreadID: threadID}, nil
}

func validManagerStart(params ThreadStartParams) bool {
	want := DelegateCapability()
	if params.CWD == "" || !filepath.IsAbs(params.CWD) || params.DeveloperInstructions != ManagerPrefix ||
		len(params.DynamicTools) != 1 {
		return false
	}
	got := params.DynamicTools[0]
	return got.Type == want.Type && got.Name == want.Name && got.Description == want.Description &&
		bytes.Equal(got.InputSchema, want.InputSchema)
}

// reserveThread serializes Start/Resume and enforces one manager thread per
// App Server client. Resume may target only the already-owned exact thread.
func (c *CodexAppServerClient) reserveThread(resumeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized || c.closed || c.claiming {
		return false
	}
	if resumeID == "" {
		if c.ownedThread != "" {
			return false
		}
	} else if c.ownedThread != "" && c.ownedThread != resumeID {
		return false
	}
	c.claiming = true
	return true
}

func (c *CodexAppServerClient) releaseThreadReservation(expected string, committed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claiming = false
	if !committed && expected == "" && c.ownedThread == "" {
		c.connection = nil
	}
}

func (c *CodexAppServerClient) ready() bool {
	if c == nil || c.peer == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized && !c.closed && c.delegate != nil && c.version == CodexAppServerVersion
}

func appThreadID(result json.RawMessage) (string, error) {
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &response) != nil || response.Thread.ID == "" || len(response.Thread.ID) > MaxCorrelationIDBytes {
		return "", ErrMalformedCompletion
	}
	return response.Thread.ID, nil
}

// ThreadRead implements replay.CodexThreadReader using exactly
// thread/read(includeTurns=true). It returns the raw thread object, not the RPC
// envelope, and never enumerates or searches other threads.
func (c *CodexAppServerClient) ThreadRead(ctx context.Context, threadID string) (json.RawMessage, error) {
	if !c.ready() || threadID == "" || len(threadID) > MaxCorrelationIDBytes {
		return nil, ErrProviderUnsupported
	}
	c.mu.Lock()
	ownedThread := c.ownedThread
	c.mu.Unlock()
	if ownedThread != "" && ownedThread != threadID {
		return nil, ErrOwnership
	}
	params, _ := json.Marshal(struct {
		ThreadID     string `json:"threadId"`
		IncludeTurns bool   `json:"includeTurns"`
	}{ThreadID: threadID, IncludeTurns: true})
	result, err := c.peer.call(ctx, appMethodThreadRead, params)
	if err != nil {
		return nil, &BoundaryError{Stage: "thread read", Err: ErrTransport}
	}
	var response struct {
		Thread json.RawMessage `json:"thread"`
	}
	if json.Unmarshal(result, &response) != nil || len(response.Thread) == 0 || !validJSONObject(response.Thread) {
		return nil, &BoundaryError{Stage: "thread read response", Err: ErrMalformedCompletion}
	}
	var identity struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response.Thread, &identity) != nil || identity.ID != threadID {
		return nil, &BoundaryError{Stage: "thread read identity", Err: ErrOwnership}
	}
	return append(json.RawMessage(nil), response.Thread...), nil
}

// CodexTurnSink receives bounded standard ACP JSON-RPC frames in native event
// order. Provider-private App Server envelopes never cross this boundary.
type CodexTurnSink func(context.Context, json.RawMessage) error

type codexTurnState struct {
	threadID      string
	publicSession string
	turnID        string
	sink          CodexTurnSink
	done          chan struct{}
	terminal      json.RawMessage
	err           error
	closed        bool
	toolRequests  map[string]struct{}
	toolCallIDs   map[string]struct{}
}

// CodexTurnHandle is a transient, process-owned correlation handle. It is not
// a persistence entity and contains no prompt, frame, or result content.
type CodexTurnHandle struct {
	client    *CodexAppServerClient
	state     *codexTurnState
	reference replay.OpaqueRef
	turnID    string
}

func (h *CodexTurnHandle) TurnID() string {
	if h == nil {
		return ""
	}
	return h.turnID
}

func (h *CodexTurnHandle) NativeReference() replay.OpaqueRef {
	if h == nil {
		return replay.OpaqueRef{}
	}
	return replay.OpaqueRefFromBytes(h.reference.Bytes())
}

// TurnStart begins one provider turn on an exact owned thread. The prompt is
// transient and is supplied once as native text input.
func (c *CodexAppServerClient) TurnStart(
	ctx context.Context,
	reference replay.OpaqueRef,
	prompt string,
	sink CodexTurnSink,
) (*CodexTurnHandle, error) {
	if !c.ready() || sink == nil || len(prompt) > acp.MaxFrameBytes {
		return nil, ErrInvalidOperation
	}
	threadID, err := replay.ResolveCodexReference(reference)
	if err != nil {
		return nil, &BoundaryError{Stage: "manager reference", Err: ErrProviderUnsupported}
	}
	state := &codexTurnState{
		threadID: threadID, publicSession: managerPublicSessionID(reference), sink: sink, done: make(chan struct{}),
		toolRequests: make(map[string]struct{}), toolCallIDs: make(map[string]struct{}),
	}
	c.mu.Lock()
	if c.ownedThread != threadID {
		c.mu.Unlock()
		return nil, ErrOwnership
	}
	if c.active[threadID] != nil {
		c.mu.Unlock()
		return nil, ErrChildBusy
	}
	c.active[threadID] = state
	c.mu.Unlock()

	params, _ := json.Marshal(struct {
		ThreadID string `json:"threadId"`
		Input    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}{
		ThreadID: threadID,
		Input: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: prompt}},
	})
	result, err := c.peer.call(ctx, appMethodTurnStart, params)
	if err != nil {
		c.removeTurn(state, &BoundaryError{Stage: "turn start", Err: ErrTransport})
		return nil, &BoundaryError{Stage: "turn start", Err: ErrTransport}
	}
	var response struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &response) != nil || response.Turn.ID == "" || len(response.Turn.ID) > MaxCorrelationIDBytes ||
		response.Turn.Status != "inProgress" {
		c.removeTurn(state, ErrMalformedCompletion)
		return nil, &BoundaryError{Stage: "turn start response", Err: ErrMalformedCompletion}
	}
	c.mu.Lock()
	if state.turnID != "" && state.turnID != response.Turn.ID {
		c.mu.Unlock()
		c.removeTurn(state, ErrOwnership)
		return nil, &BoundaryError{Stage: "turn identity", Err: ErrOwnership}
	}
	state.turnID = response.Turn.ID
	c.mu.Unlock()
	return &CodexTurnHandle{
		client: c, state: state,
		reference: replay.OpaqueRefFromBytes(reference.Bytes()), turnID: response.Turn.ID,
	}, nil
}

// Wait returns the bounded public ACP PromptResponse result derived from the
// exact validated turn/completed status. Native terminal items never cross
// this boundary.
func (h *CodexTurnHandle) Wait(ctx context.Context) (json.RawMessage, error) {
	if h == nil || h.client == nil || h.state == nil {
		return nil, ErrStaleTurn
	}
	select {
	case <-h.state.done:
		return h.result()
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.client.peer.done:
		select {
		case <-h.state.done:
			return h.result()
		default:
			return nil, &BoundaryError{Stage: "app server transport", Err: ErrTransport}
		}
	case <-h.state.done:
		return h.result()
	}
}

func (h *CodexTurnHandle) result() (json.RawMessage, error) {
	h.client.mu.Lock()
	terminal := append(json.RawMessage(nil), h.state.terminal...)
	err := h.state.err
	h.client.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return terminal, nil
}

// Interrupt requests native interruption of this exact turn. Completion still
// comes only from the correlated turn/completed notification.
func (h *CodexTurnHandle) Interrupt(ctx context.Context) error {
	if h == nil || h.client == nil || h.state == nil || h.turnID == "" {
		return ErrStaleTurn
	}
	h.client.mu.Lock()
	stale := h.state.closed || h.client.active[h.state.threadID] != h.state || h.state.turnID != h.turnID
	h.client.mu.Unlock()
	if stale {
		return ErrStaleTurn
	}
	params, _ := json.Marshal(struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}{ThreadID: h.state.threadID, TurnID: h.turnID})
	if _, err := h.client.peer.call(ctx, appMethodTurnInterrupt, params); err != nil {
		return &BoundaryError{Stage: "turn interrupt", Err: ErrTransport}
	}
	return nil
}

func (c *CodexAppServerClient) handleNotification(ctx context.Context, message appServerMessage) error {
	if message.Method == appMethodRequestDone {
		return c.handleRequestResolved(message.Params)
	}
	var boundary struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if len(message.Params) == 0 || json.Unmarshal(message.Params, &boundary) != nil || boundary.ThreadID == "" {
		return nil
	}
	if !boundedCorrelation(boundary.ThreadID) {
		return ErrTransport
	}
	strictTurn := message.Method == appMethodTurnStarted || message.Method == appMethodTurnCompleted
	turnID := boundary.TurnID
	if strictTurn {
		if !boundedCorrelation(boundary.Turn.ID) || (boundary.TurnID != "" && boundary.TurnID != boundary.Turn.ID) {
			c.mu.Lock()
			state := c.active[boundary.ThreadID]
			c.mu.Unlock()
			if state != nil {
				c.removeTurn(state, &BoundaryError{Stage: "turn notification", Err: ErrMalformedCompletion})
			}
			return nil
		}
		turnID = boundary.Turn.ID
	} else if turnID == "" {
		turnID = boundary.Turn.ID
	}
	if !boundedCorrelation(turnID) {
		return nil
	}

	c.mu.Lock()
	state := c.active[boundary.ThreadID]
	if state == nil || state.closed {
		c.mu.Unlock()
		return nil
	}
	if message.Method == appMethodTurnStarted {
		if boundary.Turn.Status != "inProgress" || !boundedCorrelation(turnID) {
			c.mu.Unlock()
			c.removeTurn(state, &BoundaryError{Stage: "turn started", Err: ErrMalformedCompletion})
			return nil
		}
		if state.turnID != "" && state.turnID != turnID {
			c.mu.Unlock()
			c.removeTurn(state, &BoundaryError{Stage: "turn identity", Err: ErrOwnership})
			return nil
		}
		state.turnID = turnID
	}
	if state.turnID == "" || state.turnID != turnID {
		c.mu.Unlock()
		return nil
	}
	sink := state.sink
	c.mu.Unlock()

	if message.Method == appMethodTurnCompleted {
		completion, terminalErr := managerCompletion(boundary.Turn.Status)
		if terminalErr != nil {
			c.removeTurn(state, terminalErr)
			return nil
		}
		c.mu.Lock()
		if !state.closed {
			state.terminal = completion
			state.closed = true
			if c.active[state.threadID] == state {
				delete(c.active, state.threadID)
			}
			close(state.done)
		}
		c.mu.Unlock()
		return nil
	}

	frame, expose, err := appNotificationToACP(state.publicSession, message)
	if err != nil {
		c.removeTurn(state, &BoundaryError{Stage: "public ACP translation", Err: ErrTransport})
		return nil
	}
	if !expose {
		return nil
	}
	if err := sink(ctx, frame); err != nil {
		c.removeTurn(state, &BoundaryError{Stage: "turn notification", Err: ErrTransport})
		return err
	}
	return nil
}

func (c *CodexAppServerClient) handleRequestResolved(raw json.RawMessage) error {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		ThreadID  string          `json:"threadId"`
	}
	if json.Unmarshal(raw, &params) != nil || !boundedCorrelation(params.ThreadID) {
		return ErrTransport
	}
	c.mu.Lock()
	owned := c.ownedThread == params.ThreadID
	c.mu.Unlock()
	if !owned {
		return nil
	}
	if _, err := appCorrelationKey(params.RequestID); err != nil {
		return ErrTransport
	}
	c.peer.resolveInbound(params.RequestID, params.ThreadID)
	return nil
}

func (c *CodexAppServerClient) removeTurn(state *codexTurnState, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state == nil || state.closed {
		return
	}
	state.err = err
	state.closed = true
	if c.active[state.threadID] == state {
		delete(c.active, state.threadID)
	}
	close(state.done)
}

func (c *CodexAppServerClient) handleRequest(ctx context.Context, message appServerMessage) (json.RawMessage, *appServerError) {
	if message.Method != appMethodDynamicTool {
		return nil, &appServerError{Code: -32601, Message: "method not supported"}
	}
	var params struct {
		Arguments json.RawMessage `json:"arguments"`
		CallID    string          `json:"callId"`
		ThreadID  string          `json:"threadId"`
		Tool      string          `json:"tool"`
		TurnID    string          `json:"turnId"`
		Namespace json.RawMessage `json:"namespace"`
	}
	if json.Unmarshal(message.Params, &params) != nil || len(params.Arguments) == 0 || !json.Valid(params.Arguments) ||
		len(params.Arguments) > acp.MaxFrameBytes ||
		params.Tool != DelegateToolName || !boundedCorrelation(params.CallID) || !boundedCorrelation(params.ThreadID) ||
		!boundedCorrelation(params.TurnID) ||
		(len(params.Namespace) != 0 && !bytes.Equal(bytes.TrimSpace(params.Namespace), []byte("null"))) {
		return dynamicToolFailure(ErrInvalidOperation), nil
	}
	requestKey, err := appCorrelationKey(message.ID)
	if err != nil {
		return dynamicToolFailure(ErrInvalidOperation), nil
	}
	c.mu.Lock()
	owned := c.ownedThread == params.ThreadID
	active := c.active[params.ThreadID]
	validTurn := active != nil && !active.closed && active.turnID != "" && active.turnID == params.TurnID
	correlationErr := error(nil)
	if owned && validTurn {
		if _, duplicate := active.toolRequests[requestKey]; duplicate {
			correlationErr = ErrStaleTurn
		} else if _, duplicate := active.toolCallIDs[params.CallID]; duplicate {
			correlationErr = ErrStaleTurn
		} else if len(active.toolRequests) >= MaxManagerToolCalls || len(active.toolCallIDs) >= MaxManagerToolCalls {
			correlationErr = ErrCapacity
		} else {
			active.toolRequests[requestKey] = struct{}{}
			active.toolCallIDs[params.CallID] = struct{}{}
		}
	}
	c.mu.Unlock()
	if !owned || !validTurn {
		return dynamicToolFailure(ErrOwnership), nil
	}
	if correlationErr != nil {
		return dynamicToolFailure(correlationErr), nil
	}
	result, err := c.delegate.Invoke(ctx, append(json.RawMessage(nil), params.Arguments...))
	if err != nil {
		return dynamicToolFailure(err), nil
	}
	wire, err := marshalDynamicToolResponse(result, true)
	if err != nil {
		return dynamicToolFailure(ErrCapacity), nil
	}
	return wire, nil
}

func boundedCorrelation(value string) bool {
	return value != "" && len(value) <= MaxCorrelationIDBytes
}

func dynamicToolFailure(err error) json.RawMessage {
	code := "operation_failed"
	switch {
	case errors.Is(err, ErrInvalidOperation):
		code = "invalid_operation"
	case errors.Is(err, ErrOwnership):
		code = "ownership_rejected"
	case errors.Is(err, ErrStaleTurn):
		code = "stale_turn"
	case errors.Is(err, ErrChildBusy):
		code = "child_busy"
	case errors.Is(err, ErrReplayUnavailable):
		code = "replay_unavailable"
	case errors.Is(err, ErrProviderUnknown), errors.Is(err, ErrProviderUnsupported):
		code = "provider_unsupported"
	case errors.Is(err, ErrCapacity), errors.Is(err, ErrAnswerLimit):
		code = "capacity"
	case errors.Is(err, ErrTransport), errors.Is(err, ErrLaunchFailed), errors.Is(err, ErrProcessFailed):
		code = "transport_failure"
	}
	encoded, _ := json.Marshal("taskai_delegate: " + code)
	wire, _ := marshalDynamicToolResponse(encoded, false)
	return wire
}

func marshalDynamicToolResponse(result json.RawMessage, success bool) (json.RawMessage, error) {
	if len(result) == 0 || !json.Valid(result) {
		return nil, ErrCapacity
	}
	text := string(result)
	if !success {
		var decoded string
		if json.Unmarshal(result, &decoded) != nil {
			return nil, ErrCapacity
		}
		text = decoded
	}
	wire, err := json.Marshal(struct {
		Success      bool `json:"success"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentItems"`
	}{
		Success: success,
		ContentItems: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "inputText", Text: text}},
	})
	if err != nil || len(wire)+DynamicToolRPCHeadroom > acp.MaxFrameBytes {
		return nil, ErrCapacity
	}
	return wire, nil
}

func capabilityResultFitsDynamicTool(result json.RawMessage) bool {
	_, err := marshalDynamicToolResponse(result, true)
	return err == nil
}

func (c *CodexAppServerClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		active := make([]*codexTurnState, 0, len(c.active))
		for _, state := range c.active {
			active = append(active, state)
		}
		c.mu.Unlock()
		for _, state := range active {
			c.removeTurn(state, &BoundaryError{Stage: "app server close", Err: ErrTransport})
		}
		if c.peer != nil {
			_ = c.peer.close()
		}
		if c.process != nil {
			_ = c.process.Close()
		}
	})
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

type appMessageKind uint8

const (
	appInvalid appMessageKind = iota
	appRequest
	appNotification
	appResponse
)

type appServerError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type appServerMessage struct {
	Raw    json.RawMessage `json:"-"`
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *appServerError `json:"error,omitempty"`
}

func (m appServerMessage) kind() appMessageKind {
	switch {
	case m.Method != "" && len(m.ID) != 0:
		return appRequest
	case m.Method != "" && len(m.ID) == 0:
		return appNotification
	case m.Method == "" && len(m.ID) != 0:
		return appResponse
	default:
		return appInvalid
	}
}

type appCallResult struct {
	message appServerMessage
	err     error
}

type appPendingCall struct {
	result        chan appCallResult
	responseLimit int
}

type appInboundRequest struct {
	threadID string
	cancel   context.CancelFunc
	mu       sync.Mutex
	resolved bool
	complete bool
}

type appRequestHandler func(context.Context, appServerMessage) (json.RawMessage, *appServerError)
type appNotificationHandler func(context.Context, appServerMessage) error

// appServerPeer is intentionally private and separate from ACP. Codex App
// Server 0.150.1 uses headerless JSON-RPC-shaped JSONL (no jsonrpc field).
type appServerPeer struct {
	reader         *bufio.Reader
	writer         io.Writer
	request        appRequestHandler
	onNotification appNotificationHandler
	seq            atomic.Uint64

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]appPendingCall
	inbound map[string]*appInboundRequest
	started bool
	err     error
	done    chan struct{}
	once    sync.Once
}

func newAppServerPeer(r io.Reader, w io.Writer, request appRequestHandler, notify appNotificationHandler) *appServerPeer {
	return &appServerPeer{
		reader: bufio.NewReaderSize(r, 64<<10), writer: w,
		request: request, onNotification: notify,
		pending: make(map[string]appPendingCall), inbound: make(map[string]*appInboundRequest),
		done: make(chan struct{}),
	}
}

func (p *appServerPeer) start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.mu.Unlock()
	go p.readLoop(ctx)
	go func() {
		select {
		case <-ctx.Done():
			p.fail(ctx.Err())
		case <-p.done:
		}
	}()
}

func (p *appServerPeer) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if method == "" || (len(params) != 0 && !json.Valid(params)) {
		return nil, ErrTransport
	}
	id := json.RawMessage(strconv.FormatUint(p.seq.Add(1), 10))
	key, _ := appCorrelationKey(id)
	result := make(chan appCallResult, 1)
	responseLimit := MaxAppServerLiveFrameBytes
	if method == appMethodThreadRead {
		responseLimit = MaxAppServerHistoryBytes
	}
	p.mu.Lock()
	if !p.started || p.err != nil {
		p.mu.Unlock()
		return nil, ErrTransport
	}
	p.pending[key] = appPendingCall{result: result, responseLimit: responseLimit}
	p.mu.Unlock()

	wire, _ := json.Marshal(appServerMessage{ID: id, Method: method, Params: params})
	if err := p.writeLine(wire, MaxAppServerLiveFrameBytes); err != nil {
		p.removePending(key)
		p.fail(err)
		return nil, err
	}
	select {
	case reply := <-result:
		if reply.err != nil || reply.message.Error != nil {
			return nil, ErrTransport
		}
		return append(json.RawMessage(nil), reply.message.Result...), nil
	case <-ctx.Done():
		p.removePending(key)
		return nil, ctx.Err()
	case <-p.done:
		p.removePending(key)
		return nil, ErrTransport
	}
}

func (p *appServerPeer) notify(method string, params json.RawMessage) error {
	if method == "" || (len(params) != 0 && !json.Valid(params)) {
		return ErrTransport
	}
	wire, _ := json.Marshal(appServerMessage{Method: method, Params: params})
	return p.writeLine(wire, MaxAppServerLiveFrameBytes)
}

func (p *appServerPeer) readLoop(ctx context.Context) {
	for {
		raw, err := readAppServerLineWithin(p.reader, p.currentReadLimit)
		if err != nil {
			p.fail(err)
			return
		}
		message, err := parseAppServerMessageWithin(raw, MaxAppServerHistoryBytes)
		if err != nil {
			p.fail(err)
			return
		}
		if len(raw) > MaxAppServerLiveFrameBytes &&
			(message.kind() != appResponse || !p.responseWithinLimit(message.ID, len(raw))) {
			p.fail(ErrTransport)
			return
		}
		switch message.kind() {
		case appResponse:
			if err := p.deliver(message); err != nil {
				p.fail(err)
				return
			}
		case appNotification:
			if p.onNotification != nil {
				if err := p.onNotification(ctx, message); err != nil {
					p.fail(ErrTransport)
					return
				}
			}
		case appRequest:
			if err := p.startRequest(ctx, message); err != nil {
				p.fail(err)
				return
			}
		default:
			p.fail(ErrTransport)
			return
		}
	}
}

func (p *appServerPeer) startRequest(parent context.Context, message appServerMessage) error {
	key, err := appCorrelationKey(message.ID)
	if err != nil {
		return ErrTransport
	}
	var boundary struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(message.Params, &boundary)
	if !boundedCorrelation(boundary.ThreadID) {
		boundary.ThreadID = ""
	}
	requestCtx, cancel := context.WithCancel(parent)
	entry := &appInboundRequest{threadID: boundary.ThreadID, cancel: cancel}
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		cancel()
		return ErrTransport
	}
	if _, duplicate := p.inbound[key]; duplicate {
		p.mu.Unlock()
		cancel()
		return ErrTransport
	}
	if len(p.inbound) >= MaxAppServerInbound {
		p.mu.Unlock()
		cancel()
		return ErrCapacity
	}
	p.inbound[key] = entry
	p.mu.Unlock()

	go func() {
		defer cancel()
		var result json.RawMessage
		var rpcErr *appServerError
		if p.request == nil {
			rpcErr = &appServerError{Code: -32601, Message: "method not supported"}
		} else {
			result, rpcErr = p.request(requestCtx, message)
		}
		response := appServerMessage{ID: append(json.RawMessage(nil), message.ID...)}
		if rpcErr != nil {
			response.Error = rpcErr
		} else {
			if len(result) == 0 {
				result = json.RawMessage("null")
			}
			response.Result = result
		}
		wire, err := json.Marshal(response)
		if err != nil {
			p.removeInboundEntry(key, entry)
			p.fail(ErrTransport)
			return
		}
		if !p.beginInboundResponse(key, entry) {
			p.removeInboundEntry(key, entry)
			return
		}
		writeErr := p.writeLine(wire, MaxAppServerLiveFrameBytes)
		p.finishInboundResponse(key, entry)
		if writeErr != nil {
			p.fail(writeErr)
		}
	}()
	return nil
}

func (p *appServerPeer) beginInboundResponse(key string, entry *appInboundRequest) bool {
	p.mu.Lock()
	current, ok := p.inbound[key]
	live := ok && current == entry && p.err == nil
	p.mu.Unlock()
	if !live {
		return false
	}
	entry.mu.Lock()
	if entry.resolved || entry.complete {
		entry.mu.Unlock()
		return false
	}
	// Claim response publication while retaining the ID in the live map. A
	// later resolution cannot cancel a handler that has already completed, and
	// a duplicate ID remains rejected until the write finishes.
	entry.complete = true
	entry.mu.Unlock()
	return true
}

func (p *appServerPeer) finishInboundResponse(key string, entry *appInboundRequest) {
	p.removeInboundEntry(key, entry)
}

func (p *appServerPeer) removeInboundEntry(key string, entry *appInboundRequest) {
	p.mu.Lock()
	if p.inbound[key] == entry {
		delete(p.inbound, key)
	}
	p.mu.Unlock()
}

// resolveInbound cancels one exact live server request. A foreign thread,
// unknown request ID, or already-completed request has no effect.
func (p *appServerPeer) resolveInbound(id json.RawMessage, threadID string) bool {
	key, err := appCorrelationKey(id)
	if err != nil || threadID == "" {
		return false
	}
	p.mu.Lock()
	entry := p.inbound[key]
	p.mu.Unlock()
	if entry == nil || entry.threadID != threadID {
		return false
	}
	entry.mu.Lock()
	if entry.resolved || entry.complete {
		entry.mu.Unlock()
		return false
	}
	entry.resolved = true
	cancel := entry.cancel
	entry.mu.Unlock()
	cancel()
	return true
}

func (p *appServerPeer) deliver(message appServerMessage) error {
	key, err := appCorrelationKey(message.ID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	pending, ok := p.pending[key]
	if ok {
		delete(p.pending, key)
	}
	p.mu.Unlock()
	if !ok {
		return ErrTransport
	}
	pending.result <- appCallResult{message: message}
	return nil
}

func (p *appServerPeer) currentReadLimit() int {
	if p == nil {
		return MaxAppServerLiveFrameBytes
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	limit := MaxAppServerLiveFrameBytes
	for _, pending := range p.pending {
		if pending.responseLimit > limit {
			limit = pending.responseLimit
		}
	}
	return limit
}

func (p *appServerPeer) responseWithinLimit(id json.RawMessage, size int) bool {
	key, err := appCorrelationKey(id)
	if err != nil {
		return false
	}
	p.mu.Lock()
	pending, ok := p.pending[key]
	p.mu.Unlock()
	return ok && size <= pending.responseLimit
}

func (p *appServerPeer) removePending(key string) {
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
}

func (p *appServerPeer) writeLine(raw []byte, limit int) error {
	if len(raw) == 0 || len(raw) > limit || bytes.IndexByte(raw, '\n') >= 0 || !json.Valid(raw) {
		return ErrTransport
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	framed := append(append([]byte(nil), raw...), '\n')
	for len(framed) != 0 {
		written, err := p.writer.Write(framed)
		if written < 0 || written > len(framed) {
			return ErrTransport
		}
		framed = framed[written:]
		if err != nil || written == 0 {
			return ErrTransport
		}
	}
	return nil
}

func (p *appServerPeer) fail(err error) {
	if err == nil {
		err = ErrTransport
	}
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		pending := p.pending
		p.pending = make(map[string]appPendingCall)
		inbound := p.inbound
		p.inbound = make(map[string]*appInboundRequest)
		p.mu.Unlock()
		for _, request := range inbound {
			request.cancel()
		}
		for _, pending := range pending {
			pending.result <- appCallResult{err: err}
		}
		close(p.done)
	})
}

func (p *appServerPeer) errValue() error {
	if p == nil {
		return ErrTransport
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *appServerPeer) close() error {
	if p == nil {
		return nil
	}
	p.fail(ErrTransport)
	if closer, ok := p.writer.(io.Closer); ok {
		_ = closer.Close()
	}
	return nil
}

func readAppServerLine(reader *bufio.Reader) ([]byte, error) {
	return readAppServerLineWithin(reader, func() int { return MaxAppServerLiveFrameBytes })
}

func readAppServerLineWithin(reader *bufio.Reader, limit func() int) ([]byte, error) {
	if reader == nil || limit == nil {
		return nil, ErrTransport
	}
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		currentLimit := limit()
		if currentLimit < MaxAppServerLiveFrameBytes || currentLimit > MaxAppServerHistoryBytes ||
			len(line)+len(fragment) > currentLimit+1 {
			return nil, ErrTransport
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, ErrTransport
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil, ErrTransport
		}
		break
	}
	if line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) != 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	currentLimit := limit()
	if currentLimit < MaxAppServerLiveFrameBytes || currentLimit > MaxAppServerHistoryBytes ||
		len(line) == 0 || len(line) > currentLimit {
		return nil, ErrTransport
	}
	return line, nil
}

func parseAppServerMessage(raw []byte) (appServerMessage, error) {
	return parseAppServerMessageWithin(raw, MaxAppServerLiveFrameBytes)
}

func parseAppServerMessageWithin(raw []byte, limit int) (appServerMessage, error) {
	if limit < MaxAppServerLiveFrameBytes || limit > MaxAppServerHistoryBytes ||
		len(raw) == 0 || len(raw) > limit || !validJSONObject(raw) {
		return appServerMessage{}, ErrTransport
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return appServerMessage{}, ErrTransport
	}
	if _, headerful := fields["jsonrpc"]; headerful {
		return appServerMessage{}, ErrTransport
	}
	var message appServerMessage
	if json.Unmarshal(raw, &message) != nil {
		return appServerMessage{}, ErrTransport
	}
	message.Raw = append(json.RawMessage(nil), raw...)
	if len(message.ID) != 0 {
		if _, err := appCorrelationKey(message.ID); err != nil {
			return appServerMessage{}, ErrTransport
		}
	}
	if len(message.Params) != 0 && !json.Valid(message.Params) {
		return appServerMessage{}, ErrTransport
	}
	if len(message.Result) != 0 && !json.Valid(message.Result) {
		return appServerMessage{}, ErrTransport
	}
	if message.Error != nil && len(message.Error.Data) != 0 && !json.Valid(message.Error.Data) {
		return appServerMessage{}, ErrTransport
	}
	switch message.kind() {
	case appRequest, appNotification:
		if len(message.Result) != 0 || message.Error != nil {
			return appServerMessage{}, ErrTransport
		}
	case appResponse:
		if (len(message.Result) == 0) == (message.Error == nil) {
			return appServerMessage{}, ErrTransport
		}
	default:
		return appServerMessage{}, ErrTransport
	}
	return message, nil
}

func appCorrelationKey(id json.RawMessage) (string, error) {
	if len(id) == 0 || len(id) > MaxCorrelationIDBytes {
		return "", ErrTransport
	}
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", ErrTransport
	}
	switch typed := value.(type) {
	case string:
		if typed == "" || len(typed) > MaxCorrelationIDBytes {
			return "", ErrTransport
		}
		return "s:" + typed, nil
	case json.Number:
		if _, err := typed.Int64(); err != nil {
			return "", ErrTransport
		}
		return "n:" + typed.String(), nil
	default:
		return "", ErrTransport
	}
}

var _ CodexAppServer = (*CodexAppServerClient)(nil)
var _ replay.CodexThreadReader = (*CodexAppServerClient)(nil)
