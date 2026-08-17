package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/native"
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
	if command.Type == "ping" {
		return worker.Message{
			Type: "pong", ProtocolVersion: worker.ProtocolVersion,
			Capabilities: []string{worker.CapabilityExclusiveNativeOwner},
		}, nil
	}
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
	initializeGitRepository(t, repo)
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &recordingRuntime{}
	service := NewSessionService(repository, registry, home, runtime)
	launched, err := service.Start(ctx, StartSessionParams{
		TaskID: createdTask.ID, Agent: "codex", Role: "analyst", RepoPath: repo,
		Model: "gpt-5.6-sol", ReasoningEffort: "high",
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
	providerOptions, err := session.ParseProviderOptions(current.MetadataJSON)
	if err != nil || providerOptions.Model != "gpt-5.6-sol" || providerOptions.ReasoningEffort != "high" {
		t.Fatalf("provider options = %#v, %v", providerOptions, err)
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
		"Role: analyst", "Repository: " + current.RepoPath, "Check the one-hour horizon too",
	} {
		if !strings.Contains(commands[0].Message, expected) {
			t.Errorf("initial prompt does not contain %q:\n%s", expected, commands[0].Message)
		}
	}
	if commands[0].DisplayMessage != "Start work on \"Investigate edge\" as analyst.\n\nCompare one-hour results\n\nCheck the one-hour horizon too" {
		t.Fatalf("initial display message = %q", commands[0].DisplayMessage)
	}
	if _, err := repository.UpdateSessionWorker(ctx, current.ID, 42, "recording.sock", current.LogPath, session.StatusWorking); err != nil {
		t.Fatal(err)
	}
	if err := service.DecidePermission(ctx, current.ID, "permission-1:tool-write", "allow-once", false); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	commands = append([]worker.Command(nil), runtime.commands...)
	runtime.mu.Unlock()
	decision := commands[len(commands)-1]
	if decision.Type != "permission_decision" || decision.PermissionID != "permission-1:tool-write" || decision.OptionID != "allow-once" || decision.Cancelled {
		t.Fatalf("permission command = %#v", decision)
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
	initializeGitRepository(t, repo)
	prepared, err := service.Prepare(StartSessionParams{Agent: "codex", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(prepared.MetadataJSON, &metadata); err != nil || metadata["permission"] != "read-approval" || metadata["acp_transport"] != acp.RuntimeTransport || prepared.Role != "worker" {
		t.Fatalf("prepared = %#v, metadata=%v, err=%v", prepared, metadata, err)
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "missing", RepoPath: repo}); err == nil {
		t.Fatal("missing profile accepted")
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "codex", RepoPath: filepath.Join(root, "missing")}); err == nil {
		t.Fatal("missing repository directory accepted")
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "grok", RepoPath: repo, Model: "grok-4.6"}); err == nil || !strings.Contains(err.Error(), "verifiable ACP") {
		t.Fatalf("Grok explicit controls were not rejected before Session creation: %v", err)
	}
	if _, err := service.Prepare(StartSessionParams{Agent: "claude", RepoPath: repo, ReasoningEffort: "default"}); err == nil || !strings.Contains(err.Error(), "leave reasoning effort empty") {
		t.Fatalf("Claude explicit default sentinel was not rejected before Session creation: %v", err)
	}
}

func initializeGitRepository(t *testing.T, path string) {
	t.Helper()
	for _, arguments := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Agent Test"},
		{"config", "user.email", "agent@example.invalid"},
	} {
		command := exec.Command("git", arguments...)
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "README.md"}, {"commit", "-m", "fixture"}} {
		command := exec.Command("git", arguments...)
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
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

func TestStartCreatesDistinctPersistentWorktrees(t *testing.T) {
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
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-worktrees", Title: "Isolate changes"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, registry, home, &recordingRuntime{})

	var started []session.Session
	for _, role := range []string{"implementer", "reviewer"} {
		result, err := service.Start(ctx, StartSessionParams{TaskID: createdTask.ID, Agent: "codex", Role: role, RepoPath: repo, Worktree: true})
		if err != nil {
			t.Fatal(err)
		}
		started = append(started, result.Session)
	}
	if started[0].WorktreePath == started[1].WorktreePath || started[0].Branch == started[1].Branch {
		t.Fatalf("worktrees were not isolated: %#v", started)
	}
	canonicalWorktrees, err := filepath.EvalSymlinks(home.Worktrees)
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range started {
		wantPath := filepath.Join(canonicalWorktrees, createdTask.ID+"-"+current.ID)
		wantBranch := "agent/" + createdTask.ID + "/" + current.ID
		if current.WorktreePath != wantPath || current.Branch != wantBranch {
			t.Errorf("Session context = path %q branch %q, want %q %q", current.WorktreePath, current.Branch, wantPath, wantBranch)
		}
		if _, err := os.Stat(filepath.Join(current.WorktreePath, "README.md")); err != nil {
			t.Errorf("worktree was not created: %v", err)
		}
		persisted, err := repository.GetSession(ctx, current.ID)
		if err != nil || persisted.WorktreePath != wantPath || persisted.Branch != wantBranch || persisted.RepoPath == "" {
			t.Errorf("persisted Session context = %#v, %v", persisted, err)
		}
		if _, err := repository.UpdateSessionStatus(ctx, current.ID, session.StatusDone); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(current.WorktreePath); err != nil {
			t.Errorf("completed Session worktree was removed: %v", err)
		}
	}
}

func TestStartProtectsSharedCheckoutAndExplicitOverride(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees")}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-shared", Title: "Protect checkout"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, registry, home, &recordingRuntime{})
	params := StartSessionParams{TaskID: createdTask.ID, Agent: "codex", RepoPath: repo}
	first, err := service.Start(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Start(ctx, params); err == nil || !strings.Contains(err.Error(), "--allow-shared-checkout") || !strings.Contains(err.Error(), first.Session.ID) {
		t.Fatalf("shared checkout error = %v", err)
	}
	params.AllowSharedCheckout = true
	if _, err := service.Start(ctx, params); err != nil {
		t.Fatalf("explicit shared checkout was rejected: %v", err)
	}
	sessions, err := repository.ListSessions(ctx)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("Sessions after protected/allowed starts = %d, %v", len(sessions), err)
	}
}

func TestConcurrentStartsCannotBothClaimCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees")}
	if err := os.MkdirAll(home.Agents, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-race", Title: "Serialize writers"})
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, registry, home, &recordingRuntime{})
	params := StartSessionParams{TaskID: createdTask.ID, Agent: "codex", RepoPath: repo}
	errorsFound := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := service.Start(ctx, params)
			errorsFound <- err
		}()
	}
	group.Wait()
	close(errorsFound)
	var successes, collisions int
	for err := range errorsFound {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "already used by active Session") {
			collisions++
		} else {
			t.Fatalf("unexpected concurrent start error: %v", err)
		}
	}
	if successes != 1 || collisions != 1 {
		t.Fatalf("concurrent starts: successes=%d collisions=%d", successes, collisions)
	}
}

func TestResumeAlsoProtectsSharedCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Logs: filepath.Join(root, "logs")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-resume-lock", Title: "Protect resumes"})
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
	stopped, err := repository.CreateSession(ctx, session.CreateParams{ID: "S-stopped", TaskID: createdTask.ID, Agent: "codex", RepoPath: canonicalRepo, Status: session.StatusStopped})
	if err != nil {
		t.Fatal(err)
	}
	active, err := repository.CreateSession(ctx, session.CreateParams{ID: "S-active", TaskID: createdTask.ID, Agent: "codex", RepoPath: canonicalRepo, Status: session.StatusWaiting})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(repository, nil, home, &recordingRuntime{})
	if _, err := service.Resume(ctx, stopped.ID); err == nil || !strings.Contains(err.Error(), active.ID) {
		t.Fatalf("protected resume error = %v", err)
	}
	if _, err := service.ResumeWithOptions(ctx, stopped.ID, true); err != nil {
		t.Fatalf("explicit shared resume was rejected: %v", err)
	}
}

type nativeTestRuntime struct {
	mu                              sync.Mutex
	active                          string
	queued                          int
	refreshing                      bool
	owner                           string
	reserved                        bool
	generation                      string
	generationID                    int
	calls                           []string
	refreshState                    string
	refreshErr                      error
	stale                           bool
	staleProtocol                   int
	staleCapabilities               []string
	acquireState                    string
	spawnCalls                      int
	releaseReservationBeforeRecover bool
	permissions                     []worker.PendingPermission
}

func (r *nativeTestRuntime) Spawn(context.Context, string) (worker.SpawnResult, error) {
	r.mu.Lock()
	r.spawnCalls++
	r.mu.Unlock()
	return worker.SpawnResult{}, nil
}

