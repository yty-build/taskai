package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yty-build/acp-gateway/internal/replay"
)

const ManagerPrefix = `You are a TaskAI Manager Session. Delegate separable work only through the single taskai_delegate capability. Every downstream interaction uses ACP. Start each subordinate invocation in Answer unless live detail is genuinely needed. Follow applies to one invocation and resets at every terminal condition. Request Replay only after completion and only when the target profile advertises it through this same capability. When a capability result says more=true, invoke its exact next_operation with its exact next_arguments until more=false. Synthesize subordinate results for the user; never ask TaskAI core to interpret prompts, frames, tools, or answers.`

const DelegateToolName = "taskai_delegate"
const CodexAppServerVersion = "0.150.1"

type DynamicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ThreadStartParams struct {
	CWD                   string        `json:"cwd"`
	DeveloperInstructions string        `json:"developerInstructions"`
	DynamicTools          []DynamicTool `json:"dynamicTools"`
}

// ThreadResumeParams is the pinned App Server 0.150.1 request boundary. Exact
// Resume sends only the previously owned native reference: developer
// instructions, dynamic tools, project instructions, prompts, and user context
// are neither accepted here nor reinjected.
type ThreadResumeParams struct {
	ThreadID string `json:"threadId"`
}

// CodexThreadSession is the narrow public App Server response boundary. Its
// thread ID exists only inside the connector and is immediately wrapped by the
// canonical Replay reference codec before crossing into TaskAI metadata.
type CodexThreadSession struct {
	ThreadID string
}

type NativeManagerSession struct {
	NativeReference replay.OpaqueRef
	Connection      ManagerConnection
}

