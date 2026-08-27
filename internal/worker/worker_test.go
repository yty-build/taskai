package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type workerTestRepository struct {
	mu              sync.Mutex
	current         session.Session
	beforeWorkerCAS func(session.WorkerState, session.WorkerState)
}

func (r *workerTestRepository) GetSession(_ context.Context, id string) (session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, fmt.Errorf("unknown Session %q", id)
	}
	return cloneWorkerTestSession(r.current), nil
}

func (r *workerTestRepository) UpdateSessionStatus(_ context.Context, id string, status session.Status) (session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, fmt.Errorf("unknown Session %q", id)
	}
	r.current.Status = status
	return cloneWorkerTestSession(r.current), nil
}

func (r *workerTestRepository) UpdateSessionProviderRef(_ context.Context, id, runtimeRef, providerRef string) (session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, fmt.Errorf("unknown Session %q", id)
	}
	if runtimeRef != "" {
		r.current.RuntimeRef = runtimeRef
	}
	if providerRef != "" {
		r.current.ProviderRef = providerRef
	}
	return cloneWorkerTestSession(r.current), nil
}

func (r *workerTestRepository) UpdateSessionWorker(_ context.Context, id string, pid int, socket, logPath string, status session.Status) (session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, fmt.Errorf("unknown Session %q", id)
	}
	r.current.WorkerPID = pid
	r.current.WorkerSocket = socket
	r.current.LogPath = logPath
	r.current.Status = status
	return cloneWorkerTestSession(r.current), nil
}

func (r *workerTestRepository) ClearSessionWorker(_ context.Context, id string, status session.Status) (session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, fmt.Errorf("unknown Session %q", id)
	}
	r.current.WorkerPID = 0
	r.current.WorkerSocket = ""
	r.current.Status = status
	return cloneWorkerTestSession(r.current), nil
}

func (r *workerTestRepository) CompareAndSwapSessionWorker(_ context.Context, id string, before, after session.WorkerState, logPath string) (session.Session, bool, error) {
	r.mu.Lock()
	if id != r.current.ID {
		r.mu.Unlock()
		return session.Session{}, false, fmt.Errorf("unknown Session %q", id)
	}
	hook := r.beforeWorkerCAS
	r.beforeWorkerCAS = nil
	r.mu.Unlock()
	if hook != nil {
		hook(before, after)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current.WorkerState() != before {
		return cloneWorkerTestSession(r.current), false, nil
	}
	r.current.Status = after.Status
	r.current.WorkerPID = after.PID
	r.current.WorkerSocket = after.Socket
	if logPath != "" {
		r.current.LogPath = logPath
	}
	return cloneWorkerTestSession(r.current), true, nil
}

func (r *workerTestRepository) CompareAndSwapSessionProviderRef(_ context.Context, id string, owner session.WorkerState, runtimeRef, providerRef string) (session.Session, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.current.ID {
		return session.Session{}, false, fmt.Errorf("unknown Session %q", id)
	}
	if r.current.WorkerState() != owner {
		return cloneWorkerTestSession(r.current), false, nil
	}
	if providerRef != "" && r.current.ProviderRef != "" && r.current.ProviderRef != providerRef {
		return session.Session{}, false, fmt.Errorf("refusing provider reference replacement")
	}
	if runtimeRef != "" {
		r.current.RuntimeRef = runtimeRef
	}
	if providerRef != "" {
		r.current.ProviderRef = providerRef
	}
	return cloneWorkerTestSession(r.current), true, nil
}

func cloneWorkerTestSession(value session.Session) session.Session {
	value.MetadataJSON = append(json.RawMessage(nil), value.MetadataJSON...)
	return value
}

type permissionDecision struct {
	permissionID string
	optionID     string
	cancelled    bool
}

type workerTestPort struct {
	mu sync.Mutex

	info               acp.SessionInfo
	exactInfo          bool
	preflight          func(context.Context) error
	probeCalls         int
	probeGate          <-chan struct{}
	probeEntered       chan<- struct{}
	probeErr           error
	startGate          <-chan struct{}
	startEntered       chan<- struct{}
	startErr           error
	startIgnoresCancel bool
	startupTimeout     time.Duration
	shutdownTimeout    time.Duration
	resumeGate         <-chan struct{}
	resumeEntered      chan<- struct{}
	resumeErr          error
	optionErr          error
	configureErr       error
	effortResetsModel  bool
	permissionErrors   []error
	catalog            acp.ProviderOptionCatalog
	startSpecs         []acp.StartSpec
	resumeSpecs        []acp.StartSpec
	promptSpecs        []acp.PromptSpec
	optionSpecs        []acp.StartSpec
	configureSpecs     []acp.StartSpec
	cancelCalls        int
	cancel             func(context.Context, acp.Target) error
	closeCalls         int
	closeGate          <-chan struct{}
	closeEntered       chan<- struct{}
	closeSignal        chan struct{}
	closeErr           error
	permissionCalls    []permissionDecision
	prompt             func(context.Context, acp.PromptSpec, func(acp.Event) error) (acp.PromptReceipt, error)
	permissionDecided  chan permissionDecision
}

func (p *workerTestPort) Probe(ctx context.Context) error {
	p.mu.Lock()
	p.probeCalls++
	gate, entered, probeErr := p.probeGate, p.probeEntered, p.probeErr
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
	}
	return probeErr
}

func (p *workerTestPort) Start(ctx context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	p.mu.Lock()
	p.startSpecs = append(p.startSpecs, spec)
	gate, entered, startErr, ignoresCancel := p.startGate, p.startEntered, p.startErr, p.startIgnoresCancel
	info := p.sessionInfo(spec)
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		if ignoresCancel {
			<-gate
		} else {
			select {
			case <-ctx.Done():
				return acp.SessionInfo{}, ctx.Err()
			case <-gate:
			}
		}
	}
	return info, startErr
}

func (p *workerTestPort) Resume(ctx context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	p.mu.Lock()
	p.resumeSpecs = append(p.resumeSpecs, spec)
	gate, entered, resumeErr := p.resumeGate, p.resumeEntered, p.resumeErr
	info := p.sessionInfo(spec)
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return acp.SessionInfo{}, ctx.Err()
		case <-gate:
		}
	}
	return info, resumeErr
}

func (p *workerTestPort) sessionInfo(spec acp.StartSpec) acp.SessionInfo {
	info := p.info
	if p.exactInfo {
		return info
	}
	if info.ConsoleSessionID == "" {
		info.ConsoleSessionID = spec.ConsoleSessionID
	}
	if info.RuntimeRef == "" {
		info.RuntimeRef = "record-1"
	}
	if info.ProviderRef == "" {
		info.ProviderRef = "acp-1"
	}
	return info
}

func (p *workerTestPort) Prompt(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
	p.mu.Lock()
	p.promptSpecs = append(p.promptSpecs, spec)
	prompt := p.prompt
	p.mu.Unlock()
	if prompt != nil {
		return prompt(ctx, spec, emit)
	}
	raw := json.RawMessage(`{"type":"text_delta","stream":"output","text":"ok","provider":"fixture"}`)
	if err := emit(acp.Event{Type: "text_delta", Raw: raw}); err != nil {
		return acp.PromptReceipt{RequestID: spec.RequestID}, err
	}
	return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "end_turn"}, nil
}

func (p *workerTestPort) Cancel(ctx context.Context, target acp.Target) error {
	p.mu.Lock()
	p.cancelCalls++
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		return cancel(ctx, target)
	}
	return nil
}

func (p *workerTestPort) ProviderOptions(_ context.Context, spec acp.StartSpec) (acp.ProviderOptionCatalog, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.optionSpecs = append(p.optionSpecs, spec)
	return cloneWorkerTestCatalog(p.catalog), p.optionErr
}

func (p *workerTestPort) Configure(_ context.Context, spec acp.StartSpec) (acp.SessionInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configureSpecs = append(p.configureSpecs, spec)
	if p.configureErr != nil {
		return acp.SessionInfo{}, p.configureErr
	}
	if spec.Model != "" {
		p.catalog.Model.CurrentValue = spec.Model
	}
	if spec.ReasoningEffort != "" {
		p.catalog.ReasoningEffort.CurrentValue = spec.ReasoningEffort
		if p.effortResetsModel {
			p.catalog.Model.CurrentValue = "provider-default"
		}
	}
	return p.sessionInfo(spec), nil
}

func (p *workerTestPort) DecidePermission(_ context.Context, permissionID, optionID string, cancelled bool) error {
	decision := permissionDecision{permissionID: permissionID, optionID: optionID, cancelled: cancelled}
	p.mu.Lock()
	p.permissionCalls = append(p.permissionCalls, decision)
	decided := p.permissionDecided
	var decisionErr error
	if len(p.permissionErrors) > 0 {
		decisionErr = p.permissionErrors[0]
		p.permissionErrors = p.permissionErrors[1:]
	}
	p.mu.Unlock()
	if decisionErr == nil && decided != nil {
		decided <- decision
	}
	return decisionErr
}

func (p *workerTestPort) Close() error {
	p.mu.Lock()
	p.closeCalls++
	gate, entered, signal, closeErr := p.closeGate, p.closeEntered, p.closeSignal, p.closeErr
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if signal != nil {
		close(signal)
	}
	return closeErr
}

