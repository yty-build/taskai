package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/yty-build/acp-gateway/internal/sessionlog"
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
	startCount     int
	resumeCount    int
	resumeErr      error
	resumeNativeID string
	inspectACP     string
	inspectNative  string
	promptErr      error
	refreshCount   int
	refreshWait    bool
}

func newFakeACP() *fakeACP {
	return &fakeACP{active: make(map[string]chan struct{}), cancelled: make(map[string]bool), started: make(chan string, 20)}
}

func (f *fakeACP) Start(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.startCount++
	f.mu.Unlock()
	return acp.SessionInfo{ACPXRecordID: "acpx-start", ACPSessionID: "acp-start", NativeSessionID: "native-start"}, nil
}

func (f *fakeACP) Resume(_ context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
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
	return acp.SessionInfo{ACPXRecordID: "acpx-resumed", ACPSessionID: spec.ResumeACPSessionID, NativeSessionID: nativeID}, nil
}

func (f *fakeACP) Refresh(ctx context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.refreshCount++
	refreshWait := f.refreshWait
	refreshErr := f.resumeErr
	f.mu.Unlock()
	if refreshWait {
		<-ctx.Done()
		return acp.SessionInfo{}, ctx.Err()
	}
	if refreshErr != nil {
		return acp.SessionInfo{}, refreshErr
	}
	return acp.SessionInfo{ACPXRecordID: "acpx-refreshed", ACPSessionID: spec.ResumeACPSessionID, NativeSessionID: "native-start"}, nil
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
	promptErr := f.promptErr
	f.mu.Unlock()
	f.finish(spec.RequestID)
	if promptErr != nil {
		return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1}, promptErr
	}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	acpID := f.inspectACP
	if acpID == "" {
		acpID = "acp-start"
	}
	nativeID := f.inspectNative
	if nativeID == "" {
		nativeID = "native-start"
	}
	return acp.SessionInfo{ACPXRecordID: "acpx-inspected", ACPSessionID: acpID, NativeSessionID: nativeID}, nil
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
	reconcile  *fakeTranscriptReconciler
	cancel     context.CancelFunc
	done       chan error
}

type fakeTranscriptReconciler struct {
	mu             sync.Mutex
	calls          int
	conversationID string
	workingDir     string
	wait           <-chan struct{}
	err            error
	started        chan struct{}
}

func newFakeTranscriptReconciler() *fakeTranscriptReconciler {
	return &fakeTranscriptReconciler{started: make(chan struct{}, 8)}
}

func (r *fakeTranscriptReconciler) Reconcile(ctx context.Context, _ agents.Profile, conversationID, workingDirectory string) error {
	r.mu.Lock()
	r.calls++
	r.conversationID = conversationID
	r.workingDir = workingDirectory
	wait, err := r.wait, r.err
	r.mu.Unlock()
	select {
	case r.started <- struct{}{}:
	default:
	}
	if wait != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
	return err
}

type permissionDecision struct {
	permissionID string
	optionID     string
	cancelled    bool
}

type permissionACP struct {
	mu         sync.Mutex
	emitted    chan struct{}
	decisions  chan permissionDecision
	recorded   []permissionDecision
	cancelled  chan struct{}
	promptDone chan struct{}
	cancelOnce sync.Once
}

func newPermissionACP() *permissionACP {
	return &permissionACP{
		emitted: make(chan struct{}), decisions: make(chan permissionDecision, 2),
		cancelled: make(chan struct{}), promptDone: make(chan struct{}),
	}
}

func (f *permissionACP) Start(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	return acp.SessionInfo{ACPXRecordID: "acpx-permission", ACPSessionID: "acp-permission", NativeSessionID: "native-permission"}, nil
}

func (f *permissionACP) Resume(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	return f.Start(context.Background(), acp.StartSpec{})
}

