package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
)

type ReconcileRepository interface {
	ListSessions(context.Context) ([]session.Session, error)
	ClearSessionWorker(context.Context, string, session.Status) (session.Session, error)
}

// ReconcileStale marks database rows stopped when their detached process and
// private socket no longer describe a responsive worker. It never restarts a
// Session automatically.
func ReconcileStale(ctx context.Context, home config.Home, repository ReconcileRepository) ([]string, error) {
	sessions, err := repository.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	var reconciled []string
	for _, current := range sessions {
		if current.WorkerPID == 0 && current.WorkerSocket == "" {
			continue
		}
		responsive := false
		if current.WorkerPID > 0 && current.WorkerSocket != "" && IsProcessAlive(current.WorkerPID) {
			pingContext, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			responsive = NewSocketClient(current.WorkerSocket).Ping(pingContext) == nil
			cancel()
		}
		if responsive {
			continue
		}
		if _, err := repository.ClearSessionWorker(ctx, current.ID, session.StatusStopped); err != nil {
			return reconciled, fmt.Errorf("reconcile stale Session %s: %w", current.ID, err)
		}
		if current.WorkerSocket != "" {
			_ = os.Remove(current.WorkerSocket)
		}
		_ = os.Remove(filepath.Join(home.Workers, current.ID+".json"))
		reconciled = append(reconciled, current.ID)
	}
	return reconciled, nil
}
