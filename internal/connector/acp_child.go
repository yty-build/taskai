package connector

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

const acpHandshakeTimeout = 10 * time.Second

// ProviderReferenceCodec is the connector-only bridge between a provider's
// public native Session ID and the one opaque envelope shared by persistence,
// exact Resume, and Replay. TaskAI core never implements or calls it.
type ProviderReferenceCodec interface {
	New(string) (replay.OpaqueRef, error)
	Resolve(replay.OpaqueRef) (string, error)
}

type conformedReferenceCodec interface {
	ProviderReferenceCodec
	Provider() replay.ProviderID
	Ready() bool
}

// CodexReferenceCodec uses the exact version-conformed Replay envelope.
type CodexReferenceCodec struct{}

func (CodexReferenceCodec) Provider() replay.ProviderID { return replay.ProviderCodex }
func (CodexReferenceCodec) Ready() bool                 { return true }

func (CodexReferenceCodec) New(sessionID string) (replay.OpaqueRef, error) {
	return replay.NewCodexReference(sessionID)
}

func (CodexReferenceCodec) Resolve(reference replay.OpaqueRef) (string, error) {
	return replay.ResolveCodexReference(reference)
}

// ClaudeReferenceCodec binds ACP Session IDs to the exact project scope used
// by Claude Resume and native Replay. ProjectKey is mechanically supplied by
// the version-conformed provider adapter; this codec never enumerates native
// Session storage.
type ClaudeReferenceCodec struct {
	ProjectKey string
}

func (ClaudeReferenceCodec) Provider() replay.ProviderID { return replay.ProviderClaude }

func (c ClaudeReferenceCodec) Ready() bool {
	_, err := replay.NewClaudeReference(c.ProjectKey, "taskai-reference-probe")
	return err == nil
}

func (c ClaudeReferenceCodec) New(sessionID string) (replay.OpaqueRef, error) {
	return replay.NewClaudeReference(c.ProjectKey, sessionID)
}

func (c ClaudeReferenceCodec) Resolve(reference replay.OpaqueRef) (string, error) {
	projectKey, sessionID, err := replay.ResolveClaudeReference(reference)
	if err != nil || projectKey != c.ProjectKey {
		return "", replay.ErrInvalidReference
	}
	return sessionID, nil
}

// GeminiRecordKeyFunc returns the exact native history-record basename
// for one public ACP Session ID. It must be a version-conformed mechanical
// mapping supplied by composition; it may not enumerate or content-search
// provider history. Gemini 0.25.0-preview.0 ACP itself has no Resume method.
type GeminiRecordKeyFunc func(sessionID string) (string, error)

type GeminiReferenceCodec struct {
	ProjectHash string
	RecordKey   GeminiRecordKeyFunc
}

func (GeminiReferenceCodec) Provider() replay.ProviderID { return replay.ProviderGemini }

func (c GeminiReferenceCodec) Ready() bool {
	if c.RecordKey == nil {
		return false
	}
	// Readiness must not ask the composition-owned mapping about a made-up
	// provider Session. Validate only the fixed project scope here; New and
	// Resolve validate the exact record key for a real Session.
	_, err := replay.NewGeminiReference(c.ProjectHash, "taskai-reference-probe.json", "taskai-reference-probe")
	return err == nil
}

func (c GeminiReferenceCodec) New(sessionID string) (replay.OpaqueRef, error) {
	if !c.Ready() {
		return replay.OpaqueRef{}, replay.ErrInvalidReference
	}
	recordKey, err := c.RecordKey(sessionID)
	if err != nil {
		return replay.OpaqueRef{}, replay.ErrInvalidReference
	}
	return replay.NewGeminiReference(c.ProjectHash, recordKey, sessionID)
}

func (c GeminiReferenceCodec) Resolve(reference replay.OpaqueRef) (string, error) {
	projectHash, recordKey, sessionID, err := replay.ResolveGeminiReference(reference)
	if err != nil || projectHash != c.ProjectHash || c.RecordKey == nil {
		return "", replay.ErrInvalidReference
	}
	expected, mapErr := c.RecordKey(sessionID)
	if mapErr != nil || expected != recordKey {
		return "", replay.ErrInvalidReference
	}
	return sessionID, nil
}

// GrokReferenceCodec binds an ACP Session ID to its exact native workspace
// scope. WorkspaceKey is provider metadata, not a filesystem path interpreted
// by TaskAI core.
type GrokReferenceCodec struct {
	WorkspaceKey string
}

