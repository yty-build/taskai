package service

import (
	"container/heap"
	"context"
	"fmt"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/worker"
)

// TaskActivity is an aggregate view over existing Session history, not a new
// persistent entity. Position remains the owning Session's monotonic cursor.
type TaskActivity struct {
	SessionID string
	Line      sessionlog.Line
}

// ReplayWindow describes the transport-only replay boundary selected for one
// Session. It says nothing about provider memory or conversation semantics.
type ReplayWindow struct {
	Cursor           uint64
	EarliestSequence uint64
	Clipped          bool
}

// ReplayUnavailableError means the Session row exists but its ordered channel
// journal does not. This is distinct from an empty starting Session whose
// worker has not claimed ownership yet, and from a resumable cursor gap within
// an existing retained segment.
type ReplayUnavailableError struct{}

func (*ReplayUnavailableError) Error() string {
	return "Session Channel replay is unavailable"
}

// TaskSessionReplayError identifies which child Session made an aggregate
// Task cursor unusable. It is transport context, not a persistent error model.
type TaskSessionReplayError struct {
	SessionID string
	Err       error
}

func (e *TaskSessionReplayError) Error() string {
	return fmt.Sprintf("replay Session %s in Task stream: %v", e.SessionID, e.Err)
}

func (e *TaskSessionReplayError) Unwrap() error { return e.Err }

// SessionStream is the shared replay/live boundary consumed by CLI, terminal,
// HTTP, and web transports. It keeps durable history and worker transport
// details behind one Task/Session-oriented contract.
type SessionStream interface {
	Replay(context.Context, string, uint64, func(sessionlog.Line) error) (uint64, error)
	Follow(context.Context, string, uint64, func(sessionlog.Line) error) error
	ReplayTask(context.Context, string, map[string]uint64, func(TaskActivity) error) error
	FollowTask(context.Context, string, map[string]uint64, func(TaskActivity) error) error
}

var _ SessionStream = (*SessionService)(nil)

const (
	taskReplayMaxSessions    = 512
	taskReplayMaxOpenReaders = 64
	taskReplayHeadByteBudget = 32 << 20
	taskReplayLineOverhead   = 256
)

type taskReplayItem struct {
	activity      TaskActivity
	cursor        *sessionlog.ReplayCursor
	retainedBytes int
	readerOpen    bool
}

type taskReplayHeap struct {
	items         []taskReplayItem
	retainedBytes int
	openReaders   int
}

func (h taskReplayHeap) Len() int { return len(h.items) }
func (h taskReplayHeap) Less(i, j int) bool {
	left, right := h.items[i].activity, h.items[j].activity
	if left.Line.Timestamp.Equal(right.Line.Timestamp) {
		if left.SessionID == right.SessionID {
			return left.Line.Seq < right.Line.Seq
		}
		return left.SessionID < right.SessionID
	}
	if left.Line.Timestamp.IsZero() {
		return true
	}
	if right.Line.Timestamp.IsZero() {
		return false
	}
	return left.Line.Timestamp.Before(right.Line.Timestamp)
}
func (h taskReplayHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *taskReplayHeap) Push(value any) {
	item := value.(taskReplayItem)
	h.items = append(h.items, item)
	h.retainedBytes += item.retainedBytes
	if item.readerOpen {
		h.openReaders++
	}
}
func (h *taskReplayHeap) Pop() any {
	old := h.items
	last := old[len(old)-1]
	h.items = old[:len(old)-1]
	h.retainedBytes -= last.retainedBytes
	if last.readerOpen {
		h.openReaders--
	}
	return last
}

func (s *SessionService) taskReplayCursors(ctx context.Context, taskID string, cursors map[string]uint64) (map[string]*sessionlog.ReplayCursor, map[string]session.Session, error) {
	sessions, err := s.ListByTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if len(sessions) > taskReplayMaxSessions {
		return nil, nil, fmt.Errorf("Task aggregate replay supports at most %d Sessions; attach individual Sessions instead", taskReplayMaxSessions)
	}
	result := make(map[string]*sessionlog.ReplayCursor, len(sessions))
	byID := make(map[string]session.Session, len(sessions))
	for _, current := range sessions {
		if _, replayErr := s.replayPathAvailable(current, cursors[current.ID]); replayErr != nil {
			return nil, nil, &TaskSessionReplayError{SessionID: current.ID, Err: replayErr}
		}
		// Decode complete records without probing every healthy worker. Only an
		// unterminated suffix needs writer-liveness proof before strict parsing.
		result[current.ID] = sessionlog.NewReplayCursor(s.logPath(current), cursors[current.ID], 0)
		byID[current.ID] = current
	}
	return result, byID, nil
}

func closeTaskReplayCursors(cursors map[string]*sessionlog.ReplayCursor) {
	for _, cursor := range cursors {
		_ = cursor.Close()
	}
}

