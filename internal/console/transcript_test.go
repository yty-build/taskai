package console

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestTranscriptProjectsV6SessionChannelFramesImmediately(t *testing.T) {
	projection := newTranscript()
	lines := []sessionlog.Line{
		testLine(1, "REQ-1", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "Where am I?"}),
		testLine(2, "REQ-1", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "reasoning", "text": "Checking the working directory."}),
		testLine(3, "REQ-1", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": "You are in "}),
		testLine(4, "REQ-1", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": "/repo."}),
		testLine(5, "REQ-1", worker.FrameProviderResult, map[string]any{"status": "completed", "stop_reason": "end_turn"}),
	}
	for _, line := range lines {
		if !projection.apply(line) {
			t.Fatalf("frame %d was not applied", line.Seq)
		}
	}
	if len(projection.requests) != 1 {
		t.Fatalf("requests = %d", len(projection.requests))
	}
	request := projection.requests[0]
	if request.state != turnComplete || request.stop != "end_turn" {
		t.Fatalf("terminal state = %q, stop = %q", request.state, request.stop)
	}
	if len(request.blocks) != 3 {
		t.Fatalf("blocks = %#v", request.blocks)
	}
	if request.blocks[0].kind != blockUser || request.blocks[0].text != "Where am I?" {
		t.Fatalf("user block = %#v", request.blocks[0])
	}
	if request.blocks[1].kind != blockThought || request.blocks[1].text != "Checking the working directory." {
		t.Fatalf("thought block = %#v", request.blocks[1])
	}
	if request.blocks[2].kind != blockAssistant || request.blocks[2].text != "You are in /repo." {
		t.Fatalf("assistant block = %#v", request.blocks[2])
	}
}

func TestTranscriptProjectsToolAndPermissionFrames(t *testing.T) {
	projection := newTranscript()
	for _, line := range []sessionlog.Line{
		testLine(1, "REQ-2", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "Edit it"}),
		testLine(2, "REQ-2", worker.FrameProviderEvent, map[string]any{
			"type": "tool_call", "tool_call": map[string]any{
				"id": "tool-1", "title": "Read file", "kind": "read", "status": "completed",
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"text": "safe preview"}}},
			},
		}),
		testLine(3, "REQ-2", worker.FramePermissionRequested, worker.PendingPermission{
			ID: "permission-1", RequestID: "REQ-2", Title: "Write file", Kind: "edit",
			Options: []worker.PermissionOption{{ID: "allow", Name: "Allow once", Kind: "allow_once"}},
		}),
	} {
		projection.apply(line)
	}
	request := projection.requests[0]
	if tool := request.tools["tool-1"]; tool == nil || tool.title != "Read file" || tool.preview != "safe preview" {
		t.Fatalf("tool = %#v", tool)
	}
	_, permission := projection.pendingPermission()
	if permission == nil || permission.id != "permission-1" || len(permission.options) != 1 {
		t.Fatalf("permission = %#v", permission)
	}
	projection.apply(testLine(4, "REQ-2", worker.FramePermissionResolved, map[string]any{
		"permission_id": "permission-1", "decision": "allow",
	}))
	if _, pending := projection.pendingPermission(); pending != nil {
		t.Fatalf("resolved permission remained pending: %#v", pending)
	}
}

