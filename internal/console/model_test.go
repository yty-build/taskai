package console

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type fakeConsoleBackend struct {
	mu               sync.Mutex
	detail           task.Detail
	sessions         map[string]session.Session
	history          map[string][]sessionlog.Line
	permissionState  map[string]appservice.PermissionSnapshot
	permissionReads  map[string]int
	profiles         []agents.Profile
	providerOptions  acp.ProviderOptionCatalog
	promptErr        error
	prompts          []string
	configured       []session.ProviderOptions
	launches         []string
	cancelled        int
	resumed          int
	stopped          int
	taskFollowCalls  int
	sessionFollows   int
	freshReplayCalls int
	taskAttempts     int
	taskAttempted    chan struct{}
	taskChanged      chan struct{}
}

func newFakeConsoleBackend() *fakeConsoleBackend {
	current := session.Session{
		ID: "S-1", TaskID: "T-1", Agent: "codex", Provider: "openai", Role: "worker",
		RepoPath: "/repo", Status: session.StatusWaiting,
	}
	return &fakeConsoleBackend{
		detail: task.Detail{
			Task:     task.Task{ID: "T-1", Title: "Ship the channel", Status: task.StatusWorking},
			Sessions: []session.Session{current},
		},
		sessions:        map[string]session.Session{"S-1": current},
		history:         make(map[string][]sessionlog.Line),
		permissionState: make(map[string]appservice.PermissionSnapshot),
		permissionReads: make(map[string]int),
		taskAttempted:   make(chan struct{}, 1024),
		taskChanged:     make(chan struct{}, 1),
		profiles: []agents.Profile{
			{Name: "codex", Provider: "openai", Command: "codex"},
			{Name: "claude", Provider: "anthropic", Command: "claude"},
		},
		providerOptions: acp.ProviderOptionCatalog{
			Model: acp.ProviderSelectCatalog{Supported: true, CurrentValue: "provider-model", Choices: []acp.ProviderOptionChoice{
				{Value: "model-a", Label: "Model A"}, {Value: "model-b", Label: "Model B", Group: "Fast"},
			}},
			ReasoningEffort: acp.ProviderSelectCatalog{Supported: true, CurrentValue: "medium", Choices: []acp.ProviderOptionChoice{
				{Value: "low", Label: "Low"}, {Value: "high", Label: "High"},
			}},
		},
	}
}

func (f *fakeConsoleBackend) Task(context.Context, string) (task.Detail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := f.detail
	result.Sessions = append([]session.Session(nil), f.detail.Sessions...)
	return result, nil
}

func (f *fakeConsoleBackend) Session(_ context.Context, id string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.sessions[id]
	if !ok {
		return session.Session{}, errors.New("missing Session")
	}
	return current, nil
}

