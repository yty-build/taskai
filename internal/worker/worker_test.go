package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

type fakeACP struct {
	mu             sync.Mutex
	active         map[string]chan struct{}
	cancelled      map[string]bool
	started        chan string
	concurrent     int
	maxConcurrent  int
	resumeCount    int
	resumeErr      error
	resumeNativeID string
}

func newFakeACP() *fakeACP {
	return &fakeACP{active: make(map[string]chan struct{}), cancelled: make(map[string]bool), started: make(chan string, 20)}
}

func (f *fakeACP) Start(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	return acp.SessionInfo{ACPXRecordID: "acpx-start", ACPSessionID: "acp-start", NativeSessionID: "native-start"}, nil
}

func (f *fakeACP) Resume(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCount++
	if f.resumeErr != nil {
		return acp.SessionInfo{}, f.resumeErr
	}
	nativeID := f.resumeNativeID
	if nativeID == "" {
		nativeID = "native-start"
	}
	return acp.SessionInfo{ACPXRecordID: "acpx-resumed", ACPSessionID: "acp-resumed", NativeSessionID: nativeID}, nil
}

func (f *fakeACP) Prompt(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
	release := make(chan struct{})
	f.mu.Lock()
	f.active[spec.RequestID] = release
	f.concurrent++
	if f.concurrent > f.maxConcurrent {
		f.maxConcurrent = f.concurrent
	}
	f.mu.Unlock()
	f.started <- spec.RequestID
	if err := emit(acp.Event{EventVersion: 1, SessionID: "acp-start", RequestID: spec.RequestID, Seq: 1, Stream: "prompt", Type: "agent_message_chunk", Raw: json.RawMessage(`{"text":"working"}`)}); err != nil {
		return acp.PromptReceipt{}, err
	}
	select {
	case <-ctx.Done():
		f.finish(spec.RequestID)
		return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1}, ctx.Err()
	case <-release:
	}
	f.mu.Lock()
	cancelled := f.cancelled[spec.RequestID]
	f.mu.Unlock()
	f.finish(spec.RequestID)
	reason := "end_turn"
	if cancelled {
		reason = "cancelled"
	}
	return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: reason}, nil
}

func (f *fakeACP) finish(requestID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, requestID)
	f.concurrent--
}

func (f *fakeACP) Cancel(context.Context, acp.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for requestID, release := range f.active {
		if !f.cancelled[requestID] {
			f.cancelled[requestID] = true
			close(release)
		}
	}
	return nil
}

func (f *fakeACP) Inspect(context.Context, acp.Target) (acp.SessionInfo, error) {
	return acp.SessionInfo{ACPXRecordID: "acpx-inspected", ACPSessionID: "acp-inspected", NativeSessionID: "native-start"}, nil
}

func (f *fakeACP) Complete(requestID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if release, ok := f.active[requestID]; ok && !f.cancelled[requestID] {
		f.cancelled[requestID] = true
		close(release)
	}
}

func (f *fakeACP) maximumConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxConcurrent
}

type workerHarness struct {
	root       string
	repository *store.Store
	runtime    *Worker
	client     *SocketClient
	acp        *fakeACP
	cancel     context.CancelFunc
	done       chan error
}

