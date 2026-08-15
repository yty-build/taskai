package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestFollowTaskReplaysAndDiscoversNewSessions(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-stream", Title: "Aggregate Sessions"}); err != nil {
		t.Fatal(err)
	}
	first := createStreamSession(t, repository, home, "S-first")
	appendStreamLine(t, first.LogPath, "req-first", "first")

	followContext, cancel := context.WithCancel(ctx)
	defer cancel()
	activities := make(chan TaskActivity, 4)
	followDone := make(chan error, 1)
	service := NewSessionService(repository, nil, home, nil)
	go func() {
		followDone <- service.FollowTask(followContext, "T-stream", nil, func(activity TaskActivity) error {
			activities <- activity
			return nil
		})
	}()

	assertStreamActivity(t, activities, first.ID, "req-first")
	second := createStreamSession(t, repository, home, "S-second")
	appendStreamLine(t, second.LogPath, "req-second", "second")
	assertStreamActivity(t, activities, second.ID, "req-second")
	cancel()
	select {
	case err := <-followDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("FollowTask error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FollowTask did not stop after cancellation")
	}
}

func TestTaskHistoryUsesIndependentSessionCursors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-cursors", Title: "Cursor replay"}); err != nil {
		t.Fatal(err)
	}
	first := createStreamSession(t, repository, home, "S-one")
	second := createStreamSession(t, repository, home, "S-two")
	appendStreamLine(t, first.LogPath, "req-1", "one")
	appendStreamLine(t, first.LogPath, "req-2", "two")
	appendStreamLine(t, second.LogPath, "req-3", "three")

	service := NewSessionService(repository, nil, home, nil)
	activities, err := service.TaskHistory(ctx, "T-cursors", map[string]uint64{first.ID: 1, second.ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].SessionID != first.ID || activities[0].Line.Seq != 2 || activities[0].Line.RequestID != "req-2" {
		t.Fatalf("cursor replay = %#v", activities)
	}
}

func createStreamSession(t *testing.T, repository *store.Store, home config.Home, id string) session.Session {
	t.Helper()
	created, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: id, TaskID: taskIDForStreamTest(id), Agent: "codex", Role: "worker", Status: session.StatusStopped,
		LogPath: filepath.Join(home.Logs, id+".jsonl"),
	})
	if err != nil {
		// Both stream tests use one fixed Task ID per set of Session IDs.
		t.Fatal(err)
	}
	return created
}

func taskIDForStreamTest(sessionID string) string {
	if sessionID == "S-first" || sessionID == "S-second" {
		return "T-stream"
	}
	return "T-cursors"
}

func appendStreamLine(t *testing.T, path, requestID, message string) {
	t.Helper()
	log, err := sessionlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(requestID, "message", map[string]string{"message": message}); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertStreamActivity(t *testing.T, activities <-chan TaskActivity, sessionID, requestID string) {
	t.Helper()
	select {
	case activity := <-activities:
		if activity.SessionID != sessionID || activity.Line.RequestID != requestID {
			t.Fatalf("activity = %#v, want Session %s request %s", activity, sessionID, requestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for Session %s activity", sessionID)
	}
}