func (f *fakeConsoleBackend) FreshReplayCursor(_ context.Context, id string, maxRecords int) (uint64, bool, error) {
	if maxRecords <= 0 {
		return 0, false, errors.New("max records must be positive")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freshReplayCalls++
	history := f.history[id]
	if len(history) <= maxRecords {
		return 0, false, nil
	}
	return history[len(history)-maxRecords-1].Seq, true, nil
}

func (f *fakeConsoleBackend) PermissionSnapshot(_ context.Context, id string) (appservice.PermissionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissionReads[id]++
	snapshot := f.permissionState[id]
	snapshot.Permissions = append([]worker.PendingPermission(nil), snapshot.Permissions...)
	for index := range snapshot.Permissions {
		snapshot.Permissions[index].Options = append([]worker.PermissionOption(nil), snapshot.Permissions[index].Options...)
	}
	return snapshot, nil
}

func (f *fakeConsoleBackend) Follow(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) error {
	f.mu.Lock()
	f.sessionFollows++
	history := append([]sessionlog.Line(nil), f.history[id]...)
	current := f.sessions[id]
	f.mu.Unlock()
	for _, line := range history {
		if line.Seq > cursor {
			if err := consume(line); err != nil {
				return err
			}
		}
	}
	if current.WorkerSocket == "" {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeConsoleBackend) TaskReplayWindows(_ context.Context, _ string, cursors map[string]uint64, maxRecords int) (map[string]uint64, map[string]appservice.ReplayWindow, error) {
	if maxRecords <= 0 {
		return nil, nil, errors.New("max records must be positive")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	effective := make(map[string]uint64, len(f.detail.Sessions))
	windows := make(map[string]appservice.ReplayWindow, len(f.detail.Sessions))
	for _, current := range f.detail.Sessions {
		position := cursors[current.ID]
		clipped := false
		history := f.history[current.ID]
		if position == 0 && len(history) > maxRecords {
			position = history[len(history)-maxRecords-1].Seq
			clipped = true
		}
		earliest := position
		if position != ^uint64(0) {
			earliest++
		}
		effective[current.ID] = position
		windows[current.ID] = appservice.ReplayWindow{Cursor: position, EarliestSequence: earliest, Clipped: clipped}
	}
	return effective, windows, nil
}

func (f *fakeConsoleBackend) FollowTask(ctx context.Context, _ string, cursors map[string]uint64, consume func(appservice.TaskActivity) error) error {
	if cursors == nil {
		cursors = make(map[string]uint64)
	}
	f.mu.Lock()
	f.taskFollowCalls++
	f.mu.Unlock()
	for {
		f.mu.Lock()
		var available []appservice.TaskActivity
		for _, current := range f.detail.Sessions {
			for _, line := range f.history[current.ID] {
				if line.Seq > cursors[current.ID] {
					available = append(available, appservice.TaskActivity{SessionID: current.ID, Line: line})
				}
			}
		}
		f.mu.Unlock()
		sort.Slice(available, func(i, j int) bool {
			left, right := available[i], available[j]
			if left.Line.Timestamp.Equal(right.Line.Timestamp) {
				if left.SessionID == right.SessionID {
					return left.Line.Seq < right.Line.Seq
				}
				return left.SessionID < right.SessionID
			}
			if left.Line.Timestamp.IsZero() {
				return true
			}
			if right.Line.Timestamp.IsZero() {
				return false
			}
			return left.Line.Timestamp.Before(right.Line.Timestamp)
		})
		for _, activity := range available {
			f.mu.Lock()
			f.taskAttempts++
			f.mu.Unlock()
			select {
			case f.taskAttempted <- struct{}{}:
			default:
			}
			if err := consume(activity); err != nil {
				return err
			}
			cursors[activity.SessionID] = activity.Line.Seq
		}
		if len(available) > 0 {
			continue
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-f.taskChanged:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (f *fakeConsoleBackend) Prompt(_ context.Context, _ string, message string) (worker.Message, error) {
	f.prompts = append(f.prompts, message)
	if f.promptErr != nil {
		return worker.Message{}, f.promptErr
	}
	return worker.Message{Type: "accepted", RequestID: "REQ-new", State: "ready"}, nil
}

func (*fakeConsoleBackend) DecidePermission(context.Context, string, string, string, bool) error {
	return nil
}

func (f *fakeConsoleBackend) Cancel(context.Context, string) error {
	f.cancelled++
	return nil
}

func (f *fakeConsoleBackend) Resume(_ context.Context, id string) (session.Session, error) {
	f.resumed++
	current := f.sessions[id]
	current.WorkerSocket = "/tmp/worker.sock"
	f.sessions[id] = current
	return current, nil
}

func TestResumeActionRequiresExactIdentityAndClearOwnership(t *testing.T) {
	backend := newFakeConsoleBackend()
	model := baseModel(context.Background(), backend)
	model.mode = taskView
	model.task = backend.detail

	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	model = updated.(Model)
	if command != nil || backend.resumed != 0 || !strings.Contains(model.notice, "no saved provider identity") {
		t.Fatalf("identityless Resume command=%v calls=%d notice=%q", command != nil, backend.resumed, model.notice)
	}
	model.overlay = paletteOverlay
	model.width, model.height = 100, 30
	if strings.Contains(model.View(), "Resume worker") {
		t.Fatalf("identityless command palette advertised Resume:\n%s", model.View())
	}

	quarantined := backend.detail.Sessions[0]
	quarantined.ProviderRef = "provider-ref"
	quarantined.WorkerPID = 42
	quarantined.WorkerSocket = "/tmp/quarantined.sock"
	quarantined.Status = session.StatusFailed
	model.overlay = noOverlay
	model.task.Sessions[0] = quarantined
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	model = updated.(Model)
	if command != nil || backend.resumed != 0 || !strings.Contains(model.notice, "quarantined worker ownership") {
		t.Fatalf("quarantined Resume command=%v calls=%d notice=%q", command != nil, backend.resumed, model.notice)
	}

	resumable := quarantined
	resumable.WorkerPID = 0
	resumable.WorkerSocket = ""
	resumable.Status = session.StatusStopped
	model.task.Sessions[0] = resumable
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	model = updated.(Model)
	if command == nil {
		t.Fatal("resumable Session did not produce a Resume command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if backend.resumed != 1 || !strings.Contains(model.notice, "resumed") {
		t.Fatalf("resumable calls=%d notice=%q", backend.resumed, model.notice)
	}
}

func (f *fakeConsoleBackend) Stop(context.Context, string) error {
	f.stopped++
	return nil
}

func (f *fakeConsoleBackend) AvailableAgents(context.Context) ([]agents.Profile, error) {
	return append([]agents.Profile(nil), f.profiles...), nil
}

func (f *fakeConsoleBackend) ConnectSession(_ context.Context, params appservice.StartSessionParams) (ProviderSession, error) {
	current := session.Session{
		ID: "S-2", TaskID: params.TaskID, Agent: params.Agent, Provider: "openai", Role: params.Role,
		RepoPath: params.RepoPath, Status: session.StatusWaiting, WorkerSocket: "/tmp/S-2.sock",
	}
	f.sessions[current.ID] = current
	return ProviderSession{Session: current, Options: f.providerOptions}, nil
}

func (f *fakeConsoleBackend) LoadProviderOptions(_ context.Context, id string) (ProviderSession, error) {
	return ProviderSession{Session: f.sessions[id], Options: f.providerOptions}, nil
}

func (f *fakeConsoleBackend) ConfigureSession(_ context.Context, id string, options session.ProviderOptions) (ProviderSession, error) {
	f.configured = append(f.configured, options)
	return ProviderSession{Session: f.sessions[id], Options: f.providerOptions}, nil
}

func (f *fakeConsoleBackend) PromptSession(_ context.Context, id, prompt string) (appservice.LaunchResult, error) {
	f.launches = append(f.launches, prompt)
	return appservice.LaunchResult{Session: f.sessions[id], RequestID: "REQ-first", State: "ready", Started: true}, nil
}

func (f *fakeConsoleBackend) addTaskSession(current session.Session, history []sessionlog.Line) {
	f.mu.Lock()
	f.detail.Sessions = append(f.detail.Sessions, current)
	f.sessions[current.ID] = current
	f.history[current.ID] = append([]sessionlog.Line(nil), history...)
	f.mu.Unlock()
	select {
	case f.taskChanged <- struct{}{}:
	default:
	}
}

func (f *fakeConsoleBackend) followCounts() (taskCalls, sessionCalls, freshCalls, taskAttempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taskFollowCalls, f.sessionFollows, f.freshReplayCalls, f.taskAttempts
}

func TestTaskConsoleReplaysOnlySessionChannelJSONL(t *testing.T) {
	backend := newFakeConsoleBackend()
	backend.history["S-1"] = []sessionlog.Line{
		testLine(1, "REQ-1", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "Hello"}),
		testLine(2, "REQ-1", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": "Hi immediately"}),
		testLine(3, "REQ-1", worker.FrameProviderResult, map[string]any{"status": "completed", "stop_reason": "end_turn"}),
	}
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for range 3 {
		select {
		case message := <-model.manager.events:
			updated, _ := model.Update(message)
			model = updated.(Model)
		case <-time.After(time.Second):
			t.Fatal("Session Channel replay did not arrive asynchronously")
		}
	}
	if model.cursors["S-1"] != 3 || len(model.histories["S-1"]) != 3 {
		t.Fatalf("replay cursor/history = %d/%d", model.cursors["S-1"], len(model.histories["S-1"]))
	}
	model.width, model.height = 100, 30
	model.mode = sessionView
	model.current = backend.sessions["S-1"]
	view := model.View()
	for _, want := range []string{"SESSION CHANNEL", "YOU", "Hello", "Hi immediately", "Turn complete"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	taskCalls, sessionCalls, freshCalls, _ := backend.followCounts()
	if taskCalls != 1 || sessionCalls != 0 || freshCalls != 0 {
		t.Fatalf("Task attachment followers = aggregate %d/direct %d/fresh %d", taskCalls, sessionCalls, freshCalls)
	}
}

func TestTaskWatcherDiscoversNewChildThroughOneAggregateFollow(t *testing.T) {
	backend := newFakeConsoleBackend()
	backend.history["S-1"] = []sessionlog.Line{
		testLine(1, "REQ-first", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "text": "first"}),
	}
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case message := <-model.manager.events:
		updated, _ := model.Update(message)
		model = updated.(Model)
	case <-time.After(time.Second):
		t.Fatal("initial aggregate Task activity did not arrive")
	}

	child := session.Session{ID: "S-child", TaskID: "T-1", Agent: "claude", Provider: "anthropic", Role: "worker", Status: session.StatusWaiting}
	backend.addTaskSession(child, []sessionlog.Line{
		testLine(1, "REQ-child", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "text": "child arrived"}),
	})
	select {
	case message := <-model.manager.events:
		activity, ok := message.(taskActivityMsg)
		if !ok || activity.activity.SessionID != child.ID {
			t.Fatalf("new child activity = %#v", message)
		}
		updated, _ := model.Update(message)
		model = updated.(Model)
	case <-time.After(time.Second):
		t.Fatal("newly discovered child did not arrive through aggregate Task follow")
	}
	if model.cursors[child.ID] != 1 || model.projections[child.ID] == nil {
		t.Fatalf("new child viewer state = cursor %d projection %v", model.cursors[child.ID], model.projections[child.ID] != nil)
	}
	taskCalls, sessionCalls, freshCalls, _ := backend.followCounts()
	if taskCalls != 1 || sessionCalls != 0 || freshCalls != 0 {
		t.Fatalf("new child spawned extra followers = aggregate %d/direct %d/fresh %d", taskCalls, sessionCalls, freshCalls)
	}
}

func TestTaskWatcherStartsFromDeclaredBoundedReplayWindow(t *testing.T) {
	backend := newFakeConsoleBackend()
	history := make([]sessionlog.Line, 0, initialReplayRecords+17)
	for sequence := 1; sequence <= initialReplayRecords+17; sequence++ {
		history = append(history, testLine(uint64(sequence), "", worker.FrameConnection, map[string]any{"state": "connected"}))
	}
	backend.history["S-1"] = history
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if model.cursors["S-1"] != 17 || !model.historyClipped["S-1"] {
		t.Fatalf("initial Task replay window = cursor %d, clipped %v", model.cursors["S-1"], model.historyClipped["S-1"])
	}
	window := model.replayWindows["S-1"]
	if window.Cursor != 17 || window.EarliestSequence != 18 || !window.Clipped {
		t.Fatalf("retained Task replay metadata = %#v", window)
	}
	view := model.View()
	for _, want := range []string{"1 replay window(s) clipped", "earliest_sequence=18", "cursor=17"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Task replay disclosure missing %q:\n%s", want, view)
		}
	}
	select {
	case message := <-model.manager.events:
		activity, ok := message.(taskActivityMsg)
		if !ok || activity.activity.Line.Seq != 18 {
			t.Fatalf("first bounded Task activity = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded Task replay did not start")
	}
}

func TestTaskWatcherStalledUIBoundsFullFrameProducersAcrossSessions(t *testing.T) {
	backend := newFakeConsoleBackend()
	backend.detail.Sessions = nil
	backend.sessions = make(map[string]session.Session)
	backend.history = make(map[string][]sessionlog.Line)
	payload := strings.Repeat("x", 256<<10)
	for index := 0; index < 64; index++ {
		current := session.Session{ID: fmt.Sprintf("S-stalled-%03d", index), TaskID: "T-1", Agent: "codex", Role: "worker", Status: session.StatusWaiting}
		backend.detail.Sessions = append(backend.detail.Sessions, current)
		backend.sessions[current.ID] = current
		backend.history[current.ID] = []sessionlog.Line{
			testLine(1, "", worker.FrameProviderEvent, map[string]any{"type": "future", "payload": payload}),
		}
	}
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-backend.taskAttempted:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("aggregate follower did not reach the bounded handoff")
		}
	}
	taskCalls, sessionCalls, freshCalls, attempts := backend.followCounts()
	if taskCalls != 1 || sessionCalls != 0 || freshCalls != 0 || attempts != 2 || len(model.manager.events) != 1 {
		cancel()
		t.Fatalf("stalled Task handoff = aggregate %d/direct %d/fresh %d/attempts %d/queued %d", taskCalls, sessionCalls, freshCalls, attempts, len(model.manager.events))
	}
	cancel()
	waitForTaskWatcherStop(t, model.manager)
}

func TestDirectSessionWatcherRemainsIndependent(t *testing.T) {
	backend := newFakeConsoleBackend()
	backend.history["S-1"] = []sessionlog.Line{
		testLine(1, "", worker.FrameConnection, map[string]any{"state": "connected"}),
	}
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case message := <-model.manager.events:
		if _, ok := message.(activityMsg); !ok {
			t.Fatalf("direct Session activity = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("direct Session replay did not arrive")
	}
	taskCalls, sessionCalls, freshCalls, _ := backend.followCounts()
	if taskCalls != 0 || sessionCalls != 1 || freshCalls != 1 {
		t.Fatalf("direct attachment followers = aggregate %d/direct %d/fresh %d", taskCalls, sessionCalls, freshCalls)
	}
}

func TestDirectSessionWatcherDeclaresBoundedReplayWindow(t *testing.T) {
	backend := newFakeConsoleBackend()
	history := make([]sessionlog.Line, 0, initialReplayRecords+17)
	for sequence := 1; sequence <= initialReplayRecords+17; sequence++ {
		history = append(history, testLine(uint64(sequence), "", worker.FrameConnection, map[string]any{"state": "connected"}))
	}
	backend.history["S-1"] = history
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	window := model.replayWindows["S-1"]
	if model.cursors["S-1"] != 17 || window.Cursor != 17 || window.EarliestSequence != 18 || !window.Clipped {
		t.Fatalf("direct replay metadata = cursor %d window %#v", model.cursors["S-1"], window)
	}
	view := model.View()
	for _, want := range []string{"REPLAY CLIPPED", "earliest_sequence=18", "cursor=17"} {
		if !strings.Contains(view, want) {
			t.Fatalf("direct replay disclosure missing %q:\n%s", want, view)
		}
	}
	select {
	case message := <-model.manager.events:
		activity, ok := message.(activityMsg)
		if !ok || activity.line.Seq != 18 {
			t.Fatalf("first bounded direct activity = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("bounded direct replay did not start")
	}
}

func TestDirectSessionReattachRestoresPermissionOlderThanReplayTail(t *testing.T) {
	backend := newFakeConsoleBackend()
	pending := worker.PendingPermission{
		ID: "permission-outside-tail-exact", RequestID: "REQ-long-turn", Title: "Approve retained action", Kind: "edit",
		Options: []worker.PermissionOption{
			{ID: "allow-outside-tail-exact", Name: "Allow once", Kind: "allow_once"},
			{ID: "reject-outside-tail-exact", Name: "Reject once", Kind: "reject_once"},
		},
	}
	total := initialReplayRecords + 40
	history := make([]sessionlog.Line, 0, total)
	history = append(history,
		testLine(1, pending.RequestID, worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "long running turn"}),
		testLine(2, pending.RequestID, worker.FramePermissionRequested, pending),
	)
	for sequence := 3; sequence <= total; sequence++ {
		history = append(history, testLine(uint64(sequence), pending.RequestID, worker.FrameProviderEvent, map[string]any{
			"type": "future_provider_activity", "index": sequence,
		}))
	}
	backend.history["S-1"] = history
	backend.permissionState["S-1"] = appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{pending}, Sequence: uint64(total), DecisionsReady: true,
	}

	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if model.pendingPermissionSnapshot == nil || model.projections["S-1"] != nil {
		t.Fatalf("watermarked snapshot before replay = queued %v projection %v", model.pendingPermissionSnapshot != nil, model.projections["S-1"] != nil)
	}
	for range initialReplayRecords {
		select {
		case message := <-model.manager.events:
			updated, _ := model.Update(message)
			model = updated.(Model)
		case <-time.After(time.Second):
			t.Fatal("bounded direct replay did not reach the permission snapshot watermark")
		}
	}
	if model.cursors["S-1"] != uint64(total) || model.pendingPermissionSnapshot != nil {
		t.Fatalf("replay/snapshot convergence = cursor %d queued %v", model.cursors["S-1"], model.pendingPermissionSnapshot != nil)
	}
	projection := model.projections["S-1"]
	_, card := projection.pendingPermission()
	if card == nil || card.id != pending.ID || len(card.options) != len(pending.Options) {
		t.Fatalf("reattached permission = %#v", card)
	}
	for index, option := range card.options {
		if option.id != pending.Options[index].ID {
			t.Fatalf("reattached option %d = %q, want %q", index, option.id, pending.Options[index].ID)
		}
	}
	for _, retained := range model.histories["S-1"] {
		if retained.line.Type == worker.FramePermissionRequested {
			t.Fatal("permission snapshot was incorrectly synthesized into exact raw JSONL history")
		}
	}
	model.width, model.height = 100, 30
	if view := model.View(); !strings.Contains(view, "APPROVAL REQUIRED") || !strings.Contains(view, pending.Title) {
		t.Fatalf("reattached actionable permission was not rendered:\n%s", view)
	}
	if backend.permissionReads["S-1"] != 1 {
		t.Fatalf("permission status reads = %d, want 1", backend.permissionReads["S-1"])
	}
}

func TestPermissionSnapshotIsDiscardedAfterNewerPermissionFrame(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = sessionView
	model.direct = true
	model.current = session.Session{ID: "S-1", TaskID: "T-1", Agent: "codex", Role: "worker"}
	model.cursors["S-1"] = 5
	model.receivePermissionSnapshot("S-1", appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{{
			ID: "permission-stale", RequestID: "REQ-stale", Title: "Stale", Kind: "edit",
			Options: []worker.PermissionOption{{ID: "allow-stale", Name: "Allow", Kind: "allow_once"}},
		}},
		Sequence: 8, DecisionsReady: true,
	})
	if model.pendingPermissionSnapshot == nil {
		t.Fatal("snapshot ahead of the local replay cursor was not queued")
	}
	model.addActivity("S-1", testLine(9, "REQ-stale", worker.FramePermissionResolved, map[string]any{
		"permission_id": "permission-stale", "decision": "allow-stale",
	}), false)
	if model.pendingPermissionSnapshot != nil {
		t.Fatal("snapshot older than a permission frame watermark was not discarded")
	}
	if projection := model.projections["S-1"]; projection != nil {
		if _, card := projection.pendingPermission(); card != nil {
			t.Fatalf("stale snapshot resurrected a resolved permission: %#v", card)
		}
	}
}

func TestPermissionSnapshotRetiresDisappearedWaitingCardAtWatermark(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = sessionView
	model.direct = true
	model.current = session.Session{ID: "S-1", TaskID: "T-1", Agent: "codex", Role: "worker"}
	model.addActivity("S-1", testLine(1, "REQ-current", worker.FramePermissionRequested, worker.PendingPermission{
		ID: "permission-gone", RequestID: "REQ-current", Title: "Gone", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-gone", Name: "Allow", Kind: "allow_once"}},
	}), false)
	if _, card := model.projections["S-1"].pendingPermission(); card == nil {
		t.Fatal("test setup did not project the waiting permission")
	}
	model.receivePermissionSnapshot("S-1", appservice.PermissionSnapshot{Permissions: []worker.PendingPermission{}, Sequence: 1, DecisionsReady: true})
	if _, card := model.projections["S-1"].pendingPermission(); card != nil {
		t.Fatalf("authoritative snapshot did not retire disappeared permission: %#v", card)
	}
}

func TestPermissionSnapshotReadinessSuppressesBlockedDecision(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = sessionView
	model.direct = true
	model.current = session.Session{ID: "S-1", TaskID: "T-1", Agent: "codex", Role: "worker"}
	pending := worker.PendingPermission{
		ID: "permission-blocked-exact", RequestID: "REQ-blocked", Title: "Blocked approval", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-blocked-exact", Name: "Allow once", Kind: "allow_once"}},
	}
	model.projections["S-1"] = newTranscript()
	model.projections["S-1"].reconcilePendingPermissions([]worker.PendingPermission{pending})
	model.updateProjectionRetention("S-1")
	model.receivePermissionSnapshot("S-1", appservice.PermissionSnapshot{
		Permissions:    []worker.PendingPermission{pending},
		DecisionsReady: false, BlockedCode: "worker_starting", BlockedMessage: strings.Repeat("not ready ", maxProjectedErrorBytes),
	})
	if model.permissionDecisionsReady["S-1"] || len(model.permissionBlockedReason["S-1"]) > maxProjectedErrorBytes {
		t.Fatalf("bounded permission readiness = ready %v reason bytes %d", model.permissionDecisionsReady["S-1"], len(model.permissionBlockedReason["S-1"]))
	}
	updated, command := model.confirmPermission(0)
	model = updated.(Model)
	if command != nil || model.confirm != nil || !strings.Contains(model.notice, "Permission decision unavailable") {
		t.Fatalf("blocked permission dispatch = command %v confirm %#v notice %q", command != nil, model.confirm, model.notice)
	}
	model.confirm = &confirmation{
		kind: "permission", sessionID: "S-1", permissionID: "permission-blocked-exact", optionID: "allow-blocked-exact",
	}
	updated, command = model.handleConfirmation("y")
	model = updated.(Model)
	if command != nil || model.confirm != nil || !strings.Contains(model.notice, "Permission decision unavailable") {
		t.Fatalf("readiness changed after confirmation = command %v confirm %#v notice %q", command != nil, model.confirm, model.notice)
	}
	model.width, model.height = 100, 30
	view := model.View()
	if !strings.Contains(view, "Decision unavailable") || strings.Contains(view, "[1] Allow once") || strings.Contains(view, "choose approval") {
		t.Fatalf("blocked permission was rendered as actionable:\n%s", view)
	}
}

func TestDelayedGenerationlessSnapshotCannotRetireResumedPermission(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = sessionView
	model.direct = true
	model.current = session.Session{ID: "S-1", TaskID: "T-1", Agent: "codex", Role: "worker", Status: session.StatusStopped}
	delayedOffline := appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{}, DecisionsReady: false,
		BlockedCode: "worker_offline", BlockedMessage: "The previous worker was offline.",
	}
	resumed := model.current
	resumed.Status = session.StatusWorking
	resumed.WorkerSocket = "/tmp/resumed-worker.sock"
	updated, _ := model.handleAction(actionMsg{kind: "resume", sessionID: "S-1", current: resumed})
	model = updated.(Model)
	pending := worker.PendingPermission{
		ID: "permission-resumed-exact", RequestID: "REQ-resumed", Title: "Current approval", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-resumed-exact", Name: "Allow once", Kind: "allow_once"}},
	}
	model.addActivity("S-1", testLine(1, pending.RequestID, worker.FramePermissionRequested, pending), false)
	if !model.permissionDecisionsReady["S-1"] {
		t.Fatal("current resumed permission frame did not establish decision readiness")
	}
	model.receivePermissionSnapshot("S-1", delayedOffline)
	_, card := model.projections["S-1"].pendingPermission()
	if card == nil || card.id != pending.ID || !model.permissionDecisionsReady["S-1"] {
		t.Fatalf("delayed generationless snapshot retired/disabled resumed permission: card %#v ready %v", card, model.permissionDecisionsReady["S-1"])
	}
	if model.permissionBlockedReason["S-1"] != "" {
		t.Fatalf("delayed generationless snapshot replaced current readiness: %q", model.permissionBlockedReason["S-1"])
	}
}

func TestPermissionSnapshotRejectsDelayedLowerAuthoritySequence(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = sessionView
	model.direct = true
	model.current = session.Session{ID: "S-1", TaskID: "T-1", Agent: "codex", Role: "worker"}
	model.cursors["S-1"] = 100
	model.receivePermissionSnapshot("S-1", appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{}, Sequence: 100, DecisionsReady: false,
		BlockedCode: "channel_failed", BlockedMessage: "The Session channel is unavailable.",
	})
	if model.permissionSnapshotCursors["S-1"] != 100 || model.permissionDecisionsReady["S-1"] {
		t.Fatalf("newer blocked authority = sequence %d ready %v", model.permissionSnapshotCursors["S-1"], model.permissionDecisionsReady["S-1"])
	}
	model.receivePermissionSnapshot("S-1", appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{{
			ID: "permission-from-delayed-read", RequestID: "REQ-delayed", Title: "Stale approval", Kind: "edit",
			Options: []worker.PermissionOption{{ID: "allow-delayed", Name: "Allow", Kind: "allow_once"}},
		}},
		Sequence: 80, DecisionsReady: true,
	})
	if model.permissionSnapshotCursors["S-1"] != 100 || model.permissionDecisionsReady["S-1"] {
		t.Fatalf("delayed authority changed sequence/readiness = %d/%v", model.permissionSnapshotCursors["S-1"], model.permissionDecisionsReady["S-1"])
	}
	if model.projections["S-1"] != nil {
		t.Fatalf("delayed lower-watermark snapshot resurrected projection: %#v", model.projections["S-1"])
	}
	if reason := model.permissionBlockedReason["S-1"]; reason != "The Session channel is unavailable." {
		t.Fatalf("delayed authority replaced bounded blocked reason: %q", reason)
	}
}

