package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/agents"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type fakeBackend struct {
	mu          sync.Mutex
	detail      task.Detail
	sessions    map[string]session.Session
	histories   map[string][]sessionlog.Line
	prompts     []promptCall
	permissions []permissionCall
	cancelled   []string
	resumed     []string
	stopped     []string
	starts      []appservice.StartSessionParams
	profiles    []agents.Profile
	promptState string
	native      []string
}

type promptCall struct {
	sessionID string
	message   string
}

type permissionCall struct {
	sessionID    string
	permissionID string
	optionID     string
	cancelled    bool
}

func (f *fakeBackend) Task(context.Context, string) (task.Detail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := f.detail
	result.Sessions = append([]session.Session(nil), f.detail.Sessions...)
	return result, nil
}

func (f *fakeBackend) Session(_ context.Context, id string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id], nil
}

func (f *fakeBackend) History(_ context.Context, id string, cursor uint64) ([]sessionlog.Line, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []sessionlog.Line
	for _, line := range f.histories[id] {
		if line.Seq > cursor {
			result = append(result, line)
		}
	}
	return result, nil
}

func (f *fakeBackend) Follow(ctx context.Context, _ string, _ uint64, _ func(sessionlog.Line) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeBackend) Prompt(_ context.Context, id, message string) (worker.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, promptCall{sessionID: id, message: message})
	state := f.promptState
	if state == "" {
		state = "ready"
	}
	return worker.Message{Type: "accepted", RequestID: "req-test", State: state}, nil
}

func (f *fakeBackend) DecidePermission(_ context.Context, sessionID, permissionID, optionID string, cancelled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions = append(f.permissions, permissionCall{
		sessionID: sessionID, permissionID: permissionID, optionID: optionID, cancelled: cancelled,
	})
	return nil
}

func (f *fakeBackend) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeBackend) Resume(_ context.Context, id string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed = append(f.resumed, id)
	current := f.sessions[id]
	current.WorkerSocket = "/tmp/" + id + ".sock"
	current.Status = session.StatusWaiting
	f.sessions[id] = current
	return current, nil
}

func (f *fakeBackend) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeBackend) AvailableAgents(context.Context) ([]agents.Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agents.Profile(nil), f.profiles...), nil
}

func (f *fakeBackend) StartSession(_ context.Context, params appservice.StartSessionParams) (appservice.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, params)
	created := session.Session{
		ID: "S-3", TaskID: params.TaskID, Role: params.Role, Agent: params.Agent, Provider: "test",
		RepoPath: params.RepoPath, WorktreePath: "/worktrees/three", Branch: "agent/S-3",
		Status: session.StatusWorking, WorkerSocket: "/tmp/S-3.sock",
	}
	f.sessions[created.ID] = created
	f.detail.Sessions = append(f.detail.Sessions, created)
	return appservice.LaunchResult{Session: created, RequestID: "REQ-3", State: "ready"}, nil
}

func (f *fakeBackend) AttachNative(_ context.Context, id string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.native = append(f.native, id)
	return nil
}

func TestTaskConsoleShowsEverySessionAndDetachesOnlyViewer(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	view := model.View()
	for _, want := range []string{"TASK T-1", "S-1", "researcher", "GEMINI", "one", "S-2", "implementer", "CODEX", "two", "waiting", "No conversation yet"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Task view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Aggregate activity") || strings.Contains(view, "worker ready") {
		t.Fatalf("Task view leaked the old aggregate event feed:\n%s", view)
	}

	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	model = updated.(Model)
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatalf("q command = %T, want tea.QuitMsg", command())
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.cancelled) != 0 || len(backend.stopped) != 0 {
		t.Fatalf("detach mutated worker: cancelled=%v stopped=%v", backend.cancelled, backend.stopped)
	}
}

