package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestServiceConstructionAndListingAreLazy(t *testing.T) {
	ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("unused")}}
	children := &fakeChildren{}
	auth := &fakeAuth{}
	registry := newRegistry(t, fakeProfile{
		id: "manager", ports: ports, children: children, auth: auth, manager: true, resume: true,
	})

	service, err := NewService(registry)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if ids := service.ActiveIDs(); len(ids) != 0 {
		t.Fatalf("ActiveIDs = %v, want empty", ids)
	}
	if instance, ok := service.Lookup("manager-one"); ok || instance != nil {
		t.Fatalf("Lookup unexpectedly returned %#v, %v", instance, ok)
	}
	_ = registry.Profiles()

	if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
		t.Fatalf("provider contacted during construction/listing: Start=%d Resume=%d", starts, resumes)
	}
	if newCalls, resumeCalls := children.counts(); newCalls != 0 || resumeCalls != 0 {
		t.Fatalf("child provider contacted during construction/listing: New=%d Resume=%d", newCalls, resumeCalls)
	}
	if probes, authenticates := auth.counts(); probes != 0 || authenticates != 0 {
		t.Fatalf("auth contacted during construction/listing: Probe=%d Authenticate=%d", probes, authenticates)
	}
	if _, err := NewService(nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("NewService(nil) error = %v, want %v", err, ErrInvalidRequest)
	}
}

func TestStartPassesExactTransientBoundaryOnce(t *testing.T) {
	connection := newFakeConnection("opaque-manager-start")
	ports := &fakeManagerPorts{connections: []*fakeConnection{connection}}
	children := &fakeChildren{}
	auth := &fakeAuth{}
	registry := newRegistry(t, fakeProfile{
		id: "manager", ports: ports, children: children, auth: auth, manager: true, resume: true,
	})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	manager := managerMetadata(t, "manager-one", "manager", nil)
	events := &fakeEventSink{}
	cwd := "/tmp/Project Space/目录/../kept-byte-exact"

	instance, err := service.Start(context.Background(), StartRequest{
		Manager: manager, ProjectCWD: cwd, ChildEvents: events,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = instance.Close() })

	if starts, resumes := ports.counts(); starts != 1 || resumes != 0 {
		t.Fatalf("factory calls = Start %d Resume %d, want 1/0", starts, resumes)
	}
	request := ports.startRequest(0)
	if request.ParentSessionID != string(manager.ID) || request.CWD != cwd || request.Registry != registry {
		t.Fatalf("Start request changed: parent=%q cwd=%q registry-match=%v", request.ParentSessionID, request.CWD, request.Registry == registry)
	}
	if request.Events != events {
		t.Fatal("runtime-owned ChildEventSink identity was not preserved")
	}
	if request.IDs != nil {
		t.Fatal("manager core unexpectedly injected a connector ID source")
	}
	if instance.ID() != manager.ID || instance.ProviderID() != manager.ProviderID ||
		!bytes.Equal(instance.NativeReference().Bytes(), []byte("opaque-manager-start")) ||
		instance.PublicSessionID() != connection.publicSessionID {
		t.Fatalf("Instance identity/reference changed: id=%q provider=%q ref=%q public=%q", instance.ID(), instance.ProviderID(), instance.NativeReference().Bytes(), instance.PublicSessionID())
	}
	if got := service.ActiveIDs(); !reflect.DeepEqual(got, []session.ID{manager.ID}) {
		t.Fatalf("ActiveIDs = %v, want [%s]", got, manager.ID)
	}
	if lookedUp, ok := service.Lookup(manager.ID); !ok || lookedUp != instance {
		t.Fatal("Lookup did not return the established instance")
	}

	if _, err := service.Start(context.Background(), StartRequest{
		Manager: manager, ProjectCWD: cwd, ChildEvents: events,
	}); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("duplicate Start error = %v, want %v", err, ErrAlreadyActive)
	}
	if starts, _ := ports.counts(); starts != 1 {
		t.Fatalf("duplicate Start reached connector; calls=%d", starts)
	}
	if newCalls, resumeCalls := children.counts(); newCalls != 0 || resumeCalls != 0 {
		t.Fatalf("manager core launched child: New=%d Resume=%d", newCalls, resumeCalls)
	}
	if probes, authenticates := auth.counts(); probes != 0 || authenticates != 0 {
		t.Fatalf("manager Start probed/authenticated: Probe=%d Authenticate=%d", probes, authenticates)
	}
}

