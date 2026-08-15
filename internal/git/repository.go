// Package git owns repository validation, isolated worktree creation, and
// short-lived filesystem locks around Session checkout allocation.
package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var idComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type Worktree struct {
	Repository string
	Path       string
	Branch     string
}

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
	inside, err := run(ctx, resolved, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		if err == nil {
			err = fmt.Errorf("not a Git worktree")
		}
		return "", fmt.Errorf("validate Git repository %q: %w", resolved, err)
	}
	root, err := run(ctx, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root %q: %w", resolved, err)
	}
	root = strings.TrimSpace(root)
	canonical, err := canonicalExisting(root)
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root %q: %w", root, err)
	}
	return canonical, nil
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
	root, err := filepath.Abs(root)
	if err != nil {
		return Worktree{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return Worktree{
		Repository: repository,
		Path:       filepath.Join(filepath.Clean(root), taskID+"-"+sessionID),
		Branch:     "agent/" + taskID + "/" + sessionID,
	}, nil
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

// LockRepository serializes checkout allocation across agent processes. The
// caller must close the returned file to release the advisory lock.
func LockRepository(ctx context.Context, stateRoot, repository string) (*os.File, error) {
	locks := filepath.Join(stateRoot, "locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, fmt.Errorf("create repository lock directory: %w", err)
	}
	if err := os.Chmod(locks, 0o700); err != nil {
		return nil, fmt.Errorf("secure repository lock directory: %w", err)
	}
	digest := sha256.Sum256([]byte(repository))
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
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
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
