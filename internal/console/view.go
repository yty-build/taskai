package console

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/render"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

const (
	minimumConsoleWidth  = 60
	minimumConsoleHeight = 16
)

func (m Model) View() string {
	width, height := m.width, m.height
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	if width < minimumConsoleWidth || height < minimumConsoleHeight {
		return compactSizeWarning(width, height)
	}
	if m.wizard != nil {
		return m.renderSessionWizard(width, height)
	}
	if m.mode == sessionView {
		return m.renderSession(width, height)
	}
	return m.renderTask(width, height)
}

func (m Model) renderSessionWizard(width, height int) string {
	wizard := m.wizard
	header := []string{
		frameTop("NEW SESSION", "Task "+safeText(m.task.Task.ID), width),
		frameLine("Add one persistent ACP agent Session to "+safeText(m.task.Task.Title), width),
		frameLine("Fields are preserved if validation or launch fails.", width),
		frameBottom(width),
	}
	profile := wizard.agents[wizard.agentIndex]
	permission := "read approval"
	if wizard.approveAll {
		permission = "approve all (unattended writes)"
	}
	worktree := "ON · isolated checkout"
	if !wizard.worktree {
		worktree = "OFF · shared checkout"
	}
	fields := []string{
		wizardFieldLine(wizard.field == wizardAgent, "Agent", profile.Name+" · "+profile.Provider+"   ↑/↓ choose", width),
		wizardFieldLine(wizard.field == wizardRole, "Role", wizardEditorValue(wizard.role, wizard.field == wizardRole, "worker"), width),
		wizardFieldLine(wizard.field == wizardRepo, "Repository", wizardEditorValue(wizard.repo, wizard.field == wizardRepo, "."), width),
		wizardFieldLine(wizard.field == wizardWorktree, "Worktree", worktree+"   Space toggles", width),
		wizardFieldLine(wizard.field == wizardPermission, "Permission", permission+"   Space toggles", width),
		wizardFieldLine(wizard.field == wizardPrompt, "Initial prompt", wizardEditorValue(wizard.prompt, wizard.field == wizardPrompt, "optional instructions"), width),
	}
	bodyHeight := max(1, height-len(header)-3)
	body := padLines(append([]string{""}, fields...), bodyHeight, width)
	notice := ""
	if wizard.submitting {
		notice = "Starting Session and submitting its initial turn…"
	} else if wizard.errorNotice != "" {
		notice = "ERROR · " + wizard.errorNotice
	}
	footer := "Tab/Shift+Tab move · Enter advances · Ctrl+S starts · Ctrl+J newline · Esc dismisses"
	lines := append(header, body...)
	lines = append(lines, noticeLine(notice, width), fitLine(footer, width), fitLine("New Session will open automatically after it starts.", width))
	return renderScreen(lines, width, height)
}

func wizardFieldLine(active bool, label, value string, width int) string {
	marker := "  "
	if active {
		marker = "› "
	}
	return fitLine(fmt.Sprintf("%s%-14s %s", marker, label, value), width)
}

func wizardEditorValue(editor composerState, active bool, placeholder string) string {
	value := editor.text()
	if value == "" {
		value = placeholder
	}
	if !active {
		return safeText(value)
	}
	cursor := min(max(0, editor.cursor), len(editor.value))
	if len(editor.value) == 0 {
		return "▌" + placeholder
	}
	return safeText(string(editor.value[:cursor]) + "▌" + string(editor.value[cursor:]))
}

func compactSizeWarning(width, height int) string {
	lines := []string{
		"Agent Task Console",
		fmt.Sprintf("Terminal too small: %d×%d", width, height),
		fmt.Sprintf("Resize to at least %d×%d. The Session continues in the background.", minimumConsoleWidth, minimumConsoleHeight),
		"q detach",
	}
	return renderScreen(lines, max(1, width), max(1, height))
}

