package presentation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func TestProjectBuildsSemanticConversationActivity(t *testing.T) {
	tests := []struct {
		name  string
		line  sessionlog.Line
		check func(*testing.T, *Activity)
	}{
		{
			name: "truthful user prompt",
			line: sessionlog.Line{Seq: 1, RequestID: "REQ", Type: "prompt_submitted", Data: json.RawMessage(`{"display_message":"Review the change."}`)},
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "user_message" || got.Text != "Review the change." {
					t.Fatalf("projection = %#v", got)
				}
			},
		},
		{
			name: "legacy prompt fallback",
			line: acpLine(t, "session_prompt", `{"jsonrpc":"2.0","params":{"prompt":[{"type":"text","text":"Task T-1: Fix login\nDescription: Repair auth\nSession: S-secret\nRole: reviewer\nRepository: /private/repo\n\nSession-specific instructions:\nCheck the tests."}]}}`),
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "legacy_user_message" || !strings.Contains(got.Text, `Start work on "Fix login" as reviewer.`) || strings.Contains(got.Text, "S-secret") || strings.Contains(got.Text, "/private/repo") {
					t.Fatalf("projection = %#v", got)
				}
			},
		},
		{
			name: "assistant chunk",
			line: acpLine(t, "agent_message_chunk", `{"jsonrpc":"2.0","params":{"update":{"content":{"type":"text","text":"Hello\u001b[31m world"},"messageId":"m-1"}}}`),
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "assistant_chunk" || got.MessageID == "" || got.MessageID == "m-1" || got.Text != "Hello world" {
					t.Fatalf("projection = %#v", got)
				}
			},
		},
		{
			name: "reasoning chunk",
			line: acpLine(t, "agent_thought_chunk", `{"jsonrpc":"2.0","params":{"update":{"content":{"type":"text","text":"Inspect the request\u001b[31m carefully"}}}}`),
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "thought_chunk" || got.Text != "Inspect the request carefully" {
					t.Fatalf("projection = %#v", got)
				}
			},
		},
		{
			name: "tool update",
			line: acpLine(t, "tool_call_update", `{"jsonrpc":"2.0","params":{"update":{"toolCallId":"tool-1","title":"Run tests","kind":"execute","status":"completed","rawInput":{"path":"/private/repo/worker.go","command":"SECRET_COMMAND","secret":"must-not-appear"},"rawOutput":{"token":"RAW_SECRET"},"_meta":{"environment":{"TOKEN":"META_SECRET"}},"content":[{"type":"content","content":{"type":"text","text":"PASS"}},{"type":"unknown","secret":"UNKNOWN_SECRET"}]}}}`),
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "tool_upsert" || got.Tool == nil || got.Tool.ID == "tool-1" || got.Tool.Metadata != "worker.go" || got.Tool.Preview != "PASS" {
					t.Fatalf("projection = %#v", got)
				}
				encoded, _ := json.Marshal(got)
				for _, secret := range []string{"must-not-appear", "SECRET_COMMAND", "RAW_SECRET", "META_SECRET", "UNKNOWN_SECRET", "rawInput", "rawOutput", "_meta", "/private/repo"} {
					if strings.Contains(string(encoded), secret) {
						t.Fatalf("projection leaked %q: %s", secret, encoded)
					}
				}
			},
		},
		{
			name: "plan update",
			line: acpLine(t, "plan_update", `{"jsonrpc":"2.0","params":{"update":{"entries":[{"content":"Inspect code","status":"completed"},{"content":"Run tests","status":"in_progress"}]}}}`),
			check: func(t *testing.T, got *Activity) {
				if got == nil || got.Type != "plan_replace" || len(got.Plan) != 2 || got.Plan[1].Content != "Run tests" {
					t.Fatalf("projection = %#v", got)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { test.check(t, Project(test.line)) })
	}
}