func cloneWorkerTestCatalog(value acp.ProviderOptionCatalog) acp.ProviderOptionCatalog {
	value.Model.Choices = append([]acp.ProviderOptionChoice(nil), value.Model.Choices...)
	value.ReasoningEffort.Choices = append([]acp.ProviderOptionChoice(nil), value.ReasoningEffort.Choices...)
	return value
}

func advertisedWorkerTestCatalog() acp.ProviderOptionCatalog {
	return acp.ProviderOptionCatalog{
		Model: acp.ProviderSelectCatalog{
			Supported: true, CurrentValue: "provider-default",
			Choices: []acp.ProviderOptionChoice{{Value: "provider-default", Label: "Provider default"}, {Value: "model-selected", Label: "Selected model"}},
		},
		ReasoningEffort: acp.ProviderSelectCatalog{
			Supported: true, CurrentValue: "medium",
			Choices: []acp.ProviderOptionChoice{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}},
		},
	}
}

type runningWorkerTest struct {
	worker        *Worker
	client        *SocketClient
	port          *workerTestPort
	repo          *workerTestRepository
	done          chan struct{}
	cancel        context.CancelFunc
	allowRunError bool
	errMu         sync.Mutex
	err           error
}

func startRunningWorkerTest(t *testing.T, current session.Session, port *workerTestPort) *runningWorkerTest {
	return startWorkerTest(t, current, port, true)
}

func startAddressableWorkerTest(t *testing.T, current session.Session, port *workerTestPort) *runningWorkerTest {
	return startWorkerTest(t, current, port, false)
}

func startWorkerTest(t *testing.T, current session.Session, port *workerTestPort, waitForProvider bool) *runningWorkerTest {
	t.Helper()
	// Darwin caps Unix-domain socket paths at roughly 104 bytes. testing.T's
	// descriptive temp path can exceed that before the Session suffix is added.
	var root string
	if current.ID != "" && filepath.Base(current.LogPath) == current.ID+".channel-v1.jsonl" {
		root = filepath.Dir(filepath.Dir(current.LogPath))
	} else {
		var err error
		root, err = os.MkdirTemp("", "acpw-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	if current.ID == "" {
		current.ID = "S-test"
	}
	if current.RepoPath == "" {
		current.RepoPath = root
	}
	if current.Status == "" {
		current.Status = session.StatusStarting
	}
	if current.LogPath == "" {
		current.LogPath = filepath.Join(root, "logs", current.ID+".channel-v1.jsonl")
	}
	repository := &workerTestRepository{current: current}
	runtime, err := New(Config{
		Session:          current,
		Profile:          agents.Profile{Name: "fixture", Provider: "test", Command: "fixture-agent"},
		Repository:       repository,
		Port:             port,
		Preflight:        port.preflight,
		WorkersDirectory: filepath.Join(root, "workers"),
		LogsDirectory:    filepath.Join(root, "logs"),
		PID:              4242,
		StartupTimeout:   port.startupTimeout,
		ShutdownTimeout:  port.shutdownTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	harness := &runningWorkerTest{worker: runtime, client: NewSocketClient(runtime.SocketPath()), port: port, repo: repository, done: make(chan struct{}), cancel: cancel}
	go func() {
		err := runtime.Run(ctx)
		harness.errMu.Lock()
		harness.err = err
		harness.errMu.Unlock()
		close(harness.done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	admitted := false
	for {
		probe, probeCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		var message Message
		var pingErr error
		if admitted {
			message, pingErr = harness.client.Send(probe, Command{Type: "ping", SessionID: harness.worker.config.Session.ID})
		} else {
			message, pingErr = harness.client.Admit(probe, harness.worker.config.Session.ID)
			if pingErr == nil {
				admitted = true
			}
		}
		probeCancel()
		if pingErr == nil {
			if message.SessionID != harness.worker.config.Session.ID || message.ProtocolVersion != ProtocolVersion || !reflect.DeepEqual(message.Capabilities, []string{CapabilitySessionChannel}) {
				t.Fatalf("ping = %#v", message)
			}
			if !waitForProvider || message.State != string(session.StatusStarting) {
				break
			}
		}
		select {
		case <-harness.done:
			t.Fatalf("worker exited during startup: %v", harness.runError())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not become ready: %v", pingErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		stopContext, stopCancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = harness.client.Send(stopContext, Command{Type: "stop"})
		stopCancel()
		harness.cancel()
		select {
		case <-harness.done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
		if err := harness.runError(); err != nil && !harness.allowRunError {
			t.Errorf("worker Run() = %v", err)
		}
	})
	return harness
}

func (h *runningWorkerTest) runError() error {
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *runningWorkerTest) waitForRequestResult(t *testing.T, requestID string) []sessionlog.Line {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		lines, err := sessionlog.ReadFile(h.worker.LogPath(), 0)
		if err != nil {
			t.Fatal(err)
		}
		var requestLines []sessionlog.Line
		for _, line := range lines {
			if line.RequestID == requestID {
				requestLines = append(requestLines, line)
			}
		}
		if len(requestLines) > 0 && requestLines[len(requestLines)-1].Type == FrameProviderResult {
			return requestLines
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %s did not reach provider_result; lines=%#v", requestID, requestLines)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *runningWorkerTest) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		message, err := h.client.Send(ctx, Command{Type: "status"})
		cancel()
		if err == nil && message.ActiveRequestID == "" && message.Status == string(session.StatusWaiting) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not become idle: %#v, %v", message, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requestLineTypes(lines []sessionlog.Line) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		result = append(result, line.Type)
	}
	return result
}

func TestWorkerStartsOnceAndStreamsOrderedSessionChannelFrames(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-start"}, port)
	for _, request := range []struct{ id, text string }{{"REQ-one", "first"}, {"REQ-two", "second"}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		response, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: request.id, Message: request.text})
		cancel()
		if err != nil || response.Type != "accepted" {
			t.Fatalf("prompt %s = %#v, %v", request.id, response, err)
		}
		lines := harness.waitForRequestResult(t, request.id)
		if got, want := requestLineTypes(lines), []string{FrameClientCommand, FrameProviderEvent, FrameProviderResult}; !reflect.DeepEqual(got, want) {
			t.Fatalf("request %s line types = %v, want %v", request.id, got, want)
		}
		if got, want := string(lines[1].Data), `{"type":"text_delta","stream":"output","text":"ok","provider":"fixture"}`; got != want {
			t.Fatalf("provider event was rewritten: %s, want %s", got, want)
		}
		harness.waitIdle(t)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.startSpecs) != 1 || len(port.resumeSpecs) != 0 || len(port.promptSpecs) != 2 {
		t.Fatalf("Port calls: start=%d resume=%d prompt=%d", len(port.startSpecs), len(port.resumeSpecs), len(port.promptSpecs))
	}
	if port.startSpecs[0].Model != "" || port.startSpecs[0].ReasoningEffort != "" {
		t.Fatalf("identity-first Start carried provider controls: %#v", port.startSpecs[0])
	}
	allLines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range allLines {
		if line.Type == FrameConnection && (strings.Contains(string(line.Data), `"pid"`) || strings.Contains(string(line.Data), `"socket"`)) {
			t.Fatalf("public connection frame leaked worker coordinates: %s", line.Data)
		}
	}
}

func TestWorkerChannelIsImmediatelyAttachableWhileProviderStarts(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), startGate: gate, startEntered: entered}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-starting"}, port)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	status, err := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.Status != string(session.StatusStarting) {
		t.Fatalf("starting status = %#v, %v", status, err)
	}

	streamContext, stopStream := context.WithCancel(context.Background())
	streamed := make(chan Message, 2)
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- harness.client.Subscribe(streamContext, 0, func(message Message) error {
			streamed <- message
			return nil
		})
	}()
	select {
	case message := <-streamed:
		if message.LineType != FrameConnection || !strings.Contains(string(message.Data), `"state":"connecting"`) {
			t.Fatalf("first startup frame = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive connecting frame")
	}

	commands := []Command{
		{Type: "prompt", RequestID: "REQ-too-early", Message: "do not send"},
		{Type: "provider_options"},
		{Type: "configure", Model: "model-selected"},
		{Type: "cancel"},
		{Type: "permission_response", PermissionID: "P-missing", OptionID: "allow-once"},
	}
	for _, command := range commands {
		commandContext, commandCancel := context.WithTimeout(context.Background(), time.Second)
		response, commandErr := harness.client.Send(commandContext, command)
		commandCancel()
		var starting *NotReadyError
		if !errors.As(commandErr, &starting) || response.ErrorCode != "session_starting" {
			t.Fatalf("%s during startup = %#v, %T %v", command.Type, response, commandErr, commandErr)
		}
	}
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if line.Type == FrameClientCommand {
			t.Fatalf("startup rejection was journaled as an accepted command: %#v", line)
		}
	}
	port.mu.Lock()
	if len(port.promptSpecs) != 0 || len(port.optionSpecs) != 0 || len(port.configureSpecs) != 0 || port.cancelCalls != 0 || len(port.permissionCalls) != 0 {
		t.Fatalf("Port was called before ready: prompt=%d options=%d config=%d cancel=%d permission=%d", len(port.promptSpecs), len(port.optionSpecs), len(port.configureSpecs), port.cancelCalls, len(port.permissionCalls))
	}
	port.mu.Unlock()

	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		readyContext, readyCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		ready, readyErr := harness.client.Send(readyContext, Command{Type: "status"})
		readyCancel()
		if readyErr == nil && ready.Status == string(session.StatusWaiting) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider Port did not become ready: %#v, %v", ready, readyErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case message := <-streamed:
		if message.LineType != FrameConnection || !strings.Contains(string(message.Data), `"state":"connected"`) {
			t.Fatalf("second startup frame = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive connected frame")
	}
	stopStream()
	<-streamDone
	port.mu.Lock()
	if len(port.startSpecs) != 1 || len(port.resumeSpecs) != 0 {
		t.Fatalf("startup calls: start=%d resume=%d", len(port.startSpecs), len(port.resumeSpecs))
	}
	port.mu.Unlock()
}

