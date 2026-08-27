package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticPrivacyCanary = "SYNTHETIC_PRIVATE_CANARY_DO_NOT_EXPOSE"

type fakeCodexThreadReader struct {
	raw   json.RawMessage
	err   error
	calls int
	gotID string
	hook  func()
}

func (reader *fakeCodexThreadReader) ThreadRead(_ context.Context, threadID string) (json.RawMessage, error) {
	reader.calls++
	reader.gotID = threadID
	if reader.hook != nil {
		reader.hook()
	}
	return append(json.RawMessage(nil), reader.raw...), reader.err
}

type capturedInvocation struct {
	boundary NativeBoundary
	records  [][]byte
}

type captureSink struct {
	targets     []TargetBoundary
	invocations []capturedInvocation
	events      []string
	failStage   string
	cancel      context.CancelFunc
}

func (sink *captureSink) fail(stage string) error {
	sink.events = append(sink.events, stage)
	if sink.failStage == stage {
		return errors.New("sink failed: " + syntheticPrivacyCanary)
	}
	return nil
}

func (sink *captureSink) BeginTarget(boundary TargetBoundary) error {
	if err := sink.fail("begin_target"); err != nil {
		return err
	}
	sink.targets = append(sink.targets, boundary)
	return nil
}

func (sink *captureSink) BeginInvocation(boundary NativeBoundary) error {
	if err := sink.fail("begin_invocation"); err != nil {
		return err
	}
	sink.invocations = append(sink.invocations, capturedInvocation{boundary: boundary})
	return nil
}

func (sink *captureSink) Record(record NativeRecord) error {
	if err := sink.fail("record"); err != nil {
		return err
	}
	last := len(sink.invocations) - 1
	sink.invocations[last].records = append(sink.invocations[last].records, append([]byte(nil), record.JSON...))
	if sink.cancel != nil {
		sink.cancel()
		sink.cancel = nil
	}
	return nil
}

func (sink *captureSink) EndInvocation(NativeBoundary) error {
	return sink.fail("end_invocation")
}

func (sink *captureSink) EndTarget(TargetBoundary) error {
	return sink.fail("end_target")
}

