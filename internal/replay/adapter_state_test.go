package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompactionAndRewindRetention(t *testing.T) {
	environment := newFixtureEnvironment(t, Limits{})

	writeNativeFile(t, environment.roots[ProviderClaude],
		filepath.Join("projects", "claude-compact-project", "claude-compact-session.jsonl"),
		fixtureBytes(t, "claude/compact.jsonl"))
	claudeReference, err := NewClaudeReference("claude-compact-project", "claude-compact-session")
	if err != nil {
		t.Fatal(err)
	}
	claudeRequest := Request{
		TargetID: "claude-compact", Provider: ProviderClaude, InstalledVersion: "2.1.226",
		Reference: claudeReference, Count: 1,
	}
	claudeSink := &captureSink{}
	if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{claudeRequest}}, claudeSink); err != nil {
		t.Fatal(err)
	}
	if len(claudeSink.invocations) != 1 || claudeSink.invocations[0].boundary.InvocationID != "compact-user-1" {
		t.Fatalf("Claude compaction retained wrong turns: %+v", claudeSink.invocations)
	}
	if raw := joinedRecords(claudeSink.invocations); strings.Contains(raw, "old synthetic") || !strings.Contains(raw, "new synthetic") {
		t.Fatalf("Claude compaction bytes wrong: %s", raw)
	}
	claudeRequest.Count = 2
	zeroSink := &captureSink{}
	requireCode(t, environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{claudeRequest}}, zeroSink), CodeRangeUnavailable)
	if len(zeroSink.events) != 0 {
		t.Fatal("Claude pre-compaction turn was emitted")
	}

	writeNativeFile(t, environment.roots[ProviderGrok],
		filepath.Join("sessions", "grok-rewind-workspace", "grok-rewind-session", "updates.jsonl"),
		fixtureBytes(t, "grok/rewind.jsonl"))
	rewindReference, err := NewGrokReference("grok-rewind-workspace", "grok-rewind-session")
	if err != nil {
		t.Fatal(err)
	}
	rewindSink := &captureSink{}
	if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{{
		TargetID: "grok-rewind", Provider: ProviderGrok, InstalledVersion: "1.0.5",
		Reference: rewindReference, Count: 2,
	}}}, rewindSink); err != nil {
		t.Fatal(err)
	}
	if len(rewindSink.invocations) != 2 ||
		rewindSink.invocations[0].boundary.InvocationID != "rewind-prompt-0" ||
		rewindSink.invocations[1].boundary.InvocationID != "rewind-live-1" {
		t.Fatalf("Grok rewind retained wrong branch: %+v", rewindSink.invocations)
	}
	if raw := joinedRecords(rewindSink.invocations); strings.Contains(raw, "dead branch") || !strings.Contains(raw, "live branch") {
		t.Fatalf("Grok rewind bytes wrong: %s", raw)
	}

	writeNativeFile(t, environment.roots[ProviderGrok],
		filepath.Join("sessions", "grok-compact-workspace", "grok-compact-session", "updates.jsonl"),
		fixtureBytes(t, "grok/compact.jsonl"))
	compactReference, err := NewGrokReference("grok-compact-workspace", "grok-compact-session")
	if err != nil {
		t.Fatal(err)
	}
	compactRequest := Request{
		TargetID: "grok-compact", Provider: ProviderGrok, InstalledVersion: "1.0.5",
		Reference: compactReference, Count: 1,
	}
	compactSink := &captureSink{}
	if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{compactRequest}}, compactSink); err != nil {
		t.Fatal(err)
	}
	if len(compactSink.invocations) != 1 || compactSink.invocations[0].boundary.InvocationID != "compact-prompt-1" {
		t.Fatalf("Grok compaction retained wrong turns: %+v", compactSink.invocations)
	}
	if raw := joinedRecords(compactSink.invocations); strings.Contains(raw, "old synthetic") || !strings.Contains(raw, "new synthetic") {
		t.Fatalf("Grok compaction bytes wrong: %s", raw)
	}
}

