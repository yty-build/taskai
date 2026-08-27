package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

type reconcileRaceRepository struct {
	listed  session.Session
	current session.Session
}

func (r *reconcileRaceRepository) ListSessions(context.Context) ([]session.Session, error) {
	return []session.Session{r.listed}, nil
}

func (r *reconcileRaceRepository) CompareAndSwapSessionWorker(_ context.Context, _ string, before, after session.WorkerState, _ string) (session.Session, bool, error) {
	if r.current.WorkerState() != before {
		return r.current, false, nil
	}
	r.current.Status, r.current.WorkerPID, r.current.WorkerSocket = after.Status, after.PID, after.Socket
	return r.current, true, nil
}

func TestReconcileStaleQuarantinesWithoutClearingOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"),
	}
	for _, directory := range []string{home.Workers, home.Logs} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-stale", Title: "Stale worker"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{ID: "S-stale", TaskID: "T-stale", Agent: "codex", Status: session.StatusWaiting})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home.Workers, created.ID+".sock")
	metadata := filepath.Join(home.Workers, created.ID+".json")
	if err := os.WriteFile(socket, []byte("not a responsive unix socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"session_id":"S-stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, os.Getpid(), socket, filepath.Join(home.Logs, created.ID+".channel-v1.jsonl"), session.StatusWaiting); err != nil {
		t.Fatal(err)
	}

	reconciled, err := ReconcileStale(ctx, home, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 1 || reconciled[0] != created.ID {
		t.Fatalf("reconciled Sessions = %#v", reconciled)
	}
	current, err := repository.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != session.StatusFailed || current.WorkerPID != os.Getpid() || current.WorkerSocket != socket {
		t.Fatalf("stale ownership was not quarantined intact: %#v", current)
	}
	for _, path := range []string{socket, metadata} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("quarantine evidence %q was removed: %v", path, err)
		}
	}

	// Reconciliation is idempotent and never turns a quarantined row into a
	// resumable one merely because its worker remains unresponsive.
	reconciled, err = ReconcileStale(ctx, home, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 0 {
		t.Fatalf("already quarantined Sessions were reported again: %#v", reconciled)
	}
}

func TestReconcileStaleDoesNotQuarantineChangedWorkerOwnership(t *testing.T) {
	stale := session.Session{
		ID: "S-reconcile-race", Status: session.StatusWaiting,
		WorkerPID: 1 << 30, WorkerSocket: "/stale.sock",
	}
	replacement := stale
	replacement.WorkerPID = 4242
	replacement.WorkerSocket = "/replacement.sock"
	repository := &reconcileRaceRepository{listed: stale, current: replacement}

	reconciled, err := ReconcileStale(context.Background(), config.Home{}, repository)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled) != 0 {
		t.Fatalf("changed ownership was reconciled from stale evidence: %#v", reconciled)
	}
	if repository.current.WorkerState() != replacement.WorkerState() {
		t.Fatalf("replacement ownership was mutated: %#v", repository.current)
	}
}
