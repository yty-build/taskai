package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

// CreateSession inserts one Manager or same-Task depth-one subordinate atomically.
func (store *Store) CreateSession(ctx context.Context, value session.Session) error {
	value = value.Clone()
	value.CreatedAt = task.CanonicalTime(value.CreatedAt)
	value.LastActiveAt = task.CanonicalTime(value.LastActiveAt)
	if err := value.Validate(); err != nil {
		return err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	err := withTx(ctx, store.db, func(tx *sql.Tx) error {
		var taskExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, string(value.TaskID)).Scan(&taskExists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return contextError(ctx, err)
		}
		if value.Role == session.RoleSubordinate {
			var (
				parentTaskID string
				parentRole   string
				grandparent  sql.NullString
			)
			if err := tx.QueryRowContext(ctx, `
				SELECT task_id, role, parent_session_id
				FROM sessions WHERE id = ?`, string(*value.ParentID)).Scan(&parentTaskID, &parentRole, &grandparent); errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidHierarchy
			} else if err != nil {
				return contextError(ctx, err)
			}
			if task.ID(parentTaskID) != value.TaskID || session.Role(parentRole) != session.RoleManager || grandparent.Valid {
				return ErrInvalidHierarchy
			}
		}
		var parent any
		if value.ParentID != nil {
			parent = string(*value.ParentID)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions(
				id, task_id, parent_session_id, role, label, provider_id,
				provider_reference, state, created_at, last_active_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(value.ID), string(value.TaskID), parent, string(value.Role), value.Label,
			value.ProviderID, nullableBlob(value.ProviderReference), string(value.State),
			toDatabaseTime(value.CreatedAt), toDatabaseTime(value.LastActiveAt),
		)
		return classifyWriteError(contextError(ctx, err))
	})
	return store.afterWrite(ctx, err)
}

// GetSession returns one Session metadata record by exact ID.
func (store *Store) GetSession(ctx context.Context, id session.ID) (session.Session, error) {
	if err := id.Validate(); err != nil {
		return session.Session{}, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return session.Session{}, ErrClosed
	}
	value, err := scanSession(store.db.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, ErrNotFound
	}
	return value, contextError(ctx, err)
}

// ListSessions returns each manager followed by its children, with deterministic
// creation-time then ID ordering at both hierarchy levels.
func (store *Store) ListSessions(ctx context.Context, taskID task.ID) ([]session.Session, error) {
	if err := taskID.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return nil, ErrClosed
	}
	var taskExists int
	if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, string(taskID)).Scan(&taskExists); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, contextError(ctx, err)
	}
	rows, err := store.db.QueryContext(ctx, sessionSelect+`
		WHERE task_id = ? ORDER BY created_at ASC, id ASC`, string(taskID))
	if err != nil {
		return nil, contextError(ctx, err)
	}
	defer rows.Close()
	var managers []session.Session
	children := make(map[session.ID][]session.Session)
	childCount := 0
	for rows.Next() {
		value, err := scanSession(rows)
		if err != nil {
			return nil, contextError(ctx, err)
		}
		if value.Role == session.RoleManager {
			managers = append(managers, value)
			continue
		}
		children[*value.ParentID] = append(children[*value.ParentID], value)
		childCount++
	}
	if err := rows.Err(); err != nil {
		return nil, contextError(ctx, err)
	}
	result := make([]session.Session, 0, len(managers)+childCount)
	seenChildren := 0
	for _, manager := range managers {
		result = append(result, manager)
		owned := children[manager.ID]
		result = append(result, owned...)
		seenChildren += len(owned)
	}
	if seenChildren != childCount {
		return nil, ErrInvalidHierarchy
	}
	return result, nil
}