func (m Model) renderTask(width, height int) string {
	item := m.task.Task
	header := []string{
		frameTop("TASK "+safeText(item.ID), statusBadge(string(item.Status)), width),
		frameLine(safeText(item.Title), width),
		frameLine(taskSubtitle(item.Description, len(m.task.Sessions)), width),
		frameBottom(width),
	}

	bodyHeight := max(1, height-len(header)-2)
	body := m.renderTaskSessions(width, bodyHeight)
	notice := noticeLine(m.notice, width)
	footer := fitLine("↑/↓ select · Enter/click open · p message · n new Session · c cancel · u resume · x stop · q detach", width)
	return renderScreen(append(append(append(header, body...), notice), footer), width, height)
}

func (m Model) renderTaskSessions(width, height int) []string {
	if len(m.task.Sessions) == 0 {
		return padLines([]string{
			"",
			centerLine("No Sessions yet", width),
			centerLine("Press n to add the first agent Session to this Task.", width),
		}, height, width)
	}

	blocks := make([][]string, 0, len(m.task.Sessions))
	for index, current := range m.task.Sessions {
		selected := "  "
		if index == m.selected {
			selected = "› "
		}
		roleAgent := strings.ToUpper(valueOr(current.Agent, "agent")) + " · " + valueOr(current.Role, "worker")
		state := statusBadge(string(current.Status))
		first := selected + roleAgent
		first = fitWithRight(first, state, width)
		context := "    " + current.ID + " · " + appservice.PermissionLabel(current) + " · " + compactSessionPath(current)
		summary := "    " + m.sessionSummary(current.ID)
		blocks = append(blocks, []string{fitLine(first, width), fitLine(context, width), fitLine(summary, width)})
	}

	visibleBlocks := max(1, height/3)
	start := 0
	if m.selected >= visibleBlocks {
		start = m.selected - visibleBlocks + 1
	}
	if start+visibleBlocks > len(blocks) {
		start = max(0, len(blocks)-visibleBlocks)
	}
	var lines []string
	for index := start; index < len(blocks) && index < start+visibleBlocks; index++ {
		lines = append(lines, blocks[index]...)
	}
	if start > 0 && len(lines) > 0 {
		lines[0] = fitLine(fmt.Sprintf("  ↑ %d more Session(s)", start), width)
	}
	remaining := len(blocks) - (start + visibleBlocks)
	if remaining > 0 && len(lines) > 0 {
		lines[len(lines)-1] = fitLine(fmt.Sprintf("  ↓ %d more Session(s)", remaining), width)
	}
	return padLines(lines, height, width)
}

func (m Model) sessionSummary(sessionID string) string {
	projection := m.projections[sessionID]
	if projection == nil {
		return "No conversation yet"
	}
	return projection.latestSummary()
}

func (m Model) renderSession(width, height int) string {
	current := m.current
	provider := strings.ToUpper(valueOr(current.Agent, "agent"))
	header := []string{
		frameTop("SESSION "+safeText(current.ID), statusBadge(string(current.Status)), width),
		frameLine(provider+" · "+valueOr(current.Role, "worker")+" · "+appservice.PermissionLabel(current), width),
		frameLine("Task "+safeText(current.TaskID)+" · "+compactSessionPath(current), width),
		frameBottom(width),
	}

	composer := renderComposer(m.composer, provider, width)
	reserved := len(header) + len(composer) + 2
	bodyHeight := max(1, height-reserved)
	var allBody []string
	if m.raw {
		allBody = renderRawHistory(m.histories[current.ID], width)
	} else {
		allBody = renderConversation(m.projections[current.ID], provider, width, conversationOptions{
			showReasoning: m.showReasoning,
			expandedTool:  m.expandedTool,
			selectedTool:  m.selectedTool,
		})
	}
	body := viewportLines(allBody, bodyHeight, m.scroll, width)
	notice := m.sessionNotice(width)
	footer := m.sessionFooter(width)
	lines := append(header, body...)
	lines = append(lines, notice)
	lines = append(lines, composer...)
	lines = append(lines, footer)
	return renderScreen(lines, width, height)
}

func (m Model) sessionNotice(width int) string {
	if m.confirm != nil {
		return fitLine("CONFIRM · "+m.confirm.message+"  [y/N]", width)
	}
	if m.unread > 0 {
		return fitLine(fmt.Sprintf("%d new update(s) · press End to follow live", m.unread), width)
	}
	return noticeLine(m.notice, width)
}