func TestProjectBoundsLocationMetadataAndPlanEntries(t *testing.T) {
	locations := make([]map[string]string, 0, 20)
	for index := 0; index < 20; index++ {
		locations = append(locations, map[string]string{"path": "/private/repo/file-" + strings.Repeat("x", 140)})
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "params": map[string]any{"update": map[string]any{
		"toolCallId": "tool", "locations": locations,
	}}})
	tool := Project(acpLine(t, "tool_call", string(raw)))
	if tool == nil || tool.Tool == nil || len(tool.Tool.Metadata) > 512 || strings.Contains(tool.Tool.Metadata, "/private/repo") {
		t.Fatalf("bounded tool metadata = %#v", tool)
	}

	entries := make([]map[string]string, 0, 160)
	for index := 0; index < 160; index++ {
		entries = append(entries, map[string]string{"content": strings.Repeat("p", 2048), "status": strings.Repeat("s", 100)})
	}
	planRaw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "params": map[string]any{"update": map[string]any{"entries": entries}}})
	plan := Project(acpLine(t, "plan_update", string(planRaw)))
	if plan == nil || len(plan.Plan) > 128 {
		t.Fatalf("plan entry bound = %#v", plan)
	}
	total := 0
	for _, entry := range plan.Plan {
		total += len(entry.Content)
		if len(entry.Status) > 64 {
			t.Fatalf("plan status was not bounded: %d", len(entry.Status))
		}
	}
	if total > maxMessageBytes {
		t.Fatalf("plan text = %d bytes", total)
	}
}

func TestProjectOmitsDiagnosticOnlyACPActivity(t *testing.T) {
	line := acpLine(t, "usage_update", `{"jsonrpc":"2.0","params":{"update":{"used":42}}}`)
	if got := Project(line); got != nil {
		t.Fatalf("usage metadata appeared in primary transcript: %#v", got)
	}
}

func TestProjectRedactsProviderAndWorkerErrors(t *testing.T) {
	secret := `TOKEN=top-secret /Users/private/provider stderr`
	lines := []sessionlog.Line{
		{Type: "turn_error", Data: json.RawMessage(`{"message":"` + secret + `"}`)},
		{Type: "worker_error", Data: json.RawMessage(`{"message":"` + secret + `"}`)},
		acpLine(t, "error", `{"jsonrpc":"2.0","error":{"message":"`+secret+`"}}`),
	}
	for _, line := range lines {
		display := Project(line)
		if display == nil || display.Type != "error_upsert" || strings.Contains(display.Text, secret) || strings.Contains(display.Text, "top-secret") || strings.Contains(display.Text, "/Users/private") {
			t.Fatalf("unsafe error projection = %#v", display)
		}
		if summary := Summary(line, display); strings.Contains(summary, secret) || strings.Contains(summary, "top-secret") {
			t.Fatalf("unsafe error summary = %q", summary)
		}
	}
}

func TestProjectBoundsToolContentDuringProjection(t *testing.T) {
	large := strings.Repeat("tool-output-", 200000)
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"params": map[string]any{"update": map[string]any{
			"toolCallId": "large-tool", "title": "Large result", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": large}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	display := Project(acpLine(t, "tool_call_update", string(raw)))
	if display == nil || display.Tool == nil || len(display.Tool.Detail) > maxToolBytes || !display.Tool.DetailClipped {
		t.Fatalf("bounded large tool = %#v", display)
	}
}

func TestProjectAllowListsStopReason(t *testing.T) {
	secret := strings.Repeat("/private/provider/SECRET", 10000)
	display := Project(sessionlog.Line{Type: "turn_completed", Data: json.RawMessage(`{"stop_reason":"` + secret + `"}`)})
	if display == nil || display.StopReason != "other" || strings.Contains(display.StopReason, "SECRET") {
		t.Fatalf("unsafe stop reason = %#v", display)
	}
	cancelled := Project(sessionlog.Line{Type: "turn_completed", Data: json.RawMessage(`{"stop_reason":"cancelled"}`)})
	if cancelled == nil || cancelled.State != "cancelled" || cancelled.StopReason != "cancelled" {
		t.Fatalf("cancelled stop reason = %#v", cancelled)
	}
	american := Project(sessionlog.Line{Type: "turn_completed", Data: json.RawMessage(`{"stop_reason":"canceled"}`)})
	if american == nil || american.State != "cancelled" || american.StopReason != "cancelled" {
		t.Fatalf("canceled stop reason = %#v", american)
	}
}

func acpLine(t *testing.T, eventType, raw string) sessionlog.Line {
	t.Helper()
	event, err := json.Marshal(acp.Event{EventVersion: 1, RequestID: "REQ", Type: eventType, Raw: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return sessionlog.Line{Seq: 2, RequestID: "REQ", Type: "acp", Data: event}
}
