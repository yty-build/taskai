package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
)

type ReconcileRepository interface {
	ListSessions(context.Context) ([]session.Session, error)
	CompareAndSwapSessionWorker(context.Context, string, session.WorkerState, session.WorkerState, string) (session.Session, bool, error)
}

// ReconcileStale quarantines unresponsive worker ownership. A dead worker PID
// does not prove that the separately-grouped provider process tree exited, so
// this recovery path must never clear the locator or make an exact Resume/new
// checkout writer possible. Verified/manual provider teardown is a separate
// operational action.
func ReconcileStale(ctx context.Context, _ config.Home, repository ReconcileRepository) ([]string, error) {
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
			responsive = NewSocketClient(current.WorkerSocket).Ping(pingContext, current.ID) == nil
			cancel()
		}
		if responsive {
			continue
		}
		// Failed + retained ownership is already the durable quarantine marker.
		if current.Status == session.StatusFailed {
			continue
		}
		failed := current.WorkerState()
		failed.Status = session.StatusFailed
		if _, swapped, err := repository.CompareAndSwapSessionWorker(ctx, current.ID, current.WorkerState(), failed, ""); err != nil {
			return reconciled, fmt.Errorf("reconcile stale Session %s: %w", current.ID, err)
		} else if !swapped {
			// Ownership or status changed after the failed liveness probe. The
			// newer state wins; never quarantine it using stale evidence.
			continue
		}
		reconciled = append(reconciled, current.ID)
	}
	return reconciled, nil
}
