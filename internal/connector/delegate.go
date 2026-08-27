package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/replay"
)

const (
	MaxChildrenPerManager    = 64
	MaxTurnHandles           = 64
	MaxAnswerChunks          = 256
	MaxAnswerBytes           = acp.MaxFrameBytes
	MaxCapabilityResultBytes = acp.MaxFrameBytes
	MaxCorrelationIDBytes    = 256
	MaxNativeReferenceBytes  = 4 << 10
)

type Mode string

const (
	ModeAnswer Mode = "answer"
	ModeFollow Mode = "follow"
)

type Operation string

const (
	OperationStart      Operation = "start"
	OperationNextAnswer Operation = "next_answer"
	OperationFollowNext Operation = "follow_next"
	OperationCancel     Operation = "cancel"
	OperationReplay     Operation = "replay"
)

// ChildEvent is the only TaskAI-core-facing output of this package. It contains
// structural ownership and transient correlation only.
type ChildEvent struct {
	ParentSessionID string `json:"parent_session_id"`
	ChildSessionID  string `json:"child_session_id"`
	Provider        string `json:"provider"`
	// NativeReference is present only for the lifecycle that creates a child.
	// Restored and later invocations are already owned and never re-emit it.
	NativeReference []byte `json:"native_reference,omitempty"`
	Lifecycle       string `json:"lifecycle"`
	Mode            Mode   `json:"mode"`
	RequestID       string `json:"request_id"`
	TurnID          string `json:"turn_id"`
}

type ChildEventSink interface {
	ChildEvent(context.Context, ChildEvent) error
}

type ChildEventSinkFunc func(context.Context, ChildEvent) error

func (f ChildEventSinkFunc) ChildEvent(ctx context.Context, event ChildEvent) error {
	return f(ctx, event)
}

// ChildPort is one provider-owned ACP Session. Prompt content and frame content
// are transient arguments only and are never fields on Delegator state.
type ChildPort interface {
	NativeReference() replay.OpaqueRef
	Prompt(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error)
	Cancel(context.Context) error
	Close() error
}

type ChildFactory interface {
	New(context.Context) (ChildPort, error)
}

type ResumeChildFactory interface {
	Resume(context.Context, replay.OpaqueRef) (ChildPort, error)
}

type IDSource interface {
	Next(string) (string, error)
}

type randomIDs struct{}