func TestWorkerChannelIsImmediatelyAttachableWhileRuntimeProbeBlocks(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), probeGate: gate, probeEntered: entered}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-probe-starting"}, port)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("runtime Probe was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	status, err := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.Status != string(session.StatusStarting) {
		t.Fatalf("status while Probe blocks = %#v, %v", status, err)
	}
	port.mu.Lock()
	if len(port.startSpecs) != 0 || len(port.resumeSpecs) != 0 {
		port.mu.Unlock()
		t.Fatal("provider identity call overtook runtime Probe")
	}
	port.mu.Unlock()
	close(gate)
	harness.waitIdle(t)
}

func TestWorkerBootstrapPreflightFailureIsReplayableBeforeAnyPortCall(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{}
	port.preflight = func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
			return errors.New("private persisted checkout drift detail")
		}
	}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-preflight-fails"}, port)
	harness.allowRunError = true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bootstrap preflight was not entered after admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	status, err := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.Status != string(session.StatusStarting) {
		t.Fatalf("status while bootstrap preflight blocks = %#v, %v", status, err)
	}
	port.mu.Lock()
	if port.probeCalls != 0 || len(port.startSpecs) != 0 || len(port.resumeSpecs) != 0 {
		port.mu.Unlock()
		t.Fatal("bootstrap drift reached the provider Port")
	}
	port.mu.Unlock()

	close(gate)
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after bootstrap preflight failure")
	}
	lines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameWorkerError, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bootstrap preflight frame order = %v, want %v", got, want)
	}
	if strings.Contains(string(lines[1].Data), "private persisted checkout drift detail") {
		t.Fatalf("bootstrap preflight leaked private detail: %s", lines[1].Data)
	}
	current, loadErr := harness.repo.GetSession(context.Background(), "S-preflight-fails")
	if loadErr != nil || current.Status != session.StatusFailed {
		t.Fatalf("bootstrap failure status = %#v, %v", current, loadErr)
	}
}

func TestWorkerDoesNotStartProviderBeforePrivateChannelAdmission(t *testing.T) {
	root, err := os.MkdirTemp("", "acpw-admission-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	current := session.Session{ID: "S-admission", RepoPath: root, Status: session.StatusStarting}
	repository := &workerTestRepository{current: current}
	startGate := make(chan struct{})
	startEntered := make(chan struct{}, 1)
	port := &workerTestPort{startGate: startGate, startEntered: startEntered}
	runtime, err := New(Config{
		Session: current, Profile: agents.Profile{Name: "fixture", Provider: "test", Command: "fixture-agent"},
		Repository: repository, Port: port, WorkersDirectory: filepath.Join(root, "workers"), LogsDirectory: filepath.Join(root, "logs"),
		PID: 4242, StartupTimeout: time.Second, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	for deadline := time.Now().Add(time.Second); ; {
		loaded, loadErr := repository.GetSession(context.Background(), current.ID)
		if loadErr == nil && loaded.WorkerSocket != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Session Channel was not registered: %#v, %v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-startEntered:
		t.Fatal("provider Start overtook the private channel admission")
	case <-time.After(75 * time.Millisecond):
	}
	client := NewSocketClient(runtime.SocketPath())
	pingContext, pingCancel := context.WithTimeout(context.Background(), time.Second)
	pong, err := client.Send(pingContext, Command{Type: "ping", SessionID: current.ID})
	pingCancel()
	if err != nil || pong.Type != "pong" || pong.SessionID != current.ID || pong.State != string(session.StatusStarting) {
		t.Fatalf("observational ping = %#v, %v", pong, err)
	}
	mismatchContext, mismatchCancel := context.WithTimeout(context.Background(), time.Second)
	_, mismatchErr := client.Send(mismatchContext, Command{Type: "ping", SessionID: "S-other"})
	mismatchCancel()
	if mismatchErr == nil || !strings.Contains(mismatchErr.Error(), "target does not match") {
		t.Fatalf("mismatched Session ping error = %v", mismatchErr)
	}
	mismatchContext, mismatchCancel = context.WithTimeout(context.Background(), time.Second)
	_, mismatchErr = client.Admit(mismatchContext, "S-other")
	mismatchCancel()
	if mismatchErr == nil || !strings.Contains(mismatchErr.Error(), "target does not match") {
		t.Fatalf("mismatched Session admission error = %v", mismatchErr)
	}
	select {
	case <-startEntered:
		t.Fatal("observational control traffic released provider startup")
	case <-time.After(75 * time.Millisecond):
	}
	admissionContext, admissionCancel := context.WithTimeout(context.Background(), time.Second)
	admitted, err := client.Admit(admissionContext, current.ID)
	admissionCancel()
	if err != nil || admitted.Type != "admission_ready" || admitted.State != string(session.StatusStarting) {
		t.Fatalf("private admission = %#v, %v", admitted, err)
	}
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		t.Fatal("provider Start did not begin after channel admission")
	}
	close(startGate)
	for deadline := time.Now().Add(time.Second); ; {
		statusContext, statusCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		status, statusErr := client.Send(statusContext, Command{Type: "status"})
		statusCancel()
		if statusErr == nil && status.Status == string(session.StatusWaiting) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider did not reach waiting: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopContext, stopCancel := context.WithTimeout(context.Background(), time.Second)
	_, err = client.Send(stopContext, Command{Type: "stop"})
	stopCancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker stopped with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerProbeFailureIsReplayableWithoutProviderIdentityCall(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{probeGate: gate, probeEntered: entered, probeErr: errors.New("private probe detail")}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-probe-fails"}, port)
	harness.allowRunError = true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("runtime Probe was not entered")
	}
	close(gate)
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after runtime Probe failure")
	}
	port.mu.Lock()
	if len(port.startSpecs) != 0 || len(port.resumeSpecs) != 0 {
		port.mu.Unlock()
		t.Fatal("Probe failure reached provider identity call")
	}
	port.mu.Unlock()
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameWorkerError, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Probe failure frame order = %v, want %v", got, want)
	}
	if strings.Contains(string(lines[1].Data), "private probe detail") {
		t.Fatalf("Probe failure leaked private detail: %s", lines[1].Data)
	}
}

func TestWorkerChannelIsImmediatelyAttachableWhileExactResumeStarts(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{
		info:       acp.SessionInfo{RuntimeRef: "record-saved", ProviderRef: "provider-saved"},
		resumeGate: gate, resumeEntered: entered,
	}
	harness := startAddressableWorkerTest(t, session.Session{
		ID: "S-resume-starting", RuntimeRef: "record-saved", ProviderRef: "provider-saved",
	}, port)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Resume was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	status, err := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.Status != string(session.StatusStarting) {
		t.Fatalf("resume startup status = %#v, %v", status, err)
	}
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		ping, pingErr := harness.client.Send(ctx, Command{Type: "ping", SessionID: harness.worker.config.Session.ID})
		cancel()
		if pingErr == nil && ping.State == string(session.StatusWaiting) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exact Resume did not become ready: %#v, %v", ping, pingErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.startSpecs) != 0 || len(port.resumeSpecs) != 1 || port.resumeSpecs[0].ResumeProviderRef != "provider-saved" {
		t.Fatalf("startup calls: start=%d resume=%#v", len(port.startSpecs), port.resumeSpecs)
	}
}

func TestWorkerStartupFailureRemainsReplayableAndClearsOwnership(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{startGate: gate, startEntered: entered, startErr: errors.New("private provider startup detail")}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-start-fails"}, port)
	harness.allowRunError = true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}
	close(gate)
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after provider startup failure")
	}
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameWorkerError, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("startup failure frame order = %v, want %v", got, want)
	}
	if strings.Contains(string(lines[1].Data), "private provider startup detail") || !strings.Contains(string(lines[1].Data), `"code":"provider_connect_failed"`) {
		t.Fatalf("startup failure frame was unsafe or unclassified: %s", lines[1].Data)
	}
	current, err := harness.repo.GetSession(context.Background(), "S-start-fails")
	if err != nil || current.Status != session.StatusFailed || current.WorkerPID != 0 || current.WorkerSocket != "" {
		t.Fatalf("failed Session ownership = %#v, %v", current, err)
	}
}

func TestWorkerProviderStartupTimeoutFailsThroughAttachedChannel(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{startGate: gate, startEntered: entered, startupTimeout: 75 * time.Millisecond}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-start-timeout"}, port)
	harness.allowRunError = true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}
	select {
	case <-harness.done:
	case <-time.After(time.Second):
		t.Fatal("provider startup timeout did not stop worker")
	}
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameWorkerError, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("startup timeout frame order = %v, want %v", got, want)
	}
	current, err := harness.repo.GetSession(context.Background(), "S-start-timeout")
	if err != nil || current.Status != session.StatusFailed || current.WorkerSocket != "" {
		t.Fatalf("timed-out Session = %#v, %v", current, err)
	}
}

