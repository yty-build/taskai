package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/testkit"
)

func TestReplayUsesNativeHistoryForRestoredChildWithoutTransientCompletionState(t *testing.T) {
	t.Parallel()
	reference, _ := replay.NewCodexReference("restored-native-thread")
	adapter := &scriptedReplayPort{invocations: 2}
	delegator := newReplayDelegator(t, adapter, nil, testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference}))
	if err := delegator.RestoreOwnedChild("child-restored", "fake", reference); err != nil {
		t.Fatal(err)
	}

	raw, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-restored"],"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Operation connector.Operation `json:"operation"`
		More      bool                `json:"more"`
		Replay    []struct {
			ChildSessionID string `json:"child_session_id"`
			Invocations    []struct {
				InvocationID string            `json:"invocation_id"`
				Records      []json.RawMessage `json:"records"`
			} `json:"invocations"`
		} `json:"replay"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation != connector.OperationReplay || result.More || len(result.Replay) != 1 ||
		result.Replay[0].ChildSessionID != "child-restored" || len(result.Replay[0].Invocations) != 2 {
		t.Fatalf("Replay result = %s", raw)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.calls != 1 || adapter.lastCount != 2 || !bytes.Equal(adapter.lastReference.Bytes(), reference.Bytes()) {
		t.Fatalf("adapter calls=%d count=%d reference changed=%v", adapter.calls, adapter.lastCount, !bytes.Equal(adapter.lastReference.Bytes(), reference.Bytes()))
	}
}

func TestReplayDefaultsToOneIndependentlyForExplicitRestoredTargets(t *testing.T) {
	t.Parallel()
	firstReference, _ := replay.NewCodexReference("restored-native-a")
	secondReference, _ := replay.NewCodexReference("restored-native-b")
	adapter := &scriptedReplayPort{invocations: 1}
	delegator := newReplayDelegator(t, adapter, nil, testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: firstReference}))
	if err := delegator.RestoreOwnedChild("child-a", "fake", firstReference); err != nil {
		t.Fatal(err)
	}
	if err := delegator.RestoreOwnedChild("child-b", "fake", secondReference); err != nil {
		t.Fatal(err)
	}
	raw, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-a","child-b"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Replay []struct {
			ChildSessionID string            `json:"child_session_id"`
			Invocations    []json.RawMessage `json:"invocations"`
		} `json:"replay"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Replay) != 2 ||
		result.Replay[0].ChildSessionID != "child-a" || result.Replay[1].ChildSessionID != "child-b" ||
		len(result.Replay[0].Invocations) != 1 || len(result.Replay[1].Invocations) != 1 {
		t.Fatalf("multi-target Replay = %s", raw)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.calls != 2 || len(adapter.counts) != 2 || adapter.counts[0] != 1 || adapter.counts[1] != 1 ||
		!bytes.Equal(adapter.references[0].Bytes(), firstReference.Bytes()) ||
		!bytes.Equal(adapter.references[1].Bytes(), secondReference.Bytes()) {
		t.Fatalf("adapter targets calls=%d counts=%v", adapter.calls, adapter.counts)
	}
}

