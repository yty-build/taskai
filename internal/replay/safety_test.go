package replay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type gateBlockingSink struct {
	captureSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (sink *gateBlockingSink) BeginTarget(boundary TargetBoundary) error {
	sink.once.Do(func() { close(sink.entered) })
	<-sink.release
	return sink.captureSink.BeginTarget(boundary)
}

func TestCorruptAndOversizedSourcesFailWithoutEmission(t *testing.T) {
	corrupt := []struct {
		provider ProviderID
		mutate   func(fixtureEnvironment)
	}{
		{ProviderCodex, func(environment fixtureEnvironment) { environment.codex.raw = json.RawMessage(`{`) }},
		{ProviderClaude, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
			if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
				panic(err)
			}
		}},
		{ProviderGemini, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderGemini], "tmp", "gemini-synthetic-project", "chats", "session-synthetic.json")
			if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
				panic(err)
			}
		}},
		{ProviderGrok, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
			if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
				panic(err)
			}
		}},
	}
	for _, test := range corrupt {
		t.Run(string(test.provider)+"/corrupt", func(t *testing.T) {
			environment := newFixtureEnvironment(t, Limits{})
			test.mutate(environment)
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(test.provider, 1),
			}}, sink)
			requireCode(t, err, CodeCorruptHistory)
			if len(sink.events) != 0 {
				t.Fatal("corrupt source emitted")
			}
		})
	}

	for _, limits := range []Limits{
		{MaxSourceBytes: 64},
		{MaxRecordBytes: 32},
		{MaxRecords: 1},
		{MaxSelectedBytes: 1},
	} {
		for _, provider := range []ProviderID{ProviderCodex, ProviderClaude, ProviderGemini, ProviderGrok} {
			t.Run(string(provider)+"/bounded", func(t *testing.T) {
				environment := newFixtureEnvironment(t, limits)
				sink := &captureSink{}
				err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
					environment.request(provider, 1),
				}}, sink)
				requireCode(t, err, CodeBoundExceeded)
				if len(sink.events) != 0 {
					t.Fatal("bounded source emitted")
				}
			})
		}
	}
}

func TestServiceRequestBoundsAndExactLookup(t *testing.T) {
	if _, err := NewService(Dependencies{}, Limits{MaxTargets: -1}); !errors.Is(err, ErrBoundExceeded) {
		t.Fatalf("negative limits accepted: %v", err)
	}
	environment := newFixtureEnvironment(t, Limits{MaxTargets: 1})
	sink := &captureSink{}
	err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
		environment.request(ProviderCodex, 1), environment.request(ProviderClaude, 1),
	}}, sink)
	requireCode(t, err, CodeBoundExceeded)
	if len(sink.events) != 0 {
		t.Fatal("too many targets emitted")
	}

	environment = newFixtureEnvironment(t, Limits{})
	first := environment.request(ProviderCodex, 1)
	second := environment.request(ProviderClaude, 1)
	second.TargetID = first.TargetID
	sink = &captureSink{}
	err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{first, second}}, sink)
	requireCode(t, err, CodeInvalidReference)
	if len(sink.events) != 0 {
		t.Fatal("duplicate target IDs emitted")
	}
	overlong := environment.request(ProviderCodex, 1)
	overlong.TargetID = strings.Repeat("x", maxTargetIDBytes+1)
	sink = &captureSink{}
	err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{overlong}}, sink)
	requireCode(t, err, CodeBoundExceeded)
	if len(sink.events) != 0 {
		t.Fatal("overlong target ID emitted")
	}

	root := filepath.Join(t.TempDir(), "claude-root")
	// A valid-looking transcript elsewhere is deliberately not discovered.
	writeNativeFile(t, root, filepath.Join("projects", "exact-project", "other-session.jsonl"), fixtureBytes(t, "claude/session.jsonl"))
	service, err := NewService(Dependencies{Roots: map[ProviderID]string{ProviderClaude: root}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewClaudeReference("exact-project", "missing-session")
	if err != nil {
		t.Fatal(err)
	}
	sink = &captureSink{}
	err = service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "exact-only", Provider: ProviderClaude, InstalledVersion: "2.1.226", Reference: reference, Count: 1,
	}}}, sink)
	requireCode(t, err, CodeHistoryMissing)
	if len(sink.events) != 0 {
		t.Fatal("exact lookup miss emitted")
	}
}

