package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/native"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

type fakeACP struct {
	mu               sync.Mutex
	active           map[string]chan struct{}
	cancelled        map[string]bool
	cancelCount      int
	cancelGate       <-chan struct{}
	cancelInFlight   int
	trackClose       bool
	closed           bool
	closeWhileCancel bool
	started          chan string
	concurrent       int
	maxConcurrent    int
	startCount       int
	resumeCount      int
	resumeErr        error
	resumeHook       func()
	resumeNativeID   string
	inspectACP       string
	inspectNative    string
	promptErr        error
	promptReturnGate <-chan struct{}
	promptReturning  chan string
	refreshCount     int
	refreshWait      bool
	refreshErr       error
	refreshNativeID  string
	refreshGate      <-chan struct{}
	refreshStarted   chan int
	configureCount   int
	configureErr     error
	configureHook    func(acp.StartSpec)
	configureSpecs   []acp.StartSpec
	calls            []string
}

func newFakeACP() *fakeACP {
	return &fakeACP{active: make(map[string]chan struct{}), cancelled: make(map[string]bool), started: make(chan string, 20)}
}

func (f *fakeACP) Start(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.startCount++
	f.calls = append(f.calls, "start")
	f.mu.Unlock()
	return acp.SessionInfo{ACPXRecordID: "acpx-start", ACPSessionID: "acp-start", NativeSessionID: "native-start"}, nil
}

func (f *fakeACP) Resume(_ context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.resumeCount++
	f.calls = append(f.calls, "resume")
	resumeErr := f.resumeErr
	resumeHook := f.resumeHook
	nativeID := f.resumeNativeID
	f.mu.Unlock()
	if resumeHook != nil {
		resumeHook()
	}
	if resumeErr != nil {
		return acp.SessionInfo{}, resumeErr
	}
	if nativeID == "" {
		nativeID = "native-start"
	}
	return acp.SessionInfo{ACPXRecordID: "acpx-resumed", ACPSessionID: spec.ResumeACPSessionID, NativeSessionID: nativeID}, nil
}

func (f *fakeACP) Refresh(ctx context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.refreshCount++
	f.calls = append(f.calls, "refresh")
	refreshWait := f.refreshWait
	refreshErr := f.refreshErr
	refreshGate := f.refreshGate
	refreshStarted := f.refreshStarted
	refreshCount := f.refreshCount
	nativeID := f.refreshNativeID
	f.mu.Unlock()
	if refreshStarted != nil {
		refreshStarted <- refreshCount
	}
	if refreshGate != nil {
		select {
		case <-refreshGate:
		case <-ctx.Done():
			return acp.SessionInfo{}, ctx.Err()
		}
	}
	if refreshWait {
		<-ctx.Done()
		return acp.SessionInfo{}, ctx.Err()
	}
	if refreshErr != nil {
		return acp.SessionInfo{}, refreshErr
	}
	if nativeID == "" {
		nativeID = "native-start"
	}
	return acp.SessionInfo{ACPXRecordID: "acpx-refreshed", ACPSessionID: spec.ResumeACPSessionID, NativeSessionID: nativeID}, nil
}

func (f *fakeACP) Configure(_ context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.configureCount++
	f.calls = append(f.calls, "configure")
	f.configureSpecs = append(f.configureSpecs, spec)
	configureErr := f.configureErr
	configureHook := f.configureHook
	f.mu.Unlock()
	if configureHook != nil {
		configureHook(spec)
	}
	if configureErr != nil {
		return acp.SessionInfo{}, configureErr
	}
	nativeID := spec.ExpectedNativeSessionID
	if nativeID == "" {
		nativeID = "native-start"
	}
	return acp.SessionInfo{
		ACPXRecordID: "acpx-configured", ACPSessionID: spec.ResumeACPSessionID, NativeSessionID: nativeID,
	}, nil
}

func (f *fakeACP) Prompt(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
	release := make(chan struct{})
	f.mu.Lock()
	f.active[spec.RequestID] = release
	f.calls = append(f.calls, "prompt:"+spec.RequestID)
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
	promptReturnGate := f.promptReturnGate
	promptReturning := f.promptReturning
	f.mu.Unlock()
	if promptReturning != nil {
		promptReturning <- spec.RequestID
	}
	if promptReturnGate != nil {
		select {
		case <-promptReturnGate:
		case <-ctx.Done():
			f.finish(spec.RequestID)
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1}, ctx.Err()
		}
	}
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
	f.cancelInFlight++
	gate := f.cancelGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelInFlight--
	f.cancelCount++
	for requestID, release := range f.active {
		if !f.cancelled[requestID] {
			f.cancelled[requestID] = true
			close(release)
		}
	}
	return nil
}

func (f *fakeACP) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.trackClose {
		return nil
	}
	f.closed = true
	f.closeWhileCancel = f.cancelInFlight > 0
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

