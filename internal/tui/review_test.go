package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestNonActiveManagerCannotOwnOrMutateHiddenDraft(t *testing.T) {
	snapshot := baseSnapshot()
	for i := range snapshot.Sessions {
		if snapshot.Sessions[i].ID == "manager-a" {
			snapshot.Sessions[i].State = StateDormant
		}
	}
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.focus = focusList

	updateKey(model, "tab")
	if model.focus != focusList {
		t.Fatal("Tab moved focus into hidden input for dormant Manager")
	}
	model.Update(tea.PasteMsg{Content: "must-not-appear"})
	if _, exists := model.drafts["manager-a"]; exists {
		t.Fatal("Paste created a hidden draft for dormant Manager")
	}

	draft := newEditor(model.limits.DraftBytes)
	draft.insert("existing")
	model.drafts["manager-a"] = draft
	model.focus = focusInput // defensive stale-focus case
	model.Update(tea.PasteMsg{Content: "-mutation"})
	updateKey(model, "x")
	if got := model.drafts["manager-a"].value; got != "existing" {
		t.Fatalf("non-active hidden draft mutated to %q", got)
	}
	if model.focus != focusList {
		t.Fatal("stale non-active input focus did not return to list")
	}
}

func TestActiveManagerBecomingNonActiveReturnsFocusToList(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.focus = focusInput
	model.insertManagerText("draft preserved")
	draft := model.drafts["manager-a"]
	draft.left()
	model.drafts["manager-a"] = draft

	replacement := cloneSnapshot(model.snapshot)
	for i := range replacement.Sessions {
		if replacement.Sessions[i].ID == "manager-a" {
			replacement.Sessions[i].State = StateInterrupted
		}
	}
	model.applySnapshot(replacement)
	if model.focus != focusList {
		t.Fatal("non-active transition retained manager-input focus")
	}
	got := model.drafts["manager-a"]
	if got.value != draft.value || got.cursor != draft.cursor {
		t.Fatalf("state transition changed RAM draft/cursor: %#v", got)
	}
}

func TestResizeKeepsSelectedManagerVisibleAfterShrink(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-b"
	model.treeOffset = 0
	model.focus = focusInput
	model.insertManagerText("resize draft")
	draft := model.drafts["manager-b"]
	draft.left()
	model.drafts["manager-b"] = draft
	p := testPane(model, "manager-b")
	p.append(paneFrame{sequence: 1, text: "one", bytes: 3}, model.limits)
	p.scrollUp(2)
	beforeOffset, beforeFollow := p.offsetFromTail, p.followTail

	model.Update(tea.WindowSizeMsg{Width: 40, Height: 16})
	visible := model.treeLines()
	visible = visible[:min(len(visible), model.treeVisibleHeight())]
	found := false
	for _, line := range visible {
		if strings.Contains(line.text, "Manager B") {
			found = true
		}
	}
	if !found || model.selectedManager != "manager-b" {
		t.Fatalf("selected Manager not visible after shrink: offset=%d lines=%#v", model.treeOffset, visible)
	}
	got := model.drafts["manager-b"]
	if got.value != draft.value || got.cursor != draft.cursor || p.offsetFromTail != beforeOffset || p.followTail != beforeFollow {
		t.Fatal("resize changed draft/cursor or pane scroll intent")
	}
}

