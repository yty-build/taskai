package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestPushStreamPreservesOrderAndSessionIsolation(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()
	if next == nil || !model.streamWaiting {
		t.Fatal("validated snapshot did not arm one-shot event wait")
	}
	events := []PublicEvent{
		frameEvent(7, 1, "manager-a", "A-one"),
		frameEvent(7, 2, "manager-b", "B-only"),
		frameEvent(7, 3, "manager-a", "A-two"),
	}
	for _, event := range events {
		fake.events <- event
		next = runCmd(t, model, next)
		if next == nil {
			t.Fatalf("stream not re-armed after sequence %d", event.Sequence)
		}
	}
	if fake.maxNext != 1 || fake.nextActive != 0 {
		t.Fatalf("one-shot waits active/max = %d/%d, want 0/1", fake.nextActive, fake.maxNext)
	}
	framesA := model.panes["manager-a"].frames
	if len(framesA) != 2 || framesA[0].sequence != 1 || framesA[1].sequence != 3 ||
		framesA[0].text != "A-one" || framesA[1].text != "A-two" {
		t.Fatalf("manager-a frames = %#v", framesA)
	}
	framesB := model.panes["manager-b"].frames
	if len(framesB) != 1 || framesB[0].sequence != 2 || framesB[0].text != "B-only" {
		t.Fatalf("manager-b frames = %#v", framesB)
	}

	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	view := model.View().Content
	if !strings.Contains(view, "A-one") || !strings.Contains(view, "A-two") || strings.Contains(view, "B-only") {
		t.Fatalf("selected pane cross-contaminated:\n%s", view)
	}
	model.selectedManager = "manager-b"
	view = model.View().Content
	if !strings.Contains(view, "B-only") || strings.Contains(view, "A-one") {
		t.Fatalf("manager-b pane cross-contaminated:\n%s", view)
	}
}