func TestTaskRefreshReconcilesSelectedSessionPermissionSnapshot(t *testing.T) {
	backend := newFakeConsoleBackend()
	second := session.Session{ID: "S-2", TaskID: "T-1", Agent: "claude", Provider: "anthropic", Role: "worker", Status: session.StatusWorking}
	backend.detail.Sessions = append(backend.detail.Sessions, second)
	backend.sessions[second.ID] = second
	backend.permissionState[second.ID] = appservice.PermissionSnapshot{
		Permissions: []worker.PendingPermission{{
			ID: "permission-selected-exact", RequestID: "REQ-selected", Title: "Approve selected Session", Kind: "execute",
			Options: []worker.PermissionOption{{ID: "allow-selected-exact", Name: "Allow once", Kind: "allow_once"}},
		}},
		Sequence: 1, DecisionsReady: true,
	}
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	model.moveTaskSelection(1)
	model.cursors[second.ID] = 1
	updated, _ := model.Update(model.refreshCmd()())
	model = updated.(Model)
	projection := model.projections[second.ID]
	_, card := projection.pendingPermission()
	if card == nil || card.id != "permission-selected-exact" || card.options[0].id != "allow-selected-exact" {
		t.Fatalf("selected Task Session permission = %#v", card)
	}
	if backend.permissionReads[second.ID] != 1 {
		t.Fatalf("selected Session permission reads = %d, want 1", backend.permissionReads[second.ID])
	}
	model.width, model.height = 120, 35
	if view := model.View(); !strings.Contains(view, "Approve selected Session") || !strings.Contains(view, "● Approval") {
		t.Fatalf("selected Task preview did not surface the authoritative permission:\n%s", view)
	}
}

