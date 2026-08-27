package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/testkit"
)

func TestDelegateStrictOperationFieldRejectionHasNoDownstreamActivity(t *testing.T) {
	t.Parallel()
	factory := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "native"})
	recorder := &testkit.EventRecorder{}
	delegator := newDelegator(t, "manager-a", factory, recorder)
	cases := []string{
		`{}`,
		`{"operation":"delegate","provider":"fake","prompt":"x","mode":"follow"}`,
		`{"operation":"start","provider":"fake","child_session_id":"child","prompt":"x","mode":"answer"}`,
		`{"operation":"start","provider":"fake","prompt":"x","mode":"answer","turn_token":"illegal"}`,
		`{"operation":"start","provider":"fake","prompt":"x","mode":"replay"}`,
		`{"operation":"next_answer","child_session_id":"illegal"}`,
		`{"operation":"follow_next","child_session_id":"child","turn_token":"token","provider":"illegal"}`,
		`{"operation":"follow_next","child_session_id":"child","turn_token":"token","prompt":"illegal"}`,
		`{"operation":"follow_next","child_session_id":"child","turn_token":"token","mode":"follow"}`,
		`{"operation":"cancel","child_session_id":"child","turn_token":"token","mode":"answer"}`,
		`{"operation":"replay","child_session_ids":[]}`,
		`{"operation":"replay","child_session_ids":["child","child"]}`,
		`{"operation":"replay","child_session_ids":["child"],"n":0}`,
		`{"operation":"replay","child_session_ids":["child"],"n":33}`,
		`{"operation":"replay","child_session_ids":["child"],"provider":"illegal"}`,
	}
	for _, input := range cases {
		if _, err := delegator.Invoke(context.Background(), json.RawMessage(input)); !errors.Is(err, connector.ErrInvalidOperation) {
			t.Errorf("Invoke(%s) error = %v, want ErrInvalidOperation", input, err)
		}
	}
	if got := len(factory.Created()); got != 0 {
		t.Fatalf("invalid calls created %d children", got)
	}
	if got := len(recorder.Snapshot()); got != 0 {
		t.Fatalf("invalid calls emitted %d core events", got)
	}
}

func TestStartEmitsContentFreeOwnershipBeforePromptAcceptance(t *testing.T) {
	t.Parallel()
	begin := make(chan struct{})
	started := make(chan struct{}, 1)
	promptSeen := make(chan string, 1)
	canary := "OPAQUE-PROMPT-CONTENT-CANARY"
	factory := testkit.NewScriptedFactory(testkit.ChildScript{
		NativeReference: "native-ref-1",
		Begin:           begin,
		Started:         started,
		PromptSeen:      promptSeen,
		Frames:          []json.RawMessage{testkit.AgentMessageFrame("native-ref-1", "answer", "ok")},
	})
	recorder := &testkit.EventRecorder{Notify: make(chan connector.ChildEvent, 1)}
	delegator := newDelegator(t, "manager-a", factory, recorder)

	start := invokeResult(t, delegator, fmt.Sprintf(`{"operation":"start","provider":"fake","prompt":%q,"mode":"answer"}`, canary))
	if start.Operation != connector.OperationStart || start.Mode != connector.ModeAnswer || !start.More || start.NextOperation != connector.OperationNextAnswer {
		t.Fatalf("start result = %+v", start)
	}
	<-started
	event := <-recorder.Notify
	if got := <-promptSeen; got != canary {
		t.Fatalf("opaque prompt changed")
	}
	expectedReference, _ := replay.NewCodexReference("native-ref-1")
	if event.ParentSessionID != "manager-a" || event.ChildSessionID != start.ChildSessionID || event.Provider != "fake" || !bytes.Equal(event.NativeReference, expectedReference.Bytes()) || event.Mode != connector.ModeAnswer || event.Lifecycle != "prompt_pending" {
		t.Fatalf("event = %+v", event)
	}
	encodedEvent, _ := json.Marshal(event)
	if bytes.Contains(encodedEvent, []byte(canary)) || bytes.Contains(encodedEvent, []byte(`"prompt":`)) || bytes.Contains(encodedEvent, []byte(`"answer":`)) {
		t.Fatalf("core event contains content field: %s", encodedEvent)
	}
	close(begin)
	answer := invokeResult(t, delegator, `{"operation":"next_answer"}`)
	if answer.Completion == nil || !answer.Completion.Success {
		t.Fatalf("completion = %+v", answer.Completion)
	}
	events := recorder.Snapshot()
	if len(events) != 2 || events[1].Lifecycle != "terminal" || events[1].RequestID != event.RequestID ||
		events[1].TurnID != event.TurnID || events[1].ChildSessionID != event.ChildSessionID ||
		!bytes.Equal(events[1].NativeReference, event.NativeReference) {
		t.Fatalf("terminal lifecycle events = %+v", events)
	}
	encodedTerminal, _ := json.Marshal(events[1])
	if bytes.Contains(encodedTerminal, []byte(canary)) || bytes.Contains(encodedTerminal, []byte("ok")) ||
		bytes.Contains(encodedTerminal, []byte(`"completion"`)) {
		t.Fatalf("terminal core event contains content: %s", encodedTerminal)
	}
}

