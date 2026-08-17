package console

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/presentation"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
)

const visualSentinel = "SENTINEL-FULL-SOURCE-MUST-STAY-OUT-OF-CONVERSATION"

func TestTaskConsoleVisualContract(t *testing.T) {
	model := visualTaskModel()
	for _, size := range []struct {
		width  int
		height int
		mode   taskWorkspaceMode
		wants  []string
	}{
		{width: 40, height: 16, mode: taskWorkspaceCompact, wants: []string{"TASK · Ship the native", "T-VISUAL", "SESSIONS", "CLAUDE", "Enter Native", "a View", "n New", "q Detach"}},
		{width: 60, height: 16, mode: taskWorkspaceStacked, wants: []string{"SESSIONS", "CONVERSATION · Claude · reviewer", "Enter Claude CLI", "Structured view"}},
		{width: 80, height: 24, mode: taskWorkspaceSplit, wants: []string{"SESSIONS", "CONVERSATION · Claude · reviewer", "Working now", "Reasoning", "Tool · Read worker state"}},
		{width: 120, height: 40, mode: taskWorkspaceSplit, wants: []string{"SESSIONS", "CONVERSATION · Claude · reviewer", "Conversation linked · verified on open", "Claude CLI", "Provider transcript confirms safe detach"}},
	} {
		t.Run(strings.Join([]string{itoa(size.width), "x", itoa(size.height)}, ""), func(t *testing.T) {
			model.width, model.height = size.width, size.height
			view := model.View()
			assertScreenSize(t, view, size.width, size.height)
			if got := layoutTaskWorkspace(size.width, size.height).Mode; got != size.mode {
				t.Fatalf("%dx%d workspace mode = %d, want %d", size.width, size.height, got, size.mode)
			}
			for _, want := range size.wants {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d Task view missing %q:\n%s", size.width, size.height, want, view)
				}
			}
			if strings.Contains(view, "╭─ AGENT TASK CONSOLE") || strings.Contains(view, "SESSION PREVIEW") {
				t.Fatalf("%dx%d Task view retained the old framed hierarchy:\n%s", size.width, size.height, view)
			}
			for _, line := range strings.Split(ansi.Strip(view), "\n") {
				if strings.Count(line, " │ ") > 1 {
					t.Fatalf("%dx%d Task view rendered a nested third pane: %q\n%s", size.width, size.height, line, view)
				}
			}
			for _, noise := range []string{"LIVE ACP", "PROVIDER COMMITTED", "PROVENANCE", "  CONTEXT"} {
				if strings.Contains(ansi.Strip(view), noise) {
					t.Fatalf("%dx%d Task view exposed implementation noise %q:\n%s", size.width, size.height, noise, view)
				}
			}
			footer := strings.Split(view, "\n")[size.height-1]
			if strings.Contains(footer, "…") || !strings.Contains(footer, "q Detach") {
				t.Fatalf("%dx%d footer lost a core action: %q", size.width, size.height, footer)
			}
		})
	}
}

func TestTaskConversationPreviewIsDenseBoundedAndHumanReadable(t *testing.T) {
	model := visualTaskModel()
	for _, size := range []struct {
		width, height int
		minimumFilled int
		wants         []string
	}{
		{
			width: 80, height: 24, minimumFilled: 16,
			wants: []string{"CONVERSATION", "◇ Reasoning", "▸ Tool · Read worker state", "● Claude", "● Agent is working"},
		},
		{
			width: 120, height: 36, minimumFilled: 22,
			wants: []string{"❯ You", "● Claude", "◇ Reasoning", "▸ Tool · Read worker state", "✓ Turn complete", "Conversation linked · verified on open"},
		},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			model.width, model.height = size.width, size.height
			assertScreenSize(t, model.View(), size.width, size.height)
			layout := layoutTaskWorkspace(size.width, size.height)
			preview := model.renderTaskPreview(layout.Preview.Width, layout.Preview.Height)
			if len(preview) != layout.Preview.Height {
				t.Fatalf("preview height = %d, want %d", len(preview), layout.Preview.Height)
			}
			joined := strings.Join(preview, "\n")
			for _, want := range size.wants {
				if !strings.Contains(joined, want) {
					t.Fatalf("%dx%d preview missing %q:\n%s", size.width, size.height, want, joined)
				}
			}
			filled := 0
			for _, line := range preview {
				if strings.TrimSpace(ansi.Strip(line)) != "" {
					filled++
				}
			}
			if filled < size.minimumFilled {
				t.Fatalf("%dx%d preview has only %d intentional lines, want at least %d:\n%s", size.width, size.height, filled, size.minimumFilled, joined)
			}
			for _, forbidden := range []string{"REQ-TASK", "jsonrpc", visualSentinel, "LIVE ACP", "PROVIDER COMMITTED", "PROVENANCE", "CONTEXT"} {
				if strings.Contains(joined, forbidden) {
					t.Fatalf("%dx%d preview leaked %q:\n%s", size.width, size.height, forbidden, joined)
				}
			}
		})
	}
}

