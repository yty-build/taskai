package manager

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/session"
)

const (
	MaxPromptBytes      = acp.MaxFrameBytes
	MaxPublicFrameBytes = acp.MaxFrameBytes
	MaxCompletionBytes  = acp.MaxFrameBytes
)

// FrameSink is a synchronous, backpressured caller port. PublicFrame must not
// return until it has accepted the frame or rejected the turn. Instance never
// buffers, drops, spills, polls, or dispatches a goroutine per frame.
type FrameSink interface {
	PublicFrame(context.Context, session.ID, json.RawMessage) error
}

type FrameSinkFunc func(context.Context, session.ID, json.RawMessage) error

func (function FrameSinkFunc) PublicFrame(ctx context.Context, id session.ID, frame json.RawMessage) error {
	return function(ctx, id, frame)
}

// TerminalKind is the one-time mechanical lifetime outcome of an Instance.
type TerminalKind string

const (
	TerminalStopped TerminalKind = "stopped"
	TerminalFailed  TerminalKind = "failed"
)

// Terminal contains no provider text or content. Done closes exactly once
// before Terminal becomes observable.
type Terminal struct {
	Kind TerminalKind
	Code Code
}

// Instance owns exactly one live connector.ManagerConnection and no viewer
// attachment state. It retains only identity, opaque provider reference, and
// bounded mechanical concurrency/lifecycle state.
type Instance struct {
	id         session.ID
	providerID string
	reference  replay.OpaqueRef
	connection connector.ManagerConnection
	lifetime   context.Context
	cancel     context.CancelFunc
	release    func(*Instance)

	stateMu     sync.Mutex
	busy        bool
	unavailable bool
	stopping    bool

	done         chan struct{}
	terminalOnce sync.Once
	terminalMu   sync.RWMutex
	terminal     Terminal

	stopOnce sync.Once
	stopMu   sync.Mutex
	stopErr  error
}

func newInstance(
	id session.ID,
	providerID string,
	reference replay.OpaqueRef,
	connection connector.ManagerConnection,
	lifetime context.Context,
	cancel context.CancelFunc,
	release func(*Instance),
) *Instance {
	return &Instance{
		id: id, providerID: providerID,
		reference: reference, connection: connection,
		lifetime: lifetime, cancel: cancel, release: release,
		done: make(chan struct{}),
	}
}

func (instance *Instance) ID() session.ID {
	if instance == nil {
		return ""
	}
	return instance.id
}

func (instance *Instance) ProviderID() string {
	if instance == nil {
		return ""
	}
	return instance.providerID
}

// NativeReference returns only a defensive opaque envelope for metadata-only
// persistence and exact Resume.
func (instance *Instance) NativeReference() replay.OpaqueRef {
	if instance == nil {
		return replay.OpaqueRef{}
	}
	return replay.OpaqueRefFromBytes(instance.reference.Bytes())
}

func (instance *Instance) Done() <-chan struct{} {
	if instance == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return instance.done
}

// Terminal returns the content-free one-time outcome after Done closes.
func (instance *Instance) Terminal() (Terminal, bool) {
	if instance == nil {
		return Terminal{Kind: TerminalFailed, Code: CodeInvalidRequest}, true
	}
	select {
	case <-instance.done:
		instance.terminalMu.RLock()
		terminal := instance.terminal
		instance.terminalMu.RUnlock()
		return terminal, true
	default:
		return Terminal{}, false
	}
}

func (instance *Instance) Available() bool {
	if instance == nil {
		return false
	}
	instance.stateMu.Lock()
	available := !instance.unavailable && !instance.stopping
	instance.stateMu.Unlock()
	return available
}

// Send executes one manager prompt turn. Prompt bytes are passed unchanged;
// frames cross the synchronous sink in connector order; the returned native
// completion is defensively copied and never retained by Instance. ctx owns
// only this turn and does not own the Manager lifetime.
func (instance *Instance) Send(
	ctx context.Context,
	prompt string,
	sink FrameSink,
) (json.RawMessage, error) {
	if instance == nil || nilPort(ctx) || nilPort(sink) || len(prompt) == 0 || len(prompt) > MaxPromptBytes {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, sanitizeError(err)
	}
	if err := instance.beginSend(); err != nil {
		return nil, err
	}
	defer instance.endSend()

	turnContext, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(instance.lifetime, cancel)
	defer func() {
		stopLifetime()
		cancel()
	}()

	var sinkFailed atomic.Bool
	completion, err := instance.connection.Prompt(turnContext, prompt, func(frame json.RawMessage) error {
		if turnContext.Err() != nil {
			return turnContext.Err()
		}
		if len(frame) == 0 || len(frame) > MaxPublicFrameBytes {
			return ErrCapacity
		}
		copyOfFrame := append(json.RawMessage(nil), frame...)
		if sink.PublicFrame(turnContext, instance.id, copyOfFrame) != nil {
			sinkFailed.Store(true)
			return ErrFrameDelivery
		}
		return nil
	})
	if sinkFailed.Load() {
		return nil, ErrFrameDelivery
	}
	if err != nil {
		if !instance.Available() {
			if terminal, ok := instance.Terminal(); ok && terminal.Code != "" {
				return nil, SafeError{code: terminal.Code}
			}
			return nil, ErrUnavailable
		}
		return nil, sanitizeError(err)
	}
	if len(completion) == 0 || len(completion) > MaxCompletionBytes || !json.Valid(completion) {
		return nil, ErrMalformedCompletion
	}
	return append(json.RawMessage(nil), completion...), nil
}