func waitForTaskWatcherStop(t *testing.T, manager *watchManager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		manager.mu.Lock()
		live := manager.taskLive
		manager.mu.Unlock()
		if !live {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Task watcher did not stop after cancellation")
}

func TestLiveFrameAppendsWithoutHydrationOrReplacement(t *testing.T) {
	backend := newFakeConsoleBackend()
	model := baseModel(context.Background(), backend)
	model.mode = sessionView
	model.current = backend.sessions["S-1"]
	model.addActivity("S-1", testLine(2, "REQ-live", worker.FrameProviderEvent, map[string]any{
		"type": "text_delta", "stream": "output", "text": "streamed now",
	}), true)
	if model.cursors["S-1"] != 2 || len(model.histories["S-1"]) != 1 {
		t.Fatalf("live frame state = cursor %d, history %d", model.cursors["S-1"], len(model.histories["S-1"]))
	}
	request := model.projections["S-1"].byID["REQ-live"]
	if request == nil || request.state != turnLive || request.blocks[0].text != "streamed now" {
		t.Fatalf("live projection = %#v", request)
	}
}

func TestModelBoundsRawChannelFrameRetention(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	for sequence := 1; sequence <= maxRetainedChannelFrames+73; sequence++ {
		model.addActivity("S-bounded", testLine(uint64(sequence), "", worker.FrameConnection, map[string]any{"state": "connected"}), false)
	}
	if got := len(model.histories["S-bounded"]); got != maxRetainedChannelFrames {
		t.Fatalf("retained raw frames = %d, want %d", got, maxRetainedChannelFrames)
	}
	if !model.historyClipped["S-bounded"] || model.cursors["S-bounded"] != maxRetainedChannelFrames+73 {
		t.Fatalf("bounded raw state = clipped %v cursor %d", model.historyClipped["S-bounded"], model.cursors["S-bounded"])
	}
	model.addActivity("S-bounded", testLine(1, "", worker.FrameConnection, map[string]any{"state": "connected"}), false)
	if len(model.histories["S-bounded"]) != maxRetainedChannelFrames {
		t.Fatal("old duplicate expanded bounded raw history")
	}

	large := strings.Repeat("x", 700<<10)
	for sequence := 1; sequence <= 8; sequence++ {
		model.addActivity("S-byte-bounded", testLine(uint64(sequence), "", worker.FrameProviderEvent, map[string]any{
			"type": "future_large_event", "payload": large,
		}), false)
	}
	if model.historyBytes["S-byte-bounded"] > maxRetainedChannelBytes || len(model.histories["S-byte-bounded"]) >= 8 || !model.historyClipped["S-byte-bounded"] {
		t.Fatalf("byte-bounded raw history = bytes %d frames %d clipped %v", model.historyBytes["S-byte-bounded"], len(model.histories["S-byte-bounded"]), model.historyClipped["S-byte-bounded"])
	}
}

func TestModelBoundsRawChannelRetentionAcrossSessions(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	const sessionCount = 64
	framesPerSession := maxRetainedChannelTotalFrames/sessionCount + 8
	for sessionIndex := range sessionCount {
		sessionID := fmt.Sprintf("S-global-%03d", sessionIndex)
		for sequence := 1; sequence <= framesPerSession; sequence++ {
			model.addActivity(sessionID, testLine(uint64(sequence), "", worker.FrameConnection, map[string]any{
				"state": "connected",
			}), false)
		}
	}
	if model.retainedHistoryFrames != maxRetainedChannelTotalFrames {
		t.Fatalf("global retained frames = %d, want %d", model.retainedHistoryFrames, maxRetainedChannelTotalFrames)
	}
	if !model.historyClipped["S-global-000"] {
		t.Fatal("oldest Session did not disclose global frame eviction")
	}
	assertRetainedHistoryAccounting(t, &model)

	largePayload := strings.Repeat("z", 512<<10)
	var lastLine sessionlog.Line
	for sessionIndex := range sessionCount {
		sessionID := fmt.Sprintf("S-large-%03d", sessionIndex)
		lastLine = testLine(1, "", worker.FrameProviderEvent, map[string]any{
			"type": "future_large_event", "payload": largePayload, "session": sessionID,
		})
		model.addActivity(sessionID, lastLine, false)
	}
	if model.retainedHistoryBytes > maxRetainedChannelTotalBytes {
		t.Fatalf("global retained bytes = %d, limit %d", model.retainedHistoryBytes, maxRetainedChannelTotalBytes)
	}
	if !model.historyClipped["S-large-000"] {
		t.Fatal("oldest large Session did not disclose global byte eviction")
	}
	latest := model.histories["S-large-063"]
	if len(latest) != 1 || string(latest[0].line.Data) != string(lastLine.Data) {
		t.Fatal("newest retained raw JSONL frame was altered")
	}
	assertRetainedHistoryAccounting(t, &model)
}

func TestWatchManagerQueuesAtMostOneFramePayload(t *testing.T) {
	manager := newWatchManager(context.Background(), newFakeConsoleBackend())
	defer manager.cancel()
	if got := cap(manager.events); got != 1 {
		t.Fatalf("watch event handoff capacity = %d, want 1", got)
	}
}

func TestModelBoundsProjectionBytesAcrossTaskSessions(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = taskView
	model.task.Task = task.Task{ID: "T-projection-bytes", Title: "Bound projection bytes", Status: task.StatusWorking}
	prompt := strings.Repeat("p", maxProjectedMessageBytes)
	const sessionCount = 140
	for index := 0; index < sessionCount; index++ {
		sessionID := fmt.Sprintf("S-projection-bytes-%03d", index)
		model.task.Sessions = append(model.task.Sessions, session.Session{ID: sessionID, TaskID: model.task.Task.ID, Agent: "codex", Role: "worker"})
		model.addActivity(sessionID, testLine(1, "REQ", worker.FrameClientCommand, map[string]any{
			"command": "prompt", "text": prompt,
		}), false)
	}
	if model.retainedProjectionBytes > maxRetainedProjectionBytes || model.retainedProjectionObjects > maxRetainedProjectionObjects {
		t.Fatalf("projection totals exceed budget: %d bytes/%d objects", model.retainedProjectionBytes, model.retainedProjectionObjects)
	}
	if !model.projectionClipped["S-projection-bytes-000"] || model.projections["S-projection-bytes-000"] != nil {
		t.Fatal("oldest Task projection was not evicted with clipping disclosure")
	}
	latestID := fmt.Sprintf("S-projection-bytes-%03d", sessionCount-1)
	if model.projections[latestID] == nil || len(model.histories[latestID]) != 1 {
		t.Fatal("newest projection or independently retained raw frame was lost")
	}
	assertProjectionAccounting(t, &model)
	model.width, model.height = 120, 32
	view := model.View()
	for _, want := range []string{"projection(s) clipped", "Projection clipped"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Task projection disclosure missing %q:\n%s", want, view)
		}
	}
}

