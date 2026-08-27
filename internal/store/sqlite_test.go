package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
	_ "modernc.org/sqlite"
)

var (
	_ session.Repository = (*Store)(nil)
	_ task.Repository    = (*Store)(nil)
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	repository, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return repository, path
}

func mustTask(t *testing.T, repository *Store, id string) task.Task {
	t.Helper()
	created, err := repository.CreateTask(context.Background(), task.CreateParams{ID: id, Title: "Task " + id})
	if err != nil {
		t.Fatalf("CreateTask(%s) error = %v", id, err)
	}
	return created
}

func mustSession(t *testing.T, repository *Store, id, taskID string) session.Session {
	t.Helper()
	created, err := repository.CreateSession(context.Background(), session.CreateParams{ID: id, TaskID: taskID, Agent: "codex"})
	if err != nil {
		t.Fatalf("CreateSession(%s) error = %v", id, err)
	}
	return created
}

func TestFreshDatabaseHasOnlyTaskAndSessionEntities(t *testing.T) {
	repository, path := openTestStore(t)
	rows, err := repository.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if got, want := fmt.Sprint(names), "[sessions tasks]"; got != want {
		t.Fatalf("tables = %s, want %s", got, want)
	}
	var version int
	if err := repository.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	var obsoleteIndex int
	if err := repository.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'sessions_task_prepare_request_unique'`).Scan(&obsoleteIndex); err != nil {
		t.Fatal(err)
	}
	if obsoleteIndex != 0 {
		t.Fatal("prepared-launch uniqueness index survived migration 005")
	}
	info, err := filepath.Glob(path + "*")
	if err != nil || len(info) == 0 {
		t.Fatalf("database files = %v, err = %v", info, err)
	}
}

func TestDatabaseReopenPreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.CreateTask(ctx, task.CreateParams{Title: "persist me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	loaded, err := second.GetTask(ctx, created.ID)
	if err != nil || loaded.Title != "persist me" {
		t.Fatalf("GetTask() = %#v, %v", loaded, err)
	}
}

func TestMigrationFromEveryExistingVersionToSix(t *testing.T) {
	for version := 1; version <= 5; version++ {
		version := version
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			ctx := context.Background()
			path := databaseAtVersion(t, version)
			repository, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open(v%d) error = %v", version, err)
			}
			defer repository.Close()

			var gotVersion int
			if err := repository.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&gotVersion); err != nil || gotVersion != 6 {
				t.Fatalf("schema version = %d, err = %v", gotVersion, err)
			}
			columns := tableColumns(t, repository.db, "sessions")
			for _, name := range []string{
				"parent_session_id", "acpx_record_id", "acp_session_id", "native_session_id",
				"provider_conversation_id", "conversation_id_source", "conversation_id_verified_at",
			} {
				if !columns[name] {
					t.Fatalf("sessions.%s missing after v%d migration", name, version)
				}
			}

			loaded, err := repository.GetSession(ctx, "S-legacy")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.ProviderRef != "" {
				t.Fatalf("ProviderRef = %q, want unverified legacy identity cleared", loaded.ProviderRef)
			}
			wantRuntime := ""
			if version >= 2 {
				wantRuntime = "runtime-legacy"
			}
			if loaded.RuntimeRef != wantRuntime {
				t.Fatalf("RuntimeRef = %q, want %q", loaded.RuntimeRef, wantRuntime)
			}
			if loaded.ParentSessionID != "" {
				t.Fatalf("legacy parent = %q", loaded.ParentSessionID)
			}
			if loaded.Status != session.StatusStopped {
				t.Fatalf("legacy status = %q, want stopped", loaded.Status)
			}
		})
	}
}

func TestMigrationClearsEveryLegacyProviderRefAndPreservesFreshProviderRef(t *testing.T) {
	for _, test := range []struct {
		name             string
		providerRef      string
		canonicalRef     any
		canonicalSource  any
		canonicalChecked any
		nativeRef        any
		wantProviderRef  string
	}{
		{
			name: "verified legacy provider store is audit only", providerRef: "legacy-evidence",
			canonicalRef: "verified-provider", canonicalSource: "provider-store", canonicalChecked: "2026-01-02T03:04:05Z",
			nativeRef:       "legacy-native",
			wantProviderRef: "",
		},
		{
			name: "fresh Session Channel row", providerRef: "fresh-provider",
			canonicalRef: nil, canonicalSource: nil, canonicalChecked: nil,
			wantProviderRef: "fresh-provider",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := databaseAtVersion(t, 5)
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.ExecContext(ctx, `UPDATE sessions
				SET acp_session_id = ?, provider_conversation_id = ?, conversation_id_source = ?, conversation_id_verified_at = ?, native_session_id = ?
				WHERE id = 'S-legacy'`, test.providerRef, test.canonicalRef, test.canonicalSource, test.canonicalChecked, test.nativeRef); err != nil {
				database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			repository, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			loaded, err := repository.GetSession(ctx, "S-legacy")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.ProviderRef != test.wantProviderRef || loaded.Status != session.StatusStopped {
				t.Fatalf("migrated Session = %#v, want provider %q and stopped", loaded, test.wantProviderRef)
			}

			var canonicalRef, canonicalSource, canonicalChecked sql.NullString
			if err := repository.db.QueryRowContext(ctx, `SELECT provider_conversation_id, conversation_id_source,
				conversation_id_verified_at FROM sessions WHERE id = 'S-legacy'`).Scan(
				&canonicalRef, &canonicalSource, &canonicalChecked,
			); err != nil {
				t.Fatal(err)
			}
			asAuditValue := func(value sql.NullString) any {
				if !value.Valid {
					return nil
				}
				return value.String
			}
			if got := asAuditValue(canonicalRef); got != test.canonicalRef {
				t.Fatalf("provider_conversation_id = %#v, want preserved audit value %#v", got, test.canonicalRef)
			}
			if got := asAuditValue(canonicalSource); got != test.canonicalSource {
				t.Fatalf("conversation_id_source = %#v, want preserved audit value %#v", got, test.canonicalSource)
			}
			if got := asAuditValue(canonicalChecked); got != test.canonicalChecked {
				t.Fatalf("conversation_id_verified_at = %#v, want preserved audit value %#v", got, test.canonicalChecked)
			}
		})
	}
}

func TestMigrationDoesNotGuessProviderRefFromLegacyEvidence(t *testing.T) {
	ctx := context.Background()
	path := databaseAtVersion(t, 4)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE sessions
		SET provider_conversation_id = NULL, acp_session_id = 'acp-evidence', native_session_id = 'native-evidence'
		WHERE id = 'S-legacy'`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	loaded, err := repository.GetSession(ctx, "S-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProviderRef != "" || loaded.Status != session.StatusStopped {
		t.Fatalf("ambiguous legacy Session = %#v; want blank provider ref and stopped status", loaded)
	}
}