func TestAggregateDraftAndPaneBoundsAcrossManagers(t *testing.T) {
	snapshot := baseSnapshot()
	fake := newFake(snapshot)
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake,
		Limits: Limits{
			DraftBytes: 10, AggregateDraftBytes: 10,
			FrameBytes: 16, PaneBytes: 32, PaneFrames: 8,
			AggregatePaneBytes: 20, AggregatePaneFrames: 3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	model.Update(model.Init()())
	if len(model.drafts) != 0 || len(model.panes) != 0 {
		t.Fatal("metadata load eagerly allocated per-Manager content RAM")
	}
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.insertManagerText("12345678")
	model.selectedManager = "manager-b"
	model.insertManagerText("ABCDEFGH")
	if got := model.totalDraftBytes(); got != 10 || len(model.drafts["manager-b"].value) != 2 {
		t.Fatalf("aggregate draft bytes=%d drafts=%#v", got, model.drafts)
	}

	for sequence, manager := range []SessionID{"manager-a", "manager-b", "manager-a", "manager-b"} {
		event := frameEvent(7, uint64(sequence+1), manager, "0123456789")
		if err := model.applyEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	bytes, frames := model.totalPaneUsage()
	if bytes > 20 || frames > 3 {
		t.Fatalf("aggregate pane usage bytes=%d frames=%d", bytes, frames)
	}
	var sequences []uint64
	for _, p := range model.panes {
		for _, frame := range p.frames {
			sequences = append(sequences, frame.sequence)
		}
	}
	for _, old := range []uint64{1, 2} {
		for _, sequence := range sequences {
			if sequence == old {
				t.Fatalf("old aggregate frame %d was not evicted: %v", old, sequences)
			}
		}
	}
}

func TestCustomLimitsClampToHardProductionBounds(t *testing.T) {
	huge := int(^uint(0) >> 1)
	limits := normalizeLimits(Limits{
		DraftBytes: huge, FrameBytes: huge, UnknownTypeBytes: huge,
		PaneBytes: huge, PaneFrames: huge, AggregateDraftBytes: huge,
		AggregatePaneBytes: huge, AggregatePaneFrames: huge,
	})
	if limits.DraftBytes != hardMaxDraftBytes || limits.FrameBytes != hardMaxFrameBytes ||
		limits.UnknownTypeBytes != hardMaxUnknownTypeBytes || limits.PaneBytes != hardMaxPaneBytes ||
		limits.PaneFrames != hardMaxPaneFrames || limits.AggregateDraftBytes != hardMaxAggregateDraftBytes ||
		limits.AggregatePaneBytes != hardMaxAggregatePaneBytes || limits.AggregatePaneFrames != hardMaxAggregatePaneFrames {
		t.Fatalf("custom limits escaped hard caps: %#v", limits)
	}
	defaults := normalizeLimits(Limits{DraftBytes: -1, AggregatePaneBytes: -1})
	if defaults.DraftBytes <= 0 || defaults.AggregatePaneBytes <= 0 {
		t.Fatalf("non-positive custom values disabled limits: %#v", defaults)
	}
}

func TestSnapshotEntityCountsHaveHardBounds(t *testing.T) {
	tests := []Snapshot{
		{Tasks: make([]Task, maxSnapshotTasks+1)},
		{Sessions: make([]Session, maxSnapshotSessions+1)},
		{Providers: make([]Provider, maxSnapshotProviders+1)},
	}
	for index, snapshot := range tests {
		if err := validateSnapshot(snapshot); err == nil {
			t.Fatalf("oversized hostile Snapshot count %d was accepted", index)
		}
	}
	large := Snapshot{Tasks: make([]Task, maxSnapshotTasks)}
	chunk := strings.Repeat("x", maxTaskTitleBytes)
	for i := range large.Tasks {
		large.Tasks[i] = Task{ID: TaskID(itoa(i + 1)), Title: chunk}
	}
	if err := validateSnapshot(large); err == nil {
		t.Fatal("oversized aggregate Snapshot metadata bytes were accepted")
	}
}

func TestQuitFromAttachedNestedScreensDetachesExactlyOnce(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Model, *fakeBackend)
	}{
		{
			name:  "add-manager",
			setup: func(model *Model, _ *fakeBackend) { updateKey(model, "a") },
		},
		{
			name: "settings-from-add-manager",
			setup: func(model *Model, _ *fakeBackend) {
				updateKey(model, "a")
				updateKey(model, "enter")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := baseSnapshot()
			if test.name == "settings-from-add-manager" {
				for i := range snapshot.Providers {
					snapshot.Providers[i].Enabled = false
				}
			}
			model, fake, _ := loadedModel(t, snapshot, false)
			runCmd(t, model, updateKey(model, "enter"))
			test.setup(model, fake)
			if test.name == "add-manager" && model.screen != screenAddManager {
				t.Fatal("not on Add Manager")
			}
			if test.name == "settings-from-add-manager" && model.screen != screenSettings {
				t.Fatal("not on Settings reached from Add Manager")
			}
			cmd := updateCtrlC(model)
			if cmd == nil {
				t.Fatal("Ctrl+C returned no quit command")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("quit command did not return Bubble Tea QuitMsg")
			}
			calls := fake.callsOf("detach")
			if len(calls) != 1 || calls[0].task != "task-new" {
				t.Fatalf("detach calls = %#v, want task-new exactly once", calls)
			}
			if len(fake.callsOf("stop")) != 0 {
				t.Fatal("quit stopped runtime work")
			}
		})
	}
}

