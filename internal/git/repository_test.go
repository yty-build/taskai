package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRepositoryResolvesRootAndRejectsNonGit(t *testing.T) {
	ctx := context.Background()
	repository := initializeRepository(t)
	subdirectory := filepath.Join(repository, "nested", "path")
	if err := os.MkdirAll(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := ValidateRepository(ctx, subdirectory)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(repository)
	if resolved != want {
		t.Fatalf("resolved repository = %q, want %q", resolved, want)
	}
	if _, err := ValidateRepository(ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "validate Git repository") {
		t.Fatalf("non-Git directory error = %v", err)
	}
}

func TestCreateMultipleIsolatedWorktreesAndPreserveCollisions(t *testing.T) {
	ctx := context.Background()
	repository := initializeRepository(t)
	root := filepath.Join(t.TempDir(), "worktrees")
	first, err := PlanWorktree(root, repository, "T-1", "S-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanWorktree(root, repository, "T-1", "S-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, worktree := range []Worktree{first, second} {
		if err := worktree.Create(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(worktree.Path, "README.md")); err != nil {
			t.Fatalf("worktree %q missing committed file: %v", worktree.Path, err)
		}
		branch := gitOutput(t, repository, "-C", worktree.Path, "branch", "--show-current")
		if strings.TrimSpace(branch) != worktree.Branch {
			t.Fatalf("worktree branch = %q, want %q", strings.TrimSpace(branch), worktree.Branch)
		}
		same, err := SameRepository(ctx, repository, worktree.Path)
		if err != nil || !same {
			t.Fatalf("worktree repository association = %v, %v", same, err)
		}
	}
	unrelated := initializeRepository(t)
	if same, err := SameRepository(ctx, repository, unrelated); err != nil || same {
		t.Fatalf("unrelated repository association = %v, %v", same, err)
	}

	marker := filepath.Join(first.Path, "keep-me")
	if err := os.WriteFile(marker, []byte("user work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Create(ctx); err == nil || !strings.Contains(err.Error(), "left unchanged") {
		t.Fatalf("duplicate worktree error = %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "user work" {
		t.Fatalf("collision changed worktree: body=%q err=%v", body, err)
	}
}

func TestWorktreePreflightRejectsExistingPathAndBranch(t *testing.T) {
	ctx := context.Background()
	repository := initializeRepository(t)
	root := filepath.Join(t.TempDir(), "worktrees")
	worktree, err := PlanWorktree(root, repository, "T-2", "S-path")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worktree.CheckAvailable(ctx); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("path collision error = %v", err)
	}

	branchWorktree, err := PlanWorktree(root, repository, "T-2", "S-branch")
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repository, "branch", branchWorktree.Branch)
	if err := branchWorktree.CheckAvailable(ctx); err == nil || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("branch collision error = %v", err)
	}
}

func TestRepositoryLockHonorsContextAndReleases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repository := initializeRepository(t)
	first, err := LockRepository(ctx, root, repository)
	if err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, err := LockRepository(waitContext, root, repository); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("contended lock error = %v", err)
	}
	if err := UnlockRepository(first); err != nil {
		t.Fatal(err)
	}
	second, err := LockRepository(ctx, root, repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := UnlockRepository(second); err != nil {
		t.Fatal(err)
	}
}

func initializeRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repository, "init", "-b", "main")
	gitOutput(t, repository, "config", "user.name", "Agent Test")
	gitOutput(t, repository, "config", "user.email", "agent@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repository, "add", "README.md")
	gitOutput(t, repository, "commit", "-m", "fixture")
	return repository
}

func gitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
