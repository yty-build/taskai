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

const currentSchemaVersion = 2

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
	row := s.db.QueryRowContext(ctx, `SELECT id, title, description, status, external_ref, metadata_json, created_at, updated_at
		FROM tasks WHERE id = ?`, id)
	result, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Task{}, fmt.Errorf("Task %s: %w", id, ErrNotFound)
	}
	return result, err
}

func (s *Store) ListTasks(ctx context.Context) ([]task.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, description, status, external_ref, metadata_json, created_at, updated_at
		FROM tasks ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list Tasks: %w", err)
	}
	defer rows.Close()
	var results []task.Task
	for rows.Next() {
		result, err := scanTask(rows)
		if err != nil {
			return nil, err
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
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, string(status), formatTime(s.now().UTC()), id)
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
		ID: params.ID, TaskID: params.TaskID, Role: params.Role, Agent: strings.TrimSpace(params.Agent), Provider: params.Provider,
		RepoPath: params.RepoPath, WorktreePath: params.WorktreePath, Branch: params.Branch,
		ACPXRecordID: params.ACPXRecordID, ACPSessionID: params.ACPSessionID, NativeSessionID: params.NativeSessionID, Status: params.Status,
		WorkerPID: params.WorkerPID, WorkerSocket: params.WorkerSocket, LogPath: params.LogPath,
		MetadataJSON: cloneJSON(params.MetadataJSON), CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, task_id, role, agent, provider, repo_path, worktree_path, branch, acpx_record_id, acp_session_id, native_session_id,
		 status, worker_pid, worker_socket, log_path, metadata_json, created_at, updated_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		result.ID, result.TaskID, nullString(result.Role), result.Agent, nullString(result.Provider), nullString(result.RepoPath),
		nullString(result.WorktreePath), nullString(result.Branch), nullString(result.ACPXRecordID), nullString(result.ACPSessionID), nullString(result.NativeSessionID),
		string(result.Status), nullInt(result.WorkerPID), nullString(result.WorkerSocket), nullString(result.LogPath), nullJSON(result.MetadataJSON),
		formatTime(result.CreatedAt), formatTime(result.UpdatedAt))
	if err != nil {
		return session.Session{}, fmt.Errorf("create Session %s: %w", result.ID, err)
	}
	return result, nil
}