func TestGlobalPreparedBatchMemoryBound(t *testing.T) {
	probeEnvironment := newFixtureEnvironment(t, Limits{})
	profile, _ := ConformedProfile(ProviderCodex, "0.150.1")
	probe, err := probeEnvironment.service.prepare(context.Background(), profile, probeEnvironment.request(ProviderCodex, 1))
	if err != nil {
		t.Fatal(err)
	}
	oneTargetBytes := probe.bytes
	probe.close()
	if oneTargetBytes < 1 {
		t.Fatal("prepared memory estimate was empty")
	}

	environment := newFixtureEnvironment(t, Limits{MaxBatchBytes: oneTargetBytes})
	first := environment.request(ProviderCodex, 1)
	if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{first}}, &captureSink{}); err != nil {
		t.Fatalf("one target should fit exact batch bound: %v", err)
	}
	second := first
	second.TargetID = first.TargetID + "-second"
	sink := &captureSink{}
	err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{first, second}}, sink)
	requireCode(t, err, CodeBoundExceeded)
	if len(sink.events) != 0 {
		t.Fatalf("cumulative batch overflow emitted: %v", sink.events)
	}
}

func TestCapabilityRequiresUsableExactSourceBoundary(t *testing.T) {
	realRoot := filepath.Join(t.TempDir(), "real-root")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reader := &fakeCodexThreadReader{raw: fixtureBytes(t, "codex/thread.json")}
	service, err := NewService(Dependencies{
		Codex: reader,
		Roots: map[ProviderID]string{
			ProviderClaude: realRoot,
			ProviderGemini: "relative-root",
			ProviderGrok:   symlinkRoot,
		},
	}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := service.Capability(ProviderCodex, "0.150.1"); !ok {
		t.Fatal("injected Codex API should be available")
	}
	if _, ok := service.Capability(ProviderClaude, "2.1.226"); !ok {
		t.Fatal("absolute real directory should be available")
	}
	if _, ok := service.Capability(ProviderGemini, "0.25.0-preview.0"); ok {
		t.Fatal("relative root reported available")
	}
	if _, ok := service.Capability(ProviderGrok, "1.0.5"); ok {
		t.Fatal("symlink root reported available")
	}
	if _, ok := service.Capability(ProviderClaude, "2.1.227"); ok {
		t.Fatal("wrong version reported available")
	}

	geminiReference, _ := NewGeminiReference("scope", "record.json", "session")
	sink := &captureSink{}
	err = service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "missing-root", Provider: ProviderGemini, InstalledVersion: "0.25.0-preview.0",
		Reference: geminiReference, Count: 1,
	}}}, sink)
	requireCode(t, err, CodeSourceFailure)
	if len(sink.events) != 0 {
		t.Fatal("unavailable source emitted")
	}
}

func TestConcurrentFileReplacementFails(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	path := filepath.Join(environment.roots[ProviderGemini], "tmp", "gemini-synthetic-project", "chats", "session-synthetic.json")
	replacement := filepath.Join(filepath.Dir(path), "replacement.json")
	if err := os.WriteFile(replacement, fixtureBytes(t, "gemini/conversation.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment.service.afterFileRead = func(provider ProviderID) {
		if provider == ProviderGemini {
			environment.service.afterFileRead = nil
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	sink := &captureSink{}
	err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
		environment.request(ProviderGemini, 1),
	}}, sink)
	requireCode(t, err, CodeConcurrentWrite)
	if len(sink.events) != 0 {
		t.Fatal("replacement race emitted")
	}
}

