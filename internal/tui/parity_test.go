package tui

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type parityCase struct {
	name     string
	snapshot func() Snapshot
	prepare  func(*Model)
	keyboard func(*Model) tea.Cmd
	action   mouseAction
	index    int
}

type parityState struct {
	view                                  string
	screen, returnTo                      screen
	focus                                 focus
	filter                                taskFilter
	selectedTask, currentTask             TaskID
	selectedProvider, addProvider         ProviderID
	selectedManager, detailsReturnSession SessionID
	status                                string
	help, details, confirmStop            bool
	detailsReturnFocus                    focus
	detailIndex, detailScroll, treeOffset int
	pending                               actionKind
	attachedTask                          TaskID
	snapshot                              Snapshot
	taskDraft, managerLabel               editor
	drafts                                map[SessionID]editor
	calls                                 []fakeCall
}

// TestVisiblePointerActionsHaveKeyboardTransitionParity exercises the state
// transition behind every visible pointer action. It intentionally compares
// complete post-transition model state and backend calls, rather than merely
// asserting that a hitbox exists.
func TestVisiblePointerActionsHaveKeyboardTransitionParity(t *testing.T) {
	base := func() Snapshot { return baseSnapshot() }
	empty := func() Snapshot { return Snapshot{StreamGeneration: 1} }
	dormant := func() Snapshot {
		snapshot := baseSnapshot()
		for i := range snapshot.Sessions {
			if snapshot.Sessions[i].ID == "manager-a" {
				snapshot.Sessions[i].State = StateDormant
			}
		}
		return snapshot
	}
	twoManagers := func() Snapshot {
		snapshot := baseSnapshot()
		snapshot.Providers = append(snapshot.Providers, Provider{
			ID: "other-manager", Name: "Other Manager", Enabled: true, ProbeStatus: ProbeReady,
			Capabilities: Capabilities{Manager: true},
		})
		return snapshot
	}
	noManagerProvider := func() Snapshot {
		snapshot := baseSnapshot()
		for i := range snapshot.Providers {
			snapshot.Providers[i].Enabled = false
		}
		return snapshot
	}

	tasks := func(model *Model) {}
	addTask := func(model *Model) {
		model.screen = screenAddTask
		model.focus = focusInput
	}
	settings := func(model *Model) {
		model.screen = screenSettings
		model.returnTo = screenTasks
		model.focus = focusList
	}
	workspace := func(model *Model) {
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		model.focus = focusList
	}
	addManager := func(model *Model) {
		workspace(model)
		model.openAddManager()
	}
	details := func(model *Model) {
		workspace(model)
		model.openSessionDetails("manager-a")
	}
	key := func(name string) func(*Model) tea.Cmd {
		return func(model *Model) tea.Cmd { return updateKey(model, name) }
	}
	keys := func(names ...string) func(*Model) tea.Cmd {
		return func(model *Model) tea.Cmd {
			var command tea.Cmd
			for _, name := range names {
				command = updateKey(model, name)
			}
			return command
		}
	}

	cases := []parityCase{
		{name: "tasks-help", snapshot: base, prepare: tasks, keyboard: key("f1"), action: mouseHelp},
		{name: "tasks-select", snapshot: base, prepare: func(model *Model) { model.filter = filterAll }, keyboard: key("down"), action: mouseTask, index: 1},
		{name: "tasks-active", snapshot: base, prepare: func(model *Model) { model.filter = filterAll }, keyboard: key("tab"), action: mouseTaskActive},
		{name: "tasks-all", snapshot: base, prepare: tasks, keyboard: key("tab"), action: mouseTaskAll},
		{name: "tasks-open", snapshot: base, prepare: tasks, keyboard: key("enter"), action: mouseTaskOpen},
		{name: "tasks-add", snapshot: base, prepare: tasks, keyboard: key("a"), action: mouseAddTask},
		{name: "tasks-settings", snapshot: base, prepare: tasks, keyboard: key("s"), action: mouseSettings},

		{name: "add-task-back", snapshot: base, prepare: addTask, keyboard: key("esc"), action: mouseBack},
		{name: "add-task-submit", snapshot: base, prepare: func(model *Model) {
			addTask(model)
			model.taskDraft.insert("Exact pointer parity Task")
		}, keyboard: key("enter"), action: mouseSubmitTask},
		{name: "add-task-empty-error", snapshot: empty, prepare: addTask, keyboard: key("enter"), action: mouseSubmitTask},
		{name: "add-task-help", snapshot: base, prepare: addTask, keyboard: key("f1"), action: mouseHelp},

		{name: "settings-back-tasks", snapshot: base, prepare: settings, keyboard: key("esc"), action: mouseBack},
		{name: "settings-back-add-manager", snapshot: base, prepare: func(model *Model) {
			settings(model)
			model.returnTo = screenAddManager
			model.currentTask = "task-new"
		}, keyboard: key("esc"), action: mouseBack},
		{name: "settings-select-provider", snapshot: base, prepare: settings, keyboard: key("down"), action: mouseProvider, index: 1},
		{name: "settings-enable", snapshot: base, prepare: settings, keyboard: key("space"), action: mouseProviderEnable},
		{name: "settings-authenticate", snapshot: base, prepare: settings, keyboard: key("a"), action: mouseProviderAuth},
		{name: "settings-recheck", snapshot: base, prepare: settings, keyboard: key("r"), action: mouseProviderRecheck},
		{name: "settings-help", snapshot: base, prepare: settings, keyboard: key("f1"), action: mouseHelp},

		{name: "workspace-back-detach", snapshot: base, prepare: func(model *Model) {
			workspace(model)
			model.attachedTask = "task-new"
		}, keyboard: key("esc"), action: mouseBack},
		{name: "workspace-manager-select", snapshot: base, prepare: workspace, keyboard: key("down"), action: mouseManager, index: 3},
		{name: "workspace-manager-select-from-input", snapshot: base, prepare: func(model *Model) {
			workspace(model)
			model.focus = focusInput
			model.insertManagerText("draft remains with its original Manager")
		}, keyboard: keys("esc", "down"), action: mouseManager, index: 3},
		{name: "workspace-subordinate-details", snapshot: base, prepare: workspace, keyboard: keys("v", "right"), action: mouseManager, index: 1},
		{name: "workspace-input", snapshot: base, prepare: workspace, keyboard: key("tab"), action: mouseInput},
		{name: "workspace-send", snapshot: base, prepare: func(model *Model) {
			workspace(model)
			model.focus = focusInput
			model.insertManagerText("Exact pointer parity prompt")
		}, keyboard: key("enter"), action: mouseSend},
		{name: "workspace-resume", snapshot: dormant, prepare: workspace, keyboard: key("r"), action: mouseResume},
		{name: "workspace-add-manager", snapshot: base, prepare: workspace, keyboard: key("a"), action: mouseAddManager},
		{name: "workspace-stop-prompt", snapshot: base, prepare: workspace, keyboard: key("x"), action: mouseStop},
		{name: "workspace-details", snapshot: base, prepare: workspace, keyboard: key("v"), action: mouseDetails},
		{name: "workspace-help", snapshot: base, prepare: workspace, keyboard: key("f1"), action: mouseHelp},

		{name: "stop-confirm", snapshot: base, prepare: func(model *Model) {
			workspace(model)
			model.confirmStop = true
		}, keyboard: key("y"), action: mouseConfirmStop},
		{name: "stop-cancel", snapshot: base, prepare: func(model *Model) {
			workspace(model)
			model.confirmStop = true
		}, keyboard: key("n"), action: mouseCancelStop},

		{name: "add-manager-back", snapshot: base, prepare: addManager, keyboard: key("esc"), action: mouseBack},
		{name: "add-manager-select-provider", snapshot: twoManagers, prepare: addManager, keyboard: key("down"), action: mouseAddProvider, index: 1},
		{name: "add-manager-label", snapshot: base, prepare: addManager, keyboard: key("tab"), action: mouseLabel},
		{name: "add-manager-create", snapshot: base, prepare: func(model *Model) {
			addManager(model)
			model.managerLabel.insert("Pointer parity manager")
		}, keyboard: key("enter"), action: mouseCreateManager},
		{name: "add-manager-route-settings", snapshot: noManagerProvider, prepare: addManager, keyboard: key("enter"), action: mouseRouteSettings},
		{name: "add-manager-help", snapshot: base, prepare: addManager, keyboard: key("f1"), action: mouseHelp},

		{name: "details-previous", snapshot: base, prepare: func(model *Model) {
			details(model)
			model.moveDetail(1)
		}, keyboard: key("left"), action: mouseDetailsPrev},
		{name: "details-next", snapshot: base, prepare: details, keyboard: key("right"), action: mouseDetailsNext},
		{name: "details-close", snapshot: base, prepare: details, keyboard: key("esc"), action: mouseDetailsClose},
		{name: "details-help", snapshot: base, prepare: details, keyboard: key("f1"), action: mouseHelp},
		{name: "help-close", snapshot: base, prepare: func(model *Model) { model.help = true }, keyboard: key("esc"), action: mouseHelp},
	}

	covered := make(map[mouseAction]bool)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			left, leftFake, _ := loadedModel(t, test.snapshot(), false)
			defer left.Close()
			right, rightFake, _ := loadedModel(t, test.snapshot(), false)
			defer right.Close()
			test.prepare(left)
			test.prepare(right)

			finishParityCommand(left, test.keyboard(left))
			finishParityCommand(right, click(t, right, test.action, test.index))
			want := captureParityState(left, leftFake)
			got := captureParityState(right, rightFake)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pointer transition differs from keyboard\n--- keyboard ---\n%#v\n--- pointer ---\n%#v", want, got)
			}
		})
		covered[test.action] = true
	}
	for action := mouseHelp; action <= mouseDetailsClose; action++ {
		if !covered[action] {
			t.Errorf("mouse action %v lacks a transition-level parity case", action)
		}
	}
}