func (instance *Instance) beginSend() error {
	instance.stateMu.Lock()
	defer instance.stateMu.Unlock()
	if instance.unavailable || instance.stopping {
		return ErrUnavailable
	}
	if instance.lifetime.Err() != nil {
		instance.unavailable = true
		return ErrUnavailable
	}
	if instance.busy {
		return ErrBusy
	}
	instance.busy = true
	return nil
}

func (instance *Instance) endSend() {
	instance.stateMu.Lock()
	instance.busy = false
	instance.stateMu.Unlock()
}

// Stop interrupts and closes only this Manager composite. Concurrent and
// repeated Stop/Close calls execute the connector operations exactly once.
func (instance *Instance) Stop(ctx context.Context) error {
	if instance == nil || nilPort(ctx) {
		return ErrInvalidRequest
	}
	instance.stopOnce.Do(func() {
		instance.stopMu.Lock()
		instance.stopErr = instance.stop(ctx)
		instance.stopMu.Unlock()
	})
	instance.stopMu.Lock()
	err := instance.stopErr
	instance.stopMu.Unlock()
	return err
}

func (instance *Instance) stop(ctx context.Context) error {
	instance.stateMu.Lock()
	instance.stopping = true
	instance.unavailable = true
	instance.stateMu.Unlock()

	interruptErr := instance.connection.Interrupt(ctx)
	// Cancellation wakes the active Send even if the provider transport is
	// concurrently failing; Close still owns exact composite teardown.
	instance.cancel()
	closeErr := instance.connection.Close()

	failed := meaningfulStopError(interruptErr) || closeErr != nil
	if failed {
		instance.finish(Terminal{Kind: TerminalFailed, Code: CodeStopFailed})
		return ErrStopFailed
	}
	instance.finish(Terminal{Kind: TerminalStopped, Code: CodeStopped})
	return nil
}

// Close is the idempotent ownership fallback for callers without a request
// context. Runtime supervisors should prefer Stop with their bounded context.
func (instance *Instance) Close() error {
	if instance == nil {
		return nil
	}
	return instance.Stop(context.Background())
}

func meaningfulStopError(err error) bool {
	return err != nil && !errors.Is(err, connector.ErrStaleTurn) &&
		!errors.Is(err, connector.ErrTransport) && !errors.Is(err, context.Canceled)
}

func (instance *Instance) watch() {
	go func() {
		select {
		case <-instance.connection.Done():
			instance.stateMu.Lock()
			stopping := instance.stopping
			instance.unavailable = true
			instance.stateMu.Unlock()
			if !stopping {
				code := CodeOf(sanitizeError(instance.connection.Err()))
				if code == "" || code == CodeCancelled {
					code = CodeTransport
				}
				instance.finish(Terminal{Kind: TerminalFailed, Code: code})
			}
			// Publish the terminal outcome before waking an in-flight Send so
			// that the caller observes the same one-time mechanical code.
			instance.cancel()
		case <-instance.done:
		}
	}()
}

func (instance *Instance) finish(terminal Terminal) {
	instance.terminalOnce.Do(func() {
		if terminal.Kind != TerminalStopped && terminal.Kind != TerminalFailed {
			terminal = Terminal{Kind: TerminalFailed, Code: CodeTransport}
		}
		if !terminal.Code.valid() || len(terminal.Code) > MaxDiagnosticCodeBytes {
			terminal.Code = CodeTransport
		}
		instance.stateMu.Lock()
		instance.unavailable = true
		instance.stateMu.Unlock()
		instance.terminalMu.Lock()
		instance.terminal = terminal
		instance.terminalMu.Unlock()
		if instance.release != nil {
			instance.release(instance)
		}
		close(instance.done)
	})
}