func TestStartRejectsInvalidRequestsBeforeConnector(t *testing.T) {
	valid := managerMetadata(t, "manager-one", "manager", nil)
	child := childMetadata(t, "child-one", "manager-one", "manager", []byte("child-ref"))
	withReference := managerMetadata(t, "manager-two", "manager", []byte("existing-ref"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var typedNilEvents *fakeEventSink

	tests := []struct {
		name    string
		ctx     context.Context
		manager session.Session
		cwd     string
		events  connector.ChildEventSink
	}{
		{name: "nil context", manager: valid, cwd: "/tmp/project", events: noOpChildEvents()},
		{name: "cancelled context", ctx: cancelled, manager: valid, cwd: "/tmp/project", events: noOpChildEvents()},
		{name: "nil event sink", ctx: context.Background(), manager: valid, cwd: "/tmp/project"},
		{name: "typed nil event sink", ctx: context.Background(), manager: valid, cwd: "/tmp/project", events: typedNilEvents},
		{name: "subordinate role", ctx: context.Background(), manager: child, cwd: "/tmp/project", events: noOpChildEvents()},
		{name: "existing reference", ctx: context.Background(), manager: withReference, cwd: "/tmp/project", events: noOpChildEvents()},
		{name: "empty cwd", ctx: context.Background(), manager: valid, events: noOpChildEvents()},
		{name: "relative cwd", ctx: context.Background(), manager: valid, cwd: "relative/project", events: noOpChildEvents()},
		{name: "nul cwd", ctx: context.Background(), manager: valid, cwd: "/tmp/project\x00other", events: noOpChildEvents()},
		{name: "invalid utf8 cwd", ctx: context.Background(), manager: valid, cwd: "/tmp/project/\xff", events: noOpChildEvents()},
		{name: "oversized cwd", ctx: context.Background(), manager: valid, cwd: "/" + strings.Repeat("x", MaxProjectCWDBytes), events: noOpChildEvents()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("unused")}}
			registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
			service, err := NewService(registry)
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.Start(test.ctx, StartRequest{Manager: test.manager, ProjectCWD: test.cwd, ChildEvents: test.events})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Start error = %v, want %v", err, ErrInvalidRequest)
			}
			if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
				t.Fatalf("invalid request contacted connector: Start=%d Resume=%d", starts, resumes)
			}
		})
	}
}

