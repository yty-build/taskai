package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

type ReviewSessionParams struct {
	ProducerID          string
	Agent               string
	Model               string
	ReasoningEffort     string
	AllowActiveProducer bool
}

func (s *SessionService) Review(ctx context.Context, params ReviewSessionParams) (LaunchResult, error) {
	producer, err := s.repository.GetSession(ctx, params.ProducerID)
	if err != nil {
		return LaunchResult{}, err
	}
	if strings.TrimSpace(params.Agent) == "" {
		return LaunchResult{}, fmt.Errorf("reviewer agent is required")
	}
	if !params.AllowActiveProducer && !safeReviewProducer(producer) {
		return LaunchResult{}, fmt.Errorf("producer Session %s checkout may still change (status %s, worker PID %d); stop it with 'acpctl session stop %s' before review, or pass --allow-active-producer only when concurrent review is intentional", producer.ID, producer.Status, producer.WorkerPID, producer.ID)
	}
	taskItem, err := s.repository.GetTask(ctx, producer.TaskID)
	if err != nil {
		return LaunchResult{}, err
	}
	checkout := producer.WorktreePath
	if checkout == "" {
		checkout = producer.RepoPath
	}
	snapshot, err := gitrepo.CaptureReviewSnapshot(ctx, checkout)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("capture producer Git evidence: %w", err)
	}
	logPath := producer.LogPath
	if logPath == "" {
		logPath = filepath.Join(s.home.Logs, producer.ID+".jsonl")
	}
	prompt := BuildReviewPrompt(taskItem, producer, logPath, snapshot)
	launched, err := s.Start(ctx, StartSessionParams{
		TaskID: producer.TaskID, Agent: params.Agent, Role: "reviewer",
		Model: params.Model, ReasoningEffort: params.ReasoningEffort,
		RepoPath: producer.RepoPath, WorktreePath: producer.WorktreePath, Prompt: prompt,
		ApproveAll: false, AllowSharedCheckout: params.AllowActiveProducer,
	})
	if err != nil {
		return launched, err
	}
	if _, err := s.repository.UpdateTaskStatus(ctx, producer.TaskID, task.StatusReview); err != nil {
		return launched, err
	}
	return launched, nil
}

func safeReviewProducer(producer session.Session) bool {
	if producer.WorkerPID != 0 || producer.WorkerSocket != "" {
		return false
	}
	switch producer.Status {
	case session.StatusDone, session.StatusFailed, session.StatusCancelled, session.StatusStopped:
		return true
	default:
		return false
	}
}

func BuildReviewPrompt(taskItem task.Task, producer session.Session, logPath string, snapshot gitrepo.ReviewSnapshot) string {
	worktree := producer.WorktreePath
	if worktree == "" {
		worktree = "(same as repository checkout)"
	}
	status := snapshot.Status
	if status == "" {
		status = "(clean)"
	}
	staged := snapshot.StagedDiff
	if staged == "" {
		staged = "(none)"
	}
	unstaged := snapshot.UnstagedDiff
	if unstaged == "" {
		unstaged = "(none)"
	}
	truncation := ""
	if snapshot.Truncated {
		truncation = "\nThe embedded diff was truncated for process safety. Run git diff and git diff --cached yourself before reaching a conclusion.\n"
	}
	return fmt.Sprintf(`Perform an independent, skeptical review. This is a new reviewer Session; do not treat the producer's conversation as your own context.

Original Task: %s — %s
Task description: %s
Producer Session ID: %s
Producer JSONL log (read-only evidence): %s
Repository: %s
Worktree: %s
Recorded producer branch: %s
Snapshot branch: %s

Git status at review start:
%s

Staged diff at review start:
%s

Unstaged diff at review start:
%s
%s
Review both what the producer created and how the producer behaved. Read the producer JSONL journal for commands, tool output, errors, and claims, but do not treat that journal as this Session's conversation. Independently rerun every critical verification needed to support your findings. Treat producer evidence as read-only; do not repair the work during review.

Explicit suspicious-behavior checklist:
- ignored test failures
- selective tests presented as complete tests
- failed commands that were ignored
- claims unsupported by tool output
- suspicious shortcuts
- unrelated file changes
- repeated broken approaches
- security regressions

Report findings with concrete file/evidence references, then state residual risks and the verification you independently ran.`,
		taskItem.ID, taskItem.Title, valueOrReview(taskItem.Description, "(none)"), producer.ID, logPath,
		producer.RepoPath, worktree, valueOrReview(producer.Branch, "(unknown)"), valueOrReview(snapshot.Branch, "(unknown)"),
		status, staged, unstaged, truncation,
	)
}

func valueOrReview(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
