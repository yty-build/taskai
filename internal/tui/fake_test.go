package tui

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

type fakeCall struct {
	kind     string
	task     TaskID
	session  SessionID
	provider ProviderID
	text     string
	enabled  bool
}

type fakeBackend struct {
	mu sync.Mutex

	snapshot Snapshot
	calls    []fakeCall
	events   chan PublicEvent

	nextTask    int
	nextManager int
	nextCalls   int
	nextActive  int
	maxNext     int

	snapshotErr error
	actionErr   map[string]error

	addManagerOnError  *Session
	attachStarted      chan struct{}
	attachRelease      chan struct{}
	attachIgnoreCancel bool
	attachStartOnce    sync.Once
	detachStarted      chan struct{}
	detachStartOnce    sync.Once
	resumeStateOnError *SessionState
}

func newFake(snapshot Snapshot) *fakeBackend {
	return &fakeBackend{
		snapshot:  cloneSnapshot(snapshot),
		events:    make(chan PublicEvent, 1024),
		actionErr: make(map[string]error),
	}
}

func (f *fakeBackend) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "snapshot"})
	return cloneSnapshot(f.snapshot), f.snapshotErr
}

func (f *fakeBackend) AddTask(_ context.Context, title string) (TaskID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "add-task", text: title})
	if err := f.actionErr["add-task"]; err != nil {
		return "", err
	}
	f.nextTask++
	id := TaskID(fmt.Sprintf("created-task-%d", f.nextTask))
	f.snapshot.Tasks = append(f.snapshot.Tasks, Task{
		ID: id, Title: title,
		CreatedAt: time.Date(2030, 1, f.nextTask, 0, 0, 0, 0, time.UTC),
	})
	return id, nil
}

func (f *fakeBackend) AddManager(_ context.Context, task TaskID, provider ProviderID, label string) (SessionID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "add-manager", task: task, provider: provider, text: label})
	if err := f.actionErr["add-manager"]; err != nil {
		if f.addManagerOnError != nil {
			created := *f.addManagerOnError
			f.snapshot.Sessions = append(f.snapshot.Sessions, created)
			return created.ID, err
		}
		return "", err
	}
	f.nextManager++
	id := SessionID(fmt.Sprintf("created-manager-%d", f.nextManager))
	f.snapshot.Sessions = append(f.snapshot.Sessions, Session{
		ID: id, TaskID: task, Role: RoleManager, Label: label, ProviderID: provider,
		State: StateActive, CreatedAt: time.Date(2030, 2, f.nextManager, 0, 0, 0, 0, time.UTC),
	})
	return id, nil
}

func (f *fakeBackend) SetEnabled(_ context.Context, provider ProviderID, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "enable", provider: provider, enabled: enabled})
	if err := f.actionErr["enable"]; err != nil {
		return err
	}
	for i := range f.snapshot.Providers {
		if f.snapshot.Providers[i].ID == provider {
			f.snapshot.Providers[i].Enabled = enabled
		}
	}
	return nil
}

func (f *fakeBackend) Authenticate(_ context.Context, provider ProviderID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "authenticate", provider: provider})
	if err := f.actionErr["authenticate"]; err != nil {
		return err
	}
	for i := range f.snapshot.Providers {
		if f.snapshot.Providers[i].ID == provider {
			f.snapshot.Providers[i].ProbeStatus = ProbeReady
		}
	}
	return nil
}

func (f *fakeBackend) Recheck(_ context.Context, provider ProviderID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "recheck", provider: provider})
	return f.actionErr["recheck"]
}

func (f *fakeBackend) Attach(ctx context.Context, task TaskID) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{kind: "attach", task: task})
	err := f.actionErr["attach"]
	started := f.attachStarted
	release := f.attachRelease
	ignoreCancel := f.attachIgnoreCancel
	f.mu.Unlock()
	if started != nil {
		f.attachStartOnce.Do(func() { close(started) })
	}
	if release != nil {
		if ignoreCancel {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

func (f *fakeBackend) Detach(_ context.Context, task TaskID) error {
	if f.detachStarted != nil {
		f.detachStartOnce.Do(func() { close(f.detachStarted) })
	}
	return f.record("detach", task, "", "", "")
}

func (f *fakeBackend) Send(_ context.Context, session SessionID, text string) error {
	return f.record("send", "", session, "", text)
}

func (f *fakeBackend) Resume(_ context.Context, session SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "resume", session: session})
	if err := f.actionErr["resume"]; err != nil {
		if f.resumeStateOnError != nil {
			for i := range f.snapshot.Sessions {
				if f.snapshot.Sessions[i].ID == session {
					f.snapshot.Sessions[i].State = *f.resumeStateOnError
				}
			}
		}
		return err
	}
	for i := range f.snapshot.Sessions {
		if f.snapshot.Sessions[i].ID == session {
			f.snapshot.Sessions[i].State = StateActive
		}
	}
	return nil
}

func (f *fakeBackend) Stop(_ context.Context, session SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: "stop", session: session})
	if err := f.actionErr["stop"]; err != nil {
		return err
	}
	for i := range f.snapshot.Sessions {
		candidate := &f.snapshot.Sessions[i]
		if candidate.ID == session || candidate.ParentID == session {
			candidate.State = StateInterrupted
		}
	}
	return nil
}

func (f *fakeBackend) record(kind string, task TaskID, session SessionID, provider ProviderID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{kind: kind, task: task, session: session, provider: provider, text: text})
	return f.actionErr[kind]
}