func TestTaskConversationPreviewBoundsLongHistoryAndKeepsRecentContext(t *testing.T) {
	model := visualTaskModel()
	projection := newTranscript()
	for index := 0; index < taskPreviewRequestLimit+20; index++ {
		request := projection.request(fmt.Sprintf("request-%03d", index))
		request.setUserMessage(fmt.Sprintf("prompt-%03d", index), false)
		request.appendMessage(blockAssistant, fmt.Sprintf("answer-%03d", index), "answer", maxProjectedMessageBytes)
		request.state = turnComplete
	}
	latest := projection.requests[len(projection.requests)-1]
	latest.appendThought("latest bounded reasoning")
	latest.upsertTool(map[string]any{"toolCallId": "latest-tool", "title": "Latest bounded tool", "kind": "read", "status": "completed"})
	model.projections[model.task.Sessions[0].ID] = projection

	entries := taskPreviewEntries(projection, "CLAUDE")
	if len(entries) > taskPreviewEntryLimit {
		t.Fatalf("preview retained %d entries, limit is %d", len(entries), taskPreviewEntryLimit)
	}
	model.width, model.height = 120, 36
	layout := layoutTaskWorkspace(model.width, model.height)
	preview := strings.Join(model.renderTaskPreview(layout.Preview.Width, layout.Preview.Height), "\n")
	for _, want := range []string{"prompt-051", "answer-051", "latest bounded reasoning", "Latest bounded tool"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("bounded preview lost %q:\n%s", want, preview)
		}
	}
	if strings.Contains(preview, "prompt-000") || strings.Contains(preview, "answer-000") {
		t.Fatalf("bounded preview retained stale history:\n%s", preview)
	}
}

func TestTaskOverflowCueSurvivesLongSessionPaths(t *testing.T) {
	model := visualTaskModel()
	model.task.Sessions = nil
	for index := range 10 {
		model.task.Sessions = append(model.task.Sessions, session.Session{
			ID: fmt.Sprintf("S-LONG-%02d", index), TaskID: "T-VISUAL", Agent: "claude", Role: "reviewer",
			WorktreePath: "/worktrees/this-is-an-intentionally-long-checkout-name", Branch: "agent/long-responsive-console-review",
			Status: session.StatusWaiting,
		})
	}
	for _, size := range []struct {
		width, height int
		want          string
	}{{width: 60, height: 16, want: "more ↓"}, {width: 80, height: 24, want: "more ↓"}} {
		model.width, model.height = size.width, size.height
		view := model.View()
		assertScreenSize(t, view, size.width, size.height)
		if !strings.Contains(view, size.want) {
			t.Fatalf("%dx%d long Session rows hid %q:\n%s", size.width, size.height, size.want, view)
		}
	}
}

func TestStructuredACPVisualContract(t *testing.T) {
	model := visualSessionModel(t)
	for _, size := range []struct {
		width  int
		height int
		wants  []string
	}{
		{width: 40, height: 16, wants: []string{"SESSION S-VISUAL", "MESSAGE CLAUDE", "p Write", "q Detach"}},
		{width: 60, height: 16, wants: []string{"SESSION S-VISUAL", "CLAUDE · reviewer", "23.1 KB", "MESSAGE CLAUDE"}},
		{width: 80, height: 24, wants: []string{"CONVERSATION · SESSION S-VISUAL", "CLAUDE · reviewer", "TOOL · model.go", "YOU", "I found two issues", "CLAUDE · update"}},
		{width: 120, height: 40, wants: []string{"CONVERSATION · SESSION S-VISUAL", "CLAUDE · reviewer", "TOOL · model.go", "YOU", "I found two issues", "CLAUDE · update"}},
	} {
		t.Run(strings.Join([]string{itoa(size.width), "x", itoa(size.height)}, ""), func(t *testing.T) {
			model.width, model.height = size.width, size.height
			view := model.View()
			assertScreenSize(t, view, size.width, size.height)
			for _, want := range size.wants {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d view missing %q:\n%s", size.width, size.height, want, view)
				}
			}
			for _, forbidden := range []string{visualSentinel, "jsonrpc", "REQ-VISUAL", "toolCallId", "package example"} {
				if strings.Contains(view, forbidden) {
					t.Fatalf("%dx%d normal view leaked %q:\n%s", size.width, size.height, forbidden, view)
				}
			}
		})
	}
}