func (r *nativeTestRuntime) Send(_ context.Context, _ string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, command.Type)
	switch command.Type {
	case "ping":
		if r.stale {
			return worker.Message{Type: "pong", ProtocolVersion: r.staleProtocol, Capabilities: append([]string(nil), r.staleCapabilities...)}, nil
		}
		return worker.Message{
			Type: "pong", ProtocolVersion: worker.ProtocolVersion,
			Capabilities: []string{worker.CapabilityExclusiveNativeOwner},
		}, nil
	case "status":
		return worker.Message{Type: "status", ActiveRequestID: r.active, Queued: r.queued, Status: "working", Permissions: append([]worker.PendingPermission(nil), r.permissions...)}, nil
	case "prompt":
		if r.owner != "" {
			return worker.Message{}, errors.New("Session S-native native harness is running; ACP prompt was not accepted. Exit or close that native provider launch, then retry")
		}
		return worker.Message{Type: "accepted", State: "ready", RequestID: command.RequestID}, nil
	case "permission_decision":
		return worker.Message{Type: "permission_decided"}, nil
	case "refresh":
		if r.refreshErr != nil {
			return worker.Message{}, r.refreshErr
		}
		state := r.refreshState
		if state == "" {
			state = "scheduled"
		}
		return worker.Message{Type: "refresh_scheduled", State: state}, nil
	case "native_acquire":
		startsProvider := r.owner == "" || (r.owner == command.OwnerToken && r.reserved && command.NativeOwnerRole == worker.NativeOwnerRoleProvider)
		if startsProvider && (r.active != "" || r.queued > 0) {
			return worker.Message{}, errors.New("accepted ACP work is pending; wait for the current ACP turn to settle, then open Native CLI again")
		}
		if r.owner == "" {
			if command.NativeOwnerRole == worker.NativeOwnerRoleProvider {
				return worker.Message{}, errors.New("native provider reservation is no longer active")
			}
			r.owner = command.OwnerToken
			r.generationID++
			r.generation = fmt.Sprintf("%032x", r.generationID)
			state := r.acquireState
			if state == "" {
				if command.NativeOwnerRole == worker.NativeOwnerRoleReservation {
					state = "reserved"
					r.reserved = true
				} else {
					state = "acquired"
				}
			}
			return worker.Message{Type: "native_acquired", State: state, NativeOwned: true, NativeGeneration: r.generation}, nil
		}
		if r.owner == command.OwnerToken {
			switch command.NativeOwnerRole {
			case worker.NativeOwnerRoleReservation:
				state := "already_acquired"
				if r.reserved {
					state = "already_reserved"
				}
				return worker.Message{Type: "native_acquired", State: state, NativeOwned: true, NativeGeneration: r.generation}, nil
			case worker.NativeOwnerRoleProvider:
				if command.NativeGeneration != r.generation {
					return worker.Message{}, errors.New("native reservation generation mismatch")
				}
				if !r.reserved {
					return worker.Message{Type: "native_acquired", State: "already_acquired", NativeOwned: true, NativeGeneration: r.generation}, nil
				}
				r.reserved = false
				return worker.Message{Type: "native_acquired", State: "activated", NativeOwned: true, NativeGeneration: r.generation}, nil
			default:
				if r.reserved {
					return worker.Message{}, errors.New("direct execution cannot activate tmux reservation")
				}
				return worker.Message{Type: "native_acquired", State: "already_acquired", NativeOwned: true, NativeGeneration: r.generation}, nil
			}
		}
		return worker.Message{}, errors.New("another native provider process owns this Session")
	case "native_reservation_status":
		if r.owner != "" && r.owner != command.OwnerToken {
			return worker.Message{}, errors.New("native owner token mismatch")
		}
		state := "released"
		if r.owner != "" {
			state = "active"
			if r.reserved {
				state = "reserved"
			}
		}
		return worker.Message{Type: "native_reservation_status", State: state, NativeOwned: r.owner != "", NativeGeneration: r.generation}, nil
	case "native_recover_reservation":
		if r.releaseReservationBeforeRecover && r.reserved {
			r.owner = ""
			r.reserved = false
			r.generation = ""
			r.releaseReservationBeforeRecover = false
		}
		if r.owner == "" {
			return worker.Message{Type: "native_reservation_recovered", State: "already_released"}, nil
		}
		if r.owner != command.OwnerToken {
			return worker.Message{}, errors.New("native owner token mismatch")
		}
		if !r.reserved {
			return worker.Message{}, errors.New("active provider-native ownership cannot be recovered")
		}
		if command.NativeGeneration != r.generation {
			return worker.Message{}, errors.New("native reservation generation mismatch")
		}
		r.owner = ""
		r.reserved = false
		r.generation = ""
		return worker.Message{Type: "native_reservation_recovered", State: "released"}, nil
	case "native_release":
		if r.owner != command.OwnerToken {
			return worker.Message{}, errors.New("native owner token mismatch")
		}
		if command.NativeGeneration != r.generation {
			return worker.Message{}, errors.New("native owner generation mismatch")
		}
		r.owner = ""
		r.reserved = false
		r.generation = ""
		return worker.Message{Type: "native_released", State: "released"}, nil
	default:
		return worker.Message{}, errors.New("unexpected worker command " + command.Type)
	}
}

func (r *nativeTestRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
}

func TestPromptNegotiatesCurrentWorkerBeforeSending(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
		accepted, err := service.Prompt(context.Background(), current.ID, "continue safely")
		if err != nil || accepted.Type != "accepted" || accepted.RequestID == "" {
			t.Fatalf("current prompt = %#v, %v", accepted, err)
		}
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		if !reflect.DeepEqual(runtime.calls, []string{"ping", "prompt"}) {
			t.Fatalf("current prompt calls = %v", runtime.calls)
		}
	})

	t.Run("stale", func(t *testing.T) {
		service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
		runtime.stale = true
		_, err := service.Prompt(context.Background(), current.ID, "must not reach provider")
		var upgrade *WorkerUpgradeRequiredError
		if !errors.Is(err, ErrWorkerUpgradeRequired) || !errors.As(err, &upgrade) || upgrade.Operation != "prompt not sent" {
			t.Fatalf("stale prompt error = %T %v", err, err)
		}
		for _, want := range []string{"prompt not sent", "agent-S-native", "acpctl session stop S-native", "acpctl session resume S-native"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("stale prompt error missing %q: %v", want, err)
			}
		}
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		if !reflect.DeepEqual(runtime.calls, []string{"ping"}) {
			t.Fatalf("stale prompt reached provider: %v", runtime.calls)
		}
	})
}

