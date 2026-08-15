package native

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/agents"
)

func TestStoreVerifierExactProviderStores(t *testing.T) {
	workingDirectory := t.TempDir()
	tests := []struct {
		name    string
		profile agents.Profile
		id      string
		prepare func(*testing.T, string, string)
	}{
		{
			name: "codex", id: "01-codex", profile: agents.Profile{Name: "codex", Provider: "openai", Command: "codex"},
			prepare: func(t *testing.T, root, id string) {
				path := filepath.Join(root, "sessions", "2026", "08", "15", "rollout-2026-"+id+".jsonl")
				writeFixture(t, path, `{"type":"session_meta","payload":{"id":"`+id+`","cwd":"`+workingDirectory+`"}}`+"\n")
			},
		},
		{
			name: "claude", id: "claude-exact", profile: agents.Profile{Name: "claude", Provider: "anthropic", Command: "claude"},
			prepare: func(t *testing.T, root, id string) {
				path := filepath.Join(root, "projects", "encoded", id+".jsonl")
				writeFixture(t, path, `{"type":"queue-operation"}`+"\n"+`{"sessionId":"`+id+`","cwd":"`+workingDirectory+`"}`+"\n")
			},
		},
		{
			name: "grok", id: "grok-exact", profile: agents.Profile{Name: "grok", Provider: "xai", Command: "grok"},
			prepare: func(t *testing.T, root, id string) {
				group := url.PathEscape(workingDirectory)
				if err := os.MkdirAll(filepath.Join(root, "sessions", group, id), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "opencode", id: "ses_exact", profile: agents.Profile{Name: "opencode", Provider: "opencode", Command: "opencode"},
			prepare: func(t *testing.T, root, id string) {
				databasePath := filepath.Join(root, "opencode.db")
				database, err := sql.Open("sqlite", databasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				if _, err := database.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`); err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`INSERT INTO session (id, directory) VALUES (?, ?)`, id, workingDirectory); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.prepare(t, root, test.id)
			profile := test.profile
			profile.Environment = map[string]string{}
			switch test.name {
			case "codex":
				profile.Environment["CODEX_HOME"] = root
			case "claude":
				profile.Environment["CLAUDE_CONFIG_DIR"] = root
			case "grok":
				profile.Environment["GROK_HOME"] = root
			case "opencode":
				profile.Environment["OPENCODE_DB"] = filepath.Join(root, "opencode.db")
			}
			verified, err := (StoreVerifier{}).Verify(context.Background(), profile, test.id, workingDirectory)
			if err != nil || verified.ConversationID != test.id || verified.StorePath == "" {
				t.Fatalf("Verify() = %#v, %v", verified, err)
			}
			if _, err := (StoreVerifier{}).Verify(context.Background(), profile, test.id, t.TempDir()); err == nil || !strings.Contains(err.Error(), "refusing to open a different conversation") {
				t.Fatalf("mismatched checkout error = %v", err)
			}
		})
	}
}

func TestStoreVerifierRejectsUnsafeAndUnknownConversation(t *testing.T) {
	profile := agents.Profile{Name: "codex", Provider: "openai", Command: "codex", Environment: map[string]string{"CODEX_HOME": t.TempDir()}}
	for _, id := range []string{"", "../escape", "nested/id"} {
		if _, err := (StoreVerifier{}).Verify(context.Background(), profile, id, t.TempDir()); err == nil {
			t.Fatalf("unsafe ID %q accepted", id)
		}
	}
	if _, err := (StoreVerifier{}).Verify(context.Background(), profile, "missing", t.TempDir()); err == nil {
		t.Fatal("missing exact conversation accepted")
	}
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