func TestUI602BurstPreservesAll258FramesInExactOrder(t *testing.T) {
	snapshot := baseSnapshot()
	fake := newFake(snapshot)
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake, Events: fake,
		Limits: Limits{
			FrameBytes: 64, PaneBytes: 32 << 10, PaneFrames: 300,
			AggregatePaneBytes: 64 << 10, AggregatePaneFrames: 600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	_, next := model.Update(model.Init()())
	if next == nil {
		t.Fatal("validated Snapshot did not arm the stream")
	}

	const burst = 258
	for sequence := 1; sequence <= burst; sequence++ {
		fake.events <- frameEvent(7, uint64(sequence), "manager-a", fmt.Sprintf("burst-%03d", sequence))
	}
	for sequence := 1; sequence <= burst; sequence++ {
		next = runCmd(t, model, next)
		if next == nil {
			t.Fatalf("stream stopped during burst at sequence %d", sequence)
		}
	}
	p := model.panes["manager-a"]
	if p == nil || len(p.frames) != burst || p.evicted {
		t.Fatalf("burst pane frames=%d evicted=%v, want %d retained", len(p.frames), p != nil && p.evicted, burst)
	}
	for index, frame := range p.frames {
		sequence := uint64(index + 1)
		wantText := fmt.Sprintf("burst-%03d", index+1)
		if frame.sequence != sequence || frame.text != wantText {
			t.Fatalf("burst frame %d = sequence:%d text:%q, want %d/%q",
				index, frame.sequence, frame.text, sequence, wantText)
		}
	}
	if model.streamSequence != burst || model.snapshot.ThroughSequence != burst {
		t.Fatalf("burst watermarks stream=%d snapshot=%d", model.streamSequence, model.snapshot.ThroughSequence)
	}
}

type synchronizedEventSource struct {
	events  chan PublicEvent
	slots   chan struct{}
	started chan StreamCursor
}

func newSynchronizedEventSource(capacity int) *synchronizedEventSource {
	source := &synchronizedEventSource{
		events:  make(chan PublicEvent, capacity),
		slots:   make(chan struct{}, capacity),
		started: make(chan StreamCursor, capacity+2),
	}
	for index := 0; index < capacity; index++ {
		source.slots <- struct{}{}
	}
	return source
}

// enqueue models a bounded producer. If blocked is non-nil it reports whether
// the enqueue entered the full-channel backpressure path before it can finish.
func (source *synchronizedEventSource) enqueue(event PublicEvent, blocked chan<- bool) {
	select {
	case <-source.slots:
		if blocked != nil {
			blocked <- false
		}
	default:
		if blocked != nil {
			blocked <- true
		}
		<-source.slots
	}
	source.events <- event
}

func (source *synchronizedEventSource) Next(ctx context.Context, cursor StreamCursor) (PublicEvent, error) {
	select {
	case source.started <- cursor:
	case <-ctx.Done():
		return PublicEvent{}, ctx.Err()
	}
	select {
	case event := <-source.events:
		source.slots <- struct{}{}
		return event, nil
	case <-ctx.Done():
		return PublicEvent{}, ctx.Err()
	}
}

func TestUI602SlowConsumerAppliesProducerBackpressureWithoutDrops(t *testing.T) {
	fake := newFake(baseSnapshot())
	source := newSynchronizedEventSource(1)
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake, Events: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	_, next := model.Update(model.Init()())
	if next == nil {
		t.Fatal("validated Snapshot did not arm the stream")
	}

	sentFirst := make(chan struct{})
	sentSecond := make(chan struct{})
	thirdBackpressure := make(chan bool, 1)
	sentThird := make(chan struct{})
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		source.enqueue(frameEvent(7, 1, "manager-a", "slow-one"), nil)
		close(sentFirst)
		source.enqueue(frameEvent(7, 2, "manager-a", "slow-two"), nil)
		close(sentSecond)
		source.enqueue(frameEvent(7, 3, "manager-a", "slow-three"), thirdBackpressure)
		close(sentThird)
	}()
	<-sentFirst // The single channel slot is now full.

	messageResult := make(chan tea.Msg, 1)
	go func() { messageResult <- next() }()
	firstCursor := <-source.started
	firstMessage := <-messageResult
	_, next = model.Update(firstMessage)
	if firstCursor != (StreamCursor{Generation: 7, After: 0}) {
		t.Fatalf("first cursor = %#v", firstCursor)
	}
	<-sentSecond
	if blocked := <-thirdBackpressure; !blocked {
		t.Fatal("third producer enqueue did not enter the bounded full-channel path")
	}
	select {
	case <-sentThird:
		t.Fatal("third producer send completed while the one-slot channel still held the second event")
	default:
		// The producer mechanically observed no slot and cannot complete until
		// the model re-arms Next and releases the second event's permit.
	}

	secondMessage := next()
	secondCursor := <-source.started
	_, next = model.Update(secondMessage)
	if secondCursor != (StreamCursor{Generation: 7, After: 1}) {
		t.Fatalf("second cursor = %#v", secondCursor)
	}
	<-sentThird
	thirdMessage := next()
	thirdCursor := <-source.started
	_, next = model.Update(thirdMessage)
	if thirdCursor != (StreamCursor{Generation: 7, After: 2}) {
		t.Fatalf("third cursor = %#v", thirdCursor)
	}
	<-producerDone

	p := model.panes["manager-a"]
	if p == nil || len(p.frames) != 3 {
		t.Fatalf("slow-consumer pane = %#v", p)
	}
	for index, want := range []string{"slow-one", "slow-two", "slow-three"} {
		if p.frames[index].sequence != uint64(index+1) || p.frames[index].text != want {
			t.Fatalf("slow-consumer frame %d = %#v, want %q", index, p.frames[index], want)
		}
	}
}