func TestPromptRejectsImmediatelyWhileNativeHarnessOwnsSession(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.owner = "native-owner"
	runtime.generation = "0123456789abcdef0123456789abcdef"

	_, err := service.Prompt(context.Background(), current.ID, "must not be queued")
	if err == nil || !strings.Contains(err.Error(), "native harness is running") || !strings.Contains(err.Error(), "prompt was not accepted") {
		t.Fatalf("native-open prompt error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "prompt"}) || runtime.owner != "native-owner" {
		t.Fatalf("native-open prompt calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestV2ExclusiveWriterWorkerRequiresUpgradeBeforeCrossSurfaceActions(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.stale = true
	runtime.staleProtocol = 2
	runtime.staleCapabilities = []string{"exclusive-native-owner-v1"}

	if _, err := service.Prompt(context.Background(), current.ID, "must not remain queued behind native"); !errors.Is(err, ErrWorkerUpgradeRequired) || !strings.Contains(err.Error(), "Worker protocol 2") || !strings.Contains(err.Error(), worker.CapabilityExclusiveNativeOwner) {
		t.Fatalf("v2 prompt upgrade error = %v", err)
	}
	runtime.mu.Lock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping"}) {
		runtime.mu.Unlock()
		t.Fatalf("v2 prompt reached worker: %v", runtime.calls)
	}
	runtime.calls = nil
	runtime.mu.Unlock()

	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: &acceptingNativeVerifier{}, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: io.Discard,
	})
	if !errors.Is(err, ErrWorkerUpgradeRequired) || !strings.Contains(err.Error(), "Worker protocol 2") {
		t.Fatalf("v2 native upgrade error = %v", err)
	}
	if runner.invocation.Command != "" {
		t.Fatalf("v2 worker launched native provider: %#v", runner.invocation)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping"}) || runtime.owner != "" {
		t.Fatalf("v2 native calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestStaleWorkerPermissionDecisionIsNotSent(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.stale = true
	err := service.DecidePermission(context.Background(), current.ID, "permission-1", "allow-once", false)
	if !errors.Is(err, ErrWorkerUpgradeRequired) || !strings.Contains(err.Error(), "permission not sent") {
		t.Fatalf("stale permission error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping"}) {
		t.Fatalf("stale permission decision reached provider: %v", runtime.calls)
	}
}

func TestPendingPermissionSnapshotDisablesStaleWorkerDecisions(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "claude", "native-exact")
	runtime.stale = true
	runtime.permissions = []worker.PendingPermission{{
		ID: "permission-stale", RequestID: "request-stale", Title: "Write scratch test", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-once", Name: "Allow once", Kind: "allow_once"}},
	}}
	snapshot, err := service.PendingPermissionSnapshot(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DecisionsReady || snapshot.BlockedCode != "worker_upgrade_required" || len(snapshot.Permissions) != 1 {
		t.Fatalf("permission snapshot = %#v", snapshot)
	}
	for _, want := range []string{"permission not sent", "agent-S-native", "acpctl session stop S-native", "acpctl session resume S-native"} {
		if !strings.Contains(snapshot.BlockedMessage, want) {
			t.Fatalf("blocked message missing %q: %s", want, snapshot.BlockedMessage)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"status", "ping"}) {
		t.Fatalf("permission snapshot calls = %v", runtime.calls)
	}
}

func TestResumeLiveWorkerNegotiatesCurrentOrRequiresUpgradeWithoutSpawn(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale bool
	}{
		{name: "current"},
		{name: "stale", stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
			runtime.stale = test.stale
			_, err := service.Resume(context.Background(), current.ID)
			if test.stale {
				if !errors.Is(err, ErrWorkerUpgradeRequired) || !strings.Contains(err.Error(), "resume not performed") {
					t.Fatalf("stale resume error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "worker is already running") {
				t.Fatalf("current resume error = %v", err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if !reflect.DeepEqual(runtime.calls, []string{"ping"}) || runtime.spawnCalls != 0 {
				t.Fatalf("%s resume calls=%v spawn=%d", test.name, runtime.calls, runtime.spawnCalls)
			}
		})
	}
}

func TestNativeAttachRejectsStaleWorkerBeforeAcquireOrProviderStart(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.stale = true
	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: &acceptingNativeVerifier{}, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err == nil {
		t.Fatal("stale worker unexpectedly opened provider-native attachment")
	}
	if !errors.Is(err, ErrWorkerUpgradeRequired) {
		t.Fatalf("stale native error does not expose upgrade sentinel: %v", err)
	}
	for _, want := range []string{
		"worker upgrade required", "Worker protocol 0", worker.CapabilityExclusiveNativeOwner,
		"native not started", "existing provider terminal", "tmux attach-session -t =agent-S-native",
		"acpctl session stop S-native", "acpctl session resume S-native",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("stale worker error missing %q: %v", want, err)
		}
	}
	if runner.invocation.Command != "" {
		t.Fatalf("stale worker launched native provider: %#v", runner.invocation)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping"}) || runtime.owner != "" {
		t.Fatalf("stale worker calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

type acceptingNativeVerifier struct {
	err   error
	calls []string
}

func (v *acceptingNativeVerifier) Verify(_ context.Context, _ agents.Profile, conversationID, workingDirectory string) (native.Verification, error) {
	v.calls = append(v.calls, conversationID+"@"+workingDirectory)
	if v.err != nil {
		return native.Verification{}, v.err
	}
	return native.Verification{ConversationID: conversationID, StorePath: "/provider/store"}, nil
}

type recordingNativeRunner struct {
	invocation native.Invocation
	err        error
}

func (r *recordingNativeRunner) Run(_ context.Context, invocation native.Invocation, _ io.Reader, _, _ io.Writer) error {
	r.invocation = invocation
	return r.err
}

func TestNativeAttachRejectsBeforeProviderLaunchWhileACPWorkIsAccepted(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.active = "req-active"
	runtime.queued = 2
	runner := &recordingNativeRunner{}
	verifier := &acceptingNativeVerifier{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: verifier, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("native attach while ACP work is accepted error = %v", err)
	}
	if runner.invocation.Command != "" {
		t.Fatalf("rejected native attachment launched provider: %#v", runner.invocation)
	}
	runtime.mu.Lock()
	calls, active, queued := append([]string(nil), runtime.calls...), runtime.active, runtime.queued
	runtime.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"ping", "native_acquire"}) || active != "req-active" || queued != 2 {
		t.Fatalf("native worker calls=%v active=%q queued=%d", calls, active, queued)
	}
	if len(verifier.calls) != 1 {
		t.Fatalf("verification calls = %v", verifier.calls)
	}
}

func TestRejectedHiddenNativeActivationReleasesOnlyProvenReservationAndCanRetry(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	ownerToken, err := native.OwnerToken(service.home.Root, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	runtime.owner = ownerToken
	runtime.reserved = true
	runtime.generation = generation
	runtime.active = "req-accepted-before-provider-start"
	blockedRunner := &recordingNativeRunner{}
	err = service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		OwnerToken: ownerToken, ReservationGeneration: generation,
		Runner: blockedRunner, Verifier: &acceptingNativeVerifier{}, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("hidden provider activation error = %v", err)
	}
	if blockedRunner.invocation.Command != "" {
		t.Fatalf("rejected hidden activation launched provider: %#v", blockedRunner.invocation)
	}
	runtime.mu.Lock()
	if runtime.owner != "" || runtime.reserved {
		runtime.mu.Unlock()
		t.Fatalf("proven rejected reservation remains: owner=%q reserved=%t", runtime.owner, runtime.reserved)
	}
	runtime.active = ""
	runtime.queued = 0
	runtime.mu.Unlock()

	retryRunner := &recordingNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: retryRunner, Verifier: &acceptingNativeVerifier{}, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: io.Discard,
	}); err != nil {
		t.Fatalf("native retry after ACP settled: %v", err)
	}
	if retryRunner.invocation.Command != "codex" {
		t.Fatalf("native retry did not launch exact provider resume: %#v", retryRunner.invocation)
	}
	runtime.mu.Lock()
	calls := append([]string(nil), runtime.calls...)
	runtime.mu.Unlock()
	want := []string{"ping", "native_acquire", "native_reservation_status", "native_release", "ping", "native_acquire", "native_release"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("hidden activation cleanup/retry calls = %v, want %v", calls, want)
	}
}

func TestRejectedHiddenNativeActivationNeverReleasesUncertainActiveOwner(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	ownerToken, err := native.OwnerToken(service.home.Root, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	runtime.owner = ownerToken
	runtime.reserved = false
	runtime.generation = generation
	err = service.releaseRejectedNativeActivationReservation(current, ownerToken, generation)
	if err == nil || !strings.Contains(err.Error(), "ownership was preserved") {
		t.Fatalf("uncertain active cleanup error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.owner != ownerToken || runtime.generation != generation || runtime.reserved || !reflect.DeepEqual(runtime.calls, []string{"native_reservation_status"}) {
		t.Fatalf("uncertain active ownership changed: owner=%q generation=%q reserved=%t calls=%v", runtime.owner, runtime.generation, runtime.reserved, runtime.calls)
	}
}

func TestNativeAttachRunsVerifiedCanonicalConversationAndReleasesHarness(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runner := &recordingNativeRunner{}
	verifier := &acceptingNativeVerifier{}
	var notices bytes.Buffer
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: verifier, Input: strings.NewReader(""), Output: io.Discard, ErrorOutput: &notices,
	}); err != nil {
		t.Fatal(err)
	}
	if runner.invocation.Command != "codex" || !reflect.DeepEqual(runner.invocation.Args, []string{"resume", "native-exact"}) || runner.invocation.WorkingDirectory != current.RepoPath {
		t.Fatalf("native invocation = %#v", runner.invocation)
	}
	runtime.mu.Lock()
	calls, owner := append([]string(nil), runtime.calls...), runtime.owner
	runtime.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"ping", "native_acquire", "native_release"}) || owner != "" {
		t.Fatalf("native worker calls=%v owner=%q", calls, owner)
	}
	if !strings.Contains(notices.String(), "provider transcript reload requested") || !strings.Contains(notices.String(), "ACP prompts remain available") {
		t.Fatalf("release notice = %q", notices.String())
	}
	verified, err := service.Get(context.Background(), current.ID)
	if err != nil || verified.ProviderConversationID != "native-exact" || verified.ConversationIDSource != session.ConversationSourceProviderStore || verified.ConversationIDVerifiedAt == nil {
		t.Fatalf("verified Session = %#v, %v", verified, err)
	}
	if len(verifier.calls) != 1 || !strings.HasPrefix(verifier.calls[0], "native-exact@") {
		t.Fatalf("verification calls = %v", verifier.calls)
	}
}