func TestConfiguredRootSwapToSymlinkFailsClosed(t *testing.T) {
	if !rootPinningSupported() {
		t.Skip("descriptor-pinned roots are unavailable")
	}
	base := t.TempDir()
	configured := filepath.Join(base, "configured-root")
	outside := filepath.Join(base, "outside-root")
	backup := filepath.Join(base, "original-root")
	writeNativeFile(t, configured,
		filepath.Join("projects", "claude-synthetic-project", "claude-synthetic-session.jsonl"),
		fixtureBytes(t, "claude/session.jsonl"))
	writeNativeFile(t, outside,
		filepath.Join("projects", "claude-synthetic-project", "claude-synthetic-session.jsonl"),
		fixtureBytes(t, "claude/session.jsonl"))
	service, err := NewService(Dependencies{Roots: map[ProviderID]string{ProviderClaude: configured}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	service.beforeRootOpen = func(provider ProviderID) {
		if provider != ProviderClaude {
			return
		}
		service.beforeRootOpen = nil
		if err := os.Rename(configured, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, configured); err != nil {
			t.Fatal(err)
		}
	}
	reference, err := NewClaudeReference("claude-synthetic-project", "claude-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	err = service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "root-swap", Provider: ProviderClaude, InstalledVersion: "2.1.226",
		Reference: reference, Count: 1,
	}}}, sink)
	requireCode(t, err, CodeContainment)
	if len(sink.events) != 0 {
		t.Fatal("root-swap escape emitted")
	}
}

func TestReplayGateBoundsAggregateConcurrentMemory(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	attempts := make(chan struct{}, 2)
	environment.service.beforeGateWait = func() { attempts <- struct{}{} }

	blocking := &gateBlockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, blocking)
	}()
	<-attempts
	<-blocking.entered
	if environment.codex.calls != 1 {
		t.Fatalf("first Replay source calls = %d", environment.codex.calls)
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- environment.service.Replay(secondCtx, BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, &captureSink{})
	}()
	<-attempts
	if environment.codex.calls != 1 {
		t.Fatalf("concurrent Replay bypassed aggregate gate: %d source calls", environment.codex.calls)
	}
	cancelSecond()
	requireCode(t, <-secondDone, CodeCancelled)
	if environment.codex.calls != 1 {
		t.Fatalf("cancelled gate waiter touched source: %d calls", environment.codex.calls)
	}

	close(blocking.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Replay failed after releasing gate: %v", err)
	}
}

func TestCancellationAndSinkFailureAreContentFreeAndReusable(t *testing.T) {
	t.Run("cancelled before prepare", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sink := &captureSink{}
		err := environment.service.Replay(ctx, BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, sink)
		requireCode(t, err, CodeCancelled)
		if environment.codex.calls != 0 || len(sink.events) != 0 {
			t.Fatal("pre-cancel touched source or sink")
		}
	})

	t.Run("cancelled during API prepare", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		ctx, cancel := context.WithCancel(context.Background())
		environment.codex.hook = cancel
		sink := &captureSink{}
		err := environment.service.Replay(ctx, BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, sink)
		requireCode(t, err, CodeCancelled)
		if len(sink.events) != 0 {
			t.Fatal("prepare cancellation emitted")
		}
	})

	t.Run("cancelled during file snapshot", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		ctx, cancel := context.WithCancel(context.Background())
		environment.service.afterFileRead = func(ProviderID) { cancel() }
		sink := &captureSink{}
		err := environment.service.Replay(ctx, BatchRequest{Targets: []Request{
			environment.request(ProviderGemini, 1),
		}}, sink)
		requireCode(t, err, CodeCancelled)
		if len(sink.events) != 0 {
			t.Fatal("snapshot cancellation emitted")
		}
	})

	t.Run("cancelled during emission cleans up", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		ctx, cancel := context.WithCancel(context.Background())
		sink := &captureSink{cancel: cancel}
		err := environment.service.Replay(ctx, BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, sink)
		requireCode(t, err, CodeCancelled)
		if strings.Contains(strings.Join(sink.events, ","), "end_invocation") ||
			strings.Contains(strings.Join(sink.events, ","), "end_target") {
			t.Fatalf("emission continued after cancellation: %v", sink.events)
		}
		// A new invocation succeeds after the failed call's buffers are released.
		if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderCodex, 1),
		}}, &captureSink{}); err != nil {
			t.Fatalf("service not reusable after cancellation: %v", err)
		}
	})

	for _, stage := range []string{"begin_target", "begin_invocation", "record", "end_invocation", "end_target"} {
		t.Run("sink/"+stage, func(t *testing.T) {
			environment := newFixtureEnvironment(t, Limits{})
			sink := &captureSink{failStage: stage}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(ProviderCodex, 1),
			}}, sink)
			requireCode(t, err, CodeSinkFailure)
			if strings.Contains(err.Error(), syntheticPrivacyCanary) {
				t.Fatal("sink error detail leaked")
			}
			if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(ProviderCodex, 1),
			}}, &captureSink{}); err != nil {
				t.Fatalf("service not reusable after sink failure: %v", err)
			}
		})
	}
}

