package service

import (
	"container/heap"
	"context"
	"fmt"
	"time"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

// TaskActivity is an aggregate view over existing Session history, not a new
// persistent entity. Position remains the owning Session's monotonic cursor.
type TaskActivity struct {
	SessionID string
	Line      sessionlog.Line
}

// SessionStream is the shared replay/live boundary consumed by CLI, terminal,
// HTTP, and web transports. It keeps durable history and worker transport
// details behind one Task/Session-oriented contract.
type SessionStream interface {
	History(context.Context, string, uint64) ([]sessionlog.Line, error)
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

func (s *SessionService) taskReplayCursors(ctx context.Context, taskID string, cursors map[string]uint64) (map[string]*sessionlog.ReplayCursor, error) {
	sessions, err := s.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if len(sessions) > taskReplayMaxSessions {
		return nil, fmt.Errorf("Task aggregate replay supports at most %d Sessions; attach individual Sessions instead", taskReplayMaxSessions)
	}
	result := make(map[string]*sessionlog.ReplayCursor, len(sessions))
	for _, current := range sessions {
		result[current.ID] = sessionlog.NewReplayCursor(s.logPath(current), cursors[current.ID], 0)
	}
	return result, nil
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
		return err
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

// ReplayTask performs one bounded, chronological merge over the durable
// history currently available for every child Session. It streams each line
// to the consumer instead of retaining a Task-sized result slice.
func (s *SessionService) ReplayTask(ctx context.Context, taskID string, cursors map[string]uint64, consume func(TaskActivity) error) error {
	readers, err := s.taskReplayCursors(ctx, taskID, cursors)
	if err != nil {
		return err
	}
	defer closeTaskReplayCursors(readers)
	return drainTaskReplay(ctx, readers, consume)
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
		for _, item := range sessions {
			if readers[item.ID] == nil {
				readers[item.ID] = sessionlog.NewReplayCursor(s.logPath(item), cursors[item.ID], 0)
			}
		}
		if err := drainTaskReplay(ctx, readers, consume); err != nil {
			return err
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