func (f *permissionACP) Prompt(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
	defer close(f.promptDone)
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":"bridge-permission","method":"session/request_permission","params":{"sessionId":"acp-permission","toolCall":{"toolCallId":"tool-write","title":"Write marker","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"allow-always","name":"Always allow","kind":"allow_always"},{"optionId":"reject-once","name":"Reject once","kind":"reject_once"}]}}`)
	if err := emit(acp.Event{
		EventVersion: 1, SessionID: spec.ConsoleSessionID, RequestID: spec.RequestID,
		Seq: 1, Stream: "prompt", Type: "session_request_permission", Raw: raw,
	}); err != nil {
		return acp.PromptReceipt{}, err
	}
	close(f.emitted)
	select {
	case <-ctx.Done():
		return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1}, ctx.Err()
	case <-f.cancelled:
		return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "cancelled"}, nil
	case decision := <-f.decisions:
		if decision.cancelled {
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "cancelled"}, nil
		}
		return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "end_turn"}, nil
	}
}

func (f *permissionACP) DecidePermission(ctx context.Context, permissionID, optionID string, cancelled bool) error {
	decision := permissionDecision{permissionID: permissionID, optionID: optionID, cancelled: cancelled}
	f.mu.Lock()
	f.recorded = append(f.recorded, decision)
	f.mu.Unlock()
	select {
	case f.decisions <- decision:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *permissionACP) recordedDecisions() []permissionDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]permissionDecision(nil), f.recorded...)
}

func (f *permissionACP) Cancel(context.Context, acp.Target) error {
	f.cancelOnce.Do(func() { close(f.cancelled) })
	return nil
}

func (f *permissionACP) Inspect(context.Context, acp.Target) (acp.SessionInfo, error) {
	return acp.SessionInfo{ACPXRecordID: "acpx-permission", ACPSessionID: "acp-permission", NativeSessionID: "native-permission"}, nil
}

type permissionWorkerHarness struct {
	runtime *Worker
	client  *SocketClient
	acp     *permissionACP
	cancel  context.CancelFunc
	done    chan error
}

func startPermissionWorkerHarness(t *testing.T) *permissionWorkerHarness {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "atc-permission-worker-")
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
	t.Cleanup(func() { _ = repository.Close() })
	createdTask, err := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-permission", Title: "Permission test"})
	if err != nil {
		t.Fatal(err)
	}
	createdSession, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-permission", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: repo,
		Status: session.StatusStarting, MetadataJSON: json.RawMessage(`{"permission":"read-approval"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := newPermissionACP()
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
		probe, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer stop()
		return client.Ping(probe) == nil
	})
	harness := &permissionWorkerHarness{runtime: runtime, client: client, acp: fake, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("permission worker did not stop")
		}
	})
	return harness
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
	reconciler := newFakeTranscriptReconciler()
	runtime, err := New(Config{
		Session: createdSession, Profile: agents.Profile{Name: "codex", Provider: "openai", Command: "codex"},
		Repository: repository, ACP: fake, WorkersDirectory: filepath.Join(root, "workers"), LogsDirectory: filepath.Join(root, "logs"),
		RefreshTimeout: 250 * time.Millisecond, ReconcileProviderTranscript: reconciler.Reconcile,
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
	harness := &workerHarness{root: root, repository: repository, runtime: runtime, client: client, acp: fake, reconcile: reconciler, cancel: cancel, done: done}
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
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "owner-active"}); err == nil || !strings.Contains(err.Error(), "ACP request req-1 is active") {
		t.Fatalf("native acquire during ACP turn error = %v", err)
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

func TestPermissionSurvivesDetachAndConcurrentViewerDecisionIsFirstWins(t *testing.T) {
	harness := startPermissionWorkerHarness(t)
	viewerContext, detachViewer := context.WithCancel(context.Background())
	viewerLines := make(chan Message, 32)
	go func() {
		_ = harness.client.Subscribe(viewerContext, 0, func(message Message) error {
			viewerLines <- message
			return nil
		})
	}()
	waitFor(t, time.Second, func() bool { return len(viewerLines) > 0 })

	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "REQ-permission", Message: "write marker"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt = %#v, %v", accepted, err)
	}
	select {
	case <-harness.acp.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("permission was not emitted")
	}
	waitFor(t, time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && len(status.Permissions) == 1
	})
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || len(status.Permissions) != 1 {
		t.Fatalf("pending status = %#v, %v", status, err)
	}
	pending := status.Permissions[0]
	if pending.ID != PermissionConsoleID("REQ-permission", "bridge-permission") || strings.Contains(pending.ID, "tool-write") || pending.RequestID != "REQ-permission" || len(pending.Options) != 3 {
		t.Fatalf("pending permission = %#v", pending)
	}

	// A disposable viewer disconnect is not a turn or permission lifecycle
	// action. A later viewer can still inspect and resolve the same request.
	detachViewer()
	time.Sleep(50 * time.Millisecond)
	status, err = harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || status.ActiveRequestID != "REQ-permission" || len(status.Permissions) != 1 {
		t.Fatalf("status after detach = %#v, %v", status, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "permission_decision", PermissionID: pending.ID, OptionID: "not-offered",
	}); err == nil || !strings.Contains(err.Error(), "was not offered") {
		t.Fatalf("invalid option error = %v", err)
	}
	status, err = harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || len(status.Permissions) != 1 {
		t.Fatalf("invalid option consumed pending request: %#v, %v", status, err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, optionID := range []string{"allow-once", "reject-once"} {
		optionID := optionID
		go func() {
			<-start
			_, decisionErr := harness.client.Send(context.Background(), Command{
				Type: "permission_decision", PermissionID: pending.ID, OptionID: optionID,
			})
			results <- decisionErr
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		decisionErr := <-results
		if decisionErr == nil {
			successes++
		} else if strings.Contains(decisionErr.Error(), "no longer pending") {
			conflicts++
		} else {
			t.Fatalf("unexpected viewer race result: %v", decisionErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("viewer race = %d success, %d conflict", successes, conflicts)
	}
	select {
	case <-harness.acp.promptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not continue after permission decision")
	}
	decisions := harness.acp.recordedDecisions()
	if len(decisions) != 1 || decisions[0].permissionID != "bridge-permission" || (decisions[0].optionID != "allow-once" && decisions[0].optionID != "reject-once") {
		t.Fatalf("provider decisions = %#v", decisions)
	}
	waitFor(t, time.Second, func() bool {
		current, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && current.ActiveRequestID == "" && len(current.Permissions) == 0
	})

	var types []string
	if err := harness.client.Tail(context.Background(), 0, func(message Message) error {
		types = append(types, message.LineType)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"permission_requested", "acp", "permission_resolved", "turn_completed"} {
		if !containsString(types, required) {
			t.Errorf("durable replay is missing %s: %v", required, types)
		}
	}
}

func TestCancelAndStopAbandonPendingPermission(t *testing.T) {
	t.Run("cancel turn", func(t *testing.T) {
		harness := startPermissionWorkerHarness(t)
		if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "REQ-cancel-permission", Message: "write"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-harness.acp.emitted:
		case <-time.After(2 * time.Second):
			t.Fatal("permission was not emitted")
		}
		if _, err := harness.client.Send(context.Background(), Command{Type: "cancel", RequestID: "REQ-cancel-permission"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-harness.acp.promptDone:
		case <-time.After(2 * time.Second):
			t.Fatal("cancel did not release pending prompt")
		}
		waitFor(t, time.Second, func() bool {
			status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
			return statusErr == nil && len(status.Permissions) == 0
		})
		var abandoned bool
		if err := harness.client.Tail(context.Background(), 0, func(message Message) error {
			abandoned = abandoned || message.LineType == "permission_abandoned"
			return nil
		}); err != nil || !abandoned {
			t.Fatalf("permission abandonment was not durable: %v", err)
		}
	})

	t.Run("stop worker", func(t *testing.T) {
		harness := startPermissionWorkerHarness(t)
		if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "REQ-stop-permission", Message: "write"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-harness.acp.emitted:
		case <-time.After(2 * time.Second):
			t.Fatal("permission was not emitted")
		}
		if _, err := harness.client.Send(context.Background(), Command{Type: "stop"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-harness.acp.promptDone:
		case <-time.After(2 * time.Second):
			t.Fatal("stop did not release pending prompt")
		}
		waitFor(t, 2*time.Second, func() bool {
			_, statErr := os.Stat(harness.runtime.SocketPath())
			return os.IsNotExist(statErr)
		})
		lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
		if err != nil {
			t.Fatal(err)
		}
		var abandoned bool
		for _, line := range lines {
			abandoned = abandoned || line.Type == "permission_abandoned"
		}
		if !abandoned {
			t.Fatalf("stop history lacks permission_abandoned: %#v", lines)
		}
	})
}

func TestPendingPermissionLimitFailsClosed(t *testing.T) {
	runtime := &Worker{
		active:      &queuedPrompt{RequestID: "REQ-permission-cap"},
		permissions: make(map[string]*pendingPermission),
	}
	for index := 0; index < maxPendingPermissions; index++ {
		pending := &pendingPermission{PendingPermission: PendingPermission{
			ID: fmt.Sprintf("permission-%03d", index), RequestID: "REQ-permission-cap",
		}}
		if err := runtime.registerPermission(pending); err != nil {
			t.Fatalf("register permission %d: %v", index, err)
		}
	}
	overflow := &pendingPermission{PendingPermission: PendingPermission{
		ID: "permission-overflow", RequestID: "REQ-permission-cap",
	}}
	if err := runtime.registerPermission(overflow); !errors.Is(err, errPendingPermissionCap) {
		t.Fatalf("overflow error = %v", err)
	}
	if len(runtime.permissions) != maxPendingPermissions || runtime.permissions[overflow.ID] != nil {
		t.Fatalf("permission cap did not fail closed: count=%d overflow=%#v", len(runtime.permissions), runtime.permissions[overflow.ID])
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestNativeRefreshDefersUntilActiveAndQueuedPromptsFinish(t *testing.T) {
	harness := startWorkerHarness(t)
	first, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-active", Message: "active"})
	if err != nil || first.State != "ready" {
		t.Fatalf("active prompt = %#v, %v", first, err)
	}
	waitStarted(t, harness.acp.started, "req-active")
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-queued", Message: "queued"})
	if err != nil || accepted.State != "queued" {
		t.Fatalf("prompt = %#v, %v", accepted, err)
	}
	refresh, err := harness.client.Send(context.Background(), Command{Type: "refresh"})
	if err != nil || refresh.Type != "refresh_scheduled" || refresh.State != "deferred" {
		t.Fatalf("refresh = %#v, %v", refresh, err)
	}
	harness.acp.mu.Lock()
	if harness.acp.refreshCount != 0 {
		harness.acp.mu.Unlock()
		t.Fatal("refresh ran while an ACP turn was active")
	}
	harness.acp.mu.Unlock()
	harness.acp.Complete("req-active")
	waitStarted(t, harness.acp.started, "req-queued")
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.RefreshPending || status.ActiveRequestID != "req-queued" {
		t.Fatalf("status while refresh deferred = %#v, %v", status, err)
	}
	harness.acp.Complete("req-queued")
	waitFor(t, 2*time.Second, func() bool {
		harness.acp.mu.Lock()
		defer harness.acp.mu.Unlock()
		return harness.acp.refreshCount == 1
	})
	waitFor(t, 2*time.Second, func() bool {
		latest, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && !latest.RefreshPending && !latest.Refreshing && latest.Queued == 0 && latest.ActiveRequestID == ""
	})
}

func TestNativeOwnershipQueuesACPThroughExactRefreshBarrier(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-native-barrier"
	reconcileRelease := make(chan struct{})
	harness.reconcile.mu.Lock()
	harness.reconcile.wait = reconcileRelease
	harness.reconcile.mu.Unlock()
	acquired, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken})
	if err != nil || acquired.Type != "native_acquired" || acquired.State != "acquired" {
		t.Fatalf("native acquire = %#v, %v", acquired, err)
	}
	if duplicate, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken}); err != nil || duplicate.State != "already_acquired" {
		t.Fatalf("idempotent native acquire = %#v, %v", duplicate, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "another-owner"}); err == nil || !strings.Contains(err.Error(), "another native provider process") {
		t.Fatalf("competing native acquire error = %v", err)
	}
	marker, err := os.Stat(harness.runtime.NativeOwnerPath())
	if err != nil || marker.Mode().Perm() != 0o600 {
		t.Fatalf("native ownership marker = %#v, %v", marker, err)
	}
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-native", Message: "continue"})
	if err != nil || accepted.State != "queued" {
		t.Fatalf("prompt while native owns = %#v, %v", accepted, err)
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.NativeOwned || status.ProviderWriter != "native" || status.Queued != 1 || status.ActiveRequestID != "" {
		t.Fatalf("native-owned status = %#v, %v", status, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "stop"}); err == nil || !strings.Contains(err.Error(), "exit the native provider") {
		t.Fatalf("stop while native owns error = %v", err)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("ACP prompt %s started while native owned the provider", requestID)
	case <-time.After(100 * time.Millisecond):
	}
	harness.acp.mu.Lock()
	harness.acp.refreshWait = true
	harness.acp.mu.Unlock()
	released, err := harness.client.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeActivity: true})
	if err != nil || released.State != "refresh_scheduled" {
		t.Fatalf("native release = %#v, %v", released, err)
	}
	waitFor(t, time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && !status.NativeOwned && status.Refreshing && status.ProviderWriter == "refresh"
	})
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); err != nil {
		t.Fatalf("refresh barrier marker disappeared before exact refresh completed: %v", err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken}); err == nil || !strings.Contains(err.Error(), "identity refresh is active") {
		t.Fatalf("native acquire during refresh error = %v", err)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("ACP prompt %s beat the exact identity refresh", requestID)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-harness.reconcile.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider transcript reconciliation did not start after exact ACP recovery")
	}
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); err != nil {
		t.Fatalf("refresh barrier marker disappeared during provider transcript reconciliation: %v", err)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("ACP prompt %s beat provider transcript reconciliation", requestID)
	case <-time.After(100 * time.Millisecond):
	}
	close(reconcileRelease)
	waitStarted(t, harness.acp.started, "req-after-native")
	harness.acp.mu.Lock()
	refreshCount, resumeCount := harness.acp.refreshCount, harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if refreshCount != 1 || resumeCount != 1 {
		t.Fatalf("exact refresh/recovery count = %d/%d, want 1/1", refreshCount, resumeCount)
	}
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native refresh barrier marker remains after exact transcript reconciliation: %v", err)
	}
	harness.reconcile.mu.Lock()
	if harness.reconcile.calls != 1 || harness.reconcile.conversationID != "native-start" || harness.reconcile.workingDir == "" {
		t.Fatalf("provider transcript reconcile = calls %d, conversation %q, directory %q", harness.reconcile.calls, harness.reconcile.conversationID, harness.reconcile.workingDir)
	}
	harness.reconcile.mu.Unlock()
	harness.acp.Complete("req-after-native")
}