func databaseAtVersion(t *testing.T, version int) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("v%d.db", version))
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for current := 1; current <= version; current++ {
		name := map[int]string{
			1: "001_init.sql",
			2: "002_acpx_record_id.sql",
			3: "003_provider_conversation_id.sql",
			4: "004_prepared_session_idempotency.sql",
			5: "005_session_parent.sql",
		}[current]
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			database.Close()
			t.Fatal(err)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			database.Close()
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO tasks
		(id, title, status, created_at, updated_at)
		VALUES ('T-legacy', 'legacy', 'open', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions
		(id, task_id, agent, acp_session_id, native_session_id, status, metadata_json, created_at, updated_at)
		VALUES ('S-legacy', 'T-legacy', 'codex', 'provider-legacy', 'ignored-native', 'waiting', '{',
		'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if version >= 2 {
		if _, err := database.ExecContext(ctx, `UPDATE sessions SET acpx_record_id = 'runtime-legacy' WHERE id = 'S-legacy'`); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if version >= 3 {
		// These historical fields remain on disk, but malformed or conflicting
		// values must not participate in the Session model.
		if _, err := database.ExecContext(ctx, `UPDATE sessions
			SET provider_conversation_id = 'ignored-history', conversation_id_source = 'provider-store',
				conversation_id_verified_at = 'not-a-timestamp'
			WHERE id = 'S-legacy'`); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func tableColumns(t *testing.T, database *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := database.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		result[name] = true
	}
	return result
}

func TestTaskAndSessionCRUD(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	now := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	repository.now = func() time.Time { return now }

	createdTask, err := repository.CreateTask(ctx, task.CreateParams{
		ID: "T-crud", Title: "  Research  ", Description: "details", ExternalRef: "ISSUE-1",
		MetadataJSON: json.RawMessage(`{"scope":"btc"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if createdTask.Title != "Research" || createdTask.Status != task.StatusOpen || !createdTask.CreatedAt.Equal(now) {
		t.Fatalf("created Task = %#v", createdTask)
	}
	createdSession, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-crud", TaskID: createdTask.ID, Role: "manager", Agent: " codex ", Provider: "openai",
		RepoPath: "/repo", WorktreePath: "/worktree", Branch: "agent/S-crud",
		RuntimeRef: "runtime-1", ProviderRef: "provider-1", Status: session.StatusWaiting,
		MetadataJSON: json.RawMessage(`{"permission":"read"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if createdSession.Agent != "codex" || createdSession.RuntimeRef != "runtime-1" || createdSession.ProviderRef != "provider-1" || createdSession.LastActivityAt != nil {
		t.Fatalf("created Session = %#v", createdSession)
	}

	loaded, err := repository.GetSession(ctx, createdSession.ID)
	if err != nil || loaded.TaskID != createdTask.ID || string(loaded.MetadataJSON) != `{"permission":"read"}` {
		t.Fatalf("GetSession() = %#v, %v", loaded, err)
	}
	byTask, err := repository.ListSessionsByTask(ctx, createdTask.ID)
	if err != nil || len(byTask) != 1 || byTask[0].ID != createdSession.ID {
		t.Fatalf("ListSessionsByTask() = %#v, %v", byTask, err)
	}
	all, err := repository.ListSessions(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSessions() = %#v, %v", all, err)
	}

	now = now.Add(time.Minute)
	updatedSession, err := repository.UpdateSessionStatus(ctx, createdSession.ID, session.StatusWorking)
	if err != nil || updatedSession.Status != session.StatusWorking || updatedSession.LastActivityAt == nil || !updatedSession.LastActivityAt.Equal(now) {
		t.Fatalf("UpdateSessionStatus() = %#v, %v", updatedSession, err)
	}
	updatedTask, err := repository.UpdateTaskStatus(ctx, createdTask.ID, task.StatusWorking)
	if err != nil || updatedTask.Status != task.StatusWorking || !updatedTask.UpdatedAt.Equal(now) {
		t.Fatalf("UpdateTaskStatus() = %#v, %v", updatedTask, err)
	}
	if tasks, err := repository.ListTasks(ctx); err != nil || len(tasks) != 1 || tasks[0].ID != createdTask.ID {
		t.Fatalf("ListTasks() = %#v, %v", tasks, err)
	}

	for name, call := range map[string]func() error{
		"get Task":    func() error { _, err := repository.GetTask(ctx, "T-missing"); return err },
		"get Session": func() error { _, err := repository.GetSession(ctx, "S-missing"); return err },
		"update Task": func() error { _, err := repository.UpdateTaskStatus(ctx, "T-missing", task.StatusDone); return err },
		"update Session": func() error {
			_, err := repository.UpdateSessionStatus(ctx, "S-missing", session.StatusDone)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSessionParentMustExistInSameTask(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	first := mustTask(t, repository, "T-first")
	second := mustTask(t, repository, "T-second")
	parent := mustSession(t, repository, "S-parent", first.ID)

	child, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-child", TaskID: first.ID, ParentSessionID: parent.ID, Agent: "claude",
	})
	if err != nil || child.ParentSessionID != parent.ID {
		t.Fatalf("child = %#v, %v", child, err)
	}
	if _, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-cross-task", TaskID: second.ID, ParentSessionID: parent.ID, Agent: "codex",
	}); err == nil || !strings.Contains(err.Error(), "different Task") {
		t.Fatalf("cross-Task parent error = %v", err)
	}
	if _, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-missing-parent", TaskID: first.ID, ParentSessionID: "S-missing", Agent: "codex",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent error = %v", err)
	}
	if _, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-self", TaskID: first.ID, ParentSessionID: "S-self", Agent: "codex",
	}); err == nil {
		t.Fatal("self parent was accepted")
	}
	if _, err := repository.CreateSession(ctx, session.CreateParams{TaskID: "T-missing", Agent: "codex"}); err == nil {
		t.Fatal("missing Task foreign key was accepted")
	}
	if _, err := repository.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, parent.ID); err == nil {
		t.Fatal("parent Session deletion bypassed the foreign key")
	}
}

func TestProviderReferenceIsImmutable(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	createdTask := mustTask(t, repository, "T-provider")
	current := mustSession(t, repository, "S-provider", createdTask.ID)

	type result struct {
		provider string
		updated  session.Session
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, providerRef := range []string{"provider-a", "provider-b"} {
		providerRef := providerRef
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			updated, err := repository.UpdateSessionProviderRef(ctx, current.ID, "runtime-"+providerRef, providerRef)
			results <- result{provider: providerRef, updated: updated, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	winner := ""
	for item := range results {
		if item.err == nil {
			if winner != "" {
				t.Fatalf("multiple provider references won: %q and %q", winner, item.provider)
			}
			winner = item.provider
		}
	}
	if winner == "" {
		t.Fatal("no provider reference won")
	}
	stored, err := repository.GetSession(ctx, current.ID)
	if err != nil || stored.ProviderRef != winner || stored.RuntimeRef != "runtime-"+winner {
		t.Fatalf("stored Session = %#v, %v", stored, err)
	}

	advanced, err := repository.UpdateSessionProviderRef(ctx, current.ID, "runtime-next", winner)
	if err != nil || advanced.ProviderRef != winner || advanced.RuntimeRef != "runtime-next" {
		t.Fatalf("same provider update = %#v, %v", advanced, err)
	}
	preserved, err := repository.UpdateSessionProviderRef(ctx, current.ID, "runtime-final", "")
	if err != nil || preserved.ProviderRef != winner || preserved.RuntimeRef != "runtime-final" {
		t.Fatalf("blank provider update = %#v, %v", preserved, err)
	}
	if _, err := repository.UpdateSessionProviderRef(ctx, current.ID, "runtime-conflict", "provider-replacement"); err == nil || !strings.Contains(err.Error(), "refusing replacement") {
		t.Fatalf("replacement error = %v", err)
	}
	unchanged, err := repository.GetSession(ctx, current.ID)
	if err != nil || unchanged.RuntimeRef != "runtime-final" || unchanged.ProviderRef != winner {
		t.Fatalf("conflict partially updated Session = %#v, %v", unchanged, err)
	}
	if _, err := repository.UpdateSessionProviderRef(ctx, current.ID, " runtime", ""); err == nil {
		t.Fatal("whitespace runtime reference accepted")
	}
	if _, err := repository.UpdateSessionProviderRef(ctx, "S-missing", "runtime", "provider"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Session error = %v", err)
	}
}

func TestWorkerLifecycle(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	createdTask := mustTask(t, repository, "T-worker")
	created := mustSession(t, repository, "S-worker", createdTask.ID)

	for name, call := range map[string]func() error{
		"zero PID": func() error {
			_, err := repository.UpdateSessionWorker(ctx, created.ID, 0, "/worker.sock", "/session.jsonl", session.StatusWaiting)
			return err
		},
		"blank socket": func() error {
			_, err := repository.UpdateSessionWorker(ctx, created.ID, 42, " ", "/session.jsonl", session.StatusWaiting)
			return err
		},
		"blank log": func() error {
			_, err := repository.UpdateSessionWorker(ctx, created.ID, 42, "/worker.sock", "", session.StatusWaiting)
			return err
		},
		"bad status": func() error {
			_, err := repository.UpdateSessionWorker(ctx, created.ID, 42, "/worker.sock", "/session.jsonl", "paused")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("invalid worker update accepted")
			}
		})
	}

	running, err := repository.UpdateSessionWorker(ctx, created.ID, 42, "/worker.sock", "/session.jsonl", session.StatusWaiting)
	if err != nil || running.WorkerPID != 42 || running.WorkerSocket != "/worker.sock" || running.LogPath != "/session.jsonl" || running.LastActivityAt == nil {
		t.Fatalf("running Session = %#v, %v", running, err)
	}
	stopped, err := repository.ClearSessionWorker(ctx, created.ID, session.StatusStopped)
	if err != nil || stopped.WorkerPID != 0 || stopped.WorkerSocket != "" || stopped.LogPath != "/session.jsonl" || stopped.Status != session.StatusStopped {
		t.Fatalf("stopped Session = %#v, %v", stopped, err)
	}
	if _, err := repository.ClearSessionWorker(ctx, "S-missing", session.StatusStopped); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Session error = %v", err)
	}
}

func TestSessionWorkerCASFencesStaleLifecycleAndProviderWrites(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	createdTask := mustTask(t, repository, "T-worker-cas")
	created := mustSession(t, repository, "S-worker-cas", createdTask.ID)
	ownerlessStarting := created.WorkerState()
	firstOwner := session.WorkerState{Status: session.StatusStarting, PID: 42, Socket: "/first.sock"}

	claimed, swapped, err := repository.CompareAndSwapSessionWorker(ctx, created.ID, ownerlessStarting, firstOwner, "/session.jsonl")
	if err != nil || !swapped || claimed.WorkerState() != firstOwner || claimed.LogPath != "/session.jsonl" {
		t.Fatalf("first claim = %#v, swapped=%t, err=%v", claimed, swapped, err)
	}
	stopped := ownerlessStarting
	stopped.Status = session.StatusStopped
	current, swapped, err := repository.CompareAndSwapSessionWorker(ctx, created.ID, ownerlessStarting, stopped, "")
	if err != nil || swapped || current.WorkerState() != firstOwner {
		t.Fatalf("stale ownerless Stop = %#v, swapped=%t, err=%v", current, swapped, err)
	}

	waitingOwner := firstOwner
	waitingOwner.Status = session.StatusWaiting
	current, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, firstOwner, waitingOwner, "")
	if err != nil || !swapped || current.WorkerState() != waitingOwner {
		t.Fatalf("owner status advance = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	staleFailure := firstOwner
	staleFailure.Status = session.StatusFailed
	current, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, firstOwner, staleFailure, "")
	if err != nil || swapped || current.WorkerState() != waitingOwner {
		t.Fatalf("stale reconciliation = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	current, swapped, err = repository.CompareAndSwapSessionProviderRef(ctx, created.ID, firstOwner, "runtime-stale", "provider-one")
	if err != nil || swapped || current.ProviderRef != "" {
		t.Fatalf("stale provider identity = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	current, swapped, err = repository.CompareAndSwapSessionProviderRef(ctx, created.ID, waitingOwner, "runtime-one", "provider-one")
	if err != nil || !swapped || current.ProviderRef != "provider-one" || current.RuntimeRef != "runtime-one" {
		t.Fatalf("owned provider identity = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	if _, _, err := repository.CompareAndSwapSessionProviderRef(ctx, created.ID, waitingOwner, "runtime-two", "provider-two"); err == nil || !strings.Contains(err.Error(), "refusing replacement") {
		t.Fatalf("provider replacement error = %v", err)
	}

	ownerlessStopped := session.WorkerState{Status: session.StatusStopped}
	current, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, waitingOwner, ownerlessStopped, "")
	if err != nil || !swapped || current.WorkerState() != ownerlessStopped {
		t.Fatalf("first owner release = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	secondAdmission := session.WorkerState{Status: session.StatusStarting}
	if _, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, ownerlessStopped, secondAdmission, ""); err != nil || !swapped {
		t.Fatalf("second admission swapped=%t, err=%v", swapped, err)
	}
	secondOwner := session.WorkerState{Status: session.StatusStarting, PID: 43, Socket: "/second.sock"}
	current, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, secondAdmission, secondOwner, "")
	if err != nil || !swapped || current.WorkerState() != secondOwner {
		t.Fatalf("second claim = %#v, swapped=%t, err=%v", current, swapped, err)
	}
	current, swapped, err = repository.CompareAndSwapSessionWorker(ctx, created.ID, waitingOwner, ownerlessStopped, "")
	if err != nil || swapped || current.WorkerState() != secondOwner {
		t.Fatalf("stale owner release mutated replacement = %#v, swapped=%t, err=%v", current, swapped, err)
	}
}

func TestCreateTaskWithSessionIsAtomic(t *testing.T) {
	ctx := context.Background()
	repository, _ := openTestStore(t)
	createdTask, createdSession, err := repository.CreateTaskWithSession(ctx,
		task.CreateParams{ID: "T-atomic", Title: "Atomic"},
		session.CreateParams{ID: "S-atomic", Agent: "codex", ProviderRef: "provider-atomic"})
	if err != nil || createdSession.TaskID != createdTask.ID || createdSession.ProviderRef != "provider-atomic" {
		t.Fatalf("CreateTaskWithSession() = %#v, %#v, %v", createdTask, createdSession, err)
	}

	if _, _, err := repository.CreateTaskWithSession(ctx,
		task.CreateParams{ID: "T-parent-invalid", Title: "Invalid parent"},
		session.CreateParams{ID: "S-parent-invalid", ParentSessionID: createdSession.ID, Agent: "codex"}); err == nil {
		t.Fatal("first Session parent was accepted")
	}
	if _, err := repository.GetTask(ctx, "T-parent-invalid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid parent left Task behind: %v", err)
	}

	if _, _, err := repository.CreateTaskWithSession(ctx,
		task.CreateParams{ID: "T-rollback", Title: "Rollback"},
		session.CreateParams{ID: createdSession.ID, Agent: "codex"}); err == nil {
		t.Fatal("duplicate Session ID accepted")
	}
	if _, err := repository.GetTask(ctx, "T-rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction left Task behind: %v", err)
	}
}

func TestRepositoriesRejectInvalidInput(t *testing.T) {
	repository, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := repository.CreateTask(ctx, task.CreateParams{Title: "bad", Status: "running"}); err == nil {
		t.Fatal("invalid Task status accepted")
	}
	if _, err := repository.CreateTask(ctx, task.CreateParams{Title: "bad", MetadataJSON: []byte("{")}); err == nil {
		t.Fatal("invalid Task metadata accepted")
	}
	createdTask := mustTask(t, repository, "T-validation")
	if _, err := repository.CreateSession(ctx, session.CreateParams{TaskID: createdTask.ID, Agent: "codex", MetadataJSON: []byte("{")}); err == nil {
		t.Fatal("invalid Session metadata accepted")
	}
	if _, err := repository.UpdateTaskStatus(ctx, createdTask.ID, "running"); err == nil {
		t.Fatal("invalid Task status update accepted")
	}
}