func (GrokReferenceCodec) Provider() replay.ProviderID { return replay.ProviderGrok }

func (c GrokReferenceCodec) Ready() bool {
	_, err := replay.NewGrokReference(c.WorkspaceKey, "taskai-reference-probe")
	return err == nil
}

func (c GrokReferenceCodec) New(sessionID string) (replay.OpaqueRef, error) {
	return replay.NewGrokReference(c.WorkspaceKey, sessionID)
}

func (c GrokReferenceCodec) Resolve(reference replay.OpaqueRef) (string, error) {
	workspaceKey, sessionID, err := replay.ResolveGrokReference(reference)
	if err != nil || workspaceKey != c.WorkspaceKey {
		return "", replay.ErrInvalidReference
	}
	return sessionID, nil
}

// SubordinateFactoryConformance is executable boundary evidence, not a
// capability claim. Registry profiles intersect it with independently probed
// exact versions and available Resume/Replay ports.
type SubordinateFactoryConformance struct {
	Provider replay.ProviderID
	Versions Versions
	Resume   bool
}

type ConformedSubordinateFactory interface {
	ChildFactory
	SubordinateConformance() (SubordinateFactoryConformance, bool)
}

type acpFactoryBoundary struct {
	conformance SubordinateFactoryConformance
	executable  string
	cwd         string
	args        []string
}

// ACPFactory launches one literal connector process per provider-owned child
// Session. Empty additional roots and an empty mcpServers array are explicit:
// this package never introduces an MCP server.
type ACPFactory struct {
	Launcher       Launcher
	Command        CommandSpec
	CWD            string
	RequestHandler acp.Handler
	References     ProviderReferenceCodec

	boundary *acpFactoryBoundary
}

func NewCodexSubordinateFactory(executable, cwd string, requestHandler acp.Handler) ACPFactory {
	return newProviderACPFactory(
		executable, nil, cwd, requestHandler, CodexReferenceCodec{},
		replay.ProviderCodex, CodexConformance().Exact, true,
	)
}

func NewClaudeSubordinateFactory(executable, cwd, projectKey string, requestHandler acp.Handler) ACPFactory {
	return newProviderACPFactory(
		executable, nil, cwd, requestHandler, ClaudeReferenceCodec{ProjectKey: projectKey},
		replay.ProviderClaude, ClaudeConformance().Exact, true,
	)
}

func NewGeminiSubordinateFactory(
	executable, cwd, projectHash string,
	recordKey GeminiRecordKeyFunc,
	requestHandler acp.Handler,
) ACPFactory {
	return newProviderACPFactory(
		executable, []string{"--experimental-acp"}, cwd, requestHandler,
		GeminiReferenceCodec{ProjectHash: projectHash, RecordKey: recordKey},
		replay.ProviderGemini, GeminiConformance().Exact, false,
	)
}

func NewGrokSubordinateFactory(executable, cwd, workspaceKey string, requestHandler acp.Handler) ACPFactory {
	return newProviderACPFactory(
		executable, []string{"agent", "stdio"}, cwd, requestHandler,
		GrokReferenceCodec{WorkspaceKey: workspaceKey},
		replay.ProviderGrok, GrokConformance().Exact, true,
	)
}

func newProviderACPFactory(
	executable string,
	args []string,
	cwd string,
	requestHandler acp.Handler,
	references ProviderReferenceCodec,
	provider replay.ProviderID,
	versions Versions,
	resume bool,
) ACPFactory {
	commandArgs := append([]string(nil), args...)
	return ACPFactory{
		Command: CommandSpec{Executable: executable, Args: commandArgs},
		CWD:     cwd, RequestHandler: requestHandler, References: references,
		boundary: &acpFactoryBoundary{
			conformance: SubordinateFactoryConformance{Provider: provider, Versions: versions, Resume: resume},
			executable:  executable,
			cwd:         cwd,
			args:        append([]string(nil), commandArgs...),
		},
	}
}