func TestQuitCancelsBeforeBestEffortDetach(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	runCmd(t, model, updateKey(model, "enter"))
	fake.detachWaitForCancel = true

	cmd := updateCtrlC(model)
	if cmd == nil {
		t.Fatal("Ctrl+C returned no quit command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case message := <-done:
		if _, ok := message.(tea.QuitMsg); !ok {
			t.Fatalf("quit command message = %T, want Bubble Tea QuitMsg", message)
		}
	case <-time.After(time.Second):
		model.Close()
		t.Fatal("quit waited on Detach before cancelling terminal-owned work")
	}
	if calls := fake.callsOf("detach"); len(calls) != 1 || calls[0].task != "task-new" {
		t.Fatalf("detach calls = %#v, want task-new exactly once", calls)
	}
}

func TestHistoricalManagerResumeAvailabilityIsMechanical(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		snapshot := baseSnapshot()
		for i := range snapshot.Sessions {
			if snapshot.Sessions[i].ID == "manager-a" {
				snapshot.Sessions[i].State = StateDormant
				snapshot.Sessions[i].ResumeAvailable = false
			}
		}
		model, fake, _ := loadedModel(t, snapshot, false)
		defer model.Close()
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		if !strings.Contains(model.View().Content, "Resume unavailable") {
			t.Fatalf("historical Manager lacks Resume-unavailable wording:\n%s", model.View().Content)
		}
		if cmd := updateKey(model, "r"); cmd != nil || len(fake.callsOf("resume")) != 0 {
			t.Fatal("unavailable Resume invoked runtime")
		}
	})

	t.Run("available-exact", func(t *testing.T) {
		snapshot := baseSnapshot()
		for i := range snapshot.Sessions {
			if snapshot.Sessions[i].ID == "manager-a" {
				snapshot.Sessions[i].State = StateDormant
			}
		}
		model, fake, _ := loadedModel(t, snapshot, false)
		defer model.Close()
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		assertContainsAll(t, model.View().Content, "Exact Session Resume [Resume]", "state=Dormant")
		assertHitActions(t, model.render(), mouseResume)
		runCmd(t, model, updateKey(model, "r"))
		calls := fake.callsOf("resume")
		if len(calls) != 1 || calls[0].session != "manager-a" || len(fake.callsOf("attach")) != 0 {
			t.Fatalf("Resume/Attach calls = %#v / %#v", calls, fake.callsOf("attach"))
		}
		manager, _ := model.session("manager-a")
		if manager.State != StateActive {
			t.Fatalf("resumed Manager state = %s", manager.State)
		}
	})
}

func TestResumeFailureRefreshesPersistedInterruptedMetadata(t *testing.T) {
	snapshot := baseSnapshot()
	for i := range snapshot.Sessions {
		if snapshot.Sessions[i].ID == "manager-a" {
			snapshot.Sessions[i].State = StateDormant
		}
	}
	model, fake, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	interrupted := StateInterrupted
	fake.resumeStateOnError = &interrupted
	fake.actionErr["resume"] = fakeSafeError{code: "resume-denied"}
	fake.mu.Lock()
	fake.snapshot.ThroughSequence = model.streamSequence + 1
	fake.mu.Unlock()

	runCmd(t, model, updateKey(model, "r"))
	manager, _ := model.session("manager-a")
	if manager.State != StateInterrupted {
		t.Fatalf("failed Resume did not refresh persisted state: %s", manager.State)
	}
	if model.status != "Manager resume failed (resume-denied)" {
		t.Fatalf("Resume SafeError status = %q", model.status)
	}
	if model.snapshot.ThroughSequence != 1 || model.streamSequence != 1 {
		t.Fatalf("ahead refresh watermark snapshot=%d stream=%d", model.snapshot.ThroughSequence, model.streamSequence)
	}
	for _, private := range []string{"/tmp/private", "credential=canary"} {
		if strings.Contains(model.View().Content, private) {
			t.Fatalf("Resume error leaked raw content %q", private)
		}
	}
	fake.mu.Lock()
	calls := append([]fakeCall(nil), fake.calls...)
	fake.mu.Unlock()
	if len(calls) < 2 || calls[len(calls)-2].kind != "resume" || calls[len(calls)-1].kind != "snapshot" {
		t.Fatalf("failed Resume did not refresh immediately: %#v", calls)
	}
}

