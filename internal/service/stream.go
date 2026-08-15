package service

import (
	"context"
	"sort"
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
	Follow(context.Context, string, uint64, func(sessionlog.Line) error) error
	TaskHistory(context.Context, string, map[string]uint64) ([]TaskActivity, error)
	FollowTask(context.Context, string, map[string]uint64, func(TaskActivity) error) error
}

var _ SessionStream = (*SessionService)(nil)

func (s *SessionService) TaskHistory(ctx context.Context, taskID string, cursors map[string]uint64) ([]TaskActivity, error) {
	sessions, err := s.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var result []TaskActivity
	for _, current := range sessions {
		lines, err := s.History(ctx, current.ID, cursors[current.ID])
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			result = append(result, TaskActivity{SessionID: current.ID, Line: line})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := result[i], result[j]
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
	})
	return result, nil
}

// FollowTask polls durable Session history so reconnect, worker restarts, and
// newly added Sessions share one lossless aggregate behavior across clients.
func (s *SessionService) FollowTask(ctx context.Context, taskID string, cursors map[string]uint64, consume func(TaskActivity) error) error {
	current := make(map[string]uint64, len(cursors))
	for sessionID, cursor := range cursors {
		current[sessionID] = cursor
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := s.TaskHistory(ctx, taskID, current)
		if err != nil {
			return err
		}
		for _, activity := range batch {
			if activity.Line.Seq <= current[activity.SessionID] {
				continue
			}
			if err := consume(activity); err != nil {
				return err
			}
			current[activity.SessionID] = activity.Line.Seq
		}
		if len(batch) != 0 {
			continue
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