func TestNativeAttachRequiresCanonicalIDWithoutTouchingWorker(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "")
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: &recordingNativeRunner{}, Verifier: &acceptingNativeVerifier{}})
	if err == nil || !strings.Contains(err.Error(), "no canonical provider conversation ID") {
		t.Fatalf("missing canonical ID error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.calls) != 0 {
		t.Fatalf("missing ID touched worker: calls=%v", runtime.calls)
	}
}

func TestNativeAttachRejectsUnverifiedConversationBeforeRunner(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: &acceptingNativeVerifier{err: errors.New("exact transcript missing")},
	})
	if err == nil || !strings.Contains(err.Error(), "exact transcript missing") {
		t.Fatalf("verification error = %v", err)
	}
	runtime.mu.Lock()
	calls := append([]string(nil), runtime.calls...)
	runtime.mu.Unlock()
	if len(calls) != 0 || runner.invocation.Command != "" {
		t.Fatalf("unverified attach touched runtime or runner: calls=%v invocation=%#v", calls, runner.invocation)
	}
}

func TestNativeProcessFailureStillReleasesHarnessOwnership(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: &recordingNativeRunner{err: errors.New("exit 7")}, Verifier: &acceptingNativeVerifier{}})
	if err == nil || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("native process error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire", "native_release"}) || runtime.owner != "" {
		t.Fatalf("process failure refresh calls=%v", runtime.calls)
	}
}

