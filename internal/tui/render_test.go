package tui

import (
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestResizeMatrixPreservesInteractionAndLifecycleState(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.currentTask = "task-new"
	model.selectedTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-b"
	model.focus = focusInput
	model.treeOffset = 1
	draft := testDraft(model, "manager-b")
	draft.insert("draft 世界")
	draft.left()
	model.drafts["manager-b"] = draft
	p := testPane(model, "manager-b")
	p.append(paneFrame{sequence: 2, text: "second", bytes: 6}, model.limits)
	p.append(paneFrame{sequence: 4, text: "fourth", bytes: 6}, model.limits)
	p.scrollUp(2)
	model.streamSequence = 9

	beforeTasks := append([]Task(nil), model.snapshot.Tasks...)
	beforeSessions := append([]Session(nil), model.snapshot.Sessions...)
	beforeDraft := model.drafts["manager-b"]
	beforeOffset, beforeFollow := p.offsetFromTail, p.followTail
	for _, size := range []struct{ width, height int }{{40, 16}, {60, 18}, {80, 24}, {120, 40}} {
		model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		if model.currentTask != "task-new" || model.selectedTask != "task-new" || model.selectedManager != "manager-b" {
			t.Fatalf("%dx%d changed Task/Manager selection", size.width, size.height)
		}
		if model.focus != focusInput || model.treeOffset != 1 || model.streamSequence != 9 {
			t.Fatalf("%dx%d changed focus/tree/order state", size.width, size.height)
		}
		if got := model.drafts["manager-b"]; got.value != beforeDraft.value || got.cursor != beforeDraft.cursor {
			t.Fatalf("%dx%d changed draft/cursor: %#v", size.width, size.height, got)
		}
		if p.offsetFromTail != beforeOffset || p.followTail != beforeFollow {
			t.Fatalf("%dx%d changed scroll intent", size.width, size.height)
		}
		if !reflect.DeepEqual(model.snapshot.Tasks, beforeTasks) || !reflect.DeepEqual(model.snapshot.Sessions, beforeSessions) {
			t.Fatalf("%dx%d changed ordering or lifecycle metadata", size.width, size.height)
		}
		assertViewBounds(t, model.View(), size.width, size.height)
		cursor := model.View().Cursor
		if cursor == nil || cursor.X < 0 || cursor.X >= size.width || cursor.Y < 0 || cursor.Y >= size.height {
			t.Fatalf("%dx%d invalid editor cursor: %#v", size.width, size.height, cursor)
		}
	}
}

func TestSemanticScreensAtRequiredSizes(t *testing.T) {
	for _, size := range []struct{ width, height int }{{40, 16}, {60, 18}, {80, 24}, {120, 40}} {
		t.Run(strings.ReplaceAll(strings.Join([]string{itoa(size.width), "x", itoa(size.height)}, ""), " ", ""), func(t *testing.T) {
			model, _, _ := loadedModel(t, baseSnapshot(), false)
			defer model.Close()
			model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			tasks := model.View()
			assertViewBounds(t, tasks, size.width, size.height)
			assertContainsAll(t, tasks.Content, "TaskAI / Tasks", "Created ↓", "Active", "Add Task", "Settings")
			assertHitActions(t, model.render(), mouseTaskOpen, mouseAddTask, mouseSettings, mouseHelp)

			updateKey(model, "s")
			settings := model.View()
			assertViewBounds(t, settings, size.width, size.height)
			assertContainsAll(t, settings.Content, "Provider Settings", "Manager:yes", "Replay:no")
			assertHitActions(t, model.render(), mouseBack, mouseProviderEnable, mouseProviderAuth, mouseProviderRecheck, mouseHelp)

			updateKey(model, "esc")
			runCmd(t, model, updateKey(model, "enter"))
			workspace := model.View()
			assertViewBounds(t, workspace, size.width, size.height)
			assertContainsAll(t, workspace.Content, "TaskAI / Task", "Manager", "Subordinate", "Answer", "Follow", "Input")
			if size.width < 72 {
				assertContainsAll(t, workspace.Content,
					"Manager manager-a | Manager A", "Codex | Working | sub:2 | A:3 D:1")
			} else {
				assertContainsAll(t, workspace.Content,
					"Manager manager-a | Manager A | Codex | Working | subordinates:2")
			}
			assertHitActions(t, model.render(), mouseBack, mouseAddManager, mouseStop, mouseHelp, mouseInput, mouseSend)

			updateKey(model, "f1")
			help := model.View()
			assertViewBounds(t, help, size.width, size.height)
			assertContainsAll(t, help.Content, "TaskAI / Help", "Back detaches", "Subordinates")
		})
	}
}

func TestHostileMetadataCannotEmitTerminalControls(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Tasks[1].Title = "title\x1b]52;c;clipboard\a\u202e"
	snapshot.Providers[0].Name = "provider\x1b[31mred"
	snapshot.Sessions[1].Label = "manager\r\nspoof"
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.filter = filterAll
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	view := model.View().Content
	for _, control := range []rune{'\x1b', '\a', '\r', '\u202e'} {
		if strings.ContainsRune(view, control) {
			t.Fatalf("raw terminal control U+%04X reached view", control)
		}
	}
	for _, escaped := range []string{"\\x1b", "\\x07", "\\x0d"} {
		if !strings.Contains(view, escaped) {
			t.Errorf("safe metadata view missing %q:\n%s", escaped, view)
		}
	}
	if got := safeInlinePrefix("line\nnext", 64); got != "line\\nnext" {
		t.Fatalf("inline newline representation = %q", got)
	}
}

func TestEditorIsUTF8SafeBoundedAndControlFree(t *testing.T) {
	editor := newEditor(8)
	editor.insert("ab世界z")
	if editor.value != "ab世界" || len(editor.value) > 10 {
		t.Fatalf("bounded UTF-8 value = %q bytes=%d", editor.value, len(editor.value))
	}
	editor.left()
	if editor.cursor != len("ab世") {
		t.Fatalf("rune-safe left cursor = %d", editor.cursor)
	}
	editor.backspace()
	if editor.value != "ab界" || editor.cursor != len("ab") {
		t.Fatalf("rune-safe backspace = %#v", editor)
	}
	editor.insert("\x1b\n\u202eOK")
	if strings.ContainsAny(editor.value, "\x1b\n") || strings.ContainsRune(editor.value, '\u202e') {
		t.Fatalf("editor retained hostile controls: %q", editor.value)
	}
}

func TestMouseWheelParityPreservesLiveScrollIntent(t *testing.T) {
	keyboard, _, _ := loadedModel(t, baseSnapshot(), false)
	defer keyboard.Close()
	pointer, _, _ := loadedModel(t, baseSnapshot(), false)
	defer pointer.Close()
	prepare := func(model *Model) *pane {
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		p := testPane(model, "manager-a")
		for i := 0; i < 20; i++ {
			p.append(paneFrame{sequence: uint64(i + 1), text: "line", bytes: 4}, model.limits)
		}
		return p
	}
	keyboardPane := prepare(keyboard)
	pointerPane := prepare(pointer)

	updateKey(keyboard, "pgup")
	wheelPane(t, pointer, tea.MouseWheelUp)
	if keyboardPane.followTail || keyboardPane.offsetFromTail == 0 ||
		pointerPane.followTail || pointerPane.offsetFromTail == 0 {
		t.Fatal("keyboard and pointer did not both transition from live edge to historical scroll")
	}
	if !reflect.DeepEqual(keyboardPane.frames, pointerPane.frames) ||
		keyboard.selectedManager != pointer.selectedManager || keyboard.focus != pointer.focus {
		t.Fatal("scroll transition changed frames, selection, or focus")
	}

	updateKey(keyboard, "end")
	for !pointerPane.followTail {
		wheelPane(t, pointer, tea.MouseWheelDown)
	}
	if !keyboardPane.followTail || keyboardPane.offsetFromTail != 0 ||
		!pointerPane.followTail || pointerPane.offsetFromTail != 0 {
		t.Fatal("keyboard End and pointer wheel did not converge on the same live-edge state")
	}
}

func wheelPane(t *testing.T, model *Model, button tea.MouseButton) {
	t.Helper()
	rendered := model.render()
	view := model.View()
	mouse := tea.MouseWheelMsg(tea.Mouse{X: rendered.pane.x1, Y: rendered.pane.y1, Button: button})
	command := view.OnMouse(mouse)
	if command == nil {
		t.Fatal("visible public ACP pane wheel had no transition command")
	}
	model.Update(command())
}

func TestInvalidHierarchyFailsClosed(t *testing.T) {
	tests := map[string]func(*Snapshot){
		"manager-parent":        func(snapshot *Snapshot) { snapshot.Sessions[1].ParentID = "manager-b" },
		"subordinate-no-parent": func(snapshot *Snapshot) { snapshot.Sessions[2].ParentID = "" },
		"cross-task-parent":     func(snapshot *Snapshot) { snapshot.Sessions[2].ParentID = "manager-old" },
		"nested-subordinate":    func(snapshot *Snapshot) { snapshot.Sessions[3].ParentID = "child-answer" },
		"bad-state":             func(snapshot *Snapshot) { snapshot.Sessions[1].State = "working" },
		"manager-mode":          func(snapshot *Snapshot) { snapshot.Sessions[1].Mode = ModeAnswer },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := baseSnapshot()
			mutate(&snapshot)
			model, _, _ := loadedModelUnchecked(t, snapshot)
			defer model.Close()
			if !strings.Contains(model.status, "invalid snapshot") || len(model.snapshot.Sessions) != 0 {
				t.Fatalf("invalid hierarchy accepted: status=%q sessions=%d", model.status, len(model.snapshot.Sessions))
			}
		})
	}
}