func TestStartCapabilitySelectionFailsClosed(t *testing.T) {
	children := &fakeChildren{}
	registry := newRegistry(t, fakeProfile{id: "child-only", children: children, auth: &fakeAuth{}, resume: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	manager := managerMetadata(t, "manager-one", "child-only", nil)
	if _, err := service.Start(context.Background(), StartRequest{
		Manager: manager, ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("child-only Start error = %v, want %v", err, ErrUnsupported)
	}
	manager.ProviderID = "not-registered"
	if _, err := service.Start(context.Background(), StartRequest{
		Manager: manager, ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}); !errors.Is(err, ErrProviderUnknown) {
		t.Fatalf("unknown-provider Start error = %v, want %v", err, ErrProviderUnknown)
	}
	if newCalls, resumeCalls := children.counts(); newCalls != 0 || resumeCalls != 0 {
		t.Fatalf("capability selection contacted provider: New=%d Resume=%d", newCalls, resumeCalls)
	}

	typedNilRegistry := newRegistry(t, fakeProfile{id: "typed-nil-manager", ports: nil, auth: &fakeAuth{}, manager: true})
	typedNilService, _ := NewService(typedNilRegistry)
	typedNilManager := managerMetadata(t, "manager-two", "typed-nil-manager", nil)
	if _, err := typedNilService.Start(context.Background(), StartRequest{
		Manager: typedNilManager, ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("typed-nil manager port error = %v, want %v", err, ErrUnsupported)
	}
}

func TestConcurrentStartReservesOneManagerBeforeLaunch(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	connection := newFakeConnection("manager-ref")
	ports := &fakeManagerPorts{
		connections: []*fakeConnection{connection}, startGate: gate, startEntered: entered,
	}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	request := StartRequest{
		Manager:    managerMetadata(t, "manager-one", "manager", nil),
		ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}
	type result struct {
		instance *Instance
		err      error
	}
	first := make(chan result, 1)
	go func() {
		instance, err := service.Start(context.Background(), request)
		first <- result{instance: instance, err: err}
	}()
	waitClosed(t, entered, "first Start factory entry")
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("concurrent Start error = %v, want %v", err, ErrAlreadyActive)
	}
	close(gate)
	var got result
	select {
	case got = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first Start")
	}
	if got.err != nil || got.instance == nil {
		t.Fatalf("first Start = (%v, %v)", got.instance, got.err)
	}
	t.Cleanup(func() { _ = got.instance.Close() })
	if starts, resumes := ports.counts(); starts != 1 || resumes != 0 {
		t.Fatalf("factory calls = Start %d Resume %d, want 1/0", starts, resumes)
	}
}

func TestStartTimeoutKeepsLateProviderOwnedUntilItCanBeClosed(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	connection := newFakeConnection("manager-ref")
	ports := &fakeManagerPorts{
		connections: []*fakeConnection{connection}, startGate: gate, startEntered: entered,
		startIgnoresContext: true,
	}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	service.startupTimeout = 20 * time.Millisecond
	request := StartRequest{
		Manager:    managerMetadata(t, "manager-one", "manager", nil),
		ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}
	started := time.Now()
	_, err = service.Start(context.Background(), request)
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("silent Start = %v, want transport", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("silent Start exceeded its finite launch deadline")
	}
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("late provider reservation = %v, want already_active", err)
	}
	close(gate)
	waitClosed(t, connection.Done(), "late provider cleanup")
	if got := connection.snapshot(); got.closes != 1 {
		t.Fatalf("late provider Close calls = %d, want 1", got.closes)
	}
}

func TestCancelledStartKeepsReservationUntilLateProviderIsClosed(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	connection := newFakeConnection("manager-ref")
	ports := &fakeManagerPorts{
		connections: []*fakeConnection{connection}, startGate: gate, startEntered: entered,
		startIgnoresContext: true,
	}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	request := StartRequest{
		Manager:    managerMetadata(t, "manager-one", "manager", nil),
		ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, startErr := service.Start(ctx, request)
		result <- startErr
	}()
	waitClosed(t, entered, "cancelled Start factory entry")
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("cancelled Start = %v, want cancelled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Start did not return")
	}
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("abandoned reservation = %v, want already_active", err)
	}
	close(gate)
	waitClosed(t, connection.Done(), "cancelled late provider cleanup")
	deadline := time.Now().Add(time.Second)
	for {
		service.mu.RLock()
		_, reserved := service.active[request.Manager.ID]
		service.mu.RUnlock()
		if !reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late provider cleanup did not release its reservation")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerDoneAndReservationWaitForCompositeOwnedCleanup(t *testing.T) {
	childRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(childRelease) }) }
	defer releaseChild()
	child := &blockingCleanupChild{
		reference:    replay.OpaqueRefFromBytes([]byte("cleanup-child-reference")),
		closeEntered: make(chan struct{}),
		closeRelease: childRelease,
	}
	children := &blockingCleanupChildFactory{child: child}
	app := &cleanupManagerApp{threadID: "cleanup-manager-thread"}
	apps := &cleanupManagerAppFactory{app: app}
	managerPort := connector.CodexManager{Apps: apps}

	registry := connector.NewRegistry()
	childVersions := connector.Versions{ACPProtocol: 1, Connector: "cleanup-child-test"}
	if err := registry.Register(connector.NewConformedProfile(
		"cleanup-child", "Cleanup child", childVersions,
		connector.Conformance{Exact: childVersions, Proven: connector.Capabilities{Subordinate: true}},
		nil, children, nil, nil, nil, connector.ReplayBinding{},
	)); err != nil {
		t.Fatal(err)
	}
	managerVersions := connector.Versions{
		ACPProtocol: 1, ManagerRuntime: connector.CodexAppServerVersion, Connector: "cleanup-manager-test",
	}
	if err := registry.Register(connector.NewConformedProfile(
		"cleanup-manager", "Cleanup manager", managerVersions,
		connector.Conformance{Exact: managerVersions, Proven: connector.Capabilities{Manager: true}},
		nil, nil, nil, managerPort, nil, connector.ReplayBinding{},
	)); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	metadata := managerMetadata(t, "manager-cleanup", "cleanup-manager", nil)
	request := StartRequest{Manager: metadata, ProjectCWD: t.TempDir(), ChildEvents: noOpChildEvents()}
	instance, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	handler := apps.handlerSnapshot()
	delegator, ok := handler.(*connector.Delegator)
	if !ok {
		t.Fatalf("manager handler = %T, want *connector.Delegator", handler)
	}
	if _, err := handler.Invoke(context.Background(), json.RawMessage(
		`{"operation":"start","provider":"cleanup-child","prompt":"opaque","mode":"answer"}`,
	)); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if _, err := handler.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); err != nil {
		t.Fatalf("complete child turn: %v", err)
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- delegator.Close() }()
	waitClosed(t, child.closeEntered, "owned child cleanup entry")

	select {
	case <-delegator.Done():
		t.Fatal("Delegator published Done before its owned child Close completed")
	default:
	}
	select {
	case <-instance.connection.Done():
		t.Fatal("Manager composite published Done before subordinate cleanup completed")
	default:
	}
	select {
	case <-instance.Done():
		t.Fatal("Instance published Done before composite cleanup completed")
	default:
	}
	service.mu.RLock()
	reserved := service.active[metadata.ID]
	service.mu.RUnlock()
	if reserved != instance {
		t.Fatal("Service released the exact Manager reservation during composite cleanup")
	}
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("Start during composite cleanup = %v, want already_active", err)
	}
	if _, err := handler.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); !errors.Is(err, connector.ErrTransport) {
		t.Fatalf("Delegator accepted work during cleanup: %v", err)
	}
	select {
	case err := <-closeResult:
		t.Fatalf("Delegator Close returned before owned child cleanup: %v", err)
	default:
	}

	releaseChild()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Delegator Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delegator Close did not join completed child cleanup")
	}
	waitClosed(t, instance.connection.Done(), "Manager composite cleanup")
	waitClosed(t, instance.Done(), "Instance cleanup terminal")
	if terminal, ok := instance.Terminal(); !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeTransport}) {
		t.Fatalf("cleanup terminal = %#v, %v", terminal, ok)
	}
	service.mu.RLock()
	_, stillReserved := service.active[metadata.ID]
	service.mu.RUnlock()
	if stillReserved {
		t.Fatal("Service retained Manager reservation after exact cleanup completed")
	}
	if got := child.closeCount(); got != 1 {
		t.Fatalf("owned child Close calls = %d, want 1", got)
	}
	if got := app.closeCount(); got != 1 {
		t.Fatalf("App Server Close calls = %d, want 1", got)
	}
}

