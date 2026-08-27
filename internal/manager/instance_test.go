package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/session"
)

func startTestManager(
	t *testing.T,
	lifetime context.Context,
	id string,
	connection *fakeConnection,
) (*Service, *Instance, *fakeManagerPorts) {
	t.Helper()
	ports := &fakeManagerPorts{connections: []*fakeConnection{connection}}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := service.Start(lifetime, StartRequest{
		Manager: managerMetadata(t, id, "manager", nil), ProjectCWD: "/tmp/project",
		ChildEvents: noOpChildEvents(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return service, instance, ports
}

func discardFrame(context.Context, session.ID, json.RawMessage) error { return nil }

func TestNilInstanceMethodsAreSafeAndPerformNoWork(t *testing.T) {
	var instance *Instance
	if instance.ID() != "" || instance.ProviderID() != "" || len(instance.NativeReference().Bytes()) != 0 {
		t.Fatal("nil Instance exposed identity")
	}
	waitClosed(t, instance.Done(), "nil Instance Done")
	if terminal, ok := instance.Terminal(); !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeInvalidRequest}) {
		t.Fatalf("nil Terminal = %#v, %v", terminal, ok)
	}
	if instance.Available() {
		t.Fatal("nil Instance is available")
	}
	if _, err := instance.Send(context.Background(), "prompt", FrameSinkFunc(discardFrame)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil Send error = %v, want %v", err, ErrInvalidRequest)
	}
	if err := instance.Stop(context.Background()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil Stop error = %v, want %v", err, ErrInvalidRequest)
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestSendPreservesPromptAndOrderedPublicFramesExactly(t *testing.T) {
	prompt := "\n\t Keep  double spaces, emoji 🧭, देवनागरी, and trailing whitespace \t\n"
	frames := []json.RawMessage{
		json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sequence":1,"unknown":{"kind":"α","bytes":"\\u0000"}}}`),
		json.RawMessage(`{"future_shape":[1,{"nested":true}],"sequence":2}`),
		json.RawMessage(`{"sequence":3,"opaque":"final public frame"}`),
	}
	completionSource := json.RawMessage(`{"stopReason":"end_turn","native":{"unknown":7}}`)
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(_ context.Context, gotPrompt string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
		if gotPrompt != prompt {
			t.Errorf("provider prompt = %q, want byte-exact %q", gotPrompt, prompt)
		}
		for _, frame := range frames {
			if err := onFrame(frame); err != nil {
				return nil, err
			}
		}
		return completionSource, nil
	}
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
	t.Cleanup(func() { _ = instance.Close() })

	var gotIDs []session.ID
	var gotFrames []json.RawMessage
	completion, err := instance.Send(context.Background(), prompt, FrameSinkFunc(
		func(_ context.Context, id session.ID, frame json.RawMessage) error {
			gotIDs = append(gotIDs, id)
			gotFrames = append(gotFrames, append(json.RawMessage(nil), frame...))
			return nil
		},
	))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := connection.snapshot(); !reflect.DeepEqual(got.prompts, []string{prompt}) || got.promptCalls != 1 {
		t.Fatalf("provider prompts = %#v (%d calls), want exact one", got.prompts, got.promptCalls)
	}
	if !reflect.DeepEqual(gotFrames, frames) {
		t.Fatalf("frames changed or reordered:\n got %s\nwant %s", gotFrames, frames)
	}
	for index, id := range gotIDs {
		if id != instance.ID() {
			t.Fatalf("frame %d routed to %q, want %q", index, id, instance.ID())
		}
	}
	if !bytes.Equal(completion, completionSource) {
		t.Fatalf("completion = %s, want %s", completion, completionSource)
	}

	// The result and delivered frames are defensive transient copies.
	completionSource[0] = '!'
	frames[0][0] = '!'
	if completion[0] != '{' || gotFrames[0][0] != '{' {
		t.Fatal("caller-visible data aliases connector-owned buffers")
	}
}

func TestSendSlowSinkBackpressuresProviderInPlace(t *testing.T) {
	sinkEntered := make(chan struct{})
	releaseSink := make(chan struct{})
	providerAdvanced := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(ctx context.Context, _ string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
		if err := onFrame(json.RawMessage(`{"sequence":1}`)); err != nil {
			return nil, err
		}
		close(providerAdvanced)
		if err := onFrame(json.RawMessage(`{"sequence":2}`)); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"stopReason":"end_turn"}`), nil
	}
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
	t.Cleanup(func() { _ = instance.Close() })

	var calls atomic.Int32
	result := make(chan error, 1)
	go func() {
		_, err := instance.Send(context.Background(), "prompt", FrameSinkFunc(
			func(ctx context.Context, _ session.ID, _ json.RawMessage) error {
				if calls.Add(1) == 1 {
					close(sinkEntered)
					select {
					case <-releaseSink:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			},
		))
		result <- err
	}()
	waitClosed(t, sinkEntered, "first sink call")
	select {
	case <-providerAdvanced:
		t.Fatal("provider advanced while synchronous sink was blocked")
	default:
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("sink calls while blocked = %d, want 1", got)
	}
	close(releaseSink)
	waitClosed(t, providerAdvanced, "provider continuation")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Send after releasing sink: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backpressured Send")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("sink calls = %d, want 2", got)
	}
}

func TestSendRejectsConcurrentTurnAsBusy(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(ctx context.Context, _ string, _ func(json.RawMessage) error) (json.RawMessage, error) {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
			return json.RawMessage(`{"stopReason":"end_turn"}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
	t.Cleanup(func() { _ = instance.Close() })

	first := make(chan error, 1)
	go func() {
		_, err := instance.Send(context.Background(), "first prompt", FrameSinkFunc(discardFrame))
		first <- err
	}()
	waitClosed(t, entered, "first prompt entry")
	if _, err := instance.Send(context.Background(), "second prompt", FrameSinkFunc(discardFrame)); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent Send error = %v, want %v", err, ErrBusy)
	}
	if got := connection.snapshot().promptCalls; got != 1 {
		t.Fatalf("concurrent Send reached provider; Prompt calls=%d", got)
	}
	close(release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Send: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first Send")
	}
	if _, err := instance.Send(context.Background(), "third prompt", FrameSinkFunc(discardFrame)); err != nil {
		t.Fatalf("next sequential Send: %v", err)
	}
	if got := connection.snapshot().promptCalls; got != 2 {
		t.Fatalf("sequential Prompt calls=%d, want 2", got)
	}
}

func TestTwoManagersUseIsolatedConnectionsAndLifetimes(t *testing.T) {
	connectionA := newFakeConnection("ref-a")
	connectionB := newFakeConnection("ref-b")
	ports := &fakeManagerPorts{connections: []*fakeConnection{connectionA, connectionB}}
	registry := newRegistry(t, fakeProfile{id: "manager", ports: ports, auth: &fakeAuth{}, manager: true, resume: true})
	service, err := NewService(registry)
	if err != nil {
		t.Fatal(err)
	}
	start := func(id string) *Instance {
		instance, err := service.Start(context.Background(), StartRequest{
			Manager: managerMetadata(t, id, "manager", nil), ProjectCWD: "/tmp/" + id,
			ChildEvents: noOpChildEvents(),
		})
		if err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
		return instance
	}
	managerB := start("manager-b")
	managerA := start("manager-a")
	t.Cleanup(func() {
		_ = managerA.Close()
		_ = managerB.Close()
	})
	if got := service.ActiveIDs(); !reflect.DeepEqual(got, []session.ID{"manager-a", "manager-b"}) {
		t.Fatalf("ActiveIDs = %v, want deterministic [manager-a manager-b]", got)
	}
	if starts, resumes := ports.counts(); starts != 2 || resumes != 0 {
		t.Fatalf("factory calls = Start %d Resume %d, want 2/0", starts, resumes)
	}

	if _, err := managerA.Send(context.Background(), "only A", FrameSinkFunc(discardFrame)); err != nil {
		t.Fatalf("manager A Send: %v", err)
	}
	if _, err := managerB.Send(context.Background(), "only B", FrameSinkFunc(discardFrame)); err != nil {
		t.Fatalf("manager B Send: %v", err)
	}
	if got := connectionA.snapshot().prompts; !reflect.DeepEqual(got, []string{"only B"}) {
		t.Fatalf("first factory connection prompts = %v, want manager B only", got)
	}
	if got := connectionB.snapshot().prompts; !reflect.DeepEqual(got, []string{"only A"}) {
		t.Fatalf("second factory connection prompts = %v, want manager A only", got)
	}

	if err := managerA.Stop(context.Background()); err != nil {
		t.Fatalf("Stop(manager A): %v", err)
	}
	if !managerB.Available() {
		t.Fatal("stopping manager A made manager B unavailable")
	}
	if _, ok := service.Lookup(managerB.ID()); !ok {
		t.Fatal("stopping manager A removed manager B")
	}
	if got := connectionA.snapshot(); got.interrupts != 0 || got.closes != 0 {
		t.Fatalf("stopping manager A touched peer connection: Interrupt=%d Close=%d", got.interrupts, got.closes)
	}
	if got := connectionB.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("manager A connection Stop calls = Interrupt %d Close %d, want 1/1", got.interrupts, got.closes)
	}
	if _, err := managerB.Send(context.Background(), "B remains live", FrameSinkFunc(discardFrame)); err != nil {
		t.Fatalf("peer Send after manager A stop: %v", err)
	}
}

func TestSendBoundsAndSafeFailures(t *testing.T) {
	t.Run("invalid arguments do not call provider", func(t *testing.T) {
		connection := newFakeConnection("manager-ref")
		_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
		t.Cleanup(func() { _ = instance.Close() })
		validSink := FrameSinkFunc(discardFrame)
		var typedNilSink FrameSinkFunc
		for name, call := range map[string]func() error{
			"nil context": func() error {
				_, err := instance.Send(nil, "prompt", validSink)
				return err
			},
			"empty prompt": func() error {
				_, err := instance.Send(context.Background(), "", validSink)
				return err
			},
			"oversized prompt": func() error {
				_, err := instance.Send(context.Background(), strings.Repeat("x", MaxPromptBytes+1), validSink)
				return err
			},
			"nil sink": func() error {
				_, err := instance.Send(context.Background(), "prompt", nil)
				return err
			},
			"typed nil sink": func() error {
				_, err := instance.Send(context.Background(), "prompt", typedNilSink)
				return err
			},
		} {
			t.Run(name, func(t *testing.T) {
				if err := call(); !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("Send error = %v, want %v", err, ErrInvalidRequest)
				}
			})
		}
		if got := connection.snapshot().promptCalls; got != 0 {
			t.Fatalf("invalid Send called provider %d times", got)
		}
		if _, err := instance.Send(context.Background(), strings.Repeat("x", MaxPromptBytes), validSink); err != nil {
			t.Fatalf("max-size prompt rejected: %v", err)
		}
	})

	for _, test := range []struct {
		name     string
		function promptFunction
		sink     FrameSink
		want     error
	}{
		{
			name: "empty frame",
			function: func(_ context.Context, _ string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, onFrame(nil)
			},
			sink: FrameSinkFunc(discardFrame), want: ErrCapacity,
		},
		{
			name: "oversized frame",
			function: func(_ context.Context, _ string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, onFrame(json.RawMessage(bytes.Repeat([]byte{'x'}, MaxPublicFrameBytes+1)))
			},
			sink: FrameSinkFunc(discardFrame), want: ErrCapacity,
		},
		{
			name: "sink failure",
			function: func(_ context.Context, _ string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, onFrame(json.RawMessage(`{"sequence":1}`))
			},
			sink: FrameSinkFunc(func(context.Context, session.ID, json.RawMessage) error {
				return errors.New("sink content secret /path --token=value")
			}),
			want: ErrFrameDelivery,
		},
		{
			name: "empty completion",
			function: func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, nil
			},
			sink: FrameSinkFunc(discardFrame), want: ErrMalformedCompletion,
		},
		{
			name: "invalid completion json",
			function: func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
				return json.RawMessage(`{"stopReason":`), nil
			},
			sink: FrameSinkFunc(discardFrame), want: ErrMalformedCompletion,
		},
		{
			name: "oversized completion",
			function: func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
				return json.RawMessage(bytes.Repeat([]byte{'x'}, MaxCompletionBytes+1)), nil
			},
			sink: FrameSinkFunc(discardFrame), want: ErrMalformedCompletion,
		},
		{
			name: "connector malformed completion",
			function: func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, connector.ErrMalformedCompletion
			},
			sink: FrameSinkFunc(discardFrame), want: ErrMalformedCompletion,
		},
		{
			name: "raw provider failure",
			function: func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
				return nil, errors.New("provider secret content /native/path --api-key=value")
			},
			sink: FrameSinkFunc(discardFrame), want: ErrTransport,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := newFakeConnection("manager-ref")
			connection.promptFunction = test.function
			_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
			t.Cleanup(func() { _ = instance.Close() })
			_, err := instance.Send(context.Background(), "prompt", test.sink)
			if !errors.Is(err, test.want) {
				t.Fatalf("Send error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/native/") || strings.Contains(err.Error(), "api-key") {
				t.Fatalf("unsafe diagnostic escaped: %q", err)
			}
		})
	}
}

