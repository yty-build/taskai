package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHomeCreatesPrivateLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv(homeEnv, root)

	home, err := ResolveHome()
	if err != nil {
		t.Fatalf("ResolveHome() error = %v", err)
	}
	if home.Database != filepath.Join(root, "agent.db") {
		t.Fatalf("Database = %q", home.Database)
	}

	for _, path := range []string{home.Root, home.Agents, home.Workers, home.Logs, home.Worktrees} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("permissions for %q = %o, want 700", path, got)
		}
	}
}

func TestResolveHomeRejectsFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(homeEnv, root)

	if _, err := ResolveHome(); err == nil {
		t.Fatal("ResolveHome() error = nil, want an error")
	}
}