func TestTranscriptPermissionCompactionKeepsEveryWaitingCardActionable(t *testing.T) {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ-pending", worker.FrameClientCommand, map[string]any{
		"command": "prompt", "text": strings.Repeat("history", 4096),
	}))
	projection.apply(testLine(2, "REQ-pending", worker.FrameProviderEvent, map[string]any{
		"type": "tool_call", "tool_call": map[string]any{
			"id": "tool-history", "title": "Large history", "kind": "read", "status": "completed",
			"content": strings.Repeat("x", maxToolDetailBytes),
		},
	}))
	want := []worker.PendingPermission{
		{
			ID: "permission-one  exact", RequestID: "REQ-pending", Title: "First", Kind: "edit",
			Options: []worker.PermissionOption{
				{ID: "allow-one  exact", Name: "Allow once", Kind: "allow_once"},
				{ID: "reject-one-exact", Name: "Reject once", Kind: "reject_once"},
			},
		},
		{
			ID: "permission-two-exact", RequestID: "REQ-pending", Title: "Second", Kind: "execute",
			Options: []worker.PermissionOption{
				{ID: "allow-two-exact", Name: "Allow once", Kind: "allow_once"},
				{ID: "reject-two-exact", Name: "Reject once", Kind: "reject_once"},
			},
		},
		{
			ID: "permission-three-exact", RequestID: "REQ-pending", Title: "Third", Kind: "network",
			Options: []worker.PermissionOption{
				{ID: "allow-three-exact", Name: "Allow once", Kind: "allow_once"},
				{ID: "reject-three-exact", Name: "Reject once", Kind: "reject_once"},
			},
		},
	}
	sequence := uint64(3)
	for _, pending := range want {
		projection.apply(testLine(sequence, pending.RequestID, worker.FramePermissionRequested, pending))
		sequence++
	}
	if !projection.compactToPendingPermissions() {
		t.Fatal("permission-bearing transcript was not compacted")
	}
	request := projection.requests[0]
	if len(projection.requests) != 1 || len(request.blocks) != len(want) || len(request.permission) != len(want) || len(request.tools) != 0 {
		t.Fatalf("compacted transcript retained requests=%d blocks=%d permissions=%d tools=%d", len(projection.requests), len(request.blocks), len(request.permission), len(request.tools))
	}
	for index := len(want) - 1; index >= 0; index-- {
		pendingRequest, card := projection.pendingPermission()
		if pendingRequest != request || card == nil || card.id != want[index].ID || len(card.options) != len(want[index].Options) {
			t.Fatalf("pending card %d = request %#v card %#v", index, pendingRequest, card)
		}
		for optionIndex, option := range card.options {
			if option.id != want[index].Options[optionIndex].ID {
				t.Fatalf("pending card %d option %d = %q, want %q", index, optionIndex, option.id, want[index].Options[optionIndex].ID)
			}
		}
		request.resolvePermission(card.id, "allow_once")
	}
	if _, card := projection.pendingPermission(); card != nil {
		t.Fatalf("resolved compacted permissions still reported pending: %#v", card)
	}
	if projection.retainedUsage().bytes >= maxToolDetailBytes {
		t.Fatalf("compaction retained large non-permission history: %d bytes", projection.retainedUsage().bytes)
	}
}

func TestTranscriptProviderFailureIsTerminalAndBounded(t *testing.T) {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ-3", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "go"}))
	projection.apply(testLine(2, "REQ-3", worker.FrameProviderResult, map[string]any{
		"status": "failed", "code": "rate_limited", "message": "Try again shortly.\x1b[31m",
	}))
	request := projection.requests[0]
	if request.state != turnFailed || len(request.blocks) != 2 {
		t.Fatalf("failed request = %#v", request)
	}
	if strings.Contains(request.errorText, "\x1b") || request.errorText != "Try again shortly." {
		t.Fatalf("sanitized error = %q", request.errorText)
	}
}