func TestResumeSnapshotFailureIsPrimarySanitizedAndDoesNotApplyMetadata(t *testing.T) {
	snapshot := baseSnapshot()
	for i := range snapshot.Sessions {
		if snapshot.Sessions[i].ID == "manager-a" {
			snapshot.Sessions[i].State = StateDormant
		}
	}
	model, fake, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	interrupted := StateInterrupted
	fake.resumeStateOnError = &interrupted
	fake.actionErr["resume"] = fakeSafeError{code: "resume-denied"}
	fake.snapshotErr = errors.New("snapshot credential=refresh-canary path=/private/catalog")

	runCmd(t, model, updateKey(model, "r"))
	manager, _ := model.session("manager-a")
	if manager.State != StateDormant {
		t.Fatalf("failed snapshot was applied: Manager state=%s", manager.State)
	}
	want := "Metadata refresh failed; Manager resume failed (resume-denied)"
	if model.status != want {
		t.Fatalf("failure precedence/status = %q, want %q", model.status, want)
	}
	for _, private := range []string{"refresh-canary", "/private/catalog", "/tmp/private", "credential=canary"} {
		if strings.Contains(model.status, private) || strings.Contains(model.View().Content, private) {
			t.Fatalf("combined failure leaked raw content %q", private)
		}
	}
}

func TestProviderStatusesUseApprovedUserLabels(t *testing.T) {
	tests := []struct {
		status ProbeStatus
		want   string
	}{
		{ProbeReady, "Connected"}, {ProbeUnknown, "Not connected"}, {ProbeDisconnected, "Disconnected"},
		{ProbeNeedsRepair, "Needs repair"}, {ProbeMissing, "Missing connector"}, {ProbeChecking, "Checking"},
	}
	for _, test := range tests {
		if got := probeStatusLabel(test.status); got != test.want {
			t.Errorf("status %q label = %q, want %q", test.status, got, test.want)
		}
	}
	providers := make([]Provider, 0, len(tests))
	for index, test := range tests {
		providers = append(providers, Provider{
			ID: ProviderID(itoa(index + 1)), Name: "Provider " + itoa(index+1), Enabled: true, ProbeStatus: test.status,
		})
	}
	model, _, _ := loadedModel(t, Snapshot{StreamGeneration: 1, Providers: providers}, false)
	defer model.Close()
	model.screen = screenSettings
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := model.View().Content
	for _, test := range tests {
		if !strings.Contains(view, "status:"+test.want) {
			t.Errorf("Settings did not render approved status label %q:\n%s", test.want, view)
		}
	}
}

