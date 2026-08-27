//go:build darwin || linux

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/store"
)

func TestWorkspaceBindingIsRestrictiveOneWayAndProjectExact(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	project := filepath.Join(root, "project-a")
	otherProject := filepath.Join(root, "project-b")

	if err := inspectWorkspaceBinding(stateDir, project, false); err != nil {
		t.Fatalf("absent pre-acquire binding = %v", err)
	}
	if err := establishWorkspaceBinding(stateDir, project); err != nil {
		t.Fatal(err)
	}
	if err := establishWorkspaceBinding(stateDir, project); err != nil {
		t.Fatalf("idempotent same-project binding = %v", err)
	}
	if err := inspectWorkspaceBinding(stateDir, project, true); err != nil {
		t.Fatalf("same-project binding = %v", err)
	}
	if err := inspectWorkspaceBinding(stateDir, otherProject, true); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("other-project binding = %v, want conflict", err)
	}
	if err := establishWorkspaceBinding(stateDir, otherProject); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("other-project establish = %v, want conflict", err)
	}

	rootInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	bindingPath := filepath.Join(stateDir, workspaceBindingFilename)
	bindingInfo, err := os.Lstat(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 || bindingInfo.Mode().Perm() != 0o600 || !bindingInfo.Mode().IsRegular() {
		t.Fatalf("binding modes root=%o file=%v", rootInfo.Mode().Perm(), bindingInfo.Mode())
	}
	content, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := workspaceBindingValue(project)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, expected) || bytes.Contains(content, []byte(project)) || len(content) != len(workspaceBindingPrefix)+64+1 {
		t.Fatalf("workspace binding is not the exact one-way v1 envelope: %q", content)
	}
}

func TestCanonicalProjectCWDResolvesAndPinsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "project-first")
	second := filepath.Join(root, "project-second")
	for _, directory := range []string{first, second} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "project-link")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	canonicalFirst, err := canonicalProjectCWD(alias)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst, err := filepath.EvalSymlinks(first)
	if err != nil || canonicalFirst != filepath.Clean(wantFirst) {
		t.Fatalf("canonical first = %q, %v; want %q", canonicalFirst, err, filepath.Clean(wantFirst))
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	canonicalSecond, err := canonicalProjectCWD(alias)
	if err != nil || canonicalSecond == canonicalFirst {
		t.Fatalf("retargeted canonical cwd = %q, %v; previous %q", canonicalSecond, err, canonicalFirst)
	}
	firstBinding, err := workspaceBindingValue(canonicalFirst)
	if err != nil {
		t.Fatal(err)
	}
	secondBinding, err := workspaceBindingValue(canonicalSecond)
	if err != nil || bytes.Equal(firstBinding, secondBinding) {
		t.Fatalf("retargeted binding reused first project: equal=%v error=%v", bytes.Equal(firstBinding, secondBinding), err)
	}
}

func TestWorkspaceBindingRejectsUnsafeEntriesAndUnboundState(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "target")
		canary := []byte("workspace-binding-target-canary")
		if err := os.WriteFile(target, canary, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(stateDir, workspaceBindingFilename)); err != nil {
			t.Fatal(err)
		}
		if err := inspectWorkspaceBinding(stateDir, filepath.Join(root, "project"), true); !errors.Is(err, runtime.ErrEndpointUnsafe) {
			t.Fatalf("symlink binding error = %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, canary) {
			t.Fatalf("symlink target changed: %q, %v", got, err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "target")
		expected, err := workspaceBindingValue(filepath.Join(root, "project"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, expected, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(target, filepath.Join(stateDir, workspaceBindingFilename)); err != nil {
			t.Fatal(err)
		}
		if err := inspectWorkspaceBinding(stateDir, filepath.Join(root, "project"), true); !errors.Is(err, runtime.ErrEndpointUnsafe) {
			t.Fatalf("hardlink binding error = %v", err)
		}
	})

	t.Run("unbound nonempty root", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := os.Mkdir(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "legacy-metadata"), []byte("canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		project := filepath.Join(root, "project")
		if err := inspectWorkspaceBinding(stateDir, project, false); !errors.Is(err, runtime.ErrEndpointUnsafe) {
			t.Fatalf("pre-acquire unbound state error = %v", err)
		}
		if err := establishWorkspaceBinding(stateDir, project); !errors.Is(err, runtime.ErrEndpointUnsafe) {
			t.Fatalf("worker unbound state error = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(stateDir, workspaceBindingFilename)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unsafe root was claimed: %v", err)
		}
	})
}

type closeRecordingPort struct {
	*portFake
	closes int
}

func (port *closeRecordingPort) Close() error {
	port.closes++
	return nil
}

func TestAcquireWorkspaceChecksBindingBeforeAndAfterAttach(t *testing.T) {
	t.Run("pre-acquire mismatch", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		if err := establishWorkspaceBinding(stateDir, filepath.Join(root, "project-a")); err != nil {
			t.Fatal(err)
		}
		dials, spawns := 0, 0
		command := systemCommand{
			dial: func(context.Context, string) (runtime.ClientPort, error) {
				dials++
				return &portFake{}, nil
			},
			spawn: func() error { spawns++; return nil },
		}
		if _, err := command.acquireWorkspace(t.Context(), stateDir, filepath.Join(root, "project-b")); !errors.Is(err, runtime.ErrConflict) {
			t.Fatalf("pre-acquire mismatch = %v", err)
		}
		if dials != 0 || spawns != 0 {
			t.Fatalf("mismatch contacted runtime: dials=%d spawns=%d", dials, spawns)
		}
	})

	t.Run("post-acquire race", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		port := &closeRecordingPort{portFake: &portFake{}}
		command := systemCommand{dial: func(context.Context, string) (runtime.ClientPort, error) {
			if err := establishWorkspaceBinding(stateDir, filepath.Join(root, "winning-project")); err != nil {
				t.Fatal(err)
			}
			return port, nil
		}}
		if _, err := command.acquireWorkspace(t.Context(), stateDir, filepath.Join(root, "losing-project")); !errors.Is(err, runtime.ErrConflict) {
			t.Fatalf("post-acquire mismatch = %v", err)
		}
		if port.closes != 1 {
			t.Fatalf("mismatched attached client closes = %d, want 1", port.closes)
		}
	})

	t.Run("same project", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		project := filepath.Join(root, "project")
		if err := establishWorkspaceBinding(stateDir, project); err != nil {
			t.Fatal(err)
		}
		port := &closeRecordingPort{portFake: &portFake{}}
		command := systemCommand{dial: func(context.Context, string) (runtime.ClientPort, error) { return port, nil }}
		got, err := command.acquireWorkspace(t.Context(), stateDir, project)
		if err != nil || got != port || port.closes != 0 {
			t.Fatalf("same-project client=%p error=%v closes=%d", got, err, port.closes)
		}
	})
}

func TestWorkerRejectsForeignWorkspaceBeforeStoreOrEndpoint(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	foreignProject := filepath.Join(t.TempDir(), "foreign-project")
	if err := establishWorkspaceBinding(stateDir, foreignProject); err != nil {
		t.Fatal(err)
	}
	t.Setenv(StateDirEnvironment, stateDir)
	if err := (systemCommand{}).Work(t.Context()); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("foreign workspace Work error = %v", err)
	}
	for _, name := range []string{store.DatabaseFilename, runtime.SocketFilename, runtime.LockFilename} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("foreign workspace created %s: %v", name, err)
		}
	}
}