type prelaunchNativeRunner struct{}

func (*prelaunchNativeRunner) Run(context.Context, native.Invocation, io.Reader, io.Writer, io.Writer) error {
	return errors.New("unowned tmux collision")
}

func (*prelaunchNativeRunner) ProcessMayContinue() bool     { return false }
func (*prelaunchNativeRunner) NativeActivityOccurred() bool { return false }

func TestNativePrelaunchFailureDoesNotRefreshACP(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: &prelaunchNativeRunner{}, Verifier: &acceptingNativeVerifier{}})
	if err == nil || !strings.Contains(err.Error(), "unowned tmux collision") {
		t.Fatalf("prelaunch error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire", "native_release"}) || runtime.owner != "" {
		t.Fatalf("prelaunch failure worker calls=%v", runtime.calls)
	}
}

type continuingNativeRunner struct{ recordingNativeRunner }

func (*continuingNativeRunner) ProcessMayContinue() bool { return true }

type reservationNativeRunner struct {
	recordingNativeRunner
	activator  func(context.Context) error
	generation string
	continuing bool
	activity   bool
}

func (r *reservationNativeRunner) SetPreexistingOwnership(bool) {}
func (r *reservationNativeRunner) SetReservationActivator(activate func(context.Context) error) {
	r.activator = activate
}
func (r *reservationNativeRunner) SetReservationGeneration(generation string) {
	r.generation = generation
}
func (r *reservationNativeRunner) ProcessMayContinue() bool     { return r.continuing }
func (r *reservationNativeRunner) NativeActivityOccurred() bool { return r.activity }
func (r *reservationNativeRunner) Run(ctx context.Context, invocation native.Invocation, _ io.Reader, _, _ io.Writer) error {
	r.invocation = invocation
	if r.err != nil {
		return r.err
	}
	if r.continuing {
		return nil
	}
	if r.activator == nil {
		return errors.New("reservation activator was not configured")
	}
	if err := r.activator(ctx); err != nil {
		return err
	}
	r.activity = true
	return nil
}

type reservationRecoveryVerifier struct {
	err   error
	calls []string
}

func (v *reservationRecoveryVerifier) VerifyReservationAbsent(_ context.Context, sessionID string) error {
	v.calls = append(v.calls, sessionID)
	return v.err
}

type existingCollisionNativeRunner struct{ recordingNativeRunner }

func (*existingCollisionNativeRunner) ProcessMayContinue() bool     { return true }
func (*existingCollisionNativeRunner) NativeActivityOccurred() bool { return false }

type preexistingOuterFailureRunner struct {
	recordingNativeRunner
	preexisting bool
}

func (r *preexistingOuterFailureRunner) SetPreexistingOwnership(value bool) {
	r.preexisting = value
}

func TestDetachedTmuxViewerLeavesRefreshToNativeProcessOwner(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runner := &continuingNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: runner, Verifier: &acceptingNativeVerifier{}}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire"}) || runtime.owner == "" {
		t.Fatalf("viewer detach scheduled premature refresh: %v", runtime.calls)
	}
}