type fixtureEnvironment struct {
	service *Service
	codex   *fakeCodexThreadReader
	roots   map[ProviderID]string
	refs    map[ProviderID]OpaqueRef
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeNativeFile(t *testing.T, root, rel string, raw []byte) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newFixtureEnvironment(t *testing.T, limits Limits) fixtureEnvironment {
	t.Helper()
	base := t.TempDir()
	roots := map[ProviderID]string{
		ProviderClaude: filepath.Join(base, "claude-root"),
		ProviderGemini: filepath.Join(base, "gemini-root"),
		ProviderGrok:   filepath.Join(base, "grok-root"),
	}
	writeNativeFile(t, roots[ProviderClaude],
		filepath.Join("projects", "claude-synthetic-project", "claude-synthetic-session.jsonl"),
		fixtureBytes(t, "claude/session.jsonl"))
	writeNativeFile(t, roots[ProviderGemini],
		filepath.Join("tmp", "gemini-synthetic-project", "chats", "session-synthetic.json"),
		fixtureBytes(t, "gemini/conversation.json"))
	writeNativeFile(t, roots[ProviderGrok],
		filepath.Join("sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl"),
		fixtureBytes(t, "grok/updates.jsonl"))

	codex := &fakeCodexThreadReader{raw: fixtureBytes(t, "codex/thread.json")}
	service, err := NewService(Dependencies{Codex: codex, Roots: roots}, limits)
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[ProviderID]OpaqueRef, 4)
	refs[ProviderCodex], err = NewCodexReference("codex-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	refs[ProviderClaude], err = NewClaudeReference("claude-synthetic-project", "claude-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	refs[ProviderGemini], err = NewGeminiReference("gemini-synthetic-project", "session-synthetic.json", "gemini-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	refs[ProviderGrok], err = NewGrokReference("grok-synthetic-workspace", "grok-synthetic-session")
	if err != nil {
		t.Fatal(err)
	}
	return fixtureEnvironment{service: service, codex: codex, roots: roots, refs: refs}
}

func installedVersion(provider ProviderID) string {
	switch provider {
	case ProviderCodex:
		return "0.150.1"
	case ProviderClaude:
		return "2.1.226"
	case ProviderGemini:
		return "0.25.0-preview.0"
	case ProviderGrok:
		return "1.0.5"
	default:
		return ""
	}
}

func (environment fixtureEnvironment) request(provider ProviderID, count uint32) Request {
	return Request{
		TargetID:         "target-" + string(provider),
		Provider:         provider,
		InstalledVersion: installedVersion(provider),
		Reference:        environment.refs[provider],
		Count:            count,
	}
}

func requireCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	actual, ok := CodeOf(err)
	if !ok || actual != code {
		t.Fatalf("error code = %q, %v; want %q", actual, err, code)
	}
}

func joinedRecords(invocations []capturedInvocation) string {
	var buffer bytes.Buffer
	for _, invocation := range invocations {
		for _, record := range invocation.records {
			buffer.Write(record)
			buffer.WriteByte('\n')
		}
	}
	return buffer.String()
}

func TestPinnedProfiles(t *testing.T) {
	profiles := Profiles()
	if len(profiles) != 4 {
		t.Fatalf("profile count = %d", len(profiles))
	}
	seen := make(map[ProviderID]bool)
	for _, profile := range profiles {
		if seen[profile.Provider] {
			t.Fatalf("duplicate provider profile: %s", profile.Provider)
		}
		seen[profile.Provider] = true
		if !strings.HasPrefix(profile.Fingerprint, "sha256:") || len(profile.Fingerprint) != len("sha256:")+64 {
			t.Fatalf("profile %s has non-digest fingerprint %q", profile.ID, profile.Fingerprint)
		}
		if matched, ok := ConformedProfile(profile.Provider, profile.InstalledVersion); !ok || matched != profile {
			t.Fatalf("exact profile did not match: %s", profile.ID)
		}
		if _, ok := ConformedProfile(profile.Provider, profile.InstalledVersion+"+new"); ok {
			t.Fatalf("non-exact version matched: %s", profile.ID)
		}
	}
}

func TestReplayExactOneAndNForEveryProvider(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	tests := []struct {
		provider   ProviderID
		firstID    string
		lastID     string
		lastStatus string
		toolNeedle string
		owned      string
	}{
		{ProviderCodex, "codex-turn-1", "codex-turn-2", "failed", "commandExecution", "futureOwnedItem"},
		{ProviderClaude, "claude-user-1", "claude-user-2", "failed", "tool_result", "future-native"},
		{ProviderGemini, "gemini-user-1", "gemini-user-2", "error", "toolCalls", "future_owned_message"},
		{ProviderGrok, "grok-prompt-0", "grok-prompt-1", "cancelled", "tool_call_update", "future_owned_update"},
	}
	for _, test := range tests {
		t.Run(string(test.provider)+"/one", func(t *testing.T) {
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{
				Targets: []Request{environment.request(test.provider, 1)},
			}, sink)
			if err != nil {
				t.Fatal(err)
			}
			if len(sink.targets) != 1 || len(sink.invocations) != 1 {
				t.Fatalf("targets/invocations = %d/%d", len(sink.targets), len(sink.invocations))
			}
			if got := sink.invocations[0].boundary; got.InvocationID != test.lastID || got.Status != test.lastStatus {
				t.Fatalf("last boundary = %+v", got)
			}
			if !strings.Contains(joinedRecords(sink.invocations), test.owned) {
				t.Fatalf("unknown owned record was not passed through")
			}
		})

		t.Run(string(test.provider)+"/N", func(t *testing.T) {
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{
				Targets: []Request{environment.request(test.provider, 2)},
			}, sink)
			if err != nil {
				t.Fatal(err)
			}
			if len(sink.invocations) != 2 || sink.invocations[0].boundary.InvocationID != test.firstID ||
				sink.invocations[1].boundary.InvocationID != test.lastID {
				t.Fatalf("unexpected exact N boundaries: %+v", sink.invocations)
			}
			raw := joinedRecords(sink.invocations)
			if !strings.Contains(raw, "SYNTHETIC_REPLAY_CANARY") || !strings.Contains(raw, test.toolNeedle) {
				t.Fatalf("native canary/tool record missing for %s", test.provider)
			}
		})
	}
}

