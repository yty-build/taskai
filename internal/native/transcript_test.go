package native

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
)

func TestReadCodexProviderTranscript(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "codex-conversation"
	path := filepath.Join(root, "sessions", "2026", "08", "16", "rollout-"+conversationID+".jsonl")
	writeTranscriptFixture(t, path, []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": conversationID, "cwd": workingDirectory}},
		map[string]any{"timestamp": "2026-08-16T01:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "SECRET_SYSTEM"}}}},
		map[string]any{"timestamp": "2026-08-16T01:00:01Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "hello\x1b[31mworld\x1b[0m"}},
		map[string]any{"timestamp": "2026-08-16T01:00:02Z", "type": "response_item", "payload": map[string]any{"id": "reason-1", "type": "reasoning", "encrypted_content": "SECRET_ENCRYPTED", "summary": []any{map[string]any{"type": "summary_text", "text": "safe summary"}}}},
		map[string]any{"timestamp": "2026-08-16T01:00:03Z", "type": "response_item", "payload": map[string]any{"id": "answer-1", "type": "message", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
		map[string]any{"timestamp": "2026-08-16T01:00:03Z", "type": "event_msg", "payload": map[string]any{"type": "agent_message", "message": "done"}},
	}, `{"type":"response_item"`)

	profile := agents.Profile{Name: "codex", Environment: map[string]string{"CODEX_HOME": root}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("read Codex transcript: %v", err)
	}
	assertTranscript(t, snapshot, []TranscriptKind{TranscriptUser, TranscriptReasoning, TranscriptAssistant}, []string{"helloworld", "safe summary", "done"})
	if snapshot.Records[2].Phase != "final" {
		t.Fatalf("assistant phase = %q", snapshot.Records[2].Phase)
	}
	assertNoTranscriptSecret(t, snapshot, "SECRET_SYSTEM", "SECRET_ENCRYPTED")
	second, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil || second.Checkpoint != snapshot.Checkpoint {
		t.Fatalf("stable checkpoint = %q, %v; want %q", second.Checkpoint, err, snapshot.Checkpoint)
	}
}

func TestReadCodexProviderTranscriptStreamsBeyondLegacySourceLimit(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "codex-large-conversation"
	path := filepath.Join(root, "sessions", conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONLine := func(record any) {
		t.Helper()
		encoded, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			file.Close()
			t.Fatal(marshalErr)
		}
		if _, writeErr := file.Write(append(encoded, '\n')); writeErr != nil {
			file.Close()
			t.Fatal(writeErr)
		}
	}
	writeJSONLine(map[string]any{"type": "session_meta", "payload": map[string]any{"id": conversationID, "cwd": workingDirectory}})
	writeJSONLine(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "old prompt"}})
	filler := []byte(`{"type":"provider_internal","padding":"` + strings.Repeat("x", 512<<10) + `"}` + "\n")
	for written := 0; written <= 33<<20; written += len(filler) {
		if _, err := file.Write(filler); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	writeJSONLine(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "newest prompt"}})
	writeJSONLine(map[string]any{"type": "response_item", "payload": map[string]any{
		"id": "latest-answer", "type": "message", "role": "assistant", "phase": "final_answer",
		"content": []any{map[string]any{"type": "output_text", "text": "newest answer"}},
	}})
	if _, err := file.WriteString(`{"type":"response_item"`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	profile := agents.Profile{Name: "codex", Environment: map[string]string{"CODEX_HOME": root}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("stream large Codex transcript: %v", err)
	}
	assertTranscript(t, snapshot,
		[]TranscriptKind{TranscriptUser, TranscriptUser, TranscriptAssistant},
		[]string{"old prompt", "newest prompt", "newest answer"},
	)
}

func TestWalkBoundedJSONLinesEnforcesLineLimitAndMidStreamCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oversized := filepath.Join(root, "oversized.jsonl")
	if err := os.WriteFile(oversized, []byte(`{"padding":"`+strings.Repeat("x", maxTranscriptLineBytes)+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := walkBoundedJSONLines(context.Background(), oversized, func(int, json.RawMessage) error { return nil }); err == nil || !strings.Contains(err.Error(), "line 1 exceeds") {
		t.Fatalf("oversized JSONL line error = %v", err)
	}

	cancellable := filepath.Join(root, "cancellable.jsonl")
	if err := os.WriteFile(cancellable, []byte("{}\n{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	err := walkBoundedJSONLines(ctx, cancellable, func(int, json.RawMessage) error {
		seen++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("mid-stream cancellation = %v after %d records", err, seen)
	}
}

func TestReadClaudeProviderTranscript(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "claude-conversation"
	path := filepath.Join(root, "projects", "encoded-worktree", conversationID+".jsonl")
	identity := map[string]any{"sessionId": conversationID, "cwd": workingDirectory, "isSidechain": false}
	writeTranscriptFixture(t, path, []any{
		mergeFixture(identity, map[string]any{"type": "user", "uuid": "user-1", "timestamp": "2026-08-16T02:00:00Z", "promptSource": "typed", "origin": map[string]any{"kind": "human"}, "message": map[string]any{"role": "user", "content": "hello"}}),
		mergeFixture(identity, map[string]any{"type": "attachment", "uuid": "attachment-1", "parentUuid": "user-1", "timestamp": "2026-08-16T02:00:00Z", "attachment": map[string]any{"content": "SECRET_ATTACHMENT"}}),
		mergeFixture(identity, map[string]any{"type": "assistant", "uuid": "assistant-1", "parentUuid": "attachment-1", "timestamp": "2026-08-16T02:00:01Z", "message": map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{
			map[string]any{"type": "thinking", "thinking": "safe thought", "signature": "SECRET_SIGNATURE"},
			map[string]any{"type": "tool_use", "name": "SECRET_INTERNAL_TOOL", "input": map[string]any{"token": "SECRET_TOKEN"}},
		}}}),
		mergeFixture(identity, map[string]any{"type": "user", "uuid": "tool-result-1", "parentUuid": "assistant-1", "timestamp": "2026-08-16T02:00:02Z", "message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "SECRET_TOOL_OUTPUT"},
		}}}),
		mergeFixture(identity, map[string]any{"type": "assistant", "uuid": "assistant-2", "parentUuid": "tool-result-1", "timestamp": "2026-08-16T02:00:03Z", "message": map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{
			map[string]any{"type": "text", "text": "done"},
		}}}),
		mergeFixture(identity, map[string]any{"type": "user", "uuid": "meta-1", "isMeta": true, "timestamp": "2026-08-16T02:00:04Z", "message": map[string]any{"role": "user", "content": "SECRET_META"}}),
		mergeFixture(identity, map[string]any{"type": "assistant", "uuid": "assistant-meta", "parentUuid": "meta-1", "timestamp": "2026-08-16T02:00:05Z", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "SECRET_META_RESPONSE"}}}}),
	}, "")

	profile := agents.Profile{Name: "claude", Environment: map[string]string{"CLAUDE_CONFIG_DIR": root}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("read Claude transcript: %v", err)
	}
	assertTranscript(t, snapshot, []TranscriptKind{TranscriptUser, TranscriptReasoning, TranscriptAssistant}, []string{"hello", "safe thought", "done"})
	if snapshot.Records[2].Phase != "final" {
		t.Fatalf("completed Claude assistant phase = %q", snapshot.Records[2].Phase)
	}
	assertNoTranscriptSecret(t, snapshot, "SECRET_ATTACHMENT", "SECRET_SIGNATURE", "SECRET_INTERNAL_TOOL", "SECRET_TOKEN", "SECRET_TOOL_OUTPUT", "SECRET_META", "SECRET_META_RESPONSE")
}