func TestModelBoundsProjectionObjectsAcrossToolHeavySessions(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = taskView
	const sessionCount = 300
	for sessionIndex := 0; sessionIndex < sessionCount; sessionIndex++ {
		sessionID := fmt.Sprintf("S-projection-objects-%03d", sessionIndex)
		model.addActivity(sessionID, testLine(1, "REQ", worker.FrameClientCommand, map[string]any{
			"command": "prompt", "text": "inspect",
		}), false)
		for toolIndex := 0; toolIndex < 40; toolIndex++ {
			model.addActivity(sessionID, testLine(uint64(toolIndex+2), "REQ", worker.FrameProviderEvent, map[string]any{
				"type": "tool_call", "tool_call": map[string]any{
					"id": fmt.Sprintf("tool-%d", toolIndex), "title": "Inspect", "kind": "read", "status": "completed",
				},
			}), false)
		}
	}
	if model.retainedProjectionObjects > maxRetainedProjectionObjects || model.retainedProjectionBytes > maxRetainedProjectionBytes {
		t.Fatalf("tool-heavy projection totals exceed budget: %d bytes/%d objects", model.retainedProjectionBytes, model.retainedProjectionObjects)
	}
	if !model.projectionClipped["S-projection-objects-000"] {
		t.Fatal("tool-heavy Task did not evict its oldest projection")
	}
	assertProjectionAccounting(t, &model)
}