func TestTranscriptDeduplicatesChannelSequenceAndBoundsRollingOutput(t *testing.T) {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ-4", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "stream"}))
	chunk := strings.Repeat("x", maxProjectedMessageBytes/2+100)
	line := testLine(2, "REQ-4", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": chunk})
	if !projection.apply(line) || projection.apply(line) {
		t.Fatal("channel sequence was not applied exactly once")
	}
	projection.apply(testLine(3, "REQ-4", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": chunk}))
	request := projection.requests[0]
	if request.assistantBytes > maxProjectedMessageBytes || !request.contentClipped {
		t.Fatalf("retained assistant bytes = %d, clipped = %v", request.assistantBytes, request.contentClipped)
	}
}

func TestTranscriptBoundsRequestAndSequenceState(t *testing.T) {
	projection := newTranscript()
	for sequence := 1; sequence <= maxProjectedRequests+37; sequence++ {
		requestID := fmt.Sprintf("REQ-%03d", sequence)
		projection.apply(testLine(uint64(sequence), requestID, worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "bounded"}))
	}
	if len(projection.requests) != maxProjectedRequests || len(projection.byID) != maxProjectedRequests || !projection.clipped {
		t.Fatalf("bounded projection = requests %d index %d clipped %v", len(projection.requests), len(projection.byID), projection.clipped)
	}
	if projection.apply(testLine(1, "REQ-old", worker.FrameConnection, map[string]any{"state": "connected"})) {
		t.Fatal("old duplicate frame was reapplied")
	}
}

func TestTranscriptBoundsProviderControlledMetadataPromptAndErrors(t *testing.T) {
	projection := newTranscript()
	requestID := strings.Repeat("r", maxProjectedIdentifierBytes+73)
	prompt := strings.Repeat("p", maxProjectedMessageBytes+73)
	promptLine := testLine(1, requestID, worker.FrameClientCommand, map[string]any{"command": "prompt", "text": prompt})
	originalPromptJSON := append([]byte(nil), promptLine.Data...)
	projection.apply(promptLine)

	toolID := strings.Repeat("t", maxProjectedIdentifierBytes+73)
	projection.apply(testLine(2, requestID, worker.FrameProviderEvent, map[string]any{
		"type": "tool_call", "tool_call": map[string]any{
			"id": toolID, "title": strings.Repeat("T", maxProjectedTitleBytes+73),
			"kind": strings.Repeat("k", maxProjectedKindBytes+73), "status": strings.Repeat("s", maxProjectedStatusBytes+73),
			"rawInput": map[string]any{"query": strings.Repeat("m", maxProjectedMetadataBytes+73)},
			"content":  strings.Repeat("d", maxToolDetailBytes+73),
		},
	}))

	permissionID := strings.Repeat("a", maxProjectedPermissionIDBytes+73)
	options := make([]worker.PermissionOption, 0, maxProjectedPermissionOptions+7)
	for index := 0; index < maxProjectedPermissionOptions+7; index++ {
		options = append(options, worker.PermissionOption{
			ID:   strings.Repeat("o", maxProjectedIdentifierBytes+73) + fmt.Sprint(index),
			Name: strings.Repeat("n", maxProjectedTitleBytes+73),
			Kind: strings.Repeat("q", maxProjectedKindBytes+73),
		})
	}
	projection.apply(testLine(3, requestID, worker.FramePermissionRequested, worker.PendingPermission{
		ID: permissionID, Title: strings.Repeat("A", maxProjectedTitleBytes+73),
		Kind: strings.Repeat("e", maxProjectedKindBytes+73), Options: options,
	}))
	projection.apply(testLine(4, requestID, worker.FramePermissionResolved, map[string]any{
		"permission_id": permissionID, "decision": strings.Repeat("c", maxProjectedDecisionBytes+73),
	}))

	sequence := uint64(5)
	for range maxProjectedErrorBlocks + 7 {
		projection.apply(testLine(sequence, requestID, worker.FrameProviderEvent, map[string]any{
			"type": "error", "message": strings.Repeat("E", maxProjectedErrorBytes+73),
		}))
		sequence++
	}
	projection.apply(testLine(sequence, requestID, worker.FrameProviderResult, map[string]any{
		"status": "failed", "stop_reason": strings.Repeat("z", maxProjectedStopBytes+73),
		"message": strings.Repeat("F", maxProjectedErrorBytes+73),
	}))

	if string(promptLine.Data) != string(originalPromptJSON) {
		t.Fatal("projection mutated the exact raw JSONL payload")
	}
	if len(projection.requests) != 1 || !projection.clipped {
		t.Fatalf("request projection = %d requests, clipped %v", len(projection.requests), projection.clipped)
	}
	request := projection.requests[0]
	if len(request.id) > maxProjectedIdentifierBytes || !request.contentClipped {
		t.Fatalf("request id/content bounds = %d bytes, clipped %v", len(request.id), request.contentClipped)
	}
	if len(request.stop) > maxProjectedStopBytes || len(request.errorText) > maxProjectedErrorBytes {
		t.Fatalf("terminal metadata = stop %d/error %d bytes", len(request.stop), len(request.errorText))
	}
	if len(request.blocks) > maxProjectedBlocks {
		t.Fatalf("retained blocks = %d, limit %d", len(request.blocks), maxProjectedBlocks)
	}
	var user *transcriptBlock
	errorBlocks := 0
	for _, block := range request.blocks {
		switch block.kind {
		case blockUser:
			user = block
		case blockError:
			errorBlocks++
			if len(block.text) > maxProjectedErrorBytes {
				t.Fatalf("error block retained %d bytes", len(block.text))
			}
		}
	}
	if user == nil || len(user.text) > maxProjectedMessageBytes || !user.truncated {
		t.Fatalf("bounded user block = %#v", user)
	}
	if errorBlocks != maxProjectedErrorBlocks {
		t.Fatalf("error blocks = %d, want capped %d", errorBlocks, maxProjectedErrorBlocks)
	}

	tool := request.tools[strings.Repeat("t", maxProjectedIdentifierBytes)]
	if tool == nil {
		t.Fatalf("bounded tool keys = %#v", request.tools)
	}
	if len(tool.id) > maxProjectedIdentifierBytes || len(tool.title) > maxProjectedTitleBytes ||
		len(tool.kind) > maxProjectedKindBytes || len(tool.status) > maxProjectedStatusBytes ||
		len(tool.metadata) > maxProjectedMetadataBytes || len(tool.detail) > maxToolDetailBytes || !tool.detailClipped {
		t.Fatalf("bounded tool = %#v", tool)
	}

	permission := request.permission[strings.Repeat("a", maxProjectedPermissionIDBytes)]
	if permission == nil {
		t.Fatalf("bounded permission keys = %#v", request.permission)
	}
	if permission.state != "resolved" || len(permission.id) > maxProjectedPermissionIDBytes ||
		len(permission.title) > maxProjectedTitleBytes || len(permission.kind) > maxProjectedKindBytes ||
		len(permission.decision) > maxProjectedDecisionBytes || len(permission.options) != maxProjectedPermissionOptions {
		t.Fatalf("bounded permission = %#v", permission)
	}
	for _, option := range permission.options {
		if len(option.id) > maxProjectedIdentifierBytes || len(option.name) > maxProjectedTitleBytes || len(option.kind) > maxProjectedKindBytes {
			t.Fatalf("unbounded permission option = %#v", option)
		}
	}
}

func TestTranscriptBoundsAlternatingProviderBlocks(t *testing.T) {
	projection := newTranscript()
	projection.apply(testLine(1, "REQ-blocks", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "stream"}))
	for index := 0; index < maxProjectedBlocks+73; index++ {
		projection.apply(testLine(uint64(index+2), "REQ-blocks", worker.FrameProviderEvent, map[string]any{
			"type": "text_delta", "stream": "output", "text": "x", "message_id": fmt.Sprintf("message-%d", index),
		}))
	}
	request := projection.requests[0]
	if len(request.blocks) != maxProjectedBlocks || !request.contentClipped {
		t.Fatalf("alternating blocks = %d, clipped %v", len(request.blocks), request.contentClipped)
	}
}

func TestConsoleSanitizersRemoveUnicodeFormatAndTerminalControls(t *testing.T) {
	input := "safe\x1b[2J\u009b31m\u202e\u2066\u200btext\r\nnext\tfield"
	display := sanitizeDisplay(input)
	for _, forbidden := range []rune{'\x1b', '\u009b', '\u202e', '\u2066', '\u200b', '\r'} {
		if strings.ContainsRune(display, forbidden) {
			t.Fatalf("sanitizeDisplay retained %U in %q", forbidden, display)
		}
	}
	if !strings.Contains(display, "safe") || !strings.Contains(display, "text\nnext\tfield") {
		t.Fatalf("sanitizeDisplay lost printable or layout text: %q", display)
	}
	inline := safeText(input)
	if strings.ContainsAny(inline, "\r\n\t\x1b") || strings.ContainsRune(inline, '\u202e') || strings.ContainsRune(inline, '\u200b') {
		t.Fatalf("safeText retained terminal/layout controls: %q", inline)
	}
}

func testLine(sequence uint64, requestID, lineType string, data any) sessionlog.Line {
	encoded, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return sessionlog.Line{Seq: sequence, RequestID: requestID, Type: lineType, Data: encoded}
}