func (f ACPFactory) SubordinateConformance() (SubordinateFactoryConformance, bool) {
	if f.boundary == nil || f.Command.validate() != nil || f.Command.Dir != "" ||
		f.Command.Executable != f.boundary.executable || f.CWD != f.boundary.cwd ||
		f.CWD == "" || !filepath.IsAbs(f.CWD) || len(f.Command.Args) != len(f.boundary.args) {
		return SubordinateFactoryConformance{}, false
	}
	for index := range f.Command.Args {
		if f.Command.Args[index] != f.boundary.args[index] {
			return SubordinateFactoryConformance{}, false
		}
	}
	codec, ok := f.References.(conformedReferenceCodec)
	if !ok || !codec.Ready() || codec.Provider() != f.boundary.conformance.Provider {
		return SubordinateFactoryConformance{}, false
	}
	return f.boundary.conformance, true
}

func (f ACPFactory) New(ctx context.Context) (ChildPort, error) {
	return f.open(ctx, "", replay.OpaqueRef{}, false)
}

// Resume launches a fresh public connector transport and invokes exactly
// session/resume for the supplied native Session ID. It never falls back to
// session/new.
func (f ACPFactory) Resume(ctx context.Context, nativeReference replay.OpaqueRef) (ChildPort, error) {
	if f.boundary != nil && !f.boundary.conformance.Resume {
		return nil, ErrProviderUnsupported
	}
	if f.References == nil {
		return nil, ErrProviderUnsupported
	}
	sessionID, err := f.References.Resolve(nativeReference)
	if err != nil || sessionID == "" {
		return nil, &BoundaryError{Stage: "ACP reference", Err: ErrProviderUnsupported}
	}
	return f.open(ctx, sessionID, nativeReference, true)
}