func TestBackSerializesPendingAttachBeforeExactDetach(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	fake.attachStarted = make(chan struct{})
	fake.attachRelease = make(chan struct{})
	fake.attachIgnoreCancel = true
	fake.detachStarted = make(chan struct{})

	attachCmd := updateKey(model, "enter")
	if attachCmd == nil || model.screen != screenWorkspace {
		t.Fatal("opening Task did not start Attach")
	}
	attachMessage := make(chan tea.Msg, 1)
	go func() { attachMessage <- attachCmd() }()
	select {
	case <-fake.attachStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not start")
	}

	detachCmd := updateKey(model, "b")
	if detachCmd == nil || model.screen != screenTasks {
		t.Fatal("Back did not schedule serialized Detach")
	}
	detachMessage := make(chan tea.Msg, 1)
	go func() { detachMessage <- detachCmd() }()
	select {
	case <-fake.detachStarted:
		t.Fatal("Detach overtook the still-running Attach")
	case <-time.After(50 * time.Millisecond):
	}
	close(fake.attachRelease)

	var messages []tea.Msg
	for _, channel := range []chan tea.Msg{attachMessage, detachMessage} {
		select {
		case message := <-channel:
			messages = append(messages, message)
		case <-time.After(2 * time.Second):
			t.Fatal("serialized lifecycle command did not complete")
		}
	}
	// Delivery order is intentionally irrelevant; pointer ownership prevents a
	// late Attach result from resurrecting a viewer after Back.
	for i := len(messages) - 1; i >= 0; i-- {
		model.Update(messages[i])
	}
	if model.attachedTask != "" || model.pendingAttach != nil || model.detachOp != nil {
		t.Fatalf("viewer ownership survived Back: attached=%q pending=%v detach=%v",
			model.attachedTask, model.pendingAttach != nil, model.detachOp != nil)
	}
	if calls := fake.callsOf("detach"); len(calls) != 1 || calls[0].task != "task-new" {
		t.Fatalf("Detach calls = %#v, want exact Task once", calls)
	}
	if _, ok := updateCtrlC(model)().(tea.QuitMsg); !ok {
		t.Fatal("quit did not return Bubble Tea QuitMsg")
	}
	if calls := fake.callsOf("detach"); len(calls) != 1 {
		t.Fatalf("quit duplicated completed Detach: %#v", calls)
	}
}

func TestSameGenerationSnapshotCannotOverwriteNewerEvent(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()
	updated, _ := model.session("child-follow")
	updated.State = StateActive
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 1, Kind: EventSessionUpsert,
		TaskID: updated.TaskID, SessionID: updated.ID, Session: updated,
	}
	next = runCmd(t, model, next)
	if next == nil || model.snapshot.ThroughSequence != 1 {
		t.Fatal("accepted event did not advance the metadata watermark")
	}
	stale := baseSnapshot()
	stale.ThroughSequence = 0
	model.Update(snapshotMsg{action: actionReload, snapshot: stale, hasSnapshot: true})
	got, _ := model.session("child-follow")
	if got.State != StateActive || model.streamSequence != 1 || model.snapshot.ThroughSequence != 1 {
		t.Fatalf("stale snapshot overwrote event: state=%s stream=%d through=%d",
			got.State, model.streamSequence, model.snapshot.ThroughSequence)
	}
	if !strings.Contains(model.status, "stale stream watermark") {
		t.Fatalf("stale refresh was not reported mechanically: %q", model.status)
	}
}

