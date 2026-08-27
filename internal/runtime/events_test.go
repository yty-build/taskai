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

func TestDecodePublicFrameRequiresExactExpectedManagerSession(t *testing.T) {
	raw := json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"manager_public_owned","update":{"sessionUpdate":"agent_message_chunk","messageId":"message","content":{"type":"text","text":"public"}}}}`)
	frame, err := decodePublicFrame(raw, "manager_public_owned")
	if err != nil || frame.Kind != FrameText || string(frame.Payload) != "public" {
		t.Fatalf("owned frame = %#v, %v", frame, err)
	}
	for _, expected := range []string{"manager_public_foreign", "", string(make([]byte, connector.MaxCorrelationIDBytes+1))} {
		if _, err := decodePublicFrame(raw, expected); !errors.Is(err, ErrTransport) {
			t.Fatalf("expected Session %q decode error = %v, want transport", expected, err)
		}
	}
}

const eventTestClientID = "0123456789abcdef0123456789abcdef"

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
	client, err := value.OpenClient(eventTestClientID)
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

func TestSelectedViewerHasOneOutstandingDeliveryAndOrderedBackpressure(t *testing.T) {
	supervisor, client, managerSession := eventTestSupervisor(t)
	supervisor.mu.Lock()
	supervisor.viewers[eventTestClientID].attached[managerSession.TaskID] = struct{}{}
	supervisor.mu.Unlock()
	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("one")})
	}); err != nil {
		t.Fatalf("first publication: %v", err)
	}

	prepared := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			close(prepared)
			return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("two")})
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

func TestSlowViewerDoesNotBlockOtherManagersOrMetadataAndCanResync(t *testing.T) {
	supervisor, slow, managerSession := eventTestSupervisor(t)
	const fastClientID = "11111111111111111111111111111111"
	fast, err := supervisor.OpenClient(fastClientID)
	if err != nil {
		t.Fatalf("OpenClient fast: %v", err)
	}
	t.Cleanup(func() { _ = fast.Close() })

	otherManager, err := session.NewManager(
		"manager-other", "task-other", "Other", "provider", nil,
		session.StateStarting, managerSession.CreatedAt,
	)
	if err != nil {
		t.Fatalf("NewManager other: %v", err)
	}
	supervisor.mu.Lock()
	supervisor.sessions[otherManager.ID] = otherManager
	supervisor.viewers[eventTestClientID].attached[managerSession.TaskID] = struct{}{}
	supervisor.viewers[fastClientID].attached[otherManager.TaskID] = struct{}{}
	supervisor.mu.Unlock()

	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("slow-one")})
	}); err != nil {
		t.Fatalf("first slow publication: %v", err)
	}
	fastCursor := Cursor{Generation: 1, After: 0}
	firstFast, err := fast.Next(context.Background(), fastCursor)
	if err != nil || firstFast.Sequence != 1 || firstFast.Kind != EventSessionUpsert {
		t.Fatalf("fast first placeholder = %#v, %v", firstFast, err)
	}
	fastCursor.After = firstFast.Sequence

	prepared := make(chan struct{})
	slowFrameDone := make(chan error, 1)
	go func() {
		slowFrameDone <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			close(prepared)
			return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("slow-two")})
		})
	}()
	<-prepared
	select {
	case err := <-slowFrameDone:
		t.Fatalf("selected slow frame did not apply backpressure: %v", err)
	default:
	}
	secondFast, err := fast.Next(context.Background(), fastCursor)
	if err != nil || secondFast.Sequence != 2 || secondFast.Kind != EventSessionUpsert {
		t.Fatalf("fast second placeholder = %#v, %v", secondFast, err)
	}
	fastCursor.After = secondFast.Sequence

	otherDone := make(chan error, 1)
	go func() {
		otherDone <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			return supervisor.prepareFrameLocked(otherManager, PublicFrame{Kind: FrameText, Payload: []byte("fast")})
		})
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("unrelated manager publication: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow viewer blocked an unrelated selected manager")
	}
	thirdFast, err := fast.Next(context.Background(), fastCursor)
	if err != nil || thirdFast.Sequence != 3 || thirdFast.Kind != EventPublicFrame {
		t.Fatalf("fast selected frame = %#v, %v", thirdFast, err)
	}
	fastCursor.After = thirdFast.Sequence

	// The unrelated placeholder already occupies one bounded backlog slot for
	// the slow viewer. Fill the remainder and cross the bound; every metadata
	// mutation and ordered fast-viewer delivery must continue meanwhile.
	for index := 0; index < viewerDeliveryBacklog; index++ {
		mutationDone := make(chan error, 1)
		go func() {
			mutationDone <- supervisor.commitMutation(func() ([]eventDelivery, error) {
				return supervisor.prepareSessionLocked(otherManager)
			})
		}()
		select {
		case err := <-mutationDone:
			if err != nil {
				t.Fatalf("metadata publication %d: %v", index, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("slow viewer blocked metadata publication %d", index)
		}
		event, err := fast.Next(context.Background(), fastCursor)
		if err != nil || event.Sequence != fastCursor.After+1 || event.Kind != EventSessionUpsert {
			t.Fatalf("fast metadata %d = %#v, %v", index, event, err)
		}
		fastCursor.After = event.Sequence
	}
	select {
	case err := <-slowFrameDone:
		if err != nil {
			t.Fatalf("slow frame after bounded detach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded slow-viewer detach did not release selected producer")
	}
	if _, err := slow.Snapshot(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("slow viewer Snapshot after detach = %v, want closed", err)
	}

	replacement, err := supervisor.OpenClient(eventTestClientID)
	if err != nil {
		t.Fatalf("OpenClient replacement: %v", err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	snapshot, err := replacement.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("replacement Snapshot: %v", err)
	}
	if snapshot.ThroughSequence != fastCursor.After {
		t.Fatalf("replacement ThroughSequence = %d, want %d", snapshot.ThroughSequence, fastCursor.After)
	}
	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareSessionLocked(otherManager)
	}); err != nil {
		t.Fatalf("post-resync publication: %v", err)
	}
	replacementEvent, err := replacement.Next(context.Background(), Cursor{
		Generation: snapshot.StreamGeneration, After: snapshot.ThroughSequence,
	})
	if err != nil || replacementEvent.Sequence != snapshot.ThroughSequence+1 {
		t.Fatalf("replacement Next = %#v, %v", replacementEvent, err)
	}
}

func TestPersistentlyStalledSelectedViewerIsDetached(t *testing.T) {
	supervisor, client, managerSession := eventTestSupervisor(t)
	supervisor.mu.Lock()
	value := supervisor.viewers[eventTestClientID]
	value.attached[managerSession.TaskID] = struct{}{}
	value.deliveryTimeout = 20 * time.Millisecond
	supervisor.mu.Unlock()
	if err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("one")})
	}); err != nil {
		t.Fatalf("first publication: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		result <- supervisor.commitMutation(func() ([]eventDelivery, error) {
			return supervisor.prepareFrameLocked(managerSession, PublicFrame{Kind: FrameText, Payload: []byte("two")})
		})
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("stalled publication: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("persistently stalled viewer was not detached")
	}
	if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot after stalled detach = %v, want closed", err)
	}
}

type blockingStopHandle struct {
	entered   chan struct{}
	release   chan struct{}
	done      chan struct{}
	enterOnce sync.Once
	doneOnce  sync.Once
}

func (handle *blockingStopHandle) ID() session.ID          { return "manager-stop" }
func (handle *blockingStopHandle) ProviderID() string      { return "provider" }
func (handle *blockingStopHandle) NativeReference() []byte { return []byte("reference") }
func (handle *blockingStopHandle) PublicSessionID() string { return "manager_public_fixture" }
func (handle *blockingStopHandle) Done() <-chan struct{}   { return handle.done }
func (handle *blockingStopHandle) Terminal() (manager.Terminal, bool) {
	return manager.Terminal{}, false
}
func (handle *blockingStopHandle) Available() bool { return true }
func (handle *blockingStopHandle) Send(context.Context, string, manager.FrameSink) (json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (handle *blockingStopHandle) Stop(context.Context) error {
	handle.enterOnce.Do(func() { close(handle.entered) })
	<-handle.release
	handle.doneOnce.Do(func() { close(handle.done) })
	return nil
}
func (handle *blockingStopHandle) ForceClose() error {
	handle.doneOnce.Do(func() { close(handle.done) })
	return nil
}

func TestStopHandleIsMechanicallyBounded(t *testing.T) {
	handle := &blockingStopHandle{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- stopHandleWithin(handle, 20*time.Millisecond) }()
	<-handle.entered
	err := <-result
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("stop timeout = %v, want transport code", err)
	}
	select {
	case <-handle.Done():
	default:
		t.Fatal("stop timeout returned before forced-close ownership acknowledgement")
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
