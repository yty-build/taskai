// Package git owns repository validation, isolated worktree creation, and
// short-lived filesystem locks around Session checkout allocation.
package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var idComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const maxNestedRepositoryEntries = 256

type Worktree struct {
	Repository string
	Path       string
	Branch     string
}

// RepositorySuggestionError reports a single, nearby checkout without
// silently changing the repository the user selected. Transports can offer
// SuggestedRepositoryPath as an explicit correction and require resubmission.
type RepositorySuggestionError struct {
	InputPath               string
	SuggestedRepositoryPath string
}

func (e *RepositorySuggestionError) Error() string {
	return fmt.Sprintf("validate Git repository %q: not a Git worktree; found one immediate Git repository at %q; use that exact path to continue", e.InputPath, e.SuggestedRepositoryPath)
}

type repositoryVerificationError struct {
	Path  string
	Cause error
}

func (e *repositoryVerificationError) Error() string {
	return fmt.Sprintf("validate Git repository %q: Git metadata exists but could not be verified; check its ownership, permissions, and metadata", e.Path)
}

func (e *repositoryVerificationError) Unwrap() error { return e.Cause }

// ValidateRepository resolves path to the root of a non-bare Git worktree.
func ValidateRepository(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("repository path is required")
	}
	resolved, err := canonicalExisting(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository %q: %w", path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect repository %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository %q is not a directory", resolved)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("validate Git repository %q: the Git executable is unavailable", resolved)
	}
	hasMarker, err := hasRepositoryMarker(resolved)
	if err != nil {
		return "", &repositoryVerificationError{Path: resolved, Cause: err}
	}
	if hasMarker {
		root, err := repositoryRoot(ctx, resolved)
		if err == nil {
			return root, nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("validate Git repository %q: %w", resolved, ctx.Err())
		}
		return "", &repositoryVerificationError{Path: resolved, Cause: err}
	}

	nested, truncated, discoveryErr := immediateRepositories(ctx, resolved)
	if discoveryErr != nil {
		var verification *repositoryVerificationError
		if errors.As(discoveryErr, &verification) {
			return "", verification
		}
		return "", fmt.Errorf("validate Git repository %q: not a Git worktree; inspect immediate child repositories: %w", resolved, discoveryErr)
	}
	if truncated {
		return "", fmt.Errorf("validate Git repository %q: not a Git worktree; automatic child-repository discovery is limited to folders with at most %d entries, so enter the exact repository folder or a path inside it", resolved, maxNestedRepositoryEntries)
	}
	if len(nested) == 1 {
		return "", &RepositorySuggestionError{InputPath: resolved, SuggestedRepositoryPath: nested[0]}
	}
	if len(nested) > 1 {
		return "", fmt.Errorf("validate Git repository %q: not a Git worktree and contains multiple immediate Git repositories; enter one explicitly: %s", resolved, summarizeRepositoryPaths(nested))
	}
	return "", fmt.Errorf("validate Git repository %q: not a Git worktree; enter the repository folder itself or a path inside it", resolved)
}

func hasRepositoryMarker(path string) (bool, error) {
	for candidate := path; ; candidate = filepath.Dir(candidate) {
		if _, err := os.Lstat(filepath.Join(candidate, ".git")); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return false, nil
		}
	}
}

func repositoryRoot(ctx context.Context, path string) (string, error) {
	inside, err := run(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(inside) != "true" {
		return "", fmt.Errorf("not a Git worktree")
	}
	root, err := run(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root %q: %w", path, err)
	}
	root = strings.TrimSpace(root)
	canonical, err := canonicalExisting(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root %q: %w", root, err)
	}
	return canonical, nil
}

// immediateRepositories performs a deliberately shallow and bounded fallback
// for chooser-style input. A directory that contains exactly one checkout is
// unambiguous; deeper trees and multi-repository folders still require an
// explicit path from the caller.
func immediateRepositories(ctx context.Context, parent string) ([]string, bool, error) {
	directory, err := os.Open(parent)
	if err != nil {
		return nil, false, err
	}
	defer directory.Close()

	entries, err := directory.ReadDir(maxNestedRepositoryEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > maxNestedRepositoryEntries {
		return nil, true, nil
	}

	var repositories []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(parent, entry.Name())
		if _, err := os.Lstat(filepath.Join(candidate, ".git")); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, false, err
		}
		root, err := repositoryRoot(ctx, candidate)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, &repositoryVerificationError{Path: candidate, Cause: err}
		}
		repositories = append(repositories, root)
	}
	sort.Strings(repositories)
	return repositories, false, nil
}

func summarizeRepositoryPaths(paths []string) string {
	const displayed = 4
	limit := min(len(paths), displayed)
	quoted := make([]string, 0, limit)
	for _, path := range paths[:limit] {
		quoted = append(quoted, fmt.Sprintf("%q", path))
	}
	summary := strings.Join(quoted, ", ")
	if remaining := len(paths) - limit; remaining > 0 {
		summary += fmt.Sprintf(", and %d more", remaining)
	}
	return summary
}

