package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
	"github.com/yty-build/acp-gateway/internal/config"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	info := buildinfo.Info{Version: "1.2.3", Commit: "abc", Date: "today"}
	code := Run(context.Background(), []string{"version"}, &stdout, &stderr, info)
	if code != ExitOK || stdout.String() != "agent 1.2.3 (commit abc, built today)\n" {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
}

func TestRunUnknownCommandHasStableError(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"wat"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if got, want := stderr.String(), "agent: unknown command \"wat\"; run 'agent help' for usage\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestServeUsesLoopbackAndStopsOnlyTheViewer(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newReadyWriter()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"serve", "--listen", "127.0.0.1:0"}, stdout, &stderr, buildinfo.Info{Version: "test"})
	}()
	select {
	case <-stdout.ready:
	case <-time.After(4 * time.Second):
		cancel()
		t.Fatal("agent serve did not start")
	}
	cancel()
	select {
	case code := <-done:
		if code != ExitOK || stderr.Len() != 0 {
			t.Fatalf("serve: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("agent serve did not stop after context cancellation")
	}
	for _, expected := range []string{"http://127.0.0.1:", "Session workers will continue"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Errorf("serve output %q does not contain %q", stdout.String(), expected)
		}
	}
}

func TestServeRejectsPublicOrImplicitBinding(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0"} {
		code, _, stderr := runForTest(t, "serve", "--listen", address)
		if code != ExitFailure || !strings.Contains(stderr, "refusing non-loopback listen address") {
			t.Errorf("serve %s: code=%d stderr=%q", address, code, stderr)
		}
	}
}

