package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func TestFreshDatabaseHasOnlyTaskAndSessionTables(t *testing.T) {
	store, _ := openTestStore(t)
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
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
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}

func TestDatabaseReopenAndIdempotentMigration(t *testing.T) {
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

func TestMigrationFromVersionOneAddsDistinctACPXRecordID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := migrationFiles.ReadFile("migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, string(initial)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	rows, err := upgraded.db.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		found[name] = true
	}
	for _, name := range []string{"acpx_record_id", "provider_conversation_id", "conversation_id_source", "conversation_id_verified_at"} {
		if !found[name] {
			t.Fatalf("sessions.%s was not added", name)
		}
	}
}

func TestMigrationBackfillsCanonicalProviderConversation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_init.sql", "002_acpx_record_id.sql"} {
		body, readErr := migrationFiles.ReadFile("migrations/" + name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO tasks (id, title, status, created_at, updated_at) VALUES ('T-migrate', 'migrate', 'open', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions (id, task_id, agent, acp_session_id, status, created_at, updated_at) VALUES ('S-acp', 'T-migrate', 'codex', 'acp-exact', 'waiting', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions (id, task_id, agent, acp_session_id, native_session_id, status, created_at, updated_at) VALUES ('S-native', 'T-migrate', 'claude', 'acp-other', 'native-exact', 'waiting', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	acpOnly, err := upgraded.GetSession(ctx, "S-acp")
	if err != nil || acpOnly.ProviderConversationID != "acp-exact" || acpOnly.ConversationIDSource != session.ConversationSourceACP {
		t.Fatalf("ACP backfill = %#v, %v", acpOnly, err)
	}
	native, err := upgraded.GetSession(ctx, "S-native")
	if err != nil || native.ProviderConversationID != "native-exact" || native.ConversationIDSource != session.ConversationSourceNative {
		t.Fatalf("native backfill = %#v, %v", native, err)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	store, _ := openTestStore(t)
	_, err := store.CreateSession(context.Background(), session.CreateParams{TaskID: "T-missing", Agent: "codex"})
	if err == nil {
		t.Fatal("CreateSession() error = nil")
	}
}

func TestRepositoriesValidateAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	if _, err := store.CreateTask(ctx, task.CreateParams{Title: "bad", Status: "running"}); err == nil {
		t.Fatal("invalid Task status accepted")
	}
	if _, err := store.CreateTask(ctx, task.CreateParams{Title: "bad", MetadataJSON: []byte("{")}); err == nil {
		t.Fatal("invalid Task metadata accepted")
	}
	createdTask, err := store.CreateTask(ctx, task.CreateParams{Title: "Research", Description: "Details", ExternalRef: "ISSUE-1", MetadataJSON: []byte(`{"scope":"btc"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if createdTask.CreatedAt.Location().String() != "UTC" || createdTask.Status != task.StatusOpen {
		t.Fatalf("Task defaults = %#v", createdTask)
	}
	createdSession, err := store.CreateSession(ctx, session.CreateParams{TaskID: createdTask.ID, Role: "analyst", Agent: "codex", MetadataJSON: []byte(`{"permission":"read"}`)})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetSession(ctx, createdSession.ID)
	if err != nil || loaded.TaskID != createdTask.ID || loaded.Status != session.StatusStarting || string(loaded.MetadataJSON) != `{"permission":"read"}` {
		t.Fatalf("GetSession() = %#v, %v", loaded, err)
	}
	updated, err := store.UpdateSessionStatus(ctx, loaded.ID, session.StatusWorking)
	if err != nil || updated.Status != session.StatusWorking || updated.LastActivityAt == nil {
		t.Fatalf("UpdateSessionStatus() = %#v, %v", updated, err)
	}
	withIDs, err := store.UpdateSessionRuntimeIDs(ctx, loaded.ID, "acpx-record", "acp-session", "native-session")
	if err != nil || withIDs.ID != loaded.ID || withIDs.ACPXRecordID != "acpx-record" || withIDs.ACPSessionID != "acp-session" || withIDs.NativeSessionID != "native-session" || withIDs.ProviderConversationID != "native-session" || withIDs.ConversationIDSource != session.ConversationSourceNative {
		t.Fatalf("UpdateSessionRuntimeIDs() = %#v, %v", withIDs, err)
	}
	verifiedAt := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	verified, err := store.UpdateSessionConversation(ctx, loaded.ID, "native-session", session.ConversationSourceProviderStore, &verifiedAt)
	if err != nil || verified.ConversationIDVerifiedAt == nil || !verified.ConversationIDVerifiedAt.Equal(verifiedAt) || verified.ConversationIDSource != session.ConversationSourceProviderStore {
		t.Fatalf("UpdateSessionConversation() = %#v, %v", verified, err)
	}
	if _, err := store.UpdateSessionConversation(ctx, loaded.ID, "different-session", session.ConversationSourceProviderStore, &verifiedAt); err == nil {
		t.Fatal("canonical provider conversation replacement was accepted")
	}
	if _, err := store.UpdateSessionConversation(ctx, loaded.ID, "native-session", session.ConversationSourceProviderStore, nil); err == nil {
		t.Fatal("provider-store identity without verification time was accepted")
	}
	if _, err := store.UpdateSessionConversation(ctx, loaded.ID, "native-session", session.ConversationSourceACP, &verifiedAt); err == nil {
		t.Fatal("non-provider identity with verification time was accepted")
	}
	if _, err := store.UpdateSessionConversation(ctx, loaded.ID, "native-session", session.ConversationSourceACP, nil); err == nil || !strings.Contains(err.Error(), "cannot be downgraded") {
		t.Fatalf("provider-store verification downgrade error = %v", err)
	}
	if _, err := store.UpdateSessionRuntimeIDs(ctx, loaded.ID, "acpx-new", "acp-replacement", "native-session"); err == nil {
		t.Fatal("ACP adapter evidence replacement was accepted")
	}
	if _, err := store.UpdateSessionRuntimeIDs(ctx, loaded.ID, "acpx-new", "acp-session", "native-replacement"); err == nil {
		t.Fatal("native adapter evidence replacement was accepted")
	}
	preserved, err := store.UpdateSessionRuntimeIDs(ctx, loaded.ID, "acpx-refreshed", "", "")
	if err != nil || preserved.ACPXRecordID != "acpx-refreshed" || preserved.ACPSessionID != "acp-session" || preserved.NativeSessionID != "native-session" || preserved.ProviderConversationID != "native-session" {
		t.Fatalf("blank runtime evidence did not preserve canonical IDs: %#v, %v", preserved, err)
	}
	providerStoreACPOnly, err := store.CreateSession(ctx, session.CreateParams{
		ID: "S-provider-store-acp", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: t.TempDir(),
		ACPSessionID: "canonical-acp", ProviderConversationID: "canonical-acp",
		ConversationIDSource: session.ConversationSourceProviderStore, ConversationIDVerifiedAt: &verifiedAt,
		Status: session.StatusWaiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSessionRuntimeIDs(ctx, providerStoreACPOnly.ID, "acpx-provider", "canonical-acp", "native-drift"); err == nil || !strings.Contains(err.Error(), "provider-store verified") {
		t.Fatalf("provider-store native drift error = %v", err)
	}
	providerStoreACPOnly, err = store.GetSession(ctx, providerStoreACPOnly.ID)
	if err != nil || providerStoreACPOnly.NativeSessionID != "" {
		t.Fatalf("provider-store drift changed native evidence: %#v, %v", providerStoreACPOnly, err)
	}
	driftBeforeVerification, err := store.CreateSession(ctx, session.CreateParams{
		ID: "S-drift-before-verification", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: t.TempDir(),
		ACPSessionID: "canonical-before-verification", NativeSessionID: "unverified-native",
		ProviderConversationID: "canonical-before-verification", ConversationIDSource: session.ConversationSourceACP,
		Status: session.StatusWaiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSessionConversation(ctx, driftBeforeVerification.ID, "canonical-before-verification", session.ConversationSourceProviderStore, &verifiedAt); err == nil || !strings.Contains(err.Error(), "does not match provider-store verified") {
		t.Fatalf("provider-store verification accepted prior native drift: %v", err)
	}
	unchanged, err := store.GetSession(ctx, loaded.ID)
	if err != nil || unchanged.ACPSessionID != "acp-session" || unchanged.NativeSessionID != "native-session" || unchanged.ProviderConversationID != "native-session" {
		t.Fatalf("runtime identity changed after rejected evidence: %#v, %v", unchanged, err)
	}
	if _, err := store.GetTask(ctx, "T-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTask missing error = %v", err)
	}
}

func TestConcurrentTaskCreation(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	const count = 64
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created, err := store.CreateTask(ctx, task.CreateParams{Title: fmt.Sprintf("Task %d", i)})
			if err != nil {
				errs <- err
				return
			}
			ids <- created.ID
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Errorf("CreateTask() error = %v", err)
	}
	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("created %d Tasks, want %d", len(seen), count)
	}
}

func TestCreateTaskWithSessionRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t)
	existingTask, err := store.CreateTask(ctx, task.CreateParams{ID: "T-existing", Title: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, session.CreateParams{ID: "S-duplicate", TaskID: existingTask.ID, Agent: "codex"}); err != nil {
		t.Fatal(err)
	}

	_, _, err = store.CreateTaskWithSession(ctx,
		task.CreateParams{ID: "T-should-rollback", Title: "atomic"},
		session.CreateParams{ID: "S-duplicate", Agent: "claude"},
	)
	if err == nil {
		t.Fatal("CreateTaskWithSession() error = nil")
	}
	if _, err := store.GetTask(ctx, "T-should-rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back Task lookup error = %v", err)
	}
}
