package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/presentation"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type fakeBackend struct {
	mu           sync.Mutex
	detail       task.Detail
	sessions     map[string]session.Session
	histories    map[string][]sessionlog.Line
	historyCalls []historyCall
	prompts      []promptCall
	permissions  []permissionCall
	cancelled    []string
	resumed      []string
	stopped      []string
	starts       []appservice.StartSessionParams
	profiles     []agents.Profile
	promptState  string
	native       []string
}

type conversationFakeBackend struct {
	*fakeBackend
	snapshot appservice.ConversationSnapshot
}

func (f *conversationFakeBackend) Conversation(context.Context, string) (appservice.ConversationSnapshot, error) {
	return f.snapshot, nil
}

type promptCall struct {
	sessionID string
	message   string
}

type historyCall struct {
	sessionID string
	cursor    uint64
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
	f.historyCalls = append(f.historyCalls, historyCall{sessionID: id, cursor: cursor})
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
	for _, want := range []string{"TASK · Investigate gateway", "T-1", "researcher", "GEMINI", "one", "implementer", "CODEX", "two", "waiting", "No conversation yet"} {
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
	if model.mode != taskView || model.selected != 0 || nativeCommand != nil {
		t.Fatalf("mouse selection: selected=%d mode=%d command=%v", model.selected, model.mode, nativeCommand)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.native) != 0 {
		t.Fatalf("single click opened native CLI: %v", backend.native)
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
	if model.mode != taskView || nativeCommand != nil || !strings.Contains(model.notice, "Native CLI unavailable") || !strings.Contains(model.notice, "structured view") {
		t.Fatalf("unsupported native surface: mode=%d command=%v notice=%q", model.mode, nativeCommand, model.notice)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	if model.mode != sessionView || model.current.ID != "S-1" || model.composer.focused || !strings.Contains(model.View(), "CONVERSATION · SESSION S-1") {
		t.Fatalf("Structured ACP action = mode %d Session %s composer=%v\n%s", model.mode, model.current.ID, model.composer.focused, model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, nativeCommand = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if model.mode != taskView || nativeCommand == nil || !strings.Contains(model.notice, "Opening Codex CLI") || !strings.Contains(model.notice, "same provider conversation") {
		t.Fatalf("Enter did not choose native CLI: mode=%d command=%v notice=%q", model.mode, nativeCommand, model.notice)
	}
	direct := &nativeAttachCommand{ctx: context.Background(), backend: backend, sessionID: "S-2", input: strings.NewReader(""), output: io.Discard, stderr: io.Discard}
	if err := direct.Run(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	if !reflect.DeepEqual(backend.native, []string{"S-2"}) {
		backend.mu.Unlock()
		t.Fatalf("native calls = %v", backend.native)
	}
	backend.mu.Unlock()

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	if model.mode != sessionView || model.current.ID != "S-2" || model.composer.focused {
		t.Fatalf("ACP action = mode %d Session %s composer=%v", model.mode, model.current.ID, model.composer.focused)
	}
	if !strings.Contains(model.View(), "n Native CLI") {
		t.Fatalf("Structured ACP footer does not expose native handoff:\n%s", model.View())
	}
	updated, nativeFromSession := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = updated.(Model)
	if nativeFromSession == nil || !strings.Contains(model.notice, "Opening Codex CLI") || model.mode != sessionView {
		t.Fatalf("Structured ACP n handoff: command=%v notice=%q mode=%d", nativeFromSession, model.notice, model.mode)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
	model = updated.(Model)
	if !strings.Contains(model.View(), "n              Open the exact conversation in the provider's native CLI") {
		t.Fatalf("Structured ACP help does not document native handoff:\n%s", model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
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
	if model.overlay != helpOverlay || !strings.Contains(model.View(), "KEYBOARD HELP") || !strings.Contains(model.View(), "provider CLI") || !strings.Contains(model.View(), "structured view") || strings.Contains(model.View(), "harness") {
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

func TestTaskConsoleMouseUsesScrolledNavigatorWindow(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for index := 3; index <= 9; index++ {
		model.task.Sessions = append(model.task.Sessions, session.Session{
			ID: fmt.Sprintf("S-%d", index), TaskID: "T-1", Agent: "codex", Role: "worker", Status: session.StatusWaiting,
		})
	}
	model.width, model.height = 100, 16
	model.selected = len(model.task.Sessions) - 1
	layout := layoutTaskWorkspace(model.width, model.height)
	indices, start, visible := model.taskSessionWindow(layout.Navigator.Height)
	if start == 0 || visible >= len(indices) {
		t.Fatalf("test did not create a scrolled window: start=%d visible=%d total=%d", start, visible, len(indices))
	}
	hits := model.taskSessionHits(layout)
	if len(hits) == 0 {
		t.Fatal("layout exposed no visible Session hit targets")
	}
	updated, command := model.Update(tea.MouseMsg(tea.MouseEvent{
		X: hits[0].Rect.X, Y: hits[0].Rect.Y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	}))
	model = updated.(Model)
	if command != nil || model.selected != indices[start] {
		t.Fatalf("scrolled mouse selected index %d, want %d; command=%v", model.selected, indices[start], command)
	}
}

func TestTaskConsoleMouseUsesResponsiveLayoutHitRegions(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120} {
		t.Run(fmt.Sprintf("%d-columns", width), func(t *testing.T) {
			model := visualTaskModel()
			model.width, model.height = width, 24
			model.selected = 1
			layout := layoutTaskWorkspace(model.width, model.height)
			hits := model.taskSessionHits(layout)
			if len(hits) < 2 {
				t.Fatalf("%d-column layout exposed %d Session targets", width, len(hits))
			}
			updated, command := model.handleMouse(tea.MouseEvent{
				X:      hits[0].Rect.X + hits[0].Rect.Width/2,
				Y:      hits[0].Rect.Y,
				Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			})
			model = updated.(Model)
			if command != nil || model.selected != 0 {
				t.Fatalf("%d-column hit selected=%d command=%v", width, model.selected, command)
			}
			if layout.Preview.Width > 0 {
				updated, command = model.handleMouse(tea.MouseEvent{
					X: layout.Preview.X, Y: layout.Preview.Y,
					Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
				})
				model = updated.(Model)
				if command != nil || model.selected != 0 {
					t.Fatalf("%d-column preview click changed selection=%d command=%v", width, model.selected, command)
				}
			}
		})
	}
}

func TestTaskConsoleMouseWheelMovesFilteredSelectionAcrossResponsiveLayouts(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120} {
		t.Run(fmt.Sprintf("%d-columns", width), func(t *testing.T) {
			model := visualTaskModel()
			model.task.Sessions = nil
			for index := 0; index < 10; index++ {
				agent := "claude"
				if index%2 == 0 {
					agent = "codex"
				}
				model.task.Sessions = append(model.task.Sessions, session.Session{
					ID: fmt.Sprintf("S-%02d", index), TaskID: "T-VISUAL", Agent: agent,
					Role: "worker", Status: session.StatusWaiting,
				})
			}
			model.width, model.height = width, 16
			model.filter = "codex"
			model.selected = 0
			model.scroll, model.unread = 7, 2
			matches := model.filteredSessionIndices()
			if !reflect.DeepEqual(matches, []int{0, 2, 4, 6, 8}) {
				t.Fatalf("filtered Sessions = %v", matches)
			}
			layout := layoutTaskWorkspace(model.width, model.height)
			for position := 1; position < len(matches); position++ {
				updated, command := model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
				model = updated.(Model)
				if command != nil || model.selected != matches[position] {
					t.Fatalf("wheel step %d selected=%d, want %d; command=%v", position, model.selected, matches[position], command)
				}
				indices, start, visible := model.taskSessionWindow(layout.Navigator.Height)
				selectedPosition := -1
				for candidate, actual := range indices {
					if actual == model.selected {
						selectedPosition = candidate
						break
					}
				}
				if selectedPosition < start || selectedPosition >= start+visible {
					t.Fatalf("selected Session is outside visible window: selectedPosition=%d start=%d visible=%d", selectedPosition, start, visible)
				}
			}
			updated, command := model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
			model = updated.(Model)
			if command != nil || model.selected != matches[len(matches)-1] {
				t.Fatalf("wheel moved past filtered end: selected=%d command=%v", model.selected, command)
			}
			updated, command = model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
			model = updated.(Model)
			if command != nil || model.selected != matches[len(matches)-2] || model.scroll != 7 || model.unread != 2 {
				t.Fatalf("wheel up state: selected=%d scroll=%d unread=%d command=%v", model.selected, model.scroll, model.unread, command)
			}
		})
	}
}

func TestSessionConsoleMouseWheelRetainsTranscriptScrolling(t *testing.T) {
	model := visualSessionModel(t)
	model.scroll, model.unread = 3, 2
	updated, command := model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	model = updated.(Model)
	if command != nil || model.scroll != 6 || model.unread != 2 {
		t.Fatalf("wheel up state: scroll=%d unread=%d command=%v", model.scroll, model.unread, command)
	}
	updated, command = model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	model = updated.(Model)
	updated, command = model.handleMouse(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	model = updated.(Model)
	if command != nil || model.scroll != 0 || model.unread != 0 {
		t.Fatalf("wheel down state: scroll=%d unread=%d command=%v", model.scroll, model.unread, command)
	}
}

func TestTaskFooterUsesActionRegistryAndKeepsNewSessionVisible(t *testing.T) {
	backend := newConsoleBackend()
	model, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	newAction, ok := taskActionForID(actionNew)
	if !ok {
		t.Fatal("New Session action is missing from the registry")
	}
	wantNew := displayActionKey(newAction.Key) + " " + newAction.FooterLabel
	for _, width := range []int{60, 72, 80, 91, 92, 120} {
		model.width, model.height = width, 24
		lines := strings.Split(model.View(), "\n")
		footer := lines[len(lines)-1]
		if !strings.Contains(footer, wantNew) || strings.Contains(footer, "…") {
			t.Fatalf("%d-column footer does not expose %q intact: %q", width, wantNew, footer)
		}
		for _, want := range []string{"a View", "? Help", "q Detach"} {
			if !strings.Contains(footer, want) {
				t.Fatalf("%d-column footer missing %q: %q", width, want, footer)
			}
		}
	}
	model.selected, model.width = 1, 60
	availableFooter := strings.Split(model.View(), "\n")[model.height-1]
	if !strings.Contains(availableFooter, "Enter Native CLI") || !strings.Contains(availableFooter, wantNew) || strings.Contains(availableFooter, "…") {
		t.Fatalf("minimum-width available footer is not intact: %q", availableFooter)
	}
	model.width = 120
	footer := strings.Split(model.View(), "\n")[model.height-1]
	for _, want := range []string{"p Message", "/ Filter", ": Commands"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("wide footer missing %q: %q", want, footer)
		}
	}
}

func TestTaskNativeCapabilityUsesSupportedMappingAndCanonicalIdentity(t *testing.T) {
	verifiedAt := time.Now().UTC()
	tests := []struct {
		name      string
		current   session.Session
		available bool
		want      string
	}{
		{name: "supported pending verification", current: session.Session{Agent: "claude", ProviderConversationID: "claude-id", RepoPath: "/repo"}, available: true, want: "safety checked on open"},
		{name: "supported verified", current: session.Session{Agent: "opencode", ProviderConversationID: "ses-id", RepoPath: "/repo", ConversationIDVerifiedAt: &verifiedAt}, available: true, want: "identity verified"},
		{name: "missing identity", current: session.Session{Agent: "codex", RepoPath: "/repo"}, want: "exact provider conversation identity"},
		{name: "unsupported mapping", current: session.Session{Agent: "gemini", ProviderConversationID: "gemini-id", RepoPath: "/repo"}, want: "no supported exact native resume command"},
		{name: "missing checkout", current: session.Session{Agent: "grok", ProviderConversationID: "grok-id"}, want: "no checkout path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capability := nativeCapability(test.current)
			if capability.available != test.available || !strings.Contains(capability.status+" "+capability.reason, test.want) {
				t.Fatalf("capability = %#v, want available=%t and %q", capability, test.available, test.want)
			}
		})
	}
}

func TestTaskMouseCannotClickThroughInteractiveLayers(t *testing.T) {
	backend := newConsoleBackend()
	base, cancel, err := newTaskModel(context.Background(), backend, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	base.selected = 1
	base.scroll, base.unread = 7, 2
	tests := []struct {
		name   string
		setup  func(*Model)
		active func(Model) bool
	}{
		{name: "help", setup: func(m *Model) { m.overlay = helpOverlay }, active: func(m Model) bool { return m.overlay == helpOverlay }},
		{name: "palette", setup: func(m *Model) { m.overlay = paletteOverlay }, active: func(m Model) bool { return m.overlay == paletteOverlay }},
		{name: "filter", setup: func(m *Model) { m.overlay = filterOverlay }, active: func(m Model) bool { return m.overlay == filterOverlay }},
		{name: "wizard", setup: func(m *Model) { m.wizard = &sessionWizard{} }, active: func(m Model) bool { return m.wizard != nil }},
		{name: "composer", setup: func(m *Model) { m.composer.focused = true }, active: func(m Model) bool { return m.composer.focused }},
		{name: "confirmation", setup: func(m *Model) { m.confirm = &confirmation{kind: "stop", sessionID: "S-2"} }, active: func(m Model) bool { return m.confirm != nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := base
			test.setup(&model)
			updated, command := model.Update(tea.MouseMsg(tea.MouseEvent{X: 2, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}))
			model = updated.(Model)
			if command != nil || model.selected != 1 || !test.active(model) {
				t.Fatalf("click reached Task navigator: selected=%d command=%v", model.selected, command)
			}
			updated, command = model.Update(tea.MouseMsg(tea.MouseEvent{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown}))
			model = updated.(Model)
			if command != nil || model.selected != 1 || model.scroll != 7 || model.unread != 2 || !test.active(model) {
				t.Fatalf("wheel reached underlying view: selected=%d scroll=%d unread=%d command=%v", model.selected, model.scroll, model.unread, command)
			}
		})
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

func TestSessionConsoleLoadsProviderHistoryAndKeepsLiveACPEventsAcrossReconcile(t *testing.T) {
	base := newConsoleBackend()
	snapshot := appservice.ConversationSnapshot{
		Source:        appservice.ConversationSourceProvider,
		GatewayCursor: 40,
		Turns: []appservice.ConversationTurn{
			{
				Key:       "provider-first",
				Committed: true,
				Activities: []appservice.ConversationActivity{
					{Display: &presentation.Activity{Type: "user_message", Text: "First ACP prompt"}},
					{Display: &presentation.Activity{Type: "assistant_chunk", Text: "First answer"}},
					{Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
				},
			},
			{
				Key:       "provider-native",
				Committed: true,
				Activities: []appservice.ConversationActivity{
					{Display: &presentation.Activity{Type: "user_message", Text: "Native-only prompt"}},
					{Kind: "provider_transcript", Display: &presentation.Activity{Type: "thought_chunk", Text: "Native reasoning summary"}},
					{Kind: "provider_transcript", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Native answer"}},
					{Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
				},
			},
		},
	}
	backend := &conversationFakeBackend{fakeBackend: base, snapshot: snapshot}
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	if got := model.View(); !strings.Contains(got, "Native-only prompt") || !strings.Contains(got, "Reasoning · saved · 1 line") || !strings.Contains(got, "RESPONSE · GEMINI · saved") || !strings.Contains(got, "Native answer") {
		t.Fatalf("provider-backed transcript is incomplete:\n%s", got)
	}
	nativeTurn := model.projections["S-1"].byID["provider-native"]
	if nativeTurn == nil || !nativeTurn.reasoningSeen || len(nativeTurn.blocks) < 2 || nativeTurn.blocks[1].kind != blockThought || nativeTurn.blocks[1].text != "Native reasoning summary" {
		t.Fatalf("provider reasoning projection = %#v", nativeTurn)
	}
	if model.cursors["S-1"] != 40 {
		t.Fatalf("provider snapshot cursor = %d, want 40", model.cursors["S-1"])
	}

	// An ACP event can arrive while the provider refresh is being read. A
	// completed snapshot must not erase anything newer than its captured cursor.
	model.addActivity("S-1", projectionLine(41, "REQ-LIVE", "prompt_submitted", map[string]any{"display_message": "Live follow-up"}), true)
	model.addActivity("S-1", projectionACPLine(42, "REQ-LIVE", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "Still streaming", "live-message"))), true)
	updated, _ := model.Update(conversationMsg{sessionID: "S-1", snapshot: snapshot})
	model = updated.(Model)

	if got := model.View(); !strings.Contains(got, "Native-only prompt") || !strings.Contains(got, "Live follow-up") || !strings.Contains(got, "Still streaming") {
		t.Fatalf("atomic reconciliation dropped committed or live content:\n%s", got)
	}
	if model.cursors["S-1"] != 42 {
		t.Fatalf("cursor after buffered live events = %d, want 42", model.cursors["S-1"])
	}
}

func TestSessionConsolePreservesIncompleteProviderTurnState(t *testing.T) {
	snapshot := appservice.ConversationSnapshot{Turns: []appservice.ConversationTurn{{
		Key: "provider-partial", Committed: false,
		Activities: []appservice.ConversationActivity{
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "user_message", Text: "Interrupted native prompt"}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Partial provider output"}},
			{Kind: "provider_turn_incomplete", Display: &presentation.Activity{Type: "turn_state", State: "incomplete", StopReason: "provider_history_incomplete"}},
		},
	}}}
	projection := transcriptFromConversation(snapshot)
	request := projection.byID["provider-partial"]
	if request == nil || request.committed || request.state != turnIncomplete {
		t.Fatalf("incomplete provider request = %#v", request)
	}
	if got := renderTurnState(request.state); !strings.Contains(got, "Provider turn incomplete") {
		t.Fatalf("incomplete state rendering = %q", got)
	}
}

func TestProviderTranscriptReconcileLifecycleRefreshesWithoutReplacingLiveTranscript(t *testing.T) {
	base := newConsoleBackend()
	snapshot := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceProvider, GatewayCursor: 40,
		Turns: []appservice.ConversationTurn{{
			Key: "provider-turn", Committed: true,
			Activities: []appservice.ConversationActivity{
				{Display: &presentation.Activity{Type: "user_message", Text: "Keep this committed prompt"}},
				{Display: &presentation.Activity{Type: "assistant_chunk", Text: "Keep this committed answer"}},
			},
		}},
	}
	backend := &conversationFakeBackend{fakeBackend: base, snapshot: snapshot}
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	warning := projectionLine(41, "", "provider_transcript_reconcile_warning", map[string]any{"message": "provider store still flushing"})
	updated, _ := model.Update(activityMsg{sessionID: "S-1", line: warning})
	model = updated.(Model)
	if !strings.Contains(model.notice, "reload is delayed") || !strings.Contains(model.View(), "Keep this committed answer") {
		t.Fatalf("reconcile warning replaced transcript or hid notice: notice=%q\n%s", model.notice, model.View())
	}
	if shouldReconcileConversation(warning) {
		t.Fatal("reconcile warning should preserve the current transcript instead of loading a replacement")
	}

	reconciled := projectionLine(42, "", "provider_transcript_reconciled", map[string]any{"source": "provider-store", "state": "reconciled"})
	updated, command := model.Update(activityMsg{sessionID: "S-1", line: reconciled})
	model = updated.(Model)
	if command == nil || !shouldReconcileConversation(reconciled) {
		t.Fatalf("provider transcript reconciliation did not schedule a joined snapshot: command=%v", command)
	}
	if !strings.Contains(model.View(), "Keep this committed answer") {
		t.Fatalf("reconciliation signal replaced transcript before snapshot join:\n%s", model.View())
	}
	updated, _ = model.Update(conversationMsg{sessionID: "S-1", snapshot: snapshot})
	model = updated.(Model)
	if model.notice != "" {
		t.Fatalf("successful provider snapshot left stale reconcile warning: %q", model.notice)
	}

	model.composer.focused = true
	model.composer.insert([]rune("keep this draft")...)
	model.scroll = 4
	model.addActivity("S-1", projectionLine(43, "REQ-REHYDRATE", "prompt_submitted", map[string]any{"display_message": "Provisional ACP prompt"}), true)
	model.addActivity("S-1", projectionACPLine(44, "REQ-REHYDRATE", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "Provisional ACP answer", "rehydrate-live"))), true)
	rehydrated := projectionLine(45, "REQ-REHYDRATE", "acp_rehydrated", map[string]any{"state": "refreshed"})
	updated, command = model.Update(activityMsg{sessionID: "S-1", line: rehydrated})
	model = updated.(Model)
	if command == nil || !shouldReconcileConversation(rehydrated) {
		t.Fatalf("ACP rehydration did not schedule a joined provider snapshot: command=%v", command)
	}
	if model.composer.text() != "keep this draft" || !model.composer.focused || model.scroll != 4 || !strings.Contains(model.View(), "Provisional ACP answer") {
		t.Fatalf("ACP rehydration signal changed disposable viewer state: draft=%q focused=%t scroll=%d\n%s", model.composer.text(), model.composer.focused, model.scroll, model.View())
	}
	updated, _ = model.Update(conversationMsg{sessionID: "S-1", snapshot: snapshot})
	model = updated.(Model)
	if model.composer.text() != "keep this draft" || !model.composer.focused || model.scroll != 4 || !strings.Contains(model.View(), "Provisional ACP answer") {
		t.Fatalf("ACP rehydration snapshot dropped draft, scroll, or provisional ACP output: draft=%q focused=%t scroll=%d\n%s", model.composer.text(), model.composer.focused, model.scroll, model.View())
	}
}

func TestSessionConsoleAdoptsGatewayFallbackSnapshotBeforeRawHistory(t *testing.T) {
	base := newConsoleBackend()
	base.histories["S-1"] = []sessionlog.Line{
		projectionLine(10, "REQ-RAW-COVERED", "prompt_submitted", map[string]any{"display_message": "covered raw prompt"}),
		projectionLine(21, "REQ-AFTER-CAPTURE", "prompt_submitted", map[string]any{"display_message": "newer live prompt"}),
	}
	snapshot := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceGateway, GatewayCursor: 20,
		Warning: "Provider history is unavailable; showing gateway history.",
		Turns: []appservice.ConversationTurn{{
			Key: "REQ-BOUNDED", Committed: false,
			Activities: []appservice.ConversationActivity{
				{Position: 10, RequestID: "REQ-BOUNDED", Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: "bounded gateway prompt"}},
				{Position: 11, RequestID: "REQ-BOUNDED", Kind: "agent_message_chunk", Display: &presentation.Activity{Type: "assistant_chunk", Text: "bounded gateway answer"}},
			},
		}},
	}
	backend := &conversationFakeBackend{fakeBackend: base, snapshot: snapshot}
	model, cancel, err := newSessionModel(context.Background(), backend, "S-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	view := model.View()
	for _, want := range []string{"bounded gateway prompt", "bounded gateway answer", "newer live prompt"} {
		if !strings.Contains(view, want) {
			t.Fatalf("gateway snapshot view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "covered raw prompt") {
		t.Fatalf("snapshot-covered raw activity was independently projected:\n%s", view)
	}
	if model.notice != snapshot.Warning || model.snapshotCursors["S-1"] != 20 || model.cursors["S-1"] != 21 {
		t.Fatalf("fallback state: notice=%q snapshotCursor=%d cursor=%d", model.notice, model.snapshotCursors["S-1"], model.cursors["S-1"])
	}
	if got := len(model.histories["S-1"]); got != 1 || model.histories["S-1"][0].Seq != 21 {
		t.Fatalf("post-snapshot raw history = %#v, want only position 21", model.histories["S-1"])
	}
	if _, exists := model.projections["S-1"].byID["REQ-RAW-COVERED"]; exists {
		t.Fatal("snapshot-covered raw request was re-applied")
	}
	base.mu.Lock()
	historyCalls := append([]historyCall(nil), base.historyCalls...)
	base.mu.Unlock()
	if !reflect.DeepEqual(historyCalls, []historyCall{{sessionID: "S-1", cursor: 20}}) {
		t.Fatalf("History calls = %#v, want captured cursor 20", historyCalls)
	}
	model.raw = true
	if raw := model.View(); strings.Contains(raw, `"seq":10`) || !strings.Contains(raw, `"seq":21`) || !strings.Contains(raw, "Pre-snapshot raw is not retained") || !strings.Contains(raw, "acpctl session logs <id> --raw") {
		t.Fatalf("bounded raw diagnostics are misleading or incomplete:\n%s", raw)
	}
}

func TestConversationSnapshotCursorRejectsLateProjectionButKeepsRawHistory(t *testing.T) {
	backend := newConsoleBackend()
	model := baseModel(context.Background(), backend)
	defer model.manager.cancel()
	model.mode = sessionView
	model.current = session.Session{ID: "S-LATE", ProviderConversationID: "provider-conversation"}
	model.scroll = 3
	snapshot := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceProvider, GatewayCursor: 40,
		Turns: []appservice.ConversationTurn{{
			Key: "provider-committed", Committed: true,
			Activities: []appservice.ConversationActivity{
				{Display: &presentation.Activity{Type: "user_message", Text: "committed prompt"}},
				{Display: &presentation.Activity{Type: "assistant_chunk", Text: "committed answer"}},
			},
		}},
	}
	updated, command := model.Update(conversationMsg{sessionID: "S-LATE", snapshot: snapshot})
	model = updated.(Model)
	if command != nil || model.snapshotCursors["S-LATE"] != 40 {
		t.Fatalf("snapshot adoption: cursor=%d command=%v", model.snapshotCursors["S-LATE"], command)
	}

	for _, line := range []sessionlog.Line{
		projectionLine(39, "REQ-LATE-39", "prompt_submitted", map[string]any{"display_message": "late covered 39"}),
		projectionLine(40, "REQ-LATE-40", "prompt_submitted", map[string]any{"display_message": "late covered 40"}),
	} {
		model.addActivity("S-LATE", line, true)
	}
	if got := len(model.histories["S-LATE"]); got != 2 {
		t.Fatalf("late raw history has %d records, want 2", got)
	}
	if len(model.projections["S-LATE"].requests) != 1 || model.projections["S-LATE"].byID["REQ-LATE-39"] != nil || model.projections["S-LATE"].byID["REQ-LATE-40"] != nil {
		t.Fatalf("snapshot-covered activities were re-applied: requests=%v", model.projections["S-LATE"].byID)
	}
	if model.unread != 0 {
		t.Fatalf("snapshot-covered late delivery incremented unread to %d", model.unread)
	}

	model.addActivity("S-LATE", projectionLine(41, "REQ-NEW", "prompt_submitted", map[string]any{"display_message": "after snapshot"}), true)
	if model.projections["S-LATE"].byID["REQ-NEW"] == nil || model.unread != 1 || model.cursors["S-LATE"] != 41 {
		t.Fatalf("post-snapshot activity was not projected: unread=%d cursor=%d", model.unread, model.cursors["S-LATE"])
	}
	model.raw = true
	if raw := model.View(); !strings.Contains(raw, `"seq":39`) || !strings.Contains(raw, `"seq":40`) || !strings.Contains(raw, `"seq":41`) {
		t.Fatalf("raw history is incomplete after reconciliation:\n%s", raw)
	}

	stale := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceGateway, GatewayCursor: 39,
		Turns: []appservice.ConversationTurn{{Key: "stale-snapshot", Activities: []appservice.ConversationActivity{{Display: &presentation.Activity{Type: "user_message", Text: "stale replacement"}}}}},
	}
	if model.adoptConversationSnapshot("S-LATE", stale) {
		t.Fatal("strictly older snapshot replaced the adopted projection")
	}
	if model.projections["S-LATE"].byID["stale-snapshot"] != nil || model.snapshotCursors["S-LATE"] != 40 {
		t.Fatalf("stale snapshot mutated projection: cursor=%d", model.snapshotCursors["S-LATE"])
	}

	downgrade := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceGateway, GatewayCursor: 41,
		Warning: "Provider history is temporarily unavailable.",
		Turns: []appservice.ConversationTurn{{
			Key: "fallback-replacement", Committed: false,
			Activities: []appservice.ConversationActivity{
				{Display: &presentation.Activity{Type: "user_message", Text: "fallback replacement"}},
				{Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
			},
		}},
	}
	updated, command = model.Update(conversationMsg{sessionID: "S-LATE", snapshot: downgrade})
	model = updated.(Model)
	if command == nil || !model.conversationCommitPending["S-LATE"] || !strings.Contains(model.notice, "refresh is delayed") {
		t.Fatalf("provider downgrade did not retain promotion retry: pending=%v notice=%q command=%v", model.conversationCommitPending["S-LATE"], model.notice, command)
	}
	if model.snapshotSources["S-LATE"] != appservice.ConversationSourceProvider || model.snapshotCursors["S-LATE"] != 40 || model.projections["S-LATE"].byID["fallback-replacement"] != nil || model.projections["S-LATE"].byID["REQ-NEW"] == nil {
		t.Fatalf("gateway fallback replaced provider/live projection: source=%q cursor=%d", model.snapshotSources["S-LATE"], model.snapshotCursors["S-LATE"])
	}
}

func TestConversationCommitRetryContinuesUntilProviderSnapshotCommits(t *testing.T) {
	backend := newConsoleBackend()
	model := baseModel(context.Background(), backend)
	defer model.manager.cancel()
	model.current = session.Session{ID: "S-1", ProviderConversationID: "provider-conversation"}
	pending := appservice.ConversationSnapshot{
		Source: appservice.ConversationSourceGateway,
		Turns: []appservice.ConversationTurn{{
			Key: "REQ-pending", Committed: false,
			Activities: []appservice.ConversationActivity{{
				Kind: "turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete"},
			}},
		}},
	}
	updated, command := model.Update(conversationMsg{sessionID: "S-1", snapshot: pending})
	model = updated.(Model)
	if command == nil || !model.conversationCommitPending["S-1"] || model.conversationCommitRetries["S-1"] != 1 {
		t.Fatalf("pending provider commit did not schedule retry: pending=%v retries=%d command=%v", model.conversationCommitPending["S-1"], model.conversationCommitRetries["S-1"], command)
	}

	updated, command = model.Update(conversationMsg{sessionID: "S-1", err: fmt.Errorf("provider store still flushing")})
	model = updated.(Model)
	if command == nil || model.conversationCommitRetries["S-1"] != 2 || !strings.Contains(model.notice, "live ACP output remains current") {
		t.Fatalf("transient provider error stopped retrying: retries=%d notice=%q command=%v", model.conversationCommitRetries["S-1"], model.notice, command)
	}

	committed := pending
	committed.Turns = append([]appservice.ConversationTurn(nil), pending.Turns...)
	committed.Turns[0].Committed = true
	committed.Source = appservice.ConversationSourceProvider
	updated, command = model.Update(conversationMsg{sessionID: "S-1", snapshot: committed})
	model = updated.(Model)
	if command != nil || model.conversationCommitPending["S-1"] || model.conversationCommitRetries["S-1"] != 0 {
		t.Fatalf("committed provider snapshot left retry state: pending=%v retries=%d command=%v", model.conversationCommitPending["S-1"], model.conversationCommitRetries["S-1"], command)
	}

	cancelled := pending
	cancelled.Turns = append([]appservice.ConversationTurn(nil), pending.Turns...)
	cancelled.Turns[0].Activities = []appservice.ConversationActivity{{
		Kind: "turn_cancelled", Display: &presentation.Activity{Type: "turn_state", State: "cancelled"},
	}}
	if conversationSnapshotNeedsCommit(cancelled) {
		t.Fatal("cancelled turn incorrectly requested perpetual provider commit polling")
	}
	if conversationCommitRetryDelay(0) != 220*time.Millisecond || conversationCommitRetryDelay(6) != 10*time.Second {
		t.Fatalf("conversation retry delays = %s, %s", conversationCommitRetryDelay(0), conversationCommitRetryDelay(6))
	}

	for _, test := range []struct {
		name      string
		identity  string
		turnState string
		wantRetry bool
	}{
		{name: "missing canonical identity", turnState: "complete"},
		{name: "live turn", identity: "provider-conversation", turnState: "working", wantRetry: true},
		{name: "failed turn", identity: "provider-conversation", turnState: "error", wantRetry: true},
		{name: "cancelled turn", identity: "provider-conversation", turnState: "cancelled", wantRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := baseModel(context.Background(), backend)
			defer candidate.manager.cancel()
			candidate.current = session.Session{ID: "S-OTHER", ProviderConversationID: test.identity}
			snapshot := appservice.ConversationSnapshot{
				Source: appservice.ConversationSourceGateway,
				Turns: []appservice.ConversationTurn{{
					Key: "REQ-other", Committed: false,
					Activities: []appservice.ConversationActivity{{Display: &presentation.Activity{Type: "turn_state", State: test.turnState}}},
				}},
			}
			updated, retry := candidate.Update(conversationMsg{sessionID: "S-OTHER", snapshot: snapshot})
			candidate = updated.(Model)
			if gotRetry := retry != nil && candidate.conversationCommitPending["S-OTHER"]; gotRetry != test.wantRetry {
				t.Fatalf("fallback authority retry = %v, want %v: pending=%v command=%v", gotRetry, test.wantRetry, candidate.conversationCommitPending["S-OTHER"], retry)
			}
		})
	}
}

func TestConversationAuthorityRetryRecoversIdleEmptyGatewayFallback(t *testing.T) {
	backend := newConsoleBackend()
	model := baseModel(context.Background(), backend)
	defer model.manager.cancel()
	model.current = session.Session{ID: "S-IDLE", ProviderConversationID: "provider-conversation"}

	fallback := appservice.ConversationSnapshot{Source: appservice.ConversationSourceGateway}
	updated, retry := model.Update(conversationMsg{sessionID: "S-IDLE", snapshot: fallback})
	model = updated.(Model)
	if retry == nil || !model.conversationCommitPending["S-IDLE"] {
		t.Fatalf("idle gateway fallback did not schedule provider-authority retry: pending=%v command=%v", model.conversationCommitPending["S-IDLE"], retry)
	}

	provider := appservice.ConversationSnapshot{Source: appservice.ConversationSourceProvider, Turns: []appservice.ConversationTurn{{
		Key: "provider-native", Committed: true,
		Activities: []appservice.ConversationActivity{
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "user_message", Text: "Native history"}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Loaded later"}},
			{Kind: "provider_turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
		},
	}}}
	updated, retry = model.Update(conversationMsg{sessionID: "S-IDLE", snapshot: provider})
	model = updated.(Model)
	if retry != nil || model.conversationCommitPending["S-IDLE"] || model.snapshotSources["S-IDLE"] != appservice.ConversationSourceProvider {
		t.Fatalf("provider authority did not clear retry state: pending=%v source=%q command=%v", model.conversationCommitPending["S-IDLE"], model.snapshotSources["S-IDLE"], retry)
	}
	if request := model.projections["S-IDLE"].byID["provider-native"]; request == nil || request.state != turnComplete {
		t.Fatalf("late provider history was not adopted: %#v", request)
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

func TestAcceptedOrQueuedPromptRemainsCancellableDuringPreparation(t *testing.T) {
	for _, state := range []turnState{turnReady, turnQueued} {
		t.Run(string(state), func(t *testing.T) {
			current := session.Session{Status: session.StatusWaiting}
			projection := newTranscript()
			projection.request("REQ-preparing").state = state
			if !sessionIsActive(current, projection) {
				t.Fatalf("%s ACP head was not exposed as cancellable", state)
			}
		})
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
	if len(decisions) != 1 || decisions[0].sessionID != "S-1" || decisions[0].permissionID != worker.PermissionConsoleID(requestID, "permission-7") || decisions[0].optionID != "allow-always" || decisions[0].cancelled {
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
	if len(model.task.Sessions) != 2 || !strings.Contains(model.View(), "S-2") || !strings.Contains(model.View(), "CODEX") || !strings.Contains(model.View(), "implementer") {
		t.Fatalf("new Session did not appear:\n%s", model.View())
	}
}

func TestTaskContinuationReadinessDistinguishesLinkedFromVerified(t *testing.T) {
	linked := nativeCLICapability{available: true, status: "identity present · safety checked on open"}
	if got := taskContinuationReadiness(linked); got != "Conversation linked · verified on open" {
		t.Fatalf("linked readiness = %q", got)
	}
	verified := nativeCLICapability{available: true, status: "identity verified · worker checked on open"}
	if got := taskContinuationReadiness(verified); got != "Exact conversation verified" {
		t.Fatalf("verified readiness = %q", got)
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
	// Populate both optional provider controls, then move to the final field and
	// submit. Defaults still prove isolated worktree + read approval.
	for model.wizard.field != wizardModel {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
		model = updated.(Model)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("model-selected")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("high")})
	model = updated.(Model)
	for model.wizard.field != wizardPrompt {
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
	if len(starts) != 1 || starts[0].TaskID != "T-1" || starts[0].Agent != "codex" || starts[0].Model != "model-selected" || starts[0].ReasoningEffort != "high" || !starts[0].Worktree || starts[0].ApproveAll {
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
			ProviderConversationID: "codex-conversation",
			Status:                 session.StatusWaiting, WorkerSocket: "/tmp/S-2.sock",
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
