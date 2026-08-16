package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/agents"
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
		digest string
	}{
		{width: 60, height: 16, digest: "011cb8eee81251ab"},
		{width: 80, height: 24, digest: "1fb9a784a30b7934"},
		{width: 120, height: 40, digest: "8d76b7037ec500be"},
	} {
		t.Run(strings.Join([]string{itoa(size.width), "x", itoa(size.height)}, ""), func(t *testing.T) {
			model.width, model.height = size.width, size.height
			view := model.View()
			assertScreenSize(t, view, size.width, size.height)
			for _, want := range []string{
				"AGENT TASK CONSOLE · TASK T-VISUAL", "SESSIONS", "SELECTED SESSION",
				"CLAUDE · reviewer", "Enter  Claude CLI", "a        Structured ACP",
				"Enter Native CLI", "a ACP", "n New Session", "? Help", "q Detach",
			} {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d Task view missing %q:\n%s", size.width, size.height, want, view)
				}
			}
			if strings.Contains(view, "╭─ AGENT TASK CONSOLE") || strings.Contains(view, "SESSION PREVIEW") {
				t.Fatalf("%dx%d Task view retained the old framed hierarchy:\n%s", size.width, size.height, view)
			}
			if got := screenDigest(view); got != size.digest {
				t.Fatalf("%dx%d Task snapshot digest = %s, want %s:\n%s", size.width, size.height, got, size.digest, view)
			}
		})
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
	}{{width: 60, height: 16, want: "8 more ↓"}, {width: 80, height: 24, want: "7 more ↓"}} {
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
		digest string
	}{
		{width: 60, height: 16, digest: "b2e6e0ac7ef4366c"},
		{width: 80, height: 24, digest: "8f2bb85be2959479"},
		{width: 120, height: 40, digest: "3ba32891d706ce56"},
		{width: 200, height: 60, digest: "b21851286bbd9c76"},
	} {
		t.Run(strings.Join([]string{itoa(size.width), "x", itoa(size.height)}, ""), func(t *testing.T) {
			model.width, model.height = size.width, size.height
			view := model.View()
			assertScreenSize(t, view, size.width, size.height)
			wants := []string{"STRUCTURED ACP · SESSION S-VISUAL", "CLAUDE · reviewer", "23.1 KB", "761 lines", "MESSAGE CLAUDE"}
			if size.height >= 24 {
				wants = append(wants, "TOOL · model.go", "YOU", "Review the implementation", "I found two issues")
			}
			for _, want := range wants {
				if !strings.Contains(view, want) {
					t.Fatalf("%dx%d view missing %q:\n%s", size.width, size.height, want, view)
				}
			}
			for _, forbidden := range []string{visualSentinel, "jsonrpc", "REQ-VISUAL", "toolCallId", "package example"} {
				if strings.Contains(view, forbidden) {
					t.Fatalf("%dx%d normal view leaked %q:\n%s", size.width, size.height, forbidden, view)
				}
			}
			if got := screenDigest(view); got != size.digest {
				t.Fatalf("%dx%d snapshot digest = %s, want %s:\n%s", size.width, size.height, got, size.digest, view)
			}
		})
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
	projection := newTranscript()
	projection.apply(projectionLine(1, "REQ-TASK", "prompt_submitted", map[string]any{"display_message": "Review the native attachment UX."}))
	projection.apply(projectionLine(2, "REQ-TASK", "turn_started", map[string]any{}))
	projection.apply(projectionACPLine(3, "REQ-TASK", "agent_message_chunk", sessionUpdate(messageUpdate("agent_message_chunk", "Auditing the terminal interaction model.", "message"))))
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

func screenDigest(view string) string {
	digest := sha256.Sum256([]byte(view))
	return hex.EncodeToString(digest[:8])
}

func itoa(value int) string { return fmt.Sprintf("%d", value) }