func TestUI602OversizedFrameAtConfiguredLimitIsRetainedExactly(t *testing.T) {
	const frameLimit = 4096
	payload := "BEGIN:" + strings.Repeat("x", frameLimit-len("BEGIN:")-len(":END")) + ":END"
	fake := newFake(baseSnapshot())
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake, Events: fake,
		Limits: Limits{
			FrameBytes: frameLimit, PaneBytes: frameLimit * 2, PaneFrames: 4,
			AggregatePaneBytes: frameLimit * 2, AggregatePaneFrames: 4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	_, next := model.Update(model.Init()())
	fake.events <- frameEvent(7, 1, "manager-a", payload)
	next = runCmd(t, model, next)
	if next == nil {
		t.Fatal("stream did not re-arm after within-limit oversized frame")
	}
	p := model.panes["manager-a"]
	if p == nil || len(p.frames) != 1 || p.frames[0].text != payload || p.frames[0].bytes != frameLimit {
		t.Fatalf("within-limit frame was changed: pane=%#v", p)
	}
	if p.evicted || strings.Contains(p.frames[0].text, "[public frame truncated]") {
		t.Fatal("within-limit oversized frame was truncated or evicted")
	}
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	view := model.View().Content
	if !strings.Contains(view, ":END") || strings.Contains(view, "[public frame truncated]") {
		t.Fatalf("within-limit oversized frame renderer lost its live tail:\n%s", view)
	}
}

func TestUnknownAndHostileFramesAreBoundedAndTerminalSafe(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 1, Kind: EventPublicFrame, TaskID: "task-new", SessionID: "manager-a",
		Frame: PublicFrame{Kind: FrameUnknown, PublicType: "future\x1b]52;c;steal\a\u202e", Payload: []byte("UNKNOWN-PAYLOAD-SECRET")},
	}
	next = runCmd(t, model, next)
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 2, Kind: EventPublicFrame, TaskID: "task-new", SessionID: "manager-a",
		Frame: PublicFrame{Kind: FrameText, PublicType: "text", Payload: []byte("safe\x1b[31mRED\x1b[0m\r\u202ereordered\x00")},
	}
	runCmd(t, model, next)
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	view := model.View().Content
	if strings.Contains(view, "UNKNOWN-PAYLOAD-SECRET") {
		t.Fatal("unknown public payload was rendered or retained in its representation")
	}
	if strings.ContainsRune(view, '\x1b') || strings.ContainsRune(view, '\u202e') || strings.ContainsRune(view, '\x00') {
		t.Fatalf("hostile control reached terminal view: %q", view)
	}
	for _, escaped := range []string{"unknown public ACP", "bytes=22", "\\x1b", "\\u202e", "\\x00"} {
		if !strings.Contains(view, escaped) {
			t.Errorf("safe representation missing %q:\n%s", escaped, view)
		}
	}
}

func TestLiveScrollIntentDraftAndFocusSurviveFrames(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()
	model.currentTask = "task-new"
	model.screen = screenWorkspace
	model.selectedManager = "manager-a"
	model.focus = focusInput
	draft := testDraft(model, "manager-a")
	draft.insert("draft preserved")
	draft.left()
	model.drafts["manager-a"] = draft
	for sequence := uint64(1); sequence <= 8; sequence++ {
		fake.events <- frameEvent(7, sequence, "manager-a", "frame-line")
		next = runCmd(t, model, next)
	}
	model.scrollPane(4)
	p := model.panes["manager-a"]
	beforeOffset := p.offsetFromTail
	if p.followTail || beforeOffset == 0 {
		t.Fatal("scrolling away did not disable live-edge following")
	}
	fake.events <- frameEvent(7, 9, "manager-a", "new-live-frame")
	runCmd(t, model, next)
	if p.followTail || p.offsetFromTail <= beforeOffset {
		t.Fatal("new output forced pane back to live edge")
	}
	gotDraft := model.drafts["manager-a"]
	if gotDraft.value != "draft preserved" || gotDraft.cursor != draft.cursor || model.focus != focusInput {
		t.Fatalf("stream changed draft/cursor/focus: %#v focus=%v", gotDraft, model.focus)
	}
	updateKey(model, "esc")
	updateKey(model, "end")
	if !p.followTail || p.offsetFromTail != 0 {
		t.Fatal("End did not restore live-edge following")
	}
}