func taskReplayItemBytes(sessionID string, line sessionlog.Line) int {
	return taskReplayLineOverhead + len(sessionID) + len(line.RequestID) + len(line.Type) + len(line.Data)
}

func pushTaskReplayItem(items *taskReplayHeap, item taskReplayItem) error {
	if item.retainedBytes > taskReplayHeadByteBudget || items.retainedBytes > taskReplayHeadByteBudget-item.retainedBytes {
		return fmt.Errorf("Task aggregate replay exceeds its %d MiB working-set limit; attach individual Sessions instead", taskReplayHeadByteBudget>>20)
	}
	heap.Push(items, item)
	return nil
}

func pushTaskReplayHead(ctx context.Context, items *taskReplayHeap, sessionID string, cursor *sessionlog.ReplayCursor) error {
	line, ok, err := cursor.Next(ctx)
	if err != nil {
		return &TaskSessionReplayError{SessionID: sessionID, Err: err}
	}
	if !ok {
		return nil
	}
	readerOpen := cursor.ReaderActive()
	if readerOpen && items.openReaders >= taskReplayMaxOpenReaders {
		if err := cursor.ReleaseReader(); err != nil {
			return fmt.Errorf("release Session log cursor: %w", err)
		}
		readerOpen = false
	}
	retained := taskReplayItemBytes(sessionID, line)
	return pushTaskReplayItem(items, taskReplayItem{
		activity:      TaskActivity{SessionID: sessionID, Line: line},
		cursor:        cursor,
		retainedBytes: retained,
		readerOpen:    readerOpen,
	})
}

func fillTaskReplayHeap(ctx context.Context, items *taskReplayHeap, cursors map[string]*sessionlog.ReplayCursor) error {
	for sessionID, cursor := range cursors {
		if err := pushTaskReplayHead(ctx, items, sessionID, cursor); err != nil {
			return err
		}
	}
	return nil
}

func drainTaskReplay(ctx context.Context, readers map[string]*sessionlog.ReplayCursor, consume func(TaskActivity) error) error {
	if consume == nil {
		return fmt.Errorf("Task replay consumer is required")
	}
	items := &taskReplayHeap{}
	if err := fillTaskReplayHeap(ctx, items, readers); err != nil {
		return err
	}
	heap.Init(items)
	for items.Len() > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := heap.Pop(items).(taskReplayItem)
		if err := consume(item.activity); err != nil {
			return err
		}
		sessionID, cursor := item.activity.SessionID, item.cursor
		// Release the delivered payload before decoding the next head from this
		// Session; consumers decide independently whether to retain their copy.
		item.activity = TaskActivity{}
		if err := pushTaskReplayHead(ctx, items, sessionID, cursor); err != nil {
			return err
		}
	}
	return nil
}

// makeUnownedTaskTailsStrict switches only cursors with withheld bytes. A
// responsive, exact worker socket proves that the suffix may still be one
// in-progress append; PID/socket fields alone are not ownership proof.
func (s *SessionService) makeUnownedTaskTailsStrict(ctx context.Context, readers map[string]*sessionlog.ReplayCursor, sessions map[string]session.Session) (bool, error) {
	changed := false
	for sessionID, cursor := range readers {
		unread, err := cursor.HasUnreadBytes()
		if err != nil {
			return false, &TaskSessionReplayError{SessionID: sessionID, Err: err}
		}
		if !unread || s.sessionReplayWriterLive(ctx, sessions[sessionID]) {
			continue
		}
		if err := cursor.SetStrictEOF(true); err != nil {
			return false, &TaskSessionReplayError{SessionID: sessionID, Err: err}
		}
		changed = true
	}
	return changed, nil
}