func CurrentBranch(ctx context.Context, repository string) (string, error) {
	branch, err := run(ctx, repository, "symbolic-ref", "--short", "HEAD")
	if err == nil {
		return strings.TrimSpace(branch), nil
	}
	branch, err = run(ctx, repository, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read current Git branch: %w", err)
	}
	return strings.TrimSpace(branch), nil
}

func PlanWorktree(root, repository, taskID, sessionID string) (Worktree, error) {
	if strings.TrimSpace(root) == "" {
		return Worktree{}, fmt.Errorf("worktree root is required")
	}
	if !idComponent.MatchString(taskID) {
		return Worktree{}, fmt.Errorf("Task ID %q is not safe for a Git branch", taskID)
	}
	if !idComponent.MatchString(sessionID) {
		return Worktree{}, fmt.Errorf("Session ID %q is not safe for a Git branch", sessionID)
	}
	root, err := CanonicalPath(root)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return Worktree{
		Repository: repository,
		Path:       filepath.Join(filepath.Clean(root), taskID+"-"+sessionID),
		Branch:     "agent/" + taskID + "/" + sessionID,
	}, nil
}

// CanonicalPath resolves symlinks in the deepest existing parent while
// preserving any not-yet-created suffix. This keeps planned checkout paths and
// command working directories in the same physical namespace.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(abs)
	var missing []string
	for {
		if _, err := os.Lstat(candidate); err == nil {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", fmt.Errorf("no existing parent for %q", path)
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

// CheckAvailable rejects collisions without deleting or changing anything.
func (w Worktree) CheckAvailable(ctx context.Context) error {
	if _, err := os.Lstat(w.Path); err == nil {
		return fmt.Errorf("worktree path %q already exists; it was left unchanged", w.Path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect worktree path %q: %w", w.Path, err)
	}
	if _, err := run(ctx, w.Repository, "rev-parse", "--verify", "HEAD"); err != nil {
		return fmt.Errorf("create worktree from repository %q: HEAD has no commit: %w", w.Repository, err)
	}
	command := exec.CommandContext(ctx, "git", "-C", w.Repository, "show-ref", "--verify", "--quiet", "refs/heads/"+w.Branch)
	err := command.Run()
	if err == nil {
		return fmt.Errorf("Git branch %q already exists; it was left unchanged", w.Branch)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("check Git branch %q: %w", w.Branch, err)
	}
	return nil
}

// Create adds the planned worktree and branch. It deliberately has no cleanup
// counterpart: product-created branches and worktrees are user work products.
func (w Worktree) Create(ctx context.Context) error {
	if err := w.CheckAvailable(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o700); err != nil {
		return fmt.Errorf("create worktree parent: %w", err)
	}
	if err := os.Chmod(filepath.Dir(w.Path), 0o700); err != nil {
		return fmt.Errorf("secure worktree parent: %w", err)
	}
	if _, err := run(ctx, w.Repository, "worktree", "add", "-b", w.Branch, w.Path, "HEAD"); err != nil {
		return fmt.Errorf("create worktree %q on branch %q: %w", w.Path, w.Branch, err)
	}
	return nil
}

// LockRepository serializes checkout allocation across agent processes. Every
// linked worktree of one repository hashes the same canonical Git common
// directory, so alternate main/worktree path representations cannot split the
// exclusion domain. The caller must close the returned file to release it.
func LockRepository(ctx context.Context, stateRoot, repository string) (*os.File, error) {
	identity, err := commonDirectory(ctx, repository)
	if err != nil {
		return nil, err
	}
	locks := filepath.Join(stateRoot, "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, fmt.Errorf("create repository lock directory: %w", err)
	}
	if err := os.Chmod(locks, 0o700); err != nil {
		return nil, fmt.Errorf("secure repository lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(identity))
	path := filepath.Join(locks, fmt.Sprintf("repo-%x.lock", digest[:16]))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open repository lock: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure repository lock: %w", err)
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("lock repository: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("wait for repository lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func UnlockRepository(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock repository: %w", unlockErr)
	}
	return closeErr
}

func SameCheckout(left, right string) bool {
	leftPath, leftErr := canonicalExisting(left)
	rightPath, rightErr := canonicalExisting(right)
	return leftErr == nil && rightErr == nil && leftPath == rightPath
}

func SameRepository(ctx context.Context, left, right string) (bool, error) {
	leftCommon, err := commonDirectory(ctx, left)
	if err != nil {
		return false, err
	}
	rightCommon, err := commonDirectory(ctx, right)
	if err != nil {
		return false, err
	}
	return leftCommon == rightCommon, nil
}

func commonDirectory(ctx context.Context, repository string) (string, error) {
	common, err := run(ctx, repository, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(repository, common)
	}
	resolved, err := canonicalExisting(common)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory %q: %w", common, err)
	}
	return resolved, nil
}

func canonicalExisting(path string) (string, error) {
	resolved, err := CanonicalPath(path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func run(ctx context.Context, repository string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(arguments, " "), detail, err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
	}
	return string(output), nil
}