// CreateTaskWithSession atomically persists the convenience one-Session Task
// without changing the underlying Task -> Session relationship.
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
	if taskParams.Status == "" {
		taskParams.Status = task.StatusOpen
	}
	if sessionParams.Status == "" {
		sessionParams.Status = session.StatusStarting
	}
	now := s.now().UTC()
	createdTask := task.Task{
		ID: taskParams.ID, Title: strings.TrimSpace(taskParams.Title), Description: taskParams.Description, Status: taskParams.Status,
		ExternalRef: taskParams.ExternalRef, MetadataJSON: cloneJSON(taskParams.MetadataJSON), CreatedAt: now, UpdatedAt: now,
	}
	createdSession := session.Session{
		ID: sessionParams.ID, TaskID: taskParams.ID, Role: sessionParams.Role, Agent: strings.TrimSpace(sessionParams.Agent), Provider: sessionParams.Provider,
		RepoPath: sessionParams.RepoPath, WorktreePath: sessionParams.WorktreePath, Branch: sessionParams.Branch,
		ACPXRecordID: sessionParams.ACPXRecordID, ACPSessionID: sessionParams.ACPSessionID, NativeSessionID: sessionParams.NativeSessionID, Status: sessionParams.Status,
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, createdTask.ID, createdTask.Title, nullString(createdTask.Description), string(createdTask.Status),
		nullString(createdTask.ExternalRef), nullJSON(createdTask.MetadataJSON), formatTime(now), formatTime(now)); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("create Task %s: %w", createdTask.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions
		(id, task_id, role, agent, provider, repo_path, worktree_path, branch, acpx_record_id, acp_session_id, native_session_id,
		 status, worker_pid, worker_socket, log_path, metadata_json, created_at, updated_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		createdSession.ID, createdSession.TaskID, nullString(createdSession.Role), createdSession.Agent, nullString(createdSession.Provider), nullString(createdSession.RepoPath),
		nullString(createdSession.WorktreePath), nullString(createdSession.Branch), nullString(createdSession.ACPXRecordID), nullString(createdSession.ACPSessionID), nullString(createdSession.NativeSessionID),
		string(createdSession.Status), nullInt(createdSession.WorkerPID), nullString(createdSession.WorkerSocket), nullString(createdSession.LogPath),
		nullJSON(createdSession.MetadataJSON), formatTime(now), formatTime(now)); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("create Session %s: %w", createdSession.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return task.Task{}, session.Session{}, fmt.Errorf("commit Task creation: %w", err)
	}
	return createdTask, createdSession, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (session.Session, error) {
	row := s.db.QueryRowContext(ctx, sessionSelect+` WHERE id = ?`, id)
	result, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	return result, err
}

func (s *Store) ListSessionsByTask(ctx context.Context, taskID string) ([]session.Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionSelect+` WHERE task_id = ? ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list Sessions for Task %s: %w", taskID, err)
	}
	defer rows.Close()
	var results []session.Session
	for rows.Next() {
		result, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list Sessions for Task %s: %w", taskID, err)
	}
	return results, nil
}

func (s *Store) UpdateSessionStatus(ctx context.Context, id string, status session.Status) (session.Session, error) {
	if err := status.Validate(); err != nil {
		return session.Session{}, err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ?, updated_at = ?, last_activity_at = ? WHERE id = ?`, string(status), formatTime(now), formatTime(now), id)
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

// UpdateSessionRuntimeIDs persists product, acpx, ACP, and provider-native IDs
// as distinct concepts on the existing Session entity.
func (s *Store) UpdateSessionRuntimeIDs(ctx context.Context, id, acpxRecordID, acpSessionID, nativeSessionID string) (session.Session, error) {
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE sessions
		SET acpx_record_id = ?, acp_session_id = ?, native_session_id = ?, updated_at = ?, last_activity_at = ?
		WHERE id = ?`, nullString(acpxRecordID), nullString(acpSessionID), nullString(nativeSessionID), formatTime(now), formatTime(now), id)
	if err != nil {
		return session.Session{}, fmt.Errorf("update Session %s runtime IDs: %w", id, err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return session.Session{}, fmt.Errorf("update Session %s runtime IDs: %w", id, err)
	} else if count == 0 {
		return session.Session{}, fmt.Errorf("Session %s: %w", id, ErrNotFound)
	}
	return s.GetSession(ctx, id)
}

const sessionSelect = `SELECT id, task_id, role, agent, provider, repo_path, worktree_path, branch,
	acpx_record_id, acp_session_id, native_session_id, status, worker_pid, worker_socket, log_path, metadata_json,
	created_at, updated_at, last_activity_at FROM sessions`

type scanner interface {
	Scan(...any) error
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
	var role, provider, repoPath, worktreePath, branch, acpxRecordID, acpID, nativeID, socket, logPath, metadata, lastActivity sql.NullString
	var workerPID sql.NullInt64
	var status, createdAt, updatedAt string
	if err := row.Scan(&result.ID, &result.TaskID, &role, &result.Agent, &provider, &repoPath, &worktreePath, &branch,
		&acpxRecordID, &acpID, &nativeID, &status, &workerPID, &socket, &logPath, &metadata, &createdAt, &updatedAt, &lastActivity); err != nil {
		return session.Session{}, err
	}
	result.Role, result.Provider, result.RepoPath, result.WorktreePath, result.Branch = role.String, provider.String, repoPath.String, worktreePath.String, branch.String
	result.ACPXRecordID, result.ACPSessionID, result.NativeSessionID, result.WorkerSocket, result.LogPath = acpxRecordID.String, acpID.String, nativeID.String, socket.String, logPath.String
	result.Status, result.WorkerPID = session.Status(status), int(workerPID.Int64)
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

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

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