func TestServiceBoundsActiveManagerReservations(t *testing.T) {
	ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("unused")}}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	for index := range MaxActiveManagers {
		id := session.ID("reserved-" + strconv.Itoa(index))
		if err := service.reserve(id); err != nil {
			t.Fatalf("reserve(%d): %v", index, err)
		}
	}
	_, err = service.Start(context.Background(), StartRequest{
		Manager:    managerMetadata(t, "manager-over-capacity", "manager", nil),
		ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
	})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("Start at active capacity error = %v, want %v", err, ErrCapacity)
	}
	if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
		t.Fatalf("capacity failure contacted connector: Start=%d Resume=%d", starts, resumes)
	}
}

func TestFailedStartReleasesReservationAndRedactsProviderError(t *testing.T) {
	const canary = "provider-secret --token=abc /private/path opaque-ref content"
	failedConnection := newFakeConnection("not-returned")
	goodConnection := newFakeConnection("good-ref")
	ports := &fakeManagerPorts{
		connections: []*fakeConnection{failedConnection, goodConnection}, startErr: errors.New(canary),
	}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	request := StartRequest{
		Manager:    managerMetadata(t, "manager-one", "manager", nil),
		ProjectCWD: "/private/project", ChildEvents: noOpChildEvents(),
	}
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), canary) {
		t.Fatalf("failed Start error = %q, want safe transport code", err)
	}
	if _, ok := service.Lookup(request.Manager.ID); ok {
		t.Fatal("failed Start left an active reservation")
	}
	ports.mu.Lock()
	ports.startErr = nil
	ports.mu.Unlock()
	instance, err := service.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("retry after failed Start: %v", err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	if starts, _ := ports.counts(); starts != 2 {
		t.Fatalf("Start calls = %d, want 2", starts)
	}
}