func finishParityCommand(model *Model, command tea.Cmd) {
	for command != nil {
		message := command()
		if _, quit := message.(tea.QuitMsg); quit {
			return
		}
		_, command = model.Update(message)
	}
}

func captureParityState(model *Model, fake *fakeBackend) parityState {
	fake.mu.Lock()
	calls := append([]fakeCall(nil), fake.calls...)
	fake.mu.Unlock()
	drafts := make(map[SessionID]editor, len(model.drafts))
	for id, draft := range model.drafts {
		drafts[id] = draft
	}
	return parityState{
		view:   model.View().Content,
		screen: model.screen, returnTo: model.returnTo, focus: model.focus, filter: model.filter,
		selectedTask: model.selectedTask, currentTask: model.currentTask,
		selectedProvider: model.selectedProvider, addProvider: model.addProvider,
		selectedManager: model.selectedManager, detailsReturnSession: model.detailsReturnSession,
		status: model.status, help: model.help, details: model.details, confirmStop: model.confirmStop,
		detailsReturnFocus: model.detailsReturnFocus, detailIndex: model.detailIndex,
		detailScroll: model.detailScroll, treeOffset: model.treeOffset,
		pending: model.pending, attachedTask: model.attachedTask,
		snapshot: cloneSnapshot(model.snapshot), taskDraft: model.taskDraft, managerLabel: model.managerLabel,
		drafts: drafts, calls: calls,
	}
}