func TestReplayReservationBlocksConcurrentStartUntilNativeReadCompletes(t *testing.T) {
	t.Parallel()
	reference, _ := replay.NewCodexReference("restored-native-thread")
	adapter := &scriptedReplayPort{invocations: 1, entered: make(chan struct{}), release: make(chan struct{})}
	resumer := &blockingResumeFactory{factory: testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference})}
	delegator := newReplayDelegator(t, adapter, resumer, testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference}))
	if err := delegator.RestoreOwnedChild("child-restored", "fake", reference); err != nil {
		t.Fatal(err)
	}

	replayDone := make(chan error, 1)
	go func() {
		_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-restored"]}`))
		replayDone <- err
	}()
	<-adapter.entered
	_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"start","child_session_id":"child-restored","prompt":"opaque","mode":"answer"}`))
	if !errors.Is(err, connector.ErrChildBusy) {
		t.Fatalf("Start during Replay error = %v, want ErrChildBusy", err)
	}
	resumer.mu.Lock()
	resumeCalls := resumer.calls
	resumer.mu.Unlock()
	if resumeCalls != 0 {
		t.Fatalf("Start crossed Replay reservation and issued %d Resume calls", resumeCalls)
	}
	close(adapter.release)
	if err := <-replayDone; err != nil {
		t.Fatalf("Replay error = %v", err)
	}
}

func TestReplayRejectsActiveChildBeforeAdapterRead(t *testing.T) {
	t.Parallel()
	begin := make(chan struct{})
	factory := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "native", Begin: begin})
	adapter := &scriptedReplayPort{invocations: 1}
	delegator := newReplayDelegator(t, adapter, nil, factory)
	startRaw, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"start","provider":"fake","prompt":"opaque","mode":"follow"}`))
	if err != nil {
		t.Fatal(err)
	}
	var start struct {
		ChildID string `json:"child_session_id"`
		Token   string `json:"turn_token"`
	}
	if json.Unmarshal(startRaw, &start) != nil {
		t.Fatal("invalid start result")
	}
	request, _ := json.Marshal(map[string]any{"operation": "replay", "child_session_ids": []string{start.ChildID}})
	if _, err := delegator.Invoke(context.Background(), request); !errors.Is(err, connector.ErrReplayUnavailable) {
		t.Fatalf("active Replay error = %v", err)
	}
	adapter.mu.Lock()
	calls := adapter.calls
	adapter.mu.Unlock()
	if calls != 0 {
		t.Fatalf("active Replay reached adapter %d times", calls)
	}
	close(begin)
	for {
		next, _ := json.Marshal(map[string]string{"operation": "follow_next", "child_session_id": start.ChildID, "turn_token": start.Token})
		raw, invokeErr := delegator.Invoke(context.Background(), next)
		if invokeErr != nil {
			t.Fatal(invokeErr)
		}
		var result struct {
			More bool `json:"more"`
		}
		_ = json.Unmarshal(raw, &result)
		if !result.More {
			break
		}
	}
}

func TestReplayFailsClosedWhenOwnedProviderHasNoConformedAdapter(t *testing.T) {
	t.Parallel()
	reference, _ := replay.NewCodexReference("native-without-replay")
	factory := testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference})
	delegator := newDelegator(t, "manager", factory, &testkit.EventRecorder{})
	if err := delegator.RestoreOwnedChild("child-owned", "fake", reference); err != nil {
		t.Fatal(err)
	}
	_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-owned"]}`))
	if !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("unsupported Replay error = %v", err)
	}
}

