package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDetailsHelpMouseIsModalAndCloseRestoresManagerInputFocus(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.focus = focusInput
	model.insertManagerText("focus and draft survive")
	draft := model.drafts["manager-a"]
	draft.left()
	model.drafts["manager-a"] = draft

	click(t, model, mouseDetails)
	if !model.details || model.focus != focusList || model.detailsReturnFocus != focusInput {
		t.Fatalf("details did not capture invoking input focus: details=%v focus=%v return=%v",
			model.details, model.focus, model.detailsReturnFocus)
	}
	click(t, model, mouseHelp)
	if !model.help || !model.details {
		t.Fatal("Help did not open modally over Session Details")
	}
	click(t, model, mouseHelp)
	if model.help || !model.details {
		t.Fatal("mouse Close Help did not return to Session Details")
	}
	click(t, model, mouseDetailsClose)
	if model.details || model.focus != focusInput {
		t.Fatalf("closing details did not restore Manager input focus: details=%v focus=%v", model.details, model.focus)
	}
	if got := model.drafts["manager-a"]; got.value != draft.value || got.cursor != draft.cursor {
		t.Fatalf("modal round trip changed draft/cursor: %#v", got)
	}
}

func TestSubordinateDetailsTreeInvocationNeverRestoresComposerFocus(t *testing.T) {
	t.Run("keyboard", func(t *testing.T) {
		model, _, _ := loadedModel(t, baseSnapshot(), false)
		defer model.Close()
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		model.focus = focusList

		updateKey(model, "v")
		updateKey(model, "right")
		if !model.details || model.detailSessions()[model.detailIndex].ID != "child-answer" ||
			model.detailsReturnFocus != focusList {
			t.Fatalf("keyboard subordinate Details provenance = details:%v index:%d return:%v",
				model.details, model.detailIndex, model.detailsReturnFocus)
		}
		updateKey(model, "esc")
		if model.details || model.focus != focusList {
			t.Fatalf("keyboard subordinate Details restored focus %v", model.focus)
		}
	})

	t.Run("mouse-from-composer", func(t *testing.T) {
		model, _, _ := loadedModel(t, baseSnapshot(), false)
		defer model.Close()
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		model.focus = focusInput
		model.insertManagerText("composer must remain untouched")
		before := model.drafts["manager-a"]

		click(t, model, mouseManager, 1) // child-answer in the visible Session tree.
		if !model.details || model.detailSessions()[model.detailIndex].ID != "child-answer" ||
			model.detailsReturnFocus != focusList {
			t.Fatalf("pointer subordinate Details provenance = details:%v index:%d return:%v",
				model.details, model.detailIndex, model.detailsReturnFocus)
		}
		click(t, model, mouseDetailsClose)
		if model.details || model.focus != focusList {
			t.Fatalf("pointer subordinate Details restored composer focus %v", model.focus)
		}
		if got := model.drafts["manager-a"]; got.value != before.value || got.cursor != before.cursor {
			t.Fatalf("pointer subordinate Details mutated composer: %#v", got)
		}
	})
}

func TestOpenTaskDoesNotEnterUnattachedWorkspaceWhenActionPending(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.pending = actionRecheck
	model.pendingToken = 99
	model.currentTask = ""
	model.screen = screenTasks
	selected := model.selectedTask

	if cmd := updateKey(model, "enter"); cmd != nil {
		t.Fatal("pending action unexpectedly returned Attach command")
	}
	if model.screen != screenTasks || model.currentTask != "" || model.selectedTask != selected {
		t.Fatalf("pending action exposed unattached workspace: screen=%v current=%q selected=%q",
			model.screen, model.currentTask, model.selectedTask)
	}
	if model.pendingAttach != nil || len(fake.callsOf("attach")) != 0 {
		t.Fatal("pending action created an Attach attempt")
	}
}