func TestProjectionEvictionNeverMutatesRawChannelRetention(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	line := testLine(1, "REQ", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "exact"})
	model.addActivity("S-independent-raw", line, false)
	model.dropProjection("S-independent-raw")
	history := model.histories["S-independent-raw"]
	if len(history) != 1 || string(history[0].line.Data) != string(line.Data) {
		t.Fatal("projection eviction mutated independently retained raw JSONL")
	}
	if !model.projectionClipped["S-independent-raw"] {
		t.Fatal("projection eviction was not disclosed")
	}
}

func TestProjectionBudgetPreservesPermissionThatCrossesLimit(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = taskView
	const sessionID = "S-permission-crossing"
	sequence := uint64(1)
	requestIndex := 0
	requestID := fmt.Sprintf("REQ-%03d", requestIndex)
	model.addActivity(sessionID, testLine(sequence, requestID, worker.FrameClientCommand, map[string]any{
		"command": "prompt", "text": "inspect before approval",
	}), false)

	pending := worker.PendingPermission{
		ID: "permission-exact", RequestID: requestID, Title: strings.Repeat("A", 512), Kind: "edit",
		Options: []worker.PermissionOption{
			{ID: "allow-once-exact", Name: strings.Repeat("n", 512), Kind: "allow_once"},
			{ID: "allow-always-exact", Name: strings.Repeat("n", 512), Kind: "allow_always"},
			{ID: "reject-once-exact", Name: strings.Repeat("n", 512), Kind: "reject_once"},
			{ID: "reject-always-exact", Name: strings.Repeat("n", 512), Kind: "reject_always"},
		},
	}
	permissionIncrement := projectedPermissionIncrement(pending)
	fullToolIncrement := projectedToolIncrement(maxToolDetailBytes)
	toolIndex := 0
	for {
		remaining := maxRetainedProjectionBytes - model.retainedProjectionBytes
		if remaining < permissionIncrement {
			break
		}
		if toolIndex == maxProjectedTools {
			requestIndex++
			requestID = fmt.Sprintf("REQ-%03d", requestIndex)
			sequence++
			model.addActivity(sessionID, testLine(sequence, requestID, worker.FrameClientCommand, map[string]any{
				"command": "prompt", "text": "continue inspection",
			}), false)
			toolIndex = 0
			continue
		}
		contentBytes := maxToolDetailBytes
		if remaining < fullToolIncrement+permissionIncrement/2 {
			targetIncrement := remaining - permissionIncrement/2
			contentBytes = projectedToolContentForIncrement(targetIncrement)
			if contentBytes <= 0 {
				break
			}
		}
		sequence++
		toolID := fmt.Sprintf("tool-%026d", sequence)
		model.addActivity(sessionID, testLine(sequence, requestID, worker.FrameProviderEvent, map[string]any{
			"type": "tool_call", "tool_call": map[string]any{
				"id": toolID, "title": "Inspect", "kind": "read", "status": "completed",
				"content": strings.Repeat("x", contentBytes),
			},
		}), false)
		toolIndex++
		if contentBytes < maxToolDetailBytes {
			break
		}
	}
	if model.projections[sessionID] == nil {
		t.Fatal("near-budget projection was evicted before the permission frame")
	}
	remaining := maxRetainedProjectionBytes - model.retainedProjectionBytes
	if remaining >= permissionIncrement {
		t.Fatalf("test setup did not make permission cross budget: remaining %d increment %d", remaining, permissionIncrement)
	}
	sequence++
	pending.RequestID = requestID
	permissionLine := testLine(sequence, requestID, worker.FramePermissionRequested, pending)
	model.addActivity(sessionID, permissionLine, false)

	projection := model.projections[sessionID]
	if projection == nil {
		t.Fatal("crossing permission evicted its own projection")
	}
	_, card := projection.pendingPermission()
	if card == nil || card.id != pending.ID || len(card.options) != len(pending.Options) {
		t.Fatalf("preserved permission = %#v", card)
	}
	for index, option := range card.options {
		if option.id != pending.Options[index].ID {
			t.Fatalf("preserved option %d id = %q, want %q", index, option.id, pending.Options[index].ID)
		}
	}
	if model.retainedProjectionBytes > maxRetainedProjectionBytes || model.retainedProjectionObjects > maxRetainedProjectionObjects {
		t.Fatalf("permission compaction exceeded projection budget: %d/%d", model.retainedProjectionBytes, model.retainedProjectionObjects)
	}
	if !model.projectionClipped[sessionID] || !projection.clipped {
		t.Fatal("permission-preserving compaction was not disclosed")
	}
	history := model.histories[sessionID]
	if len(history) == 0 || history[len(history)-1].line.Type != worker.FramePermissionRequested ||
		string(history[len(history)-1].line.Data) != string(permissionLine.Data) {
		t.Fatal("permission-preserving compaction lost the exact raw permission frame")
	}
	assertProjectionAccounting(t, &model)
}

