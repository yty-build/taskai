package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

var fixtureTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

type recordedResume struct {
	parent    string
	reference []byte
	children  []recordedChild
	registry  *connector.Registry
	events    connector.ChildEventSink
	ids       connector.IDSource
}

type recordedChild struct {
	id, provider string
	reference    []byte
}

type fakeManagerPorts struct {
	mu sync.Mutex

	startCalls   []connector.ManagerStartRequest
	resumeCalls  []recordedResume
	connections  []*fakeConnection
	next         int
	startErr     error
	resumeErr    error
	startRef     []byte
	resumeRef    []byte
	startGate    <-chan struct{}
	startEntered chan<- struct{}
}

func (ports *fakeManagerPorts) Start(ctx context.Context, request connector.ManagerStartRequest) (connector.NativeManagerSession, error) {
	ports.mu.Lock()
	ports.startCalls = append(ports.startCalls, request)
	gate := ports.startGate
	entered := ports.startEntered
	err := ports.startErr
	startRef := append([]byte(nil), ports.startRef...)
	hasStartRef := ports.startRef != nil
	connection := ports.nextConnectionLocked()
	ports.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return connector.NativeManagerSession{}, ctx.Err()
		}
	}
	if err != nil {
		return connector.NativeManagerSession{}, err
	}
	if connection == nil {
		return connector.NativeManagerSession{}, connector.ErrTransport
	}
	connection.observeLifetime(ctx)
	reference := connection.NativeReference()
	if hasStartRef {
		reference = replay.OpaqueRefFromBytes(startRef)
	}
	return connector.NativeManagerSession{
		NativeReference: reference, Connection: connection,
	}, nil
}

func (ports *fakeManagerPorts) Resume(ctx context.Context, request connector.ManagerResumeRequest) (connector.NativeManagerSession, error) {
	recorded := recordedResume{
		parent: request.ParentSessionID, reference: request.NativeReference.Bytes(),
		registry: request.Registry, events: request.Events, ids: request.IDs,
		children: make([]recordedChild, len(request.OwnedChildren)),
	}
	for index, child := range request.OwnedChildren {
		recorded.children[index] = recordedChild{
			id: child.SessionID, provider: child.ProviderID, reference: child.NativeReference.Bytes(),
		}
	}
	ports.mu.Lock()
	ports.resumeCalls = append(ports.resumeCalls, recorded)
	err := ports.resumeErr
	resumeRef := append([]byte(nil), ports.resumeRef...)
	hasResumeRef := ports.resumeRef != nil
	connection := ports.nextConnectionLocked()
	ports.mu.Unlock()
	if err != nil {
		return connector.NativeManagerSession{}, err
	}
	if connection == nil {
		return connector.NativeManagerSession{}, connector.ErrTransport
	}
	connection.observeLifetime(ctx)
	reference := connection.NativeReference()
	if hasResumeRef {
		reference = replay.OpaqueRefFromBytes(resumeRef)
	}
	return connector.NativeManagerSession{
		NativeReference: reference, Connection: connection,
	}, nil
}

func (ports *fakeManagerPorts) nextConnectionLocked() *fakeConnection {
	if ports.next >= len(ports.connections) {
		return nil
	}
	connection := ports.connections[ports.next]
	ports.next++
	return connection
}

func (ports *fakeManagerPorts) counts() (starts, resumes int) {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return len(ports.startCalls), len(ports.resumeCalls)
}

func (ports *fakeManagerPorts) startRequest(index int) connector.ManagerStartRequest {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return ports.startCalls[index]
}

func (ports *fakeManagerPorts) resumeRequest(index int) recordedResume {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	return ports.resumeCalls[index]
}

type promptFunction func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error)

type fakeConnection struct {
	reference replay.OpaqueRef

	mu             sync.Mutex
	prompts        []string
	promptCalls    int
	interrupts     int
	closes         int
	nativeReads    int
	err            error
	interruptErr   error
	closeErr       error
	promptFunction promptFunction
	interruptGate  <-chan struct{}
	interruptEnter chan<- struct{}

	done     chan struct{}
	doneOnce sync.Once
}

func newFakeConnection(reference string) *fakeConnection {
	return &fakeConnection{
		reference: replay.OpaqueRefFromBytes([]byte(reference)),
		done:      make(chan struct{}),
	}
}

func (connection *fakeConnection) NativeReference() replay.OpaqueRef {
	return replay.OpaqueRefFromBytes(connection.reference.Bytes())
}

func (connection *fakeConnection) Prompt(
	ctx context.Context,
	prompt string,
	onFrame func(json.RawMessage) error,
) (json.RawMessage, error) {
	connection.mu.Lock()
	connection.prompts = append(connection.prompts, prompt)
	connection.promptCalls++
	function := connection.promptFunction
	connection.mu.Unlock()
	if function != nil {
		return function(ctx, prompt, onFrame)
	}
	if err := onFrame(json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sequence":1}}`)); err != nil {
		return nil, err
	}
	return json.RawMessage(`{"stopReason":"end_turn"}`), nil
}

func (connection *fakeConnection) Interrupt(ctx context.Context) error {
	connection.mu.Lock()
	connection.interrupts++
	err := connection.interruptErr
	gate := connection.interruptGate
	entered := connection.interruptEnter
	connection.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	return connector.ErrStaleTurn
}