func TestReadGrokProviderTranscript(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "grok-conversation"
	group := filepath.Join(root, "sessions", "encoded-worktree")
	path := filepath.Join(group, conversationID, "chat_history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, ".cwd"), []byte(workingDirectory), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTranscriptFixture(t, path, []any{
		map[string]any{"type": "system", "content": "SECRET_SYSTEM"},
		map[string]any{"type": "user", "synthetic_reason": "system_reminder", "content": []any{map[string]any{"type": "text", "text": "SECRET_REMINDER"}}},
		map[string]any{"type": "user", "prompt_index": 0, "content": []any{map[string]any{"type": "text", "text": "<user_query>\nhello\n</user_query>"}}},
		map[string]any{"type": "reasoning", "id": "reason-1", "summary": []any{map[string]any{"type": "summary_text", "text": "safe thought"}}, "encrypted_content": "SECRET_ENCRYPTED"},
		map[string]any{"type": "assistant", "content": "done", "model_fingerprint": "SECRET_FINGERPRINT"},
	}, `{"type":"assistant"`)

	profile := agents.Profile{Name: "grok", Environment: map[string]string{"GROK_HOME": root}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("read Grok transcript: %v", err)
	}
	assertTranscript(t, snapshot, []TranscriptKind{TranscriptUser, TranscriptReasoning, TranscriptAssistant}, []string{"hello", "safe thought", "done"})
	if snapshot.Records[2].Phase != "final" {
		t.Fatalf("completed Grok assistant phase = %q", snapshot.Records[2].Phase)
	}
	assertNoTranscriptSecret(t, snapshot, "SECRET_SYSTEM", "SECRET_REMINDER", "SECRET_ENCRYPTED", "SECRET_FINGERPRINT")
}

