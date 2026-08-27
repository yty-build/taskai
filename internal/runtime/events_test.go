package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

func eventTestSupervisor(t *testing.T) (*Supervisor, ClientPort, session.Session) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	value := &Supervisor{
		ctx: ctx, cancel: cancel, generation: 1,
		tasks: make(map[task.ID]task.Task), sessions: make(map[session.ID]session.Session),
		modes: make(map[session.ID]connector.Mode), turns: make(map[session.ID]turnCorrelation),
		catalog: make(map[settings.ProviderID]providerCatalog), viewers: make(map[string]*viewer),
	}
	created := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	managerSession, err := session.NewManager(
		"manager-event", "task-event", "Manager", "provider", nil, session.StateStarting, created,
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	value.sessions[managerSession.ID] = managerSession
	client, err := value.OpenClient("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("OpenClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return value, client, managerSession
}

func TestEventBeforeNextDoesNotHoldStateLock(t *testing.T) {
	supervisor, client, managerSession := eventTestSupervisor(t)
	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareSessionLocked(managerSession)
	}); err != nil {
		t.Fatalf("publish before Next: %v", err)
	}

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.ThroughSequence != 1 {
		t.Fatalf("ThroughSequence = %d, want 1", snapshot.ThroughSequence)
	}
	// Snapshot supersedes the pending event. A second publication proves the
	// stale pending delivery is drained without a cursor gap.
	published := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			close(published)
			return supervisor.prepareSessionLocked(managerSession)
		})
	}()
	<-published

	event, err := client.Next(context.Background(), Cursor{Generation: 1, After: 0})
	if err != nil {
		t.Fatalf("Next first: %v", err)
	}
	if event.Sequence != 1 || event.Kind != EventSessionUpsert {
		t.Fatalf("first event = %#v", event)
	}
	if err := <-done; err != nil {
		t.Fatalf("second publication: %v", err)
	}
}

func TestViewerHasOneOutstandingDeliveryAndOrderedBackpressure(t *testing.T) {
	supervisor, client, managerSession := eventTestSupervisor(t)
	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareSessionLocked(managerSession)
	}); err != nil {
		t.Fatalf("first publication: %v", err)
	}

	prepared := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			close(prepared)
			return supervisor.prepareSessionLocked(managerSession)
		})
	}()
	<-prepared
	select {
	case err := <-secondDone:
		t.Fatalf("second delivery bypassed backpressure: %v", err)
	default:
	}

	first, err := client.Next(context.Background(), Cursor{Generation: 1, After: 0})
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first Next = %#v, %v", first, err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	second, err := client.Next(context.Background(), Cursor{Generation: 1, After: 1})
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second Next = %#v, %v", second, err)
	}
}

type blockingStopHandle struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (handle *blockingStopHandle) ID() session.ID          { return "manager-stop" }
func (handle *blockingStopHandle) ProviderID() string      { return "provider" }
func (handle *blockingStopHandle) NativeReference() []byte { return []byte("reference") }
func (handle *blockingStopHandle) Done() <-chan struct{}   { return make(chan struct{}) }
func (handle *blockingStopHandle) Terminal() (manager.Terminal, bool) {
	return manager.Terminal{}, false
}
func (handle *blockingStopHandle) Available() bool { return true }
func (handle *blockingStopHandle) Send(context.Context, string, manager.FrameSink) (json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (handle *blockingStopHandle) Stop(context.Context) error {
	handle.once.Do(func() { close(handle.entered) })
	<-handle.release
	return nil
}

func TestStopHandleIsMechanicallyBounded(t *testing.T) {
	handle := &blockingStopHandle{entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- stopHandleWithin(handle, 20*time.Millisecond) }()
	<-handle.entered
	err := <-result
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("stop timeout = %v, want transport code", err)
	}
	close(handle.release)
}

type hostileSafeError string

func (value hostileSafeError) Error() string    { return string(value) }
func (value hostileSafeError) SafeCode() string { return string(value) }

func TestUnknownLowerLayerSafeCodeCollapsesToTransport(t *testing.T) {
	err := hostileSafeError("prompt-content-canary")
	if codeOf(err) != CodeTransport || !errors.Is(codeError(Code(err)), ErrTransport) {
		t.Fatalf("unknown code escaped: code=%q error=%v", codeOf(err), codeError(Code(err)))
	}
}