func TestReplayRequiresAdapterToReturnExactlyRequestedCompletedRange(t *testing.T) {
	t.Parallel()
	reference, _ := replay.NewCodexReference("native-short-history")
	adapter := &scriptedReplayPort{invocations: 1}
	delegator := newReplayDelegator(t, adapter, nil, testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference}))
	if err := delegator.RestoreOwnedChild("child-owned", "fake", reference); err != nil {
		t.Fatal(err)
	}
	_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"replay","child_session_ids":["child-owned"],"n":2}`))
	if !errors.Is(err, connector.ErrReplayUnavailable) {
		t.Fatalf("short native range error = %v", err)
	}
}

func TestRestoredChildConcurrentStartsIssueExactResumeOnce(t *testing.T) {
	t.Parallel()
	reference, _ := replay.NewCodexReference("restored-native-thread")
	resumeEntered := make(chan struct{})
	resumeRelease := make(chan struct{})
	childFactory := testkit.NewScriptedFactory(testkit.ChildScript{OpaqueReference: reference})
	resumer := &blockingResumeFactory{factory: childFactory, entered: resumeEntered, release: resumeRelease}
	delegator := newReplayDelegator(t, &scriptedReplayPort{invocations: 1}, resumer, childFactory)
	if err := delegator.RestoreOwnedChild("child-restored", "fake", reference); err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() {
		_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"start","child_session_id":"child-restored","prompt":"one","mode":"answer"}`))
		first <- err
	}()
	<-resumeEntered
	_, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"start","child_session_id":"child-restored","prompt":"two","mode":"answer"}`))
	if !errors.Is(err, connector.ErrChildBusy) {
		t.Fatalf("concurrent restored Start error = %v, want ErrChildBusy", err)
	}
	resumer.mu.Lock()
	if resumer.calls != 1 || !bytes.Equal(resumer.lastReference.Bytes(), reference.Bytes()) {
		t.Fatalf("Resume calls=%d exact-reference=%v", resumer.calls, bytes.Equal(resumer.lastReference.Bytes(), reference.Bytes()))
	}
	resumer.mu.Unlock()
	close(resumeRelease)
	if err := <-first; err != nil {
		t.Fatalf("first Start error = %v", err)
	}
	if _, err := delegator.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); err != nil {
		t.Fatalf("next_answer error = %v", err)
	}
}

func newReplayDelegator(
	t *testing.T,
	adapter connector.ReplayPort,
	resumer connector.ResumeChildFactory,
	factory connector.ChildFactory,
) *connector.Delegator {
	t.Helper()
	versions := connector.Versions{ACPProtocol: 1, Connector: "replay-fixture"}
	conformance := connector.Conformance{
		Exact: versions,
		Proven: connector.Capabilities{
			Subordinate: true, Resume: resumer != nil, Replay: true,
		},
		ReplayProvider: replay.ProviderCodex,
		ReplayVersion:  connector.CodexAppServerVersion,
	}
	profile := connector.NewConformedProfile(
		"fake", "Fake", versions, conformance, nil, factory, resumer, nil, nil,
		connector.ReplayBinding{Provider: replay.ProviderCodex, InstalledVersion: connector.CodexAppServerVersion, Port: adapter},
	)
	registry := connector.NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	delegator, err := connector.NewDelegator(context.Background(), "manager", registry, &testkit.EventRecorder{}, &testkit.SequenceIDs{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = delegator.Close() })
	return delegator
}

type scriptedReplayPort struct {
	mu            sync.Mutex
	calls         int
	lastCount     uint32
	lastReference replay.OpaqueRef
	counts        []uint32
	references    []replay.OpaqueRef
	invocations   int
	entered       chan struct{}
	release       chan struct{}
}

func (p *scriptedReplayPort) Capability(provider replay.ProviderID, version string) (replay.Profile, bool) {
	profile, ok := replay.ConformedProfile(provider, version)
	return profile, ok
}

func (p *scriptedReplayPort) Replay(ctx context.Context, request replay.BatchRequest, sink replay.Sink) error {
	if len(request.Targets) != 1 {
		return errors.New("fixture target count")
	}
	target := request.Targets[0]
	p.mu.Lock()
	p.calls++
	p.lastCount = target.Count
	p.lastReference = replay.OpaqueRefFromBytes(target.Reference.Bytes())
	p.counts = append(p.counts, target.Count)
	p.references = append(p.references, replay.OpaqueRefFromBytes(target.Reference.Bytes()))
	entered := p.entered
	release := p.release
	p.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
	}
	profile, _ := replay.ConformedProfile(target.Provider, target.InstalledVersion)
	boundary := replay.TargetBoundary{TargetID: target.TargetID, Provider: target.Provider, Profile: profile.ID}
	if err := sink.BeginTarget(boundary); err != nil {
		return err
	}
	for index := 1; index <= p.invocations; index++ {
		invocation := replay.NativeBoundary{InvocationID: "native-turn-" + string(rune('0'+index)), Status: "completed"}
		if err := sink.BeginInvocation(invocation); err != nil {
			return err
		}
		record, _ := json.Marshal(map[string]any{"type": "opaque_native_item", "sequence": index})
		if err := sink.Record(replay.NativeRecord{JSON: record}); err != nil {
			return err
		}
		if err := sink.EndInvocation(invocation); err != nil {
			return err
		}
	}
	return sink.EndTarget(boundary)
}

type blockingResumeFactory struct {
	mu            sync.Mutex
	calls         int
	lastReference replay.OpaqueRef
	entered       chan struct{}
	release       chan struct{}
	factory       connector.ChildFactory
}

func (r *blockingResumeFactory) Resume(ctx context.Context, reference replay.OpaqueRef) (connector.ChildPort, error) {
	r.mu.Lock()
	r.calls++
	r.lastReference = replay.OpaqueRefFromBytes(reference.Bytes())
	entered := r.entered
	release := r.release
	r.mu.Unlock()
	if entered != nil {
		select {
		case <-entered:
		default:
			close(entered)
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	return r.factory.New(ctx)
}