func TestStructuredACPCompletedResponseAndReasoningDisclosure(t *testing.T) {
	projection := newTranscript()
	projection.apply(projectionLine(1, "REQ-COMPLETE", "prompt_submitted", map[string]any{"display_message": "Give me the result."}))
	projection.apply(projectionLine(2, "REQ-COMPLETE", "turn_started", map[string]any{}))
	projection.apply(projectionACPLine(3, "REQ-COMPLETE", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "The verified result is 42.", "response"))))
	projection.apply(projectionLine(4, "REQ-COMPLETE", "turn_completed", map[string]any{"stopReason": "end_turn"}))
	model := visualSessionModel(t)
	model.projections[model.current.ID] = projection
	model.width, model.height = 80, 24
	view := model.View()
	for _, want := range []string{
		"✓ RESPONSE · CLAUDE · complete",
		"The verified result is 42.",
		"No separate reasoning summary was available",
		"✓ Turn complete",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("completed turn missing %q:\n%s", want, view)
		}
	}
}

func TestTaskPreviewMarksCleanGatewayPromptAsPartOfCommittedTurn(t *testing.T) {
	projection := transcriptFromConversation(appservice.ConversationSnapshot{Turns: []appservice.ConversationTurn{{
		Key: "matched-provider-turn", Committed: true,
		Activities: []appservice.ConversationActivity{
			{Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: "Clean user-facing prompt"}},
			{Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "ACP-only reasoning"}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Committed answer"}},
			{Kind: "turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
		},
	}}})
	entries := taskPreviewEntries(projection, "CODEX")
	var user, thought taskPreviewEntry
	for _, entry := range entries {
		switch entry.kind {
		case "user":
			user = entry
		case "reasoning":
			thought = entry
		}
	}
	if user.provenance != taskPreviewProvider || thought.provenance != taskPreviewLive {
		t.Fatalf("committed prompt / supplemental reasoning provenance = user:%q thought:%q", user.provenance, thought.provenance)
	}
}

func TestStructuredACPRawViewIsExplicitAndForensic(t *testing.T) {
	model := visualSessionModel(t)
	model.width, model.height, model.raw = 120, 40, true
	view := model.View()
	assertScreenSize(t, view, 120, 40)
	if !strings.Contains(strings.ReplaceAll(view, "\n", ""), visualSentinel) {
		t.Fatalf("raw view did not expose forensic history:\n%s", view)
	}
	model.scroll = 1 << 30
	top := model.View()
	if !strings.Contains(top, "jsonrpc") {
		t.Fatalf("raw view did not expose raw ACP when scrolled to the start:\n%s", top)
	}
}

func TestStructuredACPExpandedReasoningAndComposerHierarchy(t *testing.T) {
	model := visualSessionModel(t)
	model.width, model.height = 80, 24
	model.showReasoning = true
	model.composer = editorFixture("Steer the active turn with this context.")
	model.composer.focused = true
	view := model.View()
	assertScreenSize(t, view, model.width, model.height)
	for _, want := range []string{
		"▾ Reasoning", "Checking the worker and UI boundaries.",
		"MESSAGE CLAUDE · editing", "Steer the active turn with this context.▌", "Enter send · Ctrl+J newline · Esc keep draft",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded ACP console missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "◇ REASONING") || strings.Contains(view, "╭─ ✓ TOOL") {
		t.Fatalf("expanded ACP console retained the old framed activity hierarchy:\n%s", view)
	}
}

func TestStructuredACPHeaderKeepsIdentityBesideLongLocation(t *testing.T) {
	model := visualSessionModel(t)
	model.width, model.height = 120, 40
	model.current.WorktreePath = "/worktrees/this-is-an-intentionally-long-checkout-name-that-must-not-displace-session-identity"
	model.current.Branch = "agent/an-equally-long-responsive-terminal-console-review-branch"
	view := model.View()
	assertScreenSize(t, view, model.width, model.height)
	header := strings.Split(view, "\n")[1]
	for _, want := range []string{"CLAUDE · reviewer · read approval", "Task T-VISUAL"} {
		if !strings.Contains(header, want) {
			t.Fatalf("long Session location displaced %q from metadata header: %q", want, header)
		}
	}
}

func TestStructuredACPShowsCompletedTurnState(t *testing.T) {
	if got := renderTurnState(turnComplete); got != "  ✓ Turn complete" {
		t.Fatalf("completed turn status = %q", got)
	}
}