func TestProviderTranscriptReconcileFailureKeepsDurableBarrierRetryable(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-reconcile-retry"
	harness.reconcile.mu.Lock()
	harness.reconcile.err = errors.New("provider transcript is temporarily unreadable")
	harness.reconcile.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken}); err != nil {
		t.Fatal(err)
	}
	if queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-blocked-by-reconcile", Message: "must wait"}); err != nil || queued.State != "queued" {
		t.Fatalf("queued prompt = %#v, %v", queued, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeActivity: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		current, err := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
		return err == nil && current.WorkerPID == 0
	})
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("ACP prompt %s dispatched after provider transcript reconciliation failed", requestID)
	case <-time.After(150 * time.Millisecond):
	}
	marker, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil || !strings.Contains(string(marker), `"state": "refresh_pending"`) {
		t.Fatalf("durable refresh barrier after reconcile failure = %q, %v", marker, err)
	}

	current, err := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	retryReconciler := newFakeTranscriptReconciler()
	retryConfig := harness.runtime.config
	retryConfig.Session = current
	retryConfig.ReconcileProviderTranscript = retryReconciler.Reconcile
	restarted, err := New(retryConfig)
	if err != nil {
		t.Fatal(err)
	}
	restartContext, stopRestart := context.WithCancel(context.Background())
	restartDone := make(chan error, 1)
	go func() { restartDone <- restarted.Run(restartContext) }()
	t.Cleanup(func() {
		stopRestart()
		select {
		case err := <-restartDone:
			if err != nil {
				t.Errorf("retry Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("retry worker did not stop")
		}
	})
	restartedClient := NewSocketClient(restarted.SocketPath())
	waitFor(t, 3*time.Second, func() bool {
		probe, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return restartedClient.Ping(probe) == nil
	})
	waitFor(t, 3*time.Second, func() bool {
		_, markerErr := os.Stat(restarted.NativeOwnerPath())
		status, statusErr := restartedClient.Send(context.Background(), Command{Type: "status"})
		return os.IsNotExist(markerErr) && statusErr == nil && !status.RefreshPending && !status.Refreshing
	})
	retryReconciler.mu.Lock()
	if retryReconciler.calls != 1 {
		t.Fatalf("provider transcript reconcile retries = %d, want 1", retryReconciler.calls)
	}
	retryReconciler.mu.Unlock()
	if accepted, err := restartedClient.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-reconcile-retry", Message: "continue"}); err != nil || accepted.State != "ready" {
		t.Fatalf("prompt after retry = %#v, %v", accepted, err)
	}
	waitStarted(t, harness.acp.started, "req-after-reconcile-retry")
	harness.acp.Complete("req-after-reconcile-retry")
}