func TestThreeOverlappingAnswersReturnInNativeTerminalOrderWithIdenticalText(t *testing.T) {
	t.Parallel()
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	started := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)}
	factory := testkit.NewScriptedFactory(
		testkit.ChildScript{NativeReference: "native-a", Frames: answerAndIntermediateFrames("a", "identical"), Release: releases[0], Started: started[0]},
		testkit.ChildScript{NativeReference: "native-b", Frames: answerAndIntermediateFrames("b", "identical"), Release: releases[1], Started: started[1]},
		testkit.ChildScript{NativeReference: "native-c", Frames: answerAndIntermediateFrames("c", "third"), Release: releases[2], Started: started[2]},
	)
	recorder := &testkit.EventRecorder{}
	delegator := newDelegator(t, "manager", factory, recorder)

	starts := make([]wireResult, 3)
	for index := range starts {
		starts[index] = invokeResult(t, delegator, fmt.Sprintf(`{"operation":"start","provider":"fake","prompt":"opaque-%d","mode":"answer"}`, index))
		if !starts[index].More || starts[index].NextOperation != connector.OperationNextAnswer {
			t.Fatalf("start %d = %+v", index, starts[index])
		}
	}
	for _, signal := range started {
		<-signal
	}
	if got := len(recorder.Snapshot()); got != 3 {
		t.Fatalf("core events = %d", got)
	}

	order := []int{1, 0, 2}
	for resultIndex, childIndex := range order {
		close(releases[childIndex])
		answer := invokeResult(t, delegator, `{"operation":"next_answer"}`)
		if answer.ChildSessionID != starts[childIndex].ChildSessionID || answer.TurnToken != starts[childIndex].TurnToken {
			t.Fatalf("answer %d correlation = child %q token %q, want child %q token %q", resultIndex, answer.ChildSessionID, answer.TurnToken, starts[childIndex].ChildSessionID, starts[childIndex].TurnToken)
		}
		if answer.Completion == nil || !answer.Completion.Success || answer.Completion.StopReason != "end_turn" || len(answer.Completion.Answer) != 1 {
			t.Fatalf("answer %d completion = %+v", resultIndex, answer.Completion)
		}
		wantText := "identical"
		if childIndex == 2 {
			wantText = "third"
		}
		if got := contentText(t, answer.Completion.Answer[0]); got != wantText {
			t.Fatalf("answer %d text = %q, want %q", resultIndex, got, wantText)
		}
		if len(answer.Frames) != 0 {
			t.Fatalf("Answer exposed intermediate frames: %s", answer.Raw)
		}
		if answer.More != (resultIndex != len(order)-1) {
			t.Fatalf("answer %d more = %v", resultIndex, answer.More)
		}
	}
}