func TestExistingTmuxCollisionNeverReleasesWorkerOwnership(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runner := &existingCollisionNativeRunner{recordingNativeRunner{err: errors.New("existing tmux native-handoff protocol collision")}}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: runner, Verifier: &acceptingNativeVerifier{}})
	if err == nil || !strings.Contains(err.Error(), "native-handoff protocol collision") {
		t.Fatalf("uncertain tmux error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire"}) || runtime.owner == "" {
		t.Fatalf("uncertain tmux handoff released ownership: calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestSecondOuterAttachFailureNeverReleasesLiveHiddenProviderOwnership(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	firstOuter := &continuingNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: firstOuter, Verifier: &acceptingNativeVerifier{},
	}); err != nil {
		t.Fatal(err)
	}
	secondOuter := &preexistingOuterFailureRunner{recordingNativeRunner: recordingNativeRunner{err: errors.New("second viewer prelaunch failed")}}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: secondOuter, Verifier: &acceptingNativeVerifier{},
	})
	if err == nil || !strings.Contains(err.Error(), "second viewer prelaunch failed") {
		t.Fatalf("second outer attach error = %v", err)
	}
	if !secondOuter.preexisting {
		t.Fatal("second outer runner was not constrained to attach-only behavior")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire", "ping", "native_acquire"}) || runtime.owner == "" {
		t.Fatalf("second outer attach released live provider ownership: calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestSecondOuterAttachRejectsRunnerWithoutAttachOnlyContract(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: &continuingNativeRunner{}, Verifier: &acceptingNativeVerifier{},
	}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: &acceptingNativeVerifier{},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot prove attach-only behavior") || !strings.Contains(err.Error(), "refusing to start a second provider process") {
		t.Fatalf("unsafe second outer runner error = %v", err)
	}
	if runner.invocation.Command != "" {
		t.Fatalf("unsafe second outer runner launched provider: %#v", runner.invocation)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire", "ping", "native_acquire"}) || runtime.owner == "" {
		t.Fatalf("unsafe second outer runner released ownership: calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestUnknownNativeAcquisitionStateFailsClosedWithoutProviderOrRelease(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.acquireState = "future_state"
	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: runner, Verifier: &acceptingNativeVerifier{},
	})
	if err == nil || !strings.Contains(err.Error(), `unsafe acquisition state "future_state"`) || !strings.Contains(err.Error(), "preserving ownership") {
		t.Fatalf("unknown acquisition state error = %v", err)
	}
	if runner.invocation.Command != "" {
		t.Fatalf("unknown acquisition state launched provider: %#v", runner.invocation)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire"}) || runtime.owner == "" {
		t.Fatalf("unknown acquisition state failed open: calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestNativeAttachDoesNotGateOnRefreshState(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.refreshing = true
	runner := &recordingNativeRunner{}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: runner, Verifier: &acceptingNativeVerifier{}})
	if err != nil || runner.invocation.Command != "codex" {
		t.Fatalf("native attach during refresh state error=%v invocation=%#v", err, runner.invocation)
	}
}

func TestNativeTmuxOuterAndHiddenOwnerAcquireIdempotently(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	outer := &reservationNativeRunner{continuing: true}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: outer, Verifier: &acceptingNativeVerifier{}}); err != nil {
		t.Fatal(err)
	}
	token, err := native.OwnerToken(service.home.Root, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	hidden := &recordingNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: hidden, Verifier: &acceptingNativeVerifier{}, OwnerToken: token, ReservationGeneration: outer.generation,
	}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"ping", "native_acquire", "ping", "native_acquire", "native_release"}) || runtime.owner != "" || runtime.reserved {
		t.Fatalf("outer/hidden ownership calls=%v owner=%q", runtime.calls, runtime.owner)
	}
}

func TestOrphanedTmuxReservationRecoveryAllowsFreshNativeLifecycleAndACPContinuation(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	collision := &reservationNativeRunner{
		recordingNativeRunner: recordingNativeRunner{err: errors.New("foreign exact tmux collision")},
		continuing:            true,
	}
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: collision, Verifier: &acceptingNativeVerifier{},
	})
	if err == nil || !strings.Contains(err.Error(), "foreign exact tmux collision") {
		t.Fatalf("collision attach error = %v", err)
	}
	runtime.mu.Lock()
	firstGeneration := runtime.generation
	reserved := runtime.reserved
	runtime.mu.Unlock()
	if !reserved || firstGeneration == "" || collision.activity {
		t.Fatalf("collision reservation = reserved:%t generation:%q activity:%t", reserved, firstGeneration, collision.activity)
	}

	stillExists := &reservationRecoveryVerifier{err: errors.New(`native reservation recovery refused while exact tmux Session "agent-S-native" still exists`)}
	if _, err := service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{Verifier: stillExists}); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("live collision recovery error = %v", err)
	}
	runtime.mu.Lock()
	if runtime.owner == "" || !runtime.reserved || runtime.generation != firstGeneration {
		runtime.mu.Unlock()
		t.Fatal("failed recovery did not preserve the exact reservation generation")
	}
	runtime.mu.Unlock()

	absent := &reservationRecoveryVerifier{}
	if recovered, err := service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{Verifier: absent}); err != nil {
		t.Fatal(err)
	} else if recovered.State != "released" {
		t.Fatalf("reservation recovery state = %q", recovered.State)
	}
	runtime.mu.Lock()
	if runtime.owner != "" || runtime.reserved || runtime.refreshing {
		runtime.mu.Unlock()
		t.Fatal("verified reservation recovery retained native ownership or scheduled a refresh gate")
	}
	runtime.mu.Unlock()
	if !reflect.DeepEqual(absent.calls, []string{current.ID}) {
		t.Fatalf("reservation verifier calls = %v", absent.calls)
	}

	fresh := &reservationNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: fresh, Verifier: &acceptingNativeVerifier{}}); err != nil {
		t.Fatal(err)
	}
	if !fresh.activity || fresh.generation == "" || fresh.generation == firstGeneration {
		t.Fatalf("fresh native lifecycle activity=%t generation=%q prior=%q", fresh.activity, fresh.generation, firstGeneration)
	}
	if _, err := service.Prompt(context.Background(), current.ID, "continue after recovered native exit"); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.owner != "" || runtime.reserved {
		t.Fatalf("fresh native exit left owner=%q reserved=%t", runtime.owner, runtime.reserved)
	}
	wantSuffix := []string{"ping", "native_acquire", "native_acquire", "native_release", "ping", "prompt"}
	if len(runtime.calls) < len(wantSuffix) || !reflect.DeepEqual(runtime.calls[len(runtime.calls)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("fresh attach/ACP continuation calls = %v", runtime.calls)
	}
}