func TestReadOpenCodeProviderTranscript(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "opencode-conversation"
	databasePath := filepath.Join(root, "opencode.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session (id, directory) VALUES ('opencode-conversation', '` + strings.ReplaceAll(workingDirectory, "'", "''") + `')`,
		`INSERT INTO message VALUES ('user-1', 'opencode-conversation', 1000, '{"role":"user","SECRET_AUTH":"hidden"}')`,
		`INSERT INTO message VALUES ('assistant-1', 'opencode-conversation', 2000, '{"role":"assistant","time":{"completed":2100},"finish":"stop","SECRET_AUTH":"hidden"}')`,
		`INSERT INTO part VALUES ('part-user', 'user-1', 'opencode-conversation', 1001, '{"type":"text","text":"hello"}')`,
		`INSERT INTO part VALUES ('part-reason', 'assistant-1', 'opencode-conversation', 2001, '{"type":"reasoning","text":"safe thought","encrypted":"SECRET_ENCRYPTED"}')`,
		`INSERT INTO part VALUES ('part-tool', 'assistant-1', 'opencode-conversation', 2002, '{"type":"tool","text":"SECRET_TOOL_OUTPUT"}')`,
		`INSERT INTO part VALUES ('part-answer', 'assistant-1', 'opencode-conversation', 2003, '{"type":"text","text":"done"}')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			t.Fatalf("OpenCode fixture: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	profile := agents.Profile{Name: "opencode", Environment: map[string]string{"OPENCODE_DB": databasePath}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("read OpenCode transcript: %v", err)
	}
	assertTranscript(t, snapshot, []TranscriptKind{TranscriptUser, TranscriptReasoning, TranscriptAssistant}, []string{"hello", "safe thought", "done"})
	if snapshot.Records[1].Phase != "final" || snapshot.Records[2].Phase != "final" {
		t.Fatalf("completed OpenCode assistant phases = %q, %q", snapshot.Records[1].Phase, snapshot.Records[2].Phase)
	}
	assertNoTranscriptSecret(t, snapshot, "SECRET_AUTH", "SECRET_ENCRYPTED", "SECRET_TOOL_OUTPUT")
}