type readyWriter struct {
	mu    sync.Mutex
	text  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newReadyWriter() *readyWriter { return &readyWriter{ready: make(chan struct{})} }

func (w *readyWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written, err := w.text.Write(value)
	w.once.Do(func() { close(w.ready) })
	return written, err
}

func (w *readyWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.String()
}

func TestTaskCommandLifecycle(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_HOME", home)

	code, stdout, stderr := runForTest(t, "task", "new", "Research BTC", "--description", "Compare horizons", "--external-ref=PM-42", "--metadata-json", `{"market":"btc"}`)
	if code != ExitOK || stderr != "" {
		t.Fatalf("task new: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) != 2 || fields[0] != "Created" {
		t.Fatalf("task new output = %q", stdout)
	}
	taskID := fields[1]

	code, stdout, stderr = runForTest(t, "tasks")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, taskID) || !strings.Contains(stdout, "open") || !strings.Contains(stdout, "Research BTC") {
		t.Fatalf("tasks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runForTest(t, "task", taskID)
	for _, expected := range []string{"Research BTC", "Description: Compare horizons", "External ref: PM-42", "No Sessions."} {
		if !strings.Contains(stdout, expected) {
			t.Errorf("task show output %q does not contain %q", stdout, expected)
		}
	}
	if code != ExitOK || stderr != "" {
		t.Fatalf("task show: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr = runForTest(t, "task", "status", taskID, "review")
	if code != ExitOK || stderr != "" || stdout != "Updated "+taskID+" to review\n" {
		t.Fatalf("task status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, _, stderr = runForTest(t, "task", "status", taskID, "running")
	if code != ExitUsage || !strings.Contains(stderr, "invalid Task status") {
		t.Fatalf("invalid status: code=%d stderr=%q", code, stderr)
	}
}

func TestStartCreatesTaskAndSession(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeCLIRepository(t, repo)
	originalFactory := sessionRuntimeFactory
	sessionRuntimeFactory = func(config.Home, string) appservice.Runtime { return fakeSessionRuntime{} }
	t.Cleanup(func() { sessionRuntimeFactory = originalFactory })
	code, stdout, stderr := runForTest(t, "start", "Fix login", "--agent", "codex", "--repo", repo, "--role", "implementer", "--description", "Production regression")
	if code != ExitOK || stderr != "" {
		t.Fatalf("start: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) < 7 || fields[0] != "Created" || fields[2] != "Started" || fields[4] != "Initial" {
		t.Fatalf("start output = %q", stdout)
	}
	taskID, sessionID := fields[1], fields[3]
	code, stdout, stderr = runForTest(t, "task", taskID)
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, sessionID) || !strings.Contains(stdout, "implementer") || !strings.Contains(stdout, "codex") || !strings.Contains(stdout, "starting") {
		t.Fatalf("task show: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestStartWorktreeFlagsPersistIsolatedCheckout(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_HOME", home)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeCLIRepository(t, repo)
	originalFactory := sessionRuntimeFactory
	sessionRuntimeFactory = func(config.Home, string) appservice.Runtime { return fakeSessionRuntime{} }
	t.Cleanup(func() { sessionRuntimeFactory = originalFactory })

	code, stdout, stderr := runForTest(t, "start", "Isolated change", "--agent", "codex", "--repo", repo, "--worktree")
	if code != ExitOK || stderr != "" {
		t.Fatalf("start --worktree: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) < 4 {
		t.Fatalf("start output = %q", stdout)
	}
	taskID, sessionID := fields[1], fields[3]
	canonicalWorktrees, err := filepath.EvalSymlinks(filepath.Join(home, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(canonicalWorktrees, taskID+"-"+sessionID)
	wantBranch := "agent/" + taskID + "/" + sessionID
	code, stdout, stderr = runForTest(t, "session", sessionID)
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, "Worktree: "+wantPath) || !strings.Contains(stdout, "Branch: "+wantBranch) {
		t.Fatalf("Session context: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "README.md")); err != nil {
		t.Fatalf("isolated checkout missing: %v", err)
	}
}

type fakeSessionRuntime struct{}

func (fakeSessionRuntime) Spawn(context.Context, string) (worker.SpawnResult, error) {
	return worker.SpawnResult{PID: 123, Socket: "fake.sock"}, nil
}

func (fakeSessionRuntime) Send(_ context.Context, _ string, command worker.Command) (worker.Message, error) {
	return worker.Message{Type: "accepted", RequestID: command.RequestID, State: "ready"}, nil
}

func (fakeSessionRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
}

func TestStartValidationIsAtomic(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	code, _, stderr := runForTest(t, "start", "Invalid", "--agent", "", "--repo", ".")
	if code != ExitUsage || !strings.Contains(stderr, "usage: agent start") {
		t.Fatalf("start: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "tasks")
	if code != ExitOK || stdout != "No Tasks.\n" || stderr != "" {
		t.Fatalf("tasks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestAgentsListsDefaultsAndHidesEnvironmentValues(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_HOME", home)
	code, stdout, stderr := runForTest(t, "agents")
	if code != ExitOK || stderr != "" {
		t.Fatalf("agents: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, name := range []string{"claude", "codex", "gemini", "grok-build", "opencode"} {
		if !strings.Contains(stdout, name) {
			t.Errorf("agents output %q does not contain %q", stdout, name)
		}
	}
	profile := "name: private-agent\nprovider: custom\ncommand: custom-acp\nenvironment:\n  API_TOKEN: top-secret\n"
	if err := os.WriteFile(filepath.Join(home, "agents", "private-agent.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runForTest(t, "agents", "private-agent")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, "API_TOKEN (values hidden)") || strings.Contains(stdout, "top-secret") {
		t.Fatalf("agents private-agent: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestStartRejectsUnknownAgentProfileWithoutCreatingTask(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	code, _, stderr := runForTest(t, "start", "Unknown", "--agent", "missing", "--repo", ".")
	if code != ExitFailure || !strings.Contains(stderr, `agent profile "missing" was not found`) {
		t.Fatalf("start: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "tasks")
	if code != ExitOK || stdout != "No Tasks.\n" || stderr != "" {
		t.Fatalf("tasks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr, buildinfo.Info{Version: "test"})
	return code, stdout.String(), stderr.String()
}

func initializeCLIRepository(t *testing.T, path string) {
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