func TestCrashPublishesOneSafeTerminalAndWakesSend(t *testing.T) {
	const canary = "provider crashed: --token=secret /native/path ref=opaque content=answer"
	promptEntered := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(ctx context.Context, _ string, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(promptEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	service, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	sendResult := make(chan error, 1)
	go func() {
		_, err := instance.Send(context.Background(), "prompt", FrameSinkFunc(discardFrame))
		sendResult <- err
	}()
	waitClosed(t, promptEntered, "in-flight prompt")
	connection.crash(errors.New(canary))
	waitClosed(t, instance.Done(), "manager terminal")
	select {
	case err := <-sendResult:
		if !errors.Is(err, ErrTransport) || strings.Contains(err.Error(), canary) {
			t.Fatalf("Send after crash error = %q, want safe transport code", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("crash did not wake in-flight Send")
	}
	terminal, ok := instance.Terminal()
	if !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeTransport}) {
		t.Fatalf("Terminal = %#v, %v, want failed/transport", terminal, ok)
	}
	if again, ok := instance.Terminal(); !ok || again != terminal {
		t.Fatalf("Terminal changed on second read: %#v, %v", again, ok)
	}
	if instance.Available() {
		t.Fatal("crashed instance remains available")
	}
	if _, ok := service.Lookup(instance.ID()); ok {
		t.Fatal("crashed instance remains registered")
	}
	if got := service.ActiveIDs(); len(got) != 0 {
		t.Fatalf("ActiveIDs after crash = %v, want empty", got)
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("Close after crash: %v", err)
	}
}

func TestCallerCancellationEndsOnlyTurnAndPermitsNextSend(t *testing.T) {
	entered := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(ctx context.Context, _ string, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	service, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)
	t.Cleanup(func() { _ = instance.Close() })

	turnContext, cancelTurn := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := instance.Send(turnContext, "cancel me", FrameSinkFunc(discardFrame))
		result <- err
	}()
	waitClosed(t, entered, "cancelled turn entry")
	cancelTurn()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("cancelled Send error = %v, want %v", err, ErrCancelled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation did not wake Send")
	}
	if !instance.Available() {
		t.Fatal("caller turn cancellation stopped manager lifetime")
	}
	if lookedUp, ok := service.Lookup(instance.ID()); !ok || lookedUp != instance {
		t.Fatal("caller cancellation removed manager")
	}
	connection.mu.Lock()
	connection.promptFunction = func(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
		return json.RawMessage(`{"stopReason":"end_turn"}`), nil
	}
	connection.mu.Unlock()
	if _, err := instance.Send(context.Background(), "next turn", FrameSinkFunc(discardFrame)); err != nil {
		t.Fatalf("Send after caller cancellation: %v", err)
	}
	if got := connection.snapshot(); got.closes != 0 || got.interrupts != 0 {
		t.Fatalf("caller cancellation touched lifetime: Interrupt=%d Close=%d", got.interrupts, got.closes)
	}
}

func TestSupervisorCancellationClosesLifetimeAndDoesNotLeakCaller(t *testing.T) {
	entered := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.promptFunction = func(ctx context.Context, _ string, _ func(json.RawMessage) error) (json.RawMessage, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	_, instance, _ := startTestManager(t, lifetime, "manager-one", connection)

	result := make(chan error, 1)
	go func() {
		_, err := instance.Send(context.Background(), "prompt", FrameSinkFunc(discardFrame))
		result <- err
	}()
	waitClosed(t, entered, "supervisor-owned prompt")
	cancelLifetime()
	waitClosed(t, connection.Done(), "connector lifetime cancellation")
	waitClosed(t, instance.Done(), "instance lifetime cancellation")
	select {
	case err := <-result:
		if !errors.Is(err, ErrCancelled) && !errors.Is(err, ErrTransport) {
			t.Fatalf("Send after supervisor cancellation = %v, want bounded cancellation/transport", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor cancellation left Send goroutine blocked")
	}
	terminal, ok := instance.Terminal()
	if !ok || terminal.Kind != TerminalFailed || terminal.Code != CodeTransport {
		t.Fatalf("Terminal after supervisor cancellation = %#v, %v", terminal, ok)
	}
}

func TestStopIsExactIdempotentAndRemovesOnlyInstance(t *testing.T) {
	interruptEntered := make(chan struct{}, 1)
	interruptGate := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.interruptEnter = interruptEntered
	connection.interruptGate = interruptGate
	service, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- instance.Stop(context.Background())
		}()
	}
	close(start)
	waitClosed(t, interruptEntered, "single Interrupt")
	if got := connection.snapshot().interrupts; got != 1 {
		t.Fatalf("Interrupt calls while concurrent Stop blocked = %d, want 1", got)
	}
	close(interruptGate)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Stop: %v", err)
		}
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if got := connection.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("lifetime calls = Interrupt %d Close %d, want exact 1/1", got.interrupts, got.closes)
	}
	waitClosed(t, instance.Done(), "stopped terminal")
	if terminal, ok := instance.Terminal(); !ok || terminal != (Terminal{Kind: TerminalStopped, Code: CodeStopped}) {
		t.Fatalf("Terminal = %#v, %v, want stopped", terminal, ok)
	}
	if instance.Available() {
		t.Fatal("stopped instance remains available")
	}
	if _, ok := service.Lookup(instance.ID()); ok {
		t.Fatal("stopped instance remains registered")
	}
	if _, err := instance.Send(context.Background(), "after stop", FrameSinkFunc(discardFrame)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Send after Stop error = %v, want %v", err, ErrUnavailable)
	}
}