func TestPartialWriteRulesAndConcurrentSnapshots(t *testing.T) {
	t.Run("Claude unterminated line fails", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.TrimSuffix(raw, []byte{'\n'}), 0o600); err != nil {
			t.Fatal(err)
		}
		sink := &captureSink{}
		err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderClaude, 1),
		}}, sink)
		requireCode(t, err, CodeCorruptHistory)
		if len(sink.events) != 0 {
			t.Fatal("Claude partial emitted")
		}
	})

	t.Run("Grok unterminated tail is excluded", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(`{"timestamp":999,"method":"session/update"`); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		sink := &captureSink{}
		if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderGrok, 2),
		}}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.invocations) != 2 || strings.Contains(joinedRecords(sink.invocations), `"timestamp":999`) {
			t.Fatal("Grok partial tail was not excluded")
		}
	})

	t.Run("stable append fails", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
		environment.service.afterFileRead = func(provider ProviderID) {
			if provider != ProviderClaude {
				return
			}
			environment.service.afterFileRead = nil
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(`{"type":"summary"}` + "\n"); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		sink := &captureSink{}
		err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderClaude, 1),
		}}, sink)
		requireCode(t, err, CodeConcurrentWrite)
		if len(sink.events) != 0 {
			t.Fatal("concurrent stable write emitted")
		}
	})

	t.Run("append-only growth accepts original prefix", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
		environment.service.afterFileRead = func(provider ProviderID) {
			if provider != ProviderGrok {
				return
			}
			environment.service.afterFileRead = nil
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			line := `{"timestamp":10,"method":"session/update","params":{"sessionId":"grok-synthetic-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"concurrent append"}}}}` + "\n"
			if _, err := file.WriteString(line); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		sink := &captureSink{}
		if err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderGrok, 2),
		}}, sink); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(joinedRecords(sink.invocations), "concurrent append") {
			t.Fatal("post-snapshot append leaked into Replay")
		}
	})

	t.Run("append-only rewind fails", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		environment.service.afterFileRead = func(provider ProviderID) {
			if provider == ProviderGrok {
				environment.service.afterFileRead = nil
				if err := os.Truncate(path, info.Size()/2); err != nil {
					t.Fatal(err)
				}
			}
		}
		sink := &captureSink{}
		err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderGrok, 1),
		}}, sink)
		requireCode(t, err, CodeConcurrentWrite)
		if len(sink.events) != 0 {
			t.Fatal("concurrent rewind emitted")
		}
	})
}

func TestIdentityVersionAndUnknownControlFailClosed(t *testing.T) {
	identityTests := []struct {
		name     string
		provider ProviderID
		mutate   func([]byte) []byte
	}{
		{"Codex", ProviderCodex, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("codex-synthetic-session"), []byte("codex-mismatch-session"), 1)
		}},
		{"Claude", ProviderClaude, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("claude-synthetic-session"), []byte("claude-mismatch-session"), 1)
		}},
		{"Gemini", ProviderGemini, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("gemini-synthetic-session"), []byte("gemini-mismatch-session"), 1)
		}},
		{"Grok", ProviderGrok, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("grok-synthetic-session"), []byte("grok-mismatch-session"), 1)
		}},
	}
	for _, test := range identityTests {
		t.Run(test.name+" identity", func(t *testing.T) {
			environment := newFixtureEnvironment(t, Limits{})
			switch test.provider {
			case ProviderCodex:
				environment.codex.raw = test.mutate(environment.codex.raw)
			case ProviderClaude:
				path := filepath.Join(environment.roots[test.provider], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
				raw, _ := os.ReadFile(path)
				if err := os.WriteFile(path, test.mutate(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			case ProviderGemini:
				path := filepath.Join(environment.roots[test.provider], "tmp", "gemini-synthetic-project", "chats", "session-synthetic.json")
				raw, _ := os.ReadFile(path)
				if err := os.WriteFile(path, test.mutate(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			case ProviderGrok:
				path := filepath.Join(environment.roots[test.provider], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
				raw, _ := os.ReadFile(path)
				if err := os.WriteFile(path, test.mutate(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(test.provider, 1),
			}}, sink)
			requireCode(t, err, CodeReferenceMismatch)
			if len(sink.events) != 0 {
				t.Fatal("identity mismatch emitted")
			}
		})
	}

	t.Run("Claude embedded version", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
		raw, _ := os.ReadFile(path)
		raw = bytes.Replace(raw, []byte(`"version":"2.1.226"`), []byte(`"version":"2.1.227"`), 1)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderClaude, 1),
		}}, &captureSink{})
		requireCode(t, err, CodeUnsupportedVersion)
	})

	unknownTests := []struct {
		name     string
		provider ProviderID
		mutate   func(fixtureEnvironment)
	}{
		{"Codex status", ProviderCodex, func(environment fixtureEnvironment) {
			environment.codex.raw = bytes.Replace(environment.codex.raw, []byte(`"inProgress"`), []byte(`"futureStatus"`), 1)
		}},
		{"Claude control", ProviderClaude, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
			raw, _ := os.ReadFile(path)
			prefix := []byte(`{"type":"future-control","sessionId":"claude-synthetic-session","version":"2.1.226"}` + "\n")
			if err := os.WriteFile(path, append(prefix, raw...), 0o600); err != nil {
				panic(err)
			}
		}},
		{"Gemini control", ProviderGemini, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderGemini], "tmp", "gemini-synthetic-project", "chats", "session-synthetic.json")
			var conversation map[string]json.RawMessage
			raw, _ := os.ReadFile(path)
			_ = json.Unmarshal(raw, &conversation)
			var messages []json.RawMessage
			_ = json.Unmarshal(conversation["messages"], &messages)
			unknown := json.RawMessage(`{"id":"control","timestamp":"2026-01-01T00:00:00Z","type":"future-control","content":"control"}`)
			messages = append([]json.RawMessage{unknown}, messages...)
			conversation["messages"], _ = json.Marshal(messages)
			updated, _ := json.Marshal(conversation)
			if err := os.WriteFile(path, updated, 0o600); err != nil {
				panic(err)
			}
		}},
		{"Grok method", ProviderGrok, func(environment fixtureEnvironment) {
			path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
			file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = file.WriteString(`{"timestamp":99,"method":"future/control","params":{"sessionId":"grok-synthetic-session","update":{"sessionUpdate":"future"}}}` + "\n")
			_ = file.Close()
		}},
	}
	for _, test := range unknownTests {
		t.Run(test.name, func(t *testing.T) {
			environment := newFixtureEnvironment(t, Limits{})
			test.mutate(environment)
			sink := &captureSink{}
			err := environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(test.provider, 1),
			}}, sink)
			requireCode(t, err, CodeUnknownSchema)
			if len(sink.events) != 0 {
				t.Fatal("unknown control emitted")
			}
		})
	}
}

