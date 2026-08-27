package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type failingSubscribeRuntime struct {
	*channelRuntime
	err error
}

func TestMissingTerminalSessionReplayIsUnavailableAndTaskErrorNamesChild(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-missing", Title: "Missing replay"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-missing", TaskID: "T-missing", Agent: "codex", Status: session.StatusStopped,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-missing"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, nil, home, nil)
	_, replayErr := service.Replay(ctx, created.ID, 0, func(sessionlog.Line) error { return nil })
	var unavailable *ReplayUnavailableError
	if !errors.As(replayErr, &unavailable) {
		t.Fatalf("missing Session Replay() error = %T %v", replayErr, replayErr)
	}
	if _, _, freshErr := service.FreshReplayCursor(ctx, created.ID, 256); !errors.As(freshErr, &unavailable) {
		t.Fatalf("missing FreshReplayCursor() error = %T %v", freshErr, freshErr)
	}
	if followErr := service.Follow(ctx, created.ID, 0, func(sessionlog.Line) error { return nil }); !errors.As(followErr, &unavailable) {
		t.Fatalf("missing Session Follow() error = %T %v", followErr, followErr)
	}
	taskErr := service.ReplayTask(ctx, created.TaskID, nil, func(TaskActivity) error { return nil })
	var childErr *TaskSessionReplayError
	if !errors.As(taskErr, &childErr) || childErr.SessionID != created.ID || !errors.As(taskErr, &unavailable) {
		t.Fatalf("missing Task Replay() error = %T %v", taskErr, taskErr)
	}
	if _, _, windowErr := service.TaskReplayWindows(ctx, created.TaskID, nil, 256); !errors.As(windowErr, &childErr) || childErr.SessionID != created.ID || !errors.As(windowErr, &unavailable) {
		t.Fatalf("missing TaskReplayWindows() error = %T %v", windowErr, windowErr)
	}
}

func TestMissingUnclaimedStartingReplayRemainsEmptyAndFollowRetryable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-starting-missing", Title: "Starting replay"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-starting-missing", TaskID: "T-starting-missing", Agent: "codex", Status: session.StatusStarting,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-starting-missing"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, nil, home, nil)
	last, replayErr := service.Replay(ctx, created.ID, 0, func(sessionlog.Line) error { return nil })
	if replayErr != nil || last != 0 {
		t.Fatalf("starting Replay() = %d, %v", last, replayErr)
	}
	if cursor, clipped, freshErr := service.FreshReplayCursor(ctx, created.ID, 256); freshErr != nil || cursor != 0 || clipped {
		t.Fatalf("starting FreshReplayCursor() = %d, %v, %v", cursor, clipped, freshErr)
	}
	if taskErr := service.ReplayTask(ctx, created.TaskID, nil, func(TaskActivity) error { return nil }); taskErr != nil {
		t.Fatalf("starting Task Replay() error = %v", taskErr)
	}
	if _, _, windowErr := service.TaskReplayWindows(ctx, created.TaskID, nil, 256); windowErr != nil {
		t.Fatalf("starting TaskReplayWindows() error = %v", windowErr)
	}
	followContext, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if followErr := service.Follow(followContext, created.ID, 0, func(sessionlog.Line) error { return nil }); !errors.Is(followErr, context.DeadlineExceeded) {
		t.Fatalf("starting Follow() error = %T %v", followErr, followErr)
	}
}

func (r *failingSubscribeRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return r.err
}

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