func TestStartRejectsInvalidNativeConnectionAndReleasesReservation(t *testing.T) {
	emptyPublic := newFakeConnection("connection-ref")
	emptyPublic.publicSessionID = ""
	oversizedPublic := newFakeConnection("connection-ref")
	oversizedPublic.publicSessionID = strings.Repeat("p", connector.MaxCorrelationIDBytes+1)
	nulPublic := newFakeConnection("connection-ref")
	nulPublic.publicSessionID = "manager\x00foreign"
	tests := []struct {
		name       string
		connection *fakeConnection
		nativeRef  []byte
	}{
		{name: "mismatched references", connection: newFakeConnection("connection-ref"), nativeRef: []byte("envelope-ref")},
		{name: "empty reference", connection: newFakeConnection("connection-ref"), nativeRef: []byte{}},
		{
			name: "oversized reference", connection: newFakeConnection("connection-ref"),
			nativeRef: bytes.Repeat([]byte{'x'}, session.MaxProviderReferenceBytes+1),
		},
		{name: "empty public Session identity", connection: emptyPublic},
		{name: "oversized public Session identity", connection: oversizedPublic},
		{name: "NUL public Session identity", connection: nulPublic},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeManagerPorts{connections: []*fakeConnection{test.connection}, startRef: test.nativeRef}
			registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
			service, _ := NewService(registry)
			manager := managerMetadata(t, "manager-one", "manager", nil)
			_, err := service.Start(context.Background(), StartRequest{
				Manager: manager, ProjectCWD: "/tmp/project", ChildEvents: noOpChildEvents(),
			})
			if !errors.Is(err, ErrTransport) {
				t.Fatalf("Start error = %v, want %v", err, ErrTransport)
			}
			if starts, resumes := ports.counts(); starts != 1 || resumes != 0 {
				t.Fatalf("invalid native calls = Start %d Resume %d, want 1/0", starts, resumes)
			}
			if got := test.connection.snapshot(); got.closes != 1 {
				t.Fatalf("invalid native connection Close calls = %d, want 1", got.closes)
			}
			if _, ok := service.Lookup(manager.ID); ok {
				t.Fatal("invalid native connection left reservation active")
			}
		})
	}
}

func TestResumePassesExactManagerAndCompleteOwnedChildrenOnce(t *testing.T) {
	managerReference := []byte{0x00, 0x01, 'm', 0xff, 'r'}
	childAReference := []byte{0x00, 'a', 0xfe}
	childBReference := []byte("opaque child B \x00 bytes")
	connection := newFakeConnection(string(managerReference))
	ports := &fakeManagerPorts{connections: []*fakeConnection{connection}}
	managerChildren := &fakeChildren{}
	childAFactory := &fakeChildren{}
	childBFactory := &fakeChildren{}
	auth := &fakeAuth{}
	registry := newRegistry(t,
		fakeProfile{id: "manager", ports: ports, children: managerChildren, auth: auth, manager: true, resume: true},
		fakeProfile{id: "child-a", children: childAFactory, auth: &fakeAuth{}, resume: true},
		fakeProfile{id: "child-b", children: childBFactory, auth: &fakeAuth{}, resume: true},
	)
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	manager := managerMetadata(t, "manager-one", "manager", managerReference)
	childA := childMetadata(t, "child-a-one", "manager-one", "child-a", childAReference)
	childB := childMetadata(t, "child-b-one", "manager-one", "child-b", childBReference)
	events := &fakeEventSink{}
	request := ResumeRequest{Manager: manager, OwnedChildren: []session.Session{childB, childA}, ChildEvents: events}

	instance, err := service.Resume(context.Background(), request)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	if _, err := service.Resume(context.Background(), request); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("duplicate Resume error = %v, want %v", err, ErrAlreadyActive)
	}
	// Mutating caller-owned metadata after Resume must not alter the captured
	// connector request or the Instance's opaque envelope.
	manager.ProviderReference[0] = 0xaa
	request.OwnedChildren[0].ProviderReference[0] = 0xbb
	request.OwnedChildren[1].ProviderReference[0] = 0xcc

	if starts, resumes := ports.counts(); starts != 0 || resumes != 1 {
		t.Fatalf("factory calls = Start %d Resume %d, want 0/1", starts, resumes)
	}
	captured := ports.resumeRequest(0)
	if captured.parent != "manager-one" || captured.registry != registry || captured.events != events {
		t.Fatalf("Resume structural boundary changed: parent=%q registry-match=%v events-match=%v", captured.parent, captured.registry == registry, captured.events == events)
	}
	if captured.ids != nil {
		t.Fatal("manager core unexpectedly injected a connector ID source during Resume")
	}
	if !bytes.Equal(captured.reference, managerReference) {
		t.Fatalf("manager reference changed: %x want %x", captured.reference, managerReference)
	}
	wantChildren := []recordedChild{
		{id: "child-b-one", provider: "child-b", reference: childBReference},
		{id: "child-a-one", provider: "child-a", reference: childAReference},
	}
	if !reflect.DeepEqual(captured.children, wantChildren) {
		t.Fatalf("owned children changed:\n got %#v\nwant %#v", captured.children, wantChildren)
	}
	if !bytes.Equal(instance.NativeReference().Bytes(), managerReference) {
		t.Fatalf("Instance reference = %x, want %x", instance.NativeReference().Bytes(), managerReference)
	}
	for name, factory := range map[string]*fakeChildren{
		"manager": managerChildren, "child-a": childAFactory, "child-b": childBFactory,
	} {
		if newCalls, resumeCalls := factory.counts(); newCalls != 0 || resumeCalls != 0 {
			t.Fatalf("%s child port contacted during restore validation: New=%d Resume=%d", name, newCalls, resumeCalls)
		}
	}
	if probes, authenticates := auth.counts(); probes != 0 || authenticates != 0 {
		t.Fatalf("Resume probed/authenticated: Probe=%d Authenticate=%d", probes, authenticates)
	}
}