func TestAddManagerFailureStillRefreshesTruthfulSessionMetadata(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	created := Session{
		ID: "manager-created-then-failed", TaskID: "task-new", Role: RoleManager,
		Label: "Truthful interrupted manager", ProviderID: "codex", State: StateInterrupted,
		CreatedAt: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	fake.addManagerOnError = &created
	fake.actionErr["add-manager"] = errors.New("connector start failed")
	model.currentTask = "task-new"
	model.screen = screenAddManager
	model.addProvider = "codex"
	model.managerLabel.insert(created.Label)
	runCmd(t, model, model.createManager())

	got, ok := model.session(created.ID)
	if !ok || got.State != StateInterrupted || model.screen != screenWorkspace || model.selectedManager != created.ID {
		t.Fatalf("error-path metadata not visible: session=%#v ok=%v screen=%v selected=%q",
			got, ok, model.screen, model.selectedManager)
	}
	if !strings.Contains(model.View().Content, created.Label) || !strings.Contains(model.status, "Add Manager failed") {
		t.Fatalf("truthful failed Manager/status missing:\n%s", model.View().Content)
	}
	fake.mu.Lock()
	calls := append([]fakeCall(nil), fake.calls...)
	fake.mu.Unlock()
	if len(calls) < 3 || calls[len(calls)-2].kind != "add-manager" || calls[len(calls)-1].kind != "snapshot" {
		t.Fatalf("Add Manager failure did not force metadata refresh: %#v", calls)
	}
}

func TestSettingsActionsAreStateAwareAndRequireExactProviderTarget(t *testing.T) {
	model, _, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.screen = screenSettings
	if view := model.View().Content; !strings.Contains(view, "[Disable]") || strings.Contains(view, "[Enable]") {
		t.Fatalf("enabled provider has incorrect toggle label:\n%s", view)
	}
	model.selectedProvider = "disabled-manager"
	if view := model.View().Content; !strings.Contains(view, "[Enable]") || strings.Contains(view, "[Disable]") {
		t.Fatalf("disabled provider has incorrect toggle label:\n%s", view)
	}

	empty, fake, _ := loadedModel(t, Snapshot{StreamGeneration: 1}, false)
	defer empty.Close()
	empty.screen = screenSettings
	rendered := empty.render()
	for _, hit := range rendered.hits {
		if hit.action == mouseProviderEnable || hit.action == mouseProviderAuth || hit.action == mouseProviderRecheck {
			t.Fatalf("no-provider Settings exposed active provider hitbox: %#v", hit)
		}
	}
	for _, key := range []string{"e", "a", "r"} {
		if cmd := updateKey(empty, key); cmd != nil {
			t.Fatalf("no-provider key %q returned an action command", key)
		}
	}
	if len(fake.callsOf("enable"))+len(fake.callsOf("authenticate"))+len(fake.callsOf("recheck")) != 0 {
		t.Fatal("no-provider Settings invoked a provider action")
	}
	assertContainsAll(t, empty.View().Content, "No provider selected", "unavailable")
}

func TestDuplicateTruncatedSessionsHaveExactKeyboardAndMouseDetails(t *testing.T) {
	snapshot := baseSnapshot()
	prefix := "duplicate-session-" + strings.Repeat("x", 48)
	firstID := SessionID(prefix + "-one")
	secondID := SessionID(prefix + "-two")
	for i := range snapshot.Sessions {
		if snapshot.Sessions[i].ID == "child-answer" {
			snapshot.Sessions[i].ID = firstID
			snapshot.Sessions[i].Label = strings.Repeat("identical-label-", 8)
		}
		if snapshot.Sessions[i].ID == "child-follow" {
			snapshot.Sessions[i].ID = secondID
			snapshot.Sessions[i].Label = strings.Repeat("identical-label-", 8)
		}
	}
	model, _, _ := loadedModel(t, snapshot, false)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !strings.Contains(model.View().Content, "…") {
		t.Fatal("fixture did not exercise truncated duplicate tree rows")
	}
	updateKey(model, "v")
	if !model.details {
		t.Fatal("keyboard did not open exact Session details")
	}
	found := map[SessionID]bool{}
	for range model.detailSessions() {
		view := model.View().Content
		for _, id := range []SessionID{firstID, secondID} {
			if strings.Contains(view, string(id)) {
				found[id] = true
			}
		}
		updateKey(model, "right")
	}
	if !found[firstID] || !found[secondID] {
		t.Fatalf("exact duplicate identities were not accessible: %#v", found)
	}
	assertHitActions(t, model.render(), mouseDetailsPrev, mouseDetailsNext, mouseDetailsClose, mouseHelp)
	updateKey(model, "esc")
	assertHitActions(t, model.render(), mouseDetails)
	assertContainsAll(t, model.View().Content, "Manager manager-a", "Codex", "active", "subordinates:2")
	childLine := -1
	for _, line := range model.allTreeLines() {
		if line.sessionID == firstID {
			childLine = line.lineIndex
			break
		}
	}
	if childLine < 0 {
		t.Fatal("exact subordinate row not found")
	}
	click(t, model, mouseManager, childLine)
	current := model.detailSessions()[model.detailIndex]
	if !model.details || current.ID != firstID {
		t.Fatalf("mouse did not open exact subordinate identity: details=%v current=%q", model.details, current.ID)
	}
}

func updateCtrlC(model *Model) tea.Cmd {
	_, cmd := model.Update(tea.KeyPressMsg(tea.Key{Code: 'c', Mod: tea.ModCtrl}))
	return cmd
}