// ManagerConnection is the provider-agnostic live Manager boundary consumed
// by the runtime packet. Prompt frames are standard bounded ACP JSON-RPC
// frames; provider-private transport envelopes never cross this interface.
// Provider-native history is intentionally absent; only the dedicated Replay
// adapter may read it.
type ManagerConnection interface {
	NativeReference() replay.OpaqueRef
	PublicSessionID() string
	Prompt(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error)
	Interrupt(context.Context) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

type CodexAppServer interface {
	Version() string
	DelegateHandlerRegistered() bool
	ThreadStart(context.Context, ThreadStartParams) (CodexThreadSession, error)
	ThreadResume(context.Context, ThreadResumeParams) (CodexThreadSession, error)
	ConnectManager(replay.OpaqueRef) (ManagerConnection, error)
	Close() error
}

// ManagerStartRequest supplies the per-Manager runtime ownership boundary.
// Its context is the supervisor lifetime context, not a finite request
// timeout. Events are content-free and IDs are transient RAM correlations.
type ManagerStartRequest struct {
	ParentSessionID string
	CWD             string
	Registry        *Registry
	Events          ChildEventSink
	IDs             IDSource
}

// OwnedChild is one direct subordinate ownership record restored from TaskAI
// metadata before exact Manager Resume. NativeReference remains an opaque
// defensive byte envelope; restoration performs no provider or history I/O.
// Runtime separately validates same-Task and direct-parent roles.
type OwnedChild struct {
	SessionID       string
	ProviderID      string
	NativeReference replay.OpaqueRef
}

type ManagerResumeRequest struct {
	ParentSessionID string
	NativeReference replay.OpaqueRef
	OwnedChildren   []OwnedChild
	Registry        *Registry
	Events          ChildEventSink
	IDs             IDSource
}

// ManagerFactory and ManagerResumeFactory are deliberately separate. A
// Start-only implementation cannot satisfy or advertise exact Resume.
type ManagerFactory interface {
	Start(context.Context, ManagerStartRequest) (NativeManagerSession, error)
}

type ManagerResumeFactory interface {
	Resume(context.Context, ManagerResumeRequest) (NativeManagerSession, error)
}

// CodexAppServerFactory creates one fresh live App Server peer with the exact
// Delegator owned by one Manager Session. Implementations must never reuse a
// peer across factory calls.
type CodexAppServerFactory interface {
	New(context.Context, DynamicToolHandler) (CodexAppServer, error)
}

type CodexAppServerFactoryFunc func(context.Context, DynamicToolHandler) (CodexAppServer, error)

func (f CodexAppServerFactoryFunc) New(ctx context.Context, delegate DynamicToolHandler) (CodexAppServer, error) {
	return f(ctx, delegate)
}

// CodexAppServerLauncher is the production literal-command factory. Delegate
// is supplied per Manager and therefore is intentionally absent from its
// reusable configuration.
type CodexAppServerLauncher struct {
	Command          CommandSpec
	InstalledVersion string
}

func (l CodexAppServerLauncher) New(ctx context.Context, delegate DynamicToolHandler) (CodexAppServer, error) {
	return LaunchCodexAppServer(ctx, CodexAppServerConfig{
		Command: l.Command, InstalledVersion: l.InstalledVersion, Delegate: delegate,
	})
}

// CodexManager is a per-Manager factory. Every Start and exact Resume creates
// an isolated App Server plus Delegator composite under the supplied
// supervisor lifetime context.
type CodexManager struct {
	Apps CodexAppServerFactory
}

var _ ManagerFactory = CodexManager{}
var _ ManagerResumeFactory = CodexManager{}
var _ CodexAppServerFactory = CodexAppServerLauncher{}

func (m CodexManager) Start(ctx context.Context, request ManagerStartRequest) (NativeManagerSession, error) {
	if request.CWD == "" || !filepath.IsAbs(request.CWD) {
		return NativeManagerSession{}, ErrProviderUnsupported
	}
	delegate, app, err := m.newRuntime(ctx, request.ParentSessionID, request.Registry, request.Events, request.IDs, nil)
	if err != nil {
		return NativeManagerSession{}, err
	}
	base, err := startCodexManagerSession(ctx, app, request.CWD)
	if err != nil {
		_ = app.Close()
		_ = delegate.Close()
		return NativeManagerSession{}, err
	}
	base.Connection = newDelegatedManagerConnection(ctx, base.Connection, delegate, app)
	return base, nil
}

func (m CodexManager) Resume(ctx context.Context, request ManagerResumeRequest) (NativeManagerSession, error) {
	children, err := validateOwnedChildren(request.OwnedChildren)
	if err != nil {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager child restore", Err: err}
	}
	if _, err := replay.ResolveCodexReference(request.NativeReference); err != nil {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager reference", Err: ErrProviderUnsupported}
	}
	delegate, app, err := m.newRuntime(
		ctx, request.ParentSessionID, request.Registry, request.Events, request.IDs, children,
	)
	if err != nil {
		return NativeManagerSession{}, err
	}
	base, err := resumeCodexManagerSession(ctx, app, request.NativeReference)
	if err != nil {
		_ = app.Close()
		_ = delegate.Close()
		return NativeManagerSession{}, err
	}
	base.Connection = newDelegatedManagerConnection(ctx, base.Connection, delegate, app)
	return base, nil
}

func (m CodexManager) newRuntime(
	ctx context.Context,
	parentSessionID string,
	registry *Registry,
	events ChildEventSink,
	ids IDSource,
	ownedChildren []OwnedChild,
) (*Delegator, CodexAppServer, error) {
	if m.Apps == nil {
		return nil, nil, ErrProviderUnsupported
	}
	delegate, err := NewDelegator(ctx, parentSessionID, registry, events, ids)
	if err != nil {
		return nil, nil, err
	}
	// Restore the complete direct-child set before constructing or contacting
	// the provider peer. A partial restore is never observable: any failure
	// closes this transient Delegator and sends zero thread/start or
	// thread/resume requests.
	for _, child := range ownedChildren {
		if err := delegate.RestoreOwnedChild(child.SessionID, child.ProviderID, child.NativeReference); err != nil {
			_ = delegate.Close()
			return nil, nil, &BoundaryError{Stage: "manager child restore", Err: err}
		}
	}
	app, err := m.Apps.New(ctx, delegate)
	if err != nil || !codexManagerBoundaryReady(app) {
		if app != nil {
			_ = app.Close()
		}
		_ = delegate.Close()
		return nil, nil, ErrProviderUnsupported
	}
	return delegate, app, nil
}

func validateOwnedChildren(children []OwnedChild) ([]OwnedChild, error) {
	if len(children) > MaxChildrenPerManager {
		return nil, ErrCapacity
	}
	result := make([]OwnedChild, len(children))
	sessionIDs := make(map[string]struct{}, len(children))
	references := make(map[string]struct{}, len(children))
	for index, child := range children {
		reference := child.NativeReference.Bytes()
		if child.SessionID == "" || len(child.SessionID) > MaxCorrelationIDBytes || strings.IndexByte(child.SessionID, 0) >= 0 ||
			child.ProviderID == "" || len(child.ProviderID) > 128 || strings.IndexByte(child.ProviderID, 0) >= 0 ||
			len(reference) == 0 || len(reference) > MaxNativeReferenceBytes {
			return nil, ErrInvalidOperation
		}
		if _, duplicate := sessionIDs[child.SessionID]; duplicate {
			return nil, ErrOwnership
		}
		if _, duplicate := references[string(reference)]; duplicate {
			return nil, ErrOwnership
		}
		sessionIDs[child.SessionID] = struct{}{}
		references[string(reference)] = struct{}{}
		result[index] = OwnedChild{
			SessionID: child.SessionID, ProviderID: child.ProviderID,
			NativeReference: replay.OpaqueRefFromBytes(reference),
		}
	}
	return result, nil
}

// startCodexManagerSession and resumeCodexManagerSession are single-peer
// primitives used by the per-Manager factory. The provider discovers native
// project instructions from CWD; TaskAI never reads, concatenates, mutates, or
// persists those files.
func startCodexManagerSession(ctx context.Context, app CodexAppServer, cwd string) (NativeManagerSession, error) {
	if !codexManagerBoundaryReady(app) || cwd == "" || !filepath.IsAbs(cwd) {
		return NativeManagerSession{}, ErrProviderUnsupported
	}
	session, err := app.ThreadStart(ctx, ThreadStartParams{
		CWD:                   cwd,
		DeveloperInstructions: ManagerPrefix,
		DynamicTools:          []DynamicTool{DelegateCapability()},
	})
	if err != nil || session.ThreadID == "" {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager start", Err: ErrTransport}
	}
	reference, err := replay.NewCodexReference(session.ThreadID)
	if err != nil {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager reference", Err: ErrProviderUnsupported}
	}
	connection, err := app.ConnectManager(reference)
	if err != nil || connection == nil || !bytes.Equal(connection.NativeReference().Bytes(), reference.Bytes()) {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager connection", Err: ErrTransport}
	}
	return NativeManagerSession{NativeReference: reference, Connection: connection}, nil
}

func resumeCodexManagerSession(ctx context.Context, app CodexAppServer, nativeReference replay.OpaqueRef) (NativeManagerSession, error) {
	if !codexManagerBoundaryReady(app) {
		return NativeManagerSession{}, ErrProviderUnsupported
	}
	threadID, err := replay.ResolveCodexReference(nativeReference)
	if err != nil {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager reference", Err: ErrProviderUnsupported}
	}
	session, err := app.ThreadResume(ctx, ThreadResumeParams{ThreadID: threadID})
	if err != nil || session.ThreadID != threadID {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager resume", Err: ErrTransport}
	}
	// Pinned App Server 0.150.1 restores stored dynamic tools when exact Resume
	// succeeds. No synthetic probe, replacement Start, or dynamicTools
	// reinjection is permitted here. The registered request handler is the
	// connector-side pre-turn basis for servicing the restored tool call.
	connection, err := app.ConnectManager(nativeReference)
	if err != nil || connection == nil || !bytes.Equal(connection.NativeReference().Bytes(), nativeReference.Bytes()) {
		return NativeManagerSession{}, &BoundaryError{Stage: "manager connection", Err: ErrTransport}
	}
	return NativeManagerSession{
		NativeReference: replay.OpaqueRefFromBytes(nativeReference.Bytes()),
		Connection:      connection,
	}, nil
}

func codexManagerBoundaryReady(app CodexAppServer) bool {
	return app != nil && app.Version() == CodexAppServerVersion && app.DelegateHandlerRegistered()
}

// delegatedManagerConnection makes the Manager transport and its private
// Delegator one supervisor-owned lifetime. A transport death or a fatal
// lifecycle delivery failure interrupts the whole composite; Close stops only
// this exact composite.
type delegatedManagerConnection struct {
	base     ManagerConnection
	delegate *Delegator
	app      CodexAppServer
	closing  chan struct{}
	done     chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	err       error
}

func newDelegatedManagerConnection(
	ctx context.Context,
	base ManagerConnection,
	delegate *Delegator,
	app CodexAppServer,
) ManagerConnection {
	connection := &delegatedManagerConnection{
		base: base, delegate: delegate, app: app,
		closing: make(chan struct{}), done: make(chan struct{}),
	}
	go connection.watch(ctx)
	return connection
}

func (c *delegatedManagerConnection) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		c.shutdown(&BoundaryError{Stage: "manager supervisor", Err: ErrTransport})
	case <-c.base.Done():
		c.shutdown(&BoundaryError{Stage: "manager transport", Err: ErrTransport})
	case <-c.delegate.closing:
		err := c.delegate.Err()
		if err == nil {
			err = &BoundaryError{Stage: "manager delegation", Err: ErrTransport}
		}
		c.shutdown(err)
	case <-c.closing:
	}
}