func TestNativeOwnershipSurvivesWorkerRestartUntilOwnerRelease(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-survives-restart"
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken}); err != nil {
		t.Fatal(err)
	}
	harness.cancel()
	waitFor(t, 3*time.Second, func() bool {
		current, err := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
		return err == nil && current.WorkerPID == 0 && current.WorkerSocket == ""
	})
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); err != nil {
		t.Fatalf("native owner marker did not survive worker shutdown: %v", err)
	}

	current, err := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{
		Session: current, Profile: harness.runtime.config.Profile,
		Repository: harness.repository, ACP: harness.acp,
		WorkersDirectory:            harness.runtime.config.WorkersDirectory,
		LogsDirectory:               harness.runtime.config.LogsDirectory,
		RefreshTimeout:              250 * time.Millisecond,
		ReconcileProviderTranscript: harness.reconcile.Reconcile,
	})
	if err != nil {
		t.Fatal(err)
	}
	restartContext, stopRestart := context.WithCancel(context.Background())
	restartDone := make(chan error, 1)
	go func() { restartDone <- restarted.Run(restartContext) }()
	t.Cleanup(func() {
		stopRestart()
		select {
		case err := <-restartDone:
			if err != nil {
				t.Errorf("restarted Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("restarted worker did not stop")
		}
	})
	restartedClient := NewSocketClient(restarted.SocketPath())
	waitFor(t, 3*time.Second, func() bool {
		pingContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return restartedClient.Ping(pingContext) == nil
	})
	status, err := restartedClient.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.NativeOwned || status.ProviderWriter != "native" {
		t.Fatalf("status after worker restart = %#v, %v", status, err)
	}
	harness.acp.mu.Lock()
	resumeCountWhileNative := harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if resumeCountWhileNative != 0 {
		t.Fatalf("worker restart resumed ACP %d time(s) while native still owned the provider", resumeCountWhileNative)
	}
	accepted, err := restartedClient.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-worker-restart", Message: "continue"})
	if err != nil || accepted.State != "queued" {
		t.Fatalf("restart prompt = %#v, %v", accepted, err)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("ACP prompt %s started after worker restart while native still owned", requestID)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := restartedClient.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeActivity: true}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-after-worker-restart")
	harness.acp.mu.Lock()
	refreshCount, resumeCountAfterRelease := harness.acp.refreshCount, harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if refreshCount != 0 || resumeCountAfterRelease != 1 {
		t.Fatalf("provider calls after restart release: refresh=%d resume=%d, want 0/1", refreshCount, resumeCountAfterRelease)
	}
	if _, err := os.Stat(restarted.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native owner marker remains after restart refresh: %v", err)
	}
	harness.acp.Complete("req-after-worker-restart")
}

func TestInvalidNativeOwnershipMarkerFailsClosed(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtime.NativeOwnerPath(), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runtime.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing ACP work until it is recovered explicitly") {
		t.Fatalf("invalid native marker error = %v", err)
	}
	fake.mu.Lock()
	startCount, resumeCount := fake.startCount, fake.resumeCount
	fake.mu.Unlock()
	if startCount != 0 || resumeCount != 0 {
		t.Fatalf("ACP transport opened despite invalid native marker: start=%d resume=%d", startCount, resumeCount)
	}
}