func TestStopFailureIsBoundedAndStillClosesExactlyOnce(t *testing.T) {
	const canary = "provider stop secret --token=value /private/path opaque-ref content"
	connection := newFakeConnection("manager-ref")
	connection.interruptErr = errors.New(canary)
	connection.closeErr = errors.New(canary)
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	err := instance.Stop(context.Background())
	if !errors.Is(err, ErrStopFailed) || strings.Contains(err.Error(), canary) {
		t.Fatalf("Stop error = %q, want safe stop_failed", err)
	}
	if again := instance.Close(); !errors.Is(again, ErrStopFailed) {
		t.Fatalf("repeated Close error = %v, want same stop failure", again)
	}
	if got := connection.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("failed Stop calls = Interrupt %d Close %d, want exact 1/1", got.interrupts, got.closes)
	}
	terminal, ok := instance.Terminal()
	if !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeStopFailed}) {
		t.Fatalf("Terminal = %#v, %v, want failed/stop_failed", terminal, ok)
	}
}

func TestStopDeadlineHardClosesInterruptThatIgnoresContext(t *testing.T) {
	gate := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.interruptGate = gate
	connection.interruptIgnoresContext = true
	connection.closeInterruptGate = gate
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := instance.Stop(ctx)
	if !errors.Is(err, ErrStopFailed) {
		t.Fatalf("deadline Stop = %v, want stop_failed", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline Stop remained blocked behind provider Interrupt")
	}
	if got := connection.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("forced Stop calls = Interrupt %d Close %d, want 1/1", got.interrupts, got.closes)
	}
	if terminal, ok := instance.Terminal(); !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeStopFailed}) {
		t.Fatalf("forced Stop terminal = %#v, %v", terminal, ok)
	}
}

