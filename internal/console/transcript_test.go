package console

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func TestTranscriptProjectsTurnsAndCoalescesRealShapedACP(t *testing.T) {
	projection := newTranscript()
	requestID := "REQ-1"
	lines := []sessionlog.Line{
		projectionLine(1, requestID, "prompt_submitted", map[string]any{"display_message": "First paragraph.\n\nSecond paragraph."}),
		projectionLine(2, requestID, "prompt_queued", map[string]any{"state": "ready"}),
		projectionLine(3, requestID, "turn_started", map[string]any{"request_id": requestID}),
		projectionACPLine(4, requestID, "initialize", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}),
		projectionACPLine(5, requestID, "usage_update", sessionUpdate(map[string]any{"sessionUpdate": "usage_update", "used": 15})),
		projectionACPLine(6, requestID, "agent_thought_chunk", sessionUpdate(messageUpdate("agent_thought_chunk", "check ", "thought-1"))),
		projectionACPLine(7, requestID, "agent_thought_chunk", sessionUpdate(messageUpdate("agent_thought_chunk", "constraints", "thought-1"))),
		projectionACPLine(8, requestID, "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "Hello, ", "message-1"))),
		projectionACPLine(9, requestID, "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "world.\n\nNext paragraph.", "message-1"))),
		projectionLine(10, requestID, "turn_completed", map[string]any{"stopReason": "end_turn"}),
	}
	for _, line := range lines {
		if !projection.apply(line) {
			t.Fatalf("line %d was not applied", line.Seq)
		}
	}
	if projection.apply(lines[8]) {
		t.Fatal("duplicate stream position was applied twice")
	}
	if len(projection.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(projection.requests))
	}
	request := projection.requests[0]
	if request.state != turnComplete {
		t.Fatalf("state = %q, want complete", request.state)
	}
	if len(request.blocks) != 3 {
		t.Fatalf("blocks = %#v, want user/thought/assistant", request.blocks)
	}
	if got := request.blocks[0].text; got != "First paragraph.\n\nSecond paragraph." {
		t.Fatalf("user message = %q", got)
	}
	if got := request.blocks[1].text; got != "check constraints" {
		t.Fatalf("thought = %q", got)
	}
	if got := request.blocks[2].text; got != "Hello, world.\n\nNext paragraph." {
		t.Fatalf("assistant = %q", got)
	}
	if projection.metadata.usage != "15" {
		t.Fatalf("usage metadata = %q", projection.metadata.usage)
	}
}

func TestTranscriptProjectsLegacySessionPromptWithoutTransportScaffolding(t *testing.T) {
	projection := newTranscript()
	prompt := "Task T-1: Claude review\nDescription: (none)\nSession: S-1\nRole: reviewer\nRepository: /repo\nWorktree: /worktree\nBranch: agent/review\nWork in the repository context at /worktree. This is one Session under the Task; preserve the Task/Session boundary.\n\nSession-specific instructions:\nReview the implementation and report any problems."
	projection.apply(projectionLine(1, "REQ-LEGACY", "prompt_queued", map[string]any{"state": "ready"}))
	projection.apply(projectionACPLine(2, "REQ-LEGACY", "session_prompt", map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{
			"sessionId": "native", "prompt": []any{map[string]any{"type": "text", "text": prompt}},
		},
	}))
	request := projection.requests[0]
	if len(request.blocks) != 1 || request.blocks[0].kind != blockUser {
		t.Fatalf("legacy prompt blocks = %#v", request.blocks)
	}
	want := "Start work on \"Claude review\" as reviewer.\n\nReview the implementation and report any problems."
	if got := request.blocks[0].text; got != want {
		t.Fatalf("legacy display message = %q, want %q", got, want)
	}
	if strings.Contains(request.blocks[0].text, "Repository:") || strings.Contains(request.blocks[0].text, "Worktree:") {
		t.Fatalf("legacy prompt leaked transport scaffolding: %q", request.blocks[0].text)
	}

	// A newer truthful prompt_submitted record replaces the legacy fallback.
	projection.apply(projectionLine(3, "REQ-LEGACY", "prompt_submitted", map[string]any{"display_message": "Exact user-facing message."}))
	if got := request.blocks[0].text; got != "Exact user-facing message." || request.blocks[0].legacy {
		t.Fatalf("truthful prompt did not replace fallback: %#v", request.blocks[0])
	}
}