func TestScheduledRefreshKeepsItsQueueBoundaryAheadOfNewPrompts(t *testing.T) {
	worker := &Worker{wake: make(chan struct{}, 1)}
	if state := worker.scheduleRefresh(); state != "scheduled" {
		t.Fatalf("idle refresh state = %q", state)
	}
	worker.mu.Lock()
	worker.queue = append(worker.queue, queuedPrompt{RequestID: "new-after-refresh"})
	worker.mu.Unlock()
	if prompt, _, ok := worker.nextPrompt(context.Background()); ok {
		t.Fatalf("new prompt %q beat the scheduled refresh", prompt.RequestID)
	}

	worker = &Worker{
		wake:  make(chan struct{}, 1),
		queue: []queuedPrompt{{RequestID: "already-queued"}},
	}
	if state := worker.scheduleRefresh(); state != "deferred" {
		t.Fatalf("queued refresh state = %q", state)
	}
	worker.mu.Lock()
	worker.queue = append(worker.queue, queuedPrompt{RequestID: "new-after-barrier"})
	worker.mu.Unlock()
	prompt, _, ok := worker.nextPrompt(context.Background())
	if !ok || prompt.RequestID != "already-queued" {
		t.Fatalf("pre-existing queued prompt = %#v, %t", prompt, ok)
	}
	worker.mu.Lock()
	worker.activeCancel()
	worker.active, worker.activeCancel = nil, nil
	worker.mu.Unlock()
	if prompt, _, ok := worker.nextPrompt(context.Background()); ok {
		t.Fatalf("post-barrier prompt %q ran before refresh", prompt.RequestID)
	}
}

