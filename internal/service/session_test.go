package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	mu           sync.Mutex
	active       string
	queued       int
	calls        []string
	refreshState string
	refreshErr   error
}

func (r *nativeTestRuntime) Spawn(context.Context, string) (worker.SpawnResult, error) {
	return worker.SpawnResult{}, nil
}

func (r *nativeTestRuntime) Send(_ context.Context, _ string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, command.Type)
	switch command.Type {
	case "status":
		return worker.Message{Type: "status", ActiveRequestID: r.active, Queued: r.queued, Status: "working"}, nil
	case "refresh":
		if r.refreshErr != nil {
			return worker.Message{}, r.refreshErr
		}
		state := r.refreshState
		if state == "" {
			state = "scheduled"
		}
		return worker.Message{Type: "refresh_scheduled", State: state}, nil
	default:
		return worker.Message{}, errors.New("unexpected worker command " + command.Type)
	}
}

func (r *nativeTestRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
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

func TestNativeAttachRunsVerifiedCanonicalConversationWithoutPausingACP(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runtime.active = "req-active"
	runtime.queued = 2
	runtime.refreshState = "deferred"
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
	calls, active, queued := append([]string(nil), runtime.calls...), runtime.active, runtime.queued
	runtime.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"status", "refresh"}) || active != "req-active" || queued != 2 {
		t.Fatalf("native worker calls=%v active=%q queued=%d", calls, active, queued)
	}
	for _, expected := range []string{"ACP activity is also live", "refresh is deferred"} {
		if !strings.Contains(notices.String(), expected) {
			t.Errorf("notices missing %q: %s", expected, notices.String())
		}
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

func TestNativeProcessFailureStillSchedulesNonBlockingRefresh(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: &recordingNativeRunner{err: errors.New("exit 7")}, Verifier: &acceptingNativeVerifier{}})
	if err == nil || !strings.Contains(err.Error(), "exit 7") {
		t.Fatalf("native process error = %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"status", "refresh"}) {
		t.Fatalf("process failure refresh calls=%v", runtime.calls)
	}
}

type continuingNativeRunner struct{ recordingNativeRunner }

func (*continuingNativeRunner) ProcessMayContinue() bool { return true }

func TestDetachedTmuxViewerLeavesRefreshToNativeProcessOwner(t *testing.T) {
	service, runtime, current := nativeServiceHarness(t, "codex", "native-exact")
	runner := &continuingNativeRunner{}
	if err := service.AttachNative(context.Background(), current.ID, NativeAttachOptions{Runner: runner, Verifier: &acceptingNativeVerifier{}}); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.calls, []string{"status"}) {
		t.Fatalf("viewer detach scheduled premature refresh: %v", runtime.calls)
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