func TestStopDeadlineDoesNotWaitForInterruptAfterHardClose(t *testing.T) {
	gate := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.interruptGate = gate
	connection.interruptIgnoresContext = true
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := instance.Stop(ctx)
	close(gate)
	if !errors.Is(err, ErrStopFailed) {
		t.Fatalf("deadline Stop = %v, want stop_failed", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline Stop waited for Interrupt after hard Close returned")
	}
	if got := connection.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("forced Stop calls = Interrupt %d Close %d, want 1/1", got.interrupts, got.closes)
	}
}

func TestCrashDuringStopPublishesOneRaceSafeTerminal(t *testing.T) {
	interruptEntered := make(chan struct{}, 1)
	interruptGate := make(chan struct{})
	connection := newFakeConnection("manager-ref")
	connection.interruptEnter = interruptEntered
	connection.interruptGate = interruptGate
	_, instance, _ := startTestManager(t, context.Background(), "manager-one", connection)

	stopResult := make(chan error, 1)
	go func() { stopResult <- instance.Stop(context.Background()) }()
	waitClosed(t, interruptEntered, "Stop race Interrupt")
	connection.crash(errors.New("provider crash secret content /path"))
	close(interruptGate)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("Stop during crash: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop/crash race did not terminate")
	}
	waitClosed(t, instance.Done(), "Stop/crash terminal")
	terminal, ok := instance.Terminal()
	if !ok || terminal != (Terminal{Kind: TerminalStopped, Code: CodeStopped}) {
		t.Fatalf("terminal after Stop-owned race = %#v, %v", terminal, ok)
	}
	if got := connection.snapshot(); got.interrupts != 1 || got.closes != 1 {
		t.Fatalf("Stop/crash calls = Interrupt %d Close %d, want 1/1", got.interrupts, got.closes)
	}
}

