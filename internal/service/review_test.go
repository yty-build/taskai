package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestReviewCreatesIndependentSessionWithCompleteEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Logs: filepath.Join(root, "logs"), Workers: filepath.Join(root, "workers"), Worktrees: filepath.Join(root, "worktrees"),
	}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	taskItem, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-review", Title: "Fix login", Description: "Repair the production regression"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	canonicalRepo, err := gitrepo.ValidateRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	producerWorktree, err := gitrepo.PlanWorktree(home.Worktrees, canonicalRepo, taskItem.ID, "S-producer")
	if err != nil {
		t.Fatal(err)
	}
	if err := producerWorktree.Create(ctx); err != nil {
		t.Fatalf("create producer worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(producerWorktree.Path, "README.md"), []byte("producer change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	producerLog := filepath.Join(home.Logs, "S-producer.jsonl")
	if err := os.MkdirAll(home.Logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(producerLog, []byte(`{"seq":1,"type":"agent_message","data":{"message":"SECRET_PRODUCER_TRANSCRIPT"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	producer, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-producer", TaskID: taskItem.ID, Role: "implementer", Agent: "codex", Provider: "openai",
		RepoPath: canonicalRepo, WorktreePath: producerWorktree.Path, Branch: producerWorktree.Branch,
		LogPath: producerLog, Status: session.StatusStopped,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &recordingRuntime{}
	service := NewSessionService(repository, registry, home, runtime)
	launched, err := service.Review(ctx, ReviewSessionParams{ProducerID: producer.ID, Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := launched.Session
	if reviewer.ID == producer.ID || reviewer.TaskID != producer.TaskID || reviewer.Role != "reviewer" || reviewer.Agent != "claude" {
		t.Fatalf("reviewer identity = %#v", reviewer)
	}
	if reviewer.RepoPath != producer.RepoPath || !gitrepo.SameCheckout(reviewer.WorktreePath, producer.WorktreePath) || reviewer.Branch != producer.Branch {
		t.Fatalf("reviewer checkout = %#v, producer = %#v", reviewer, producer)
	}
	if reviewer.ACPSessionID != "" || reviewer.NativeSessionID != "" || reviewer.LogPath == producer.LogPath || PermissionLabel(reviewer) != "read-approval" {
		t.Fatalf("reviewer was not independent/read-approval: %#v", reviewer)
	}
	runtime.mu.Lock()
	commands := append([]worker.Command(nil), runtime.commands...)
	runtime.mu.Unlock()
	if len(commands) != 1 {
		t.Fatalf("review launch commands = %#v", commands)
	}
	prompt := commands[0].Message
	for _, expected := range []string{
		"Task T-review: Fix login", "Description: Repair the production regression", "Role: reviewer",
		"Original Task: T-review — Fix login", "Producer Session ID: S-producer", "Producer JSONL log (read-only evidence): " + producerLog,
		"Repository: " + canonicalRepo, "Worktree: " + producerWorktree.Path, "Recorded producer branch: " + producerWorktree.Branch,
		"producer change", "Independently rerun", "ignored test failures", "selective tests presented as complete tests",
		"failed commands that were ignored", "claims unsupported by tool output", "suspicious shortcuts", "unrelated file changes",
		"repeated broken approaches", "security regressions", "do not treat that journal as this Session's conversation",
	} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("review prompt missing %q:\n%s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "SECRET_PRODUCER_TRANSCRIPT") {
		t.Fatal("producer conversation was imported into reviewer prompt")
	}
	updatedTask, err := repository.GetTask(ctx, taskItem.ID)
	if err != nil || updatedTask.Status != task.StatusReview {
		t.Fatalf("Task after review = %#v, %v", updatedTask, err)
	}
}

func TestReviewRejectsChangingProducerUnlessExplicit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"), Logs: filepath.Join(root, "logs")}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	taskItem, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-active-review", Title: "Active producer"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	canonicalRepo, err := gitrepo.ValidateRepository(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	producer, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-active-producer", TaskID: taskItem.ID, Role: "implementer", Agent: "codex", RepoPath: canonicalRepo,
		Status: session.StatusWaiting, WorkerPID: 99, WorkerSocket: filepath.Join(root, "producer.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, registry, home, &recordingRuntime{})
	if _, err := service.Review(ctx, ReviewSessionParams{ProducerID: producer.ID, Agent: "claude"}); err == nil || !strings.Contains(err.Error(), "acpctl session stop") || !strings.Contains(err.Error(), "--allow-active-producer") {
		t.Fatalf("active producer review error = %v", err)
	}
	sessions, err := repository.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("rejected review created Session: %d, %v", len(sessions), err)
	}
	launched, err := service.Review(ctx, ReviewSessionParams{ProducerID: producer.ID, Agent: "claude", AllowActiveProducer: true})
	if err != nil {
		t.Fatalf("explicit active producer review: %v", err)
	}
	if launched.Session.Role != "reviewer" || launched.Session.TaskID != producer.TaskID {
		t.Fatalf("explicit reviewer = %#v", launched.Session)
	}
}