func TestLongTaskAndProviderLabelsPreserveMechanicalColumns(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			snapshot := baseSnapshot()
			snapshot.Tasks[1].Title = "Task-" + strings.Repeat("very-long-", 40)
			snapshot.Providers[0].Name = "Provider-" + strings.Repeat("very-long-", 40)
			snapshot.Providers[0].ProbeStatus = ProbeDisconnected
			model, _, _ := loadedModel(t, snapshot, false)
			defer model.Close()
			model.Update(tea.WindowSizeMsg{Width: width, Height: 24})

			taskLine := lineContaining(model.View().Content, "A:3 D:1")
			if taskLine == "" || !strings.HasSuffix(taskLine, "A:3 D:1") || !strings.Contains(taskLine, "…") {
				t.Fatalf("task counts were not fixed/truncated at width %d: %q", width, taskLine)
			}
			assertContainsAll(t, model.View().Content, "Active Dormant")

			updateKey(model, "s")
			providerLine := lineContaining(model.View().Content, "enabled | status:Disconnected")
			if providerLine == "" || !strings.HasSuffix(providerLine, "enabled | status:Disconnected") ||
				!strings.Contains(providerLine, "…") {
				t.Fatalf("provider columns were not fixed/truncated at width %d: %q", width, providerLine)
			}
		})
	}
}

func TestWorkspaceUsesWorkingAndAnsweredStateLabels(t *testing.T) {
	snapshot := baseSnapshot()
	for i := range snapshot.Sessions {
		if snapshot.Sessions[i].ID == "child-answer" {
			snapshot.Sessions[i].State = StateDormant
		}
	}
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	lines := model.allTreeLines()
	var joined []string
	for _, line := range lines {
		joined = append(joined, line.text)
	}
	tree := strings.Join(joined, "\n")
	assertContainsAll(t, tree, "Manager Working", "Subordinate Answer Answered", "Subordinate Follow Dormant")
	if strings.Contains(tree, "Manager active") || strings.Contains(tree, "Answer dormant") {
		t.Fatalf("raw mechanical state escaped into workspace rows:\n%s", tree)
	}
	model.openSessionDetails("child-answer")
	assertContainsAll(t, model.View().Content, "State: Answered", "Mode: answer")
}

