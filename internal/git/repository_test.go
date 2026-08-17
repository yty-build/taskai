package git

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := ValidateRepository(ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "enter the repository folder itself") || strings.Contains(err.Error(), "fatal:") {
		t.Fatalf("non-Git directory error is not actionable = %v", err)
	}
}

func TestValidateRepositorySuggestsOnlyImmediateChildWithoutSelectingIt(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	repository := filepath.Join(parent, "site")
	initializeRepositoryAt(t, repository)
	if err := os.WriteFile(filepath.Join(parent, "business-plan.html"), []byte("not a repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved, err := ValidateRepository(ctx, parent)
	if resolved != "" {
		t.Fatalf("ambiguous parent was silently rewritten to %q", resolved)
	}
	var suggestion *RepositorySuggestionError
	if !errors.As(err, &suggestion) {
		t.Fatalf("unique nested repository error = %T %v, want RepositorySuggestionError", err, err)
	}
	want, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.SuggestedRepositoryPath != want || !strings.Contains(suggestion.Error(), "use that exact path") {
		t.Fatalf("repository suggestion = %#v, want %q", suggestion, want)
	}
}

func TestValidateRepositoryRequiresExplicitChoiceForMultipleChildren(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	first := filepath.Join(parent, "api")
	second := filepath.Join(parent, "web")
	initializeRepositoryAt(t, first)
	initializeRepositoryAt(t, second)

	_, err := ValidateRepository(ctx, parent)
	if err == nil || !strings.Contains(err.Error(), "multiple immediate Git repositories") || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Fatalf("multiple repository error = %v", err)
	}
	var suggestion *RepositorySuggestionError
	if errors.As(err, &suggestion) {
		t.Fatalf("multiple repositories produced a single unsafe suggestion: %#v", suggestion)
	}
}

func TestValidateRepositoryNestedDiscoveryIsBounded(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	initializeRepositoryAt(t, filepath.Join(parent, "repository"))
	for index := 0; index < maxNestedRepositoryEntries; index++ {
		if err := os.Mkdir(filepath.Join(parent, fmt.Sprintf("folder-%03d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	_, err := ValidateRepository(ctx, parent)
	if err == nil || !strings.Contains(err.Error(), "at most 256 entries") || !strings.Contains(err.Error(), "enter the exact repository folder") {
		t.Fatalf("bounded discovery error = %v", err)
	}
	var suggestion *RepositorySuggestionError
	if errors.As(err, &suggestion) {
		t.Fatalf("truncated discovery offered a potentially incomplete suggestion: %#v", suggestion)
	}
}

func TestValidateRepositoryCanonicalizesParentAliasAndDoesNotTraverseChildAlias(t *testing.T) {
	ctx := context.Background()
	realParent := t.TempDir()
	repository := filepath.Join(realParent, "site")
	initializeRepositoryAt(t, repository)
	aliasParent := filepath.Join(t.TempDir(), "projects")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}

	_, err := ValidateRepository(ctx, aliasParent)
	var suggestion *RepositorySuggestionError
	if !errors.As(err, &suggestion) {
		t.Fatalf("aliased parent suggestion = %T %v", err, err)
	}
	canonical, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.SuggestedRepositoryPath != canonical {
		t.Fatalf("aliased parent suggested %q, want canonical %q", suggestion.SuggestedRepositoryPath, canonical)
	}

	childAliasParent := t.TempDir()
	childAlias := filepath.Join(childAliasParent, "site")
	if err := os.Symlink(repository, childAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRepository(ctx, childAliasParent); err == nil || errors.As(err, &suggestion) {
		t.Fatalf("parent discovery traversed a child symlink: %v", err)
	}
	resolved, err := ValidateRepository(ctx, childAlias)
	if err != nil || resolved != canonical {
		t.Fatalf("explicit child alias resolved to %q, %v; want %q", resolved, err, canonical)
	}
}

func TestValidateRepositorySuggestsGitFileWorktree(t *testing.T) {
	ctx := context.Background()
	repository := initializeRepository(t)
	parent := t.TempDir()
	worktree := filepath.Join(parent, "checkout")
	gitOutput(t, repository, "worktree", "add", "-b", "nested-worktree", worktree)
	info, err := os.Stat(filepath.Join(worktree, ".git"))
	if err != nil || info.IsDir() {
		t.Fatalf("fixture did not create a .git file worktree: info=%v err=%v", info, err)
	}

	_, err = ValidateRepository(ctx, parent)
	var suggestion *RepositorySuggestionError
	if !errors.As(err, &suggestion) {
		t.Fatalf("worktree suggestion = %T %v", err, err)
	}
	want, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.SuggestedRepositoryPath != want {
		t.Fatalf("worktree suggestion = %q, want %q", suggestion.SuggestedRepositoryPath, want)
	}
}

func TestValidateRepositoryDoesNotReclassifyOperationalGitFailures(t *testing.T) {
	t.Run("Git executable unavailable", func(t *testing.T) {
		parent := t.TempDir()
		t.Setenv("PATH", "")
		_, err := ValidateRepository(context.Background(), parent)
		if err == nil || !strings.Contains(err.Error(), "Git executable is unavailable") || strings.Contains(err.Error(), "exec:") {
			t.Fatalf("missing Git error = %v", err)
		}
		var suggestion *RepositorySuggestionError
		if errors.As(err, &suggestion) {
			t.Fatalf("missing Git was reclassified as a repository suggestion: %#v", suggestion)
		}
	})

	t.Run("selected checkout metadata cannot be verified", func(t *testing.T) {
		repository := t.TempDir()
		if err := os.Mkdir(filepath.Join(repository, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := ValidateRepository(context.Background(), repository)
		var verification *repositoryVerificationError
		canonical, canonicalErr := filepath.EvalSymlinks(repository)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		if !errors.As(err, &verification) || verification.Path != canonical {
			t.Fatalf("corrupt selected repository error = %T %v", err, err)
		}
		if strings.Contains(err.Error(), "fatal:") || !strings.Contains(err.Error(), "could not be verified") {
			t.Fatalf("corrupt selected repository error was not sanitized: %v", err)
		}
	})

	t.Run("child checkout metadata cannot be verified", func(t *testing.T) {
		parent := t.TempDir()
		candidate := filepath.Join(parent, "site")
		if err := os.MkdirAll(filepath.Join(candidate, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := ValidateRepository(context.Background(), parent)
		var verification *repositoryVerificationError
		canonical, canonicalErr := filepath.EvalSymlinks(candidate)
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		if !errors.As(err, &verification) || verification.Path != canonical {
			t.Fatalf("corrupt child repository error = %T %v", err, err)
		}
		var suggestion *RepositorySuggestionError
		if errors.As(err, &suggestion) || strings.Contains(err.Error(), "fatal:") {
			t.Fatalf("corrupt child was skipped or exposed raw Git output: %v", err)
		}
	})
}

func TestPlanWorktreeCanonicalizesSymlinkedParent(t *testing.T) {
	realParent := t.TempDir()
	realRoot := filepath.Join(realParent, "agent-home", "worktrees")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "tmp-alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}

	planned, err := PlanWorktree(filepath.Join(alias, "agent-home", "worktrees"), "/repo", "T-symlink", "S-canonical")
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalRoot, "T-symlink-S-canonical")
	if planned.Path != want {
		t.Fatalf("planned worktree path = %q, want canonical path %q", planned.Path, want)
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

func TestCaptureReviewSnapshotIncludesBranchStatusAndDiffs(t *testing.T) {
	repository := initializeRepository(t)
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "staged.txt"), []byte("staged evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repository, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("untracked evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureReviewSnapshot(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Branch != "main" || snapshot.Truncated {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
	for _, expected := range []string{"README.md", "staged.txt", "untracked.txt"} {
		if !strings.Contains(snapshot.Status, expected) {
			t.Errorf("status missing %q: %s", expected, snapshot.Status)
		}
	}
	if !strings.Contains(snapshot.StagedDiff, "staged evidence") || !strings.Contains(snapshot.UnstagedDiff, "-fixture") || !strings.Contains(snapshot.UnstagedDiff, "+changed") {
		t.Fatalf("snapshot diffs = staged %q, unstaged %q", snapshot.StagedDiff, snapshot.UnstagedDiff)
	}
}

func initializeRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repo")
	initializeRepositoryAt(t, repository)
	return repository
}

func initializeRepositoryAt(t *testing.T, repository string) {
	t.Helper()
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