func TestWorkerUsesOneShutdownBudgetForUncooperativeStartupAndClose(t *testing.T) {
	startGate := make(chan struct{})
	closeGate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{
		startGate: startGate, startEntered: entered, startIgnoresCancel: true,
		closeGate: closeGate, startupTimeout: 40 * time.Millisecond, shutdownTimeout: 90 * time.Millisecond,
	}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-one-shutdown-budget"}, port)
	harness.allowRunError = true
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}
	started := time.Now()
	select {
	case <-harness.done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("worker consumed multiple shutdown budgets")
	}
	if elapsed := time.Since(started); elapsed > 220*time.Millisecond {
		t.Fatalf("combined startup teardown took %s; want one 90ms shutdown budget after the 40ms startup bound", elapsed)
	}
	close(startGate)
	close(closeGate)
	current, err := harness.repo.GetSession(context.Background(), "S-one-shutdown-budget")
	if err != nil || current.Status != session.StatusFailed || current.WorkerSocket == "" {
		t.Fatalf("unproven teardown did not retain quarantine ownership: %#v, %v", current, err)
	}
}

func TestWorkerStopDuringProviderStartupCancelsWithoutFailure(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{startGate: gate, startEntered: entered}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-stop-starting"}, port)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil || response.Type != "stopping" {
		t.Fatalf("stop startup = %#v, %v", response, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop during provider startup")
	}
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("startup stop frame order = %v, want %v", got, want)
	}
	if !strings.Contains(string(lines[1].Data), `"state":"stopped"`) {
		t.Fatalf("startup stop terminal = %s", lines[1].Data)
	}
	current, err := harness.repo.GetSession(context.Background(), "S-stop-starting")
	if err != nil || current.Status != session.StatusStopped || current.WorkerPID != 0 || current.WorkerSocket != "" {
		t.Fatalf("stopped Session ownership = %#v, %v", current, err)
	}
}

func TestWorkerClosesProviderPortBeforeClearingOwnership(t *testing.T) {
	closeGate := make(chan struct{})
	closeEntered := make(chan struct{}, 1)
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), closeGate: closeGate, closeEntered: closeEntered}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-close-order"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil || response.Type != "stopping" {
		t.Fatalf("stop = %#v, %v", response, err)
	}
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("provider Port close did not begin")
	}
	current, err := harness.repo.GetSession(context.Background(), "S-close-order")
	if err != nil || current.WorkerSocket == "" || current.WorkerPID == 0 {
		t.Fatalf("worker ownership cleared before provider Close: %#v, %v", current, err)
	}
	select {
	case <-harness.done:
		t.Fatal("worker exited before provider Close completed")
	default:
	}
	close(closeGate)
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish after provider Close")
	}
	current, err = harness.repo.GetSession(context.Background(), "S-close-order")
	if err != nil || current.Status != session.StatusStopped || current.WorkerSocket != "" || current.WorkerPID != 0 {
		t.Fatalf("worker ownership after provider Close = %#v, %v", current, err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.closeCalls != 1 {
		t.Fatalf("provider Port Close calls = %d, want exactly one", port.closeCalls)
	}
}

func TestWorkerClosesPortBeforeWaitingForBlockedProviderCall(t *testing.T) {
	closed := make(chan struct{})
	started := make(chan struct{}, 1)
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), closeSignal: closed}
	port.prompt = func(_ context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		started <- struct{}{}
		<-closed
		return acp.PromptReceipt{RequestID: spec.RequestID}, context.Canceled
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-close-unblocks-call"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-blocked", Message: "wait"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "stop"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop waited on provider call before closing its Port")
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.closeCalls != 1 {
		t.Fatalf("provider Port Close calls = %d, want exactly one", port.closeCalls)
	}
}

func TestWorkerCloseTimeoutPreservesOwnershipForStaleRecovery(t *testing.T) {
	closeGate := make(chan struct{})
	closeEntered := make(chan struct{}, 1)
	port := &workerTestPort{
		catalog: advertisedWorkerTestCatalog(), closeGate: closeGate, closeEntered: closeEntered,
		shutdownTimeout: 75 * time.Millisecond,
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-close-timeout"}, port)
	harness.allowRunError = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("provider Port close did not begin")
	}
	select {
	case <-harness.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not enforce provider Close timeout")
	}
	current, err := harness.repo.GetSession(context.Background(), "S-close-timeout")
	if err != nil || current.Status != session.StatusFailed || current.WorkerSocket == "" || current.WorkerPID == 0 {
		t.Fatalf("unsafe Close cleared worker ownership: %#v, %v", current, err)
	}
	close(closeGate)
}

func TestWorkerShutdownAbandonsPendingPermissionBeforeUnprovenTeardown(t *testing.T) {
	promptRelease := make(chan struct{})
	closeRelease := make(chan struct{})
	permissionEmitted := make(chan struct{})
	promptReturned := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(promptRelease)
			close(closeRelease)
		})
	}
	defer release()

	permissionRaw := json.RawMessage(`{"toolCall":{"toolCallId":"tool-stop","title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]}`)
	port := &workerTestPort{
		catalog: advertisedWorkerTestCatalog(), closeGate: closeRelease,
		shutdownTimeout: 75 * time.Millisecond,
	}
	port.prompt = func(_ context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		defer close(promptReturned)
		if err := emit(acp.Event{Type: "session_request_permission", PermissionID: "bridge-stop", Raw: permissionRaw}); err != nil {
			return acp.PromptReceipt{RequestID: spec.RequestID}, err
		}
		close(permissionEmitted)
		<-promptRelease // Deliberately ignore Prompt cancellation to force quarantine.
		return acp.PromptReceipt{RequestID: spec.RequestID}, context.Canceled
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-stopperm"}, port)
	harness.allowRunError = true

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-stop-permission", Message: "write"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-permissionEmitted:
	case <-time.After(time.Second):
		t.Fatal("provider permission was not emitted")
	}
	permissionID := PermissionConsoleID("REQ-stop-permission", "tool-stop")
	for deadline := time.Now().Add(time.Second); ; {
		statusContext, statusCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		status, statusErr := harness.client.Send(statusContext, Command{Type: "status"})
		statusCancel()
		if statusErr == nil && len(status.Permissions) == 1 && status.Permissions[0].ID == permissionID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending permission was not exposed: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil || response.Type != "stopping" {
		t.Fatalf("stop = %#v, %v", response, err)
	}
	select {
	case <-harness.done:
	case <-time.After(time.Second):
		t.Fatal("uncooperative provider calls did not reach the shutdown bound")
	}

	allLines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var requestLines []sessionlog.Line
	for _, line := range allLines {
		if line.RequestID == "REQ-stop-permission" {
			requestLines = append(requestLines, line)
		}
	}
	if got, want := requestLineTypes(requestLines), []string{
		FrameClientCommand, FrameProviderEvent, FramePermissionRequested, FramePermissionAbandoned,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe shutdown permission frames = %v, want %v", got, want)
	}
	current, loadErr := harness.repo.GetSession(context.Background(), "S-stopperm")
	if loadErr != nil || current.Status != session.StatusFailed || current.WorkerPID == 0 || current.WorkerSocket == "" {
		t.Fatalf("unsafe shutdown did not retain quarantine ownership: %#v, %v", current, loadErr)
	}

	release()
	select {
	case <-promptReturned:
	case <-time.After(time.Second):
		t.Fatal("test Prompt did not return after release")
	}
	select {
	case <-harness.worker.portCloseDone:
	case <-time.After(time.Second):
		t.Fatal("test Port.Close did not return after release")
	}
}