func TestTaskReplayNamesStoppedSessionWithInterruptedFinalRecord(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-interrupted", Title: "Interrupted child replay"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-interrupted", TaskID: "T-interrupted", Agent: "codex", Role: "worker", Status: session.StatusFailed,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-interrupted"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	complete, err := json.Marshal(sessionlog.Line{
		Seq: 1, Timestamp: time.Now().UTC(), Type: "provider_event", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	contents := append(append(complete, '\n'), []byte(`{"seq":2,"type":"provider_event"`)...)
	if err := os.WriteFile(created.LogPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, 1<<30, filepath.Join(root, "dead-worker.sock"), created.LogPath, session.StatusFailed); err != nil {
		t.Fatal(err)
	}

	service := NewSessionService(repository, nil, home, nil)
	for name, replay := range map[string]func(context.Context) error{
		"one-shot": func(callContext context.Context) error {
			return service.ReplayTask(callContext, "T-interrupted", nil, func(TaskActivity) error { return nil })
		},
		"follow": func(callContext context.Context) error {
			return service.FollowTask(callContext, "T-interrupted", nil, func(TaskActivity) error { return nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			callContext, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			err := replay(callContext)
			var childErr *TaskSessionReplayError
			if !errors.As(err, &childErr) || childErr.SessionID != created.ID || !strings.Contains(err.Error(), "incomplete record") {
				t.Fatalf("Task replay error = %T %v", err, err)
			}
		})
	}
}

func TestFiniteReplayWithholdsActiveWritersIncompleteSuffix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-live-partial", Title: "Live partial replay"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-live-partial", TaskID: "T-live-partial", Agent: "codex", Role: "worker", Status: session.StatusWorking,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-live-partial"),
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "live-worker.sock")
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, os.Getpid(), socket, created.LogPath, session.StatusWorking); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	complete, err := json.Marshal(sessionlog.Line{
		Seq: 1, Timestamp: time.Now().UTC(), Type: "provider_event", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"seq":2,"type":"provider_event"`)
	if err := os.WriteFile(created.LogPath, append(append(complete, '\n'), partial...), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime := &channelRuntime{
		repo: repository, home: home, sockets: map[string]string{socket: created.ID},
	}
	service := NewSessionService(repository, nil, home, runtime)
	var sessionPositions []uint64
	last, err := service.Replay(ctx, created.ID, 0, func(line sessionlog.Line) error {
		sessionPositions = append(sessionPositions, line.Seq)
		return nil
	})
	if err != nil || last != 1 || !reflect.DeepEqual(sessionPositions, []uint64{1}) {
		t.Fatalf("live Session replay = %v through %d, %v", sessionPositions, last, err)
	}
	var taskPositions []uint64
	if err := service.ReplayTask(ctx, "T-live-partial", nil, func(activity TaskActivity) error {
		taskPositions = append(taskPositions, activity.Line.Seq)
		return nil
	}); err != nil || !reflect.DeepEqual(taskPositions, []uint64{1}) {
		t.Fatalf("live Task replay = %v, %v", taskPositions, err)
	}
	file, err := os.OpenFile(created.LogPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`,"data":{}}` + "\n")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sessionPositions = nil
	last, err = service.Replay(ctx, created.ID, 1, func(line sessionlog.Line) error {
		sessionPositions = append(sessionPositions, line.Seq)
		return nil
	})
	if err != nil || last != 2 || !reflect.DeepEqual(sessionPositions, []uint64{2}) {
		t.Fatalf("completed live Session replay = %v through %d, %v", sessionPositions, last, err)
	}
}

func TestFollowReplaysCompleteSuffixWithoutRejectingLivePartialAppendAfterSubscriberShed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-follow-shed", Title: "Follow shed"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-follow-shed", TaskID: "T-follow-shed", Agent: "codex", Status: session.StatusWorking,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-follow-shed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "live-worker.sock")
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, os.Getpid(), socket, created.LogPath, session.StatusWorking); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	complete, err := json.Marshal(sessionlog.Line{
		Seq: 1, Timestamp: time.Now().UTC(), Type: "provider_event", Data: json.RawMessage(`{"type":"text_delta"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"seq":2,"type":"provider_event"`)
	if err := os.WriteFile(created.LogPath, append(append(complete, '\n'), partial...), 0o600); err != nil {
		t.Fatal(err)
	}
	shed := errors.New("subscriber shed")
	base := &channelRuntime{repo: repository, home: home, sockets: map[string]string{socket: created.ID}}
	service := NewSessionService(repository, nil, home, &failingSubscribeRuntime{channelRuntime: base, err: shed})
	var positions []uint64
	err = service.Follow(ctx, created.ID, 0, func(line sessionlog.Line) error {
		positions = append(positions, line.Seq)
		return nil
	})
	if !errors.Is(err, shed) {
		t.Fatalf("Follow() error = %T %v, want subscriber shed", err, err)
	}
	if !reflect.DeepEqual(positions, []uint64{1}) {
		t.Fatalf("Follow() complete suffix positions = %v, want [1]", positions)
	}
}

func TestReplayWithholdsFailedButExactlyResponsiveWriterTail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-failed-live-tail", Title: "Failed live tail"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-failed-live-tail", TaskID: "T-failed-live-tail", Agent: "codex", Role: "worker", Status: session.StatusFailed,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-failed-live-tail"),
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "failed-live-worker.sock")
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, os.Getpid(), socket, created.LogPath, session.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	complete, err := json.Marshal(sessionlog.Line{
		Seq: 1, Timestamp: time.Now().UTC(), Type: "provider_event", Data: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	partial := []byte(`{"seq":2,"type":"connection"`)
	if err := os.WriteFile(created.LogPath, append(append(complete, '\n'), partial...), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &channelRuntime{repo: repository, home: home, sockets: map[string]string{socket: created.ID}}
	service := NewSessionService(repository, nil, home, runtime)

	freshCursor, clipped, err := service.FreshReplayCursor(ctx, created.ID, 256)
	if err != nil || clipped || freshCursor != 0 {
		t.Fatalf("failed responsive fresh Session window = %d, clipped %v, %v", freshCursor, clipped, err)
	}
	var positions []uint64
	last, err := service.Replay(ctx, created.ID, freshCursor, func(line sessionlog.Line) error {
		positions = append(positions, line.Seq)
		return nil
	})
	if err != nil || last != 1 || !reflect.DeepEqual(positions, []uint64{1}) {
		t.Fatalf("failed responsive Session replay = %v through %d, %v", positions, last, err)
	}
	positions = nil
	taskCursors, windows, err := service.TaskReplayWindows(ctx, created.TaskID, nil, 256)
	if err != nil || windows[created.ID].Clipped || taskCursors[created.ID] != 0 {
		t.Fatalf("failed responsive fresh Task window = %#v, %#v, %v", taskCursors, windows, err)
	}
	if err := service.ReplayTask(ctx, created.TaskID, taskCursors, func(activity TaskActivity) error {
		positions = append(positions, activity.Line.Seq)
		return nil
	}); err != nil || !reflect.DeepEqual(positions, []uint64{1}) {
		t.Fatalf("failed responsive Task replay = %v, %v", positions, err)
	}
	followContext, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	err = service.FollowTask(followContext, created.TaskID, taskCursors, func(TaskActivity) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(fmt.Sprint(err), "incomplete record") {
		t.Fatalf("failed responsive Task follow = %T %v", err, err)
	}
}

func TestTaskReplayStrictlyRejectsPartialTailWithoutExactResponsiveWriter(t *testing.T) {
	for _, test := range []struct {
		name          string
		pid           int
		responseOwner string
	}{
		{name: "zero PID with stale socket", pid: 0},
		{name: "reused live PID with dead socket", pid: os.Getpid()},
		{name: "responsive socket belongs to another Session", pid: os.Getpid(), responseOwner: "S-other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
			repository, err := store.Open(ctx, home.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-stale-tail", Title: "Stale tail"}); err != nil {
				t.Fatal(err)
			}
			created, err := repository.CreateSession(ctx, session.CreateParams{
				ID: "S-stale-tail", TaskID: "T-stale-tail", Agent: "codex", Role: "worker", Status: session.StatusFailed,
				LogPath: sessionlog.ChannelPath(home.Logs, "S-stale-tail"),
			})
			if err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(root, "stale-worker.sock")
			if test.pid == 0 {
				// Model a retained legacy/crash row that has lost its PID but still
				// names a socket. Store writes reject this shape, but replay must
				// fail closed when it encounters one on disk.
				if _, err := repository.DB().ExecContext(ctx, `UPDATE sessions SET worker_pid = NULL, worker_socket = ? WHERE id = ?`, socket, created.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := repository.UpdateSessionWorker(ctx, created.ID, test.pid, socket, created.LogPath, session.StatusFailed); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(created.LogPath), 0o700); err != nil {
				t.Fatal(err)
			}
			complete, err := json.Marshal(sessionlog.Line{
				Seq: 1, Timestamp: time.Now().UTC(), Type: "provider_event", Data: json.RawMessage(`{}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(created.LogPath, append(append(complete, '\n'), []byte(`{"seq":2,"type":"provider_event"`)...), 0o600); err != nil {
				t.Fatal(err)
			}
			sockets := make(map[string]string)
			if test.responseOwner != "" {
				sockets[socket] = test.responseOwner
			}
			runtime := &channelRuntime{repo: repository, home: home, sockets: sockets}
			service := NewSessionService(repository, nil, home, runtime)
			taskCursors, windows, err := service.TaskReplayWindows(ctx, created.TaskID, nil, 256)
			if err != nil || windows[created.ID].Clipped || taskCursors[created.ID] != 0 {
				t.Fatalf("stale partial fresh Task window = %#v, %#v, %v", taskCursors, windows, err)
			}

			for name, replay := range map[string]func(context.Context) error{
				"one-shot": func(callContext context.Context) error {
					return service.ReplayTask(callContext, created.TaskID, taskCursors, func(TaskActivity) error { return nil })
				},
				"follow": func(callContext context.Context) error {
					return service.FollowTask(callContext, created.TaskID, taskCursors, func(TaskActivity) error { return nil })
				},
			} {
				t.Run(name, func(t *testing.T) {
					callContext, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					err := replay(callContext)
					var childErr *TaskSessionReplayError
					if !errors.As(err, &childErr) || childErr.SessionID != created.ID || !strings.Contains(err.Error(), "incomplete record") {
						t.Fatalf("Task replay error = %T %v", err, err)
					}
				})
			}
		})
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
		LogPath: sessionlog.ChannelPath(home.Logs, "S-bounded"),
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
			Type:      "provider_event",
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

func TestSessionChannelNeverReinterpretsLegacyJSONL(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-legacy-log", Title: "Legacy audit"}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(home.Logs, "S-legacy-log.jsonl")
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-legacy-log", TaskID: "T-legacy-log", Agent: "codex", Status: session.StatusStopped, LogPath: legacyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := sessionlog.Line{Seq: 1, Timestamp: time.Now().UTC(), Type: "assistant_chunk", Data: json.RawMessage(`{"text":"legacy"}`)}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home.Logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	channel, err := sessionlog.Open(sessionlog.ChannelPath(home.Logs, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Append("REQ-new", "provider_event", json.RawMessage(`{"type":"text_delta","text":"new"}`)); err != nil {
		t.Fatal(err)
	}
	if err := channel.Close(); err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, nil, home, nil)
	var lines []sessionlog.Line
	_, err = service.Replay(ctx, created.ID, 0, func(line sessionlog.Line) error {
		lines = append(lines, line)
		return nil
	})
	if err != nil || len(lines) != 1 || lines[0].Type != "provider_event" || strings.Contains(string(lines[0].Data), "legacy") {
		t.Fatalf("channel history = %#v, %v", lines, err)
	}
	preserved, err := os.ReadFile(legacyPath)
	if err != nil || !strings.Contains(string(preserved), "legacy") {
		t.Fatalf("legacy audit was not preserved: %q, %v", preserved, err)
	}
}