func TestAnswerReservesEscapedWireBudgetBeforeReadyQueuePop(t *testing.T) {
	t.Parallel()
	answerText := strings.Repeat(`\`, 48<<10)
	frame := testkit.AgentMessageFrame("native", "answer", answerText)
	if len(frame) >= 128<<10 {
		t.Fatalf("fixture ACP frame = %d bytes", len(frame))
	}
	factory := testkit.NewScriptedFactory(testkit.ChildScript{
		NativeReference: "native", Frames: []json.RawMessage{frame},
		Completion: json.RawMessage(`{"stopReason":"end_turn"}`),
	})
	delegator := newDelegator(t, "manager", factory, &testkit.EventRecorder{})
	start := invokeResult(t, delegator, `{"operation":"start","provider":"fake","prompt":"opaque","mode":"answer"}`)
	answer := invokeResult(t, delegator, `{"operation":"next_answer"}`)
	if answer.ChildSessionID != start.ChildSessionID || answer.TurnToken != start.TurnToken || answer.Completion == nil ||
		answer.Completion.Success || answer.Completion.StopReason != "malformed_completion" || len(answer.Completion.Answer) != 0 {
		t.Fatalf("wire-budget completion = %+v", answer)
	}
	if _, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); !errors.Is(err, connector.ErrStaleTurn) {
		t.Fatalf("post-delivery next_answer error = %v", err)
	}
}

func TestFollowTerminalMatrixInvalidatesAndResetsToAnswer(t *testing.T) {
	cases := []struct {
		name       string
		completion json.RawMessage
		transport  bool
		cancel     bool
		want       string
	}{
		{name: "success", completion: json.RawMessage(`{"stopReason":"end_turn"}`), want: "end_turn"},
		{name: "cancelled", cancel: true, want: "cancelled"},
		{name: "refusal", completion: json.RawMessage(`{"stopReason":"refusal"}`), want: "refusal"},
		{name: "max_tokens", completion: json.RawMessage(`{"stopReason":"max_tokens"}`), want: "max_tokens"},
		{name: "max_turn_requests", completion: json.RawMessage(`{"stopReason":"max_turn_requests"}`), want: "max_turn_requests"},
		{name: "unknown_stop", completion: json.RawMessage(`{"stopReason":"future_private_stop"}`), want: "malformed_completion"},
		{name: "malformed_completion", completion: json.RawMessage(`{"missingStop":true}`), want: "malformed_completion"},
		{name: "transport_failure", transport: true, want: "transport_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			begin := make(chan struct{})
			factory := testkit.NewScriptedFactory(testkit.ChildScript{
				NativeReference: "native",
				Begin:           begin,
				Frames:          []json.RawMessage{testkit.UnknownUpdateFrame("native", 1)},
				Completion:      tc.completion,
				TransportError:  tc.transport,
			})
			delegator := newDelegator(t, "manager", factory, &testkit.EventRecorder{})
			start := invokeResult(t, delegator, `{"operation":"start","provider":"fake","prompt":"opaque","mode":"follow"}`)
			if tc.cancel {
				cancelInput := fmt.Sprintf(`{"operation":"cancel","child_session_id":%q,"turn_token":%q}`, start.ChildSessionID, start.TurnToken)
				cancelResult := invokeResult(t, delegator, cancelInput)
				if !cancelResult.More || cancelResult.NextOperation != connector.OperationFollowNext {
					t.Fatalf("cancel result = %+v", cancelResult)
				}
			} else {
				close(begin)
			}
			terminal := drainFollow(t, delegator, start)
			if terminal.Completion == nil || terminal.Completion.StopReason != tc.want {
				t.Fatalf("terminal = %+v, want %q", terminal.Completion, tc.want)
			}
			if tc.want != "end_turn" && (terminal.Completion.Success || len(terminal.Completion.Answer) != 0) {
				t.Fatalf("non-success manufactured answer: %+v", terminal.Completion)
			}
			if mode, err := delegator.ChildMode(start.ChildSessionID); err != nil || mode != connector.ModeAnswer {
				t.Fatalf("post-terminal mode = %q, err=%v", mode, err)
			}
			stale := fmt.Sprintf(`{"operation":"follow_next","child_session_id":%q,"turn_token":%q}`, start.ChildSessionID, start.TurnToken)
			if _, err := delegator.Invoke(context.Background(), json.RawMessage(stale)); !errors.Is(err, connector.ErrStaleTurn) {
				t.Fatalf("stale follow_next error = %v", err)
			}

			answerStart := invokeResult(t, delegator, fmt.Sprintf(`{"operation":"start","child_session_id":%q,"prompt":"next","mode":"answer"}`, start.ChildSessionID))
			if answerStart.Mode != connector.ModeAnswer || answerStart.NextOperation != connector.OperationNextAnswer || len(answerStart.Frames) != 0 {
				t.Fatalf("same-child reset start = %+v", answerStart)
			}
			answerTerminal := invokeResult(t, delegator, `{"operation":"next_answer"}`)
			if answerTerminal.Mode != connector.ModeAnswer || answerTerminal.Completion == nil || answerTerminal.Completion.StopReason != tc.want {
				t.Fatalf("same-child Answer completion = %+v", answerTerminal)
			}
			if tc.want != "end_turn" && (answerTerminal.Completion.Success || len(answerTerminal.Completion.Answer) != 0) {
				t.Fatalf("same-child Answer manufactured content: %+v", answerTerminal.Completion)
			}
		})
	}
}

func TestFollowRawOrderingBatchBoundsAndActualBackpressure(t *testing.T) {
	begin := make(chan struct{})
	attempted := make(chan int, 64)
	accepted := make(chan int, 64)
	frames := make([]json.RawMessage, 48)
	for index := range frames {
		frames[index] = spacedUnknownFrame(index + 1)
	}
	factory := testkit.NewScriptedFactory(testkit.ChildScript{
		NativeReference: "native-follow",
		Begin:           begin,
		Frames:          frames,
		Completion:      json.RawMessage(`{"stopReason":"end_turn"}`),
		FrameAttempted:  attempted,
		FrameAccepted:   accepted,
	})
	delegator := newDelegator(t, "manager", factory, &testkit.EventRecorder{})
	start := invokeResult(t, delegator, `{"operation":"start","provider":"fake","prompt":"opaque","mode":"follow"}`)
	if len(start.Frames) != 0 || start.NextOperation != connector.OperationFollowNext {
		t.Fatalf("start result = %+v", start)
	}
	assertExactContinuation(t, start, connector.OperationFollowNext)
	close(begin)
	for range 17 {
		<-attempted
	}
	for range connector.FollowHighWaterFrames {
		<-accepted
	}
	waitForPauses(t, delegator, start.ChildSessionID, start.TurnToken, 1)
	child := factory.Created()[0]
	if tried, stored := child.Counts(); tried != 17 || stored != connector.FollowHighWaterFrames {
		t.Fatalf("producer counts at high water = attempted %d accepted %d", tried, stored)
	}

	collected := make([]json.RawMessage, 0, len(frames))
	current := start
	for len(collected) < 32 {
		current = invokeResult(t, delegator, encodeNext(current))
		if len(current.Frames) == 0 || len(current.Frames) > connector.FollowBatchFrames {
			t.Fatalf("follow batch size = %d", len(current.Frames))
		}
		collected = append(collected, current.Frames...)
		assertExactContinuation(t, current, connector.OperationFollowNext)
		if len(collected) < 32 {
			waitForPauses(t, delegator, start.ChildSessionID, start.TurnToken, 1+len(collected)/connector.FollowBatchFrames)
		}
	}
	for range 48 - connector.FollowHighWaterFrames {
		<-accepted
	}
	stats, err := delegator.FollowStats(start.ChildSessionID, start.TurnToken)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PeakFrames != connector.FollowHighWaterFrames || stats.PeakBytes > connector.FollowHighWaterBytes || stats.Pauses != 4 || stats.Resumes != 4 {
		t.Fatalf("backpressure stats = %+v", stats)
	}
	for current.More {
		current = invokeResult(t, delegator, encodeNext(current))
		if len(current.Frames) > connector.FollowBatchFrames {
			t.Fatalf("follow batch size = %d", len(current.Frames))
		}
		collected = append(collected, current.Frames...)
	}
	if current.Completion == nil || current.Completion.StopReason != "end_turn" {
		t.Fatalf("terminal = %+v", current.Completion)
	}
	if len(collected) != len(frames) {
		t.Fatalf("collected %d frames, want %d", len(collected), len(frames))
	}
	for index := range frames {
		if !bytes.Equal(collected[index], frames[index]) {
			t.Fatalf("frame %d changed\ngot:  %s\nwant: %s", index+1, collected[index], frames[index])
		}
	}
	if mode, err := delegator.ChildMode(start.ChildSessionID); err != nil || mode != connector.ModeAnswer {
		t.Fatalf("mode after success = %q, err=%v", mode, err)
	}
}

func TestForeignAndStaleOwnershipFailBeforeDownstreamActivity(t *testing.T) {
	t.Parallel()
	begin := make(chan struct{})
	factoryA := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "native-a", Begin: begin})
	factoryB := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "native-b"})
	delegatorA := newDelegator(t, "manager-a", factoryA, &testkit.EventRecorder{})
	delegatorB := newDelegator(t, "manager-b", factoryB, &testkit.EventRecorder{})
	start := invokeResult(t, delegatorA, `{"operation":"start","provider":"fake","prompt":"opaque","mode":"follow"}`)

	foreignFollow := fmt.Sprintf(`{"operation":"follow_next","child_session_id":%q,"turn_token":%q}`, start.ChildSessionID, start.TurnToken)
	if _, err := delegatorB.Invoke(context.Background(), json.RawMessage(foreignFollow)); !errors.Is(err, connector.ErrStaleTurn) {
		t.Fatalf("foreign follow error = %v", err)
	}
	foreignStart := fmt.Sprintf(`{"operation":"start","child_session_id":%q,"prompt":"opaque","mode":"answer"}`, start.ChildSessionID)
	if _, err := delegatorB.Invoke(context.Background(), json.RawMessage(foreignStart)); !errors.Is(err, connector.ErrOwnership) {
		t.Fatalf("foreign child start error = %v", err)
	}
	if len(factoryB.Created()) != 0 {
		t.Fatal("foreign ownership caused downstream creation")
	}
	close(begin)
	drainFollow(t, delegatorA, start)
}

func TestFollowTerminalChildEventFailureFailsQueueAndClosesTree(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	factory := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "native", Release: release})
	events := &terminalFailureSink{failure: errors.New("PRIVATE-FOLLOW-EVENT-CANARY")}
	delegator := newDelegator(t, "manager", factory, events)
	start := invokeResult(t, delegator, `{"operation":"start","provider":"fake","prompt":"opaque","mode":"follow"}`)

	done := make(chan error, 1)
	go func() {
		_, err := delegator.Invoke(context.Background(), json.RawMessage(encodeNext(start)))
		done <- err
	}()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, connector.ErrTransport) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("follow_next fatal error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow_next remained blocked after terminal event failure")
	}
	select {
	case <-delegator.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Delegator tree remained live after terminal event failure")
	}
	if err := delegator.Err(); !errors.Is(err, connector.ErrTransport) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("Delegator fatal error = %v", err)
	}
	if _, err := delegator.ChildMode(start.ChildSessionID); !errors.Is(err, connector.ErrOwnership) {
		t.Fatalf("fatal child remained owned/active: %v", err)
	}
}

func newDelegator(t *testing.T, parent string, factory connector.ChildFactory, events connector.ChildEventSink) *connector.Delegator {
	t.Helper()
	registry := connector.NewRegistry()
	versions := connector.Versions{ACPProtocol: 1, Connector: "test-fixture"}
	profile := connector.NewConformedProfile(
		"fake",
		"Fake ACP",
		versions,
		connector.Conformance{Exact: versions, Proven: connector.Capabilities{Subordinate: true}},
		nil,
		factory,
		nil,
		nil,
		nil,
		connector.ReplayBinding{},
	)
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	delegator, err := connector.NewDelegator(context.Background(), parent, registry, events, &testkit.SequenceIDs{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delegator.Close() })
	return delegator
}

type wireResult struct {
	Raw            json.RawMessage
	Operation      connector.Operation `json:"operation"`
	ChildSessionID string              `json:"child_session_id"`
	TurnToken      string              `json:"turn_token"`
	Mode           connector.Mode      `json:"mode"`
	Frames         []json.RawMessage   `json:"frames"`
	Completion     *connector.Terminal `json:"completion"`
	More           bool                `json:"more"`
	NextOperation  connector.Operation `json:"next_operation"`
	NextArguments  struct {
		Operation      connector.Operation `json:"operation"`
		ChildSessionID string              `json:"child_session_id"`
		TurnToken      string              `json:"turn_token"`
	} `json:"next_arguments"`
}

func invokeResult(t *testing.T, delegator *connector.Delegator, input string) wireResult {
	t.Helper()
	raw, err := delegator.Invoke(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Invoke(%s) error = %v", input, err)
	}
	var result wireResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("result decode error = %v: %s", err, raw)
	}
	result.Raw = append(json.RawMessage(nil), raw...)
	return result
}

func drainFollow(t *testing.T, delegator *connector.Delegator, result wireResult) wireResult {
	t.Helper()
	for result.More {
		result = invokeResult(t, delegator, encodeNext(result))
	}
	return result
}

func encodeNext(result wireResult) string {
	value, _ := json.Marshal(map[string]string{
		"operation":        string(result.NextArguments.Operation),
		"child_session_id": result.NextArguments.ChildSessionID,
		"turn_token":       result.NextArguments.TurnToken,
	})
	return string(value)
}

func assertExactContinuation(t *testing.T, result wireResult, operation connector.Operation) {
	t.Helper()
	if !result.More {
		return
	}
	if result.NextOperation != operation || result.NextArguments.Operation != operation {
		t.Fatalf("continuation operation = %q / %q, want %q", result.NextOperation, result.NextArguments.Operation, operation)
	}
	if operation == connector.OperationFollowNext && (result.NextArguments.ChildSessionID != result.ChildSessionID || result.NextArguments.TurnToken != result.TurnToken) {
		t.Fatalf("continuation correlation changed: %+v", result)
	}
	if operation == connector.OperationNextAnswer && (result.NextArguments.ChildSessionID != "" || result.NextArguments.TurnToken != "") {
		t.Fatalf("next_answer accepted target arguments: %+v", result.NextArguments)
	}
}

func answerAndIntermediateFrames(sessionID, answer string) []json.RawMessage {
	return []json.RawMessage{
		testkit.UnknownUpdateFrame(sessionID, 1),
		testkit.AgentMessageFrame(sessionID, "superseded-public-message", "not-the-final-answer"),
		testkit.AgentMessageFrame(sessionID, "public-answer", answer),
		testkit.UnknownUpdateFrame(sessionID, 2),
	}
}

func contentText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatal(err)
	}
	return content.Text
}

func spacedUnknownFrame(sequence int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0", "method":"session/update", "params": {"sessionId":"native-follow", "update": {"sessionUpdate":"future_shape", "sequence": %d, "opaque": [3, 2, 1]}}}`, sequence))
}

func waitForPauses(t *testing.T, delegator *connector.Delegator, childID, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := delegator.FollowStats(childID, token)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Pauses >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("follow queue never reported %d pauses", want)
}

func TestCanonicalNextArgumentsRejectProviderPromptAndMode(t *testing.T) {
	t.Parallel()
	tool := connector.DelegateCapability()
	if !strings.Contains(tool.Description, "exactly next_arguments") {
		t.Fatalf("tool description lacks mandatory exact continuation: %q", tool.Description)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if _, ok := schema["oneOf"]; !ok {
		t.Fatal("schema is not a discriminated operation union")
	}
}