func TestTranscriptMergesToolLifecycleAndBoundsLargeBodies(t *testing.T) {
	const sentinel = "SENTINEL-MUST-NOT-APPEAR-IN-DEFAULT-VIEW"
	line := strings.Repeat("package example // long source line\n", 760)
	body := line + sentinel
	projection := newTranscript()
	requestID := "REQ-TOOL"
	updates := []sessionlog.Line{
		projectionLine(1, requestID, "prompt_submitted", map[string]any{"display_message": "Review the implementation."}),
		projectionACPLine(2, requestID, "tool_call", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "Read", "kind": "read", "status": "pending",
		})),
		projectionACPLine(3, requestID, "tool_call_update", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "tool-1", "rawInput": map[string]any{"path": "/repo/internal/console/model.go"},
		})),
		projectionACPLine(4, requestID, "tool_call_update", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "tool-1",
			"_meta": map[string]any{"claudeCode": map[string]any{"toolResponse": map[string]any{
				"type": "text", "file": map[string]any{"filePath": "/repo/internal/console/model.go", "content": body, "numLines": 761},
			}}},
		})),
		projectionACPLine(5, requestID, "tool_call_update", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "tool-1", "status": "completed",
			"content":   []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": body}}},
			"rawOutput": body,
		})),
	}
	for _, item := range updates {
		projection.apply(item)
	}
	tools := projection.tools()
	if len(tools) != 1 {
		t.Fatalf("logical tools = %d, want 1", len(tools))
	}
	tool := tools[0]
	if tool.status != "completed" || tool.kind != "read" || tool.metadata != "model.go" {
		t.Fatalf("merged tool = %#v", tool)
	}
	if tool.outputBytes < 25_000 || tool.outputLines < 760 {
		t.Fatalf("tool size = %d bytes / %d lines", tool.outputBytes, tool.outputLines)
	}
	if tool.preview != "" || strings.Contains(tool.preview, sentinel) {
		t.Fatalf("large source leaked through preview: %q", tool.preview)
	}
	if !strings.Contains(tool.detail, sentinel) {
		t.Fatal("explicit bounded detail lost forensic content")
	}
	if len(tool.detail) > maxToolDetailBytes {
		t.Fatalf("detail is unbounded: %d bytes", len(tool.detail))
	}
	encoded, err := json.Marshal(updates[len(updates)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), sentinel) {
		t.Fatal("raw history no longer exposes complete forensic data")
	}
}

func TestTranscriptUpdatesPlanPermissionErrorsAndCancellation(t *testing.T) {
	projection := newTranscript()
	requestID := "REQ-STATE"
	lines := []sessionlog.Line{
		projectionLine(1, requestID, "prompt_submitted", map[string]any{"display_message": "Make a safe change."}),
		projectionLine(2, requestID, "turn_started", map[string]any{}),
		projectionACPLine(3, requestID, "plan", sessionUpdate(map[string]any{"sessionUpdate": "plan", "entries": []any{
			map[string]any{"content": "Inspect", "status": "in_progress"},
		}})),
		projectionACPLine(4, requestID, "plan_update", sessionUpdate(map[string]any{"sessionUpdate": "plan_update", "entries": []any{
			map[string]any{"content": "Inspect", "status": "completed"}, map[string]any{"content": "Edit", "status": "in_progress"},
		}})),
		projectionACPLine(5, requestID, "session_request_permission", map[string]any{
			"jsonrpc": "2.0", "id": 8, "method": "session/request_permission", "params": map[string]any{
				"sessionId": "provider-session", "toolCall": map[string]any{"toolCallId": "tool-write", "title": "Write config", "kind": "edit"},
				"options": []any{map[string]any{"optionId": "allow", "name": "Allow once", "kind": "allow_once"}, map[string]any{"optionId": "reject", "name": "Deny", "kind": "reject_once"}},
			},
		}),
		projectionLine(6, requestID, "permission_resolved", map[string]any{"permission_id": "8:tool-write", "decision": "deny"}),
		projectionLine(7, requestID, "cancel_requested", map[string]any{}),
		projectionACPLine(8, requestID, "prompt_result", map[string]any{"jsonrpc": "2.0", "id": 3, "result": map[string]any{"stopReason": "cancelled"}}),
		projectionLine(9, requestID, "turn_completed", map[string]any{"stopReason": "cancelled"}),
	}
	for _, line := range lines {
		projection.apply(line)
	}
	request := projection.requests[0]
	if request.state != turnCancelled {
		t.Fatalf("state = %q", request.state)
	}
	if request.plan == nil || len(request.plan.plan) != 2 || request.plan.plan[1].content != "Edit" {
		t.Fatalf("plan was not updated in place: %#v", request.plan)
	}
	if len(request.permission) != 1 {
		t.Fatalf("permissions = %#v", request.permission)
	}
	card := request.permission["8:tool-write"]
	if card == nil || card.state != "resolved" || card.decision != "deny" {
		t.Fatalf("permission = %#v", card)
	}
	if _, pending := projection.pendingPermission(); pending != nil {
		t.Fatalf("resolved permission remained pending: %#v", pending)
	}
}