func TestPaneMemoryBoundsEvictOnlyOldestRAMFrames(t *testing.T) {
	snapshot := baseSnapshot()
	fake := newFake(snapshot)
	model, err := New(Config{
		Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake, Events: fake,
		Limits: Limits{DraftBytes: 64, FrameBytes: 32, UnknownTypeBytes: 16, PaneBytes: 70, PaneFrames: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	_, next := model.Update(model.Init()())
	for sequence := uint64(1); sequence <= 6; sequence++ {
		fake.events <- frameEvent(7, sequence, "manager-a", "frame-1234567890")
		next = runCmd(t, model, next)
	}
	p := model.panes["manager-a"]
	if len(p.frames) != 3 || p.frames[0].sequence != 4 || p.frames[2].sequence != 6 || p.bytes > 70 {
		t.Fatalf("bounded pane = frames:%#v bytes:%d", p.frames, p.bytes)
	}
}

func TestMetadataRemovalPrunesRAMOnlyPaneAndDraft(t *testing.T) {
	model, fake, _ := loadedModel(t, baseSnapshot(), false)
	defer model.Close()
	model.currentTask = "task-new"
	draft := testDraft(model, "manager-b")
	draft.insert("discard with removed metadata")
	model.drafts["manager-b"] = draft
	testPane(model, "manager-b").append(paneFrame{sequence: 1, text: "discarded frame", bytes: 15}, model.limits)

	replacement := baseSnapshot()
	for i := range replacement.Sessions {
		if replacement.Sessions[i].ID == "manager-b" {
			replacement.Sessions = append(replacement.Sessions[:i], replacement.Sessions[i+1:]...)
			break
		}
	}
	fake.replaceSnapshot(replacement)
	runCmd(t, model, model.reload())
	if _, ok := model.drafts["manager-b"]; ok {
		t.Fatal("removed Manager draft remained in RAM")
	}
	if _, ok := model.panes["manager-b"]; ok {
		t.Fatal("removed Manager pane remained in RAM")
	}
}

func TestStreamRejectsGapsCrossSessionAndStaleGeneration(t *testing.T) {
	t.Run("gap", func(t *testing.T) {
		model, fake, next := loadedModel(t, baseSnapshot(), true)
		defer model.Close()
		fake.events <- frameEvent(7, 2, "manager-a", "gap")
		if got := runCmd(t, model, next); got != nil {
			t.Fatal("out-of-order stream was re-armed")
		}
		if model.streamSequence != 0 || !strings.Contains(model.status, "out-of-order") {
			t.Fatalf("gap state sequence=%d status=%q", model.streamSequence, model.status)
		}
	})

	t.Run("wrong-task", func(t *testing.T) {
		model, fake, next := loadedModel(t, baseSnapshot(), true)
		defer model.Close()
		event := frameEvent(7, 1, "manager-a", "cross")
		event.TaskID = "task-old"
		fake.events <- event
		if got := runCmd(t, model, next); got != nil {
			t.Fatal("cross-Task event was re-armed")
		}
		if model.panes["manager-a"] != nil && len(model.panes["manager-a"].frames) != 0 {
			t.Fatal("cross-Task frame contaminated pane")
		}
	})

	t.Run("generation", func(t *testing.T) {
		model, _, oldWait := loadedModel(t, baseSnapshot(), true)
		defer model.Close()
		replacement := baseSnapshot()
		replacement.StreamGeneration = 8
		_, newWait := model.Update(snapshotMsg{action: actionReload, snapshot: replacement})
		if newWait == nil || model.streamGeneration != 8 || !model.streamWaiting {
			t.Fatal("replacement generation was not armed")
		}
		// The old command captured a context cancelled by resetStream.
		oldMessage := oldWait()
		model.Update(oldMessage)
		if model.streamGeneration != 8 || !model.streamWaiting || model.streamSequence != 0 {
			t.Fatal("stale generation altered current stream state")
		}
	})
}

func TestAheadSnapshotDrainsPublicFramesWithoutRevertingMetadata(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()

	ahead := baseSnapshot()
	ahead.ThroughSequence = 2
	for index := range ahead.Sessions {
		if ahead.Sessions[index].ID == "child-follow" {
			ahead.Sessions[index].State = StateInterrupted
		}
	}
	model.Update(snapshotMsg{action: actionReload, snapshot: ahead, hasSnapshot: true})
	if got, _ := model.session("child-follow"); got.State != StateInterrupted {
		t.Fatalf("ahead snapshot state = %s", got.State)
	}

	older, _ := model.session("child-follow")
	older.State = StateDormant
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 1, Kind: EventSessionUpsert,
		TaskID: older.TaskID, SessionID: older.ID, Session: older,
	}
	next = runCmd(t, model, next)
	if got, _ := model.session("child-follow"); got.State != StateInterrupted {
		t.Fatalf("superseded metadata event reverted state to %s", got.State)
	}
	fake.events <- frameEvent(7, 2, "manager-a", "frame preserved across ahead snapshot")
	next = runCmd(t, model, next)
	if next == nil || model.streamSequence != 2 || model.snapshot.ThroughSequence != 2 {
		t.Fatalf("catch-up watermarks stream=%d snapshot=%d", model.streamSequence, model.snapshot.ThroughSequence)
	}
	pane := model.panes["manager-a"]
	if pane == nil || len(pane.frames) != 1 || pane.frames[0].text != "frame preserved across ahead snapshot" {
		t.Fatalf("catch-up frame was not retained exactly: %#v", pane)
	}
}

func TestSessionEventsAreMechanicalAndRoleImmutable(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	defer model.Close()
	updated, _ := model.session("child-follow")
	updated.State = StateActive
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 1, Kind: EventSessionUpsert,
		TaskID: updated.TaskID, SessionID: updated.ID, Session: updated,
	}
	next = runCmd(t, model, next)
	got, _ := model.session("child-follow")
	if got.State != StateActive {
		t.Fatalf("mechanical state update = %s", got.State)
	}
	updated.Role = RoleManager
	updated.ParentID = ""
	updated.Mode = ""
	fake.events <- PublicEvent{
		Generation: 7, Sequence: 2, Kind: EventSessionUpsert,
		TaskID: updated.TaskID, SessionID: updated.ID, Session: updated,
	}
	if gotNext := runCmd(t, model, next); gotNext != nil {
		t.Fatal("role-changing event was re-armed")
	}
	got, _ = model.session("child-follow")
	if got.Role != RoleSubordinate {
		t.Fatal("Session role changed through live event")
	}
}

func TestCloseCancelsOwnedWaitWithoutLifecycleMutation(t *testing.T) {
	model, fake, next := loadedModel(t, baseSnapshot(), true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = next()
	}()
	deadline := time.After(2 * time.Second)
	for {
		fake.mu.Lock()
		active := fake.nextActive
		fake.mu.Unlock()
		if active == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("event wait did not start")
		default:
		}
	}
	model.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel TUI-owned wait")
	}
	if len(fake.callsOf("stop")) != 0 || len(fake.callsOf("detach")) != 0 {
		t.Fatal("Close performed runtime lifecycle mutation")
	}
}

func frameEvent(generation, sequence uint64, manager SessionID, text string) PublicEvent {
	return PublicEvent{
		Generation: generation, Sequence: sequence, Kind: EventPublicFrame,
		TaskID: "task-new", SessionID: manager,
		Frame: PublicFrame{Kind: FrameText, PublicType: "agent_message_chunk", Payload: []byte(text)},
	}
}