func TestNativeReservationRecoveryNeverClearsActiveProviderOwnership(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: &continuingNativeRunner{}, Verifier: &acceptingNativeVerifier{},
	}); err != nil {
		t.Fatal(err)
	}
	verifier := &reservationRecoveryVerifier{}
	_, err := service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{Verifier: verifier})
	if err == nil || !strings.Contains(err.Error(), "active provider-native ownership") || !strings.Contains(err.Error(), "does not prove provider exit") || !strings.Contains(err.Error(), "second provider process") {
		t.Fatalf("active owner recovery error = %v", err)
	}
	if len(verifier.calls) != 0 {
		t.Fatalf("active ownership unnecessarily consulted tmux absence: %v", verifier.calls)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.owner == "" || runtime.reserved {
		t.Fatalf("active recovery changed owner=%q reserved=%t", runtime.owner, runtime.reserved)
	}
	if runtime.calls[len(runtime.calls)-1] != "native_reservation_status" {
		t.Fatalf("active recovery sent unsafe command: %v", runtime.calls)
	}
}

func TestV3ServiceRecoversOnlyAbsentLegacyV2Reservation(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	legacyOwner, err := native.LegacyExclusiveOwnerTokenV2(service.home.Root, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.owner = legacyOwner
	runtime.reserved = true
	runtime.generation = "0123456789abcdef0123456789abcdef"
	verifier := &reservationRecoveryVerifier{}
	recovered, err := service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{Verifier: verifier})
	if err != nil || recovered.State != "released" {
		t.Fatalf("legacy v2 reservation recovery = %#v, %v", recovered, err)
	}
	runtime.mu.Lock()
	calls, owner := append([]string(nil), runtime.calls...), runtime.owner
	runtime.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"ping", "native_reservation_status", "native_reservation_status", "native_recover_reservation"}) || owner != "" {
		t.Fatalf("legacy v2 recovery calls=%v owner=%q", calls, owner)
	}
	if !reflect.DeepEqual(verifier.calls, []string{current.ID}) {
		t.Fatalf("legacy reservation absence verification = %v", verifier.calls)
	}

	// An active legacy generation is discoverable but never cleared merely
	// because its tmux viewer is absent.
	runtime.mu.Lock()
	runtime.calls = nil
	runtime.owner = legacyOwner
	runtime.reserved = false
	runtime.generation = "abcdef0123456789abcdef0123456789"
	runtime.mu.Unlock()
	activeVerifier := &reservationRecoveryVerifier{}
	_, err = service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{Verifier: activeVerifier})
	if err == nil || !strings.Contains(err.Error(), "active provider-native ownership") {
		t.Fatalf("active legacy v2 recovery error = %v", err)
	}
	if len(activeVerifier.calls) != 0 {
		t.Fatalf("active legacy owner consulted tmux absence: %v", activeVerifier.calls)
	}
}

func TestNativeReservationRecoveryReportsConcurrentSafeReleaseWithoutPhantomRefresh(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	collision := &reservationNativeRunner{
		recordingNativeRunner: recordingNativeRunner{err: errors.New("pre-launch collision")},
		continuing:            true,
	}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{
		Runner: collision, Verifier: &acceptingNativeVerifier{},
	}); err == nil {
		t.Fatal("collision unexpectedly succeeded")
	}
	runtime.mu.Lock()
	runtime.releaseReservationBeforeRecover = true
	runtime.mu.Unlock()

	result, err := service.RecoverNativeReservation(context.Background(), current.ID, NativeReservationRecoveryOptions{
		Verifier: &reservationRecoveryVerifier{},
	})
	if err != nil || result.State != "already_released" {
		t.Fatalf("concurrent release recovery = %#v, %v", result, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.owner != "" || runtime.reserved || runtime.refreshing {
		t.Fatalf("concurrent safe release owner=%q reserved=%t refreshing=%t", runtime.owner, runtime.reserved, runtime.refreshing)
	}
}

func nativeServiceHarness(t *testing.T, agentName, nativeID string) (*SessionService, *nativeTestRuntime, session.Session) {
	t.Helper()
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
	t.Cleanup(func() { _ = repository.Close() })
	createdTask, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-native", Title: "Native test"})
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
	acpID := ""
	if nativeID != "" {
		acpID = "acp-exact"
	}
	current, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-native", TaskID: createdTask.ID, Agent: agentName, Provider: "openai", RepoPath: canonicalRepo,
		NativeSessionID: nativeID, ACPSessionID: acpID, WorkerSocket: filepath.Join(root, "worker.sock"), Status: session.StatusWaiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &nativeTestRuntime{}
	return NewSessionService(repository, registry, home, runtime), runtime, current
}
