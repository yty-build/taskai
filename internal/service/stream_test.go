package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestReplayTaskUsesIndependentSessionCursors(t *testing.T) {
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
	var activities []TaskActivity
	err = service.ReplayTask(ctx, "T-cursors", map[string]uint64{first.ID: 1, second.ID: 1}, func(activity TaskActivity) error {
		activities = append(activities, activity)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].SessionID != first.ID || activities[0].Line.Seq != 2 || activities[0].Line.RequestID != "req-2" {
		t.Fatalf("cursor replay = %#v", activities)
	}
}

func TestReplayTaskStreamsLongHistoryWithoutMaterializingIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-bounded", Title: "Bounded replay"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-bounded", TaskID: "T-bounded", Agent: "codex", Role: "worker", Status: session.StatusStopped,
		LogPath: filepath.Join(home.Logs, "S-bounded.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	encoder := json.NewEncoder(&history)
	started := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	const total = 1031
	for sequence := 1; sequence <= total; sequence++ {
		line := sessionlog.Line{
			Seq:       uint64(sequence),
			Timestamp: started.Add(time.Duration(sequence) * time.Millisecond),
			Type:      "message",
			Data:      json.RawMessage(`{}`),
		}
		if err := encoder.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(created.LogPath, history.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewSessionService(repository, nil, home, nil)
	count := 0
	var last uint64
	err = service.ReplayTask(ctx, "T-bounded", nil, func(activity TaskActivity) error {
		count++
		last = activity.Line.Seq
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != total || last != total {
		t.Fatalf("ReplayTask streamed %d lines through position %d", count, last)
	}
}

func TestTaskReplayHeadBudgetFailsClosed(t *testing.T) {
	items := &taskReplayHeap{}
	if err := pushTaskReplayItem(items, taskReplayItem{retainedBytes: taskReplayHeadByteBudget - 1}); err != nil {
		t.Fatal(err)
	}
	err := pushTaskReplayItem(items, taskReplayItem{retainedBytes: 2})
	if err == nil || !strings.Contains(err.Error(), "working-set limit") {
		t.Fatalf("push beyond Task replay byte budget error = %v", err)
	}
	if items.Len() != 1 || items.retainedBytes != taskReplayHeadByteBudget-1 {
		t.Fatalf("rejected head mutated heap = %#v", items)
	}
}

func TestTaskReplayCapsOpenReadersWhileRetainingHeads(t *testing.T) {
	root := t.TempDir()
	line, err := json.Marshal(sessionlog.Line{
		Seq: 1, Timestamp: time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC), Type: "message", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	items := &taskReplayHeap{}
	cursors := make(map[string]*sessionlog.ReplayCursor, taskReplayMaxOpenReaders+1)
	defer closeTaskReplayCursors(cursors)
	for index := 0; index <= taskReplayMaxOpenReaders; index++ {
		sessionID := fmt.Sprintf("S-%03d", index)
		path := filepath.Join(root, sessionID+".jsonl")
		if err := os.WriteFile(path, append(append([]byte(nil), line...), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		cursor := sessionlog.NewReplayCursor(path, 0, 0)
		cursors[sessionID] = cursor
		if err := pushTaskReplayHead(context.Background(), items, sessionID, cursor); err != nil {
			t.Fatal(err)
		}
	}
	active := 0
	for _, cursor := range cursors {
		if cursor.ReaderActive() {
			active++
		}
	}
	if items.Len() != taskReplayMaxOpenReaders+1 || items.openReaders != taskReplayMaxOpenReaders || active != taskReplayMaxOpenReaders {
		t.Fatalf("Task replay heads=%d tracked readers=%d active readers=%d", items.Len(), items.openReaders, active)
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
