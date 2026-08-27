package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

const (
	testProvider = settings.ProviderID("provider")
	testClientA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testClientB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testLaunch = settings.LaunchConfig{Argv: []string{"fake-provider", "agent"}}

type fixedClock struct{ value time.Time }

func (clock fixedClock) Now() time.Time { return clock.value }

type queuedIDs struct {
	mu       sync.Mutex
	tasks    []task.ID
	sessions []session.ID
}

func (ids *queuedIDs) NewTaskID() (task.ID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if len(ids.tasks) == 0 {
		return "", errors.New("empty task ID fixture")
	}
	value := ids.tasks[0]
	ids.tasks = ids.tasks[1:]
	return value, nil
}

func (ids *queuedIDs) NewSessionID() (session.ID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if len(ids.sessions) == 0 {
		return "", errors.New("empty session ID fixture")
	}
	value := ids.sessions[0]
	ids.sessions = ids.sessions[1:]
	return value, nil
}

type fakeAuth struct {
	mu     sync.Mutex
	probes int
	state  connector.AuthState
}

func (auth *fakeAuth) Probe(context.Context) connector.AuthState {
	auth.mu.Lock()
	auth.probes++
	state := auth.state
	auth.mu.Unlock()
	return state
}

func (*fakeAuth) Authenticate(context.Context, connector.AuthIO) error { return nil }

func (auth *fakeAuth) count() int {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	return auth.probes
}

type profilePorts struct{}

func (profilePorts) New(context.Context) (connector.ChildPort, error) {
	return nil, connector.ErrProviderUnsupported
}

func (profilePorts) Start(context.Context, connector.ManagerStartRequest) (connector.NativeManagerSession, error) {
	return connector.NativeManagerSession{}, connector.ErrProviderUnsupported
}

func (profilePorts) Resume(context.Context, connector.ManagerResumeRequest) (connector.NativeManagerSession, error) {
	return connector.NativeManagerSession{}, connector.ErrProviderUnsupported
}

func testRegistry(t *testing.T, auth connector.AuthPort) *connector.Registry {
	t.Helper()
	versions := connector.Versions{
		ACPProtocol: 1, ManagerRuntime: "test-manager", Connector: "test-connector",
		ProviderHarness: "test-provider", ConnectorSDK: "test-sdk",
	}
	conformance := connector.Conformance{
		Exact: versions,
		Proven: connector.Capabilities{
			Manager: true, Subordinate: true, Resume: true,
		},
	}
	ports := profilePorts{}
	profile := connector.NewConformedProfile(
		string(testProvider), "Provider", versions, conformance, auth,
		ports, nil, ports, ports, connector.ReplayBinding{},
	)
	registry := connector.NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return registry
}

type fakeHandle struct {
	id       session.ID
	provider string
	ref      []byte
	done     chan struct{}

	mu        sync.Mutex
	available bool
	terminal  manager.Terminal
	stopOnce  sync.Once
	stopFn    func(context.Context) error
	sendFn    func(context.Context, string, manager.FrameSink) (json.RawMessage, error)
}

func newFakeHandle(id session.ID, reference []byte) *fakeHandle {
	return &fakeHandle{
		id: id, provider: string(testProvider), ref: append([]byte(nil), reference...),
		done: make(chan struct{}), available: true,
	}
}

func (handle *fakeHandle) ID() session.ID          { return handle.id }
func (handle *fakeHandle) ProviderID() string      { return handle.provider }
func (handle *fakeHandle) NativeReference() []byte { return append([]byte(nil), handle.ref...) }
func (handle *fakeHandle) Done() <-chan struct{}   { return handle.done }

func (handle *fakeHandle) Available() bool {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	return handle.available
}

func (handle *fakeHandle) Terminal() (manager.Terminal, bool) {
	select {
	case <-handle.done:
		handle.mu.Lock()
		defer handle.mu.Unlock()
		return handle.terminal, true
	default:
		return manager.Terminal{}, false
	}
}

func (handle *fakeHandle) Send(
	ctx context.Context,
	prompt string,
	sink manager.FrameSink,
) (json.RawMessage, error) {
	if handle.sendFn != nil {
		return handle.sendFn(ctx, prompt, sink)
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (handle *fakeHandle) Stop(ctx context.Context) error {
	var result error
	handle.stopOnce.Do(func() {
		if handle.stopFn != nil {
			result = handle.stopFn(ctx)
		}
		handle.finish(manager.Terminal{Kind: manager.TerminalStopped})
	})
	return result
}

func (handle *fakeHandle) finish(terminal manager.Terminal) {
	handle.mu.Lock()
	if !handle.available {
		handle.mu.Unlock()
		return
	}
	handle.available = false
	handle.terminal = terminal
	close(handle.done)
	handle.mu.Unlock()
}

func (handle *fakeHandle) crash() {
	handle.finish(manager.Terminal{Kind: manager.TerminalFailed, Code: manager.CodeTransport})
}

type fakeBackend struct {
	mu sync.Mutex

	active      map[session.ID]*fakeHandle
	startCalls  []manager.StartRequest
	resumeCalls []manager.ResumeRequest
	startFn     func(manager.StartRequest) (managerHandle, error)
	resumeFn    func(manager.ResumeRequest) (managerHandle, error)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{active: make(map[session.ID]*fakeHandle)}
}

func (backend *fakeBackend) ActiveIDs() []session.ID {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	ids := make([]session.ID, 0, len(backend.active))
	for id, handle := range backend.active {
		if handle.Available() {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

func (backend *fakeBackend) Lookup(id session.ID) (managerHandle, bool) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	handle := backend.active[id]
	return handle, handle != nil && handle.Available()
}

func (backend *fakeBackend) Start(_ context.Context, request manager.StartRequest) (managerHandle, error) {
	backend.mu.Lock()
	backend.startCalls = append(backend.startCalls, request)
	function := backend.startFn
	backend.mu.Unlock()
	if function != nil {
		return function(request)
	}
	handle := newFakeHandle(request.Manager.ID, []byte("manager-ref-"+request.Manager.ID))
	backend.mu.Lock()
	backend.active[request.Manager.ID] = handle
	backend.mu.Unlock()
	return handle, nil
}

func (backend *fakeBackend) Resume(_ context.Context, request manager.ResumeRequest) (managerHandle, error) {
	backend.mu.Lock()
	backend.resumeCalls = append(backend.resumeCalls, request)
	function := backend.resumeFn
	backend.mu.Unlock()
	if function != nil {
		return function(request)
	}
	handle := newFakeHandle(request.Manager.ID, request.Manager.ProviderReference)
	backend.mu.Lock()
	backend.active[request.Manager.ID] = handle
	backend.mu.Unlock()
	return handle, nil
}

func (backend *fakeBackend) counts() (int, int) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return len(backend.startCalls), len(backend.resumeCalls)
}

type runtimeHarness struct {
	store      *store.Store
	supervisor *Supervisor
	client     ClientPort
	backend    *fakeBackend
	auth       *fakeAuth
	ids        *queuedIDs
	cwd        string
}

func newRuntimeHarness(
	t *testing.T,
	metadata MetadataStore,
	backend *fakeBackend,
	ids *queuedIDs,
) *runtimeHarness {
	t.Helper()
	var persistent *store.Store
	if metadata == nil {
		opened, err := store.Open(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		persistent = opened
		metadata = opened
	}
	if backend == nil {
		backend = newFakeBackend()
	}
	if ids == nil {
		ids = &queuedIDs{}
	}
	auth := &fakeAuth{state: connector.AuthConnected}
	registry := testRegistry(t, auth)
	provider, err := settings.NewProvider(
		testProvider, true, settings.ProbeConnected, testLaunch,
		settings.Capabilities{Manager: true, Subordinate: true, Resume: true},
	)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, getErr := metadata.GetProviderSettings(context.Background(), testProvider); errors.Is(getErr, store.ErrNotFound) {
		if err := metadata.PutProviderSettings(context.Background(), provider); err != nil {
			t.Fatalf("PutProviderSettings: %v", err)
		}
	} else if getErr != nil {
		t.Fatalf("GetProviderSettings: %v", getErr)
	}
	cwd := t.TempDir()
	supervisor, err := newSupervisor(context.Background(), Config{
		Store: metadata, Registry: registry, ProjectCWD: cwd,
		Providers: []ProviderSpec{{ID: testProvider, Launch: testLaunch}},
		Clock:     fixedClock{value: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)},
		TaskIDs:   ids, SessionIDs: ids,
	}, backend)
	if err != nil {
		t.Fatalf("newSupervisor: %v", err)
	}
	client, err := supervisor.OpenClient(testClientA)
	if err != nil {
		t.Fatalf("OpenClient: %v", err)
	}
	if persistent != nil {
		t.Cleanup(func() { _ = persistent.Close() })
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	t.Cleanup(func() { _ = client.Close() })
	return &runtimeHarness{
		store: persistent, supervisor: supervisor, client: client,
		backend: backend, auth: auth, ids: ids, cwd: cwd,
	}
}

func drainEvents(t *testing.T, client ClientPort) {
	t.Helper()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("drain Snapshot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cursor := Cursor{Generation: snapshot.StreamGeneration, After: snapshot.ThroughSequence}
		for {
			event, nextErr := client.Next(ctx, cursor)
			if nextErr != nil {
				return
			}
			clearEvent(&event)
			cursor.After = event.Sequence
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func seedTaskTree(
	t *testing.T,
	metadata MetadataStore,
	state session.State,
) (task.Task, session.Session, session.Session) {
	t.Helper()
	stamp := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	ownedTask, err := task.New("task-seeded", "Seeded", stamp)
	if err != nil {
		t.Fatal(err)
	}
	managerSession, err := session.NewManager(
		"manager-seeded", ownedTask.ID, "Manager", string(testProvider), []byte("manager-native-ref"), state, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := session.NewSubordinate(
		"child-seeded", ownedTask.ID, managerSession.ID, "", string(testProvider), []byte("child-native-ref"), state, stamp,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateTask(context.Background(), ownedTask); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateSession(context.Background(), managerSession); err != nil {
		t.Fatal(err)
	}
	if err := metadata.CreateSession(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	return ownedTask, managerSession, child
}

func TestInitialSnapshotDoesNotLaunchProbeResumeOrReplay(t *testing.T) {
	harness := newRuntimeHarness(t, nil, nil, nil)
	snapshot, err := harness.client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	starts, resumes := harness.backend.counts()
	if starts != 0 || resumes != 0 || harness.auth.count() != 0 {
		t.Fatalf("construction side effects: start=%d resume=%d probe=%d", starts, resumes, harness.auth.count())
	}
	if len(snapshot.Tasks) != 0 || len(snapshot.Sessions) != 0 || len(snapshot.Providers) != 1 {
		t.Fatalf("unexpected initial snapshot: %#v", snapshot)
	}
}

func TestFreshWorkerReconcilesExactActiveTreeBeforeSnapshot(t *testing.T) {
	metadata, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	_, managerSession, child := seedTaskTree(t, metadata, session.StateActive)
	harness := newRuntimeHarness(t, metadata, newFakeBackend(), nil)

	storedManager, err := metadata.GetSession(context.Background(), managerSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedChild, err := metadata.GetSession(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedManager.State != session.StateInterrupted || storedChild.State != session.StateInterrupted ||
		!storedManager.LastActiveAt.Equal(storedChild.LastActiveAt) ||
		!storedManager.LastActiveAt.After(managerSession.LastActiveAt) {
		t.Fatalf("reconciled manager=%#v child=%#v", storedManager, storedChild)
	}
	starts, resumes := harness.backend.counts()
	if starts != 0 || resumes != 0 || harness.auth.count() != 0 {
		t.Fatalf("reconciliation launched work: %d %d %d", starts, resumes, harness.auth.count())
	}
}

func TestAddManagerPersistsStartingThenExactReferenceAndActive(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-add"}, sessions: []session.ID{"manager-add"}}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, err := harness.client.AddTask(context.Background(), "Task")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Attach(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Primary")
	if err != nil {
		t.Fatalf("AddManager: %v", err)
	}
	stored, err := harness.store.GetSession(context.Background(), managerID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || !bytes.Equal(stored.ProviderReference, []byte("manager-ref-manager-add")) {
		t.Fatalf("stored manager = %#v", stored)
	}
	harness.backend.mu.Lock()
	requests := append([]manager.StartRequest(nil), harness.backend.startCalls...)
	harness.backend.mu.Unlock()
	if len(requests) != 1 || requests[0].ProjectCWD != harness.cwd || requests[0].Manager.State != session.StateStarting ||
		len(requests[0].Manager.ProviderReference) != 0 {
		t.Fatalf("Start requests = %#v", requests)
	}
}

func TestAddManagerFailureUsesOneInterruptedRowAndNeverRetries(t *testing.T) {
	backend := newFakeBackend()
	backend.startFn = func(manager.StartRequest) (managerHandle, error) {
		return nil, errors.New("provider content must not escape")
	}
	ids := &queuedIDs{tasks: []task.ID{"task-fail"}, sessions: []session.ID{"manager-fail"}}
	harness := newRuntimeHarness(t, nil, backend, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Primary")
	addErr := err
	if !errors.Is(addErr, ErrTransport) || managerID != "manager-fail" {
		t.Fatalf("AddManager = %q, %v", managerID, addErr)
	}
	stored, err := harness.store.GetSession(context.Background(), managerID)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := harness.store.ListSessions(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	starts, resumes := backend.counts()
	if len(owned) != 1 || stored.State != session.StateInterrupted || len(stored.ProviderReference) != 0 ||
		starts != 1 || resumes != 0 {
		t.Fatalf("owned=%#v starts=%d resumes=%d", owned, starts, resumes)
	}
	if codeOf(addErr) != CodeTransport {
		t.Fatalf("unsafe error classification: %v", addErr)
	}
}

func TestResumePassesExactReferenceAndCompleteDirectChildrenWithoutStart(t *testing.T) {
	metadata, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	ownedTask, managerSession, child := seedTaskTree(t, metadata, session.StateInterrupted)
	backend := newFakeBackend()
	harness := newRuntimeHarness(t, metadata, backend, nil)
	drainEvents(t, harness.client)
	if err := harness.client.Attach(context.Background(), ownedTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Resume(context.Background(), managerSession.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	backend.mu.Lock()
	requests := append([]manager.ResumeRequest(nil), backend.resumeCalls...)
	backend.mu.Unlock()
	starts, resumes := backend.counts()
	if starts != 0 || resumes != 1 || len(requests) != 1 ||
		!bytes.Equal(requests[0].Manager.ProviderReference, managerSession.ProviderReference) ||
		len(requests[0].OwnedChildren) != 1 || requests[0].OwnedChildren[0].ID != child.ID {
		t.Fatalf("start=%d resume=%d requests=%#v", starts, resumes, requests)
	}
	storedChild, err := metadata.GetSession(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedChild.State != session.StateInterrupted || !bytes.Equal(storedChild.ProviderReference, child.ProviderReference) {
		t.Fatalf("child changed during exact Resume: %#v", storedChild)
	}
}

func TestResumeFailureKeepsExactTreeInterruptedAndReferenceUnchanged(t *testing.T) {
	metadata, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	ownedTask, managerSession, child := seedTaskTree(t, metadata, session.StateInterrupted)
	backend := newFakeBackend()
	backend.resumeFn = func(manager.ResumeRequest) (managerHandle, error) {
		return nil, errors.New("native resume detail must be hidden")
	}
	harness := newRuntimeHarness(t, metadata, backend, nil)
	drainEvents(t, harness.client)
	if err := harness.client.Attach(context.Background(), ownedTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Resume(context.Background(), managerSession.ID); !errors.Is(err, ErrTransport) {
		t.Fatalf("Resume = %v", err)
	}
	storedManager, _ := metadata.GetSession(context.Background(), managerSession.ID)
	storedChild, _ := metadata.GetSession(context.Background(), child.ID)
	if storedManager.State != session.StateInterrupted || storedChild.State != session.StateInterrupted ||
		!bytes.Equal(storedManager.ProviderReference, managerSession.ProviderReference) ||
		!bytes.Equal(storedChild.ProviderReference, child.ProviderReference) {
		t.Fatalf("failed resume manager=%#v child=%#v", storedManager, storedChild)
	}
	starts, resumes := backend.counts()
	if starts != 0 || resumes != 1 {
		t.Fatalf("fallback occurred: starts=%d resumes=%d", starts, resumes)
	}
}

func TestChildEventsPersistBeforeAckAndTerminalBecomesAnswerDormant(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-child"}, sessions: []session.ID{"manager-child"}}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	harness.backend.mu.Lock()
	sink := harness.backend.startCalls[0].ChildEvents
	harness.backend.mu.Unlock()
	event := connector.ChildEvent{
		ParentSessionID: string(managerID), ChildSessionID: "child-created",
		Provider: string(testProvider), NativeReference: []byte("child-exact-ref"),
		Lifecycle: "prompt_pending", Mode: connector.ModeFollow,
		RequestID: "request-1", TurnID: "turn-1",
	}
	if err := sink.ChildEvent(context.Background(), event); err != nil {
		t.Fatalf("prompt_pending: %v", err)
	}
	stored, err := harness.store.GetSession(context.Background(), "child-created")
	if err != nil || stored.State != session.StateActive || !bytes.Equal(stored.ProviderReference, event.NativeReference) {
		t.Fatalf("persisted before ack = %#v, %v", stored, err)
	}
	event.Lifecycle = "terminal"
	event.NativeReference = nil
	if err := sink.ChildEvent(context.Background(), event); err != nil {
		t.Fatalf("terminal: %v", err)
	}
	stored, err = harness.store.GetSession(context.Background(), "child-created")
	if err != nil || stored.State != session.StateDormant {
		t.Fatalf("terminal persisted = %#v, %v", stored, err)
	}
	snapshot, err := harness.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range snapshot.Sessions {
		if value.ID == "child-created" && value.Mode != connector.ModeAnswer {
			t.Fatalf("terminal child mode = %s", value.Mode)
		}
	}
}

type failChildStore struct {
	*store.Store
	fail bool
}

func (metadata *failChildStore) CreateSession(ctx context.Context, value session.Session) error {
	if metadata.fail && value.Role == session.RoleSubordinate {
		return errors.New("persistence detail must be hidden")
	}
	return metadata.Store.CreateSession(ctx, value)
}

func TestChildPersistenceFailureIsFatalToExactManagerTree(t *testing.T) {
	base, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	metadata := &failChildStore{Store: base}
	ids := &queuedIDs{tasks: []task.ID{"task-child-fail"}, sessions: []session.ID{"manager-child-fail"}}
	harness := newRuntimeHarness(t, metadata, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	harness.backend.mu.Lock()
	sink := harness.backend.startCalls[0].ChildEvents
	harness.backend.mu.Unlock()
	harness.supervisor.mu.Lock()
	entry := harness.supervisor.live[managerID]
	harness.supervisor.mu.Unlock()
	metadata.fail = true
	err = sink.ChildEvent(context.Background(), connector.ChildEvent{
		ParentSessionID: string(managerID), ChildSessionID: "child-rejected",
		Provider: string(testProvider), NativeReference: []byte("child-ref"),
		Lifecycle: "prompt_pending", Mode: connector.ModeAnswer,
		RequestID: "request-1", TurnID: "turn-1",
	})
	if !errors.Is(err, ErrPersistence) {
		t.Fatalf("ChildEvent = %v", err)
	}
	<-entry.done
	stored, err := base.GetSession(context.Background(), managerID)
	if err != nil || stored.State != session.StateInterrupted {
		t.Fatalf("fatal manager = %#v, %v", stored, err)
	}
	if _, err := base.GetSession(context.Background(), "child-rejected"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected child exists: %v", err)
	}
}

func TestConcurrentManagersRemainIsolatedAcrossTasks(t *testing.T) {
	ids := &queuedIDs{
		tasks:    []task.ID{"task-one", "task-two"},
		sessions: []session.ID{"manager-one", "manager-two"},
	}
	backend := newFakeBackend()
	arrived := make(chan session.ID, 2)
	release := make(chan struct{})
	var promptMu sync.Mutex
	prompts := make(map[session.ID]string)
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("ref-"+request.Manager.ID))
		handle.sendFn = func(_ context.Context, prompt string, _ manager.FrameSink) (json.RawMessage, error) {
			arrived <- request.Manager.ID
			<-release
			promptMu.Lock()
			prompts[request.Manager.ID] = prompt
			promptMu.Unlock()
			return json.RawMessage(`{"ok":true}`), nil
		}
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	clientB, err := harness.supervisor.OpenClient(testClientB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientB.Close() })
	drainEvents(t, harness.client)
	drainEvents(t, clientB)
	taskOne, _ := harness.client.AddTask(context.Background(), "One")
	taskTwo, _ := clientB.AddTask(context.Background(), "Two")
	_ = harness.client.Attach(context.Background(), taskOne)
	_ = clientB.Attach(context.Background(), taskTwo)
	managerOne, err := harness.client.AddManager(context.Background(), taskOne, testProvider, "One")
	if err != nil {
		t.Fatal(err)
	}
	managerTwo, err := clientB.AddManager(context.Background(), taskTwo, testProvider, "Two")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- harness.client.Send(context.Background(), managerOne, "prompt-one") }()
	go func() { results <- clientB.Send(context.Background(), managerTwo, "prompt-two") }()
	first, second := <-arrived, <-arrived
	if first == second {
		t.Fatalf("same Manager received concurrent prompts: %s", first)
	}
	close(release)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	promptMu.Lock()
	defer promptMu.Unlock()
	if prompts[managerOne] != "prompt-one" || prompts[managerTwo] != "prompt-two" {
		t.Fatalf("prompt routing = %#v", prompts)
	}
}

func TestDetachReattachNeverStartsOrAuthenticatesAgain(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-attach"}, sessions: []session.ID{"manager-attach"}}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Detach(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Attach(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Send(context.Background(), managerID, "exact prompt"); err != nil {
		t.Fatal(err)
	}
	starts, resumes := harness.backend.counts()
	if starts != 1 || resumes != 0 || harness.auth.count() != 0 {
		t.Fatalf("reattach side effects: start=%d resume=%d probe=%d", starts, resumes, harness.auth.count())
	}
}

func TestDetachedDuringSendDiscardsValidatedFrames(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-discard"}, sessions: []session.ID{"manager-discard"}}
	backend := newFakeBackend()
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("discard-ref"))
		handle.sendFn = func(ctx context.Context, _ string, sink manager.FrameSink) (json.RawMessage, error) {
			close(entered)
			<-release
			frame := json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"discard-canary"}}}}`)
			if err := sink.PublicFrame(ctx, request.Manager.ID, frame); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"done":true}`), nil
		}
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := harness.client.Snapshot(context.Background())
	sendDone := make(chan error, 1)
	go func() { sendDone <- harness.client.Send(context.Background(), managerID, "prompt") }()
	<-entered
	if err := harness.client.Detach(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-sendDone; err != nil {
		t.Fatal(err)
	}
	after, _ := harness.client.Snapshot(context.Background())
	if after.ThroughSequence != before.ThroughSequence {
		t.Fatalf("discarded frame advanced sequence: before=%d after=%d", before.ThroughSequence, after.ThroughSequence)
	}
}

func TestExplicitStopTimeoutLeavesExactTreeInterrupted(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-timeout"}, sessions: []session.ID{"manager-timeout"}}
	backend := newFakeBackend()
	entered := make(chan struct{})
	release := make(chan struct{})
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("timeout-ref"))
		handle.stopFn = func(context.Context) error {
			close(entered)
			<-release
			return nil
		}
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	harness.supervisor.stopTimeout = 20 * time.Millisecond
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- harness.client.Stop(context.Background(), managerID) }()
	<-entered
	err = <-stopDone
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("Stop = %v", err)
	}
	stored, getErr := harness.store.GetSession(context.Background(), managerID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.State != session.StateInterrupted {
		t.Fatalf("state = %s, want interrupted", stored.State)
	}
	close(release)
}

func TestManagerStopCodeIsProjectedToFiniteRuntimeCode(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-stop-code"}, sessions: []session.ID{"manager-stop-code"}}
	backend := newFakeBackend()
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("stop-code-ref"))
		handle.stopFn = func(context.Context) error { return manager.ErrStopFailed }
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	err = harness.client.Stop(context.Background(), managerID)
	if !errors.Is(err, ErrTransport) || codeOf(err) != CodeTransport || !validWireCode(codeOf(err), false) {
		t.Fatalf("Stop projection = %v (%s)", err, codeOf(err))
	}
	stored, _ := harness.store.GetSession(context.Background(), managerID)
	if stored.State != session.StateInterrupted {
		t.Fatalf("failed Stop state = %s", stored.State)
	}
}

func TestStopAndCrashFinalizeOnlyTheirExactOwnedTrees(t *testing.T) {
	ids := &queuedIDs{
		tasks:    []task.ID{"task-tree"},
		sessions: []session.ID{"manager-tree-one", "manager-tree-two"},
	}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerOne, err := harness.client.AddManager(context.Background(), taskID, testProvider, "One")
	if err != nil {
		t.Fatal(err)
	}
	managerTwo, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Two")
	if err != nil {
		t.Fatal(err)
	}
	harness.backend.mu.Lock()
	firstSink := harness.backend.startCalls[0].ChildEvents
	handleTwo := harness.backend.active[managerTwo]
	harness.backend.mu.Unlock()
	if err := firstSink.ChildEvent(context.Background(), connector.ChildEvent{
		ParentSessionID: string(managerOne), ChildSessionID: "child-tree-one",
		Provider: string(testProvider), NativeReference: []byte("child-tree-ref"),
		Lifecycle: "prompt_pending", Mode: connector.ModeAnswer,
		RequestID: "request-tree", TurnID: "turn-tree",
	}); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Stop(context.Background(), managerOne); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	storedOne, _ := harness.store.GetSession(context.Background(), managerOne)
	storedChild, _ := harness.store.GetSession(context.Background(), "child-tree-one")
	storedTwo, _ := harness.store.GetSession(context.Background(), managerTwo)
	if storedOne.State != session.StateClosed || storedChild.State != session.StateClosed || storedTwo.State != session.StateActive {
		t.Fatalf("after exact Stop: one=%s child=%s two=%s", storedOne.State, storedChild.State, storedTwo.State)
	}
	harness.supervisor.mu.Lock()
	entryTwo := harness.supervisor.live[managerTwo]
	harness.supervisor.mu.Unlock()
	handleTwo.crash()
	<-entryTwo.done
	storedTwo, _ = harness.store.GetSession(context.Background(), managerTwo)
	storedOne, _ = harness.store.GetSession(context.Background(), managerOne)
	if storedTwo.State != session.StateInterrupted || storedOne.State != session.StateClosed {
		t.Fatalf("after crash: one=%s two=%s", storedOne.State, storedTwo.State)
	}
}

func TestCurrentConformanceOverridesStoredFalseButTamperedArgvNeverLaunches(t *testing.T) {
	ids := &queuedIDs{
		tasks:    []task.ID{"task-tamper"},
		sessions: []session.ID{"manager-capability", "manager-argv"},
	}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	stored, err := harness.store.GetProviderSettings(context.Background(), testProvider)
	if err != nil {
		t.Fatal(err)
	}
	stored.Capabilities = settings.Capabilities{}
	if err := harness.store.PutProviderSettings(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Conformed")
	if err != nil || managerID != "manager-capability" {
		t.Fatalf("current conformance suppressed by stored false: %q %v", managerID, err)
	}
	stored, _ = harness.store.GetProviderSettings(context.Background(), testProvider)
	stored.LaunchConfig = settings.LaunchConfig{Argv: []string{"tampered-provider", "agent"}}
	if err := harness.store.PutProviderSettings(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	managerID, err = harness.client.AddManager(context.Background(), taskID, testProvider, "Tampered")
	if !errors.Is(err, ErrInvalidState) || managerID != "" {
		t.Fatalf("tampered argv AddManager = %q %v", managerID, err)
	}
	starts, _ := harness.backend.counts()
	if starts != 1 {
		t.Fatalf("tampered argv launched provider: starts=%d", starts)
	}
}

func TestRestartReconcilesStaleFalseCapabilitiesToCurrentCatalog(t *testing.T) {
	metadata, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	stale, err := settings.NewProvider(
		testProvider, true, settings.ProbeConnected, testLaunch, settings.Capabilities{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := metadata.PutProviderSettings(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	harness := newRuntimeHarness(t, metadata, newFakeBackend(), nil)
	snapshot, err := harness.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Providers) != 1 || !snapshot.Providers[0].Capabilities.Manager ||
		!snapshot.Providers[0].Capabilities.Subordinate || !snapshot.Providers[0].Capabilities.Resume {
		t.Fatalf("snapshot capabilities = %#v", snapshot.Providers)
	}
	reconciled, err := metadata.GetProviderSettings(context.Background(), testProvider)
	if err != nil || !reconciled.Capabilities.Manager || !reconciled.Capabilities.Resume {
		t.Fatalf("persisted capabilities = %#v, %v", reconciled, err)
	}
}

func TestClientDeathDetachesViewerWithoutStoppingManager(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-client-death"}, sessions: []session.ID{"manager-client-death"}}
	harness := newRuntimeHarness(t, nil, nil, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Close(); err != nil {
		t.Fatal(err)
	}
	clientB, err := harness.supervisor.OpenClient(testClientB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientB.Close() })
	if err := clientB.Attach(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := clientB.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundActive := false
	for _, value := range snapshot.Sessions {
		foundActive = foundActive || value.ID == managerID && value.State == session.StateActive
	}
	starts, resumes := harness.backend.counts()
	if !foundActive || starts != 1 || resumes != 0 {
		t.Fatalf("client death changed work: active=%v starts=%d resumes=%d", foundActive, starts, resumes)
	}
}

func TestWorkerAuthenticateNeverUsesProviderAuth(t *testing.T) {
	harness := newRuntimeHarness(t, nil, nil, nil)
	err := harness.client.Authenticate(context.Background(), testProvider)
	if !errors.Is(err, ErrUnsupported) || harness.auth.count() != 0 {
		t.Fatalf("Authenticate = %v, probes=%d", err, harness.auth.count())
	}
	if err := harness.client.Recheck(context.Background(), testProvider); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if harness.auth.count() != 1 {
		t.Fatalf("Recheck probes = %d", harness.auth.count())
	}
}

func TestRecheckProjectsMissingConnectorStatus(t *testing.T) {
	harness := newRuntimeHarness(t, nil, nil, nil)
	harness.auth.mu.Lock()
	harness.auth.state = connector.AuthMissingConnector
	harness.auth.mu.Unlock()
	if err := harness.client.Recheck(context.Background(), testProvider); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	stored, err := harness.store.GetProviderSettings(context.Background(), testProvider)
	if err != nil || stored.ProbeStatus != settings.ProbeMissingConnector {
		t.Fatalf("missing connector projection = %s, %v", stored.ProbeStatus, err)
	}
}

type blockingAuth struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (auth *blockingAuth) Probe(context.Context) connector.AuthState {
	auth.once.Do(func() { close(auth.entered) })
	<-auth.release
	return connector.AuthConnected
}

func (*blockingAuth) Authenticate(context.Context, connector.AuthIO) error { return nil }

func TestRecheckTimeoutPersistsNeedsRepairAndClearsBusyState(t *testing.T) {
	harness := newRuntimeHarness(t, nil, nil, nil)
	auth := &blockingAuth{entered: make(chan struct{}), release: make(chan struct{})}
	harness.supervisor.mu.Lock()
	catalog := harness.supervisor.catalog[testProvider]
	catalog.profile.Auth = auth
	harness.supervisor.catalog[testProvider] = catalog
	harness.supervisor.probeTimeout = 20 * time.Millisecond
	harness.supervisor.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- harness.client.Recheck(context.Background(), testProvider) }()
	<-auth.entered
	if err := <-result; err != nil {
		t.Fatalf("Recheck timeout: %v", err)
	}
	close(auth.release)
	stored, err := harness.store.GetProviderSettings(context.Background(), testProvider)
	if err != nil || stored.ProbeStatus != settings.ProbeNeedsRepair {
		t.Fatalf("probe status = %s, %v", stored.ProbeStatus, err)
	}
	harness.supervisor.mu.Lock()
	busy := harness.supervisor.providerOps[testProvider]
	harness.supervisor.mu.Unlock()
	if busy {
		t.Fatal("provider remained busy after bounded Recheck")
	}
}

func TestSupervisorCloseStopsAllHungManagersConcurrently(t *testing.T) {
	ids := &queuedIDs{
		tasks:    []task.ID{"task-close-concurrent"},
		sessions: []session.ID{"manager-close-one", "manager-close-two"},
	}
	backend := newFakeBackend()
	entered := make(chan session.ID, 2)
	release := make(chan struct{})
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("close-ref-"+request.Manager.ID))
		handle.stopFn = func(context.Context) error {
			entered <- request.Manager.ID
			<-release
			return nil
		}
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	harness.supervisor.stopTimeout = 2 * time.Second
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerOne, err := harness.client.AddManager(context.Background(), taskID, testProvider, "One")
	if err != nil {
		t.Fatal(err)
	}
	managerTwo, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Two")
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- harness.supervisor.Close() }()
	seen := make(map[session.ID]bool)
	for len(seen) != 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("Close did not enter every Stop concurrently")
		}
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !seen[managerOne] || !seen[managerTwo] {
		t.Fatalf("stopped Managers = %#v", seen)
	}
	storedOne, _ := harness.store.GetSession(context.Background(), managerOne)
	storedTwo, _ := harness.store.GetSession(context.Background(), managerTwo)
	if storedOne.State != session.StateInterrupted || storedTwo.State != session.StateInterrupted {
		t.Fatalf("Close states = %s, %s", storedOne.State, storedTwo.State)
	}
}

func TestPromptAndFrameCanariesNeverReachMetadataFiles(t *testing.T) {
	ids := &queuedIDs{tasks: []task.ID{"task-canary"}, sessions: []session.ID{"manager-canary"}}
	backend := newFakeBackend()
	backend.startFn = func(request manager.StartRequest) (managerHandle, error) {
		handle := newFakeHandle(request.Manager.ID, []byte("canary-ref"))
		handle.sendFn = func(ctx context.Context, prompt string, sink manager.FrameSink) (json.RawMessage, error) {
			if prompt != "prompt-storage-canary" {
				return nil, errors.New("prompt changed")
			}
			frame := json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"frame-storage-canary"}}}}`)
			if err := sink.PublicFrame(ctx, request.Manager.ID, frame); err != nil {
				return nil, err
			}
			return json.RawMessage(`{"completion":"completion-storage-canary"}`), nil
		}
		backend.mu.Lock()
		backend.active[request.Manager.ID] = handle
		backend.mu.Unlock()
		return handle, nil
	}
	harness := newRuntimeHarness(t, nil, backend, ids)
	drainEvents(t, harness.client)
	taskID, _ := harness.client.AddTask(context.Background(), "Task")
	_ = harness.client.Attach(context.Background(), taskID)
	managerID, err := harness.client.AddManager(context.Background(), taskID, testProvider, "Manager")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.client.Send(context.Background(), managerID, "prompt-storage-canary"); err != nil {
		t.Fatal(err)
	}
	stateDir := harness.store.StateDir()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	canaries := [][]byte{[]byte("prompt-storage-canary"), []byte("frame-storage-canary"), []byte("completion-storage-canary")}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, canary := range canaries {
			if bytes.Contains(content, canary) {
				t.Fatalf("content canary found in %s", entry.Name())
			}
		}
	}
}

// Keep the canonical opaque type visibly exercised in this package: runtime
// never decodes it and forwards only its byte envelope through manager APIs.
var _ = replay.OpaqueRef{}