func (s *SessionService) sessionReplayWriterLive(ctx context.Context, current session.Session) bool {
	if s.runtime == nil || current.ID == "" {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	// Replay may have taken its Session snapshot before a concurrent Resume or
	// teardown. Re-read ownership inside the same bounded proof window so an old
	// PID/socket tuple cannot classify the current tail in either direction.
	refreshed, err := s.repository.GetSession(probeContext, current.ID)
	if err != nil || refreshed.WorkerPID <= 0 || refreshed.WorkerSocket == "" || !worker.IsProcessAlive(refreshed.WorkerPID) {
		return false
	}
	return s.requireCurrentWorkerProtocolAt(probeContext, refreshed, refreshed.WorkerSocket, "prove Session replay writer") == nil
}

// ReplayTask performs one bounded, chronological merge over the durable
// history currently available for every child Session. It streams each line
// to the consumer instead of retaining a Task-sized result slice.
func (s *SessionService) ReplayTask(ctx context.Context, taskID string, cursors map[string]uint64, consume func(TaskActivity) error) error {
	readers, sessions, err := s.taskReplayCursors(ctx, taskID, cursors)
	if err != nil {
		return err
	}
	defer closeTaskReplayCursors(readers)
	if err := drainTaskReplay(ctx, readers, consume); err != nil {
		return err
	}
	strict, err := s.makeUnownedTaskTailsStrict(ctx, readers, sessions)
	if err != nil || !strict {
		return err
	}
	return drainTaskReplay(ctx, readers, consume)
}

// TaskReplayWindows resolves missing Task cursor positions to bounded
// per-Session tails for a fresh viewer. Every explicitly present position is
// exact, including zero. The returned cursor map is safe to pass directly to
// FollowTask and encode for a viewer resume.
func (s *SessionService) TaskReplayWindows(ctx context.Context, taskID string, cursors map[string]uint64, maxRecords int) (map[string]uint64, map[string]ReplayWindow, error) {
	return s.taskReplayWindows(ctx, taskID, cursors, false, maxRecords)
}

// TaskReplayWindowsAtMembershipBoundary applies a saved aggregate cursor. A
// Session absent from that cursor was added after its Task-membership boundary
// and therefore resumes exactly at zero instead of selecting a fresh tail.
func (s *SessionService) TaskReplayWindowsAtMembershipBoundary(ctx context.Context, taskID string, cursors map[string]uint64, maxRecords int) (map[string]uint64, map[string]ReplayWindow, error) {
	return s.taskReplayWindows(ctx, taskID, cursors, true, maxRecords)
}

func (s *SessionService) taskReplayWindows(ctx context.Context, taskID string, cursors map[string]uint64, membershipBoundary bool, maxRecords int) (map[string]uint64, map[string]ReplayWindow, error) {
	sessions, err := s.ListByTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	if len(sessions) > taskReplayMaxSessions {
		return nil, nil, fmt.Errorf("Task aggregate replay supports at most %d Sessions; attach individual Sessions instead", taskReplayMaxSessions)
	}
	effective := make(map[string]uint64, len(sessions))
	windows := make(map[string]ReplayWindow, len(sessions))
	for _, current := range sessions {
		position, exact := cursors[current.ID]
		if membershipBoundary && !exact {
			exact = true
			position = 0
		}
		if _, replayErr := s.replayPathAvailable(current, position); replayErr != nil {
			return nil, nil, &TaskSessionReplayError{SessionID: current.ID, Err: replayErr}
		}
		clipped := false
		if !exact {
			position, clipped, err = sessionlog.TailCursor(s.logPath(current), maxRecords)
			if err != nil {
				return nil, nil, &TaskSessionReplayError{SessionID: current.ID, Err: err}
			}
		}
		earliest := position
		if position != ^uint64(0) {
			earliest++
		}
		effective[current.ID] = position
		windows[current.ID] = ReplayWindow{
			Cursor:           position,
			EarliestSequence: earliest,
			Clipped:          clipped,
		}
	}
	return effective, windows, nil
}

// FollowTask polls durable Session history so reconnect, worker restarts, and
// newly added Sessions share one lossless aggregate behavior across clients.
func (s *SessionService) FollowTask(ctx context.Context, taskID string, cursors map[string]uint64, consume func(TaskActivity) error) error {
	readers := make(map[string]*sessionlog.ReplayCursor)
	defer closeTaskReplayCursors(readers)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sessions, err := s.ListByTask(ctx, taskID)
		if err != nil {
			return err
		}
		if len(sessions) > taskReplayMaxSessions {
			return fmt.Errorf("Task aggregate replay supports at most %d Sessions; attach individual Sessions instead", taskReplayMaxSessions)
		}
		byID := make(map[string]session.Session, len(sessions))
		for _, item := range sessions {
			position := cursors[item.ID]
			if _, replayErr := s.replayPathAvailable(item, position); replayErr != nil {
				return &TaskSessionReplayError{SessionID: item.ID, Err: replayErr}
			}
			byID[item.ID] = item
			if readers[item.ID] == nil {
				// Viewer transports resolve their initial bounded windows before
				// calling FollowTask. A Session discovered after that boundary is
				// new to this Task stream and must begin at sequence zero rather
				// than being silently clipped without replay-window metadata.
				readers[item.ID] = sessionlog.NewReplayCursor(s.logPath(item), position, 0)
			}
			// A Session may Resume after an earlier static pass. Begin every poll
			// live-safe, then require exact writer proof only for withheld bytes.
			if err := readers[item.ID].SetStrictEOF(false); err != nil {
				return &TaskSessionReplayError{SessionID: item.ID, Err: err}
			}
		}
		if err := drainTaskReplay(ctx, readers, consume); err != nil {
			return err
		}
		strict, err := s.makeUnownedTaskTailsStrict(ctx, readers, byID)
		if err != nil {
			return err
		}
		if strict {
			if err := drainTaskReplay(ctx, readers, consume); err != nil {
				return err
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