func TestResumeRejectsInvalidRequestsBeforeConnector(t *testing.T) {
	valid := managerMetadata(t, "manager-one", "manager", []byte("manager-ref"))
	withoutReference := managerMetadata(t, "manager-two", "manager", nil)
	child := childMetadata(t, "child-one", "manager-one", "manager", []byte("child-ref"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var typedNilEvents *fakeEventSink
	tests := []struct {
		name    string
		ctx     context.Context
		manager session.Session
		events  connector.ChildEventSink
	}{
		{name: "nil context", manager: valid, events: noOpChildEvents()},
		{name: "cancelled context", ctx: cancelled, manager: valid, events: noOpChildEvents()},
		{name: "nil event sink", ctx: context.Background(), manager: valid},
		{name: "typed nil event sink", ctx: context.Background(), manager: valid, events: typedNilEvents},
		{name: "missing manager reference", ctx: context.Background(), manager: withoutReference, events: noOpChildEvents()},
		{name: "subordinate role", ctx: context.Background(), manager: child, events: noOpChildEvents()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("manager-ref")}}
			registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true})
			service, _ := NewService(registry)
			_, err := service.Resume(test.ctx, ResumeRequest{Manager: test.manager, ChildEvents: test.events})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Resume error = %v, want %v", err, ErrInvalidRequest)
			}
			if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
				t.Fatalf("invalid Resume contacted connector: Start=%d Resume=%d", starts, resumes)
			}
		})
	}
}

func TestResumeNeverFallsBackToStart(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("manager-ref")}}
		registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true})
		service, _ := NewService(registry)
		manager := managerMetadata(t, "manager-one", "manager", []byte("manager-ref"))
		if _, err := service.Resume(context.Background(), ResumeRequest{
			Manager: manager, ChildEvents: noOpChildEvents(),
		}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Resume error = %v, want %v", err, ErrUnsupported)
		}
		if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
			t.Fatalf("unsupported Resume contacted factory: Start=%d Resume=%d", starts, resumes)
		}
	})

	t.Run("provider failure", func(t *testing.T) {
		const canary = "resume --secret=credential /native/path context-content"
		ports := &fakeManagerPorts{
			connections: []*fakeConnection{newFakeConnection("manager-ref")}, resumeErr: errors.New(canary),
		}
		registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true})
		service, _ := NewService(registry)
		manager := managerMetadata(t, "manager-one", "manager", []byte("manager-ref"))
		_, err := service.Resume(context.Background(), ResumeRequest{Manager: manager, ChildEvents: noOpChildEvents()})
		if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), canary) {
			t.Fatalf("Resume error = %q, want redacted transport code", err)
		}
		if starts, resumes := ports.counts(); starts != 0 || resumes != 1 {
			t.Fatalf("failed Resume calls = Start %d Resume %d, want 0/1", starts, resumes)
		}
		if _, ok := service.Lookup(manager.ID); ok {
			t.Fatal("failed Resume left Manager active")
		}
	})

	t.Run("different returned native session", func(t *testing.T) {
		connection := newFakeConnection("replacement-ref")
		ports := &fakeManagerPorts{connections: []*fakeConnection{connection}}
		registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true})
		service, _ := NewService(registry)
		manager := managerMetadata(t, "manager-one", "manager", []byte("original-ref"))
		if _, err := service.Resume(context.Background(), ResumeRequest{
			Manager: manager, ChildEvents: noOpChildEvents(),
		}); !errors.Is(err, ErrTransport) {
			t.Fatalf("replacement Resume error = %v, want %v", err, ErrTransport)
		}
		if starts, resumes := ports.counts(); starts != 0 || resumes != 1 {
			t.Fatalf("replacement Resume calls = Start %d Resume %d, want 0/1", starts, resumes)
		}
		if got := connection.snapshot(); got.closes != 1 {
			t.Fatalf("rejected replacement Close calls = %d, want 1", got.closes)
		}
	})
}