func TestTaskConsoleKeyboardMouseAndExplicitPromptTarget(t *testing.T) {
	backend := newConsoleBackend()
	backend.promptState = "queued"
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	if model.selected != 1 {
		t.Fatalf("keyboard selected = %d, want 1", model.selected)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	model = updated.(Model)
	if model.mode != sessionView || model.current.ID != "S-2" || !model.composer.focused {
		t.Fatalf("prompt drill = mode %d, Session %s, input %v", model.mode, model.current.ID, model.composer.focused)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("steer only this")})
	model = updated.(Model)
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	result := command()
	updated, _ = model.Update(result)
	model = updated.(Model)
	if !strings.Contains(model.notice, "Message queued") {
		t.Fatalf("queued notice = %q", model.notice)
	}
	backend.mu.Lock()
	if len(backend.prompts) != 1 || backend.prompts[0] != (promptCall{sessionID: "S-2", message: "steer only this"}) {
		backend.mu.Unlock()
		t.Fatalf("prompt calls = %#v", backend.prompts)
	}
	backend.mu.Unlock()

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	model = updated.(Model)
	updated, nativeCommand := model.Update(tea.MouseMsg(tea.MouseEvent{X: 2, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
	model = updated.(Model)
	if model.mode != taskView || model.selected != 0 || nativeCommand == nil {
		t.Fatalf("mouse native attach: selected=%d mode=%d command=%v", model.selected, model.mode, nativeCommand)
	}
}

func TestTaskConsoleNativeDefaultACPActionFilterHelpAndPalette(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	updated, nativeCommand := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if model.mode != taskView || nativeCommand == nil || !strings.Contains(model.notice, "native gemini harness") {
		t.Fatalf("Enter did not choose native harness: mode=%d command=%v notice=%q", model.mode, nativeCommand, model.notice)
	}
	direct := &nativeAttachCommand{ctx: context.Background(), backend: backend, sessionID: "S-1", input: strings.NewReader(""), output: io.Discard, stderr: io.Discard}
	if err := direct.Run(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	if !reflect.DeepEqual(backend.native, []string{"S-1"}) {
		backend.mu.Unlock()
		t.Fatalf("native calls = %v", backend.native)
	}
	backend.mu.Unlock()

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	if model.mode != sessionView || model.current.ID != "S-1" || model.composer.focused {
		t.Fatalf("ACP action = mode %d Session %s composer=%v", model.mode, model.current.ID, model.composer.focused)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	model = updated.(Model)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("codex")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if model.overlay != noOverlay || model.filter != "codex" || model.selected != 1 || len(model.filteredSessionIndices()) != 1 {
		t.Fatalf("filter state: overlay=%d filter=%q selected=%d matches=%v", model.overlay, model.filter, model.selected, model.filteredSessionIndices())
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	model = updated.(Model)
	if model.overlay != helpOverlay || !strings.Contains(model.View(), "KEYBOARD HELP") || !strings.Contains(model.View(), "Native harness") {
		t.Fatalf("help overlay:\n%s", model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("new")})
	model = updated.(Model)
	if model.overlay != paletteOverlay || !strings.Contains(model.View(), "New Session") || strings.Contains(model.View(), "Cancel turn") {
		t.Fatalf("filtered command palette:\n%s", model.View())
	}
}

func TestInteractivePromptPreservesSpaceKeyInput(t *testing.T) {
	for _, attachment := range []string{"task", "session"} {
		t.Run(attachment, func(t *testing.T) {
			backend := newConsoleBackend()
			var model Model
			var cancel context.CancelFunc
			var err error
			if attachment == "task" {
				model, cancel, err = newTaskModel(context.Background(), backend, "T-1")
			} else {
				model, cancel, err = newSessionModel(context.Background(), backend, "S-1")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()

			applyKey := func(key tea.KeyMsg) tea.Cmd {
				updated, command := model.Update(key)
				model = updated.(Model)
				return command
			}
			applyKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
			for _, key := range []tea.KeyMsg{
				{Type: tea.KeySpace, Runes: []rune{' '}},
				{Type: tea.KeySpace, Runes: []rune{' '}},
				{Type: tea.KeyRunes, Runes: []rune("T05x")},
				{Type: tea.KeyBackspace},
				{Type: tea.KeySpace, Runes: []rune{' '}},
				{Type: tea.KeyRunes, Runes: []rune("targeted")},
				{Type: tea.KeySpace, Runes: []rune{' '}},
				{Type: tea.KeySpace, Runes: []rune{' '}},
				{Type: tea.KeyRunes, Runes: []rune("only")},
			} {
				applyKey(key)
			}
			command := applyKey(tea.KeyMsg{Type: tea.KeyEnter})
			if command == nil {
				t.Fatal("Enter did not submit the prompt")
			}
			updated, _ := model.Update(command())
			model = updated.(Model)

			backend.mu.Lock()
			prompts := append([]promptCall(nil), backend.prompts...)
			backend.mu.Unlock()
			want := promptCall{sessionID: "S-1", message: "  T05 targeted  only"}
			if len(prompts) != 1 || prompts[0] != want {
				t.Fatalf("prompt calls = %#v, want %#v", prompts, []promptCall{want})
			}
			if model.composer.focused || len(model.composer.value) != 0 || !strings.Contains(model.notice, "accepted") {
				t.Fatalf("submitted input state: mode=%v input=%q notice=%q", model.composer.focused, model.composer.value, model.notice)
			}
		})
	}
}

func TestSessionConsoleReplaysThenDeduplicatesLiveAndTogglesRaw(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if got := len(model.histories["S-1"]); got != 1 {
		t.Fatalf("stored history = %d lines, want 1", got)
	}

	duplicate := backend.histories["S-1"][0]
	updated, command := model.Update(activityMsg{sessionID: "S-1", line: duplicate})
	model = updated.(Model)
	if command == nil {
		t.Fatal("live update did not continue subscription")
	}
	live := projectionACPLine(2, "REQ-LIVE", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "live output", "message-live")))
	updated, _ = model.Update(activityMsg{sessionID: "S-1", line: live})
	model = updated.(Model)
	if got := len(model.histories["S-1"]); got != 2 {
		t.Fatalf("history after duplicate + live = %d, want 2", got)
	}
	if !strings.Contains(model.View(), "live output") {
		t.Fatalf("normalized view missing live output:\n%s", model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	model = updated.(Model)
	if !model.raw || !strings.Contains(model.View(), `"seq":2`) {
		t.Fatalf("raw view not enabled:\n%s", model.View())
	}
}

func TestEscapeLeavesComposerWithoutCancellingAndExplicitCancelPreservesDraft(t *testing.T) {
	backend := newConsoleBackend()
	current := backend.sessions["S-1"]
	current.Status = session.StatusWorking
	backend.sessions["S-1"] = current
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("continue differently")})
	model = updated.(Model)
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if command != nil {
		t.Fatal("Esc inside the composer unexpectedly started an action")
	}
	backend.mu.Lock()
	cancelled := append([]string(nil), backend.cancelled...)
	backend.mu.Unlock()
	if len(cancelled) != 0 {
		t.Fatalf("Esc inside the composer cancelled the turn: %v", cancelled)
	}
	if model.composer.focused || model.composer.text() != "continue differently" {
		t.Fatalf("draft was not preserved: mode=%v input=%q", model.composer.focused, model.composer.text())
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	model = updated.(Model)
	if model.confirm == nil || !strings.Contains(model.View(), "CONFIRM") {
		t.Fatalf("cancel did not require confirmation: %#v", model.confirm)
	}
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	backend.mu.Lock()
	cancelled = append([]string(nil), backend.cancelled...)
	backend.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "S-1" {
		t.Fatalf("explicit cancellation = %v", cancelled)
	}
	if model.composer.text() != "continue differently" {
		t.Fatalf("explicit cancellation discarded draft: %q", model.composer.text())
	}
	if !strings.Contains(model.notice, "does not pause") {
		t.Fatalf("cancellation notice = %q", model.notice)
	}
}

func TestSessionHarnessChoosesExactOfferedPermissionAndHighlightsAlways(t *testing.T) {
	backend := newConsoleBackend()
	requestID := "REQ-APPROVAL"
	backend.histories["S-1"] = []sessionlog.Line{
		projectionLine(1, requestID, "prompt_submitted", map[string]any{"display_message": "Make a safe change."}),
		projectionLine(2, requestID, "turn_started", map[string]any{}),
		projectionACPLine(3, requestID, "session_request_permission", map[string]any{
			"jsonrpc": "2.0", "id": "permission-7", "method": "session/request_permission", "params": map[string]any{
				"sessionId": "native", "toolCall": map[string]any{"toolCallId": "tool-write", "title": "Write marker", "kind": "edit"},
				"options": []any{
					map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
					map[string]any{"optionId": "allow-always", "name": "Always allow", "kind": "allow_always"},
					map[string]any{"optionId": "reject-once", "name": "Reject", "kind": "reject_once"},
				},
			},
		}),
	}
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	view := model.View()
	if !strings.Contains(view, "APPROVAL REQUIRED") || !strings.Contains(view, "[2] Always allow · PERSISTENT") || !strings.Contains(view, "1-3 choose approval") {
		t.Fatalf("permission prompt is not actionable:\n%s", view)
	}

	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	model = updated.(Model)
	if command != nil || model.confirm == nil || !strings.Contains(model.View(), "ALLOW ALWAYS") {
		t.Fatalf("persistent permission did not require explicit confirmation:\n%s", model.View())
	}
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed permission did not produce a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	backend.mu.Lock()
	decisions := append([]permissionCall(nil), backend.permissions...)
	backend.mu.Unlock()
	if len(decisions) != 1 || decisions[0].sessionID != "S-1" || decisions[0].permissionID != "permission-7:tool-write" || decisions[0].optionID != "allow-always" || decisions[0].cancelled {
		t.Fatalf("permission decision = %#v", decisions)
	}
	if !strings.Contains(model.notice, "turn continues") {
		t.Fatalf("permission notice = %q", model.notice)
	}
}

func TestRefreshAddsNewTaskSessionWithoutRestart(t *testing.T) {
	backend := newConsoleBackend()
	backend.detail.Sessions = backend.detail.Sessions[:1]
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(model.task.Sessions) != 1 {
		t.Fatal("fixture did not begin with one Session")
	}

	backend.mu.Lock()
	backend.detail.Sessions = append(backend.detail.Sessions, backend.sessions["S-2"])
	backend.mu.Unlock()
	updated, _ := model.Update(model.refreshCmd()())
	model = updated.(Model)
	if len(model.task.Sessions) != 2 || !strings.Contains(model.View(), "S-2") {
		t.Fatalf("new Session did not appear:\n%s", model.View())
	}
}

func TestTaskConsoleCreatesAndOpensSessionThroughWizard(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(Model)
	if command == nil {
		t.Fatal("n did not load the new Session wizard")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.wizard == nil || !strings.Contains(model.View(), "NEW SESSION") {
		t.Fatalf("wizard did not open:\n%s", model.View())
	}
	// Move to the final field and submit. Defaults intentionally prove isolated
	// worktree + read approval without requiring shell commands or copied IDs.
	for range 5 {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
		model = updated.(Model)
	}
	updated, command = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if command == nil || model.wizard == nil || !model.wizard.submitting {
		t.Fatal("wizard did not submit from its final field")
	}
	updated, command = model.Update(command())
	model = updated.(Model)
	if model.wizard != nil || model.mode != sessionView || model.current.ID != "S-3" {
		t.Fatalf("created Session did not open: wizard=%v mode=%d current=%s", model.wizard != nil, model.mode, model.current.ID)
	}
	backend.mu.Lock()
	starts := append([]appservice.StartSessionParams(nil), backend.starts...)
	backend.mu.Unlock()
	if len(starts) != 1 || starts[0].TaskID != "T-1" || starts[0].Agent != "codex" || !starts[0].Worktree || starts[0].ApproveAll {
		t.Fatalf("wizard start params = %#v", starts)
	}
}

func TestAttachSessionProgramCanDetachWithoutWorkerMutation(t *testing.T) {
	backend := newConsoleBackend()
	var output bytes.Buffer
	if err := AttachSession(context.Background(), backend, "S-1", strings.NewReader("q"), &output); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.cancelled) != 0 || len(backend.stopped) != 0 {
		t.Fatalf("terminal detach mutated worker: cancelled=%v stopped=%v", backend.cancelled, backend.stopped)
	}
}

func newConsoleBackend() *fakeBackend {
	sessions := map[string]session.Session{
		"S-1": {
			ID: "S-1", TaskID: "T-1", Role: "researcher", Agent: "gemini", Provider: "google",
			RepoPath: "/repo/one", Status: session.StatusWorking, WorkerSocket: "/tmp/S-1.sock",
			MetadataJSON: json.RawMessage(`{"permission":"read-approval"}`),
		},
		"S-2": {
			ID: "S-2", TaskID: "T-1", Role: "implementer", Agent: "codex", Provider: "openai",
			RepoPath: "/repo/two", WorktreePath: "/worktrees/two", Branch: "agent/S-2",
			Status: session.StatusWaiting, WorkerSocket: "/tmp/S-2.sock",
			MetadataJSON: json.RawMessage(`{"permission":"approve-all"}`),
		},
	}
	return &fakeBackend{
		detail: task.Detail{
			Task:     task.Task{ID: "T-1", Title: "Investigate gateway", Status: task.StatusWorking},
			Sessions: []session.Session{sessions["S-1"], sessions["S-2"]},
		},
		sessions: sessions,
		histories: map[string][]sessionlog.Line{
			"S-1": {logLine(1, "worker_ready", "ready one")},
			"S-2": {logLine(1, "prompt_queued", "queued two")},
		},
		profiles: []agents.Profile{{Name: "claude", Provider: "anthropic", Command: "claude"}, {Name: "codex", Provider: "openai", Command: "codex"}},
	}
}

func logLine(sequence uint64, lineType, message string) sessionlog.Line {
	data, _ := json.Marshal(map[string]string{"message": message})
	return sessionlog.Line{Seq: sequence, Timestamp: time.Unix(int64(sequence), 0).UTC(), Type: lineType, Data: data}
}
