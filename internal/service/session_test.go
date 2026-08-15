package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type recordingRuntime struct {
	mu       sync.Mutex
	commands []worker.Command
}

func (r *recordingRuntime) Spawn(context.Context, string) (worker.SpawnResult, error) {
	return worker.SpawnResult{PID: 42, Socket: "recording.sock"}, nil
}

func (r *recordingRuntime) Send(_ context.Context, _ string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	return worker.Message{Type: "accepted", State: "ready", RequestID: command.RequestID}, nil
}

func (r *recordingRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
}

func TestStartPersistsPermissionAndSendsCompleteInitialPrompt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"), Logs: filepath.Join(root, "logs"), Workers: filepath.Join(root, "workers")}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-service", Title: "Investigate edge", Description: "Compare one-hour results"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &recordingRuntime{}
	service := NewSessionService(repository, registry, home, runtime)
	launched, err := service.Start(ctx, StartSessionParams{
		TaskID: createdTask.ID, Agent: "codex", Role: "analyst", RepoPath: repo,
		Prompt: "Check the one-hour horizon too", ApproveAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if launched.RequestID == "" || launched.State != "ready" {
		t.Fatalf("LaunchResult = %#v", launched)
	}
	current, err := repository.GetSession(ctx, launched.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if PermissionLabel(current) != "approve-all" || current.TaskID != createdTask.ID || current.LogPath != filepath.Join(home.Logs, current.ID+".jsonl") {
		t.Fatalf("Session = %#v", current)
	}
	updatedTask, err := repository.GetTask(ctx, createdTask.ID)
	if err != nil || updatedTask.Status != task.StatusWorking {
		t.Fatalf("Task = %#v, %v", updatedTask, err)
	}
	runtime.mu.Lock()
	commands := append([]worker.Command(nil), runtime.commands...)
	runtime.mu.Unlock()
	if len(commands) != 1 || commands[0].Type != "prompt" || commands[0].RequestID != launched.RequestID {
		t.Fatalf("commands = %#v", commands)
	}
	for _, expected := range []string{
		"Task T-service: Investigate edge", "Description: Compare one-hour results", "Session: " + current.ID,
		"Role: analyst", "Repository: " + repo, "Check the one-hour horizon too",
	} {
		if !strings.Contains(commands[0].Message, expected) {
			t.Errorf("initial prompt does not contain %q:\n%s", expected, commands[0].Message)
		}
	}
}

func TestPrepareDefaultsToReadApprovalAndValidatesProfileAndDirectory(t *testing.T) {
	root := t.TempDir()
	home := config.Home{Agents: filepath.Join(root, "agents"), Logs: filepath.Join(root, "logs")}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(nil, registry, home, nil)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := service.Prepare(StartSessionParams{Agent: "codex", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(prepared.MetadataJSON, &metadata); err != nil || metadata["permission"] != "read-approval" || prepared.Role != "worker" {
		t.Fatalf("prepared = %#v, metadata=%v, err=%v", prepared, metadata, err)
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "missing", RepoPath: repo}); err == nil {
		t.Fatal("missing profile accepted")
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "codex", RepoPath: filepath.Join(root, "missing")}); err == nil {
		t.Fatal("missing repository directory accepted")
	}
}

func TestBuildInitialPromptUsesWorktreeAsWorkingContext(t *testing.T) {
	prompt := BuildInitialPrompt(task.Task{ID: "T1", Title: "Title"}, session.Session{
		ID: "S1", Role: "reviewer", RepoPath: "/repo", WorktreePath: "/worktree", Branch: "agent/T1/S1",
	}, "")
	for _, expected := range []string{"Description: (none)", "Repository: /repo", "Worktree: /worktree", "Branch: agent/T1/S1", "context at /worktree"} {
		if !strings.Contains(prompt, expected) {
			t.Errorf("prompt does not contain %q: %s", expected, prompt)
		}
	}
}