func TestProjectionBudgetBoundsManyPendingSessionsAndKeepsRetainedIDsExact(t *testing.T) {
	model := baseModel(context.Background(), newFakeConsoleBackend())
	model.mode = taskView
	const sessionCount = 700
	options := make([]worker.PermissionOption, 0, maxProjectedPermissionOptions)
	for index := 0; index < maxProjectedPermissionOptions; index++ {
		options = append(options, worker.PermissionOption{
			ID: fmt.Sprintf("option-%02d-exact", index), Name: "Choose exact option", Kind: "allow_once",
		})
	}
	for index := 0; index < sessionCount; index++ {
		sessionID := fmt.Sprintf("S-pending-%03d", index)
		requestID := fmt.Sprintf("REQ-pending-%03d", index)
		permissionID := fmt.Sprintf("permission-%03d-exact", index)
		model.addActivity(sessionID, testLine(1, requestID, worker.FrameClientCommand, map[string]any{
			"command": "prompt", "text": "await approval",
		}), false)
		model.addActivity(sessionID, testLine(2, requestID, worker.FramePermissionRequested, worker.PendingPermission{
			ID: permissionID, RequestID: requestID, Title: "Approve exact action", Kind: "edit", Options: options,
		}), false)
		if index == 511 {
			if len(model.projections) != 512 || len(model.projectionOverflow) != 0 {
				t.Fatalf("conforming 512-Session Task retained %d projections with %d overflows", len(model.projections), len(model.projectionOverflow))
			}
		}
	}
	if model.retainedProjectionBytes > maxRetainedProjectionBytes || model.retainedProjectionObjects > maxRetainedProjectionObjects {
		t.Fatalf("pending-only projection totals exceed budget: %d/%d", model.retainedProjectionBytes, model.retainedProjectionObjects)
	}
	if len(model.projectionOverflow) == 0 {
		t.Fatal("nonconforming over-512 Task did not enter explicit projection overflow mode")
	}
	for sessionID, projection := range model.projections {
		_, card := projection.pendingPermission()
		if card == nil {
			t.Fatalf("retained %s projection lost its pending permission", sessionID)
		}
		wantID := "permission-" + strings.TrimPrefix(sessionID, "S-pending-") + "-exact"
		if card.id != wantID || len(card.options) != len(options) {
			t.Fatalf("retained %s permission = id %q/options %d, want %q/%d", sessionID, card.id, len(card.options), wantID, len(options))
		}
		for index, option := range card.options {
			if option.id != options[index].ID {
				t.Fatalf("retained %s option %d = %q, want %q", sessionID, index, option.id, options[index].ID)
			}
		}
	}
	assertProjectionAccounting(t, &model)
	for sessionID := range model.projectionOverflow {
		model.mode = sessionView
		model.current = session.Session{ID: sessionID, TaskID: "T-overflow", Agent: "codex", Role: "worker"}
		model.width, model.height = 100, 30
		if view := model.View(); !strings.Contains(view, "PROJECTION LIMIT") || !strings.Contains(view, "attach this exact") {
			t.Fatalf("projection overflow was not disclosed for %s:\n%s", sessionID, view)
		}
		break
	}
}