func (m Model) sessionFooter(width int) string {
	back := ""
	if !m.direct {
		back = " · b back"
	}
	mode := "r raw"
	if m.raw {
		mode = "r conversation"
	}
	return fitLine("p message · ↑/↓ scroll · End follow · d tool · o reasoning · Esc cancel · x stop · "+mode+back+" · q detach", width)
}

type conversationOptions struct {
	showReasoning bool
	expandedTool  string
	selectedTool  string
}

func renderConversation(projection *transcript, agent string, width int, options conversationOptions) []string {
	if projection == nil || len(projection.requests) == 0 {
		return []string{"", centerLine("No ACP-visible conversation yet", width), centerLine("Messages and agent work will appear here live.", width)}
	}
	contentWidth := max(20, width-4)
	var lines []string
	for requestIndex, request := range projection.requests {
		if requestIndex > 0 {
			lines = append(lines, "")
		}
		for _, block := range request.blocks {
			switch block.kind {
			case blockUser:
				lines = append(lines, "YOU")
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
			case blockAssistant:
				lines = append(lines, agent)
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
				if block.truncated {
					lines = append(lines, "  … message clipped in the viewer; raw history is preserved")
				}
			case blockThought:
				lines = append(lines, renderThought(block, contentWidth, options.showReasoning)...)
			case blockTool:
				lines = append(lines, renderTool(block.tool, width, options)...)
			case blockPlan:
				lines = append(lines, renderPlan(block.plan, contentWidth)...)
			case blockPermission:
				lines = append(lines, renderPermission(block.permission, width)...)
			case blockError:
				lines = append(lines, "ERROR")
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
			}
		}
		if state := renderTurnState(request.state); state != "" {
			lines = append(lines, state)
		}
	}
	return lines
}

func renderThought(block *transcriptBlock, width int, expanded bool) []string {
	count := logicalLines(block.text)
	if !expanded {
		return []string{fmt.Sprintf("◇ Reasoning · %d line(s) (collapsed; press o to show)", count)}
	}
	lines := []string{"◇ REASONING"}
	wrapped := indentWrapped(block.text, width, "  ")
	if len(wrapped) > 10 {
		wrapped = append(wrapped[:10], "  … reasoning clipped in this view")
	}
	return append(lines, wrapped...)
}

func renderTool(tool *toolCard, width int, options conversationOptions) []string {
	if tool == nil {
		return nil
	}
	selected := ""
	if options.selectedTool == tool.id {
		selected = " ›"
	}
	title := toolStatusGlyph(tool.status) + " TOOL · " + toolDisplayTitle(tool) + selected
	lines := []string{"╭─ " + fitLine(title, max(1, width-3))}
	var metadata []string
	if tool.status != "" {
		metadata = append(metadata, strings.ReplaceAll(tool.status, "_", " "))
	}
	if tool.kind != "" {
		metadata = append(metadata, strings.ReplaceAll(tool.kind, "_", " "))
	}
	if tool.outputBytes > 0 {
		metadata = append(metadata, humanBytes(tool.outputBytes))
	}
	if tool.outputLines > 0 {
		metadata = append(metadata, fmt.Sprintf("%d lines", tool.outputLines))
	}
	lines = append(lines, "│  "+fitLine(strings.Join(metadata, " · "), max(1, width-4)))
	if tool.preview != "" {
		preview := indentWrapped(tool.preview, max(10, width-6), "│  ")
		if len(preview) > 3 {
			preview = preview[:3]
		}
		lines = append(lines, preview...)
	}
	if options.expandedTool == tool.id {
		detail := indentWrapped(tool.detail, max(10, width-6), "│  ")
		if len(detail) > 6 {
			detail = append(detail[:6], "│  … details clipped; press r for complete raw events")
		}
		if len(detail) == 0 {
			detail = []string{"│  No provider output was attached to this tool call."}
		}
		lines = append(lines, detail...)
	}
	hint := "d details"
	if options.expandedTool == tool.id {
		hint = "d collapse"
	}
	lines = append(lines, "╰─ "+hint+" · [/] select tool")
	return lines
}

