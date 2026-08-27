package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/session"
)

func TestSchemaContainsOnlyApprovedTablesAndColumns(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	rows, err := store.db.QueryContext(ctx, `
		SELECT name FROM sqlite_schema
		WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	wantTables := []string{"provider_settings", "sessions", "tasks"}
	if fmt.Sprint(tables) != fmt.Sprint(wantTables) {
		t.Fatalf("tables = %v, want %v", tables, wantTables)
	}

	wantColumns := map[string][]string{
		"tasks": {"id", "title", "created_at", "updated_at"},
		"sessions": {
			"id", "task_id", "parent_session_id", "role", "label", "provider_id",
			"provider_reference", "state", "created_at", "last_active_at",
		},
		"provider_settings": {
			"provider_id", "enabled", "probe_status", "launch_config_json",
			"can_manager", "can_subordinate", "can_resume", "can_replay",
		},
	}
	for table, want := range wantColumns {
		columnRows, err := store.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for columnRows.Next() {
			var cid, notNull, primaryKey int
			var name, dataType string
			var defaultValue any
			if err := columnRows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
				columnRows.Close()
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := columnRows.Close(); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("columns %s = %v, want %v", table, got, want)
		}
	}

	rows, err = store.db.QueryContext(ctx, `SELECT lower(sql) FROM sqlite_schema WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prohibited := []string{
		"prompt", "answer", "message", "tool_", "event", "transcript", "replay_content",
		"native_path", "invocation", "content_index", "credential", "token_stream", "cookie",
	}
	sort.Strings(prohibited)
	for rows.Next() {
		var sqlText string
		if err := rows.Scan(&sqlText); err != nil {
			t.Fatal(err)
		}
		for _, term := range prohibited {
			if strings.Contains(sqlText, term) {
				t.Errorf("schema contains prohibited term %q in %q", term, sqlText)
			}
		}
	}
}

func TestSchemaConstraintsRejectInvalidLifecycleAndHierarchy(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_schema", "schema", fixedTime)
	manager := createManager(t, store, "manager_schema", "task_schema", session.StateActive, fixedTime)
	created := toDatabaseTime(fixedTime)
	tests := []struct {
		name       string
		id         string
		parent     any
		role       string
		state      string
		reference  any
		providerID string
	}{
		{name: "manager with parent", id: "bad_manager_parent", parent: string(manager.ID), role: "manager", state: "active", providerID: "codex"},
		{name: "subordinate without parent", id: "bad_child_parent", parent: nil, role: "subordinate", state: "active", providerID: "codex"},
		{name: "unknown role", id: "bad_role", parent: nil, role: "coordinator", state: "active", providerID: "codex"},
		{name: "unknown state", id: "bad_state", parent: nil, role: "manager", state: "working", providerID: "codex"},
		{name: "text provider reference", id: "bad_reference", parent: nil, role: "manager", state: "active", reference: "not-a-blob", providerID: "codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.db.ExecContext(context.Background(), `
				INSERT INTO sessions(
					id, task_id, parent_session_id, role, label, provider_id,
					provider_reference, state, created_at, last_active_at
				) VALUES (?, 'task_schema', ?, ?, '', ?, ?, ?, ?, ?)`,
				test.id, test.parent, test.role, test.providerID, test.reference, test.state, created, created,
			)
			if err == nil {
				t.Fatal("invalid Session row unexpectedly succeeded")
			}
		})
	}
	violations, err := store.db.QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer violations.Close()
	if violations.Next() {
		t.Fatal("foreign_key_check reported a violation")
	}
	if err := violations.Err(); err != nil {
		t.Fatal(err)
	}
}