func (c *delegatedManagerConnection) NativeReference() replay.OpaqueRef {
	if c == nil || c.base == nil {
		return replay.OpaqueRef{}
	}
	return replay.OpaqueRefFromBytes(c.base.NativeReference().Bytes())
}

func (c *delegatedManagerConnection) PublicSessionID() string {
	if c == nil || c.base == nil {
		return ""
	}
	return c.base.PublicSessionID()
}

func (c *delegatedManagerConnection) Prompt(ctx context.Context, prompt string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
	if err := c.available(); err != nil {
		return nil, err
	}
	return c.base.Prompt(ctx, prompt, onFrame)
}

func (c *delegatedManagerConnection) Interrupt(ctx context.Context) error {
	if err := c.available(); err != nil {
		return err
	}
	return c.base.Interrupt(ctx)
}

func (c *delegatedManagerConnection) Done() <-chan struct{} { return c.done }

func (c *delegatedManagerConnection) Err() error {
	if c == nil {
		return &BoundaryError{Stage: "manager runtime", Err: ErrTransport}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *delegatedManagerConnection) Close() error {
	if c == nil {
		return nil
	}
	c.shutdown(nil)
	return nil
}

func (c *delegatedManagerConnection) available() error {
	if c == nil || c.base == nil || c.delegate == nil {
		return ErrTransport
	}
	select {
	case <-c.closing:
		if err := c.Err(); err != nil {
			return err
		}
		return ErrTransport
	default:
		return nil
	}
}

func (c *delegatedManagerConnection) shutdown(cause error) {
	c.closeOnce.Do(func() {
		if cause != nil {
			c.mu.Lock()
			c.err = cause
			c.mu.Unlock()
		}
		// Reject new work immediately, while reserving Done for the stronger
		// cleanup-complete contract consumed by manager.Instance.
		close(c.closing)
		if cause != nil {
			c.delegate.failTree(&BoundaryError{Stage: "manager runtime", Err: ErrTransport})
		}
		_ = c.base.Close()
		_ = c.app.Close()
		_ = c.delegate.Close()
		close(c.done)
	})
}