func TestProductionModelHasNoPollingOrPersistenceImports(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	directory := filename[:strings.LastIndex(filename, "/")]
	files := []string{"doc.go", "editor.go", "model.go", "render.go", "types.go"}
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), directory+"/"+name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, "\"")
			for _, forbidden := range []string{"os", "io", "log", "database/sql", "net/http", "time"} {
				if path == forbidden && !(name == "types.go" && path == "time") {
					t.Errorf("%s imports forbidden runtime/persistence/polling package %q", name, path)
				}
			}
		}
	}
	configType := reflect.TypeOf(Config{})
	providerType := reflect.TypeOf(Provider{})
	for _, typ := range []reflect.Type{configType, providerType} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, forbidden := range []string{"credential", "secret", "argv", "command", "working", "log", "cache", "store", "transcript", "persist"} {
				if name == forbidden || strings.HasPrefix(name, forbidden) {
					t.Errorf("%s exposes forbidden field %s", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func loadedModelUnchecked(t *testing.T, snapshot Snapshot) (*Model, *fakeBackend, tea.Cmd) {
	t.Helper()
	fake := newFake(snapshot)
	model, err := New(Config{Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake})
	if err != nil {
		t.Fatal(err)
	}
	message := model.Init()()
	_, next := model.Update(message)
	return model, fake, next
}

func assertViewBounds(t *testing.T, view tea.View, width, height int) {
	t.Helper()
	lines := strings.Split(view.Content, "\n")
	if len(lines) != height {
		t.Fatalf("view height = %d, want %d", len(lines), height)
	}
	for row, line := range lines {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("row %d width = %d, want <= %d: %q", row, got, width, line)
		}
	}
}

func assertContainsAll(t *testing.T, value string, expected ...string) {
	t.Helper()
	for _, substring := range expected {
		if !strings.Contains(value, substring) {
			t.Errorf("view missing %q:\n%s", substring, value)
		}
	}
}

func assertHitActions(t *testing.T, rendered renderResult, expected ...mouseAction) {
	t.Helper()
	seen := make(map[mouseAction]bool)
	for _, hit := range rendered.hits {
		if hit.x2 > hit.x1 && hit.y2 > hit.y1 {
			seen[hit.action] = true
		}
	}
	for _, action := range expected {
		if !seen[action] {
			t.Errorf("visible action %v has no non-empty mouse hitbox", action)
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