func (randomIDs) Next(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

type childState struct {
	id        string
	provider  string
	port      ChildPort
	reference replay.OpaqueRef
	active    *turnState
	mode      Mode
	closed    bool
	replaying bool
	resuming  bool
}

type turnState struct {
	child     *childState
	token     string
	turnID    string
	requestID string
	mode      Mode
	created   bool
	queue     *frameQueue
	terminal  bool
}

type readyAnswer struct {
	childID  string
	token    string
	mode     Mode
	terminal Terminal
}

// Delegator is one manager-owned, bounded, RAM-only taskai_delegate runtime.
// It has no persistence port and emits no content through ChildEventSink.
type Delegator struct {
	parentID string
	registry *Registry
	events   ChildEventSink
	ids      IDSource

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	children map[string]*childState
	turns    map[string]*turnState

	answerPending int
	answerReady   []readyAnswer
	answerChanged chan struct{}
	closed        bool
	fatal         error
	done          chan struct{}
	doneOnce      sync.Once
}

func NewDelegator(ctx context.Context, parentSessionID string, registry *Registry, events ChildEventSink, ids IDSource) (*Delegator, error) {
	if parentSessionID == "" || len(parentSessionID) > MaxCorrelationIDBytes || registry == nil || events == nil {
		return nil, ErrProviderUnsupported
	}
	if ids == nil {
		ids = randomIDs{}
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	return &Delegator{
		parentID:      parentSessionID,
		registry:      registry,
		events:        events,
		ids:           ids,
		ctx:           ownerCtx,
		cancel:        cancel,
		children:      make(map[string]*childState),
		turns:         make(map[string]*turnState),
		answerChanged: make(chan struct{}),
		done:          make(chan struct{}),
	}, nil
}

// Invoke is the sole model-facing capability handler. The input and returned
// JSON never pass through ChildEventSink or a persistence interface.
func (d *Delegator) Invoke(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, ErrTransport
	}
	input, err := parseDelegateInput(raw)
	if err != nil {
		return nil, err
	}
	var result capabilityResult
	switch input.Operation {
	case OperationStart:
		result, err = d.start(ctx, input)
	case OperationNextAnswer:
		result, err = d.nextAnswer(ctx)
	case OperationFollowNext:
		result, err = d.followNext(ctx, input.ChildSessionID, input.TurnToken)
	case OperationCancel:
		result, err = d.cancelTurn(ctx, input.ChildSessionID, input.TurnToken)
	case OperationReplay:
		result, err = d.replay(ctx, input)
	default:
		err = ErrInvalidOperation
	}
	if err != nil {
		return nil, err
	}
	return result.marshal()
}

func (d *Delegator) start(ctx context.Context, input delegateInput) (capabilityResult, error) {
	token, err := d.nextID("turn")
	if err != nil {
		return capabilityResult{}, &BoundaryError{Stage: "turn correlation", Err: ErrCapacity}
	}
	turnID, err := d.nextID("invocation")
	if err != nil {
		return capabilityResult{}, &BoundaryError{Stage: "turn correlation", Err: ErrCapacity}
	}
	requestID, err := d.nextID("request")
	if err != nil {
		return capabilityResult{}, &BoundaryError{Stage: "request correlation", Err: ErrCapacity}
	}
	child, created, err := d.resolveChild(input.Provider, input.ChildSessionID)
	if err != nil {
		return capabilityResult{}, err
	}

	turn := &turnState{
		child: child, token: token, turnID: turnID, requestID: requestID,
		mode: input.Mode, created: created,
	}
	if input.Mode == ModeFollow {
		turn.queue = newFrameQueue()
	}

	d.mu.Lock()
	if d.closed {
		if created {
			delete(d.children, child.id)
			child.closed = true
		}
		d.mu.Unlock()
		if created {
			_ = child.port.Close()
		}
		return capabilityResult{}, ErrTransport
	}
	if child.closed || child.active != nil || child.replaying || child.resuming {
		if created {
			delete(d.children, child.id)
			child.closed = true
		}
		d.mu.Unlock()
		if created {
			_ = child.port.Close()
		}
		return capabilityResult{}, ErrChildBusy
	}
	if len(d.turns)+len(d.answerReady) >= MaxTurnHandles {
		if created {
			delete(d.children, child.id)
			child.closed = true
		}
		d.mu.Unlock()
		if created {
			_ = child.port.Close()
		}
		return capabilityResult{}, ErrCapacity
	}
	child.active = turn
	child.mode = input.Mode
	d.turns[token] = turn
	if input.Mode == ModeAnswer {
		d.answerPending++
	}
	d.mu.Unlock()

	event := ChildEvent{
		ParentSessionID: d.parentID,
		ChildSessionID:  child.id,
		Provider:        child.provider,
		Lifecycle:       "prompt_pending",
		Mode:            input.Mode,
		RequestID:       requestID,
		TurnID:          turnID,
	}
	if created {
		event.NativeReference = child.reference.Bytes()
	}
	if err := d.events.ChildEvent(ctx, event); err != nil {
		d.rollbackTurn(turn, created)
		return capabilityResult{}, &BoundaryError{Stage: "child event", Err: ErrTransport}
	}

	d.wg.Add(1)
	go d.runTurn(turn, input.Prompt)

	result := capabilityResult{
		Operation:      OperationStart,
		ChildSessionID: child.id,
		TurnToken:      token,
		Mode:           input.Mode,
		More:           true,
	}
	if input.Mode == ModeAnswer {
		result.setContinuation(OperationNextAnswer, "", "")
		return result, nil
	}

	if batch, ready, batchErr := turn.queue.tryNext(d.followBatchFits(OperationStart, child.id, token)); batchErr != nil && !errors.Is(batchErr, ErrCapacity) {
		return capabilityResult{}, batchErr
	} else if ready {
		result.Frames = batch.Frames
		result.Terminal = batch.Terminal
		result.More = batch.More
		if !batch.More {
			d.invalidateFollow(turn)
			return result, nil
		}
	}
	result.setContinuation(OperationFollowNext, child.id, token)
	return result, nil
}

func (d *Delegator) resolveChild(provider, childID string) (*childState, bool, error) {
	if (provider == "") == (childID == "") {
		return nil, false, ErrInvalidOperation
	}
	if childID != "" {
		d.mu.Lock()
		child, ok := d.children[childID]
		if !ok || child.closed {
			d.mu.Unlock()
			return nil, false, ErrOwnership
		}
		if child.port != nil {
			if child.replaying || child.resuming {
				d.mu.Unlock()
				return nil, false, ErrChildBusy
			}
			d.mu.Unlock()
			return child, false, nil
		}
		if child.active != nil || child.replaying || child.resuming {
			d.mu.Unlock()
			return nil, false, ErrChildBusy
		}
		provider = child.provider
		reference := replay.OpaqueRefFromBytes(child.reference.Bytes())
		child.resuming = true
		d.mu.Unlock()

		profile, err := d.registry.Profile(provider)
		if err != nil || !profile.Capabilities.Resume || profile.ChildResume == nil {
			d.clearResumeReservation(child)
			return nil, false, ErrProviderUnsupported
		}
		port, err := profile.ChildResume.Resume(d.ctx, reference)
		if err != nil || port == nil {
			d.clearResumeReservation(child)
			return nil, false, &BoundaryError{Stage: "subordinate resume", Err: ErrTransport}
		}
		resolved := port.NativeReference()
		if !bytes.Equal(resolved.Bytes(), reference.Bytes()) {
			_ = port.Close()
			d.clearResumeReservation(child)
			return nil, false, &BoundaryError{Stage: "subordinate resume reference", Err: ErrProviderUnsupported}
		}

		d.mu.Lock()
		defer d.mu.Unlock()
		current, ok := d.children[childID]
		if !ok || current != child || child.closed || d.closed {
			child.resuming = false
			_ = port.Close()
			return nil, false, ErrOwnership
		}
		if child.port != nil || child.active != nil || child.replaying {
			child.resuming = false
			_ = port.Close()
			return nil, false, ErrChildBusy
		}
		child.port = port
		child.resuming = false
		return child, false, nil
	}

	profile, err := d.registry.Profile(provider)
	if err != nil {
		return nil, false, err
	}
	if !profile.Capabilities.Subordinate || profile.Children == nil {
		return nil, false, ErrProviderUnsupported
	}
	d.mu.Lock()
	if len(d.children) >= MaxChildrenPerManager {
		d.mu.Unlock()
		return nil, false, ErrCapacity
	}
	d.mu.Unlock()

	port, err := profile.Children.New(d.ctx)
	if err != nil || port == nil {
		return nil, false, &BoundaryError{Stage: "subordinate start", Err: ErrTransport}
	}
	reference := port.NativeReference()
	if len(reference.Bytes()) == 0 || len(reference.Bytes()) > MaxNativeReferenceBytes {
		_ = port.Close()
		return nil, false, &BoundaryError{Stage: "subordinate reference", Err: ErrProviderUnsupported}
	}
	newID, err := d.nextID("child")
	if err != nil {
		_ = port.Close()
		return nil, false, &BoundaryError{Stage: "child identity", Err: ErrCapacity}
	}
	child := &childState{
		id: newID, provider: provider, port: port,
		reference: replay.OpaqueRefFromBytes(reference.Bytes()), mode: ModeAnswer,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || len(d.children) >= MaxChildrenPerManager {
		_ = port.Close()
		return nil, false, ErrCapacity
	}
	if _, exists := d.children[newID]; exists {
		_ = port.Close()
		return nil, false, ErrCapacity
	}
	d.children[newID] = child
	return child, true, nil
}

func (d *Delegator) clearResumeReservation(child *childState) {
	d.mu.Lock()
	if child != nil {
		child.resuming = false
	}
	d.mu.Unlock()
}

func (d *Delegator) nextID(prefix string) (string, error) {
	value, err := d.ids.Next(prefix)
	if err != nil || value == "" || len(value) > MaxCorrelationIDBytes {
		return "", ErrCapacity
	}
	return value, nil
}

func (d *Delegator) runTurn(turn *turnState, prompt string) {
	defer d.wg.Done()
	collector := answerCollector{}
	var collectErr error
	onFrame := func(frame json.RawMessage) error {
		if turn.mode == ModeFollow {
			return turn.queue.push(d.ctx, frame)
		}
		if collectErr == nil {
			collectErr = collector.add(frame)
		}
		return nil
	}
	native, err := turn.child.port.Prompt(d.ctx, prompt, onFrame)
	terminal := classifyTerminal(native, err, nil)
	if turn.mode == ModeAnswer {
		if collectErr != nil {
			terminal = Terminal{Success: false, StopReason: "malformed_completion"}
		} else if terminal.Success {
			terminal.Answer = collector.frames()
		}
		// Reserve the exact encoded next_answer budget before this completion
		// becomes ready. An oversized native completion/answer is exposed as a
		// correlated non-success, never as a handle that can only fail after pop.
		probe := answerCapabilityResult(readyAnswer{
			childID: turn.child.id, token: turn.token, mode: ModeAnswer, terminal: terminal,
		}, true)
		if _, marshalErr := probe.marshal(); marshalErr != nil {
			terminal = Terminal{Success: false, StopReason: "malformed_completion"}
		}
	}
	d.finishTurn(turn, terminal)
}

func (d *Delegator) finishTurn(turn *turnState, terminal Terminal) {
	d.mu.Lock()
	if turn.terminal {
		d.mu.Unlock()
		return
	}
	turn.terminal = true
	event := ChildEvent{
		ParentSessionID: d.parentID,
		ChildSessionID:  turn.child.id,
		Provider:        turn.child.provider,
		Lifecycle:       "terminal",
		Mode:            turn.mode,
		RequestID:       turn.requestID,
		TurnID:          turn.turnID,
	}
	if turn.created {
		event.NativeReference = turn.child.reference.Bytes()
	}
	d.mu.Unlock()

	// The runtime may not continue with falsely active metadata when the
	// content-free terminal transition is undeliverable. Interrupt this exact
	// Manager tree and expose only a fixed mechanical failure.
	if err := d.events.ChildEvent(d.ctx, event); err != nil {
		d.failTree(&BoundaryError{Stage: "terminal child event", Err: ErrTransport})
		return
	}

	d.mu.Lock()
	if turn.child.active == turn {
		turn.child.active = nil
		turn.child.mode = ModeAnswer
	}
	if turn.mode == ModeAnswer {
		delete(d.turns, turn.token)
		d.answerReady = append(d.answerReady, readyAnswer{
			childID:  turn.child.id,
			token:    turn.token,
			mode:     ModeAnswer,
			terminal: terminal,
		})
		d.notifyAnswersLocked()
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	turn.queue.complete(terminal)
}

func (d *Delegator) nextAnswer(ctx context.Context) (capabilityResult, error) {
	for {
		d.mu.Lock()
		if len(d.answerReady) != 0 {
			ready := d.answerReady[0]
			more := d.answerPending > 1
			result := answerCapabilityResult(ready, more)
			// Marshal while the completion is still owned by answerReady. If a
			// future result-shape change breaks the budget invariant, retrying
			// cannot lose the content or its correlation handle.
			if _, err := result.marshal(); err != nil {
				d.mu.Unlock()
				return capabilityResult{}, err
			}
			copy(d.answerReady, d.answerReady[1:])
			d.answerReady[len(d.answerReady)-1] = readyAnswer{}
			d.answerReady = d.answerReady[:len(d.answerReady)-1]
			d.answerPending--
			d.mu.Unlock()
			return result, nil
		}
		if d.answerPending == 0 {
			d.mu.Unlock()
			return capabilityResult{}, ErrStaleTurn
		}
		changed := d.answerChanged
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return capabilityResult{}, ctx.Err()
		case <-d.ctx.Done():
			return capabilityResult{}, ErrTransport
		case <-changed:
		}
	}
}

func answerCapabilityResult(ready readyAnswer, more bool) capabilityResult {
	result := capabilityResult{
		Operation:      OperationNextAnswer,
		ChildSessionID: ready.childID,
		TurnToken:      ready.token,
		Mode:           ModeAnswer,
		Terminal:       cloneTerminal(&ready.terminal),
		More:           more,
	}
	if more {
		result.setContinuation(OperationNextAnswer, "", "")
	}
	return result
}

func (d *Delegator) followNext(ctx context.Context, childID, token string) (capabilityResult, error) {
	turn, err := d.ownedTurn(childID, token, ModeFollow, true)
	if err != nil {
		return capabilityResult{}, err
	}
	batch, err := turn.queue.next(ctx, d.followBatchFits(OperationFollowNext, childID, token))
	if err != nil {
		return capabilityResult{}, err
	}
	result := capabilityResult{
		Operation:      OperationFollowNext,
		ChildSessionID: childID,
		TurnToken:      token,
		Mode:           ModeFollow,
		Frames:         batch.Frames,
		Terminal:       batch.Terminal,
		More:           batch.More,
	}
	if batch.More {
		result.setContinuation(OperationFollowNext, childID, token)
	} else {
		d.invalidateFollow(turn)
	}
	return result, nil
}

func (d *Delegator) followBatchFits(operation Operation, childID, token string) frameBatchFit {
	return func(batch frameBatch) bool {
		result := capabilityResult{
			Operation:      operation,
			ChildSessionID: childID,
			TurnToken:      token,
			Mode:           ModeFollow,
			Frames:         batch.Frames,
			Terminal:       batch.Terminal,
			More:           batch.More,
		}
		if batch.More {
			result.setContinuation(OperationFollowNext, childID, token)
		}
		_, err := result.marshal()
		return err == nil
	}
}

func (d *Delegator) cancelTurn(ctx context.Context, childID, token string) (capabilityResult, error) {
	turn, err := d.ownedTurn(childID, token, "", false)
	if err != nil {
		return capabilityResult{}, err
	}
	if err := turn.child.port.Cancel(ctx); err != nil {
		return capabilityResult{}, &BoundaryError{Stage: "turn cancellation", Err: ErrTransport}
	}
	result := capabilityResult{
		Operation:      OperationCancel,
		ChildSessionID: childID,
		TurnToken:      token,
		Mode:           turn.mode,
		More:           true,
	}
	if turn.mode == ModeFollow {
		result.setContinuation(OperationFollowNext, childID, token)
	} else {
		result.setContinuation(OperationNextAnswer, "", "")
	}
	return result, nil
}

func (d *Delegator) ownedTurn(childID, token string, mode Mode, allowTerminal bool) (*turnState, error) {
	if childID == "" || token == "" {
		return nil, ErrInvalidOperation
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrTransport
	}
	turn, ok := d.turns[token]
	if !ok || turn.child.id != childID {
		return nil, ErrStaleTurn
	}
	if mode != "" && turn.mode != mode {
		return nil, ErrInvalidOperation
	}
	if turn.terminal && !allowTerminal {
		return nil, ErrStaleTurn
	}
	return turn, nil
}

func (d *Delegator) invalidateFollow(turn *turnState) {
	d.mu.Lock()
	if current, ok := d.turns[turn.token]; ok && current == turn {
		delete(d.turns, turn.token)
	}
	d.mu.Unlock()
}

func (d *Delegator) rollbackTurn(turn *turnState, created bool) {
	d.mu.Lock()
	delete(d.turns, turn.token)
	if turn.mode == ModeAnswer {
		d.answerPending--
	}
	if turn.child.active == turn {
		turn.child.active = nil
		turn.child.mode = ModeAnswer
	}
	if created {
		delete(d.children, turn.child.id)
		turn.child.closed = true
	}
	d.mu.Unlock()
	if created {
		_ = turn.child.port.Close()
	}
}

func (d *Delegator) notifyAnswersLocked() {
	close(d.answerChanged)
	d.answerChanged = make(chan struct{})
}

func (d *Delegator) FollowStats(childID, token string) (QueueStats, error) {
	turn, err := d.ownedTurn(childID, token, ModeFollow, true)
	if err != nil {
		return QueueStats{}, err
	}
	return turn.queue.statsSnapshot(), nil
}

// ChildMode reports only the invocation behavior mechanic needed by runtime
// and tests. It exposes no prompt, frame, answer, or native completion.
func (d *Delegator) ChildMode(childID string) (Mode, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	child, ok := d.children[childID]
	if !ok || child.closed {
		return "", ErrOwnership
	}
	return child.mode, nil
}

// RestoreOwnedChild registers metadata-only ownership after a TaskAI runtime
// restart. It neither opens a provider process nor reads native history. A
// later invocation exact-resumes through ChildResume; Replay can immediately
// ask the conformed native adapter to prove the requested completed range.
func (d *Delegator) RestoreOwnedChild(childID, provider string, reference replay.OpaqueRef) error {
	if childID == "" || len(childID) > MaxCorrelationIDBytes || provider == "" ||
		len(reference.Bytes()) == 0 || len(reference.Bytes()) > MaxNativeReferenceBytes {
		return ErrInvalidOperation
	}
	profile, err := d.registry.Profile(provider)
	if err != nil {
		return err
	}
	if !profile.Capabilities.Subordinate {
		return ErrProviderUnsupported
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrTransport
	}
	if len(d.children) >= MaxChildrenPerManager {
		return ErrCapacity
	}
	if _, exists := d.children[childID]; exists {
		return ErrOwnership
	}
	d.children[childID] = &childState{
		id: childID, provider: provider,
		reference: replay.OpaqueRefFromBytes(reference.Bytes()), mode: ModeAnswer,
	}
	return nil
}

// Done closes on either explicit shutdown or a runtime-fatal boundary. Err is
// non-nil only for the latter and is always content-free.
func (d *Delegator) Done() <-chan struct{} { return d.done }

func (d *Delegator) Err() error {
	if d == nil {
		return &BoundaryError{Stage: "manager delegation", Err: ErrTransport}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fatal
}

func (d *Delegator) failTree(cause error) {
	if cause == nil {
		cause = &BoundaryError{Stage: "manager delegation", Err: ErrTransport}
	}
	d.shutdown(cause)
}

func (d *Delegator) shutdown(cause error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if cause != nil && d.fatal == nil {
		d.fatal = cause
	}
	if d.closed {
		d.mu.Unlock()
		d.cancel()
		d.doneOnce.Do(func() { close(d.done) })
		return
	}
	d.closed = true
	children := make([]ChildPort, 0, len(d.children))
	for _, child := range d.children {
		child.closed = true
		child.active = nil
		child.mode = ModeAnswer
		if child.port != nil {
			children = append(children, child.port)
		}
	}
	queues := make([]*frameQueue, 0, len(d.turns))
	for _, turn := range d.turns {
		if turn.queue != nil {
			queues = append(queues, turn.queue)
		}
	}
	clear(d.turns)
	for index := range d.answerReady {
		d.answerReady[index] = readyAnswer{}
	}
	d.answerReady = nil
	d.answerPending = 0
	d.notifyAnswersLocked()
	d.mu.Unlock()
	d.cancel()
	d.doneOnce.Do(func() { close(d.done) })
	for _, queue := range queues {
		queue.fail(ErrTransport)
	}
	for _, child := range children {
		_ = child.Close()
	}
}

func (d *Delegator) Close() error {
	d.shutdown(nil)
	d.wg.Wait()
	return nil
}

type delegateInput struct {
	Operation      Operation
	Provider       string
	ChildSessionID string
	Prompt         string
	Mode           Mode
	TurnToken      string
	ReplayTargets  []string
	ReplayCount    uint32
}

func parseDelegateInput(raw json.RawMessage) (delegateInput, error) {
	if len(raw) == 0 || len(raw) > acp.MaxFrameBytes || !json.Valid(raw) {
		return delegateInput{}, ErrInvalidOperation
	}
	var head struct {
		Operation Operation `json:"operation"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return delegateInput{}, ErrInvalidOperation
	}
	switch head.Operation {
	case OperationStart:
		var value struct {
			Operation      Operation `json:"operation"`
			Provider       string    `json:"provider"`
			ChildSessionID string    `json:"child_session_id"`
			Prompt         string    `json:"prompt"`
			Mode           Mode      `json:"mode"`
		}
		if strictJSON(raw, &value) != nil || (value.Provider == "") == (value.ChildSessionID == "") ||
			len(value.Provider) > 128 || len(value.ChildSessionID) > MaxCorrelationIDBytes ||
			(value.Mode != ModeAnswer && value.Mode != ModeFollow) {
			return delegateInput{}, ErrInvalidOperation
		}
		return delegateInput{
			Operation:      value.Operation,
			Provider:       value.Provider,
			ChildSessionID: value.ChildSessionID,
			Prompt:         value.Prompt,
			Mode:           value.Mode,
		}, nil
	case OperationNextAnswer:
		var value struct {
			Operation Operation `json:"operation"`
		}
		if strictJSON(raw, &value) != nil {
			return delegateInput{}, ErrInvalidOperation
		}
		return delegateInput{Operation: value.Operation}, nil
	case OperationFollowNext, OperationCancel:
		var value struct {
			Operation      Operation `json:"operation"`
			ChildSessionID string    `json:"child_session_id"`
			TurnToken      string    `json:"turn_token"`
		}
		if strictJSON(raw, &value) != nil || value.ChildSessionID == "" || value.TurnToken == "" ||
			len(value.ChildSessionID) > MaxCorrelationIDBytes || len(value.TurnToken) > MaxCorrelationIDBytes {
			return delegateInput{}, ErrInvalidOperation
		}
		return delegateInput{Operation: value.Operation, ChildSessionID: value.ChildSessionID, TurnToken: value.TurnToken}, nil
	case OperationReplay:
		var value struct {
			Operation       Operation `json:"operation"`
			ChildSessionIDs []string  `json:"child_session_ids"`
			N               *uint32   `json:"n"`
		}
		if strictJSON(raw, &value) != nil || len(value.ChildSessionIDs) == 0 || len(value.ChildSessionIDs) > MaxReplayTargets {
			return delegateInput{}, ErrInvalidOperation
		}
		count := uint32(1)
		if value.N != nil {
			count = *value.N
		}
		if count == 0 || count > MaxReplayCount {
			return delegateInput{}, ErrInvalidOperation
		}
		seen := make(map[string]struct{}, len(value.ChildSessionIDs))
		for _, childID := range value.ChildSessionIDs {
			if childID == "" || len(childID) > MaxCorrelationIDBytes {
				return delegateInput{}, ErrInvalidOperation
			}
			if _, duplicate := seen[childID]; duplicate {
				return delegateInput{}, ErrInvalidOperation
			}
			seen[childID] = struct{}{}
		}
		return delegateInput{
			Operation: value.Operation, ReplayTargets: append([]string(nil), value.ChildSessionIDs...), ReplayCount: count,
		}, nil
	default:
		return delegateInput{}, ErrInvalidOperation
	}
}

func strictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidOperation
	}
	return nil
}

type answerCollector struct {
	chunks       []json.RawMessage
	bytes        int
	messageID    string
	hasMessageID bool
}

func (c *answerCollector) add(raw json.RawMessage) error {
	message, err := acp.ParseMessage(raw)
	if err != nil {
		return err
	}
	if message.Kind() != acp.KindNotification || message.Method != acp.MethodSessionUpdate {
		return nil
	}
	var params struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			MessageID     *string         `json:"messageId"`
		} `json:"update"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		return ErrMalformedCompletion
	}
	if params.Update.SessionUpdate != "agent_message_chunk" {
		return nil
	}
	if len(params.Update.Content) == 0 || !json.Valid(params.Update.Content) {
		return ErrMalformedCompletion
	}
	if params.Update.MessageID != nil {
		if c.hasMessageID && c.messageID != *params.Update.MessageID {
			for index := range c.chunks {
				c.chunks[index] = nil
			}
			c.chunks = c.chunks[:0]
			c.bytes = 0
		}
		c.messageID = *params.Update.MessageID
		c.hasMessageID = true
	}
	if len(c.chunks) >= MaxAnswerChunks || c.bytes+len(params.Update.Content) > MaxAnswerBytes {
		return ErrAnswerLimit
	}
	c.chunks = append(c.chunks, append(json.RawMessage(nil), params.Update.Content...))
	c.bytes += len(params.Update.Content)
	return nil
}

func (c *answerCollector) frames() []json.RawMessage { return cloneFrames(c.chunks) }

var allowedStopReasons = map[string]bool{
	"end_turn":          true,
	"max_tokens":        true,
	"max_turn_requests": true,
	"refusal":           true,
	"cancelled":         true,
}

func classifyTerminal(native json.RawMessage, transportErr error, answer []json.RawMessage) Terminal {
	if transportErr != nil {
		return Terminal{Success: false, StopReason: "transport_failure"}
	}
	if len(native) == 0 || len(native) > acp.MaxFrameBytes || !json.Valid(native) {
		return Terminal{Success: false, StopReason: "malformed_completion"}
	}
	var completion struct {
		StopReason string `json:"stopReason"`
	}
	if json.Unmarshal(native, &completion) != nil || !allowedStopReasons[completion.StopReason] {
		return Terminal{Success: false, StopReason: "malformed_completion"}
	}
	terminal := Terminal{
		Success:    completion.StopReason == "end_turn",
		StopReason: completion.StopReason,
		Native:     append(json.RawMessage(nil), native...),
	}
	if terminal.Success {
		terminal.Answer = cloneFrames(answer)
	}
	return terminal
}

type continuation struct {
	Operation      Operation `json:"operation"`
	ChildSessionID string    `json:"child_session_id,omitempty"`
	TurnToken      string    `json:"turn_token,omitempty"`
}

type capabilityResult struct {
	Operation      Operation
	ChildSessionID string
	TurnToken      string
	Mode           Mode
	Frames         []json.RawMessage
	Terminal       *Terminal
	More           bool
	NextOperation  Operation
	NextArguments  *continuation
	ReplayTargets  []replayResultTarget
}

func (r *capabilityResult) setContinuation(operation Operation, childID, token string) {
	r.NextOperation = operation
	r.NextArguments = &continuation{Operation: operation, ChildSessionID: childID, TurnToken: token}
}

// marshal writes raw ACP frames and public answer content directly into the
// canonical JSON value, preserving their exact bytes instead of allowing
// encoding/json to compact unknown json.RawMessage shapes.
func (r capabilityResult) marshal() (json.RawMessage, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	writeStringField := func(name string, value string) {
		if value == "" {
			return
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(name)
		encoded, _ := json.Marshal(value)
		out.Write(key)
		out.WriteByte(':')
		out.Write(encoded)
	}
	writeRawField := func(name string, raw []byte) {
		if len(raw) == 0 {
			return
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		key, _ := json.Marshal(name)
		out.Write(key)
		out.WriteByte(':')
		out.Write(raw)
	}

	writeStringField("operation", string(r.Operation))
	writeStringField("child_session_id", r.ChildSessionID)
	writeStringField("turn_token", r.TurnToken)
	writeStringField("mode", string(r.Mode))
	if len(r.Frames) != 0 || r.Mode == ModeFollow {
		var frames bytes.Buffer
		frames.WriteByte('[')
		for i, frame := range r.Frames {
			if i != 0 {
				frames.WriteByte(',')
			}
			frames.Write(frame)
		}
		frames.WriteByte(']')
		writeRawField("frames", frames.Bytes())
	}
	if r.Terminal != nil {
		terminal, err := marshalTerminal(*r.Terminal)
		if err != nil {
			return nil, err
		}
		writeRawField("completion", terminal)
	}
	if len(r.ReplayTargets) != 0 {
		replayed, err := marshalReplayTargets(r.ReplayTargets)
		if err != nil {
			return nil, err
		}
		writeRawField("replay", replayed)
	}
	if !first {
		out.WriteByte(',')
	}
	first = false
	out.WriteString(`"more":`)
	if r.More {
		out.WriteString("true")
	} else {
		out.WriteString("false")
	}
	writeStringField("next_operation", string(r.NextOperation))
	if r.NextArguments != nil {
		next, err := json.Marshal(r.NextArguments)
		if err != nil {
			return nil, ErrInvalidOperation
		}
		writeRawField("next_arguments", next)
	}
	out.WriteByte('}')
	if !json.Valid(out.Bytes()) || out.Len() > MaxCapabilityResultBytes ||
		!capabilityResultFitsDynamicTool(out.Bytes()) {
		return nil, ErrCapacity
	}
	return append(json.RawMessage(nil), out.Bytes()...), nil
}

func marshalTerminal(terminal Terminal) ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	out.WriteString(`"success":`)
	if terminal.Success {
		out.WriteString("true")
	} else {
		out.WriteString("false")
	}
	stop, _ := json.Marshal(terminal.StopReason)
	out.WriteString(`,"stopReason":`)
	out.Write(stop)
	if len(terminal.Native) != 0 {
		if !json.Valid(terminal.Native) {
			return nil, ErrMalformedCompletion
		}
		out.WriteString(`,"native":`)
		out.Write(terminal.Native)
	}
	if len(terminal.Answer) != 0 {
		out.WriteString(`,"answer":[`)
		for i, chunk := range terminal.Answer {
			if !json.Valid(chunk) {
				return nil, ErrMalformedCompletion
			}
			if i != 0 {
				out.WriteByte(',')
			}
			out.Write(chunk)
		}
		out.WriteByte(']')
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}