func (f *permissionACP) Refresh(context.Context, acp.StartSpec) (acp.SessionInfo, error) {
	return acp.SessionInfo{ACPXRecordID: "acpx-permission-refreshed", ACPSessionID: "acp-permission", NativeSessionID: "native-permission"}, nil
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

func newProviderOptionsWorker(t *testing.T) (*Worker, *fakeACP, *store.Store) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "atc-provider-options-worker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, directory := range []string{filepath.Join(root, "repo"), filepath.Join(root, "workers"), filepath.Join(root, "logs")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(context.Background(), filepath.Join(root, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	createdTask, err := repository.CreateTask(context.Background(), task.CreateParams{ID: "T-provider-options", Title: "Provider options"})
	if err != nil {
		t.Fatal(err)
	}
	createdSession, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-provider-options", TaskID: createdTask.ID, Agent: "codex", Provider: "openai",
		RepoPath: filepath.Join(root, "repo"), Status: session.StatusStarting,
		MetadataJSON: json.RawMessage(`{"permission":"read-approval","model":"model-selected","reasoning_effort":"high"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeACP()
	runtime, err := New(Config{
		Session: createdSession, Profile: agents.Profile{Name: "codex", Provider: "openai", Command: "codex"},
		Repository: repository, ACP: fake, WorkersDirectory: filepath.Join(root, "workers"), LogsDirectory: filepath.Join(root, "logs"),
		RefreshTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, fake, repository
}

func TestWorkerPersistsIdentityBeforeProviderConfigurationFailure(t *testing.T) {
	runtime, fake, repository := newProviderOptionsWorker(t)
	observed := make(chan session.Session, 1)
	fake.mu.Lock()
	fake.configureErr = &acp.ProviderConfigurationError{}
	fake.configureHook = func(acp.StartSpec) {
		current, _ := repository.GetSession(context.Background(), "S-provider-options")
		observed <- current
	}
	fake.mu.Unlock()
	err := runtime.Run(context.Background())
	var configurationError *acp.ProviderConfigurationError
	if !errors.As(err, &configurationError) {
		t.Fatalf("Worker.Run() error = %T %v", err, err)
	}
	persistedBeforeFailure := <-observed
	if persistedBeforeFailure.ACPSessionID != "acp-start" || persistedBeforeFailure.NativeSessionID != "native-start" || persistedBeforeFailure.ACPXRecordID != "acpx-start" {
		t.Fatalf("identity before Configure = %#v", persistedBeforeFailure)
	}
	current, err := repository.GetSession(context.Background(), "S-provider-options")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != session.StatusFailed || current.ACPSessionID != "acp-start" || current.NativeSessionID != "native-start" || current.ProviderConversationID == "" {
		t.Fatalf("failed Session lost canonical identity = %#v", current)
	}
	fake.mu.Lock()
	calls := append([]string(nil), fake.calls...)
	fake.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"start", "configure"}) {
		t.Fatalf("provider calls = %v, want identity then configuration and no prompt", calls)
	}
	lines, err := sessionlog.ReadFile(runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var safeFailure bool
	for _, line := range lines {
		if line.Type != "worker_error" {
			continue
		}
		var failure map[string]string
		if json.Unmarshal(line.Data, &failure) == nil && failure["source"] == "provider" && failure["code"] == "configuration_failed" && !strings.Contains(failure["message"], "SECRET") {
			safeFailure = true
		}
	}
	if !safeFailure {
		t.Fatalf("worker log did not retain a safe structured configuration failure: %#v", lines)
	}
}

func TestWorkerAppliesProviderOptionsAfterIdentityAndEveryRefreshBeforePrompt(t *testing.T) {
	runtime, fake, repository := newProviderOptionsWorker(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	client := NewSocketClient(runtime.SocketPath())
	waitFor(t, 3*time.Second, func() bool {
		probe, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer stop()
		return client.Ping(probe) == nil
	})
	accepted, err := client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-provider-options", Message: "verify"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt = %#v, %v", accepted, err)
	}
	waitStarted(t, fake.started, "req-provider-options")
	fake.mu.Lock()
	calls := append([]string(nil), fake.calls...)
	specs := append([]acp.StartSpec(nil), fake.configureSpecs...)
	fake.mu.Unlock()
	want := []string{"start", "configure", "refresh", "configure", "prompt:req-provider-options"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("provider option order = %v, want %v", calls, want)
	}
	if len(specs) != 2 {
		t.Fatalf("Configure specs = %#v", specs)
	}
	for _, spec := range specs {
		if spec.ResumeACPSessionID != "acp-start" || spec.ExpectedNativeSessionID != "native-start" || spec.Model != "model-selected" || spec.ReasoningEffort != "high" {
			t.Fatalf("Configure spec = %#v", spec)
		}
	}
	current, err := repository.GetSession(context.Background(), "S-provider-options")
	if err != nil || current.ACPSessionID != "acp-start" || current.NativeSessionID != "native-start" {
		t.Fatalf("configured Session = %#v, %v", current, err)
	}
	fake.Complete("req-provider-options")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("provider options worker did not stop")
	}
}

func TestWorkerReappliesProviderOptionsAfterRefreshFallsBackToExactResume(t *testing.T) {
	runtime, fake, _ := newProviderOptionsWorker(t)
	fake.mu.Lock()
	fake.refreshErr = errors.New("refresh unavailable")
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	client := NewSocketClient(runtime.SocketPath())
	waitFor(t, 3*time.Second, func() bool {
		probe, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer stop()
		return client.Ping(probe) == nil
	})
	if _, err := client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-resume-options", Message: "verify fallback"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, fake.started, "req-resume-options")
	fake.mu.Lock()
	calls := append([]string(nil), fake.calls...)
	specs := append([]acp.StartSpec(nil), fake.configureSpecs...)
	fake.mu.Unlock()
	want := []string{"start", "configure", "refresh", "resume", "configure", "prompt:req-resume-options"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("fallback provider option order = %v, want %v", calls, want)
	}
	if len(specs) != 2 || specs[1].ResumeACPSessionID != "acp-start" || specs[1].ExpectedNativeSessionID != "native-start" || specs[1].Model != "model-selected" || specs[1].ReasoningEffort != "high" {
		t.Fatalf("fallback Configure specs = %#v", specs)
	}
	fake.Complete("req-resume-options")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fallback provider options worker did not stop")
	}
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
	if nativeOwner, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "owner-active", NativeOwnerRole: NativeOwnerRoleDirect}); err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("native acquire during accepted ACP work = %#v, %v", nativeOwner, err)
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
	pong, err := harness.client.Send(context.Background(), Command{Type: "ping"})
	if err != nil || pong.Type != "pong" || pong.ProtocolVersion != ProtocolVersion || !reflect.DeepEqual(pong.Capabilities, []string{CapabilityExclusiveNativeOwner}) {
		t.Fatalf("worker capability pong = %#v, %v", pong, err)
	}
	harness.acp.Complete("req-2")
	waitFor(t, 2*time.Second, func() bool {
		current, err := harness.client.Send(context.Background(), Command{Type: "status"})
		return err == nil && current.ActiveRequestID == "" && current.Queued == 0 && current.Status == "waiting"
	})
	if got := harness.acp.maximumConcurrency(); got != 1 {
		t.Fatalf("maximum ACP concurrency = %d, want 1", got)
	}
	harness.acp.mu.Lock()
	providerCalls := append([]string(nil), harness.acp.calls...)
	harness.acp.mu.Unlock()
	wantProviderCalls := []string{"start", "refresh", "prompt:req-1", "refresh", "prompt:req-2"}
	if !reflect.DeepEqual(providerCalls, wantProviderCalls) {
		t.Fatalf("ACP-only FIFO did not rehydrate every prompt: calls=%v want=%v", providerCalls, wantProviderCalls)
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

func TestCancelInterruptsPredispatchRehydrationAndNextQueuedPromptProceeds(t *testing.T) {
	harness := startWorkerHarness(t)
	refreshGate := make(chan struct{})
	refreshStarted := make(chan int, 3)
	harness.acp.mu.Lock()
	harness.acp.refreshGate = refreshGate
	harness.acp.refreshStarted = refreshStarted
	harness.acp.mu.Unlock()
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-cancel-rehydration", Message: "do not dispatch"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt receipt = %#v, %v", accepted, err)
	}
	select {
	case <-refreshStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-dispatch refresh did not start")
	}
	queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-cancelled-rehydration", Message: "continue next"})
	if err != nil || queued.State != "queued" {
		t.Fatalf("queued prompt = %#v, %v", queued, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "cancel", RequestID: "req-cancel-rehydration"}); err != nil {
		t.Fatal(err)
	}
	close(refreshGate)
	waitStarted(t, harness.acp.started, "req-after-cancelled-rehydration")
	harness.acp.Complete("req-after-cancelled-rehydration")
	waitFor(t, 2*time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && status.ActiveRequestID == "" && status.Status == string(session.StatusWaiting)
	})
	harness.acp.mu.Lock()
	cancelCount, refreshCount := harness.acp.cancelCount, harness.acp.refreshCount
	harness.acp.mu.Unlock()
	if cancelCount != 0 || refreshCount != 2 {
		t.Fatalf("pre-dispatch cancel runtime calls: cancel=%d refresh=%d, want 0/2", cancelCount, refreshCount)
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var cancelled, startedCancelled bool
	var cancelRequestedAt, cancelledAt uint64
	for _, line := range lines {
		if line.RequestID != "req-cancel-rehydration" {
			continue
		}
		startedCancelled = startedCancelled || line.Type == "turn_started"
		if line.Type == "cancel_requested" {
			cancelRequestedAt = line.Seq
		}
		if line.Type == "turn_completed" {
			var receipt acp.PromptReceipt
			cancelled = json.Unmarshal(line.Data, &receipt) == nil && receipt.StopReason == "cancelled"
			cancelledAt = line.Seq
		}
	}
	if !cancelled || startedCancelled || cancelRequestedAt == 0 || cancelRequestedAt >= cancelledAt {
		t.Fatalf("pre-dispatch cancellation history cancelled=%t turnStarted=%t requested/completed=%d/%d: %#v", cancelled, startedCancelled, cancelRequestedAt, cancelledAt, lines)
	}
}

func TestCancelTerminalizesAcceptedFIFOHeadBeforeDispatch(t *testing.T) {
	harness := startWorkerHarness(t)
	const requestID = "req-ready-cancel"
	harness.runtime.mu.Lock()
	harness.runtime.queue = append(harness.runtime.queue, queuedPrompt{RequestID: requestID, Message: "never dispatch", DisplayMessage: "never dispatch"})
	harness.runtime.seenRequestIDs[requestID] = struct{}{}
	harness.runtime.mu.Unlock()

	if err := harness.runtime.cancel(context.Background(), requestID); err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	queued := len(harness.runtime.queue)
	_, seen := harness.runtime.seenRequestIDs[requestID]
	harness.runtime.mu.Unlock()
	if queued != 0 || seen {
		t.Fatalf("cancelled accepted head remained runnable: queued=%d seen=%t", queued, seen)
	}
	select {
	case started := <-harness.acp.started:
		t.Fatalf("cancelled accepted head reached provider: %s", started)
	default:
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var requestedAt, completedAt uint64
	for _, line := range lines {
		if line.RequestID != requestID {
			continue
		}
		if line.Type == "cancel_requested" {
			requestedAt = line.Seq
		}
		if line.Type == "turn_completed" {
			completedAt = line.Seq
			var receipt acp.PromptReceipt
			if json.Unmarshal(line.Data, &receipt) != nil || receipt.StopReason != "cancelled" {
				t.Fatalf("accepted-head terminal = %s", line.Data)
			}
		}
	}
	if requestedAt != 0 || completedAt == 0 {
		t.Fatalf("never-started cancellation should be one terminal record, requested/completed=%d/%d: %#v", requestedAt, completedAt, lines)
	}
}

func TestCancelClaimsTerminalBeforeProviderCompletionCanPublish(t *testing.T) {
	harness := startWorkerHarness(t)
	returnGate := make(chan struct{})
	returning := make(chan string, 1)
	harness.acp.mu.Lock()
	harness.acp.promptReturnGate = returnGate
	harness.acp.promptReturning = returning
	harness.acp.mu.Unlock()

	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-terminal-race", Message: "race completion"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-terminal-race")
	harness.acp.Complete("req-terminal-race")
	select {
	case requestID := <-returning:
		if requestID != "req-terminal-race" {
			t.Fatalf("provider returning request = %q", requestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not reach its completion boundary")
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "cancel", RequestID: "req-terminal-race"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && status.ActiveRequestID == ""
	})
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var requestedAt, terminalAt uint64
	terminalCount := 0
	for _, line := range lines {
		if line.RequestID != "req-terminal-race" {
			continue
		}
		if line.Type == "cancel_requested" {
			requestedAt = line.Seq
		}
		if line.Type == "turn_completed" || line.Type == "turn_error" {
			terminalCount++
			terminalAt = line.Seq
			if line.Type != "turn_completed" || !bytes.Contains(line.Data, []byte(`"stopReason":"cancelled"`)) {
				t.Fatalf("completion raced past cancellation claim: %#v", line)
			}
		}
	}
	if terminalCount != 1 || requestedAt == 0 || terminalAt <= requestedAt {
		t.Fatalf("terminal claim history terminals=%d requested/completed=%d/%d: %#v", terminalCount, requestedAt, terminalAt, lines)
	}
}

func TestEnqueueJournalFailureLeavesNoRunnablePrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	if err := harness.runtime.log.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := harness.runtime.enqueue("req-journal-failure", "must not run", "must not run")
	if err == nil || state != "" {
		t.Fatalf("enqueue with closed journal = state %q, error %v", state, err)
	}
	harness.runtime.mu.Lock()
	queued := len(harness.runtime.queue)
	_, seen := harness.runtime.seenRequestIDs["req-journal-failure"]
	harness.runtime.mu.Unlock()
	if queued != 0 || seen {
		t.Fatalf("journal-rejected prompt remained runnable: queued=%d seen=%t", queued, seen)
	}
	harness.runtime.signalWake()
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("journal-rejected prompt reached provider: %s", requestID)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWorkerShutdownJoinsAcceptLoopAndClosesBlockedConnections(t *testing.T) {
	harness := startWorkerHarness(t)
	connection, err := net.Dial("unix", harness.runtime.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(`{"type":"status"`)); err != nil {
		t.Fatal(err)
	}
	// A successful second request proves the accept loop moved past the first
	// connection and its handler is blocked in Scanner waiting for a newline.
	if _, err := harness.client.Send(context.Background(), Command{Type: "status"}); err != nil {
		t.Fatal(err)
	}

	harness.cancel()
	waitFor(t, 3*time.Second, func() bool {
		current, loadErr := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
		return loadErr == nil && current.WorkerPID == 0 && current.WorkerSocket == ""
	})
}

func TestWorkerShutdownJoinsCancelHandlerBeforeClosingACP(t *testing.T) {
	harness := startWorkerHarness(t)
	cancelGate := make(chan struct{})
	harness.acp.mu.Lock()
	harness.acp.trackClose = true
	harness.acp.cancelGate = cancelGate
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "REQ-stop-cancel-race", Message: "continue"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "REQ-stop-cancel-race")
	cancelDone := make(chan error, 1)
	go func() {
		_, err := harness.client.Send(context.Background(), Command{Type: "cancel", RequestID: "REQ-stop-cancel-race"})
		cancelDone <- err
	}()
	waitFor(t, time.Second, func() bool {
		harness.acp.mu.Lock()
		defer harness.acp.mu.Unlock()
		return harness.acp.cancelInFlight == 1
	})
	stopDone := make(chan error, 1)
	go func() {
		_, err := harness.client.Send(context.Background(), Command{Type: "stop"})
		stopDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	harness.acp.mu.Lock()
	closedEarly := harness.acp.closed
	harness.acp.mu.Unlock()
	if closedEarly {
		t.Fatal("ACP closed while cancel handler was still using it")
	}
	close(cancelGate)
	if err := <-cancelDone; err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		harness.acp.mu.Lock()
		defer harness.acp.mu.Unlock()
		return harness.acp.closed
	})
	harness.acp.mu.Lock()
	closeWhileCancel := harness.acp.closeWhileCancel
	harness.acp.mu.Unlock()
	if closeWhileCancel {
		t.Fatal("ACP Close overlapped an in-flight cancel handler")
	}
}

func TestQueuedCancellationJournalFailureStopsBeforePromptCanRun(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	history, err := sessionlog.Open(runtime.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	runtime.log = history
	runtime.hub = newHub(history.LastSequence())
	runtime.queue = []queuedPrompt{{RequestID: "REQ-cancel-journal", Message: "must not run"}}
	runtime.seenRequestIDs["REQ-cancel-journal"] = struct{}{}
	stopContext, stop := context.WithCancel(context.Background())
	runtime.stop = stop
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.cancel(context.Background(), "REQ-cancel-journal"); err == nil || !strings.Contains(err.Error(), "Session log is closed") {
		t.Fatalf("queued cancel journal error = %v", err)
	}
	runtime.mu.Lock()
	queued, transportError := len(runtime.queue), runtime.transportError
	runtime.mu.Unlock()
	if queued != 0 || !strings.Contains(transportError, "persist cancellation") {
		t.Fatalf("failed cancellation queue=%d transportError=%q", queued, transportError)
	}
	select {
	case <-stopContext.Done():
	default:
		t.Fatal("journal-uncertain cancellation did not stop the worker")
	}
	runtime.signalWake()
	select {
	case requestID := <-fake.started:
		t.Fatalf("journal-uncertain cancelled prompt reached provider: %s", requestID)
	case <-time.After(50 * time.Millisecond):
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
			current, loadErr := harness.runtime.config.Repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
			return loadErr == nil && current.WorkerPID == 0 && current.Status == session.StatusStopped
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

func TestNativeOwnershipRejectsACPThenReleaseRehydratesBeforePrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-native-barrier"
	reconcileRelease := make(chan struct{})
	harness.reconcile.mu.Lock()
	harness.reconcile.wait = reconcileRelease
	harness.reconcile.mu.Unlock()
	acquired, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect})
	if err != nil || acquired.Type != "native_acquired" || acquired.State != "acquired" {
		t.Fatalf("native acquire = %#v, %v", acquired, err)
	}
	if duplicate, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect}); err != nil || duplicate.State != "already_acquired" || duplicate.NativeGeneration != acquired.NativeGeneration {
		t.Fatalf("idempotent native acquire = %#v, %v", duplicate, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "another-owner", NativeOwnerRole: NativeOwnerRoleDirect}); err == nil || !strings.Contains(err.Error(), "another native provider process") {
		t.Fatalf("competing native acquire error = %v", err)
	}
	marker, err := os.Stat(harness.runtime.NativeOwnerPath())
	if err != nil || marker.Mode().Perm() != 0o600 {
		t.Fatalf("native ownership marker = %#v, %v", marker, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-during-native", Message: "continue"}); err == nil || !strings.Contains(err.Error(), "native harness is running") || !strings.Contains(err.Error(), "prompt was not accepted") {
		t.Fatalf("prompt while native owns error = %v", err)
	}
	if duplicate, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect}); err != nil || duplicate.State != "already_acquired" {
		t.Fatalf("exclusive native reuse = %#v, %v", duplicate, err)
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || !status.NativeOwned || status.Queued != 0 || status.ActiveRequestID != "" {
		t.Fatalf("native-owned status = %#v, %v", status, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "stop"}); err == nil || !strings.Contains(err.Error(), "exit the native provider") {
		t.Fatalf("stop while native owns error = %v", err)
	}
	released, err := harness.client.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeGeneration: acquired.NativeGeneration, NativeActivity: true})
	if err != nil || released.State != "released" {
		t.Fatalf("native release = %#v, %v", released, err)
	}
	releaseMarker, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil || !bytes.Contains(releaseMarker, []byte(`"state": "refresh_pending"`)) {
		t.Fatalf("native release freshness marker = %q, %v", releaseMarker, err)
	}
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-native", Message: "continue again"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("post-release ACP prompt = %#v, %v", accepted, err)
	}
	select {
	case <-harness.reconcile.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider transcript reconciliation did not start after native release")
	}
	waitStarted(t, harness.acp.started, "req-after-native")
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native release freshness marker remains after exact ACP rehydration: %v", err)
	}
	close(reconcileRelease)
	waitFor(t, 2*time.Second, func() bool {
		lines, readErr := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
		if readErr != nil {
			return false
		}
		for _, line := range lines {
			if line.Type == "provider_transcript_reconciled" {
				return true
			}
		}
		return false
	})
	harness.acp.mu.Lock()
	refreshCount, resumeCount := harness.acp.refreshCount, harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if refreshCount != 1 || resumeCount != 0 {
		t.Fatalf("exact ACP rehydration after native release: refresh=%d resume=%d, want 1/0", refreshCount, resumeCount)
	}
	wantCalls := []string{"start", "refresh", "prompt:req-after-native"}
	harness.acp.mu.Lock()
	calls := append([]string(nil), harness.acp.calls...)
	harness.acp.mu.Unlock()
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("provider activation order = %v, want %v", calls, wantCalls)
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var reconciledAt, dispatchedAt uint64
	var readyRecorded, falselyQueued bool
	for _, line := range lines {
		switch {
		case line.RequestID == "req-after-native" && line.Type == "prompt_ready":
			readyRecorded = true
		case line.RequestID == "req-after-native" && line.Type == "prompt_queued":
			falselyQueued = true
		case line.Type == "provider_transcript_reconciled":
			reconciledAt = line.Seq
			var payload map[string]string
			if json.Unmarshal(line.Data, &payload) != nil || len(payload) != 2 || payload["source"] != "provider-store" || payload["state"] != "reconciled" {
				t.Fatalf("provider transcript reconciliation payload = %s", line.Data)
			}
		case line.Type == "turn_started" && line.RequestID == "req-after-native":
			dispatchedAt = line.Seq
		}
	}
	for _, line := range lines {
		if line.RequestID == "req-during-native" {
			t.Fatalf("rejected native-open prompt mutated log: %#v", line)
		}
	}
	if !readyRecorded || falselyQueued {
		t.Fatalf("ready ACP request provenance ready=%t falselyQueued=%t", readyRecorded, falselyQueued)
	}
	if reconciledAt == 0 || dispatchedAt == 0 || dispatchedAt >= reconciledAt {
		t.Fatalf("provider transcript reconcile/dispatch ordering = %d/%d", reconciledAt, dispatchedAt)
	}
	harness.reconcile.mu.Lock()
	if harness.reconcile.calls != 1 || harness.reconcile.conversationID != "native-start" || harness.reconcile.workingDir == "" {
		t.Fatalf("provider transcript reconcile = calls %d, conversation %q, directory %q", harness.reconcile.calls, harness.reconcile.conversationID, harness.reconcile.workingDir)
	}
	harness.reconcile.mu.Unlock()
	harness.acp.Complete("req-after-native")
}

func TestReleasedNativeGenerationRehydratesBeforeEveryACPPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-alternating-surfaces"
	acquired, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_release", OwnerToken: ownerToken, NativeGeneration: acquired.NativeGeneration, NativeActivity: true,
	}); err != nil {
		t.Fatal(err)
	}

	for _, requestID := range []string{"req-after-native-one", "req-after-native-two"} {
		accepted, promptErr := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: requestID, Message: "continue exact conversation"})
		if promptErr != nil || accepted.State != "ready" {
			t.Fatalf("%s accepted = %#v, %v", requestID, accepted, promptErr)
		}
		waitStarted(t, harness.acp.started, requestID)
		harness.acp.Complete(requestID)
		waitFor(t, 2*time.Second, func() bool {
			status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
			return statusErr == nil && status.ActiveRequestID == "" && status.Status == string(session.StatusWaiting)
		})
	}

	harness.acp.mu.Lock()
	calls := append([]string(nil), harness.acp.calls...)
	harness.acp.mu.Unlock()
	want := []string{"start", "refresh", "prompt:req-after-native-one", "refresh", "prompt:req-after-native-two"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("alternating provider calls = %v, want %v", calls, want)
	}
}

func TestNativeActivationDuringACPRehydrationIsRejectedBeforeProviderStart(t *testing.T) {
	harness := startWorkerHarness(t)
	acquired, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: "owner-release-during-refresh", NativeOwnerRole: NativeOwnerRoleDirect,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_release", OwnerToken: "owner-release-during-refresh", NativeGeneration: acquired.NativeGeneration, NativeActivity: true,
	}); err != nil {
		t.Fatal(err)
	}
	refreshGate := make(chan struct{})
	refreshStarted := make(chan int, 4)
	harness.acp.mu.Lock()
	harness.acp.refreshGate = refreshGate
	harness.acp.refreshStarted = refreshStarted
	harness.acp.mu.Unlock()
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-release-during-refresh", Message: "continue exactly"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt receipt = %#v, %v", accepted, err)
	}
	select {
	case attempt := <-refreshStarted:
		if attempt != 1 {
			t.Fatalf("first refresh attempt = %d", attempt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first ACP refresh did not start")
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: "new-owner-during-refresh", NativeOwnerRole: NativeOwnerRoleDirect,
	}); err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("native activation during ACP rehydration error = %v", err)
	}
	close(refreshGate)
	waitStarted(t, harness.acp.started, "req-release-during-refresh")
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("release freshness marker remains before provider prompt: %v", err)
	}
	harness.acp.mu.Lock()
	calls := append([]string(nil), harness.acp.calls...)
	harness.acp.mu.Unlock()
	want := []string{"start", "refresh", "prompt:req-release-during-refresh"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("release/refresh provider order = %v, want %v", calls, want)
	}
	harness.acp.Complete("req-release-during-refresh")
}

func TestNativeActivationBetweenRehydrationAndDispatchIsRejected(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.runtime.mu.Lock()
	harness.runtime.active = &queuedPrompt{RequestID: "req-release-before-dispatch"}
	harness.runtime.mu.Unlock()
	epoch, err := harness.runtime.rehydrateACPForPrompt(context.Background(), "req-release-before-dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: "owner-between-rehydrate-dispatch", NativeOwnerRole: NativeOwnerRoleDirect,
	}); err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("native activation between rehydration and dispatch error = %v", err)
	}
	if err := harness.runtime.publishACPDispatch("req-release-before-dispatch", epoch); err != nil {
		t.Fatalf("exclusive ACP dispatch boundary = %v", err)
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if line.Type == "native_acquired" {
			t.Fatal("rejected native activation published ownership")
		}
	}
	harness.acp.mu.Lock()
	refreshCount := harness.acp.refreshCount
	harness.acp.mu.Unlock()
	if refreshCount != 1 {
		t.Fatalf("refreshes at exclusive dispatch boundary = %d, want 1", refreshCount)
	}
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("rejected native activation created ownership marker: %v", err)
	}
}

func TestCancelBetweenRehydrationAndDispatchPreventsTurnStart(t *testing.T) {
	harness := startWorkerHarness(t)
	promptContext, cancelPrompt := context.WithCancel(context.Background())
	harness.runtime.mu.Lock()
	harness.runtime.active = &queuedPrompt{RequestID: "req-cancel-before-dispatch"}
	harness.runtime.activeCancel = cancelPrompt
	harness.runtime.seenRequestIDs["req-cancel-before-dispatch"] = struct{}{}
	harness.runtime.mu.Unlock()
	epoch, err := harness.runtime.rehydrateACPForPrompt(promptContext, "req-cancel-before-dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.runtime.cancel(context.Background(), "req-cancel-before-dispatch"); err != nil {
		t.Fatal(err)
	}
	if err := harness.runtime.publishACPDispatch("req-cancel-before-dispatch", epoch); !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatch after cancellation = %v", err)
	}
	cancelled, claimed := harness.runtime.claimPromptTerminal("req-cancel-before-dispatch", true)
	if !claimed || !cancelled {
		t.Fatalf("cancelled terminal claim = cancelled %t claimed %t", cancelled, claimed)
	}
	harness.runtime.finishClaimedCancelledPrompt("req-cancel-before-dispatch")

	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var requestedAt, completedAt uint64
	for _, line := range lines {
		if line.RequestID != "req-cancel-before-dispatch" {
			continue
		}
		if line.Type == "turn_started" {
			t.Fatal("cancelled freshness boundary emitted turn_started")
		}
		if line.Type == "cancel_requested" {
			requestedAt = line.Seq
		}
		if line.Type == "turn_completed" {
			completedAt = line.Seq
		}
	}
	if requestedAt == 0 || completedAt <= requestedAt {
		t.Fatalf("boundary cancellation order = %d/%d: %#v", requestedAt, completedAt, lines)
	}
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("cancelled boundary reached provider Prompt: %s", requestID)
	default:
	}
}

func TestNewNativeActivationRejectsAcceptedACPWorkWithoutQueuingOrCancellingIt(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*Worker)
	}{
		{name: "active", setup: func(runtime *Worker) { runtime.active = &queuedPrompt{RequestID: "req-active"} }},
		{name: "queued", setup: func(runtime *Worker) { runtime.queue = []queuedPrompt{{RequestID: "req-queued"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
			if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			test.setup(runtime)
			runtime.mu.Unlock()
			_, _, err := runtime.acquireNative("new-native-owner", NativeOwnerRoleDirect, "")
			if err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") || !strings.Contains(err.Error(), "open Native CLI again") {
				t.Fatalf("native activation error = %v", err)
			}
			if _, markerErr := os.Stat(runtime.NativeOwnerPath()); !os.IsNotExist(markerErr) {
				t.Fatalf("rejected native activation created ownership marker: %v", markerErr)
			}
		})
	}
}

func TestNativeReservationRejectsACPBeforeProviderActivation(t *testing.T) {
	harness := startWorkerHarness(t)
	reserved, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: "owner-provider-activation", NativeOwnerRole: NativeOwnerRoleReservation,
	})
	if err != nil || reserved.State != "reserved" {
		t.Fatalf("native reservation = %#v, %v", reserved, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-before-native-activation", Message: "continue"}); err == nil || !strings.Contains(err.Error(), "active launch reservation") {
		t.Fatalf("ACP prompt during native reservation error = %v", err)
	}
	status, err := harness.client.Send(context.Background(), Command{
		Type: "native_reservation_status", OwnerToken: "owner-provider-activation",
	})
	if err != nil || status.State != "reserved" || status.NativeGeneration != reserved.NativeGeneration {
		t.Fatalf("reservation after rejected activation = %#v, %v", status, err)
	}
	activated, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: "owner-provider-activation", NativeOwnerRole: NativeOwnerRoleProvider, NativeGeneration: reserved.NativeGeneration,
	})
	if err != nil || activated.State != "activated" {
		t.Fatalf("provider activation retry = %#v, %v", activated, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_release", OwnerToken: "owner-provider-activation", NativeGeneration: reserved.NativeGeneration,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestACPRehydrationIntegrityFailureQuarantinesBeforeQueuedPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	refreshGate := make(chan struct{})
	refreshStarted := make(chan int, 1)
	harness.acp.mu.Lock()
	harness.acp.refreshErr = &acp.ProtocolError{Operation: "refresh", Message: "SECRET refresh transport failure"}
	harness.acp.resumeErr = &acp.ProtocolError{Operation: "resume", Message: "SECRET resume transport failure"}
	harness.acp.refreshGate = refreshGate
	harness.acp.refreshStarted = refreshStarted
	harness.acp.mu.Unlock()

	failed, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-rehydrate-fails", Message: "do not dispatch stale"})
	if err != nil || failed.State != "ready" {
		t.Fatalf("failed rehydration receipt = %#v, %v", failed, err)
	}
	select {
	case <-refreshStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-prompt refresh did not start")
	}
	queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-rehydrate-failure", Message: "must not dispatch"})
	if err != nil || queued.State != "queued" {
		t.Fatalf("queued behind rehydration = %#v, %v", queued, err)
	}
	close(refreshGate)
	waitFor(t, 3*time.Second, func() bool {
		current, getErr := harness.repository.GetSession(context.Background(), "S-worker")
		return getErr == nil && current.WorkerPID == 0 && current.Status == session.StatusStopped
	})
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("prompt reached provider without exact rehydration: %s", requestID)
	default:
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	terminal := map[string]bool{}
	for _, line := range lines {
		if line.Type != "turn_error" {
			continue
		}
		if bytes.Contains(line.Data, []byte("SECRET")) || len(line.Data) > 512 {
			t.Fatalf("public continuity failure leaked or was unbounded: %s", line.Data)
		}
		terminal[line.RequestID] = true
	}
	if !terminal["req-rehydrate-fails"] || !terminal["req-after-rehydrate-failure"] {
		t.Fatalf("continuity quarantine terminal history = %#v", lines)
	}
}

func TestACPRehydrationProviderConflictKeepsSafeTurnProvenance(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.refreshErr = &acp.ProviderTurnError{
		Code: "turn_failed", DetailCode: "conversation_conflict", Message: "SECRET refresh provider detail", Retryable: true,
	}
	harness.acp.resumeErr = &acp.ProviderTurnError{
		Code: "turn_failed", DetailCode: "conversation_conflict", Message: "SECRET resume provider detail", Retryable: true,
	}
	harness.acp.mu.Unlock()

	accepted, err := harness.client.Send(context.Background(), Command{
		Type: "prompt", RequestID: "req-rehydrate-conflict", Message: "do not dispatch through a provider conflict",
	})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("provider-conflict receipt = %#v, %v", accepted, err)
	}
	waitFor(t, 2*time.Second, func() bool {
		lines, readErr := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
		if readErr != nil {
			return false
		}
		for _, line := range lines {
			if line.Type != "turn_error" || line.RequestID != "req-rehydrate-conflict" {
				continue
			}
			var failure struct {
				Source    string `json:"source"`
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
				Message   string `json:"message"`
			}
			return json.Unmarshal(line.Data, &failure) == nil && failure.Source == "provider" && failure.Code == "conflict" &&
				failure.Retryable && strings.Contains(failure.Message, "conversation conflict") && !bytes.Contains(line.Data, []byte("SECRET"))
		}
		return false
	})
	select {
	case requestID := <-harness.acp.started:
		t.Fatalf("provider-conflicted rehydration dispatched Prompt: %s", requestID)
	default:
	}
	waitFor(t, 2*time.Second, func() bool {
		current, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && current.Status == string(session.StatusWaiting) && current.ActiveRequestID == ""
	})
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || status.Status != string(session.StatusWaiting) || status.ActiveRequestID != "" {
		t.Fatalf("status after provider-conflicted rehydration = %#v, %v", status, err)
	}
}

func TestGenerationScopedReservationRecoveryThenNativeExitContinuesACP(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-reservation-recovery"
	reserved, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleReservation,
	})
	if err != nil || reserved.State != "reserved" || reserved.NativeGeneration == "" {
		t.Fatalf("native reservation = %#v, %v", reserved, err)
	}
	marker, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil || !strings.Contains(string(marker), `"state": "reserved"`) || !strings.Contains(string(marker), reserved.NativeGeneration) {
		t.Fatalf("reservation marker = %q, %v", marker, err)
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "native_reservation_status", OwnerToken: ownerToken})
	if err != nil || status.State != "reserved" || status.NativeGeneration != reserved.NativeGeneration {
		t.Fatalf("reservation status = %#v, %v", status, err)
	}
	recovered, err := harness.client.Send(context.Background(), Command{
		Type: "native_recover_reservation", OwnerToken: ownerToken, NativeGeneration: reserved.NativeGeneration,
	})
	if err != nil || recovered.State != "released" {
		t.Fatalf("reservation recovery = %#v, %v", recovered, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleProvider, NativeGeneration: reserved.NativeGeneration,
	}); err == nil || !strings.Contains(err.Error(), "reservation is no longer active") {
		t.Fatalf("delayed recovered-generation activation error = %v", err)
	}

	second, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleReservation,
	})
	if err != nil || second.State != "reserved" || second.NativeGeneration == "" || second.NativeGeneration == reserved.NativeGeneration {
		t.Fatalf("second reservation = %#v, %v", second, err)
	}
	for _, stale := range []Command{
		{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleProvider, NativeGeneration: reserved.NativeGeneration},
		{Type: "native_release", OwnerToken: ownerToken, NativeGeneration: reserved.NativeGeneration, NativeActivity: true},
	} {
		if _, err := harness.client.Send(context.Background(), stale); err == nil || !strings.Contains(err.Error(), "generation") {
			t.Fatalf("stale generation command %#v error = %v", stale, err)
		}
	}
	activated, err := harness.client.Send(context.Background(), Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleProvider, NativeGeneration: second.NativeGeneration,
	})
	if err != nil || activated.State != "activated" || activated.NativeGeneration != second.NativeGeneration {
		t.Fatalf("reservation activation = %#v, %v", activated, err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_recover_reservation", OwnerToken: ownerToken, NativeGeneration: second.NativeGeneration,
	}); err == nil || !strings.Contains(err.Error(), "active provider-native ownership") {
		t.Fatalf("active owner recovery error = %v", err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-during-active-reservation-owner", Message: "do not cross surfaces"}); err == nil || !strings.Contains(err.Error(), "native harness is running") {
		t.Fatalf("prompt during active native owner error = %v", err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_release", OwnerToken: ownerToken, NativeGeneration: second.NativeGeneration, NativeActivity: true,
	}); err != nil {
		t.Fatal(err)
	}
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-reservation-recovery", Message: "continue"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt after native release = %#v, %v", accepted, err)
	}
	waitStarted(t, harness.acp.started, "req-after-reservation-recovery")
	harness.acp.Complete("req-after-reservation-recovery")
}

func TestNativeActivationPublishFailureRestoresRecoverableReservationWithoutProviderStart(t *testing.T) {
	runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	history, err := sessionlog.Open(runtime.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	runtime.log = history
	runtime.hub = newHub(history.LastSequence())
	t.Cleanup(func() {
		if runtime.log != nil {
			_ = runtime.log.Close()
		}
	})

	const ownerToken = "owner-activation-publish-failure"
	reserved, generation, err := runtime.acquireNative(ownerToken, NativeOwnerRoleReservation, "")
	if err != nil || reserved != "reserved" || generation == "" {
		t.Fatalf("native reservation = %q/%q, %v", reserved, generation, err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}

	fixture, err := filepath.Abs(filepath.Join("..", "native", "testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "provider-must-not-start")
	runner := &native.ExecRunner{}
	runner.SetReservationGeneration(generation)
	runner.SetReservationActivator(func(context.Context) error {
		_, _, activateErr := runtime.acquireNative(ownerToken, NativeOwnerRoleProvider, generation)
		return activateErr
	})
	err = runner.Run(context.Background(), native.Invocation{
		Command: fixture, WorkingDirectory: t.TempDir(),
		Environment: map[string]string{"FAKE_NATIVE_RECORD": record},
	}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "restored the exact pre-launch reservation") || runner.NativeActivityOccurred() {
		t.Fatalf("activation publish failure = %v, activity=%t", err, runner.NativeActivityOccurred())
	}
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatalf("provider started after failed activation publish: %v", statErr)
	}

	body, err := os.ReadFile(runtime.NativeOwnerPath())
	if err != nil {
		t.Fatal(err)
	}
	var marker nativeOwnerMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.State != nativeMarkerReserved || marker.Generation != generation || marker.OwnerToken != ownerToken {
		t.Fatalf("rolled-back native marker = %#v", marker)
	}
	state, currentGeneration, err := runtime.nativeReservationStatus(ownerToken)
	if err != nil || state != "reserved" || currentGeneration != generation {
		t.Fatalf("rolled-back reservation status = %q/%q, %v", state, currentGeneration, err)
	}

	reopened, err := sessionlog.Open(runtime.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	runtime.log = reopened
	recovered, err := runtime.recoverNativeReservation(ownerToken, generation)
	if err != nil || recovered != "released" {
		t.Fatalf("rolled-back reservation recovery = %q, %v", recovered, err)
	}
}

func TestNativeAcquirePublishFailureRestoresRefreshPendingAcrossRestart(t *testing.T) {
	runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	const (
		priorOwner      = "released-native-owner"
		priorGeneration = "0123456789abcdef0123456789abcdef"
	)
	if err := runtime.persistNativeOwnership(nativeMarkerRefreshPending, priorOwner, priorGeneration); err != nil {
		t.Fatal(err)
	}
	runtime.nativeReconcilePending = true
	history, err := sessionlog.Open(runtime.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	runtime.log = history
	runtime.hub = newHub(history.LastSequence())
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}

	if state, generation, err := runtime.acquireNative("new-native-owner", NativeOwnerRoleDirect, ""); err == nil || state != "" || generation != "" {
		t.Fatalf("native acquire with failed journal = %q/%q, %v", state, generation, err)
	}
	body, err := os.ReadFile(runtime.NativeOwnerPath())
	if err != nil {
		t.Fatal(err)
	}
	var marker nativeOwnerMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.State != nativeMarkerRefreshPending || marker.OwnerToken != priorOwner || marker.Generation != priorGeneration {
		t.Fatalf("restored refresh marker = %#v", marker)
	}

	restarted, err := New(runtime.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreNativeOwnership(); err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	owner, pending := restarted.nativeOwnerToken, restarted.nativeReconcilePending
	restarted.mu.Unlock()
	if owner != "" || !pending {
		t.Fatalf("restart restored owner=%q pending=%t, want no provider authorization and durable freshness", owner, pending)
	}
}

func TestProviderTranscriptReconcileFailureDoesNotBlockACP(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-reconcile-retry"
	harness.reconcile.mu.Lock()
	harness.reconcile.err = errors.New("provider transcript is temporarily unreadable")
	harness.reconcile.mu.Unlock()
	acquired, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect})
	if err != nil {
		t.Fatal(err)
	}
	released, err := harness.client.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeGeneration: acquired.NativeGeneration, NativeActivity: true})
	if err != nil || released.State != "released" {
		t.Fatalf("native release = %#v, %v", released, err)
	}
	accepted, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-during-reconcile-failure", Message: "continue"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("ACP prompt after native release = %#v, %v", accepted, err)
	}
	waitStarted(t, harness.acp.started, "req-during-reconcile-failure")
	harness.acp.Complete("req-during-reconcile-failure")
	waitFor(t, 2*time.Second, func() bool {
		lines, readErr := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
		if readErr != nil {
			return false
		}
		for _, line := range lines {
			if line.Type == "provider_transcript_reconcile_warning" {
				return true
			}
		}
		return false
	})
	if _, err := os.Stat(harness.runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native owner marker remains after reconcile warning: %v", err)
	}
	status, err := harness.client.Send(context.Background(), Command{Type: "status"})
	if err != nil || status.Status == string(session.StatusStopped) {
		t.Fatalf("worker status after reconcile warning = %#v, %v", status, err)
	}
}

func TestWorkerShutdownCancelsBlockedProviderTranscriptReconcile(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.reconcile.mu.Lock()
	harness.reconcile.wait = make(chan struct{})
	harness.reconcile.mu.Unlock()
	acquired, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "owner-blocked-reconcile", NativeOwnerRole: NativeOwnerRoleDirect})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.client.Send(context.Background(), Command{
		Type: "native_release", OwnerToken: "owner-blocked-reconcile", NativeGeneration: acquired.NativeGeneration, NativeActivity: true,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-harness.reconcile.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider transcript reconciliation did not start")
	}

	harness.cancel()
	waitFor(t, 3*time.Second, func() bool {
		current, loadErr := harness.repository.GetSession(context.Background(), harness.runtime.config.Session.ID)
		return loadErr == nil && current.WorkerPID == 0 && current.WorkerSocket == ""
	})
}

func TestNativeOwnershipSurvivesWorkerRestartUntilOwnerRelease(t *testing.T) {
	harness := startWorkerHarness(t)
	const ownerToken = "owner-survives-restart"
	acquired, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-before-worker-restart", Message: "continue before restart"}); err == nil || !strings.Contains(err.Error(), "native harness is running") {
		t.Fatalf("pre-restart native-open prompt error = %v", err)
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
	if err != nil || !status.NativeOwned || status.ActiveRequestID != "" {
		t.Fatalf("status after worker restart = %#v, %v", status, err)
	}
	harness.acp.mu.Lock()
	resumeCountWhileNative := harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if resumeCountWhileNative != 0 {
		t.Fatalf("worker restart ACP resumes = %d, want 0 while native remains open", resumeCountWhileNative)
	}
	if _, err := restartedClient.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-worker-restart", Message: "continue"}); err == nil || !strings.Contains(err.Error(), "native harness is running") {
		t.Fatalf("restart native-open prompt error = %v", err)
	}
	if reused, err := restartedClient.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: NativeOwnerRoleDirect}); err != nil || reused.State != "already_acquired" {
		t.Fatalf("restart-safe exclusive native reuse = %#v, %v", reused, err)
	}
	if _, err := restartedClient.Send(context.Background(), Command{Type: "native_release", OwnerToken: ownerToken, NativeGeneration: acquired.NativeGeneration, NativeActivity: true}); err != nil {
		t.Fatal(err)
	}
	releaseMarker, err := os.ReadFile(restarted.NativeOwnerPath())
	if err != nil || !bytes.Contains(releaseMarker, []byte(`"state": "refresh_pending"`)) {
		t.Fatalf("restart-safe release freshness marker = %q, %v", releaseMarker, err)
	}
	afterRelease, err := restartedClient.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-native-release", Message: "continue after release"})
	if err != nil || afterRelease.State != "ready" {
		t.Fatalf("post-release prompt = %#v, %v", afterRelease, err)
	}
	waitStarted(t, harness.acp.started, "req-after-native-release")
	if _, err := os.Stat(restarted.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native release freshness marker remains after exact ACP rehydration: %v", err)
	}
	harness.acp.Complete("req-after-native-release")
	harness.acp.mu.Lock()
	refreshCount, resumeCountAfterRelease := harness.acp.refreshCount, harness.acp.resumeCount
	harness.acp.mu.Unlock()
	if refreshCount != 1 || resumeCountAfterRelease != 0 {
		t.Fatalf("provider calls after restart release: refresh=%d resume=%d, want 1/0", refreshCount, resumeCountAfterRelease)
	}
	if _, err := os.Stat(restarted.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("native owner marker remains after restart refresh: %v", err)
	}
}

func TestV3RestoresLegacyRefreshPendingUntilExactPromptRehydration(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	fake.mu.Lock()
	fake.resumeNativeID = "native-canonical"
	fake.refreshNativeID = "native-canonical"
	fake.inspectACP = "acp-canonical"
	fake.inspectNative = "native-canonical"
	fake.mu.Unlock()
	if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runtime.persistNativeOwnership(nativeMarkerRefreshPending, "legacy-v2-owner", "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	reconcile := newFakeTranscriptReconciler()
	reconcileRelease := make(chan struct{})
	reconcile.wait = reconcileRelease
	runtime.config.ReconcileProviderTranscript = reconcile.Reconcile
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	client := NewSocketClient(runtime.SocketPath())
	waitFor(t, 3*time.Second, func() bool { return client.Ping(context.Background()) == nil })
	if marker, err := os.ReadFile(runtime.NativeOwnerPath()); err != nil || !bytes.Contains(marker, []byte(`"state": "refresh_pending"`)) {
		t.Fatalf("legacy refresh_pending marker was not preserved before prompt: %q, %v", marker, err)
	}
	accepted, err := client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-legacy-refresh", Message: "continue immediately"})
	if err != nil || accepted.State != "ready" {
		t.Fatalf("prompt during legacy transcript reload = %#v, %v", accepted, err)
	}
	waitStarted(t, fake.started, "req-legacy-refresh")
	if _, err := os.Stat(runtime.NativeOwnerPath()); !os.IsNotExist(err) {
		t.Fatalf("legacy refresh_pending marker remains after exact rehydration: %v", err)
	}
	select {
	case <-reconcile.started:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy transcript reconciliation did not start after exact rehydration")
	}
	fake.Complete("req-legacy-refresh")
	close(reconcileRelease)
	waitFor(t, 2*time.Second, func() bool {
		lines, readErr := sessionlog.ReadFile(runtime.LogPath(), 0)
		if readErr != nil {
			return false
		}
		for _, line := range lines {
			if line.Type == "provider_transcript_reconciled" {
				return true
			}
		}
		return false
	})
	status, err := client.Send(context.Background(), Command{Type: "status"})
	if err != nil || status.Status == string(session.StatusStopped) {
		t.Fatalf("worker after legacy reconciliation = %#v, %v", status, err)
	}
	fake.mu.Lock()
	resumeCount := fake.resumeCount
	fake.mu.Unlock()
	if resumeCount != 1 {
		t.Fatalf("legacy marker ACP resume count = %d, want 1", resumeCount)
	}
}

func TestStartupRefreshMarkerReplacementBlocksBothSurfaces(t *testing.T) {
	runtime, fake := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
	if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if err := runtime.persistNativeOwnership(nativeMarkerRefreshPending, "release-owner", generation); err != nil {
		t.Fatal(err)
	}
	hookErr := make(chan error, 1)
	fake.mu.Lock()
	fake.resumeNativeID = "native-canonical"
	fake.refreshNativeID = "native-canonical"
	fake.inspectACP = "acp-canonical"
	fake.inspectNative = "native-canonical"
	fake.resumeHook = func() {
		hookErr <- runtime.persistNativeOwnership(nativeMarkerOwned, "unexpected-owner", generation)
	}
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run() error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	client := NewSocketClient(runtime.SocketPath())
	waitFor(t, 3*time.Second, func() bool { return client.Ping(context.Background()) == nil })
	if err := <-hookErr; err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(runtime.NativeOwnerPath())
	if err != nil || !bytes.Contains(preserved, []byte(`"state": "owned"`)) {
		t.Fatalf("replacement marker = %q, %v", preserved, err)
	}
	if _, err := client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "new-owner", NativeOwnerRole: NativeOwnerRoleDirect}); err == nil || !strings.Contains(err.Error(), "unexpectedly changed") {
		t.Fatalf("native acquire with replaced marker error = %v", err)
	}
	if _, err := client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-marker-replacement", Message: "must not run"}); err == nil || !strings.Contains(err.Error(), "prompt was not accepted") {
		t.Fatalf("ACP prompt with replaced marker error = %v", err)
	}
	afterPrompt, err := os.ReadFile(runtime.NativeOwnerPath())
	if err != nil || !bytes.Equal(afterPrompt, preserved) {
		t.Fatalf("blocked surfaces changed preserved replacement marker: before=%q after=%q err=%v", preserved, afterPrompt, err)
	}
	assertNoRequestLog(t, runtime.LogPath(), "req-after-marker-replacement")
	fake.mu.Lock()
	refreshCount := fake.refreshCount
	fake.mu.Unlock()
	if refreshCount != 0 {
		t.Fatalf("ACP refresh calls after fail-closed marker replacement = %d, want 0", refreshCount)
	}
}

func TestWorkerRestorePreservesReservationPhaseAndLegacyOwnedIsActive(t *testing.T) {
	for _, test := range []struct {
		name       string
		marker     string
		generation string
		reserved   bool
	}{
		{name: "generation reservation", marker: nativeMarkerReserved, generation: "0123456789abcdef0123456789abcdef", reserved: true},
		{name: "owned marker", marker: nativeMarkerOwned, generation: "abcdef0123456789abcdef0123456789"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
			if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			const ownerToken = "owner-restored-phase"
			if err := runtime.persistNativeOwnership(test.marker, ownerToken, test.generation); err != nil {
				t.Fatal(err)
			}
			restored, err := New(runtime.config)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.restoreNativeOwnership(); err != nil {
				t.Fatal(err)
			}
			restored.mu.Lock()
			owner, generation, reserved := restored.nativeOwnerToken, restored.nativeOwnerGeneration, restored.nativeOwnerReserved
			restored.mu.Unlock()
			if owner != ownerToken || generation != test.generation || reserved != test.reserved {
				t.Fatalf("restored owner=%q generation=%q reserved=%t", owner, generation, reserved)
			}
			if !test.reserved {
				if _, err := restored.recoverNativeReservation(ownerToken, test.generation); err == nil || !strings.Contains(err.Error(), "active provider-native ownership") {
					t.Fatalf("legacy active recovery error = %v", err)
				}
			}
		})
	}
}

func TestOwnedNativeMarkerMigrationDistinguishesExclusiveAndExplicitStale(t *testing.T) {
	for _, test := range []struct {
		name       string
		staleField string
		wantStale  bool
	}{
		{name: "committed v2 marker without stale bit is reusable"},
		{name: "explicit WIP stale marker is exit only", staleField: `,"stale_after_acp":true`, wantStale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
			if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			const (
				owner      = "legacy-owner"
				generation = "0123456789abcdef0123456789abcdef"
			)
			body := fmt.Sprintf(`{"version":1,"session_id":"S-identity","state":"owned","owner_token":"legacy-owner","generation":"%s"%s}`, generation, test.staleField)
			if err := os.WriteFile(runtime.NativeOwnerPath(), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runtime.restoreNativeOwnership(); err != nil {
				t.Fatal(err)
			}
			state, restoredGeneration, err := runtime.acquireNative(owner, NativeOwnerRoleDirect, "")
			if test.wantStale {
				if err == nil || !strings.Contains(err.Error(), "exit without sending another prompt") {
					t.Fatalf("explicit stale reuse = %q/%q, %v", state, restoredGeneration, err)
				}
			} else if err != nil || state != "already_acquired" || restoredGeneration != generation {
				t.Fatalf("exclusive v2 reuse = %q/%q, %v", state, restoredGeneration, err)
			}
			history, err := sessionlog.Open(runtime.LogPath())
			if err != nil {
				t.Fatal(err)
			}
			runtime.log = history
			runtime.hub = newHub(history.LastSequence())
			t.Cleanup(func() { _ = history.Close() })
			released, err := runtime.releaseNative(owner, generation, false)
			if err != nil || released != "released" {
				t.Fatalf("exact marker release = %q, %v", released, err)
			}
			if _, err := os.Stat(runtime.NativeOwnerPath()); !os.IsNotExist(err) {
				t.Fatalf("marker remains after exact release: %v", err)
			}
		})
	}
}

func TestInvalidNativeOwnershipMarkerFailsClosedBeforeACPConnect(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: "not-json\n"},
		{name: "unknown phase", body: `{"version":1,"session_id":"S-identity","state":"future","owner_token":"owner"}`},
		{name: "owned without generation", body: `{"version":1,"session_id":"S-identity","state":"owned","owner_token":"owner"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, fake := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
			if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(runtime.NativeOwnerPath(), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := runtime.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "refusing ACP and native work") {
				t.Fatalf("Worker.Run() invalid marker error = %v", err)
			}
			preserved, err := os.ReadFile(runtime.NativeOwnerPath())
			if err != nil || string(preserved) != test.body {
				t.Fatalf("invalid native marker was changed: %q, %v", preserved, err)
			}
			fake.mu.Lock()
			startCount, resumeCount, refreshCount := fake.startCount, fake.resumeCount, fake.refreshCount
			fake.mu.Unlock()
			if startCount != 0 || resumeCount != 0 || refreshCount != 0 {
				t.Fatalf("ACP calls with invalid native marker: start=%d resume=%d refresh=%d, want 0/0/0", startCount, resumeCount, refreshCount)
			}
		})
	}
}