func renderPlan(entries []planEntry, width int) []string {
	if len(entries) == 0 {
		return nil
	}
	lines := []string{"PLAN"}
	for _, entry := range entries {
		line := "  " + planStatusGlyph(entry.status) + " " + entry.content
		lines = append(lines, indentWrapped(line, width, "")...)
	}
	return lines
}

func renderPermission(card *permissionCard, width int) []string {
	if card == nil {
		return nil
	}
	title := valueOr(card.title, "Tool permission")
	if card.state != "waiting" {
		return []string{
			"╭─ APPROVAL · " + fitLine(title, max(1, width-15)),
			"│  resolved · " + valueOr(card.decision, "decision recorded"),
			"╰─",
		}
	}
	lines := []string{
		"╭─ APPROVAL REQUIRED · " + fitLine(title, max(1, width-27)),
		"│  The agent is waiting; detaching will not dismiss this request.",
	}
	for index, option := range card.options {
		label := valueOr(option.name, strings.ReplaceAll(option.kind, "_", " "))
		lines = append(lines, fmt.Sprintf("│  [%d] %s", index+1, label))
	}
	lines = append(lines, "╰─ choose an offered option")
	return lines
}

func renderTurnState(state turnState) string {
	switch state {
	case turnQueued:
		return "◌ Message queued"
	case turnLive:
		return "● Agent is working…"
	case turnCancelling:
		return "◌ Cancellation requested…"
	case turnCancelled:
		return "× Turn cancelled · Session remains available"
	case turnFailed:
		return "! Turn failed"
	default:
		return ""
	}
}

func renderRawHistory(history []sessionlog.Line, width int) []string {
	if len(history) == 0 {
		return []string{"No raw Session history yet."}
	}
	var lines []string
	for _, line := range history {
		raw := sanitizeDisplay(render.Activity(line, true))
		wrapped := ansi.Hardwrap(raw, max(10, width), true)
		lines = append(lines, strings.Split(wrapped, "\n")...)
	}
	return lines
}

func renderComposer(composer composerState, agent string, width int) []string {
	title := "MESSAGE " + agent
	if composer.focused {
		title += " · EDITING"
	} else if len(composer.value) > 0 {
		title += " · DRAFT"
	}
	content := composer.text()
	if composer.focused {
		cursor := min(max(0, composer.cursor), len(composer.value))
		content = string(composer.value[:cursor]) + "▌" + string(composer.value[cursor:])
	}
	if content == "" {
		content = "Press p to write a message"
	}
	wrapped := indentWrapped(content, max(10, width-4), "│ ")
	if len(wrapped) > 2 {
		wrapped = wrapped[len(wrapped)-2:]
	}
	for len(wrapped) < 2 {
		wrapped = append(wrapped, "│ ")
	}
	return []string{
		frameTop(title, "", width),
		fitLine(wrapped[0], width),
		fitLine(wrapped[1], width),
		frameBottom(width),
	}
}

func renderScreen(lines []string, width, height int) string {
	if height > 0 {
		if len(lines) > height {
			lines = lines[:height]
		}
		for len(lines) < height {
			lines = append(lines, "")
		}
	}
	for index := range lines {
		lines[index] = fitLine(lines[index], width)
	}
	return strings.Join(lines, "\n")
}

func viewportLines(lines []string, count, scroll, width int) []string {
	if len(lines) == 0 {
		lines = []string{"No conversation yet."}
	}
	if len(lines) <= count {
		return padLines(lines, count, width)
	}
	maxScroll := len(lines) - count
	scroll = min(max(0, scroll), maxScroll)
	end := len(lines) - scroll
	start := max(0, end-count)
	return padLines(lines[start:end], count, width)
}

func padLines(lines []string, count, width int) []string {
	if len(lines) > count {
		lines = lines[:count]
	}
	result := make([]string, 0, count)
	for _, line := range lines {
		result = append(result, fitLine(line, width))
	}
	for len(result) < count {
		result = append(result, "")
	}
	return result
}

func indentWrapped(value string, width int, indent string) []string {
	value = sanitizeDisplay(value)
	if value == "" {
		return []string{indent}
	}
	available := max(1, width-ansi.StringWidth(indent))
	var result []string
	for _, logical := range strings.Split(value, "\n") {
		if logical == "" {
			result = append(result, indent)
			continue
		}
		wrapped := ansi.Wordwrap(logical, available, "")
		wrapped = ansi.Hardwrap(wrapped, available, true)
		for _, line := range strings.Split(wrapped, "\n") {
			result = append(result, indent+line)
		}
	}
	return result
}

