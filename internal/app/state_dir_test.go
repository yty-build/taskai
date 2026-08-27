package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStateDirAbsoluteOverrideWithoutCreation(t *testing.T) {
	override := filepath.Join(t.TempDir(), "nested", "taskai")
	t.Setenv(StateDirEnvironment, override)

	got, err := ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir() error = %v", err)
	}
	if got != filepath.Clean(override) {
		t.Fatalf("ResolveStateDir() = %q, want %q", got, filepath.Clean(override))
	}
	if _, err := os.Stat(override); !os.IsNotExist(err) {
		t.Fatalf("ResolveStateDir() created or inspected into existence: stat error = %v", err)
	}
}

func TestResolveStateDirRejectsRelativeOverride(t *testing.T) {
	t.Setenv(StateDirEnvironment, filepath.Join("relative", "taskai"))

	got, err := ResolveStateDir()
	if got != "" {
		t.Fatalf("ResolveStateDir() = %q, want empty path", got)
	}
	if !errors.Is(err, errRelativeStateDir) {
		t.Fatalf("ResolveStateDir() error = %v, want %v", err, errRelativeStateDir)
	}
}

func TestResolveStateDirDefaultNamespaceWithoutCreation(t *testing.T) {
	t.Setenv(StateDirEnvironment, "")

	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir() error = %v", err)
	}
	want := filepath.Join(configDir, "taskai")

	got, err := ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir() error = %v", err)
	}
	if got != want {
		t.Fatalf("ResolveStateDir() = %q, want %q", got, want)
	}
}