func TestResumeValidatesCompleteSameTaskOwnedChildSetBeforeProvider(t *testing.T) {
	tests := []struct {
		name string
		make func(*testing.T, session.Session) []session.Session
		want error
	}{
		{
			name: "cross task",
			make: func(t *testing.T, manager session.Session) []session.Session {
				child := childMetadata(t, "child-one", string(manager.ID), "child", []byte("child-ref"))
				child.TaskID = task.ID("task-other")
				return []session.Session{child}
			},
			want: ErrInvalidRequest,
		},
		{
			name: "wrong parent",
			make: func(t *testing.T, _ session.Session) []session.Session {
				return []session.Session{childMetadata(t, "child-one", "manager-peer", "child", []byte("child-ref"))}
			},
			want: ErrInvalidRequest,
		},
		{
			name: "empty reference",
			make: func(t *testing.T, manager session.Session) []session.Session {
				return []session.Session{childMetadata(t, "child-one", string(manager.ID), "child", nil)}
			},
			want: ErrInvalidRequest,
		},
		{
			name: "duplicate id",
			make: func(t *testing.T, manager session.Session) []session.Session {
				child := childMetadata(t, "child-one", string(manager.ID), "child", []byte("child-ref"))
				return []session.Session{child, child}
			},
			want: ErrInvalidRequest,
		},
		{
			name: "duplicate native reference",
			make: func(t *testing.T, manager session.Session) []session.Session {
				return []session.Session{
					childMetadata(t, "child-one", string(manager.ID), "child", []byte("same-ref")),
					childMetadata(t, "child-two", string(manager.ID), "child", []byte("same-ref")),
				}
			},
			want: ErrInvalidRequest,
		},
		{
			name: "unknown child provider",
			make: func(t *testing.T, manager session.Session) []session.Session {
				return []session.Session{childMetadata(t, "child-one", string(manager.ID), "missing", []byte("child-ref"))}
			},
			want: ErrProviderUnknown,
		},
		{
			name: "provider lacks subordinate port",
			make: func(t *testing.T, manager session.Session) []session.Session {
				return []session.Session{childMetadata(t, "child-one", string(manager.ID), "manager", []byte("child-ref"))}
			},
			want: ErrUnsupported,
		},
		{
			name: "over capacity",
			make: func(_ *testing.T, _ session.Session) []session.Session {
				return make([]session.Session, connector.MaxChildrenPerManager+1)
			},
			want: ErrCapacity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeManagerPorts{connections: []*fakeConnection{newFakeConnection("manager-ref")}}
			registry := newRegistry(t,
				fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true},
				fakeProfile{id: "child", children: &fakeChildren{}, auth: &fakeAuth{}, resume: true},
			)
			service, _ := NewService(registry)
			manager := managerMetadata(t, "manager-one", "manager", []byte("manager-ref"))
			_, err := service.Resume(context.Background(), ResumeRequest{
				Manager: manager, OwnedChildren: test.make(t, manager), ChildEvents: noOpChildEvents(),
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("Resume error = %v, want %v", err, test.want)
			}
			if starts, resumes := ports.counts(); starts != 0 || resumes != 0 {
				t.Fatalf("invalid child set contacted provider: Start=%d Resume=%d", starts, resumes)
			}
		})
	}
}

