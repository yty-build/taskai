package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const homeEnv = "AGENT_HOME"

// Home contains every path owned by Agent Task Console.
type Home struct {
	Root      string
	Database  string
	Agents    string
	Workers   string
	Logs      string
	Worktrees string
}

// ResolveHome resolves AGENT_HOME and creates private state directories.
func ResolveHome() (Home, error) {
	root := strings.TrimSpace(os.Getenv(homeEnv))
	if root == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Home{}, fmt.Errorf("resolve user home: %w", err)
		}
		root = filepath.Join(userHome, ".agent")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Home{}, fmt.Errorf("resolve %s: %w", homeEnv, err)
	}

	home := Home{
		Root:      absRoot,
		Database:  filepath.Join(absRoot, "agent.db"),
		Agents:    filepath.Join(absRoot, "agents"),
		Workers:   filepath.Join(absRoot, "workers"),
		Logs:      filepath.Join(absRoot, "logs"),
		Worktrees: filepath.Join(absRoot, "worktrees"),
	}

	for _, dir := range []string{home.Root, home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := ensurePrivateDir(dir); err != nil {
			return Home{}, err
		}
	}

	return home, nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create state directory %q: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect state directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state path %q is not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure state directory %q: %w", path, err)
	}
	return nil
}