// UpdateSessionState records a finite mechanical state with monotonic last-active time.
func (store *Store) UpdateSessionState(ctx context.Context, id session.ID, state session.State, lastActiveAt time.Time) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := state.Validate(); err != nil {
		return err
	}
	lastActiveAt = task.CanonicalTime(lastActiveAt)
	if lastActiveAt.IsZero() {
		return session.ErrInvalidTime
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	err := withTx(ctx, store.db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE sessions SET state = ?, last_active_at = ?
			WHERE id = ? AND created_at <= ? AND last_active_at <= ?`,
			string(state), toDatabaseTime(lastActiveAt), string(id),
			toDatabaseTime(lastActiveAt), toDatabaseTime(lastActiveAt),
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
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, string(id)).Scan(&existing); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return contextError(ctx, err)
		}
		return ErrStaleUpdate
	})
	return store.afterWrite(ctx, err)
}

// UpdateOwnedTreeState atomically applies one finite mechanical state to a
// top-level manager and only its direct same-Task subordinate Sessions. If the
// timestamp is stale for any owned Session, the entire tree update rolls back.
func (store *Store) UpdateOwnedTreeState(ctx context.Context, managerID session.ID, state session.State, lastActiveAt time.Time) error {
	if err := managerID.Validate(); err != nil {
		return err
	}
	if err := state.Validate(); err != nil {
		return err
	}
	lastActiveAt = task.CanonicalTime(lastActiveAt)
	if lastActiveAt.IsZero() {
		return session.ErrInvalidTime
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	err := withTx(ctx, store.db, func(tx *sql.Tx) error {
		var (
			taskID string
			role   string
			parent sql.NullString
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT task_id, role, parent_session_id
			FROM sessions WHERE id = ?`, string(managerID)).Scan(&taskID, &role, &parent); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return contextError(ctx, err)
		}
		if session.Role(role) != session.RoleManager || parent.Valid {
			return ErrInvalidHierarchy
		}

		var expected int64
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM sessions
			WHERE (id = ? AND task_id = ? AND role = 'manager' AND parent_session_id IS NULL)
				OR (task_id = ? AND parent_session_id = ? AND role = 'subordinate')`,
			string(managerID), taskID, taskID, string(managerID),
		).Scan(&expected); err != nil {
			return contextError(ctx, err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE sessions SET state = ?, last_active_at = ?
			WHERE (
				(id = ? AND task_id = ? AND role = 'manager' AND parent_session_id IS NULL)
				OR (task_id = ? AND parent_session_id = ? AND role = 'subordinate')
			) AND created_at <= ? AND last_active_at <= ?`,
			string(state), toDatabaseTime(lastActiveAt),
			string(managerID), taskID, taskID, string(managerID),
			toDatabaseTime(lastActiveAt), toDatabaseTime(lastActiveAt),
		)
		if err != nil {
			return classifyWriteError(contextError(ctx, err))
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return contextError(ctx, err)
		}
		if changed != expected {
			return ErrStaleUpdate
		}
		return nil
	})
	return store.afterWrite(ctx, err)
}

// SetProviderReference sets an initially absent opaque BLOB or accepts an exact idempotent repeat.
func (store *Store) SetProviderReference(ctx context.Context, id session.ID, reference []byte, lastActiveAt time.Time) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if len(reference) == 0 {
		return session.ErrInvalidProviderReference
	}
	if err := session.ValidateProviderReference(reference); err != nil {
		return err
	}
	lastActiveAt = task.CanonicalTime(lastActiveAt)
	if lastActiveAt.IsZero() {
		return session.ErrInvalidTime
	}
	reference = bytes.Clone(reference)
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	err := withTx(ctx, store.db, func(tx *sql.Tx) error {
		var (
			existing              []byte
			createdAt, lastActive int64
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT provider_reference, created_at, last_active_at
			FROM sessions WHERE id = ?`, string(id)).Scan(&existing, &createdAt, &lastActive); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return contextError(ctx, err)
		}
		if len(existing) != 0 && !bytes.Equal(existing, reference) {
			return ErrProviderReferenceImmutable
		}
		encodedTime := toDatabaseTime(lastActiveAt)
		if encodedTime < createdAt || encodedTime < lastActive {
			return ErrStaleUpdate
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET provider_reference = ?, last_active_at = ?
			WHERE id = ?`, reference, encodedTime, string(id))
		return classifyWriteError(contextError(ctx, err))
	})
	return store.afterWrite(ctx, err)
}

// DeleteSession removes only TaskAI metadata. Deleting a manager cascades its
// subordinate metadata but has no provider-state callback or filesystem action.
func (store *Store) DeleteSession(ctx context.Context, id session.ID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return ErrClosed
	}
	result, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, string(id))
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

const sessionSelect = `
	SELECT id, task_id, parent_session_id, role, label, provider_id,
		provider_reference, state, created_at, last_active_at
	FROM sessions`

func scanSession(row rowScanner) (session.Session, error) {
	var (
		id, taskID, role, label, providerID, state string
		parent                                     sql.NullString
		providerReference                          []byte
		createdAt, lastActiveAt                    int64
	)
	if err := row.Scan(
		&id, &taskID, &parent, &role, &label, &providerID,
		&providerReference, &state, &createdAt, &lastActiveAt,
	); err != nil {
		return session.Session{}, err
	}
	value := session.Session{
		ID:                session.ID(id),
		TaskID:            task.ID(taskID),
		Role:              session.Role(role),
		Label:             label,
		ProviderID:        providerID,
		ProviderReference: bytes.Clone(providerReference),
		State:             session.State(state),
		CreatedAt:         fromDatabaseTime(createdAt),
		LastActiveAt:      fromDatabaseTime(lastActiveAt),
	}
	if parent.Valid {
		parentID := session.ID(parent.String)
		value.ParentID = &parentID
	}
	if err := value.Validate(); err != nil {
		return session.Session{}, fmt.Errorf("invalid Session metadata in database: %w", err)
	}
	return value, nil
}

func nullableBlob(reference []byte) any {
	if len(reference) == 0 {
		return nil
	}
	return bytes.Clone(reference)
}