func TestErrorsNeverExposeProviderContentReferencesOrPaths(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	environment.codex.err = errors.New("native error " + syntheticPrivacyCanary)
	reference, err := NewCodexReference(syntheticPrivacyCanary)
	if err != nil {
		t.Fatal(err)
	}
	request := environment.request(ProviderCodex, 1)
	request.Reference = reference
	sink := &captureSink{}
	err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{request}}, sink)
	requireCode(t, err, CodeSourceFailure)
	for _, forbidden := range []string{syntheticPrivacyCanary, environment.roots[ProviderClaude], "native error"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked %q: %v", forbidden, err)
		}
	}

	missing, err := NewClaudeReference("privacy-project", syntheticPrivacyCanary)
	if err != nil {
		t.Fatal(err)
	}
	request = Request{
		TargetID: "privacy", Provider: ProviderClaude, InstalledVersion: "2.1.226", Reference: missing, Count: 1,
	}
	err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{request}}, &captureSink{})
	requireCode(t, err, CodeHistoryMissing)
	if strings.Contains(err.Error(), syntheticPrivacyCanary) || strings.Contains(err.Error(), environment.roots[ProviderClaude]) {
		t.Fatalf("missing-history error leaked native identity: %v", err)
	}

	if _, ok := CodeOf(errors.New(syntheticPrivacyCanary)); ok {
		t.Fatal("non-Replay error classified")
	}
}

func TestNativeRecordsStayByteExactAndOrdered(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	tests := []struct {
		provider ProviderID
		exact    []byte
		before   string
		after    string
	}{
		{
			ProviderCodex,
			[]byte(`{"type":"commandExecution","id":"codex-tool-1","command":"printf synthetic","exitCode":0}`),
			`"id":"codex-user-1"`, `"id":"codex-agent-1"`,
		},
		{
			ProviderClaude,
			[]byte(`{"type":"user","sessionId":"claude-synthetic-session","version":"2.1.226","uuid":"claude-tool-result-1","parentUuid":"claude-agent-1","isSidechain":false,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"claude-tool-1","content":"synthetic result"}]}}`),
			`"uuid":"claude-agent-1"`, `"uuid":"claude-agent-2"`,
		},
		{
			ProviderGemini,
			[]byte(`{"id":"gemini-agent-1","timestamp":"2026-01-01T00:00:04.000Z","type":"gemini","content":"synthetic complete"}`),
			`"id":"gemini-agent-tool-1"`, `"id":"gemini-user-2"`,
		},
		{
			ProviderGrok,
			[]byte(`{"timestamp":1767225603,"method":"session/update","params":{"sessionId":"grok-synthetic-session","update":{"sessionUpdate":"tool_call_update","toolCallId":"grok-tool-0","status":"completed","rawOutput":{"content":"synthetic"}}}}`),
			`"sessionUpdate":"tool_call"`, `"sessionUpdate":"turn_completed"`,
		},
	}
	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			sink := &captureSink{}
			if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(test.provider, 2),
			}}, sink); err != nil {
				t.Fatal(err)
			}
			foundExact := false
			for _, invocation := range sink.invocations {
				for _, raw := range invocation.records {
					foundExact = foundExact || bytes.Equal(raw, test.exact)
				}
			}
			if !foundExact {
				t.Fatalf("exact native record not preserved: %s", test.exact)
			}
			joined := joinedRecords(sink.invocations)
			if strings.Index(joined, test.before) < 0 || strings.Index(joined, test.after) < 0 ||
				strings.Index(joined, test.before) >= strings.Index(joined, test.after) {
				t.Fatalf("native order changed: %s before %s", test.before, test.after)
			}
		})
	}
}

func TestReplayDoesNotMutateNativeFiles(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	paths := []string{
		filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl"),
		filepath.Join(environment.roots[ProviderGemini], "tmp", "gemini-synthetic-project", "chats", "session-synthetic.json"),
		filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl"),
	}
	before := make(map[string][32]byte, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = sha256.Sum256(raw)
	}
	for _, provider := range []ProviderID{ProviderClaude, ProviderGemini, ProviderGrok} {
		if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(provider, 1),
		}}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(raw) != before[path] {
			t.Fatalf("native source changed: %s", path)
		}
	}
}