func startWorkerHarness(t *testing.T) *workerHarness {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "atc-worker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(context.Background(), filepath.Join(root, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	createdTask, err := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-worker", Title: "Worker test"})
	if err != nil {
		t.Fatal(err)
	}
	createdSession, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-worker", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: repo,
		Status: session.StatusStarting, MetadataJSON: json.RawMessage(`{"permission":"read-approval"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeACP()
	runtime, err := New(Config{
		Session: createdSession, Profile: agents.Profile{Name: "codex", Provider: "openai", Command: "codex"},
		Repository: repository, ACP: fake, WorkersDirectory: filepath.Join(root, "workers"), LogsDirectory: filepath.Join(root, "logs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	client := NewSocketClient(runtime.SocketPath())
	waitFor(t, 3*time.Second, func() bool {
		pingContext, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer stop()
		return client.Ping(pingContext) == nil
	})
	harness := &workerHarness{root: root, repository: repository, runtime: runtime, client: client, acp: fake, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return harness
}

func TestWorkerSerializesPromptsBroadcastsAndReplays(t *testing.T) {
	harness := startWorkerHarness(t)

	subscriberContexts := make([]context.CancelFunc, 2)
	subscriberLines := make([]chan Message, 2)
	for index := range subscriberLines {
		ctx, cancel := context.WithCancel(context.Background())
		subscriberContexts[index] = cancel
		subscriberLines[index] = make(chan Message, 100)
		go func(index int) {
			_ = harness.client.Subscribe(ctx, 0, func(message Message) error {
				subscriberLines[index] <- message
				return nil
			})
		}(index)
	}
	defer subscriberContexts[0]()
	defer subscriberContexts[1]()
	for _, lines := range subscriberLines {
		waitFor(t, time.Second, func() bool { return len(lines) > 0 })
	}

	first, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-1", Message: "first"})
	if err != nil || first.Type != "accepted" || first.State != "ready" {
		t.Fatalf("first prompt = %#v, %v", first, err)
	}
	waitStarted(t, harness.acp.started, "req-1")
	second, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-2", Message: "second"})
	if err != nil || second.State != "queued" {
		t.Fatalf("second prompt = %#v, %v", second, err)
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || status.ActiveRequestID != "req-1" || status.Queued != 1 || status.Status != "working" {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire"}); err == nil {
		t.Fatal("native lease acquired during active turn")
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "cancel", RequestID: "req-1"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-2")
	// Detaching one viewer must not affect the active turn.
	subscriberContexts[0]()
	if err := harness.client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping after detach: %v", err)
	}
	harness.acp.Complete("req-2")
	waitFor(t, 2*time.Second, func() bool {
		current, err := harness.client.Send(context.Background(), Command{Type: "status"})
		return err == nil && current.ActiveRequestID == "" && current.Queued == 0 && current.Status == "waiting"
	})
	if got := harness.acp.maximumConcurrency(); got != 1 {
		t.Fatalf("maximum ACP concurrency = %d, want 1", got)
	}

	var replay []Message
	if err := harness.client.Tail(context.Background(), 0, func(message Message) error {
		replay = append(replay, message)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(replay) < 8 {
		t.Fatalf("replay has only %d lines", len(replay))
	}
	seenRequests := map[string]bool{}
	for index, line := range replay {
		if line.Type != "line" || (index > 0 && line.Seq <= replay[index-1].Seq) {
			t.Fatalf("non-monotonic replay at %d: %#v", index, line)
		}
		seenRequests[line.RequestID] = true
	}
	if !seenRequests["req-1"] || !seenRequests["req-2"] {
		t.Fatalf("correlated request IDs missing from replay: %v", seenRequests)
	}

	for _, lines := range subscriberLines {
		waitFor(t, 2*time.Second, func() bool {
			for _, count := range requestCounts(lines) {
				if count > 0 {
					return true
				}
			}
			return false
		})
	}
}

func TestNativeLeaseHoldsQueueUntilSuccessfulReconciliation(t *testing.T) {
	harness := startWorkerHarness(t)
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire"}); err != nil {
		t.Fatal(err)
	}
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-held", Message: "held"})
	if err != nil || accepted.State != "queued" {
		t.Fatalf("prompt = %#v, %v", accepted, err)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("queued prompt %s started during native lease", requestID)
	case <-time.After(150 * time.Millisecond):
	}
	harness.acp.mu.Lock()
	harness.acp.resumeErr = errors.New("cannot reconcile")
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_release"}); err == nil {
		t.Fatal("native release succeeded despite reconciliation failure")
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.NativeLease || status.Queued != 1 {
		t.Fatalf("status after failed reconcile = %#v, %v", status, err)
	}
	harness.acp.mu.Lock()
	harness.acp.resumeErr = nil
	harness.acp.resumeNativeID = "different-native-conversation"
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_release"}); err == nil || !strings.Contains(err.Error(), "could not prove conversation identity") {
		t.Fatalf("native identity mismatch error = %v", err)
	}
	status, err = harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.NativeLease || status.Queued != 1 {
		t.Fatalf("status after identity mismatch = %#v, %v", status, err)
	}
	harness.acp.mu.Lock()
	harness.acp.resumeNativeID = "native-start"
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_release"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-held")
	harness.acp.Complete("req-held")
}

func TestWorkerRejectsDuplicateAndUsesPrivateRuntimeFiles(t *testing.T) {
	harness := startWorkerHarness(t)
	duplicate, err := New(harness.runtime.config)
	if err != nil {
		t.Fatal(err)
	}
	err = duplicate.Run(context.Background())
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("duplicate Worker.Run() error = %v", err)
	}
	for _, path := range []string{harness.runtime.SocketPath(), harness.runtime.MetadataPath(), harness.runtime.LockPath(), harness.runtime.LogPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("mode for %s = %o, want 600", path, got)
		}
	}
}

func TestPermissionMetadataDefaultsReadAndRequiresExplicitApproveAll(t *testing.T) {
	for _, test := range []struct {
		metadata string
		want     acp.PermissionMode
		ok       bool
	}{
		{"", acp.PermissionReadApproval, true},
		{`{"permission":"read-approval"}`, acp.PermissionReadApproval, true},
		{`{"permission":"approve-all"}`, acp.PermissionApproveAll, true},
		{`{"permission":"write"}`, "", false},
	} {
		got, err := permissionFromMetadata(json.RawMessage(test.metadata))
		if (err == nil) != test.ok || got != test.want {
			t.Errorf("permissionFromMetadata(%q) = %q, %v", test.metadata, got, err)
		}
	}
}

func TestReconcileStaleWorkerMarksSessionStoppedAndRemovesRuntimeFiles(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "atc-stale-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs")}
	if err := os.MkdirAll(home.Workers, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createdTask, _ := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-stale", Title: "Stale"})
	createdSession, err := repository.CreateSession(context.Background(), session.CreateParams{ID: "S-stale", TaskID: createdTask.ID, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home.Workers, "S-stale.sock")
	metadata := filepath.Join(home.Workers, "S-stale.json")
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"pid":999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateSessionWorker(context.Background(), createdSession.ID, 999999, socket, filepath.Join(home.Logs, "S-stale.jsonl"), session.StatusWorking); err != nil {
		t.Fatal(err)
	}
	reconciled, err := ReconcileStale(context.Background(), home, repository)
	if err != nil || len(reconciled) != 1 || reconciled[0] != createdSession.ID {
		t.Fatalf("ReconcileStale() = %v, %v", reconciled, err)
	}
	stopped, err := repository.GetSession(context.Background(), createdSession.ID)
	if err != nil || stopped.Status != session.StatusStopped || stopped.WorkerPID != 0 || stopped.WorkerSocket != "" {
		t.Fatalf("stopped Session = %#v, %v", stopped, err)
	}
	for _, path := range []string{socket, metadata} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("stale runtime path %s still exists: %v", path, err)
		}
	}
}

func TestDetachedHiddenWorkerStartupLifecycleAndDiagnostics(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "atc-detached-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	for _, directory := range []string{home.Root, home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	createdTask, _ := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-detached", Title: "Detached"})
	_, err = repository.CreateSession(context.Background(), session.CreateParams{ID: "S-detached", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.CreateSession(context.Background(), session.CreateParams{ID: "S-detached-failure", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	executable := filepath.Join(root, "agent")
	build := exec.Command("go", "build", "-o", executable, "./cmd/agent")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, output)
	}
	fixture, err := filepath.Abs(filepath.Join("..", "acp", "testdata", "fake-acpx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACPX_BINARY", fixture)
	t.Setenv("FAKE_ACPX_SCENARIO", "success")
	result, err := Spawn(context.Background(), executable, home, "S-detached")
	if err != nil {
		t.Fatal(err)
	}
	if result.PID == os.Getpid() || !IsProcessAlive(result.PID) {
		t.Fatalf("detached PID = %d", result.PID)
	}
	client := NewSocketClient(result.Socket)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(context.Background(), Command{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return !IsProcessAlive(result.PID) })
	reopened, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stopped, err := reopened.GetSession(context.Background(), "S-detached")
	if err != nil || stopped.Status != session.StatusStopped || stopped.WorkerPID != 0 || stopped.WorkerSocket != "" {
		t.Fatalf("stopped Session = %#v, %v", stopped, err)
	}

	t.Setenv("FAKE_ACPX_SCENARIO", "nonzero")
	_, err = Spawn(context.Background(), executable, home, "S-detached-failure")
	if err == nil || !strings.Contains(err.Error(), "fixture ensure failed") {
		t.Fatalf("Spawn() startup failure = %v, want underlying acpx detail", err)
	}
	failed, getErr := reopened.GetSession(context.Background(), "S-detached-failure")
	if getErr != nil || failed.Status != session.StatusFailed {
		t.Fatalf("failed Session = %#v, %v", failed, getErr)
	}
}

func waitStarted(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("started request = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("request %s did not start", want)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func requestCounts(lines chan Message) map[string]int {
	counts := make(map[string]int)
	for {
		select {
		case line := <-lines:
			counts[line.RequestID]++
		default:
			return counts
		}
	}
}
