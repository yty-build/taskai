package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
)

// SequenceIDs supplies stable, race-safe transient identities.
type SequenceIDs struct {
	mu   sync.Mutex
	next map[string]int
}

func (s *SequenceIDs) Next(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next == nil {
		s.next = make(map[string]int)
	}
	s.next[prefix]++
	return fmt.Sprintf("%s-%02d", prefix, s.next[prefix]), nil
}

type EventRecorder struct {
	mu     sync.Mutex
	Events []connector.ChildEvent
	Notify chan connector.ChildEvent
}

func (r *EventRecorder) ChildEvent(_ context.Context, event connector.ChildEvent) error {
	r.mu.Lock()
	r.Events = append(r.Events, event)
	r.mu.Unlock()
	if r.Notify != nil {
		r.Notify <- event
	}
	return nil
}

func (r *EventRecorder) Snapshot() []connector.ChildEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connector.ChildEvent(nil), r.Events...)
}

type ChildScript struct {
	NativeReference string
	OpaqueReference replay.OpaqueRef
	Frames          []json.RawMessage
	Completion      json.RawMessage
	TransportError  bool
	Begin           <-chan struct{}
	Release         <-chan struct{}
	Started         chan<- struct{}
	PromptSeen      chan<- string
	FrameAttempted  chan<- int
	FrameAccepted   chan<- int
}

type ScriptedFactory struct {
	mu               sync.Mutex
	scripts          []ChildScript
	created          []*ScriptedChild
	newCalls         int
	resumeReferences []replay.OpaqueRef
}

func NewScriptedFactory(scripts ...ChildScript) *ScriptedFactory {
	return &ScriptedFactory{scripts: append([]ChildScript(nil), scripts...)}
}

func (f *ScriptedFactory) New(context.Context) (connector.ChildPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls++
	return f.newChildLocked()
}

// Resume records and consumes the exact opaque reference without consulting
// provider history. The scripted child must expose the same reference or the
// fixture fails closed, which makes Start-versus-Resume tests deterministic.
func (f *ScriptedFactory) Resume(_ context.Context, reference replay.OpaqueRef) (connector.ChildPort, error) {
	f.mu.Lock()
	f.resumeReferences = append(f.resumeReferences, replay.OpaqueRefFromBytes(reference.Bytes()))
	child, err := f.newChildLocked()
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(child.NativeReference().Bytes(), reference.Bytes()) {
		_ = child.Close()
		return nil, connector.ErrOwnership
	}
	return child, nil
}

func (f *ScriptedFactory) newChildLocked() (*ScriptedChild, error) {
	if len(f.scripts) == 0 {
		return nil, errors.New("testkit: no child script")
	}
	script := f.scripts[0]
	f.scripts = f.scripts[1:]
	child := &ScriptedChild{script: script, cancelled: make(chan struct{}), closed: make(chan struct{})}
	f.created = append(f.created, child)
	return child, nil
}

// CallSnapshot reports mechanical factory use without exposing prompts,
// frames, completions, or native-reference text.
func (f *ScriptedFactory) CallSnapshot() (newCalls int, resumeReferences []replay.OpaqueRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	references := make([]replay.OpaqueRef, len(f.resumeReferences))
	for index, reference := range f.resumeReferences {
		references[index] = replay.OpaqueRefFromBytes(reference.Bytes())
	}
	return f.newCalls, references
}

func (f *ScriptedFactory) Created() []*ScriptedChild {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*ScriptedChild(nil), f.created...)
}

type ScriptedChild struct {
	script ChildScript

	cancelOnce sync.Once
	closeOnce  sync.Once
	cancelled  chan struct{}
	closed     chan struct{}
	attempted  atomic.Int64
	accepted   atomic.Int64
}

func (c *ScriptedChild) NativeReference() replay.OpaqueRef {
	if len(c.script.OpaqueReference.Bytes()) != 0 {
		return replay.OpaqueRefFromBytes(c.script.OpaqueReference.Bytes())
	}
	reference, _ := replay.NewCodexReference(c.script.NativeReference)
	return reference
}

func (c *ScriptedChild) Prompt(ctx context.Context, prompt string, onFrame func(json.RawMessage) error) (json.RawMessage, error) {
	if c.script.PromptSeen != nil {
		c.script.PromptSeen <- prompt
	}
	if c.script.Started != nil {
		c.script.Started <- struct{}{}
	}
	if c.script.Begin != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, errors.New("testkit: child closed")
		case <-c.cancelled:
			return json.RawMessage(`{"stopReason":"cancelled"}`), nil
		case <-c.script.Begin:
		}
	}
	for index, frame := range c.script.Frames {
		c.attempted.Add(1)
		if c.script.FrameAttempted != nil {
			c.script.FrameAttempted <- index + 1
		}
		if err := onFrame(append(json.RawMessage(nil), frame...)); err != nil {
			return nil, err
		}
		c.accepted.Add(1)
		if c.script.FrameAccepted != nil {
			c.script.FrameAccepted <- index + 1
		}
	}
	if c.script.Release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, errors.New("testkit: child closed")
		case <-c.cancelled:
			return json.RawMessage(`{"stopReason":"cancelled"}`), nil
		case <-c.script.Release:
		}
	}
	select {
	case <-c.cancelled:
		return json.RawMessage(`{"stopReason":"cancelled"}`), nil
	default:
	}
	if c.script.TransportError {
		return nil, errors.New("testkit: transport failed")
	}
	completion := c.script.Completion
	if len(completion) == 0 {
		completion = json.RawMessage(`{"stopReason":"end_turn"}`)
	}
	return append(json.RawMessage(nil), completion...), nil
}

func (c *ScriptedChild) Cancel(context.Context) error {
	c.cancelOnce.Do(func() { close(c.cancelled) })
	return nil
}

func (c *ScriptedChild) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *ScriptedChild) Counts() (attempted, accepted int) {
	return int(c.attempted.Load()), int(c.accepted.Load())
}

func AgentMessageFrame(sessionID, messageID, text string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"messageId":     messageID,
				"content": map[string]any{
					"type": "text",
					"text": text,
				},
			},
		},
	})
	return value
}

func UnknownUpdateFrame(sessionID string, sequence int) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": sessionID,
			"update": map[string]any{
				"sessionUpdate": "future_public_shape",
				"sequence":      sequence,
				"opaque":        []any{true, nil, sequence},
			},
		},
	})
	return value
}