func frameTop(left, right string, width int) string {
	if width < 4 {
		return strings.Repeat("─", width)
	}
	left, right = safeText(left), safeText(right)
	prefix := "╭─ " + left + " "
	suffix := "╮"
	if right != "" {
		suffix = " " + right + " ─╮"
	}
	available := width - ansi.StringWidth(suffix)
	prefix = ansi.Truncate(prefix, max(1, available), "…")
	fill := max(1, width-ansi.StringWidth(prefix)-ansi.StringWidth(suffix))
	return fitLine(prefix+strings.Repeat("─", fill)+suffix, width)
}

func frameLine(value string, width int) string {
	if width < 4 {
		return fitLine(value, width)
	}
	inner := ansi.Truncate(safeText(value), width-4, "…")
	return "│ " + padRight(inner, width-4) + " │"
}

func frameBottom(width int) string {
	if width < 2 {
		return strings.Repeat("─", width)
	}
	return "╰" + strings.Repeat("─", width-2) + "╯"
}

func fitWithRight(left, right string, width int) string {
	rightWidth := ansi.StringWidth(right)
	if rightWidth+2 >= width {
		return fitLine(right, width)
	}
	left = ansi.Truncate(left, width-rightWidth-1, "…")
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-rightWidth)) + right
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = strings.ReplaceAll(sanitizeDisplay(value), "\n", " ")
	return ansi.Truncate(value, width, "…")
}

func padRight(value string, width int) string {
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}

func centerLine(value string, width int) string {
	value = fitLine(value, width)
	left := max(0, (width-ansi.StringWidth(value))/2)
	return strings.Repeat(" ", left) + value
}

func noticeLine(value string, width int) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return fitLine("• "+safeText(value), width)
}

func taskSubtitle(description string, sessions int) string {
	label := fmt.Sprintf("%d Session(s)", sessions)
	if strings.TrimSpace(description) == "" {
		return label
	}
	return label + " · " + safeText(description)
}

func compactSessionPath(current session.Session) string {
	path := current.WorktreePath
	if path == "" {
		path = current.RepoPath
	}
	label := filepath.Base(strings.TrimSuffix(path, string(filepath.Separator)))
	if label == "." || label == string(filepath.Separator) || label == "" {
		label = path
	}
	if current.Branch != "" {
		label += " [" + current.Branch + "]"
	}
	return safeText(label)
}

func sessionPath(current session.Session) string {
	if current.WorktreePath != "" {
		if current.Branch != "" {
			return current.WorktreePath + " [" + current.Branch + "]"
		}
		return current.WorktreePath
	}
	return current.RepoPath
}

func statusBadge(status string) string {
	return statusGlyph(status) + " " + strings.ReplaceAll(status, "_", " ")
}

func statusGlyph(status string) string {
	switch status {
	case "working", "starting":
		return "●"
	case "waiting", "open", "review":
		return "○"
	case "done", "complete", "completed":
		return "✓"
	case "failed", "error":
		return "!"
	case "cancelled", "canceled":
		return "×"
	case "stopped":
		return "■"
	default:
		return "·"
	}
}

func toolStatusGlyph(status string) string {
	switch status {
	case "completed", "complete", "succeeded":
		return "✓"
	case "failed", "error":
		return "!"
	case "cancelled", "canceled":
		return "×"
	case "in_progress", "running", "pending":
		return "●"
	default:
		return "◇"
	}
}

func planStatusGlyph(status string) string {
	switch status {
	case "completed", "complete":
		return "✓"
	case "in_progress", "running":
		return "●"
	case "failed", "error":
		return "!"
	default:
		return "○"
	}
}

func humanBytes(size int) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
}

func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == '\u007f' {
			return -1
		}
		return r
	}, ansi.Strip(value))
}

func clipText(value string, width int) string {
	return ansi.Truncate(safeText(value), width, "…")
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return safeText(value)
}