func TestWorkerNeverReplacementStartsAfterIdentitylessPriorAttempt(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	firstPort := &workerTestPort{startGate: gate, startEntered: entered}
	first := startAddressableWorkerTest(t, session.Session{ID: "S-no-replacement"}, firstPort)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first provider Start was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := first.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.done:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker did not stop")
	}
	current, err := first.repo.GetSession(context.Background(), "S-no-replacement")
	if err != nil || current.ProviderRef != "" {
		t.Fatalf("identityless prior attempt = %#v, %v", current, err)
	}

	secondPort := &workerTestPort{}
	second, err := New(Config{
		Session: current, Profile: first.worker.config.Profile, Repository: first.repo, Port: secondPort,
		WorkersDirectory: first.worker.config.WorkersDirectory, LogsDirectory: first.worker.config.LogsDirectory,
		PID: 4243, StartupTimeout: time.Second, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(context.Background()) }()
	secondClient := NewSocketClient(second.SocketPath())
	for deadline := time.Now().Add(time.Second); ; {
		admitContext, admitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, admitErr := secondClient.Admit(admitContext, current.ID)
		admitCancel()
		if admitErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("identityless restart channel was not addressable: %v", admitErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "identity is unavailable") {
			t.Fatalf("identityless restart error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("identityless restart did not fail closed")
	}
	secondPort.mu.Lock()
	defer secondPort.mu.Unlock()
	if len(secondPort.startSpecs) != 0 || len(secondPort.resumeSpecs) != 0 {
		t.Fatalf("identityless restart called provider: start=%d resume=%d", len(secondPort.startSpecs), len(secondPort.resumeSpecs))
	}
}

func TestWorkerPersistsSuccessfulStartIdentityWhenStopWinsStartupRace(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	port := &workerTestPort{
		startGate: gate, startEntered: entered, startIgnoresCancel: true,
		info: acp.SessionInfo{RuntimeRef: "record-raced", ProviderRef: "provider-raced"},
	}
	harness := startAddressableWorkerTest(t, session.Session{ID: "S-stop-start-race"}, port)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider Start was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "stop"})
	cancel()
	if err != nil || response.Type != "stopping" {
		t.Fatalf("stop startup = %#v, %v", response, err)
	}
	close(gate)
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not finish the Start/Stop race")
	}
	current, err := harness.repo.GetSession(context.Background(), "S-stop-start-race")
	if err != nil || current.Status != session.StatusStopped || current.ProviderRef != "provider-raced" || current.RuntimeRef != "record-raced" {
		t.Fatalf("raced provider identity = %#v, %v", current, err)
	}

	resumedPort := &workerTestPort{info: acp.SessionInfo{RuntimeRef: "record-raced-2", ProviderRef: "provider-raced"}}
	resumed := startRunningWorkerTest(t, current, resumedPort)
	resumedPort.mu.Lock()
	defer resumedPort.mu.Unlock()
	if len(resumedPort.startSpecs) != 0 || len(resumedPort.resumeSpecs) != 1 || resumedPort.resumeSpecs[0].ResumeProviderRef != "provider-raced" {
		t.Fatalf("next worker replaced raced identity: start=%d resume=%#v", len(resumedPort.startSpecs), resumedPort.resumeSpecs)
	}
	_ = resumed
}

func TestWorkerResumesSavedACPIdentityOnlyAtStartup(t *testing.T) {
	port := &workerTestPort{
		info:    acp.SessionInfo{RuntimeRef: "record-saved", ProviderRef: "acp-saved"},
		catalog: advertisedWorkerTestCatalog(),
	}
	harness := startRunningWorkerTest(t, session.Session{
		ID: "S-resume", RuntimeRef: "record-saved", ProviderRef: "acp-saved",
	}, port)
	for _, requestID := range []string{"REQ-one", "REQ-two"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: requestID, Message: requestID})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		harness.waitForRequestResult(t, requestID)
		harness.waitIdle(t)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.startSpecs) != 0 || len(port.resumeSpecs) != 1 || len(port.promptSpecs) != 2 {
		t.Fatalf("Port calls: start=%d resume=%d prompt=%d", len(port.startSpecs), len(port.resumeSpecs), len(port.promptSpecs))
	}
	if port.resumeSpecs[0].ResumeProviderRef != "acp-saved" {
		t.Fatalf("Resume() spec = %#v", port.resumeSpecs[0])
	}
}