func TestUnavailableRangeAndBoundsEmitNothing(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{MaxCount: 2})
	for _, provider := range []ProviderID{ProviderCodex, ProviderClaude, ProviderGemini, ProviderGrok} {
		t.Run(string(provider)+"/range", func(t *testing.T) {
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{
				Targets: []Request{environment.request(provider, 3)},
			}, sink)
			requireCode(t, err, CodeBoundExceeded)
			if len(sink.events) != 0 {
				t.Fatalf("bounded request emitted %v", sink.events)
			}
		})
	}

	environment = newFixtureEnvironment(t, Limits{})
	for _, provider := range []ProviderID{ProviderCodex, ProviderClaude, ProviderGemini, ProviderGrok} {
		t.Run(string(provider)+"/unavailable", func(t *testing.T) {
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{
				Targets: []Request{environment.request(provider, 3)},
			}, sink)
			requireCode(t, err, CodeRangeUnavailable)
			if len(sink.events) != 0 {
				t.Fatalf("unavailable range emitted %v", sink.events)
			}
		})
	}
	zero := environment.request(ProviderCodex, 0)
	sink := &captureSink{}
	requireCode(t, environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{zero}}, sink), CodeBoundExceeded)
	if len(sink.events) != 0 {
		t.Fatal("zero count emitted")
	}
}

func TestMultiTargetPreparationIsAtomic(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	sink := &captureSink{}
	err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
		environment.request(ProviderCodex, 1),
		environment.request(ProviderGemini, 3),
	}}, sink)
	requireCode(t, err, CodeRangeUnavailable)
	if environment.codex.calls != 1 {
		t.Fatalf("first target was not prepared: calls=%d", environment.codex.calls)
	}
	if len(sink.events) != 0 {
		t.Fatalf("atomic preparation emitted: %v", sink.events)
	}
}

func TestWrongInstalledVersionFailsBeforeNativeRead(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	for _, provider := range []ProviderID{ProviderCodex, ProviderClaude, ProviderGemini, ProviderGrok} {
		request := environment.request(provider, 1)
		request.InstalledVersion += "-drift"
		sink := &captureSink{}
		err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{request}}, sink)
		requireCode(t, err, CodeUnsupportedVersion)
		if len(sink.events) != 0 {
			t.Fatalf("wrong version emitted for %s", provider)
		}
	}
	if environment.codex.calls != 0 {
		t.Fatalf("Codex API called before version gate: %d", environment.codex.calls)
	}
}

func TestOpaqueReferencesAndResumeResolvers(t *testing.T) {
	codex, err := NewCodexReference("thread-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := NewClaudeReference("project-synthetic", "session-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := NewGeminiReference("hash-synthetic", "session-file.json", "session-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	grok, err := NewGrokReference("%2FUsers%2Fsynthetic%2Fworkspace", "session-synthetic")
	if err != nil {
		t.Fatal(err)
	}

	if got, err := ResolveCodexReference(codex); err != nil || got != "thread-synthetic" {
		t.Fatalf("Codex resolve = %q, %v", got, err)
	}
	if scope, session, err := ResolveClaudeReference(claude); err != nil || scope != "project-synthetic" || session != "session-synthetic" {
		t.Fatalf("Claude resolve = %q/%q, %v", scope, session, err)
	}
	if scope, key, session, err := ResolveGeminiReference(gemini); err != nil || scope != "hash-synthetic" || key != "session-file.json" || session != "session-synthetic" {
		t.Fatalf("Gemini resolve = %q/%q/%q, %v", scope, key, session, err)
	}
	if scope, session, err := ResolveGrokReference(grok); err != nil || scope != "%2FUsers%2Fsynthetic%2Fworkspace" || session != "session-synthetic" {
		t.Fatalf("Grok resolve = %q/%q, %v", scope, session, err)
	}

	if _, _, err := ResolveClaudeReference(codex); err == nil {
		t.Fatal("cross-provider reference resolved")
	} else {
		requireCode(t, err, CodeInvalidReference)
	}

	constructorBytes := []byte(`{"v":1,"provider":"codex","session_id":"copy-safe"}`)
	copyRef := OpaqueRefFromBytes(constructorBytes)
	constructorBytes[0] = '!'
	returned := copyRef.Bytes()
	returned[0] = '!'
	if got, err := ResolveCodexReference(copyRef); err != nil || got != "copy-safe" {
		t.Fatalf("opaque defensive copy failed: %q, %v", got, err)
	}

	for name, raw := range map[string][]byte{
		"unknown-field": []byte(`{"v":1,"provider":"codex","session_id":"` + syntheticPrivacyCanary + `","future":true}`),
		"second-value":  []byte(`{"v":1,"provider":"codex","session_id":"` + syntheticPrivacyCanary + `"}{}`),
		"trailing-junk": []byte(`{"v":1,"provider":"codex","session_id":"` + syntheticPrivacyCanary + `"}{`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveCodexReference(OpaqueRefFromBytes(raw))
			requireCode(t, err, CodeInvalidReference)
			if strings.Contains(err.Error(), syntheticPrivacyCanary) {
				t.Fatal("reference content leaked through error")
			}
		})
	}
}

func TestEncodedGrokWorkspaceKeyResolvesTheExactLiteralDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "grok-root")
	scope := "%2FUsers%2Fsynthetic%2Fworkspace"
	sessionID := "grok-synthetic-session"
	writeNativeFile(t, root,
		filepath.Join("sessions", scope, sessionID, "updates.jsonl"),
		fixtureBytes(t, "grok/updates.jsonl"))
	service, err := NewService(Dependencies{Roots: map[ProviderID]string{ProviderGrok: root}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := NewGrokReference(scope, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	err = service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "encoded-workspace", Provider: ProviderGrok, InstalledVersion: "1.0.5",
		Reference: reference, Count: 1,
	}}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.invocations) != 1 || sink.invocations[0].boundary.InvocationID != "grok-prompt-1" {
		t.Fatalf("encoded workspace selected wrong invocation: %+v", sink.invocations)
	}
}

