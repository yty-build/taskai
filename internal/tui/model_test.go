package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTasksFirstOrderingFiltersAndCounts(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()

	if model.screen != screenTasks {
		t.Fatalf("initial screen = %v, want Tasks", model.screen)
	}
	visible := model.visibleTasks()
	if len(visible) != 1 || visible[0].ID != "task-new" {
		t.Fatalf("Active Tasks = %#v, want task-new only", visible)
	}
	active, dormant, total := model.taskCounts("task-new")
	if active != 3 || dormant != 1 || total != 4 {
		t.Fatalf("task-new counts = %d/%d/%d, want 3/1/4", active, dormant, total)
	}
	updateKey(model, "tab")
	visible = model.visibleTasks()
	if got := []TaskID{visible[0].ID, visible[1].ID}; got[0] != "task-new" || got[1] != "task-old" {
		t.Fatalf("All ordering = %v, want newest-created descending", got)
	}
	view := model.View().Content
	for _, expected := range []string{"TaskAI / Tasks", "[All]", "A:3 D:1", "A:0 D:1"} {
		if !strings.Contains(view, expected) {
			t.Errorf("Tasks view missing %q:\n%s", expected, view)
		}
	}
	if calls := fake.callsOf("authenticate"); len(calls) != 0 {
		t.Fatalf("Tasks navigation authenticated implicitly: %#v", calls)
	}
}

func TestEqualCreatedTaskTieBreakIsDeterministic(t *testing.T) {
	snapshot := baseSnapshot()
	snapshot.Tasks[0].CreatedAt = snapshot.Tasks[1].CreatedAt
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.filter = filterAll
	tasks := model.visibleTasks()
	if tasks[0].ID != "task-old" || tasks[1].ID != "task-new" {
		t.Fatalf("equal-time order = %q, %q; want descending ID", tasks[0].ID, tasks[1].ID)
	}
}

func TestEmptyAndNoProviderStatesNeverAuthenticate(t *testing.T) {
	model, fake, _ := loadedModel(t, Snapshot{StreamGeneration: 1}, false)
	defer model.Close()
	view := model.View().Content
	for _, expected := range []string{"TaskAI / Tasks", "No Tasks yet", "No providers enabled", "[Add Task]", "[Settings]"} {
		if !strings.Contains(view, expected) {
			t.Errorf("empty view missing %q:\n%s", expected, view)
		}
	}
	updateKey(model, "s")
	if !strings.Contains(model.View().Content, "No provider connectors registered") {
		t.Fatalf("Settings empty state missing:\n%s", model.View().Content)
	}
	if got := len(fake.callsOf("authenticate")); got != 0 {
		t.Fatalf("implicit authentication count = %d", got)
	}
}