func (f ACPFactory) open(ctx context.Context, resumeSessionID string, reference replay.OpaqueRef, resuming bool) (ChildPort, error) {
	if ctx == nil || ctx.Err() != nil || f.CWD == "" || !filepath.IsAbs(f.CWD) || f.References == nil {
		return nil, ErrProviderUnsupported
	}
	process, err := f.Launcher.Start(ctx, f.Command)
	if err != nil {
		return nil, err
	}
	session := &ACPSession{
		process: process, requestHandler: f.RequestHandler,
		closeDone: make(chan struct{}),
	}
	handler := acp.HandlerFuncs{
		Request:      session.handleRequest,
		Notification: session.handleNotification,
	}
	session.client = acp.NewClient(process.Stdout, process.Stdin, handler)
	if err := session.client.Start(ctx); err != nil {
		_ = session.Close()
		return nil, &BoundaryError{Stage: "ACP start", Err: ErrTransport}
	}
	handshakeCtx, cancelHandshake := context.WithTimeout(ctx, acpHandshakeTimeout)
	defer cancelHandshake()

	initialize := json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"taskai","version":"1"}}`)
	initialized, err := session.client.Call(handshakeCtx, acp.MethodInitialize, initialize)
	if err != nil {
		_ = session.Close()
		return nil, &BoundaryError{Stage: "ACP initialize", Err: ErrTransport}
	}
	var handshake struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(initialized, &handshake) != nil || handshake.ProtocolVersion != acp.ProtocolVersion {
		_ = session.Close()
		return nil, ErrProviderUnsupported
	}

	if !resuming {
		params, _ := json.Marshal(struct {
			CWD        string `json:"cwd"`
			MCPServers []any  `json:"mcpServers"`
		}{CWD: f.CWD, MCPServers: []any{}})
		created, callErr := session.client.Call(handshakeCtx, acp.MethodSessionNew, params)
		if callErr != nil {
			_ = session.Close()
			return nil, &BoundaryError{Stage: "ACP session new", Err: ErrTransport}
		}
		var response struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(created, &response) != nil || response.SessionID == "" {
			_ = session.Close()
			return nil, &BoundaryError{Stage: "ACP session new", Err: ErrMalformedCompletion}
		}
		reference, referenceErr := f.References.New(response.SessionID)
		if referenceErr != nil || len(reference.Bytes()) == 0 || len(reference.Bytes()) > MaxNativeReferenceBytes {
			_ = session.Close()
			return nil, &BoundaryError{Stage: "ACP reference", Err: ErrProviderUnsupported}
		}
		session.sessionID = response.SessionID
		session.reference = reference
		session.watchTransport()
		return session, nil
	}

	params, _ := json.Marshal(struct {
		SessionID  string `json:"sessionId"`
		CWD        string `json:"cwd"`
		MCPServers []any  `json:"mcpServers"`
	}{SessionID: resumeSessionID, CWD: f.CWD, MCPServers: []any{}})
	if _, err := session.client.Call(handshakeCtx, acp.MethodSessionResume, params); err != nil {
		_ = session.Close()
		return nil, &BoundaryError{Stage: "ACP session resume", Err: ErrTransport}
	}
	if len(reference.Bytes()) == 0 || len(reference.Bytes()) > MaxNativeReferenceBytes {
		_ = session.Close()
		return nil, &BoundaryError{Stage: "ACP reference", Err: ErrProviderUnsupported}
	}
	session.sessionID = resumeSessionID
	session.reference = replay.OpaqueRefFromBytes(reference.Bytes())
	session.watchTransport()
	return session, nil
}

// ACPSession is a raw public ACP child connection. Exactly one turn may be
// active on a child; different ACPSession values may run concurrently.
type ACPSession struct {
	process        *Process
	client         *acp.Client
	requestHandler acp.Handler
	sessionID      string
	reference      replay.OpaqueRef

	mu     sync.Mutex
	sink   func(json.RawMessage) error
	closed bool

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func (s *ACPSession) NativeReference() replay.OpaqueRef {
	return replay.OpaqueRefFromBytes(s.reference.Bytes())
}

// Available reports whether this exact public ACP transport is still usable.
// The Delegator uses it only to decide between reusing this transport and
// exact-resuming its already-owned native Session reference.
func (s *ACPSession) Available() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	available := !s.closed && s.client != nil && s.process != nil && s.sessionID != ""
	s.mu.Unlock()
	if !available {
		return false
	}
	select {
	case <-s.client.Done():
		return false
	default:
		return true
	}
}

// Done closes only after the connector transport has closed and its owned
// provider process has been reaped.
func (s *ACPSession) Done() <-chan struct{} {
	if s == nil || s.closeDone == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.closeDone
}

func (s *ACPSession) watchTransport() {
	go func() {
		<-s.client.Done()
		_ = s.Close()
	}()
}

func (s *ACPSession) Prompt(ctx context.Context, prompt string, sink func(json.RawMessage) error) (json.RawMessage, error) {
	if sink == nil {
		return nil, ErrInvalidOperation
	}
	s.mu.Lock()
	if s.closed || s.sink != nil || s.sessionID == "" {
		s.mu.Unlock()
		return nil, ErrChildBusy
	}
	s.sink = sink
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sink = nil
		s.mu.Unlock()
	}()

	params, _ := json.Marshal(struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}{
		SessionID: s.sessionID,
		Prompt: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: prompt}},
	})
	result, err := s.client.Call(ctx, acp.MethodSessionPrompt, params)
	if err != nil {
		_ = s.Close()
		return nil, ErrTransport
	}
	return result, nil
}

func (s *ACPSession) Cancel(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.mu.Lock()
	closed := s.closed
	sessionID := s.sessionID
	s.mu.Unlock()
	if closed || sessionID == "" {
		return ErrStaleTurn
	}
	params, _ := json.Marshal(struct {
		SessionID string `json:"sessionId"`
	}{SessionID: sessionID})
	if err := s.client.Notify(acp.MethodSessionCancel, params); err != nil {
		_ = s.Close()
		return ErrTransport
	}
	return nil
}

func (s *ACPSession) handleNotification(ctx context.Context, message acp.Message) error {
	if message.Method != acp.MethodSessionUpdate {
		return nil
	}
	var params struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(message.Params, &params) != nil || params.SessionID == "" {
		return ErrMalformedCompletion
	}
	s.mu.Lock()
	sink := s.sink
	owned := params.SessionID == s.sessionID
	s.mu.Unlock()
	if !owned {
		return ErrOwnership
	}
	if sink == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return sink(append(json.RawMessage(nil), message.Raw...))
	}
}

func (s *ACPSession) handleRequest(ctx context.Context, message acp.Message) (json.RawMessage, *acp.ErrorObject) {
	if s.requestHandler == nil {
		return nil, &acp.ErrorObject{Code: -32601, Message: "client method not supported"}
	}
	return s.requestHandler.HandleRequest(ctx, message)
}

func (s *ACPSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.client != nil {
			_ = s.client.Close()
		}
		if s.process != nil {
			s.closeErr = s.process.Close()
		}
		if s.closeDone != nil {
			close(s.closeDone)
		}
	})
	if s.closeDone != nil {
		<-s.closeDone
	}
	return s.closeErr
}