func projectedPermissionIncrement(pending worker.PendingPermission) int {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "inspect"}))
	before := projection.retainedUsage().bytes
	projection.apply(testLine(2, "REQ", worker.FramePermissionRequested, pending))
	return projection.retainedUsage().bytes - before
}

func projectedToolIncrement(contentBytes int) int {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "inspect"}))
	before := projection.retainedUsage().bytes
	projection.apply(testLine(2, "REQ", worker.FrameProviderEvent, map[string]any{
		"type": "tool_call", "tool_call": map[string]any{
			"id": "tool-00000000000000000000000002", "title": "Inspect", "kind": "read", "status": "completed",
			"content": strings.Repeat("x", contentBytes),
		},
	}))
	return projection.retainedUsage().bytes - before
}

func projectedToolContentForIncrement(target int) int {
	largeOverhead := projectedToolIncrement(maxToolPreviewBytes+1) - (maxToolPreviewBytes + 1)
	if contentBytes := target - largeOverhead; contentBytes > maxToolPreviewBytes {
		return min(contentBytes, maxToolDetailBytes)
	}
	smallOverhead := projectedToolIncrement(1) - 2
	return max(0, min(maxToolPreviewBytes, (target-smallOverhead)/2))
}

func assertRetainedHistoryAccounting(t *testing.T, model *Model) {
	t.Helper()
	var frames, bytes int
	for sessionID, history := range model.histories {
		var sessionBytes int
		for _, retained := range history {
			frames++
			bytes += retained.bytes
			sessionBytes += retained.bytes
			if retained.bytes != retainedChannelLineBytes(retained.line) {
				t.Fatalf("%s retained byte accounting = %d, want %d", sessionID, retained.bytes, retainedChannelLineBytes(retained.line))
			}
		}
		if model.historyBytes[sessionID] != sessionBytes {
			t.Fatalf("%s history bytes = %d, want %d", sessionID, model.historyBytes[sessionID], sessionBytes)
		}
	}
	if frames != model.retainedHistoryFrames || bytes != model.retainedHistoryBytes {
		t.Fatalf("global history accounting = %d frames/%d bytes, want %d/%d", model.retainedHistoryFrames, model.retainedHistoryBytes, frames, bytes)
	}
	if frames > maxRetainedChannelTotalFrames || bytes > maxRetainedChannelTotalBytes {
		t.Fatalf("global history exceeds limits: %d frames/%d bytes", frames, bytes)
	}
}

func assertProjectionAccounting(t *testing.T, model *Model) {
	t.Helper()
	var bytes, objects int
	for sessionID, projection := range model.projections {
		usage := projection.retainedUsage()
		bytes += usage.bytes
		objects += usage.objects
		if model.projectionBytes[sessionID] != usage.bytes || model.projectionObjects[sessionID] != usage.objects {
			t.Fatalf("%s projection accounting = %d/%d, want %d/%d", sessionID,
				model.projectionBytes[sessionID], model.projectionObjects[sessionID], usage.bytes, usage.objects)
		}
	}
	if bytes != model.retainedProjectionBytes || objects != model.retainedProjectionObjects {
		t.Fatalf("global projection accounting = %d bytes/%d objects, want %d/%d", model.retainedProjectionBytes, model.retainedProjectionObjects, bytes, objects)
	}
}

func TestTaskEnterOpensStructuredSessionChannel(t *testing.T) {
	backend := newFakeConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if model.mode != sessionView || model.current.ID != "S-1" {
		t.Fatalf("Enter did not open selected Session: mode=%v current=%q", model.mode, model.current.ID)
	}
	if strings.Contains(strings.ToLower(model.View()), "native cli") {
		t.Fatalf("managed Session view still advertises native attachment:\n%s", model.View())
	}
}

func TestBusyPromptRestoresDraftInsteadOfQueueing(t *testing.T) {
	backend := newFakeConsoleBackend()
	backend.promptErr = &worker.BusyError{SessionID: "S-1", ActiveRequestID: "REQ-active"}
	model := baseModel(context.Background(), backend)
	model.mode = sessionView
	model.current = backend.sessions["S-1"]
	model.composer = composerState{focused: true, value: []rune("try this"), cursor: len("try this")}
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if command == nil {
		t.Fatal("prompt command was nil")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.composer.text() != "try this" || !model.composer.focused {
		t.Fatalf("busy prompt lost draft: %#v", model.composer)
	}
	if !strings.Contains(model.notice, "active provider turn") || strings.Contains(strings.ToLower(model.notice), "queued") {
		t.Fatalf("busy notice = %q", model.notice)
	}
}

func TestDetachNeverStopsOrCancelsSession(t *testing.T) {
	backend := newFakeConsoleBackend()
	model := baseModel(context.Background(), backend)
	model.mode = sessionView
	model.current = backend.sessions["S-1"]
	_, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if command == nil {
		t.Fatal("detach did not return Bubble Tea quit command")
	}
	if backend.stopped != 0 || backend.cancelled != 0 {
		t.Fatalf("detach mutated Session lifecycle: stopped=%d cancelled=%d", backend.stopped, backend.cancelled)
	}
}

func TestWizardUsesLiveProviderCatalogThenConfiguresSameSession(t *testing.T) {
	backend := newFakeConsoleBackend()
	model := baseModel(context.Background(), backend)
	model.mode = taskView
	model.task = backend.detail
	updated, _ := model.Update(wizardReadyMsg{agents: backend.profiles, defaultRepo: "/repo"})
	model = updated.(Model)
	if model.wizard == nil || model.wizard.connectedID != "" {
		t.Fatalf("initial wizard = %#v", model.wizard)
	}
	updated, command := model.submitWizard()
	model = updated.(Model)
	if command == nil {
		t.Fatal("connect command was nil")
	}
	updated, command = model.Update(command())
	model = updated.(Model)
	if command == nil || model.wizard.connectedID != "S-2" || model.wizard.providerOptionsReady {
		t.Fatalf("connecting wizard = %#v", model.wizard)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.wizard.connectedID != "S-2" || !model.wizard.providerOptionsReady {
		t.Fatalf("connected wizard = %#v", model.wizard)
	}
	view := model.View()
	for _, want := range []string{"Provider default (currently provider-model)", "Provider default (currently medium)", "nothing is hard-coded"} {
		if !strings.Contains(view, want) {
			t.Fatalf("provider catalog view missing %q:\n%s", want, view)
		}
	}
	model.wizard.modelIndex = 2
	model.wizard.effortIndex = 2
	model.wizard.prompt.value = []rune("Begin")
	updated, command = model.submitWizard()
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(backend.configured) != 1 || backend.configured[0].Model != "model-b" || backend.configured[0].ReasoningEffort != "high" {
		t.Fatalf("configured options = %#v", backend.configured)
	}
	if len(backend.launches) != 1 || backend.launches[0] != "Begin" || model.current.ID != "S-2" || model.wizard != nil {
		t.Fatalf("launches/model = %#v / current=%q wizard=%v", backend.launches, model.current.ID, model.wizard != nil)
	}
}
