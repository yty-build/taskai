package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yty-build/acp-gateway/internal/task"
)

// CreateTask inserts one validated Task metadata record.
func (store *Store) CreateTask(ctx context.Context, value task.Task) error {
	value.CreatedAt = task.CanonicalTime(value.CreatedAt)
	value.UpdatedAt = task.CanonicalTime(value.UpdatedAt)
	if err := value.Validate(); err != nil {
		return err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	_, err := store.db.ExecContext(ctx, `
		INSERT INTO tasks(id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?)`,
		string(value.ID), value.Title, toDatabaseTime(value.CreatedAt), toDatabaseTime(value.UpdatedAt),
	)
	err = classifyWriteError(contextError(ctx, err))
	return store.afterWrite(ctx, err)
}

// GetTask returns one Task metadata record by exact ID.
func (store *Store) GetTask(ctx context.Context, id task.ID) (task.Task, error) {
	if err := id.Validate(); err != nil {
		return task.Task{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return task.Task{}, ErrClosed
	}
	value, err := scanTask(store.db.QueryRowContext(ctx, `
		SELECT id, title, created_at, updated_at
		FROM tasks WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, ErrNotFound
	}
	return value, contextError(ctx, err)
}

// UpdateTaskTitle replaces a Task title exactly and advances its mechanical updated time.
func (store *Store) UpdateTaskTitle(ctx context.Context, id task.ID, title string, updatedAt time.Time) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := task.ValidateTitle(title); err != nil {
		return err
	}
	updatedAt = task.CanonicalTime(updatedAt)
	if updatedAt.IsZero() {
		return task.ErrInvalidTime
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	err := withTx(ctx, store.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE tasks SET title = ?, updated_at = ?
			WHERE id = ? AND created_at <= ? AND updated_at <= ?`,
			title, toDatabaseTime(updatedAt), string(id), toDatabaseTime(updatedAt), toDatabaseTime(updatedAt),
		)
		if err != nil {
			return classifyWriteError(contextError(ctx, err))
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 1 {
			return nil
		}
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, string(id)).Scan(&existing); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return contextError(ctx, err)
		}
		return ErrStaleUpdate
	})
	return store.afterWrite(ctx, err)
}

// DeleteTask removes only TaskAI metadata. Session metadata cascades in SQLite;
// provider-owned processes, files, and conversations are outside this package.
func (store *Store) DeleteTask(ctx context.Context, id task.ID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	result, err := store.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, string(id))
	if err == nil {
		var changed int64
		changed, err = result.RowsAffected()
		if err == nil && changed == 0 {
			err = ErrNotFound
		}
	}
	err = classifyWriteError(contextError(ctx, err))
	return store.afterWrite(ctx, err)
}

// ListTasks returns newest-created Tasks with an ascending ID tie-breaker.
// ViewActive includes Tasks with active Sessions and newly created Tasks with none.
func (store *Store) ListTasks(ctx context.Context, view task.View) ([]task.Summary, error) {
	if err := view.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return nil, ErrClosed
	}
	rows, err := store.db.QueryContext(ctx, `
		WITH task_counts AS (
			SELECT
				t.id,
				t.title,
				t.created_at,
				t.updated_at,
				COUNT(s.id) AS session_count,
				COALESCE(SUM(CASE WHEN s.state IN ('starting', 'active') THEN 1 ELSE 0 END), 0) AS active_count,
				COALESCE(SUM(CASE WHEN s.state IN ('dormant', 'interrupted', 'closed') THEN 1 ELSE 0 END), 0) AS dormant_count
			FROM tasks AS t
			LEFT JOIN sessions AS s ON s.task_id = t.id
			GROUP BY t.id, t.title, t.created_at, t.updated_at
		)
		SELECT id, title, created_at, updated_at, active_count, dormant_count
		FROM task_counts
		WHERE ? = 'all' OR session_count = 0 OR active_count > 0
		ORDER BY created_at DESC, id ASC`, string(view))
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer rows.Close()
	var summaries []task.Summary
	for rows.Next() {
		var (
			id                        string
			title                     string
			createdAt, updatedAt      int64
			activeCount, dormantCount int64
		)
		if err := rows.Scan(&id, &title, &createdAt, &updatedAt, &activeCount, &dormantCount); err != nil {
			return nil, contextError(ctx, err)
		}
		value := task.Task{
			ID:        task.ID(id),
			Title:     title,
			CreatedAt: fromDatabaseTime(createdAt),
			UpdatedAt: fromDatabaseTime(updatedAt),
		}
		counts := task.Counts{Active: activeCount, Dormant: dormantCount}
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("invalid Task metadata in database: %w", err)
		}
		if err := counts.Validate(); err != nil {
			return nil, fmt.Errorf("invalid Task counts in database: %w", err)
		}
		summaries = append(summaries, task.Summary{Task: value, Counts: counts})
	}
	if err := rows.Err(); err != nil {
		return nil, contextError(ctx, err)
	}
	return summaries, nil
}

// TaskCounts returns current active/dormant Session counts for one Task.
func (store *Store) TaskCounts(ctx context.Context, id task.ID) (task.Counts, error) {
	if err := id.Validate(); err != nil {
		return task.Counts{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return task.Counts{}, ErrClosed
	}
	var counts task.Counts
	if err := store.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN s.state IN ('starting', 'active') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN s.state IN ('dormant', 'interrupted', 'closed') THEN 1 ELSE 0 END), 0)
		FROM tasks AS t
		LEFT JOIN sessions AS s ON s.task_id = t.id
		WHERE t.id = ?
		GROUP BY t.id`, string(id)).Scan(&counts.Active, &counts.Dormant); errors.Is(err, sql.ErrNoRows) {
		return task.Counts{}, ErrNotFound
	} else if err != nil {
		return task.Counts{}, contextError(ctx, err)
	}
	return counts, counts.Validate()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (task.Task, error) {
	var (
		id                   string
		title                string
		createdAt, updatedAt int64
	)
	if err := row.Scan(&id, &title, &createdAt, &updatedAt); err != nil {
		return task.Task{}, err
	}
	value := task.Task{
		ID:        task.ID(id),
		Title:     title,
		CreatedAt: fromDatabaseTime(createdAt),
		UpdatedAt: fromDatabaseTime(updatedAt),
	}
	if err := value.Validate(); err != nil {
		return task.Task{}, fmt.Errorf("invalid Task metadata in database: %w", err)
	}
	return value, nil
}

func toDatabaseTime(value time.Time) int64 {
	return task.CanonicalTime(value).UnixMicro()
}

func fromDatabaseTime(value int64) time.Time {
	return time.UnixMicro(value).UTC()
}