func TestReferenceContainmentAttacks(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})
	attacks := []string{
		"../outside", "/absolute", `C:\\absolute`, "nested/name", "..%2foutside",
		"%252e%252e%252foutside", "%25252e%25252e%25252foutside",
		"nul\x00byte", "nul%00byte",
	}
	for _, attack := range attacks {
		t.Run(fmt.Sprintf("%q", attack), func(t *testing.T) {
			if _, err := NewClaudeReference("safe-project", attack); !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("constructor accepted %q: %v", attack, err)
			}
			raw, _ := json.Marshal(nativeReference{Version: 1, Provider: ProviderClaude, SessionID: attack, Scope: "safe-project"})
			request := environment.request(ProviderClaude, 1)
			request.Reference = OpaqueRefFromBytes(raw)
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{request}}, sink)
			requireCode(t, err, CodeInvalidReference)
			if len(sink.events) != 0 {
				t.Fatal("containment attack emitted")
			}
		})
	}
	for _, attack := range []string{"..%2foutside", "%252e%252e%252foutside", "%25252e%25252e%25252foutside", "nul%00scope"} {
		t.Run("Grok scope/"+attack, func(t *testing.T) {
			if _, err := NewGrokReference(attack, "safe-session"); !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("Grok constructor accepted encoded scope attack %q: %v", attack, err)
			}
			raw, _ := json.Marshal(nativeReference{
				Version: 1, Provider: ProviderGrok, SessionID: "safe-session", Scope: attack,
			})
			request := environment.request(ProviderGrok, 1)
			request.Reference = OpaqueRefFromBytes(raw)
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{request}}, sink)
			requireCode(t, err, CodeInvalidReference)
			if len(sink.events) != 0 {
				t.Fatal("encoded Grok scope attack emitted")
			}
		})
	}

	root := filepath.Join(t.TempDir(), "claude-root")
	outside := writeNativeFile(t, t.TempDir(), "outside.jsonl", fixtureBytes(t, "claude/session.jsonl"))
	link := filepath.Join(root, "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	service, err := NewService(Dependencies{Roots: map[ProviderID]string{ProviderClaude: root}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	reference, _ := NewClaudeReference("claude-synthetic-project", "claude-synthetic-session")
	sink := &captureSink{}
	err = service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "symlink", Provider: ProviderClaude, InstalledVersion: "2.1.226", Reference: reference, Count: 1,
	}}}, sink)
	requireCode(t, err, CodeContainment)
	if len(sink.events) != 0 {
		t.Fatal("symlink escape emitted")
	}
}
