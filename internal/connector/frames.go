package connector

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/yty-build/acp-gateway/internal/acp"
)

const (
	FollowHighWaterFrames = 16
	FollowHighWaterBytes  = 256 << 10
	FollowBatchFrames     = 8
)

type QueueStats struct {
	PeakFrames int
	PeakBytes  int
	Pauses     int
	Resumes    int
}

type Terminal struct {
	Success    bool              `json:"success"`
	StopReason string            `json:"stopReason"`
	Native     json.RawMessage   `json:"native,omitempty"`
	Answer     []json.RawMessage `json:"answer,omitempty"`
}

type frameBatch struct {
	Frames   []json.RawMessage
	Terminal *Terminal
	More     bool
}

type frameBatchFit func(frameBatch) bool

// frameQueue is a connector-owned bounded RAM channel. Push blocks at either
// high-water mark. Because an ACP notification handler calls Push directly,
// this wait pauses the single raw stdout reader and leaves only the bounded
// kernel pipe behind it.
type frameQueue struct {
	mu sync.Mutex

	frames []json.RawMessage
	bytes  int
	stats  QueueStats

	terminal          *Terminal
	terminalDelivered bool
	failure           error
	changed           chan struct{}
}

func newFrameQueue() *frameQueue {
	return &frameQueue{changed: make(chan struct{})}
}

func (q *frameQueue) push(ctx context.Context, raw json.RawMessage) error {
	if _, err := acp.ParseMessage(raw); err != nil {
		return err
	}
	copyOfFrame := append(json.RawMessage(nil), raw...)
	paused := false
	for {
		q.mu.Lock()
		if q.failure != nil {
			err := q.failure
			q.mu.Unlock()
			return err
		}
		if q.terminal != nil {
			q.mu.Unlock()
			return ErrStaleTurn
		}
		fits := len(q.frames) < FollowHighWaterFrames && q.bytes+len(copyOfFrame) <= FollowHighWaterBytes
		if fits {
			q.frames = append(q.frames, copyOfFrame)
			q.bytes += len(copyOfFrame)
			if len(q.frames) > q.stats.PeakFrames {
				q.stats.PeakFrames = len(q.frames)
			}
			if q.bytes > q.stats.PeakBytes {
				q.stats.PeakBytes = q.bytes
			}
			if paused {
				q.stats.Resumes++
			}
			q.notifyLocked()
			q.mu.Unlock()
			return nil
		}
		if !paused {
			paused = true
			q.stats.Pauses++
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (q *frameQueue) complete(terminal Terminal) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.terminal != nil || q.failure != nil {
		return
	}
	copyOfTerminal := terminal
	copyOfTerminal.Native = append(json.RawMessage(nil), terminal.Native...)
	copyOfTerminal.Answer = cloneFrames(terminal.Answer)
	q.terminal = &copyOfTerminal
	q.notifyLocked()
}

func (q *frameQueue) fail(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.terminalDelivered || q.failure != nil {
		return
	}
	if err == nil {
		err = ErrTransport
	}
	q.failure = err
	for index := range q.frames {
		q.frames[index] = nil
	}
	q.frames = nil
	q.bytes = 0
	q.terminal = nil
	q.notifyLocked()
}

func (q *frameQueue) next(ctx context.Context, fits frameBatchFit) (frameBatch, error) {
	for {
		q.mu.Lock()
		batch, ready, err := q.takeLocked(fits)
		if ready || err != nil {
			q.mu.Unlock()
			return batch, err
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return frameBatch{}, ctx.Err()
		case <-changed:
		}
	}
}

func (q *frameQueue) tryNext(fits frameBatchFit) (frameBatch, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	batch, ready, err := q.takeLocked(fits)
	return batch, ready, err
}

// takeLocked selects the largest ordered prefix whose complete encoded
// capability result fits. It does not remove frames or mark a terminal as
// delivered until the caller-provided budget accepts that exact batch.
func (q *frameQueue) takeLocked(fits frameBatchFit) (frameBatch, bool, error) {
	if q.failure != nil {
		return frameBatch{}, false, q.failure
	}
	if q.terminalDelivered {
		return frameBatch{}, false, ErrStaleTurn
	}
	if len(q.frames) == 0 {
		if q.terminal == nil {
			return frameBatch{}, false, nil
		}
		candidate := frameBatch{Terminal: q.terminal, More: false}
		if fits != nil && !fits(candidate) {
			return frameBatch{}, false, ErrCapacity
		}
		q.terminalDelivered = true
		return frameBatch{Terminal: cloneTerminal(q.terminal), More: false}, true, nil
	}

	maximum := min(FollowBatchFrames, len(q.frames))
	for count := maximum; count > 0; count-- {
		consumesAll := count == len(q.frames)
		if consumesAll && q.terminal != nil {
			candidate := frameBatch{Frames: q.frames[:count], Terminal: q.terminal, More: false}
			if fits == nil || fits(candidate) {
				return q.commitLocked(count, true), true, nil
			}
			// A terminal may be delivered in the next result if combining it
			// with the final frame would exceed the encoded result budget.
			candidate = frameBatch{Frames: q.frames[:count], More: true}
			if fits == nil || fits(candidate) {
				return q.commitLocked(count, false), true, nil
			}
			continue
		}
		candidate := frameBatch{Frames: q.frames[:count], More: true}
		if fits == nil || fits(candidate) {
			return q.commitLocked(count, false), true, nil
		}
	}
	return frameBatch{}, false, ErrCapacity
}

func (q *frameQueue) commitLocked(count int, deliverTerminal bool) frameBatch {
	frames := cloneFrames(q.frames[:count])
	for _, frame := range q.frames[:count] {
		q.bytes -= len(frame)
	}
	copy(q.frames, q.frames[count:])
	for i := len(q.frames) - count; i < len(q.frames); i++ {
		q.frames[i] = nil
	}
	q.frames = q.frames[:len(q.frames)-count]

	batch := frameBatch{Frames: frames, More: true}
	if deliverTerminal {
		q.terminalDelivered = true
		batch.Terminal = cloneTerminal(q.terminal)
		batch.More = false
	}
	q.notifyLocked()
	return batch
}

func (q *frameQueue) statsSnapshot() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stats
}

func (q *frameQueue) notifyLocked() {
	close(q.changed)
	q.changed = make(chan struct{})
}

func cloneFrames(frames []json.RawMessage) []json.RawMessage {
	if len(frames) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, len(frames))
	for i, frame := range frames {
		cloned[i] = append(json.RawMessage(nil), frame...)
	}
	return cloned
}

func cloneTerminal(terminal *Terminal) *Terminal {
	if terminal == nil {
		return nil
	}
	copyOfTerminal := *terminal
	copyOfTerminal.Native = append(json.RawMessage(nil), terminal.Native...)
	copyOfTerminal.Answer = cloneFrames(terminal.Answer)
	return &copyOfTerminal
}