func TestZeroSessionTaskIsActiveAndLoadingIsTruthful(t *testing.T) {
	fake := newFake(Snapshot{
		StreamGeneration: 1,
		Tasks:            []Task{{ID: "new-task", Title: "No Sessions"}},
	})
	model, err := New(Config{Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	if view := model.View().Content; !strings.Contains(view, "TaskAI / Tasks") || !strings.Contains(view, "Loading metadata") {
		t.Fatalf("initial loading screen is not Tasks-first:\n%s", view)
	}
	model.Update(model.Init()())
	visible := model.visibleTasks()
	if len(visible) != 1 || visible[0].ID != "new-task" {
		t.Fatalf("zero-Session Task missing from Active: %#v", visible)
	}
}

func TestAddTaskPreservesExactBoundedTitle(t *testing.T) {
	model, fake, _ := loadedModel(t, Snapshot{StreamGeneration: 1}, false)
	defer model.Close()
	updateKey(model, "a")
	title := "  Exact 世界 title  "
	model.Update(tea.PasteMsg{Content: title})
	cmd := updateKey(model, "enter")
	runCmd(t, model, cmd)
	calls := fake.callsOf("add-task")
	if len(calls) != 1 || calls[0].text != title {
		t.Fatalf("AddTask calls = %#v, want exact title %q", calls, title)
	}
	if model.screen != screenTasks || model.taskDraft.value != "" {
		t.Fatalf("successful Add Task did not return cleanly to Tasks")
	}
}

func TestAddTaskSelectsExactCreatedTaskAndSuppressesRepeatSubmit(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	updateKey(model, "a")
	model.Update(tea.PasteMsg{Content: "Created exactly once"})
	first := updateKey(model, "enter")
	if duplicate := updateKey(model, "enter"); duplicate != nil {
		t.Fatal("repeat submit produced a second command while Add Task was pending")
	}
	runCmd(t, model, first)
	if calls := fake.callsOf("add-task"); len(calls) != 1 {
		t.Fatalf("Add Task calls = %d, want exactly one", len(calls))
	}
	if model.selectedTask != "created-task-1" {
		t.Fatalf("selected Task = %q, want exact created Task", model.selectedTask)
	}
}

func TestProviderSettingsActionsAreExplicitAndCapabilitiesTruthful(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	updateKey(model, "s")
	view := model.View().Content
	for _, expected := range []string{
		"Provider Settings", "explicit actions only", "Manager:yes", "Subordinate:yes", "Resume:yes", "Replay:no",
	} {
		if !strings.Contains(view, expected) {
			t.Errorf("Settings missing %q:\n%s", expected, view)
		}
	}
	if len(fake.callsOf("authenticate")) != 0 {
		t.Fatal("opening Settings authenticated implicitly")
	}
	runCmd(t, model, updateKey(model, "a"))
	if calls := fake.callsOf("authenticate"); len(calls) != 1 || calls[0].provider != "codex" {
		t.Fatalf("Authenticate calls = %#v", calls)
	}
	runCmd(t, model, updateKey(model, "space"))
	if calls := fake.callsOf("enable"); len(calls) != 1 || calls[0].enabled {
		t.Fatalf("Enable calls = %#v, want explicit disable", calls)
	}
	runCmd(t, model, updateKey(model, "r"))
	if calls := fake.callsOf("recheck"); len(calls) != 1 {
		t.Fatalf("Recheck calls = %#v", calls)
	}
}

func TestSettingsErrorsExposeOnlySafeCode(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	fake.actionErr["authenticate"] = fakeSafeError{code: "needs-repair"}
	updateKey(model, "s")
	runCmd(t, model, updateKey(model, "a"))
	view := model.View().Content
	if !strings.Contains(view, "Authentication failed (needs-repair)") {
		t.Fatalf("safe failure missing:\n%s", view)
	}
	for _, secret := range []string{"/tmp/private", "credential=canary"} {
		if strings.Contains(view, secret) {
			t.Fatalf("rendered private error content %q", secret)
		}
	}
}

func TestAddManagerFiltersAndRoutesToSettings(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	runCmd(t, model, updateKey(model, "enter"))
	updateKey(model, "a")
	providers := model.managerProviders()
	if len(providers) != 1 || providers[0].ID != "codex" {
		t.Fatalf("Manager providers = %#v, want enabled conformed codex only", providers)
	}
	view := model.View().Content
	if strings.Contains(view, "Disabled Manager") || strings.Contains(view, "Worker") {
		t.Fatalf("unavailable provider offered in Add Manager:\n%s", view)
	}
	if !strings.Contains(view, "Subordinate-only providers are delegated by the Manager") {
		t.Fatalf("Add Manager did not explain subordinate-only providers:\n%s", view)
	}
	updateKey(model, "tab")
	model.Update(tea.PasteMsg{Content: "  exact manager label  "})
	runCmd(t, model, updateKey(model, "enter"))
	calls := fake.callsOf("add-manager")
	if len(calls) != 1 || calls[0].provider != "codex" || calls[0].text != "  exact manager label  " {
		t.Fatalf("AddManager calls = %#v", calls)
	}
	if model.selectedManager != "created-manager-1" {
		t.Fatalf("selected manager = %q, want newly created", model.selectedManager)
	}

	snapshot := baseSnapshot()
	for i := range snapshot.Providers {
		snapshot.Providers[i].Enabled = false
	}
	model2, fake2, _ := loadedModel(t, snapshot, false)
	defer model2.Close()
	runCmd(t, model2, updateKey(model2, "enter"))
	updateKey(model2, "a")
	if !strings.Contains(model2.View().Content, "No enabled Manager-capable provider") {
		t.Fatalf("missing unavailable route:\n%s", model2.View().Content)
	}
	if !strings.Contains(model2.View().Content, "Subordinate-only providers cannot start a top-level Session") {
		t.Fatalf("missing subordinate-only explanation:\n%s", model2.View().Content)
	}
	updateKey(model2, "enter")
	if model2.screen != screenSettings {
		t.Fatalf("unavailable Add Manager did not route to Settings")
	}
	if len(fake2.callsOf("authenticate")) != 0 {
		t.Fatal("routing to Settings authenticated implicitly")
	}
}

func TestWorkspaceHierarchyManagerOnlyInputDetachAndStop(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	runCmd(t, model, updateKey(model, "enter"))
	view := model.View().Content
	for _, expected := range []string{
		"Manager A", "Codex", "Manager", "Answer child", "Subordinate", "Answer", "Follow child", "Follow", "active", "dormant",
	} {
		if !strings.Contains(view, expected) {
			t.Errorf("workspace missing %q:\n%s", expected, view)
		}
	}
	if calls := fake.callsOf("attach"); len(calls) != 1 || calls[0].task != "task-new" {
		t.Fatalf("attach calls = %#v", calls)
	}

	updateKey(model, "tab")
	prompt := "  manager 世界 prompt  "
	model.Update(tea.PasteMsg{Content: prompt})
	cmd := updateKey(model, "enter")
	// Switching the visible selection before completion cannot redirect or
	// clear the invocation that was already correlated to manager-a.
	model.selectedManager = "manager-b"
	if cmd == nil {
		t.Fatalf("send command missing: focus=%v pending=%v selected=%q draft=%q", model.focus, model.pending, model.selectedManager, model.drafts["manager-a"].value)
	}
	runCmd(t, model, cmd)
	calls := fake.callsOf("send")
	if len(calls) != 1 || calls[0].session != "manager-a" || calls[0].text != prompt {
		t.Fatalf("send calls = %#v", calls)
	}
	if model.drafts["manager-a"].value != "" {
		t.Fatal("completed exact-manager draft was not cleared")
	}
	for _, call := range calls {
		if call.session == "child-answer" || call.session == "child-follow" {
			t.Fatal("direct subordinate input occurred")
		}
	}

	model.focus = focusList
	model.selectedManager = "manager-a"
	cmd = updateKey(model, "b")
	if model.screen != screenTasks {
		t.Fatal("Back did not detach the workspace view immediately")
	}
	runCmd(t, model, cmd)
	if len(fake.callsOf("detach")) != 1 || len(fake.callsOf("stop")) != 0 {
		t.Fatalf("Back lifecycle calls detach=%#v stop=%#v", fake.callsOf("detach"), fake.callsOf("stop"))
	}

	runCmd(t, model, updateKey(model, "enter"))
	model.selectedManager = "manager-a"
	updateKey(model, "x")
	if !model.confirmStop {
		t.Fatal("Stop did not require confirmation")
	}
	cmd = updateKey(model, "y")
	runCmd(t, model, cmd)
	if calls := fake.callsOf("stop"); len(calls) != 1 || calls[0].session != "manager-a" {
		t.Fatalf("stop calls = %#v", calls)
	}
	for _, id := range []SessionID{"manager-a", "child-answer", "child-follow"} {
		session, _ := model.session(id)
		if session.State != StateInterrupted {
			t.Errorf("%s state = %s, want interrupted", id, session.State)
		}
	}
	managerB, _ := model.session("manager-b")
	if managerB.State != StateActive {
		t.Fatalf("unrelated manager state = %s", managerB.State)
	}
}

func TestWorkspaceReopenPreservesManagerDraftAndSelectionInRAM(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	runCmd(t, model, updateKey(model, "enter"))
	model.selectedManager = "manager-b"
	draft := testDraft(model, "manager-b")
	draft.insert("local draft")
	draft.left()
	model.drafts["manager-b"] = draft
	runCmd(t, model, updateKey(model, "b"))
	if model.screen != screenTasks {
		t.Fatal("did not return to Tasks")
	}
	runCmd(t, model, updateKey(model, "enter"))
	if model.selectedManager != "manager-b" {
		t.Fatalf("reopen selected %q, want manager-b", model.selectedManager)
	}
	if got := model.drafts["manager-b"]; got.value != draft.value || got.cursor != draft.cursor {
		t.Fatalf("reopen changed RAM-only draft: %#v", got)
	}
	if len(fake.callsOf("attach")) != 2 || len(fake.callsOf("authenticate")) != 0 {
		t.Fatalf("reopen attach/auth calls = %#v / %#v", fake.callsOf("attach"), fake.callsOf("authenticate"))
	}
}

func TestManagerInputUsesMechanicalLifecycleState(t *testing.T) {
	for _, state := range []SessionState{StateStarting, StateDormant, StateInterrupted, StateClosed} {
		t.Run(string(state), func(t *testing.T) {
			snapshot := baseSnapshot()
			for i := range snapshot.Sessions {
				if snapshot.Sessions[i].ID == "manager-a" {
					snapshot.Sessions[i].State = state
				}
			}
			model, fake, _ := loadedModel(t, snapshot, false)
			defer model.Close()
			model.currentTask = "task-new"
			model.screen = screenWorkspace
			model.selectedManager = "manager-a"
			view := model.View().Content
			if !strings.Contains(view, "state="+sessionStateLabel(Session{Role: RoleManager, State: state})) {
				t.Fatalf("state %s input wording missing:\n%s", state, view)
			}
			model.focus = focusInput
			draft := testDraft(model, "manager-a")
			draft.insert("must not send")
			model.drafts["manager-a"] = draft
			if cmd := updateKey(model, "enter"); cmd != nil || len(fake.callsOf("send")) != 0 {
				t.Fatalf("state %s allowed manager input", state)
			}
		})
	}
}

func TestHelpPreservesUnderlyingState(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	selected := model.selectedTask
	updateKey(model, "f1")
	if !model.help || !strings.Contains(model.View().Content, "TaskAI / Help") {
		t.Fatal("F1 did not open Help")
	}
	updateKey(model, "esc")
	if model.help || model.screen != screenTasks || model.selectedTask != selected {
		t.Fatal("closing Help changed underlying navigation state")
	}
}

func TestMouseParityForVisibleActions(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	click(t, model, mouseAddTask)
	model.Update(tea.PasteMsg{Content: "Mouse Task"})
	cmd := click(t, model, mouseSubmitTask)
	runCmd(t, model, cmd)
	if len(fake.callsOf("add-task")) != 1 {
		t.Fatal("mouse Add Task did not invoke action")
	}

	click(t, model, mouseSettings)
	cmd = click(t, model, mouseProviderAuth)
	runCmd(t, model, cmd)
	if len(fake.callsOf("authenticate")) != 1 {
		t.Fatal("mouse Authenticate did not invoke explicit action")
	}
	click(t, model, mouseBack)
	click(t, model, mouseTask, 1)
	cmd = click(t, model, mouseTaskOpen)
	runCmd(t, model, cmd)
	click(t, model, mouseInput)
	model.Update(tea.PasteMsg{Content: "mouse prompt"})
	cmd = click(t, model, mouseSend)
	runCmd(t, model, cmd)
	if calls := fake.callsOf("send"); len(calls) != 1 || calls[0].session != "manager-a" {
		t.Fatalf("mouse send calls = %#v", calls)
	}
}

func click(t *testing.T, model *Model, action mouseAction, optionalIndex ...int) tea.Cmd {
	t.Helper()
	wantIndex := -1
	if len(optionalIndex) > 0 {
		wantIndex = optionalIndex[0]
	}
	view := model.View()
	rendered := model.render()
	for _, hit := range rendered.hits {
		if hit.action != action || (wantIndex >= 0 && hit.index != wantIndex) {
			continue
		}
		mouse := tea.MouseClickMsg(tea.Mouse{X: hit.x1, Y: hit.y1, Button: tea.MouseLeft})
		cmd := view.OnMouse(mouse)
		if cmd == nil {
			t.Fatalf("mouse action %v returned no translation command", action)
		}
		message := cmd()
		_, next := model.Update(message)
		return next
	}
	t.Fatalf("visible mouse action %v has no hitbox", action)
	return nil
}