func TestACPRehydrationCleanupPreservesUntrustedMarker(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: "not-json\n"},
		{name: "wrong Session", body: `{"version":1,"session_id":"S-other","state":"refresh_pending","owner_token":"owner","generation":"0123456789abcdef0123456789abcdef"}`},
		{name: "future version", body: `{"version":99,"session_id":"S-identity","state":"refresh_pending","owner_token":"owner","generation":"0123456789abcdef0123456789abcdef"}`},
		{name: "unexpected owned", body: `{"version":1,"session_id":"S-identity","state":"owned","owner_token":"owner","generation":"0123456789abcdef0123456789abcdef","stale_after_acp":false}`},
		{name: "unexpected reserved", body: `{"version":1,"session_id":"S-identity","state":"reserved","owner_token":"owner","generation":"0123456789abcdef0123456789abcdef","stale_after_acp":false}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := disconnectedIdentityWorker(t, "acp-canonical", "native-canonical")
			if err := os.MkdirAll(runtime.config.WorkersDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(runtime.NativeOwnerPath(), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			runtime.nativeReconcilePending = true
			err := runtime.clearNativeRefreshPendingLocked()
			stillPending := runtime.nativeReconcilePending
			runtime.mu.Unlock()
			if err == nil || !stillPending {
				t.Fatalf("cleanup error=%v pending=%t", err, stillPending)
			}
			preserved, readErr := os.ReadFile(runtime.NativeOwnerPath())
			if readErr != nil || string(preserved) != test.body {
				t.Fatalf("untrusted marker changed: %q, %v", preserved, readErr)
			}
		})
	}
}

func TestRuntimeRefreshMarkerReplacementBlocksBothSurfaces(t *testing.T) {
	harness := startWorkerHarness(t)
	const generation = "0123456789abcdef0123456789abcdef"
	harness.runtime.mu.Lock()
	if err := harness.runtime.persistNativeOwnership(nativeMarkerOwned, "unexpected-runtime-owner", generation); err != nil {
		harness.runtime.mu.Unlock()
		t.Fatal(err)
	}
	harness.runtime.nativeReconcilePending = true
	harness.runtime.requireACPFreshnessLocked()
	harness.runtime.mu.Unlock()
	preserved, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.runtime.prepareACPDispatch(context.Background(), "req-replaced-at-runtime"); err == nil || !strings.Contains(err.Error(), "unexpectedly changed") {
		t.Fatalf("runtime replacement preparation error = %v", err)
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "native_acquire", OwnerToken: "new-runtime-owner", NativeOwnerRole: NativeOwnerRoleDirect}); err == nil || !strings.Contains(err.Error(), "both ACP and native work are unavailable") {
		t.Fatalf("native acquire after runtime replacement error = %v", err)
	}
	afterFailure, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil || !bytes.Equal(afterFailure, preserved) {
		t.Fatalf("runtime replacement marker changed: before=%q after=%q err=%v", preserved, afterFailure, err)
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if line.Type == "turn_started" && line.RequestID == "req-replaced-at-runtime" {
			t.Fatal("runtime marker replacement reached ACP dispatch")
		}
	}
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-runtime-replacement", Message: "must not run"}); err == nil || !strings.Contains(err.Error(), "prompt was not accepted") {
		t.Fatalf("ACP retry with blocked native marker error = %v", err)
	}
	assertNoRequestLog(t, harness.runtime.LogPath(), "req-after-runtime-replacement")
	afterPrompt, err := os.ReadFile(harness.runtime.NativeOwnerPath())
	if err != nil || !bytes.Equal(afterPrompt, preserved) {
		t.Fatalf("blocked surfaces changed marker: before=%q after=%q err=%v", preserved, afterPrompt, err)
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

func TestProviderTurnFailureKeepsWorkerAndACPQueueUsable(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.promptErr = &acp.ProviderTurnError{Code: "turn_failed", DetailCode: "provider_busy", Message: "SECRET provider detail", Retryable: true}
	harness.acp.mu.Unlock()
	if _, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-provider-busy", Message: "first"}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, harness.acp.started, "req-provider-busy")
	queued, err := harness.client.Send(context.Background(), Command{Type: "prompt", RequestID: "req-after-provider-busy", Message: "second"})
	if err != nil || queued.State != "queued" {
		t.Fatalf("queued prompt = %#v, %v", queued, err)
	}
	harness.acp.Complete("req-provider-busy")
	waitStarted(t, harness.acp.started, "req-after-provider-busy")
	harness.acp.mu.Lock()
	harness.acp.promptErr = nil
	harness.acp.mu.Unlock()
	harness.acp.Complete("req-after-provider-busy")
	waitFor(t, 2*time.Second, func() bool {
		status, statusErr := harness.client.Send(context.Background(), Command{Type: "status"})
		return statusErr == nil && status.Status == string(session.StatusWaiting) && status.ActiveRequestID == "" && status.Queued == 0
	})
	harness.acp.mu.Lock()
	resumeCount, refreshCount := harness.acp.resumeCount, harness.acp.refreshCount
	harness.acp.mu.Unlock()
	if resumeCount != 0 || refreshCount != 2 {
		t.Fatalf("provider turn rejection recovery/rehydration: resume=%d refresh=%d, want 0/2", resumeCount, refreshCount)
	}
	lines, err := sessionlog.ReadFile(harness.runtime.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var failed, completed, safeProvenance bool
	for _, line := range lines {
		if line.Type == "turn_error" && line.RequestID == "req-provider-busy" {
			failed = true
			var failure struct {
				Source    string `json:"source"`
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
				Message   string `json:"message"`
			}
			if json.Unmarshal(line.Data, &failure) == nil {
				safeProvenance = failure.Source == "provider" && failure.Code == "busy" && failure.Retryable && strings.Contains(failure.Message, "provider is busy") && !bytes.Contains(line.Data, []byte("SECRET"))
			}
		}
		completed = completed || (line.Type == "turn_completed" && line.RequestID == "req-after-provider-busy")
	}
	if !failed || !completed || !safeProvenance {
		t.Fatalf("provider failure FIFO history failed=%t completed=%t safe=%t: %#v", failed, completed, safeProvenance, lines)
	}
}

func TestPromptFailureRecoversOrQuarantinesBeforeQueuedPrompt(t *testing.T) {
	harness := startWorkerHarness(t)
	harness.acp.mu.Lock()
	harness.acp.promptErr = &acp.ProtocolError{Operation: "runtime bridge", Message: "bridge disconnected"}
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
	root, err := os.MkdirTemp("/tmp", "atc-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
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

func assertNoRequestLog(t *testing.T, path, requestID string) {
	t.Helper()
	lines, err := sessionlog.ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if line.RequestID == requestID {
			t.Fatalf("rejected request %s was journaled as %s", requestID, line.Type)
		}
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