func TestStructuredACPTaskReturnRemainsVisibleAtMinimumWidth(t *testing.T) {
	model := visualSessionModel(t)
	model.direct = false
	model.width, model.height = 60, 16
	footer := strings.Split(model.View(), "\n")[model.height-1]
	if !strings.Contains(footer, "b Back") || strings.Contains(footer, "…") {
		t.Fatalf("minimum-width Task return action is not intact: %q", footer)
	}
}

func TestCompactStructuredACPLabelsUnavailableNativeAction(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Model)
	}{
		{name: "missing identity", mutate: func(model *Model) { model.current.ProviderConversationID = "" }},
		{name: "unsupported agent", mutate: func(model *Model) { model.current.Agent = "gemini" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := visualSessionModel(t)
			test.mutate(&model)
			footer := model.sessionFooter(55)
			if !strings.Contains(footer, "n N/A") || strings.Contains(footer, "n Native") {
				t.Fatalf("compact unavailable native action = %q", footer)
			}
		})
	}
}

func TestStructuredACPToolDetailsStayBounded(t *testing.T) {
	model := visualSessionModel(t)
	model.width, model.height = 100, 32
	tool := model.currentTool()
	if tool == nil {
		t.Fatal("visual fixture has no tool")
	}
	model.expandedTool = tool.id
	view := model.View()
	assertScreenSize(t, view, 100, 32)
	if strings.Count(view, "package example") > 6 {
		t.Fatalf("tool details were not visually bounded:\n%s", view)
	}
	if !strings.Contains(view, "details clipped") {
		t.Fatalf("bounded detail did not disclose clipping:\n%s", view)
	}
}

func TestNewSessionWizardVisualContract(t *testing.T) {
	model := Model{
		mode: taskView, width: 80, height: 24,
		task: taskFixture(),
		wizard: &sessionWizard{
			agents:     []agents.Profile{{Name: "claude", Provider: "anthropic"}, {Name: "codex", Provider: "openai"}},
			agentIndex: 1, worktree: true, field: wizardPrompt,
			role: editorFixture("reviewer"), repo: editorFixture("/repo/acp-gateway"), prompt: editorFixture("Review the implementation."),
		},
	}
	view := model.View()
	assertScreenSize(t, view, 80, 24)
	for _, want := range []string{"NEW SESSION", "Agent", "codex · openai", "Role", "reviewer", "Repository", "/repo/acp-gateway", "Worktree", "ON · isolated", "Permission", "read approval", "Initial prompt", "Ctrl+S starts"} {
		if !strings.Contains(view, want) {
			t.Fatalf("wizard missing %q:\n%s", want, view)
		}
	}
}

func TestConsoleSmallTerminalFailsClosedWithoutGarbledOutput(t *testing.T) {
	model := visualSessionModel(t)
	model.width, model.height = 59, 15
	view := model.View()
	assertScreenSize(t, view, 59, 15)
	if !strings.Contains(view, "Terminal too small") || !strings.Contains(view, "Session continues") {
		t.Fatalf("small terminal view is not actionable:\n%s", view)
	}
}

func visualSessionModel(t *testing.T) Model {
	t.Helper()
	projection := newTranscript()
	body := strings.Repeat("package example // source line\n", 760) + visualSentinel
	history := []sessionlog.Line{
		projectionLine(1, "REQ-VISUAL", "prompt_submitted", map[string]any{"display_message": "Review the implementation.\n\nFocus on correctness and safe detach behavior."}),
		projectionLine(2, "REQ-VISUAL", "turn_started", map[string]any{}),
		projectionACPLine(3, "REQ-VISUAL", "agent_thought_chunk", sessionUpdate(messageUpdate("agent_thought_chunk", "Checking the worker and UI boundaries.", "thought"))),
		projectionACPLine(4, "REQ-VISUAL", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "I found two issues and I am verifying both before reporting them.", "message"))),
		projectionACPLine(5, "REQ-VISUAL", "tool_call", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": "tool-read", "title": "Read", "kind": "read", "status": "in_progress",
		})),
		projectionACPLine(6, "REQ-VISUAL", "tool_call_update", sessionUpdate(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "tool-read", "rawInput": map[string]any{"path": "/repo/internal/console/model.go"},
			"status": "completed", "rawOutput": body,
		})),
		projectionACPLine(7, "REQ-VISUAL", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "The default viewer no longer expands source files. Detaching leaves the active turn running.", "message-2"))),
	}
	for _, line := range history {
		projection.apply(line)
	}
	current := session.Session{
		ID: "S-VISUAL", TaskID: "T-VISUAL", Role: "reviewer", Agent: "claude", Provider: "anthropic",
		RepoPath: "/repo/acp-gateway", WorktreePath: "/worktrees/review", Branch: "agent/review",
		Status: session.StatusWorking, MetadataJSON: []byte(`{"permission":"read-approval"}`),
	}
	return Model{
		ctx: context.Background(), mode: sessionView, direct: true, current: current,
		histories: map[string][]sessionlog.Line{current.ID: history}, projections: map[string]*transcript{current.ID: projection},
		cursors: make(map[string]uint64), seen: make(map[string]map[uint64]struct{}), width: 80, height: 24,
	}
}