func TestStalledNativeRefreshTimesOutRecoversAndReleasesQueuedPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.refreshWait = true
	harness.acp.mu.Unlock()
	refresh, err := harness.client.Send(context.Background(), Command{Type: "refresh"})
	if err != nil || refresh.Type != "refresh_scheduled" {
		t.Fatalf("refresh = %#v, %v", refresh, err)
	}
	waitFor(t, 2*time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && status.Refreshing
	})
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-stalled-refresh", Message: "continue"})
	if err != nil || accepted.State != "queued" {
		t.Fatalf("prompt during refresh = %#v, %v", accepted, err)
	}
	waitStarted(t, harness.acp.started, "req-after-stalled-refresh")
	harness.acp.mu.Lock()
	resumeCount := harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("exact recovery Resume calls = %d, want 1", resumeCount)
	}
	harness.acp.Complete("req-after-stalled-refresh")
	waitFor(t, 2*time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && !status.Refreshing && status.Queued == 0 && status.ActiveRequestID == ""
	})
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var warned, recovered bool
	for _, line := range lines {
		warned = warned || line.Type == "native_refresh_warning"
		recovered = recovered || line.Type == "native_refresh_recovered"
	}
	if !warned || !recovered {
		t.Fatalf("refresh history warned=%t recovered=%t: %#v", warned, recovered, lines)
	}
}