func TestProviderInvocationCorrelationFailsClosed(t *testing.T) {
	t.Run("Claude rejects a known but stale active parent", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		old := []byte(`"uuid":"claude-agent-3","parentUuid":"claude-future-1"`)
		updated := []byte(`"uuid":"claude-agent-3","parentUuid":"claude-user-1"`)
		if !bytes.Contains(raw, old) {
			t.Fatal("Claude assistant fixture anchor missing")
		}
		raw = bytes.Replace(raw, old, updated, 1)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		sink := &captureSink{}
		err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderClaude, 1),
		}}, sink)
		requireCode(t, err, CodeAmbiguousBoundary)
		if len(sink.events) != 0 {
			t.Fatal("stale Claude branch emitted")
		}
	})

	t.Run("Claude rejects a terminal from an earlier branch", func(t *testing.T) {
		environment := newFixtureEnvironment(t, Limits{})
		path := filepath.Join(environment.roots[ProviderClaude], "projects", "claude-synthetic-project", "claude-synthetic-session.jsonl")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		old := []byte(`"uuid":"claude-terminal-2","parentUuid":"claude-agent-3"`)
		updated := []byte(`"uuid":"claude-terminal-2","parentUuid":"claude-agent-1"`)
		if !bytes.Contains(raw, old) {
			t.Fatal("Claude terminal fixture anchor missing")
		}
		raw = bytes.Replace(raw, old, updated, 1)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		sink := &captureSink{}
		err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
			environment.request(ProviderClaude, 1),
		}}, sink)
		requireCode(t, err, CodeAmbiguousBoundary)
		if len(sink.events) != 0 {
			t.Fatal("uncorrelated Claude terminal emitted")
		}
	})

	for _, test := range []struct {
		name    string
		old     string
		updated string
		code    ErrorCode
	}{
		{
			name:    "Grok requires the starting prompt correlation",
			old:     `"_meta":{"promptId":"grok-prompt-0"}`,
			updated: `"_meta":{"promptId":""}`,
			code:    CodeUnknownSchema,
		},
		{
			name:    "Grok completion must match its exact prompt",
			old:     `"prompt_id":"grok-prompt-1"`,
			updated: `"prompt_id":"grok-foreign-prompt"`,
			code:    CodeAmbiguousBoundary,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			environment := newFixtureEnvironment(t, Limits{})
			path := filepath.Join(environment.roots[ProviderGrok], "sessions", "grok-synthetic-workspace", "grok-synthetic-session", "updates.jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(test.old)) {
				t.Fatalf("Grok fixture anchor missing: %s", test.old)
			}
			raw = bytes.Replace(raw, []byte(test.old), []byte(test.updated), 1)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			sink := &captureSink{}
			err = environment.service.Replay(context.Background(), BatchRequest{Targets: []Request{
				environment.request(ProviderGrok, 1),
			}}, sink)
			requireCode(t, err, test.code)
			if len(sink.events) != 0 {
				t.Fatal("uncorrelated Grok history emitted")
			}
		})
	}
}