func TestWorkerRejectsInvalidProviderIdentityBeforeReadiness(t *testing.T) {
	for _, test := range []struct {
		name           string
		savedRef       string
		returnedRef    string
		wantStartCall  int
		wantResumeCall int
	}{
		{name: "Start missing reference", wantStartCall: 1},
		{name: "Start oversized reference", returnedRef: strings.Repeat("p", 513), wantStartCall: 1},
		{name: "Resume missing reference", savedRef: "provider-saved", wantResumeCall: 1},
		{name: "Resume oversized reference", savedRef: "provider-saved", returnedRef: strings.Repeat("p", 513), wantResumeCall: 1},
		{name: "saved reference is oversized", savedRef: strings.Repeat("p", 513)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			current := session.Session{ID: "S-invalid-identity", RepoPath: root, ProviderRef: test.savedRef, Status: session.StatusStarting}
			port := &workerTestPort{info: acp.SessionInfo{RuntimeRef: "record-returned", ProviderRef: test.returnedRef}, exactInfo: true}
			runtime, err := New(Config{
				Session:          current,
				Profile:          agents.Profile{Name: "fixture", Provider: "test", Command: "fixture-agent"},
				Repository:       &workerTestRepository{current: current},
				Port:             port,
				WorkersDirectory: filepath.Join(root, "workers"),
				LogsDirectory:    filepath.Join(root, "logs"),
				PID:              4242,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.openPort(context.Background()); err == nil {
				t.Fatal("openPort accepted invalid provider identity")
			} else {
				var identity *acp.IdentityError
				if !errors.As(err, &identity) {
					t.Fatalf("openPort error = %T %v, want IdentityError", err, err)
				}
			}
			port.mu.Lock()
			startCalls, resumeCalls := len(port.startSpecs), len(port.resumeSpecs)
			port.mu.Unlock()
			if startCalls != test.wantStartCall || resumeCalls != test.wantResumeCall {
				t.Fatalf("Port calls: start=%d resume=%d, want start=%d resume=%d", startCalls, resumeCalls, test.wantStartCall, test.wantResumeCall)
			}
		})
	}
}

func TestWorkerRejectsConcurrentCommandsWithoutJournalingAndDetachDoesNotCancel(t *testing.T) {
	started := make(chan struct{})
	providerCancelled := make(chan struct{})
	promptCancelled := make(chan struct{})
	var startOnce sync.Once
	var providerCancelOnce sync.Once
	var cancelOnce sync.Once
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-providerCancelled:
			return acp.PromptReceipt{RequestID: spec.RequestID, StopReason: "cancelled"}, nil
		case <-ctx.Done():
			cancelOnce.Do(func() { close(promptCancelled) })
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	port.cancel = func(context.Context, acp.Target) error {
		providerCancelOnce.Do(func() { close(providerCancelled) })
		return nil
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-busy"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-active", Message: "hold open"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider prompt did not start")
	}

	subscribeContext, detach := context.WithCancel(context.Background())
	subscribeDone := make(chan error, 1)
	go func() {
		subscribeDone <- harness.client.Subscribe(subscribeContext, 0, func(Message) error { return nil })
	}()
	detach()
	select {
	case <-subscribeDone:
	case <-time.After(time.Second):
		t.Fatal("subscriber did not detach")
	}
	select {
	case <-promptCancelled:
		t.Fatal("detaching a subscriber cancelled the provider turn")
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-rejected", Message: "must not queue"})
	cancel()
	var busy *BusyError
	if !errors.As(err, &busy) || response.ErrorCode != "session_busy" || response.ActiveRequestID != "REQ-active" {
		t.Fatalf("concurrent prompt = %#v, %T %v", response, err, err)
	}
	if strings.Contains(err.Error(), "must not queue") {
		t.Fatalf("busy error leaked rejected prompt: %v", err)
	}
	lines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, line := range lines {
		if line.RequestID == "REQ-rejected" {
			t.Fatalf("busy prompt was journaled: %#v", line)
		}
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	_, err = harness.client.Send(ctx, Command{Type: "configure", Model: "model-selected", ReasoningEffort: "high"})
	cancel()
	if !errors.As(err, &busy) {
		t.Fatalf("configure during turn = %T %v, want BusyError", err, err)
	}
	port.mu.Lock()
	if len(port.optionSpecs) != 0 || len(port.configureSpecs) != 0 {
		port.mu.Unlock()
		t.Fatal("busy configure reached the provider")
	}
	port.mu.Unlock()

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	cancelled, err := harness.client.Send(ctx, Command{Type: "cancel", RequestID: "REQ-active"})
	cancel()
	if err != nil || cancelled.Type != "cancelled" {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	requestLines := harness.waitForRequestResult(t, "REQ-active")
	if got := requestLineTypes(requestLines); !reflect.DeepEqual(got, []string{FrameClientCommand, FrameClientCommand, FrameProviderResult}) {
		t.Fatalf("cancelled request line types = %v", got)
	}
	var terminal map[string]any
	if err := json.Unmarshal(requestLines[len(requestLines)-1].Data, &terminal); err != nil || terminal["status"] != "cancelled" {
		t.Fatalf("cancel terminal = %s, %v", requestLines[len(requestLines)-1].Data, err)
	}
	harness.waitIdle(t)

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	configured, err := harness.client.Send(ctx, Command{Type: "configure", Model: "model-selected", ReasoningEffort: "high"})
	cancel()
	if err != nil || configured.Type != "configured" || configured.ProviderOptions == nil || configured.ProviderOptions.Model.CurrentValue != "model-selected" || configured.ProviderOptions.ReasoningEffort.CurrentValue != "high" {
		t.Fatalf("idle configure = %#v, %v", configured, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	configured, err = harness.client.Send(ctx, Command{Type: "configure", ReasoningEffort: "medium"})
	cancel()
	if err != nil || configured.ProviderOptions == nil || configured.ProviderOptions.Model.CurrentValue != "model-selected" || configured.ProviderOptions.ReasoningEffort.CurrentValue != "medium" {
		t.Fatalf("partial configure = %#v, %v", configured, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	options, err := harness.client.Send(ctx, Command{Type: "provider_options"})
	cancel()
	if err != nil || options.Type != "provider_options" || options.ProviderOptions == nil || options.ProviderOptions.Model.CurrentValue != "model-selected" {
		t.Fatalf("idle provider options = %#v, %v", options, err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.optionSpecs) != 5 || len(port.configureSpecs) != 2 || port.cancelCalls != 1 {
		t.Fatalf("idle control calls: options=%d configure=%d cancel=%d", len(port.optionSpecs), len(port.configureSpecs), port.cancelCalls)
	}
	if port.configureSpecs[0].ResumeProviderRef != "acp-1" {
		t.Fatalf("Configure did not use retained persisted identity: %#v", port.configureSpecs[0])
	}
	if port.configureSpecs[1].Model != "model-selected" || port.configureSpecs[1].ReasoningEffort != "medium" {
		t.Fatalf("partial Configure did not protect the fresh live tuple: %#v", port.configureSpecs[1])
	}
}

func TestWorkerQuietSubscriberEOFReleasesOnlyItsHubSlot(t *testing.T) {
	promptStarted := make(chan struct{})
	promptCancelled := make(chan struct{})
	var startedOnce sync.Once
	var cancelledOnce sync.Once
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		startedOnce.Do(func() { close(promptStarted) })
		<-ctx.Done()
		cancelledOnce.Do(func() { close(promptCancelled) })
		return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-quiet-subscriber-eof"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-quiet-subscriber-eof", Message: "hold"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-promptStarted:
	case <-time.After(time.Second):
		t.Fatal("provider prompt did not start")
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	status, err := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.ActiveRequestID != "REQ-quiet-subscriber-eof" {
		t.Fatalf("active status = %#v, %v", status, err)
	}
	connection, err := net.Dial("unix", harness.worker.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(connection).Encode(Command{Type: "subscribe", From: status.Seq}); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	waitForSubscribers := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			harness.worker.hub.mu.Lock()
			got := len(harness.worker.hub.subscribers)
			harness.worker.hub.mu.Unlock()
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("hub subscriber count = %d, want %d", got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitForSubscribers(1)
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	waitForSubscribers(0)

	select {
	case <-promptCancelled:
		t.Fatal("quiet subscriber EOF cancelled the provider turn")
	case <-time.After(50 * time.Millisecond):
	}
	port.mu.Lock()
	cancelCalls, closeCalls := port.cancelCalls, port.closeCalls
	port.mu.Unlock()
	if cancelCalls != 0 || closeCalls != 0 {
		t.Fatalf("quiet subscriber EOF affected provider Port: cancel=%d close=%d", cancelCalls, closeCalls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	status, err = harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if err != nil || status.ActiveRequestID != "REQ-quiet-subscriber-eof" || status.Status != string(session.StatusWorking) {
		t.Fatalf("status after quiet subscriber EOF = %#v, %v", status, err)
	}
}

func TestWorkerCancelFailsChannelWhenRetainedPortCannotSettle(t *testing.T) {
	started := make(chan struct{})
	providerCancelled := make(chan struct{})
	var startedOnce sync.Once
	var cancelOnce sync.Once
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-providerCancelled:
			return acp.PromptReceipt{RequestID: spec.RequestID}, &acp.ProtocolError{
				Operation: "prompt completion", Message: "retained provider Port is unavailable",
			}
		case <-ctx.Done():
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	port.cancel = func(context.Context, acp.Target) error {
		cancelOnce.Do(func() { close(providerCancelled) })
		return nil
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-cancel-continuity"}, port)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-cancel-continuity", Message: "hold open"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider prompt did not start")
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "cancel", RequestID: "REQ-cancel-continuity"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("unsafe cancel = %#v, %T %v", response, err, err)
	}
	requestLines := harness.waitForRequestResult(t, "REQ-cancel-continuity")
	if got, want := requestLineTypes(requestLines), []string{FrameClientCommand, FrameClientCommand, FrameProviderResult}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unsafe cancel line types = %v, want %v", got, want)
	}
	var terminal map[string]any
	if err := json.Unmarshal(requestLines[len(requestLines)-1].Data, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != "failed" || terminal["source"] != "transport" {
		t.Fatalf("unsafe cancel terminal = %#v", terminal)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("unsafe cancellation did not stop the Session channel")
	}
	current, err := harness.repo.GetSession(context.Background(), "S-cancel-continuity")
	if err != nil || current.Status != session.StatusFailed || current.WorkerSocket != "" {
		t.Fatalf("unsafe cancel Session = %#v, %v", current, err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.promptSpecs) != 1 || port.cancelCalls != 1 {
		t.Fatalf("provider calls after unsafe cancel: prompts=%d cancels=%d", len(port.promptSpecs), port.cancelCalls)
	}
}

func TestWorkerCancelCommandFailureIsBoundedAndFailsClosed(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
	}
	port.cancel = func(context.Context, acp.Target) error {
		return errors.New("SECRET provider cancellation detail")
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-cancel-command-fails"}, port)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-cancel-command-fails", Message: "hold open"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider prompt did not start")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "cancel", RequestID: "REQ-cancel-command-fails"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("failed provider Cancel = %#v, %T %v", response, err, err)
	}
	requestLines := harness.waitForRequestResult(t, "REQ-cancel-command-fails")
	var terminal map[string]any
	if err := json.Unmarshal(requestLines[len(requestLines)-1].Data, &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal["status"] != "failed" || terminal["code"] != "provider_cancel_unconfirmed" {
		t.Fatalf("failed Cancel terminal = %#v", terminal)
	}
	allLines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(allLines)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET provider cancellation detail") {
		t.Fatalf("provider cancellation detail leaked into the channel: %s", encoded)
	}
}

func TestWorkerDoesNotDispatchCancelWhenDurableCommandAppendFails(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, _ func(acp.Event) error) (acp.PromptReceipt, error) {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-cancel-journal-failure"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-cancel-journal-failure", Message: "hold open"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider prompt did not start")
	}
	if err := harness.worker.log.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "cancel", RequestID: "REQ-cancel-journal-failure"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("cancel with failed journal = %#v, %T %v", response, err, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("journal failure did not stop the Session channel")
	}
	port.mu.Lock()
	cancelCalls := port.cancelCalls
	port.mu.Unlock()
	if cancelCalls != 0 {
		t.Fatalf("cancel reached provider after its durable append failed %d times", cancelCalls)
	}
	lines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, line := range lines {
		if line.RequestID == "REQ-cancel-journal-failure" && line.Type == FrameClientCommand {
			var command map[string]string
			if err := json.Unmarshal(line.Data, &command); err != nil {
				t.Fatal(err)
			}
			if command["command"] == "cancel" {
				t.Fatalf("undurable cancel appeared in replay: %#v", lines)
			}
		}
	}
}

func TestWorkerFailsStoppedWithoutDispatchingPromptWhenCommandJournalIsUnavailable(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-prompt-journal-failure"}, port)
	if err := harness.worker.log.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-prompt-journal-failure", Message: "must not dispatch"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("prompt with failed journal = %#v, %T %v", response, err, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt journal failure did not stop the Session channel")
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.promptSpecs) != 0 {
		t.Fatalf("prompt reached provider after its durable append failed: %#v", port.promptSpecs)
	}
}

func TestWorkerPermissionFramesAndDecisionStayOnRetainedPort(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), permissionDecided: make(chan permissionDecision, 1)}
	permissionRaw := json.RawMessage(`{"toolCall":{"toolCallId":"tool-1","title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]}`)
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		if err := emit(acp.Event{Type: "session_request_permission", PermissionID: "bridge-permission-1", Raw: permissionRaw}); err != nil {
			return acp.PromptReceipt{RequestID: spec.RequestID}, err
		}
		select {
		case <-port.permissionDecided:
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-permission"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-permission", Message: "write"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}

	permissionID := PermissionConsoleID("REQ-permission", "tool-1")
	deadline := time.Now().Add(2 * time.Second)
	var permissionStatus Message
	for {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
		cancel()
		if statusErr == nil && len(status.Permissions) == 1 && status.Permissions[0].ID == permissionID {
			permissionStatus = status
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission was not exposed: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if permissionStatus.Seq == 0 || permissionStatus.Seq != harness.worker.log.LastSequence() {
		t.Fatalf("permission snapshot watermark = %d, log = %d", permissionStatus.Seq, harness.worker.log.LastSequence())
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	decision, err := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionID, OptionID: "allow-once"})
	cancel()
	if err != nil || decision.Type != "permission_responded" {
		t.Fatalf("permission decision = %#v, %v", decision, err)
	}
	requestLines := harness.waitForRequestResult(t, "REQ-permission")
	if got, want := requestLineTypes(requestLines), []string{FrameClientCommand, FrameProviderEvent, FramePermissionRequested, FramePermissionResolved, FrameProviderResult}; !reflect.DeepEqual(got, want) {
		t.Fatalf("permission line types = %v, want %v", got, want)
	}
	if got := string(requestLines[1].Data); got != string(permissionRaw) {
		t.Fatalf("permission provider event was rewritten: %s", got)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if got, want := port.permissionCalls, []permissionDecision{{permissionID: "bridge-permission-1", optionID: "allow-once"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider permission decisions = %#v, want %#v", got, want)
	}
}

func TestWorkerFailsClosedWhenPermissionResolutionCannotBeJournaled(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog(), permissionDecided: make(chan permissionDecision, 1)}
	permissionRaw := json.RawMessage(`{"toolCall":{"toolCallId":"tool-journal","title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]}`)
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		if err := emit(acp.Event{Type: "session_request_permission", PermissionID: "bridge-journal", Raw: permissionRaw}); err != nil {
			return acp.PromptReceipt{RequestID: spec.RequestID}, err
		}
		select {
		case <-port.permissionDecided:
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-perm-journal"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-permission-journal-failure", Message: "write"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	permissionID := PermissionConsoleID("REQ-permission-journal-failure", "tool-journal")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
		cancel()
		if statusErr == nil && len(status.Permissions) == 1 && status.Permissions[0].ID == permissionID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission was not exposed: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := harness.worker.log.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionID, OptionID: "allow-once"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("permission response with failed journal = %#v, %T %v", response, err, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("permission journal failure did not stop the Session channel")
	}
	port.mu.Lock()
	permissionCalls := append([]permissionDecision(nil), port.permissionCalls...)
	port.mu.Unlock()
	if got, want := permissionCalls, []permissionDecision{{permissionID: "bridge-journal", optionID: "allow-once"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider permission calls = %#v, want %#v", got, want)
	}
	lines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, line := range lines {
		if line.Type == FramePermissionResolved {
			t.Fatalf("undurable permission resolution appeared in replay: %#v", lines)
		}
	}
}

func TestWorkerRetainsPendingPermissionWhenProviderRejectsFirstDecisionAttempt(t *testing.T) {
	port := &workerTestPort{
		catalog: advertisedWorkerTestCatalog(), permissionDecided: make(chan permissionDecision, 1),
		permissionErrors: []error{&acp.ProviderPermissionRejectedError{}},
	}
	permissionRaw := json.RawMessage(`{"toolCall":{"toolCallId":"tool-retry","title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]}`)
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		if err := emit(acp.Event{Type: "session_request_permission", PermissionID: "bridge-retry", Raw: permissionRaw}); err != nil {
			return acp.PromptReceipt{RequestID: spec.RequestID}, err
		}
		select {
		case <-port.permissionDecided:
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "end_turn"}, nil
		case <-ctx.Done():
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-permission-retry"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-permission-retry", Message: "write"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	permissionID := PermissionConsoleID("REQ-permission-retry", "tool-retry")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
		cancel()
		if statusErr == nil && len(status.Permissions) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission was not exposed: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	firstResponse, firstErr := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionID, OptionID: "allow-once"})
	cancel()
	var rejected *acp.ProviderPermissionRejectedError
	if !errors.As(firstErr, &rejected) || firstResponse.ErrorCode != "permission_response_rejected" {
		t.Fatalf("first provider decision = %#v, %T %v", firstResponse, firstErr, firstErr)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if statusErr != nil || len(status.Permissions) != 1 || status.Permissions[0].ID != permissionID {
		t.Fatalf("failed decision consumed pending permission: %#v, %v", status, statusErr)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, retryErr := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionID, OptionID: "allow-once"})
	cancel()
	if retryErr != nil || response.Type != "permission_responded" {
		t.Fatalf("retried decision = %#v, %v", response, retryErr)
	}
	harness.waitForRequestResult(t, "REQ-permission-retry")
}

func TestWorkerPermissionContinuityFailureDurablyAbandonsCallbacksAndFailsPort(t *testing.T) {
	port := &workerTestPort{
		catalog:          advertisedWorkerTestCatalog(),
		permissionErrors: []error{&acp.ProtocolError{Operation: "permission decision", Message: "lost acknowledgement"}},
	}
	permissionRaw := func(toolID string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"toolCall":{"toolCallId":%q,"title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]}`, toolID))
	}
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		for _, item := range []struct {
			bridgeID string
			toolID   string
		}{{bridgeID: "bridge-continuity-a", toolID: "tool-continuity-a"}, {bridgeID: "bridge-continuity-b", toolID: "tool-continuity-b"}} {
			if err := emit(acp.Event{Type: "session_request_permission", PermissionID: item.bridgeID, Raw: permissionRaw(item.toolID)}); err != nil {
				return acp.PromptReceipt{RequestID: spec.RequestID}, err
			}
		}
		<-ctx.Done()
		return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-permission-continuity"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-permission-continuity", Message: "write"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	permissionA := PermissionConsoleID("REQ-permission-continuity", "tool-continuity-a")
	permissionB := PermissionConsoleID("REQ-permission-continuity", "tool-continuity-b")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
		cancel()
		if statusErr == nil && len(status.Permissions) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permissions were not exposed: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	response, decisionErr := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionA, OptionID: "allow-once"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(decisionErr, &unavailable) || response.ErrorCode != "session_channel_failed" {
		t.Fatalf("ambiguous permission decision = %#v, %T %v", response, decisionErr, decisionErr)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("permission continuity failure did not stop the Session worker")
	}
	lines, err := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := map[string]uint64{}
	var workerErrorSeq uint64
	for _, line := range lines {
		switch line.Type {
		case FramePermissionAbandoned:
			var data map[string]string
			if err := json.Unmarshal(line.Data, &data); err != nil {
				t.Fatal(err)
			}
			abandoned[data["permission_id"]] = line.Seq
		case FramePermissionResolved:
			t.Fatalf("ambiguous permission decision was recorded as resolved: %#v", line)
		case FrameWorkerError:
			if strings.Contains(string(line.Data), "provider_permission_continuity_lost") {
				workerErrorSeq = line.Seq
			}
		}
	}
	if len(abandoned) != 2 || abandoned[permissionA] == 0 || abandoned[permissionB] == 0 {
		t.Fatalf("durable abandoned callbacks = %#v, want %q and %q", abandoned, permissionA, permissionB)
	}
	if workerErrorSeq == 0 || abandoned[permissionA] >= workerErrorSeq || abandoned[permissionB] >= workerErrorSeq {
		t.Fatalf("abandonment must precede worker failure: abandoned=%#v worker_error=%d", abandoned, workerErrorSeq)
	}
	port.mu.Lock()
	permissionCalls := append([]permissionDecision(nil), port.permissionCalls...)
	closeCalls := port.closeCalls
	port.mu.Unlock()
	if got, want := permissionCalls, []permissionDecision{{permissionID: "bridge-continuity-a", optionID: "allow-once"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("provider permission calls = %#v, want %#v", got, want)
	}
	if closeCalls != 1 {
		t.Fatalf("provider Port Close calls = %d, want 1", closeCalls)
	}
}

func TestWorkerAcceptedCancelFencesLaterPermissionDecision(t *testing.T) {
	cancelEntered := make(chan struct{})
	cancelRelease := make(chan struct{})
	promptCancelled := make(chan struct{})
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	port.cancel = func(context.Context, acp.Target) error {
		close(cancelEntered)
		<-cancelRelease
		close(promptCancelled)
		return nil
	}
	permissionRaw := json.RawMessage(`{"toolCall":{"toolCallId":"tool-cancel","title":"Write file","kind":"edit"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]}`)
	port.prompt = func(ctx context.Context, spec acp.PromptSpec, emit func(acp.Event) error) (acp.PromptReceipt, error) {
		if err := emit(acp.Event{Type: "session_request_permission", PermissionID: "bridge-cancel", Raw: permissionRaw}); err != nil {
			return acp.PromptReceipt{RequestID: spec.RequestID}, err
		}
		select {
		case <-promptCancelled:
			return acp.PromptReceipt{RequestID: spec.RequestID, LastSeq: 1, StopReason: "cancelled"}, nil
		case <-ctx.Done():
			return acp.PromptReceipt{RequestID: spec.RequestID}, ctx.Err()
		}
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-permission-cancel-fence"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if _, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-permission-cancel", Message: "write"}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	permissionID := PermissionConsoleID("REQ-permission-cancel", "tool-cancel")
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
		cancel()
		if statusErr == nil && len(status.Permissions) == 1 && status.Permissions[0].ID == permissionID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission was not exposed before cancel: %#v, %v", status, statusErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	type commandResult struct {
		message Message
		err     error
	}
	cancelResult := make(chan commandResult, 1)
	go func() {
		cancelCtx, cancelCall := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCall()
		message, err := harness.client.Send(cancelCtx, Command{Type: "cancel", RequestID: "REQ-permission-cancel"})
		cancelResult <- commandResult{message: message, err: err}
	}()
	select {
	case <-cancelEntered:
	case <-time.After(time.Second):
		t.Fatal("Port.Cancel was not entered")
	}

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	decision, decisionErr := harness.client.Send(ctx, Command{Type: "permission_response", PermissionID: permissionID, OptionID: "allow-once"})
	cancel()
	if decisionErr == nil || decision.Type != "error" {
		t.Fatalf("permission after accepted cancel = %#v, %v", decision, decisionErr)
	}
	port.mu.Lock()
	permissionCalls := append([]permissionDecision(nil), port.permissionCalls...)
	port.mu.Unlock()
	if len(permissionCalls) != 0 {
		t.Fatalf("permission response overtook accepted cancel: %#v", permissionCalls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	status, statusErr := harness.client.Send(ctx, Command{Type: "status"})
	cancel()
	if statusErr != nil || len(status.Permissions) != 0 {
		t.Fatalf("accepted cancel left stale actionable permissions: %#v, %v", status, statusErr)
	}

	duplicateResult := make(chan commandResult, 1)
	go func() {
		cancelCtx, cancelCall := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCall()
		message, err := harness.client.Send(cancelCtx, Command{Type: "cancel", RequestID: "REQ-permission-cancel"})
		duplicateResult <- commandResult{message: message, err: err}
	}()
	select {
	case result := <-duplicateResult:
		t.Fatalf("duplicate cancel acknowledged before provider settlement: %#v, %v", result.message, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(cancelRelease)
	select {
	case result := <-cancelResult:
		if result.err != nil || result.message.Type != "cancelled" {
			t.Fatalf("cancel result = %#v, %v", result.message, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("accepted cancel did not settle")
	}
	select {
	case result := <-duplicateResult:
		if result.err != nil || result.message.Type != "cancelled" {
			t.Fatalf("duplicate cancel result = %#v, %v", result.message, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate cancel did not observe provider settlement")
	}
	requestLines := harness.waitForRequestResult(t, "REQ-permission-cancel")
	if got, want := requestLineTypes(requestLines), []string{
		FrameClientCommand, FrameProviderEvent, FramePermissionRequested,
		FrameClientCommand, FramePermissionAbandoned, FrameProviderResult,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cancelled permission line types = %v, want %v", got, want)
	}
	for _, line := range requestLines {
		if line.Type == FramePermissionResolved {
			t.Fatalf("permission resolved after cancel: %#v", requestLines)
		}
	}
	port.mu.Lock()
	cancelCalls := port.cancelCalls
	port.mu.Unlock()
	if cancelCalls != 1 {
		t.Fatalf("duplicate cancel reached provider %d times", cancelCalls)
	}
}

func TestWorkerFatalProviderOptionsFailureStopsPoisonedPort(t *testing.T) {
	port := &workerTestPort{
		catalog:   advertisedWorkerTestCatalog(),
		optionErr: &acp.ProtocolError{Operation: "fixture", Message: "private identity drift detail"},
	}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-options-fatal"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "provider_options"})
	cancel()
	var unavailable *ChannelUnavailableError
	if !errors.As(err, &unavailable) || response.ErrorCode != "session_channel_failed" || strings.Contains(response.Message, "private identity drift detail") {
		t.Fatalf("fatal options response = %#v, %T %v", response, err, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("poisoned provider Port did not stop")
	}
	lines, readErr := sessionlog.ReadFile(harness.worker.LogPath(), 0)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var failures int
	for _, line := range lines {
		if line.Type == FrameWorkerError {
			failures++
			if strings.Contains(string(line.Data), "private identity drift detail") {
				t.Fatalf("fatal options journal leaked private detail: %s", line.Data)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("fatal options worker errors = %d; lines=%#v", failures, lines)
	}
}

func TestWorkerDoesNotDispatchAcceptedPromptAfterLifecycleFenceChanges(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-prompt-lifecycle-fence"}, port)
	harness.allowRunError = true
	harness.repo.mu.Lock()
	harness.repo.beforeWorkerCAS = func(before, after session.WorkerState) {
		if before.Status != session.StatusWaiting || after.Status != session.StatusWorking {
			return
		}
		harness.repo.mu.Lock()
		harness.repo.current.Status = session.StatusFailed
		harness.repo.mu.Unlock()
	}
	harness.repo.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	accepted, err := harness.client.Send(ctx, Command{Type: "prompt", RequestID: "REQ-lifecycle-fence", Message: "must not dispatch"})
	cancel()
	if err != nil || accepted.Type != "accepted" {
		t.Fatalf("prompt admission = %#v, %v", accepted, err)
	}
	lines := harness.waitForRequestResult(t, "REQ-lifecycle-fence")
	port.mu.Lock()
	promptCalls := len(port.promptSpecs)
	port.mu.Unlock()
	if promptCalls != 0 {
		t.Fatalf("lifecycle-fenced prompt reached provider %d times", promptCalls)
	}
	var terminal map[string]any
	if err := json.Unmarshal(lines[len(lines)-1].Data, &terminal); err != nil || terminal["code"] != "session_lifecycle_changed" {
		t.Fatalf("lifecycle-fenced terminal = %#v, %v", terminal, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle-fenced worker did not stop")
	}
	current, loadErr := harness.repo.GetSession(context.Background(), "S-prompt-lifecycle-fence")
	if loadErr != nil || current.Status != session.StatusFailed || current.WorkerPID == 0 || current.WorkerSocket == "" {
		t.Fatalf("lifecycle quarantine = %#v, %v", current, loadErr)
	}
}

func TestWorkerPartialConfigurationFailsClosedIfProviderResetsUnspecifiedControl(t *testing.T) {
	catalog := advertisedWorkerTestCatalog()
	catalog.Model.CurrentValue = "model-selected"
	port := &workerTestPort{catalog: catalog, effortResetsModel: true}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-config-interdependent"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "configure", ReasoningEffort: "high"})
	cancel()
	var configuration *acp.ProviderConfigurationError
	if !errors.As(err, &configuration) || response.ErrorCode != "provider_configuration_failed" {
		t.Fatalf("interdependent config = %#v, %T %v", response, err, err)
	}
	select {
	case <-harness.done:
	case <-time.After(2 * time.Second):
		t.Fatal("uncertain provider configuration did not stop the Port")
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.configureSpecs) != 1 || port.configureSpecs[0].Model != "model-selected" || port.configureSpecs[0].ReasoningEffort != "high" {
		t.Fatalf("Configure did not protect fresh current tuple: %#v", port.configureSpecs)
	}
}

func TestWorkerPartialConfigurationPreservesUnselectableProviderDefault(t *testing.T) {
	catalog := advertisedWorkerTestCatalog()
	catalog.Model.CurrentValue = "provider-native-default"
	catalog.Model.Choices = []acp.ProviderOptionChoice{
		{Value: "model-selected", Label: "Selected model"},
		{Value: "model-other", Label: "Other model"},
	}
	port := &workerTestPort{catalog: catalog}
	harness := startRunningWorkerTest(t, session.Session{ID: "S-config-provider-default"}, port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	response, err := harness.client.Send(ctx, Command{Type: "configure", ReasoningEffort: "high"})
	cancel()
	if err != nil || response.Type != "configured" || response.ProviderOptions == nil {
		t.Fatalf("configure with unselectable current default = %#v, %v", response, err)
	}
	if response.ProviderOptions.Model.CurrentValue != "provider-native-default" || response.ProviderOptions.ReasoningEffort.CurrentValue != "high" {
		t.Fatalf("configured catalog did not preserve provider default: %#v", response.ProviderOptions)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.configureSpecs) != 1 || port.configureSpecs[0].Model != "" || port.configureSpecs[0].ReasoningEffort != "high" {
		t.Fatalf("unadvertised provider default was sent back as a selection: %#v", port.configureSpecs)
	}
}

func TestParsePendingPermissionBoundsEveryProviderField(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"toolCall": map[string]any{"toolCallId": "tool-1", "title": "Write file", "kind": "edit"},
			"options":  []any{map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"}},
		}
	}
	tests := map[string]func(map[string]any, *acp.Event){
		"bridge id": func(_ map[string]any, event *acp.Event) {
			event.PermissionID = strings.Repeat("x", maxPermissionIDBytes+1)
		},
		"tool id": func(raw map[string]any, _ *acp.Event) {
			raw["toolCall"].(map[string]any)["toolCallId"] = strings.Repeat("x", maxPermissionIDBytes+1)
		},
		"title": func(raw map[string]any, _ *acp.Event) {
			raw["toolCall"].(map[string]any)["title"] = strings.Repeat("x", maxPermissionDisplayBytes+1)
		},
		"tool kind": func(raw map[string]any, _ *acp.Event) {
			raw["toolCall"].(map[string]any)["kind"] = strings.Repeat("x", maxPermissionKindBytes+1)
		},
		"option id": func(raw map[string]any, _ *acp.Event) {
			raw["options"].([]any)[0].(map[string]any)["optionId"] = strings.Repeat("x", maxPermissionIDBytes+1)
		},
		"option name": func(raw map[string]any, _ *acp.Event) {
			raw["options"].([]any)[0].(map[string]any)["name"] = strings.Repeat("x", maxPermissionDisplayBytes+1)
		},
		"control text": func(raw map[string]any, _ *acp.Event) { raw["toolCall"].(map[string]any)["title"] = "unsafe\ntext" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			raw := base()
			event := acp.Event{PermissionID: "bridge-1"}
			mutate(raw, &event)
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			event.Raw = encoded
			if _, err := parsePendingPermission("REQ-one", event); err == nil {
				t.Fatal("unbounded provider permission field was accepted")
			}
		})
	}
}

func TestWorkerLeavesProviderConfigurationOwnedByResumedPort(t *testing.T) {
	port := &workerTestPort{catalog: advertisedWorkerTestCatalog()}
	metadata := json.RawMessage(`{"model":"model-selected","reasoning_effort":"high"}`)
	startRunningWorkerTest(t, session.Session{ID: "S-configured", MetadataJSON: metadata}, port)
	port.mu.Lock()
	defer port.mu.Unlock()
	if len(port.startSpecs) != 1 || len(port.configureSpecs) != 0 {
		t.Fatalf("Port calls: start=%d configure=%d", len(port.startSpecs), len(port.configureSpecs))
	}
	if port.startSpecs[0].Model != "" || port.startSpecs[0].ReasoningEffort != "" {
		t.Fatalf("Start carried gateway metadata into provider configuration: %#v", port.startSpecs[0])
	}
	if _, err := os.Stat(filepath.Dir(port.startSpecs[0].WorkingDirectory)); err != nil {
		t.Fatalf("working directory disappeared: %v", err)
	}
}
