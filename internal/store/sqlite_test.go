package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

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