func TestPaneEvictionAlwaysLeavesTruthfulVisibleMarker(t *testing.T) {
	snapshot := baseSnapshot()
	fake := newFake(snapshot)
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake,
		Limits: Limits{
			FrameBytes: 64, PaneBytes: 128, PaneFrames: 1,
			AggregatePaneBytes: 128, AggregatePaneFrames: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	model.Update(model.Init()())
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"

	if err := model.applyEvent(frameEvent(7, 1, "manager-a", "first-evicted")); err != nil {
		t.Fatal(err)
	}
	if err := model.applyEvent(frameEvent(7, 2, "manager-a", "second-retained")); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join(model.selectedPaneLines(40), "\n")
	assertContainsAll(t, lines, "[older frames evicted]", "second-retained")

	if err := model.applyEvent(frameEvent(7, 3, "manager-b", "aggregate-newest")); err != nil {
		t.Fatal(err)
	}
	model.selectedManager = "manager-a"
	lines = strings.Join(model.selectedPaneLines(40), "\n")
	if !strings.Contains(lines, "[older frames evicted]") || strings.Contains(lines, "No public ACP frames") {
		t.Fatalf("aggregate eviction became untruthful: %q", lines)
	}
	if bytes, frames := model.totalPaneUsage(); bytes > model.limits.AggregatePaneBytes || frames != 1 {
		t.Fatalf("aggregate usage after eviction = bytes:%d frames:%d", bytes, frames)
	}
}

func TestReverseMixedResizeTransitionsPreserveLiveAndFailureState(t *testing.T) {
	snapshot := baseSnapshot()
	for i := range snapshot.Providers {
		if snapshot.Providers[i].ID == "codex" {
			snapshot.Providers[i].ProbeStatus = ProbeDisconnected
		}
	}
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.selectedTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.focus = focusInput
	model.insertManagerText("resize keeps this draft")
	draft := model.drafts["manager-a"]
	draft.left()
	model.drafts["manager-a"] = draft
	if err := model.applyEvent(frameEvent(7, 1, "manager-a", "live-resize-frame")); err != nil {
		t.Fatal(err)
	}
	p := model.panes["manager-a"]
	model.status = safeFailure("Provider recheck", fakeSafeError{code: "disconnected"})

	for _, size := range []struct{ width, height int }{{120, 40}, {60, 18}, {40, 16}, {80, 24}} {
		model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := model.View()
		assertViewBounds(t, view, size.width, size.height)
		assertContainsAll(t, view.Content, "live-resize-frame", "Provider recheck failed (disconnected)")
		provider, _ := model.provider("codex")
		gotDraft := model.drafts["manager-a"]
		if provider.ProbeStatus != ProbeDisconnected || model.currentTask != "task-new" ||
			model.selectedManager != "manager-a" || model.focus != focusInput ||
			gotDraft.value != draft.value || gotDraft.cursor != draft.cursor ||
			len(p.frames) != 1 || p.frames[0].text != "live-resize-frame" {
			t.Fatalf("%dx%d changed live/failure interaction state", size.width, size.height)
		}
		if strings.Contains(view.Content, "/tmp/private") || strings.Contains(view.Content, "credential=canary") {
			t.Fatalf("%dx%d resize exposed raw SafeError", size.width, size.height)
		}
	}
}

func TestMaxBoundedSnapshotUsesIndexedViewportWork(t *testing.T) {
	tasks := make([]Task, maxSnapshotTasks)
	tasks[0] = Task{ID: "bulk", Title: "Bulk task"}
	for i := 1; i < len(tasks); i++ {
		tasks[i] = Task{ID: TaskID(fmt.Sprintf("t%05d", i)), Title: "task"}
	}
	sessions := make([]Session, 0, maxSnapshotSessions)
	for i := 0; i < maxSnapshotSessions/2; i++ {
		managerID := SessionID(fmt.Sprintf("m%05d", i))
		sessions = append(sessions,
			Session{ID: managerID, TaskID: "bulk", Role: RoleManager, ProviderID: "p", State: StateActive},
			Session{ID: SessionID(fmt.Sprintf("c%05d", i)), TaskID: "bulk", ParentID: managerID,
				Role: RoleSubordinate, ProviderID: "p", Mode: ModeAnswer, State: StateDormant},
		)
	}
	snapshot := Snapshot{
		StreamGeneration: 1, Tasks: tasks, Sessions: sessions,
		Providers: []Provider{{ID: "p", Name: "Provider", Enabled: true, ProbeStatus: ProbeReady,
			Capabilities: Capabilities{Manager: true, Subordinate: true}}},
	}
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	if len(model.tasksByID) != maxSnapshotTasks || len(model.sessionsByID) != maxSnapshotSessions ||
		len(model.treeNodesByTask["bulk"]) != maxSnapshotSessions {
		t.Fatalf("derived index cardinality mismatch: tasks=%d sessions=%d tree=%d",
			len(model.tasksByID), len(model.sessionsByID), len(model.treeNodesByTask["bulk"]))
	}
	active, dormant, total := model.taskCounts("bulk")
	if active != maxSnapshotSessions/2 || dormant != maxSnapshotSessions/2 || total != maxSnapshotSessions {
		t.Fatalf("indexed counts=%d/%d/%d", active, dormant, total)
	}
	if len(model.visibleTasks()) != maxSnapshotTasks {
		t.Fatal("max bounded task filtering lost rows")
	}

	model.currentTask = "bulk"
	model.screen = screenWorkspace
	model.selectedManager = "m00000"
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	visible := model.treeLines()
	if len(visible) > model.treeVisibleHeight() {
		t.Fatalf("render formatted %d hierarchy rows for %d-row viewport", len(visible), model.treeVisibleHeight())
	}
	view := model.View()
	assertViewBounds(t, view, 80, 24)
	assertContainsAll(t, view.Content, "Manager m00000", "subordinates:1", "Working")
}

func lineContaining(view, needle string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimRight(line, " ")
		}
	}
	return ""
}