func TestPostTurnIdentityDriftQuarantinesTransportBeforeQueuedPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.inspectNative = "native-replacement"
	harness.acp.resumeNativeID = "native-replacement"
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-before-drift", Message: "first"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-before-drift")
	if queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-must-not-run", Message: "second"}); err != nil || queued.State != "queued" {
		t.Fatalf("queued prompt = %#v, %v", queued, err)
	}
	harness.acp.Complete("req-before-drift")

	waitFor(t, 3*time.Second, func() bool {
		current, err := harness.repository.GetSession(context.Background(), "S-worker")
		return err == nil && current.WorkerPID == 0 && current.Status == session.StatusStopped
	})
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("queued prompt reached drifted ACP handle: %s", requestID)
	case <-time.After(250 * time.Millisecond):
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable, rejected bool
	for _, line := range lines {
		unavailable = unavailable || line.Type == "transport_unavailable"
		rejected = rejected || (line.Type == "turn_error" && line.RequestID == "req-must-not-run")
	}
	if !unavailable || !rejected {
		t.Fatalf("quarantine history unavailable=%t rejected=%t: %#v", unavailable, rejected, lines)
	}
}

func TestPromptFailureRecoversOrQuarantinesBeforeQueuedPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.promptErr = errors.New("bridge disconnected")
	harness.acp.resumeNativeID = "native-replacement"
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-bridge-failure", Message: "first"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-bridge-failure")
	if queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-bridge-failure", Message: "second"}); err != nil || queued.State != "queued" {
		t.Fatalf("queued prompt = %#v, %v", queued, err)
	}
	harness.acp.Complete("req-bridge-failure")
	waitFor(t, 3*time.Second, func() bool {
		current, err := harness.repository.GetSession(context.Background(), "S-worker")
		return err == nil && current.WorkerPID == 0 && current.Status == session.StatusStopped
	})
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("queued prompt reached failed ACP handle: %s", requestID)
	case <-time.After(250 * time.Millisecond):
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var rejected bool
	for _, line := range lines {
		rejected = rejected || (line.Type == "turn_error" && line.RequestID == "req-after-bridge-failure")
	}
	if !rejected {
		t.Fatalf("queued prompt was not rejected after bridge failure: %#v", lines)
	}
}