func TestStartAndResumeAPIsContainNoPrefixOrReinjectionSurface(t *testing.T) {
	assertFields := func(name string, typ reflect.Type, want []string) {
		t.Helper()
		got := make([]string, typ.NumField())
		for index := range typ.NumField() {
			got[index] = typ.Field(index).Name
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields = %v, want exact %v", name, got, want)
		}
	}
	assertFields("manager.StartRequest", reflect.TypeOf(StartRequest{}), []string{
		"Manager", "ProjectCWD", "ChildEvents",
	})
	assertFields("connector.ManagerStartRequest", reflect.TypeOf(connector.ManagerStartRequest{}), []string{
		"ParentSessionID", "CWD", "Registry", "Events", "IDs",
	})
	assertFields("manager.ResumeRequest", reflect.TypeOf(ResumeRequest{}), []string{
		"Manager", "OwnedChildren", "ChildEvents",
	})
	assertFields("connector.ManagerResumeRequest", reflect.TypeOf(connector.ManagerResumeRequest{}), []string{
		"ParentSessionID", "NativeReference", "OwnedChildren", "Registry", "Events", "IDs",
	})

	for _, typ := range []reflect.Type{reflect.TypeOf(ResumeRequest{}), reflect.TypeOf(connector.ManagerResumeRequest{})} {
		for index := range typ.NumField() {
			name := strings.ToLower(typ.Field(index).Name)
			for _, forbidden := range []string{"cwd", "prefix", "context", "prompt", "answer", "tool", "history", "path"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s unexpectedly exposes reinjection field %q", typ, typ.Field(index).Name)
				}
			}
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(StartRequest{}), reflect.TypeOf(connector.ManagerStartRequest{})} {
		for index := range typ.NumField() {
			name := strings.ToLower(typ.Field(index).Name)
			for _, forbidden := range []string{"prefix", "context", "prompt", "answer", "tool", "history"} {
				if strings.Contains(name, forbidden) {
					t.Fatalf("%s unexpectedly exposes prefix/content field %q", typ, typ.Field(index).Name)
				}
			}
		}
	}
}

type blockingCleanupChildFactory struct {
	mu    sync.Mutex
	child *blockingCleanupChild
	calls int
}

func (factory *blockingCleanupChildFactory) New(context.Context) (connector.ChildPort, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.calls != 0 || factory.child == nil {
		return nil, connector.ErrTransport
	}
	factory.calls++
	return factory.child, nil
}

type blockingCleanupChild struct {
	reference    replay.OpaqueRef
	closeEntered chan struct{}
	closeRelease <-chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func (child *blockingCleanupChild) NativeReference() replay.OpaqueRef {
	return replay.OpaqueRefFromBytes(child.reference.Bytes())
}

func (*blockingCleanupChild) Prompt(
	context.Context,
	string,
	func(json.RawMessage) error,
) (json.RawMessage, error) {
	return json.RawMessage(`{"stopReason":"end_turn"}`), nil
}

func (*blockingCleanupChild) Cancel(context.Context) error { return nil }

func (child *blockingCleanupChild) Close() error {
	child.closeOnce.Do(func() {
		child.mu.Lock()
		child.closes++
		child.mu.Unlock()
		close(child.closeEntered)
		<-child.closeRelease
	})
	return nil
}

func (child *blockingCleanupChild) closeCount() int {
	child.mu.Lock()
	defer child.mu.Unlock()
	return child.closes
}

type cleanupManagerAppFactory struct {
	mu      sync.Mutex
	app     *cleanupManagerApp
	handler connector.DynamicToolHandler
	calls   int
}

func (factory *cleanupManagerAppFactory) New(
	_ context.Context,
	handler connector.DynamicToolHandler,
) (connector.CodexAppServer, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.calls != 0 || factory.app == nil || handler == nil {
		return nil, connector.ErrTransport
	}
	factory.calls++
	factory.handler = handler
	return factory.app, nil
}

func (factory *cleanupManagerAppFactory) handlerSnapshot() connector.DynamicToolHandler {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.handler
}

type cleanupManagerApp struct {
	threadID string

	mu         sync.Mutex
	connection *fakeConnection
	closes     int
}

func (*cleanupManagerApp) Version() string { return connector.CodexAppServerVersion }

func (*cleanupManagerApp) DelegateHandlerRegistered() bool { return true }

func (app *cleanupManagerApp) ThreadStart(
	context.Context,
	connector.ThreadStartParams,
) (connector.CodexThreadSession, error) {
	return connector.CodexThreadSession{ThreadID: app.threadID}, nil
}

func (app *cleanupManagerApp) ThreadResume(
	context.Context,
	connector.ThreadResumeParams,
) (connector.CodexThreadSession, error) {
	return connector.CodexThreadSession{}, connector.ErrProviderUnsupported
}

func (app *cleanupManagerApp) ConnectManager(reference replay.OpaqueRef) (connector.ManagerConnection, error) {
	want, err := replay.NewCodexReference(app.threadID)
	if err != nil || !bytes.Equal(reference.Bytes(), want.Bytes()) {
		return nil, connector.ErrOwnership
	}
	connection := newFakeConnection(string(reference.Bytes()))
	app.mu.Lock()
	app.connection = connection
	app.mu.Unlock()
	return connection, nil
}

func (app *cleanupManagerApp) Close() error {
	app.mu.Lock()
	app.closes++
	app.mu.Unlock()
	return nil
}

func (app *cleanupManagerApp) closeCount() int {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.closes
}
