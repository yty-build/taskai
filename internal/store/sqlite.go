// Package store persists Tasks and Sessions in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/identity"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
	_ "modernc.org/sqlite"
)

const currentSchemaVersion = 6

var ErrNotFound = errors.New("not found")

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	u := url.URL{Scheme: "file", Path: absPath}
	query := u.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)

	store := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect SQLite: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(absPath, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure database: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		migrationVersion, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		if migrationVersion <= version {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("apply migration %q: %w", entry.Name(), err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", migrationVersion)); err != nil {
			return fmt.Errorf("record migration %q: %w", entry.Name(), err)
		}
		version = migrationVersion
	}
	if version != currentSchemaVersion {
		return fmt.Errorf("database schema version %d does not match expected version %d", version, currentSchemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func (s *Store) CreateTask(ctx context.Context, params task.CreateParams) (task.Task, error) {
	if err := params.Validate(); err != nil {
		return task.Task{}, err
	}
	if params.Status == "" {
		params.Status = task.StatusOpen
	}
	if params.ID == "" {
		id, err := identity.New("T")
		if err != nil {
			return task.Task{}, err
		}
		params.ID = id
	}
	now := s.now().UTC()
	result := task.Task{
		ID: params.ID, Title: strings.TrimSpace(params.Title), Description: params.Description,
		Status: params.Status, ExternalRef: params.ExternalRef, MetadataJSON: cloneJSON(params.MetadataJSON),
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks
		(id, title, description, status, external_ref, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, result.ID, result.Title, nullString(result.Description), string(result.Status),
		nullString(result.ExternalRef), nullJSON(result.MetadataJSON), formatTime(result.CreatedAt), formatTime(result.UpdatedAt))
	if err != nil {
		return task.Task{}, fmt.Errorf("create Task %s: %w", result.ID, err)
	}
	return result, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (task.Task, error) {
	result, err := scanTask(s.db.QueryRowContext(ctx, taskSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, fmt.Errorf("Task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return task.Task{}, fmt.Errorf("get Task %s: %w", id, err)
	}
	return result, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]task.Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelect+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list Tasks: %w", err)
	}
	defer rows.Close()
	var results []task.Task
	for rows.Next() {
		result, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list Tasks: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Tasks: %w", err)
	}
	return results, nil
}

func (s *Store) UpdateTaskStatus(ctx context.Context, id string, status task.Status) (task.Task, error) {
	if err := status.Validate(); err != nil {
		return task.Task{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), formatTime(s.now().UTC()), id)
	if err != nil {
		return task.Task{}, fmt.Errorf("update Task %s: %w", id, err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return task.Task{}, fmt.Errorf("update Task %s: %w", id, err)
	} else if count == 0 {
		return task.Task{}, fmt.Errorf("Task %s: %w", id, ErrNotFound)
	}
	return s.GetTask(ctx, id)
}

func (s *Store) CreateSession(ctx context.Context, params session.CreateParams) (session.Session, error) {
	if err := params.Validate(); err != nil {
		return session.Session{}, err
	}
	if params.ParentSessionID != "" {
		parent, err := s.GetSession(ctx, params.ParentSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("load parent Session %s: %w", params.ParentSessionID, err)
		}
		if parent.TaskID != params.TaskID {
			return session.Session{}, fmt.Errorf("parent Session %s belongs to a different Task", params.ParentSessionID)
		}
	}
	if params.Status == "" {
		params.Status = session.StatusStarting
	}
	if params.ID == "" {
		id, err := identity.New("S")
		if err != nil {
			return session.Session{}, err
		}
		params.ID = id
	}
	now := s.now().UTC()
	result := session.Session{
		ID: params.ID, TaskID: params.TaskID, ParentSessionID: params.ParentSessionID,
		Role: params.Role, Agent: strings.TrimSpace(params.Agent), Provider: params.Provider,
		RepoPath: params.RepoPath, WorktreePath: params.WorktreePath, Branch: params.Branch,
		RuntimeRef: params.RuntimeRef, ProviderRef: params.ProviderRef, Status: params.Status,
		WorkerPID: params.WorkerPID, WorkerSocket: params.WorkerSocket, LogPath: params.LogPath,
		MetadataJSON: cloneJSON(params.MetadataJSON), CreatedAt: now, UpdatedAt: now,
	}
	if err := insertSession(ctx, s.db, result); err != nil {
		return session.Session{}, fmt.Errorf("create Session %s: %w", result.ID, err)
	}
	return result, nil
}

// CreateTaskWithSession atomically persists the convenience one-Session Task.
// A new Task has no existing Session that its first Session could name as a
// parent.
func (s *Store) CreateTaskWithSession(ctx context.Context, taskParams task.CreateParams, sessionParams session.CreateParams) (task.Task, session.Session, error) {
	if taskParams.ID == "" {
		id, err := identity.New("T")
		if err != nil {
			return task.Task{}, session.Session{}, err
		}
		taskParams.ID = id
	}
	sessionParams.TaskID = taskParams.ID
	if sessionParams.ID == "" {
		id, err := identity.New("S")
		if err != nil {
			return task.Task{}, session.Session{}, err
		}
		sessionParams.ID = id
	}
	if err := taskParams.Validate(); err != nil {
		return task.Task{}, session.Session{}, err
	}
	if err := sessionParams.Validate(); err != nil {
		return task.Task{}, session.Session{}, err
	}
	if sessionParams.ParentSessionID != "" {
		return task.Task{}, session.Session{}, fmt.Errorf("the first Session in a new Task cannot name a parent Session")
	}
	if taskParams.Status == "" {
		taskParams.Status = task.StatusOpen
	}
	if sessionParams.Status == "" {
		sessionParams.Status = session.StatusStarting
	}
	now := s.now().UTC()
	createdTask := task.Task{
		ID: taskParams.ID, Title: strings.TrimSpace(taskParams.Title), Description: taskParams.Description,
		Status: taskParams.Status, ExternalRef: taskParams.ExternalRef,
		MetadataJSON: cloneJSON(taskParams.MetadataJSON), CreatedAt: now, UpdatedAt: now,
	}
	createdSession := session.Session{
		ID: sessionParams.ID, TaskID: taskParams.ID, Role: sessionParams.Role,
		Agent: strings.TrimSpace(sessionParams.Agent), Provider: sessionParams.Provider,
		RepoPath: sessionParams.RepoPath, WorktreePath: sessionParams.WorktreePath, Branch: sessionParams.Branch,
		RuntimeRef: sessionParams.RuntimeRef, ProviderRef: sessionParams.ProviderRef, Status: sessionParams.Status,
		WorkerPID: sessionParams.WorkerPID, WorkerSocket: sessionParams.WorkerSocket, LogPath: sessionParams.LogPath,
		MetadataJSON: cloneJSON(sessionParams.MetadataJSON), CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("begin Task creation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
		(id, title, description, status, external_ref, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, createdTask.ID, createdTask.Title, nullString(createdTask.Description),
		string(createdTask.Status), nullString(createdTask.ExternalRef), nullJSON(createdTask.MetadataJSON), formatTime(now), formatTime(now)); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("create Task %s: %w", createdTask.ID, err)
	}
	if err := insertSession(ctx, tx, createdSession); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("create Session %s: %w", createdSession.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("commit Task creation: %w", err)
	}
	return createdTask, createdSession, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (session.Session, error) {
	result, err := scanSession(s.db.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("get Session %s: %w", id, err)
	}
	return result, nil
}

func (s *Store) ListSessionsByTask(ctx context.Context, taskID string) ([]session.Session, error) {
	return s.listSessions(ctx, sessionSelect+` WHERE task_id = ? ORDER BY created_at, id`, taskID)
}

func (s *Store) ListSessions(ctx context.Context) ([]session.Session, error) {
	return s.listSessions(ctx, sessionSelect+` ORDER BY created_at, id`)
}

func (s *Store) listSessions(ctx context.Context, query string, args ...any) ([]session.Session, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list Sessions: %w", err)
	}
	defer rows.Close()
	var results []session.Session
	for rows.Next() {
		result, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("list Sessions: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Sessions: %w", err)
	}
	return results, nil
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id string, status session.Status) (session.Session, error) {
	if err := status.Validate(); err != nil {
		return session.Session{}, err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ?, last_activity_at = ? WHERE id = ?`,
		string(status), formatTime(now), formatTime(now), id)
	if err != nil {
		return session.Session{}, fmt.Errorf("update Session %s: %w", id, err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return session.Session{}, fmt.Errorf("update Session %s: %w", id, err)
	} else if count == 0 {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	return s.GetSession(ctx, id)
}

// CompareAndSwapSessionWorker is the single Session lifecycle mutation
// primitive. A transition succeeds only while status and both worker locators
// still exactly match the caller's observation. A blank logPath preserves the
// existing journal address; claims provide the canonical path explicitly.
func (s *Store) CompareAndSwapSessionWorker(ctx context.Context, id string, before, after session.WorkerState, logPath string) (session.Session, bool, error) {
	if err := before.Status.Validate(); err != nil {
		return session.Session{}, false, err
	}
	if err := after.Status.Validate(); err != nil {
		return session.Session{}, false, err
	}
	if before.PID < 0 || after.PID < 0 {
		return session.Session{}, false, fmt.Errorf("worker PID must not be negative")
	}
	if strings.TrimSpace(before.Socket) != before.Socket || strings.TrimSpace(after.Socket) != after.Socket {
		return session.Session{}, false, fmt.Errorf("worker socket must not contain surrounding whitespace")
	}
	if strings.TrimSpace(logPath) != logPath {
		return session.Session{}, false, fmt.Errorf("worker log path must not contain surrounding whitespace")
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET worker_pid = NULLIF(?, 0), worker_socket = NULLIF(?, ''),
			log_path = COALESCE(NULLIF(?, ''), log_path), status = ?,
			updated_at = ?, last_activity_at = ?
		WHERE id = ? AND status = ?
			AND COALESCE(worker_pid, 0) = ? AND COALESCE(worker_socket, '') = ?`,
		after.PID, after.Socket, logPath, string(after.Status), formatTime(now), formatTime(now),
		id, string(before.Status), before.PID, before.Socket)
	if err != nil {
		return session.Session{}, false, fmt.Errorf("transition Session %s worker: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return session.Session{}, false, fmt.Errorf("transition Session %s worker: %w", id, err)
	}
	current, err := s.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, false, err
	}
	return current, count != 0, nil
}

// UpdateSessionProviderRef records the opaque reference needed to exactly
// resume the provider conversation. Once present, ProviderRef is immutable.
// RuntimeRef identifies the current provider Port record and may advance when
// that same Session is reconnected. Blank values preserve stored values.
func (s *Store) UpdateSessionProviderRef(ctx context.Context, id, runtimeRef, providerRef string) (session.Session, error) {
	if strings.TrimSpace(runtimeRef) != runtimeRef {
		return session.Session{}, fmt.Errorf("Session runtime reference must not contain surrounding whitespace")
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET acpx_record_id = COALESCE(NULLIF(?, ''), acpx_record_id),
			acp_session_id = COALESCE(NULLIF(?, ''), acp_session_id),
			updated_at = ?, last_activity_at = ?
		WHERE id = ?
			AND (COALESCE(acp_session_id, '') = '' OR ? = '' OR acp_session_id = ?)`,
		runtimeRef, providerRef, formatTime(now), formatTime(now), id, providerRef, providerRef)
	if err != nil {
		return session.Session{}, fmt.Errorf("update Session %s provider reference: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return session.Session{}, fmt.Errorf("update Session %s provider reference: %w", id, err)
	}
	if count == 0 {
		current, lookupErr := s.GetSession(ctx, id)
		if lookupErr != nil {
			return session.Session{}, lookupErr
		}
		return session.Session{}, fmt.Errorf("Session %s already uses provider reference %q; refusing replacement with %q", id, current.ProviderRef, providerRef)
	}
	return s.GetSession(ctx, id)
}

// CompareAndSwapSessionProviderRef applies provider identity only for the
// exact worker owner and status that opened the Port. ProviderRef remains
// write-once; a stale owner may neither advance RuntimeRef nor replace it.
func (s *Store) CompareAndSwapSessionProviderRef(ctx context.Context, id string, owner session.WorkerState, runtimeRef, providerRef string) (session.Session, bool, error) {
	if err := owner.Status.Validate(); err != nil {
		return session.Session{}, false, err
	}
	if owner.PID <= 0 || strings.TrimSpace(owner.Socket) == "" {
		return session.Session{}, false, fmt.Errorf("owned provider reference update requires worker PID and socket")
	}
	if strings.TrimSpace(owner.Socket) != owner.Socket || strings.TrimSpace(runtimeRef) != runtimeRef {
		return session.Session{}, false, fmt.Errorf("Session worker socket and runtime reference must not contain surrounding whitespace")
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET acpx_record_id = COALESCE(NULLIF(?, ''), acpx_record_id),
			acp_session_id = COALESCE(NULLIF(?, ''), acp_session_id),
			updated_at = ?, last_activity_at = ?
		WHERE id = ? AND status = ?
			AND COALESCE(worker_pid, 0) = ? AND COALESCE(worker_socket, '') = ?
			AND (COALESCE(acp_session_id, '') = '' OR ? = '' OR acp_session_id = ?)`,
		runtimeRef, providerRef, formatTime(now), formatTime(now), id, string(owner.Status), owner.PID, owner.Socket,
		providerRef, providerRef)
	if err != nil {
		return session.Session{}, false, fmt.Errorf("update owned Session %s provider reference: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return session.Session{}, false, fmt.Errorf("update owned Session %s provider reference: %w", id, err)
	}
	current, err := s.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, false, err
	}
	if count == 0 && current.WorkerState() == owner && providerRef != "" && current.ProviderRef != "" && current.ProviderRef != providerRef {
		return session.Session{}, false, fmt.Errorf("Session %s already uses provider reference %q; refusing replacement with %q", id, current.ProviderRef, providerRef)
	}
	return current, count != 0, nil
}

func (s *Store) UpdateSessionWorker(ctx context.Context, id string, pid int, socket, logPath string, status session.Status) (session.Session, error) {
	if pid <= 0 {
		return session.Session{}, fmt.Errorf("worker PID must be positive")
	}
	if strings.TrimSpace(socket) == "" || strings.TrimSpace(logPath) == "" {
		return session.Session{}, fmt.Errorf("worker socket and log path are required")
	}
	if err := status.Validate(); err != nil {
		return session.Session{}, err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET worker_pid = ?, worker_socket = ?, log_path = ?, status = ?, updated_at = ?, last_activity_at = ?
		WHERE id = ?`, pid, socket, logPath, string(status), formatTime(now), formatTime(now), id)
	if err != nil {
		return session.Session{}, fmt.Errorf("update Session %s worker: %w", id, err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return session.Session{}, fmt.Errorf("update Session %s worker: %w", id, err)
	} else if count == 0 {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	return s.GetSession(ctx, id)
}

func (s *Store) ClearSessionWorker(ctx context.Context, id string, status session.Status) (session.Session, error) {
	if err := status.Validate(); err != nil {
		return session.Session{}, err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET worker_pid = NULL, worker_socket = NULL, status = ?, updated_at = ?, last_activity_at = ?
		WHERE id = ?`, string(status), formatTime(now), formatTime(now), id)
	if err != nil {
		return session.Session{}, fmt.Errorf("clear Session %s worker: %w", id, err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return session.Session{}, fmt.Errorf("clear Session %s worker: %w", id, err)
	} else if count == 0 {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	return s.GetSession(ctx, id)
}

const taskSelect = `SELECT id, title, description, status, external_ref, metadata_json, created_at, updated_at FROM tasks`

// Old schemas retain native/provider-history columns for migration
// compatibility. The application intentionally selects only the two opaque
// references used by the Session Channel.
const sessionSelect = `SELECT id, task_id, parent_session_id, role, agent, provider, repo_path, worktree_path, branch,
	acpx_record_id, acp_session_id, status, worker_pid, worker_socket, log_path, metadata_json,
	created_at, updated_at, last_activity_at FROM sessions`

type scanner interface {
	Scan(...any) error
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertSession(ctx context.Context, executor sqlExecer, value session.Session) error {
	_, err := executor.ExecContext(ctx, `INSERT INTO sessions
		(id, task_id, parent_session_id, role, agent, provider, repo_path, worktree_path, branch,
		 acpx_record_id, acp_session_id, status, worker_pid, worker_socket, log_path, metadata_json,
		 created_at, updated_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		value.ID, value.TaskID, nullString(value.ParentSessionID), nullString(value.Role), value.Agent,
		nullString(value.Provider), nullString(value.RepoPath), nullString(value.WorktreePath), nullString(value.Branch),
		nullString(value.RuntimeRef), nullString(value.ProviderRef), string(value.Status), nullInt(value.WorkerPID),
		nullString(value.WorkerSocket), nullString(value.LogPath), nullJSON(value.MetadataJSON),
		formatTime(value.CreatedAt), formatTime(value.UpdatedAt))
	return err
}

func scanTask(row scanner) (task.Task, error) {
	var result task.Task
	var description, externalRef, metadata sql.NullString
	var status, createdAt, updatedAt string
	if err := row.Scan(&result.ID, &result.Title, &description, &status, &externalRef, &metadata, &createdAt, &updatedAt); err != nil {
		return task.Task{}, err
	}
	result.Description, result.ExternalRef = description.String, externalRef.String
	result.Status = task.Status(status)
	if metadata.Valid {
		result.MetadataJSON = json.RawMessage(metadata.String)
	}
	var err error
	if result.CreatedAt, err = parseTime(createdAt); err != nil {
		return task.Task{}, err
	}
	if result.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return task.Task{}, err
	}
	return result, nil
}

func scanSession(row scanner) (session.Session, error) {
	var result session.Session
	var parentSessionID, role, provider, repoPath, worktreePath, branch sql.NullString
	var runtimeRef, providerRef, socket, logPath, metadata, lastActivity sql.NullString
	var workerPID sql.NullInt64
	var status, createdAt, updatedAt string
	if err := row.Scan(&result.ID, &result.TaskID, &parentSessionID, &role, &result.Agent, &provider,
		&repoPath, &worktreePath, &branch, &runtimeRef, &providerRef, &status, &workerPID,
		&socket, &logPath, &metadata, &createdAt, &updatedAt, &lastActivity); err != nil {
		return session.Session{}, err
	}
	result.ParentSessionID = parentSessionID.String
	result.Role = role.String
	result.Provider = provider.String
	result.RepoPath = repoPath.String
	result.WorktreePath = worktreePath.String
	result.Branch = branch.String
	result.RuntimeRef = runtimeRef.String
	result.ProviderRef = providerRef.String
	result.Status = session.Status(status)
	result.WorkerPID = int(workerPID.Int64)
	result.WorkerSocket = socket.String
	result.LogPath = logPath.String
	if metadata.Valid {
		result.MetadataJSON = json.RawMessage(metadata.String)
	}
	var err error
	if result.CreatedAt, err = parseTime(createdAt); err != nil {
		return session.Session{}, err
	}
	if result.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return session.Session{}, err
	}
	if lastActivity.Valid {
		parsed, err := parseTime(lastActivity.String)
		if err != nil {
			return session.Session{}, err
		}
		result.LastActivityAt = &parsed
	}
	return result, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse database timestamp %q: %w", value, err)
	}
	return parsed.UTC(), nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func nullInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), value...)
}