func TestSafeDiagnosticsAreFiniteAndContentFree(t *testing.T) {
	safeErrors := []struct {
		err  error
		code Code
	}{
		{ErrInvalidRequest, CodeInvalidRequest},
		{ErrProviderUnknown, CodeProviderUnknown},
		{ErrUnsupported, CodeUnsupported},
		{ErrAlreadyActive, CodeAlreadyActive},
		{ErrUnavailable, CodeUnavailable},
		{ErrBusy, CodeBusy},
		{ErrCapacity, CodeCapacity},
		{ErrFrameDelivery, CodeFrameDelivery},
		{ErrMalformedCompletion, CodeMalformedCompletion},
		{ErrTransport, CodeTransport},
		{ErrCancelled, CodeCancelled},
		{ErrStopFailed, CodeStopFailed},
	}
	for _, test := range safeErrors {
		if got := CodeOf(test.err); got != test.code {
			t.Errorf("CodeOf(%v) = %q, want %q", test.err, got, test.code)
		}
		if len(test.err.Error()) > len("manager: ")+MaxDiagnosticCodeBytes {
			t.Errorf("diagnostic exceeds hard bound: %q", test.err)
		}
		var coded interface{ SafeCode() string }
		if !errors.As(test.err, &coded) || coded.SafeCode() != string(test.code) {
			t.Errorf("SafeCode(%v) unavailable or changed", test.err)
		}
	}
	const canary = "credential --api-key=value /private/path native-ref prompt answer environment"
	raw := &connector.BoundaryError{Stage: "provider stage " + canary, Err: errors.New(canary)}
	sanitized := sanitizeError(raw)
	if !errors.Is(sanitized, ErrTransport) || strings.Contains(sanitized.Error(), canary) {
		t.Fatalf("sanitizeError leaked boundary detail: %q", sanitized)
	}
	if got := CodeOf(errors.New(canary)); got != CodeTransport {
		t.Fatalf("CodeOf(raw) = %q, want transport", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Fatalf("CodeOf(nil) = %q, want empty", got)
	}
	for _, mapping := range []struct {
		input error
		want  error
	}{
		{nil, nil},
		{context.Canceled, ErrCancelled},
		{context.DeadlineExceeded, ErrCancelled},
		{connector.ErrProviderUnknown, ErrProviderUnknown},
		{connector.ErrProviderUnsupported, ErrUnsupported},
		{connector.ErrChildBusy, ErrBusy},
		{connector.ErrCapacity, ErrCapacity},
		{connector.ErrAnswerLimit, ErrCapacity},
		{connector.ErrMalformedCompletion, ErrMalformedCompletion},
		{connector.ErrInvalidOperation, ErrInvalidRequest},
		{connector.ErrOwnership, ErrInvalidRequest},
		{connector.ErrTransport, ErrTransport},
	} {
		got := sanitizeError(mapping.input)
		if mapping.want == nil {
			if got != nil {
				t.Errorf("sanitizeError(nil) = %v", got)
			}
			continue
		}
		if !errors.Is(got, mapping.want) {
			t.Errorf("sanitizeError(%v) = %v, want %v", mapping.input, got, mapping.want)
		}
	}

	instance := &Instance{done: make(chan struct{})}
	instance.finish(Terminal{Kind: TerminalFailed, Code: Code("provider-secret-code")})
	if terminal, ok := instance.Terminal(); !ok || terminal != (Terminal{Kind: TerminalFailed, Code: CodeTransport}) {
		t.Fatalf("unknown terminal code was not fail-closed: %#v, %v", terminal, ok)
	}
}

func TestInstanceSurfaceContainsNoContentQueueHistoryOrViewerState(t *testing.T) {
	instanceType := reflect.TypeOf(Instance{})
	gotFields := make([]string, instanceType.NumField())
	for index := range instanceType.NumField() {
		gotFields[index] = instanceType.Field(index).Name
	}
	wantFields := []string{
		"id", "providerID", "reference", "publicSessionID", "connection", "lifetime", "cancel", "release",
		"stateMu", "busy", "unavailable", "stopping",
		"done", "terminalOnce", "terminalMu", "terminal",
		"stopOnce", "stopMu", "stopErr", "closeOnce", "closeErr",
	}
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("Instance fields changed:\n got %v\nwant %v", gotFields, wantFields)
	}
	for _, field := range gotFields {
		lower := strings.ToLower(field)
		for _, forbidden := range []string{
			"prompt", "answer", "frame", "tool", "replay", "transcript", "history",
			"queue", "buffer", "spill", "viewer", "attach", "detach", "command", "path", "environment",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("Instance retains forbidden %q field %q", forbidden, field)
			}
		}
	}

	pointerType := reflect.TypeOf((*Instance)(nil))
	methods := make([]string, pointerType.NumMethod())
	for index := range pointerType.NumMethod() {
		methods[index] = pointerType.Method(index).Name
	}
	sort.Strings(methods)
	wantMethods := []string{"Available", "Close", "Done", "ForceClose", "ID", "NativeReference", "ProviderID", "PublicSessionID", "Send", "Stop", "Terminal"}
	if !reflect.DeepEqual(methods, wantMethods) {
		t.Fatalf("Instance public methods = %v, want narrow %v", methods, wantMethods)
	}
}