func visualTaskModel() Model {
	claude := session.Session{
		ID: "S-CLAUDE", TaskID: "T-VISUAL", Role: "reviewer", Agent: "claude", Provider: "anthropic",
		RepoPath: "/repo/acp-gateway", WorktreePath: "/worktrees/review", Branch: "agent/review",
		ProviderConversationID: "claude-conversation", Status: session.StatusWorking,
		MetadataJSON: []byte(`{"permission":"read-approval"}`),
	}
	codex := session.Session{
		ID: "S-CODEX", TaskID: "T-VISUAL", Role: "implementer", Agent: "codex", Provider: "openai",
		RepoPath: "/repo/acp-gateway", WorktreePath: "/worktrees/build", Branch: "agent/build",
		ProviderConversationID: "codex-conversation", Status: session.StatusWaiting,
		MetadataJSON: []byte(`{"permission":"approve-all"}`),
	}
	projection := transcriptFromConversation(appservice.ConversationSnapshot{Turns: []appservice.ConversationTurn{{
		Key: "provider-task", Committed: true,
		Activities: []appservice.ConversationActivity{
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "user_message", Text: "Review the native attachment UX and preserve exact conversation continuity."}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "thought_chunk", Text: "Checked the durable provider transcript before continuing."}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "tool_upsert", Tool: &presentation.Tool{ID: "provider-read", Title: "Inspect continuity", Kind: "read", Status: "completed"}}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "assistant_chunk", MessageID: "provider-answer", Text: "Provider transcript confirms safe detach and exact native continuity."}},
			{Kind: "provider_transcript", Display: &presentation.Activity{Type: "turn_state", State: "complete"}},
		},
	}}})

	projection.apply(projectionLine(1, "REQ-TASK", "prompt_submitted", map[string]any{"display_message": "Check the active ACP handoff and report the latest state."}))
	projection.apply(projectionLine(2, "REQ-TASK", "turn_started", map[string]any{}))
	projection.apply(projectionACPLine(3, "REQ-TASK", "agent_thought_chunk", sessionUpdate(messageUpdate("agent_thought_chunk", "Checking the active ACP handoff and ownership barrier.", "thought"))))
	projection.apply(projectionACPLine(4, "REQ-TASK", "tool_call", sessionUpdate(map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "tool-worker", "title": "Read worker state", "kind": "read", "status": "in_progress",
	})))
	projection.apply(projectionACPLine(5, "REQ-TASK", "tool_call_update", sessionUpdate(map[string]any{
		"sessionUpdate": "tool_call_update", "toolCallId": "tool-worker", "title": "Read worker state", "kind": "read", "status": "completed",
	})))
	projection.apply(projectionACPLine(6, "REQ-TASK", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "Streaming the latest findings through ACP while the provider conversation remains continuous.", "message"))))
	return Model{
		mode: taskView,
		task: task.Detail{
			Task:     task.Task{ID: "T-VISUAL", Title: "Ship the native-first developer console", Status: task.StatusWorking},
			Sessions: []session.Session{claude, codex},
		},
		selected: 0,
		projections: map[string]*transcript{
			claude.ID: projection,
			codex.ID:  newTranscript(),
		},
		histories: make(map[string][]sessionlog.Line), cursors: make(map[string]uint64), seen: make(map[string]map[uint64]struct{}),
		width: 80, height: 24,
	}
}

func taskFixture() task.Detail {
	return task.Detail{Task: task.Task{ID: "T-VISUAL", Title: "Build native Session attachment", Status: task.StatusWorking}}
}

func editorFixture(value string) composerState {
	return composerState{value: []rune(value), cursor: len([]rune(value))}
}

func assertScreenSize(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) != height {
		t.Fatalf("screen lines = %d, want %d", len(lines), height)
	}
	for index, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("line %d width = %d, terminal width %d: %q", index+1, got, width, line)
		}
	}
}

func itoa(value int) string { return fmt.Sprintf("%d", value) }