func (connection *fakeConnection) ReadNativeSession(context.Context) (json.RawMessage, error) {
	connection.mu.Lock()
	connection.nativeReads++
	connection.mu.Unlock()
	return json.RawMessage(`{"private":"must-not-be-read"}`), nil
}

func (connection *fakeConnection) Done() <-chan struct{} { return connection.done }

func (connection *fakeConnection) Err() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.err
}

func (connection *fakeConnection) Close() error {
	connection.mu.Lock()
	connection.closes++
	err := connection.closeErr
	connection.mu.Unlock()
	connection.doneOnce.Do(func() { close(connection.done) })
	return err
}

func (connection *fakeConnection) crash(err error) {
	connection.mu.Lock()
	connection.err = err
	connection.mu.Unlock()
	connection.doneOnce.Do(func() { close(connection.done) })
}

func (connection *fakeConnection) observeLifetime(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			connection.crash(ctx.Err())
		case <-connection.done:
		}
	}()
}

type connectionSnapshot struct {
	prompts                         []string
	promptCalls, interrupts, closes int
	nativeReads                     int
	err                             error
}

func (connection *fakeConnection) snapshot() connectionSnapshot {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connectionSnapshot{
		prompts:     append([]string(nil), connection.prompts...),
		promptCalls: connection.promptCalls, interrupts: connection.interrupts,
		closes: connection.closes, nativeReads: connection.nativeReads, err: connection.err,
	}
}

type fakeChildren struct {
	mu      sync.Mutex
	new     int
	resumes int
}

func (children *fakeChildren) New(context.Context) (connector.ChildPort, error) {
	children.mu.Lock()
	children.new++
	children.mu.Unlock()
	return nil, errors.New("test child launch must not be called")
}

func (children *fakeChildren) Resume(context.Context, replay.OpaqueRef) (connector.ChildPort, error) {
	children.mu.Lock()
	children.resumes++
	children.mu.Unlock()
	return nil, errors.New("test child resume must not be called")
}

func (children *fakeChildren) counts() (newCalls, resumeCalls int) {
	children.mu.Lock()
	defer children.mu.Unlock()
	return children.new, children.resumes
}

type fakeAuth struct {
	mu            sync.Mutex
	probes, auths int
}

func (auth *fakeAuth) Probe(context.Context) connector.AuthState {
	auth.mu.Lock()
	auth.probes++
	auth.mu.Unlock()
	return connector.AuthConnected
}

func (auth *fakeAuth) Authenticate(context.Context, connector.AuthIO) error {
	auth.mu.Lock()
	auth.auths++
	auth.mu.Unlock()
	return nil
}

func (auth *fakeAuth) counts() (probes, authenticates int) {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	return auth.probes, auth.auths
}

type fakeProfile struct {
	id       string
	ports    *fakeManagerPorts
	children *fakeChildren
	auth     *fakeAuth
	resume   bool
	manager  bool
}

func newRegistry(t *testing.T, profiles ...fakeProfile) *connector.Registry {
	t.Helper()
	registry := connector.NewRegistry()
	for _, profile := range profiles {
		versions := connector.Versions{
			ACPProtocol: 1, ManagerRuntime: "test-runtime", Connector: "test-connector-" + profile.id,
			ProviderHarness: "test-harness", ConnectorSDK: "test-sdk",
		}
		proven := connector.Capabilities{Manager: profile.manager, Subordinate: profile.children != nil, Resume: profile.resume}
		var managerPort connector.ManagerFactory
		var resumePort connector.ManagerResumeFactory
		if profile.manager {
			managerPort = profile.ports
		}
		if profile.resume && profile.manager {
			resumePort = profile.ports
		}
		registered := connector.NewConformedProfile(
			profile.id, "Test "+profile.id, versions,
			connector.Conformance{Exact: versions, Proven: proven},
			profile.auth, profile.children, profile.children,
			managerPort, resumePort, connector.ReplayBinding{},
		)
		if err := registry.Register(registered); err != nil {
			t.Fatalf("Register(%s): %v", profile.id, err)
		}
	}
	return registry
}

func managerMetadata(t *testing.T, id, provider string, reference []byte) session.Session {
	t.Helper()
	value, err := session.NewManager(
		session.ID(id), task.ID("task-main"), "Manager "+id, provider,
		reference, session.StateStarting, fixtureTime,
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return value
}

func childMetadata(t *testing.T, id, parent, provider string, reference []byte) session.Session {
	t.Helper()
	value, err := session.NewSubordinate(
		session.ID(id), task.ID("task-main"), session.ID(parent), "Child "+id,
		provider, reference, session.StateDormant, fixtureTime,
	)
	if err != nil {
		t.Fatalf("NewSubordinate: %v", err)
	}
	return value
}

func noOpChildEvents() connector.ChildEventSink {
	return connector.ChildEventSinkFunc(func(context.Context, connector.ChildEvent) error { return nil })
}

type fakeEventSink struct {
	mu     sync.Mutex
	events []connector.ChildEvent
}

func (sink *fakeEventSink) ChildEvent(_ context.Context, event connector.ChildEvent) error {
	sink.mu.Lock()
	sink.events = append(sink.events, event)
	sink.mu.Unlock()
	return nil
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