func TestTranscriptSanitizesHostileTerminalSequences(t *testing.T) {
	projection := newTranscript()
	projection.apply(projectionLine(1, "REQ", "prompt_submitted", map[string]any{
		"display_message": "alpha\x1b]52;c;SECRET\a\x1b[31m red\x1b[0m\n\tindented\x00",
	}))
	got := projection.requests[0].blocks[0].text
	if strings.Contains(got, "\x1b") || strings.Contains(got, "SECRET") || strings.ContainsRune(got, '\x00') {
		t.Fatalf("hostile control sequence survived: %q", got)
	}
	if !strings.Contains(got, "\n\tindented") {
		t.Fatalf("newlines/tabs were not preserved: %q", got)
	}
}

func TestTranscriptAppliesTenThousandEventsWithinBudget(t *testing.T) {
	projection := newTranscript()
	started := time.Now()
	projection.apply(projectionLine(1, "REQ-PERF", "prompt_submitted", map[string]any{"display_message": "Stream a response."}))
	for sequence := uint64(2); sequence <= 10_001; sequence++ {
		projection.apply(projectionACPLine(sequence, "REQ-PERF", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "x", "message-perf"))))
	}
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("10k event projection took %s, budget is 2s", elapsed)
	}
	request := projection.requests[0]
	if len(request.blocks) != 2 || len(request.blocks[1].text) != 10_000 {
		t.Fatalf("stream did not coalesce: blocks=%d bytes=%d", len(request.blocks), len(request.blocks[1].text))
	}
}

func projectionLine(sequence uint64, requestID, lineType string, data any) sessionlog.Line {
	raw, _ := json.Marshal(data)
	return sessionlog.Line{Seq: sequence, Timestamp: time.Unix(int64(sequence), 0).UTC(), RequestID: requestID, Type: lineType, Data: raw}
}

func projectionACPLine(sequence uint64, requestID, eventType string, rawPayload map[string]any) sessionlog.Line {
	raw, _ := json.Marshal(rawPayload)
	event, _ := json.Marshal(acp.Event{EventVersion: 1, RequestID: requestID, Seq: sequence, Stream: "prompt", Type: eventType, Raw: raw})
	return sessionlog.Line{Seq: sequence, Timestamp: time.Unix(int64(sequence), 0).UTC(), RequestID: requestID, Type: "acp", Data: event}
}

func sessionUpdate(update map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "provider-session", "update": update}}
}

func messageUpdate(kind, text, messageID string) map[string]any {
	return map[string]any{
		"sessionUpdate": kind, "messageId": messageID,
		"content": map[string]any{"type": "text", "text": text},
	}
}

func BenchmarkTranscriptApplyTenThousandEvents(b *testing.B) {
	lines := make([]sessionlog.Line, 10_000)
	for index := range lines {
		lines[index] = projectionACPLine(uint64(index+1), "REQ-BENCH", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", fmt.Sprintf("%d", index%10), "message")))
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		projection := newTranscript()
		for _, line := range lines {
			projection.apply(line)
		}
	}
}
