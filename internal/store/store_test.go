package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

var fixedTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	value, err := Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := value.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return value
}

func createTask(t *testing.T, store *Store, id task.ID, title string, created time.Time) task.Task {
	t.Helper()
	value, err := task.New(id, title, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	return value
}

func createManager(t *testing.T, store *Store, id session.ID, taskID task.ID, state session.State, created time.Time) session.Session {
	t.Helper()
	value, err := session.NewManager(id, taskID, string(id), "codex", nil, state, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	return value
}

func createChild(t *testing.T, store *Store, id session.ID, taskID task.ID, parent session.ID, state session.State, created time.Time) session.Session {
	t.Helper()
	value, err := session.NewSubordinate(id, taskID, parent, string(id), "claude", nil, state, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOpenPermissionsPragmasAndClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not available")
	}

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}

	version, err := store.UserVersion(context.Background())
	if err != nil || version != SchemaVersion {
		t.Fatalf("UserVersion = %d, %v", version, err)
	}
	var foreignKeys, busyTimeout int
	if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != int(BusyTimeout.Milliseconds()) {
		t.Fatalf("pragmas: foreign_keys=%d busy_timeout=%d", foreignKeys, busyTimeout)
	}
	var journal string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode = %q, %v", journal, err)
	}

	createTask(t, store, "task_permissions", "permissions", fixedTime)
	for _, path := range []string{stateDir, store.Path(), store.Path() + "-wal", store.Path() + "-shm"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", filepath.Base(path), err)
		}
		want := stateFileMode
		if path == stateDir {
			want = stateDirectoryMode
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode %s = %04o, want %04o", filepath.Base(path), got, want)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := store.GetTask(context.Background(), "task_permissions"); !errors.Is(err, ErrClosed) {
		t.Fatalf("operation after Close error = %v", err)
	}
}

func TestOpenRejectsStateAndDatabaseSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateLink := filepath.Join(root, "state-link")
	if err := os.Symlink(realDir, stateLink); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), stateLink); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("state symlink error = %v", err)
	}

	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(stateDir, DatabaseFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("database symlink error = %v", err)
	}
}

func TestOpenHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, filepath.Join(t.TempDir(), "state"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v, want context.Canceled", err)
	}
}

func TestOpenRejectsRelativeStateDirectoryWithoutCreatingIt(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "relative-state-target")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(workingDirectory, target)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(relative) {
		t.Fatalf("test path unexpectedly absolute: %s", relative)
	}
	if _, err := Open(context.Background(), relative); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("relative state directory error = %v, want ErrUnsafeStatePath", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative state directory was created: %v", err)
	}
}