func TestFollowHonorsResolvedFreshCursorWithoutRetailing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-tail", Title: "Bounded viewer"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-tail", TaskID: "T-tail", Agent: "codex", Status: session.StatusStopped,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-tail"),
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(sessionlog.ChannelPath(home.Logs, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 1000; sequence++ {
		if _, err := log.AppendBuffered("", "provider_event", map[string]int{"index": sequence}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, nil, home, nil)
	cursor, clipped, err := service.FreshReplayCursor(ctx, created.ID, 256)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 744 || !clipped {
		t.Fatalf("fresh replay cursor = %d clipped=%t, want 744 true", cursor, clipped)
	}
	// Frames may arrive after a viewer's declared replay window but before its
	// Follow call. Follow must honor the declared cursor exactly rather than
	// silently choosing a second, later tail and skipping those frames.
	log, err = sessionlog.Open(sessionlog.ChannelPath(home.Logs, created.ID))
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1001; sequence <= 1100; sequence++ {
		if _, err := log.AppendBuffered("", "provider_event", map[string]int{"index": sequence}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	var positions []uint64
	if err := service.Follow(ctx, created.ID, cursor, func(line sessionlog.Line) error {
		positions = append(positions, line.Seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(positions) != 356 || positions[0] != 745 || positions[len(positions)-1] != 1100 {
		t.Fatalf("resolved Follow positions = %d %#v...%#v", len(positions), positions[:min(3, len(positions))], positions[max(0, len(positions)-3):])
	}
}

func TestTaskReplayWindowsDeclareBoundedPerSessionTails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-task-tail", Title: "Bounded Task viewer"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-task-tail", TaskID: "T-task-tail", Agent: "codex", Status: session.StatusStopped,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-task-tail"),
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(created.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 300; sequence++ {
		if _, err := log.AppendBuffered("", "provider_event", map[string]int{"index": sequence}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	service := NewSessionService(repository, nil, home, nil)
	effective, windows, err := service.TaskReplayWindows(ctx, "T-task-tail", nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	window := windows[created.ID]
	if effective[created.ID] != 44 || window.Cursor != 44 || window.EarliestSequence != 45 || !window.Clipped {
		t.Fatalf("fresh Task replay window = %#v, effective=%#v", window, effective)
	}

	effective, windows, err = service.TaskReplayWindowsAtMembershipBoundary(ctx, "T-task-tail", nil, 256)
	if err != nil {
		t.Fatal(err)
	}
	window = windows[created.ID]
	if effective[created.ID] != 0 || window.Cursor != 0 || window.EarliestSequence != 1 || window.Clipped {
		t.Fatalf("post-boundary Task child replay window = %#v, effective=%#v", window, effective)
	}

	effective, windows, err = service.TaskReplayWindows(ctx, "T-task-tail", map[string]uint64{created.ID: 0}, 256)
	if err != nil {
		t.Fatal(err)
	}
	window = windows[created.ID]
	if effective[created.ID] != 0 || window.Cursor != 0 || window.EarliestSequence != 1 || window.Clipped {
		t.Fatalf("exact-zero Task replay window = %#v, effective=%#v", window, effective)
	}

	effective, windows, err = service.TaskReplayWindows(ctx, "T-task-tail", map[string]uint64{created.ID: 120}, 256)
	if err != nil {
		t.Fatal(err)
	}
	window = windows[created.ID]
	if effective[created.ID] != 120 || window.Cursor != 120 || window.EarliestSequence != 121 || window.Clipped {
		t.Fatalf("exact Task replay window = %#v, effective=%#v", window, effective)
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
		LogPath: sessionlog.ChannelPath(home.Logs, id),
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