func TestCanonicalConversationCannotStartWithoutACPTransportIdentity(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "", "native-canonical")
	if _, err := runtime.connectACP(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing to start a replacement conversation") {
		t.Fatalf("connectACP() error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.startCount != 0 || fake.resumeCount != 0 {
		t.Fatalf("Start=%d Resume=%d, want neither called", fake.startCount, fake.resumeCount)
	}
}

func TestResumeRejectsProviderIdentityDrift(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "acp-canonical-transport", "native-canonical")
	fake.resumeNativeID = "native-replacement"
	if _, err := runtime.connectACP(context.Background()); err == nil || !strings.Contains(err.Error(), "expected canonical provider conversation") {
		t.Fatalf("connectACP() error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.startCount != 0 || fake.resumeCount != 1 {
		t.Fatalf("Start=%d Resume=%d, want exact Resume only", fake.startCount, fake.resumeCount)
	}
}

func disconnectedIdentityWorker(t *testing.T, acpSessionID, canonicalID string) (*Worker, *fakeACP) {
	t.Helper()
	root := t.TempDir()
	repository, err := store.Open(context.Background(), filepath.Join(root, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	createdTask, err := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-identity", Title: "Identity test"})
	if err != nil {
		t.Fatal(err)
	}
	createdSession, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-identity", TaskID: createdTask.ID, Agent: "codex", Provider: "openai", RepoPath: root,
		ACPSessionID: acpSessionID, NativeSessionID: canonicalID, ProviderConversationID: canonicalID,
		ConversationIDSource: session.ConversationSourceNative, Status: session.StatusWaiting,
		MetadataJSON: json.RawMessage(`{"permission":"read-approval"}`),
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
	return runtime, fake
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

func TestRuntimeTransportMarkerPreventsBlindLegacyCutover(t *testing.T) {
	if usesRuntimeTransport(nil) || usesRuntimeTransport(json.RawMessage(`{"permission":"read-approval"}`)) {
		t.Fatal("legacy Session metadata was treated as runtime-backed")
	}
	metadata := json.RawMessage(`{"permission":"read-approval","acp_transport":"acpx-runtime-v1"}`)
	if !usesRuntimeTransport(metadata) {
		t.Fatal("runtime-backed Session metadata was not recognized")
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

	executable := filepath.Join(root, "acpctl")
	build := exec.Command("go", "build", "-o", executable, "./cmd/acpctl")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acpctl: %v\n%s", err, output)
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