func TestReadOpenCodeProviderTranscriptCoalescesUserTextPartsByMessage(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "opencode-multipart-user"
	databasePath := filepath.Join(root, "opencode.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session (id, directory) VALUES ('opencode-multipart-user', '` + strings.ReplaceAll(workingDirectory, "'", "''") + `')`,
		`INSERT INTO message VALUES ('user-message', 'opencode-multipart-user', 1000, '{"role":"user"}')`,
		`INSERT INTO message VALUES ('assistant-message', 'opencode-multipart-user', 2000, '{"role":"assistant","time":{"completed":2100},"finish":"stop"}')`,
		`INSERT INTO part VALUES ('user-text-z', 'user-message', 'opencode-multipart-user', 1001, '{"type":"text","text":"first fragment"}')`,
		`INSERT INTO part VALUES ('user-reasoning', 'user-message', 'opencode-multipart-user', 1002, '{"type":"reasoning","text":"SECRET_USER_REASONING"}')`,
		`INSERT INTO part VALUES ('user-text-a', 'user-message', 'opencode-multipart-user', 1003, '{"type":"text","text":"second fragment"}')`,
		`INSERT INTO part VALUES ('assistant-text', 'assistant-message', 'opencode-multipart-user', 2001, '{"type":"text","text":"complete answer"}')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			t.Fatalf("OpenCode multipart fixture: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	profile := agents.Profile{Name: "opencode", Environment: map[string]string{"OPENCODE_DB": databasePath}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatalf("read OpenCode multipart transcript: %v", err)
	}
	assertTranscript(t, snapshot,
		[]TranscriptKind{TranscriptUser, TranscriptAssistant},
		[]string{"first fragment\nsecond fragment", "complete answer"},
	)
	if snapshot.Truncated || snapshot.Records[0].Truncated {
		t.Fatalf("short multipart user message was truncated: %#v", snapshot)
	}
	wantKey := opaqueTranscriptKey("opencode", conversationID, string(TranscriptUser), "user-message")
	if snapshot.Records[0].Key != wantKey || !snapshot.Records[0].Timestamp.Equal(time.UnixMilli(1000).UTC()) {
		t.Fatalf("OpenCode multipart user identity/order = %#v, want key %q at message time", snapshot.Records[0], wantKey)
	}
	assertNoTranscriptSecret(t, snapshot, "SECRET_USER_REASONING")

	database, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO part VALUES ('user-text-new', 'user-message', 'opencode-multipart-user', 1004, '{"type":"text","text":"third fragment"}')`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Records[0].Key != snapshot.Records[0].Key || reloaded.Records[0].Text != "first fragment\nsecond fragment\nthird fragment" || reloaded.Checkpoint == snapshot.Checkpoint {
		t.Fatalf("OpenCode multipart user update lost its stable identity or content revision: %#v / %#v", snapshot, reloaded)
	}
}

func TestOpenCodeUserMessageCoalescingPreservesTextTruncation(t *testing.T) {
	message := &openCodeUserMessage{messageID: "bounded-user", created: 1000}
	message.appendText(strings.Repeat("a", maxProviderTranscriptTextBytes), false)
	message.appendText("must be truncated", false)
	builder := newTranscriptBuilder("opencode", "bounded-conversation")
	message.addTo(builder)
	snapshot := builder.snapshot()
	if len(snapshot.Records) != 1 || len(snapshot.Records[0].Text) != maxProviderTranscriptTextBytes ||
		!snapshot.Records[0].Truncated || !snapshot.Truncated {
		t.Fatalf("bounded OpenCode multipart user record = %#v", snapshot)
	}
}

func TestReadOpenCodeProviderTranscriptMarksActiveAssistantCommentary(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "opencode-active-conversation"
	databasePath := filepath.Join(root, "opencode.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session (id, directory) VALUES ('opencode-active-conversation', '` + strings.ReplaceAll(workingDirectory, "'", "''") + `')`,
		`INSERT INTO message VALUES ('user-1', 'opencode-active-conversation', 1000, '{"role":"user"}')`,
		`INSERT INTO message VALUES ('assistant-1', 'opencode-active-conversation', 2000, '{"role":"assistant","time":{"completed":2100},"finish":"stop"}')`,
		`INSERT INTO message VALUES ('user-2', 'opencode-active-conversation', 3000, '{"role":"user"}')`,
		`INSERT INTO message VALUES ('assistant-2', 'opencode-active-conversation', 4000, '{"role":"assistant"}')`,
		`INSERT INTO part VALUES ('user-part-1', 'user-1', 'opencode-active-conversation', 1001, '{"type":"text","text":"finished prompt"}')`,
		`INSERT INTO part VALUES ('answer-part-1', 'assistant-1', 'opencode-active-conversation', 2001, '{"type":"text","text":"finished answer"}')`,
		`INSERT INTO part VALUES ('user-part-2', 'user-2', 'opencode-active-conversation', 3001, '{"type":"text","text":"active prompt"}')`,
		`INSERT INTO part VALUES ('reason-part-2', 'assistant-2', 'opencode-active-conversation', 4001, '{"type":"reasoning","text":"partial reasoning"}')`,
		`INSERT INTO part VALUES ('answer-part-2', 'assistant-2', 'opencode-active-conversation', 4002, '{"type":"text","text":"partial answer"}')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			t.Fatalf("OpenCode active fixture: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	profile := agents.Profile{Name: "opencode", Environment: map[string]string{"OPENCODE_DB": databasePath}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	assertTranscript(t, snapshot,
		[]TranscriptKind{TranscriptUser, TranscriptAssistant, TranscriptUser, TranscriptReasoning, TranscriptAssistant},
		[]string{"finished prompt", "finished answer", "active prompt", "partial reasoning", "partial answer"},
	)
	if snapshot.Records[1].Phase != "final" || snapshot.Records[3].Phase != "commentary" || snapshot.Records[4].Phase != "commentary" {
		t.Fatalf("OpenCode completed/active phases = %q, %q, %q", snapshot.Records[1].Phase, snapshot.Records[3].Phase, snapshot.Records[4].Phase)
	}
}

func TestReadOpenCodeProviderTranscriptRetainsNewestBoundedHistory(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "opencode-long-conversation"
	databasePath := filepath.Join(root, "opencode.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session (id, directory) VALUES ('opencode-long-conversation', '` + strings.ReplaceAll(workingDirectory, "'", "''") + `')`,
		`INSERT INTO message VALUES ('assistant', 'opencode-long-conversation', 1, '{"role":"assistant","time":{"completed":2},"finish":"stop"}')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	transaction, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert, err := transaction.Prepare(`INSERT INTO part VALUES (?, 'assistant', ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxProviderTranscriptRecords+2; index++ {
		id := fmt.Sprintf("part-%04d", index)
		payload, _ := json.Marshal(map[string]string{"type": "text", "text": fmt.Sprintf("answer-%04d", index)})
		if _, err := insert.Exec(id, conversationID, index+1, string(payload)); err != nil {
			insert.Close()
			transaction.Rollback()
			t.Fatal(err)
		}
	}
	if err := insert.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}

	profile := agents.Profile{Name: "opencode", Environment: map[string]string{"OPENCODE_DB": databasePath}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != maxProviderTranscriptRecords || !snapshot.Truncated {
		t.Fatalf("bounded OpenCode history = %d records, truncated %v", len(snapshot.Records), snapshot.Truncated)
	}
	if first, last := snapshot.Records[0].Text, snapshot.Records[len(snapshot.Records)-1].Text; first != "answer-0002" || last != "answer-2049" {
		t.Fatalf("OpenCode retained range = %q…%q, want newest answer-0002…answer-2049", first, last)
	}
}

func TestReadVerifiedProviderTranscriptRejectsPathDrift(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "codex-drift"
	path := filepath.Join(root, "sessions", conversationID+".jsonl")
	writeTranscriptFixture(t, path, []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": conversationID, "cwd": workingDirectory}}}, "")
	other := filepath.Join(root, "other.jsonl")
	if err := os.WriteFile(other, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := agents.Profile{Name: "codex", Environment: map[string]string{"CODEX_HOME": root}}
	_, err := ReadVerifiedProviderTranscript(context.Background(), profile, Verification{ConversationID: conversationID, StorePath: other}, workingDirectory)
	if err == nil || !strings.Contains(err.Error(), "mismatched store path") {
		t.Fatalf("path drift error = %v", err)
	}
}

func TestProviderTranscriptBoundsAndMalformedInput(t *testing.T) {
	t.Parallel()
	root, workingDirectory := t.TempDir(), t.TempDir()
	conversationID := "codex-bounds"
	path := filepath.Join(root, "sessions", conversationID+".jsonl")
	records := []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": conversationID, "cwd": workingDirectory}}}
	for index := 0; index < maxProviderTranscriptRecords+2; index++ {
		text := fmt.Sprintf("message-%04d", index)
		if index == maxProviderTranscriptRecords+1 {
			text += strings.Repeat("x", maxProviderTranscriptTextBytes)
		}
		records = append(records, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": text}})
	}
	writeTranscriptFixture(t, path, records, "")
	profile := agents.Profile{Name: "codex", Environment: map[string]string{"CODEX_HOME": root}}
	snapshot, err := ReadProviderTranscript(context.Background(), profile, conversationID, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) > maxProviderTranscriptRecords || !snapshot.Truncated || !snapshot.Records[len(snapshot.Records)-1].Truncated {
		t.Fatalf("bounded snapshot = records %d, truncated %v, last %#v", len(snapshot.Records), snapshot.Truncated, snapshot.Records[len(snapshot.Records)-1])
	}
	if !strings.HasPrefix(snapshot.Records[len(snapshot.Records)-1].Text, "message-2049") {
		t.Fatalf("latest history was not retained: %.40q", snapshot.Records[len(snapshot.Records)-1].Text)
	}

	aggregateID := "codex-aggregate"
	aggregatePath := filepath.Join(root, "sessions", aggregateID+".jsonl")
	aggregateRecords := []any{map[string]any{"type": "session_meta", "payload": map[string]any{"id": aggregateID, "cwd": workingDirectory}}}
	for index := 0; index < 40; index++ {
		aggregateRecords = append(aggregateRecords, map[string]any{
			"type": "event_msg", "payload": map[string]any{
				"type": "user_message", "message": fmt.Sprintf("aggregate-%02d-", index) + strings.Repeat("x", maxProviderTranscriptTextBytes-16),
			},
		})
	}
	writeTranscriptFixture(t, aggregatePath, aggregateRecords, "")
	aggregate, err := ReadProviderTranscript(context.Background(), profile, aggregateID, workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	retained := 0
	for _, record := range aggregate.Records {
		retained += transcriptRecordBytes(record)
	}
	if retained > maxProviderTranscriptAggregateBytes || len(aggregate.Records) >= 40 || !aggregate.Truncated || !strings.HasPrefix(aggregate.Records[len(aggregate.Records)-1].Text, "aggregate-39-") {
		t.Fatalf("aggregate bound = bytes %d, records %d, truncated %v", retained, len(aggregate.Records), aggregate.Truncated)
	}

	malformedID := "codex-malformed"
	malformedPath := filepath.Join(root, "sessions", malformedID+".jsonl")
	identity, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": malformedID, "cwd": workingDirectory}})
	if err := os.WriteFile(malformedPath, append(append(identity, '\n'), []byte("{bad}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProviderTranscript(context.Background(), profile, malformedID, workingDirectory); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("malformed interior error = %v", err)
	}
}

func TestTranscriptBuilderCompactsEvictedBackingStorage(t *testing.T) {
	t.Parallel()
	builder := newTranscriptBuilder("codex", "dense-conversation")
	total := maxProviderTranscriptRecords*8 + 17
	for index := 0; index < total; index++ {
		text := fmt.Sprintf("answer-%05d", index)
		builder.add(TranscriptAssistant, text, text, time.Time{}, "final", text)
	}
	if retained := len(builder.records) - builder.start; retained != maxProviderTranscriptRecords {
		t.Fatalf("retained builder records = %d, want %d", retained, maxProviderTranscriptRecords)
	}
	if len(builder.records) > maxProviderTranscriptRecords*2 || cap(builder.records) > maxProviderTranscriptRecords*2 {
		t.Fatalf("builder backing storage = len %d cap %d, want at most %d", len(builder.records), cap(builder.records), maxProviderTranscriptRecords*2)
	}
	snapshot := builder.snapshot()
	wantFirst := fmt.Sprintf("answer-%05d", total-maxProviderTranscriptRecords)
	wantLast := fmt.Sprintf("answer-%05d", total-1)
	if !snapshot.Truncated || len(snapshot.Records) != maxProviderTranscriptRecords || snapshot.Records[0].Text != wantFirst || snapshot.Records[len(snapshot.Records)-1].Text != wantLast {
		t.Fatalf("compacted snapshot = truncated %v, records %d, range %q…%q; want %q…%q",
			snapshot.Truncated, len(snapshot.Records), snapshot.Records[0].Text, snapshot.Records[len(snapshot.Records)-1].Text, wantFirst, wantLast)
	}
}

func TestProviderTranscriptHonorsCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReadProviderTranscript(ctx, agents.Profile{Name: "codex"}, "conversation", t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v", err)
	}
}

func writeTranscriptFixture(t *testing.T, path string, records []any, trailing string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(encoded)
		body.WriteByte('\n')
	}
	body.WriteString(trailing)
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mergeFixture(left, right map[string]any) map[string]any {
	result := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] = value
	}
	return result
}

func assertTranscript(t *testing.T, snapshot TranscriptSnapshot, kinds []TranscriptKind, texts []string) {
	t.Helper()
	if snapshot.Checkpoint == "" || len(snapshot.Records) != len(kinds) || len(kinds) != len(texts) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	seenKeys := make(map[string]bool)
	for index, record := range snapshot.Records {
		if record.Kind != kinds[index] || record.Text != texts[index] || record.Key == "" || seenKeys[record.Key] {
			t.Fatalf("record %d = %#v; want kind %q text %q and unique key", index, record, kinds[index], texts[index])
		}
		seenKeys[record.Key] = true
	}
}

func assertNoTranscriptSecret(t *testing.T, snapshot TranscriptSnapshot, secrets ...string) {
	t.Helper()
	display := fmt.Sprintf("%#v", snapshot)
	for _, secret := range secrets {
		if strings.Contains(display, secret) {
			t.Fatalf("provider-only field %q leaked into snapshot", secret)
		}
	}
}