func (f *fakeBackend) Next(ctx context.Context, cursor StreamCursor) (PublicEvent, error) {
	f.mu.Lock()
	f.nextCalls++
	f.nextActive++
	if f.nextActive > f.maxNext {
		f.maxNext = f.nextActive
	}
	f.calls = append(f.calls, fakeCall{kind: fmt.Sprintf("next:%d:%d", cursor.Generation, cursor.After)})
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.nextActive--
		f.mu.Unlock()
	}()
	select {
	case event := <-f.events:
		return event, nil
	case <-ctx.Done():
		return PublicEvent{}, ctx.Err()
	}
}

func (f *fakeBackend) callsOf(kind string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []fakeCall
	for _, call := range f.calls {
		if call.kind == kind {
			calls = append(calls, call)
		}
	}
	return calls
}

func (f *fakeBackend) replaceSnapshot(snapshot Snapshot) {
	f.mu.Lock()
	f.snapshot = cloneSnapshot(snapshot)
	f.mu.Unlock()
}

func loadedModel(t *testing.T, snapshot Snapshot, withEvents bool) (*Model, *fakeBackend, tea.Cmd) {
	t.Helper()
	fake := newFake(snapshot)
	config := Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake,
		Limits: Limits{DraftBytes: 256, FrameBytes: 256, UnknownTypeBytes: 64, PaneBytes: 2048, PaneFrames: 32},
	}
	if withEvents {
		config.Events = fake
	}
	model, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cmd := model.Init()
	if cmd == nil {
		t.Fatal("Init returned no metadata command")
	}
	message := cmd()
	_, next := model.Update(message)
	if !model.loaded {
		t.Fatal("model not loaded")
	}
	return model, fake, next
}

func testDraft(model *Model, id SessionID) editor {
	draft, ok := model.drafts[id]
	if !ok {
		draft = newEditor(model.limits.DraftBytes)
	}
	return draft
}

func testPane(model *Model, id SessionID) *pane {
	p := model.panes[id]
	if p == nil {
		p = newPane()
		model.panes[id] = p
	}
	return p
}

func runCmd(t *testing.T, model *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected command")
	}
	message := cmd()
	_, next := model.Update(message)
	return next
}

func updateKey(model *Model, name string) tea.Cmd {
	_, cmd := model.Update(testKey(name))
	return cmd
}

func testKey(name string) tea.KeyPressMsg {
	var code rune
	switch name {
	case "enter":
		code = tea.KeyEnter
	case "esc":
		code = tea.KeyEscape
	case "tab":
		code = tea.KeyTab
	case "up":
		code = tea.KeyUp
	case "down":
		code = tea.KeyDown
	case "left":
		code = tea.KeyLeft
	case "right":
		code = tea.KeyRight
	case "backspace":
		code = tea.KeyBackspace
	case "delete":
		code = tea.KeyDelete
	case "pgup":
		code = tea.KeyPgUp
	case "pgdown":
		code = tea.KeyPgDown
	case "home":
		code = tea.KeyHome
	case "end":
		code = tea.KeyEnd
	case "f1":
		code = tea.KeyF1
	case "space":
		code = tea.KeySpace
	default:
		runes := []rune(name)
		if len(runes) == 1 {
			return tea.KeyPressMsg(tea.Key{Code: runes[0], Text: name})
		}
		panic("unsupported test key: " + name)
	}
	return tea.KeyPressMsg(tea.Key{Code: code})
}

func baseSnapshot() Snapshot {
	t0 := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return Snapshot{
		StreamGeneration: 7,
		Tasks: []Task{
			{ID: "task-old", Title: "Older Task", CreatedAt: t0},
			{ID: "task-new", Title: "Newest Task", CreatedAt: t0.Add(time.Hour)},
		},
		Providers: []Provider{
			{ID: "codex", Name: "Codex", Enabled: true, ProbeStatus: ProbeReady,
				Capabilities: Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: false}},
			{ID: "worker", Name: "Worker", Enabled: true, ProbeStatus: ProbeReady,
				Capabilities: Capabilities{Subordinate: true}},
			{ID: "disabled-manager", Name: "Disabled Manager", ProbeStatus: ProbeDisconnected,
				Capabilities: Capabilities{Manager: true, Resume: false, Replay: true}},
		},
		Sessions: []Session{
			{ID: "manager-old", TaskID: "task-old", Role: RoleManager, Label: "Planning manager", ProviderID: "codex", State: StateDormant, ResumeAvailable: true, CreatedAt: t0},
			{ID: "manager-a", TaskID: "task-new", Role: RoleManager, Label: "Manager A", ProviderID: "codex", State: StateActive, ResumeAvailable: true, CreatedAt: t0},
			{ID: "child-answer", TaskID: "task-new", ParentID: "manager-a", Role: RoleSubordinate, Label: "Answer child", ProviderID: "worker", Mode: ModeAnswer, State: StateActive, CreatedAt: t0.Add(time.Minute)},
			{ID: "child-follow", TaskID: "task-new", ParentID: "manager-a", Role: RoleSubordinate, Label: "Follow child", ProviderID: "worker", Mode: ModeFollow, State: StateDormant, CreatedAt: t0.Add(2 * time.Minute)},
			{ID: "manager-b", TaskID: "task-new", Role: RoleManager, Label: "Manager B", ProviderID: "codex", State: StateActive, ResumeAvailable: true, CreatedAt: t0.Add(3 * time.Minute)},
		},
	}
}

type fakeSafeError struct{ code string }

func (e fakeSafeError) Error() string    { return "secret path /tmp/private credential=canary" }
func (e fakeSafeError) SafeCode() string { return e.code }
