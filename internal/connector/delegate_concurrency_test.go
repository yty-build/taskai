package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/replay"
)

func TestConcurrentIdleDeathResumeCannotInstallNilTurnPort(t *testing.T) {
	reference, err := replay.NewCodexReference("native-idle-race")
	if err != nil {
		t.Fatal(err)
	}
	initial := newIdleRacePort(reference, nil)
	initial.available.Store(true)
	resumedRelease := make(chan struct{})
	resumed := newIdleRacePort(reference, resumedRelease)
	resumed.available.Store(true)
	factory := &idleRaceFactory{initial: initial, resumed: resumed, reference: reference}

	registry := NewRegistry()
	versions := Versions{ACPProtocol: 1, Connector: "idle-race-fixture"}
	profile := NewConformedProfile(
		"idle-race", "Idle race ACP", versions,
		Conformance{Exact: versions, Proven: Capabilities{Subordinate: true, Resume: true}},
		nil, factory, factory, nil, nil, ReplayBinding{},
	)
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	delegator, err := NewDelegator(context.Background(), "manager", registry, ChildEventSinkFunc(
		func(context.Context, ChildEvent) error { return nil },
	), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delegator.Close() })

	first := invokeIdleRace(t, delegator, `{"operation":"start","provider":"idle-race","prompt":"first","mode":"answer"}`)
	if _, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); err != nil {
		t.Fatal(err)
	}

	claimReached := make(chan struct{})
	releaseFirstClaim := make(chan struct{})
	var claims atomic.Int64
	delegator.beforeTurnClaim = func() {
		if claims.Add(1) == 1 {
			close(claimReached)
			<-releaseFirstClaim
		}
	}

	startInput := fmt.Sprintf(
		`{"operation":"start","child_session_id":%q,"prompt":"next","mode":"answer"}`,
		first.ChildSessionID,
	)
	firstStartErr := make(chan error, 1)
	go func() {
		_, invokeErr := delegator.Invoke(context.Background(), json.RawMessage(startInput))
		firstStartErr <- invokeErr
	}()
	select {
	case <-claimReached:
	case <-time.After(3 * time.Second):
		t.Fatal("first invocation did not pause after resolving the idle port")
	}

	// The exact old transport dies after the first caller resolves it but before
	// that caller can claim child.active. A second caller detects the idle death,
	// closes the stale port, exact-resumes, and claims the replacement.
	initial.available.Store(false)
	secondStart := invokeIdleRace(t, delegator, startInput)
	if secondStart.ChildSessionID != first.ChildSessionID {
		t.Fatalf("resumed child = %q, want %q", secondStart.ChildSessionID, first.ChildSessionID)
	}
	close(releaseFirstClaim)
	select {
	case invokeErr := <-firstStartErr:
		if !errors.Is(invokeErr, ErrChildBusy) {
			t.Fatalf("losing concurrent start error = %v, want ErrChildBusy", invokeErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("losing concurrent start remained blocked")
	}

	close(resumedRelease)
	if _, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); err != nil {
		t.Fatal(err)
	}
	newCalls, resumeCalls := factory.snapshot()
	if newCalls != 1 || resumeCalls != 1 {
		t.Fatalf("lifecycle calls New=%d Resume=%d, want 1/1", newCalls, resumeCalls)
	}
}

type idleRaceWireResult struct {
	ChildSessionID string `json:"child_session_id"`
}

func invokeIdleRace(t *testing.T, delegator *Delegator, input string) idleRaceWireResult {
	t.Helper()
	raw, err := delegator.Invoke(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Invoke(%s): %v", input, err)
	}
	var result idleRaceWireResult
	if err := json.Unmarshal(raw, &result); err != nil || result.ChildSessionID == "" {
		t.Fatalf("decode result %s: %v", raw, err)
	}
	return result
}

type idleRaceFactory struct {
	mu        sync.Mutex
	initial   *idleRacePort
	resumed   *idleRacePort
	reference replay.OpaqueRef
	newCalls  int
	resumes   int
}

func (factory *idleRaceFactory) New(context.Context) (ChildPort, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	factory.newCalls++
	return factory.initial, nil
}

func (factory *idleRaceFactory) Resume(_ context.Context, reference replay.OpaqueRef) (ChildPort, error) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if !bytes.Equal(reference.Bytes(), factory.reference.Bytes()) {
		return nil, ErrOwnership
	}
	factory.resumes++
	return factory.resumed, nil
}

func (factory *idleRaceFactory) snapshot() (int, int) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return factory.newCalls, factory.resumes
}

type idleRacePort struct {
	reference replay.OpaqueRef
	release   <-chan struct{}
	available atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
}

func newIdleRacePort(reference replay.OpaqueRef, release <-chan struct{}) *idleRacePort {
	return &idleRacePort{
		reference: replay.OpaqueRefFromBytes(reference.Bytes()),
		release:   release,
		closed:    make(chan struct{}),
	}
}

func (port *idleRacePort) NativeReference() replay.OpaqueRef {
	return replay.OpaqueRefFromBytes(port.reference.Bytes())
}

func (port *idleRacePort) Available() bool { return port.available.Load() }

func (port *idleRacePort) Prompt(ctx context.Context, _ string, _ func(json.RawMessage) error) (json.RawMessage, error) {
	if port.release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-port.closed:
			return nil, ErrTransport
		case <-port.release:
		}
	}
	return json.RawMessage(`{"stopReason":"end_turn"}`), nil
}

func (port *idleRacePort) Cancel(context.Context) error { return nil }

func (port *idleRacePort) Close() error {
	port.closeOnce.Do(func() {
		port.available.Store(false)
		close(port.closed)
	})
	return nil
}
