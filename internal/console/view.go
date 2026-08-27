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
)

const (
	minimumConsoleWidth  = 40
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
	if m.overlay != noOverlay {
		return m.renderOverlay(width, height)
	}
	if m.mode == sessionView {
		return m.renderSession(width, height)
	}
	return m.renderTask(width, height)
}

func (m Model) renderSessionWizard(width, height int) string {
	wizard := m.wizard
	header := []string{
		topBar("NEW SESSION", "Task "+safeText(m.task.Task.ID), width),
		fitLine("  Add one persistent agent Session to "+safeText(m.task.Task.Title), width),
		horizontalRule(width),
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
	locked := ""
	agentHint := "   ↑/↓ choose"
	toggleHint := "   Space toggles"
	submitVerb := "connects provider"
	if wizard.connectedID != "" {
		locked = "   locked after provider connection"
		agentHint = locked
		toggleHint = locked
		submitVerb = "configures and starts"
		if !wizard.providerOptionsReady {
			submitVerb = "reloads provider controls"
		}
	}
	model := "Connect provider to load choices"
	effort := model
	if wizard.connectedID != "" && wizard.providerOptionsReady {
		model = providerChoiceLabel(wizard.providerOptions.Model, wizard.modelIndex) + "   ↑/↓ choose"
		effort = providerChoiceLabel(wizard.providerOptions.ReasoningEffort, wizard.effortIndex) + "   ↑/↓ choose"
	} else if wizard.connectedID != "" {
		model = "Reconnect required   locked"
		effort = model
	}
	prompt := wizardEditorValue(wizard.prompt, wizard.field == wizardPrompt, "optional instructions")
	fields := []string{
		wizardFieldLine(wizard.field == wizardAgent, "Agent", profile.Name+" · "+profile.Provider+agentHint, width),
		wizardFieldLine(wizard.field == wizardRole, "Role", wizardEditorValue(wizard.role, wizard.field == wizardRole && wizard.connectedID == "", "worker")+locked, width),
		wizardFieldLine(wizard.field == wizardRepo, "Repository", wizardEditorValue(wizard.repo, wizard.field == wizardRepo && wizard.connectedID == "", ".")+locked, width),
		wizardFieldLine(wizard.field == wizardWorktree, "Worktree", worktree+toggleHint, width),
		wizardFieldLine(wizard.field == wizardPermission, "Permission", permission+toggleHint, width),
		wizardFieldLine(wizard.field == wizardModel, "Model", model, width),
		wizardFieldLine(wizard.field == wizardReasoningEffort, "Reasoning", effort, width),
		wizardFieldLine(wizard.field == wizardPrompt, "Initial prompt", prompt, width),
	}
	bodyHeight := max(1, height-len(header)-3)
	body := padLines(append([]string{""}, fields...), bodyHeight, width)
	notice := ""
	if wizard.submitting && wizard.connectedID == "" {
		notice = "Connecting one Session Port and loading provider-advertised choices…"
	} else if wizard.submitting && !wizard.providerOptionsReady {
		notice = "Resuming this Session Port and reloading its provider controls…"
	} else if wizard.submitting {
		notice = "Applying advertised choices and submitting the initial turn…"
	} else if wizard.errorNotice != "" {
		notice = "ERROR · " + wizard.errorNotice
	} else if wizard.connectedID != "" {
		notice = "Connected " + wizard.connectedID + " · no prompt sent yet"
	}
	footer := "Tab/Shift+Tab move · Enter advances · Ctrl+S " + submitVerb + " · Ctrl+J newline · Esc dismisses"
	lines := append(header, body...)
	lines = append(lines, noticeLine(notice, width), fitLine(footer, width), fitLine("Model and reasoning values come from this live provider Port; nothing is hard-coded.", width))
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
		"◆ AGENT TASK CONSOLE",
		fmt.Sprintf("Terminal too small: %d×%d", width, height),
		fmt.Sprintf("Resize to at least %d×%d. The Session continues in the background.", minimumConsoleWidth, minimumConsoleHeight),
		"q detach",
	}
	return renderScreen(lines, max(1, width), max(1, height))
}

func (m Model) renderTask(width, height int) string {
	item := m.task.Task
	layout := layoutTaskWorkspace(width, height)
	title := "TASK · " + safeText(item.Title)
	subtitle := taskSubtitle(item.Description, len(m.task.Sessions))
	if clipped := m.clippedReplayWindowCount(); clipped > 0 {
		subtitle += fmt.Sprintf(" · %d replay window(s) clipped", clipped)
	}
	if clipped := m.clippedProjectionCount(); clipped > 0 {
		subtitle += fmt.Sprintf(" · %d projection(s) clipped", clipped)
	}
	header := []string{
		topBar(title, statusBadge(string(item.Status)), width),
		fitLine("  "+safeText(item.ID)+" · "+subtitle, width),
		horizontalRule(width),
	}

	var body []string
	switch layout.Mode {
	case taskWorkspaceSplit:
		navigator := m.renderTaskNavigator(layout.Navigator.Width, layout.Navigator.Height)
		preview := m.renderTaskPreview(layout.Preview.Width, layout.Preview.Height)
		body = joinColumns(navigator, preview, layout.Navigator.Width, layout.Preview.Width)
	case taskWorkspaceStacked:
		body = append(m.renderTaskNavigator(layout.Navigator.Width, layout.Navigator.Height),
			m.renderTaskPreview(layout.Preview.Width, layout.Preview.Height)...)
	default:
		body = m.renderTaskNavigator(layout.Navigator.Width, layout.Navigator.Height)
	}
	notice := noticeLine(m.notice, width)
	footer := m.taskFooter(width)
	return renderScreen(append(append(append(header, body...), notice), footer), width, height)
}

func (m Model) renderTaskNavigator(width, height int) []string {
	indices, start, visible := m.taskSessionWindow(height)
	filterLabel := fmt.Sprintf("%d of %d", len(indices), len(m.task.Sessions))
	lines := []string{sectionHeading("SESSIONS", filterLabel, width), sectionRule(width)}
	innerHeight := max(1, height-2)
	if len(m.task.Sessions) == 0 {
		lines = append(lines, padLines([]string{"", centerLine("No Sessions yet", width), centerLine("Press n to add one", width)}, innerHeight, width)...)
		return lines
	}
	if len(indices) == 0 {
		lines = append(lines, padLines([]string{"", centerLine("No Sessions match /"+safeText(m.filter), width), centerLine("Esc clears the filter", width)}, innerHeight, width)...)
		return lines
	}
	var entries []string
	for position := start; position < len(indices) && position < start+visible; position++ {
		actual := indices[position]
		current := m.task.Sessions[actual]
		marker := "  "
		if actual == m.selected {
			marker = "▌ "
		}
		name := marker + strings.ToUpper(valueOr(current.Agent, "agent")) + " · " + valueOr(current.Role, "worker")
		entries = append(entries, fitWithRight(name, statusBadge(string(current.Status)), width))
		detailMarker := "  "
		if actual == m.selected {
			detailMarker = "┃ "
		}
		entries = append(entries, fitLine(detailMarker+compactSessionID(current.ID)+" · "+m.sessionSummary(current.ID)+" · "+compactSessionPath(current), width))
	}
	if start > 0 && len(entries) > 0 {
		entries[0] = fitLine(fmt.Sprintf("  ↑ %d earlier · ", start)+entries[0], width)
	}
	remaining := len(indices) - (start + visible)
	if remaining > 0 && len(entries) > 0 {
		entries[len(entries)-1] = fitWithRight(entries[len(entries)-1], fmt.Sprintf("%d more ↓", remaining), width)
	}
	lines = append(lines, padLines(entries, innerHeight, width)...)
	return lines
}

func compactSessionID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= 12 {
		return id
	}
	return id[:2] + "…" + id[len(id)-8:]
}

func (m Model) taskSessionWindow(navigatorHeight int) (indices []int, start, visible int) {
	indices = m.filteredSessionIndices()
	visible = max(1, max(1, navigatorHeight-2)/2)
	selectedPosition := 0
	for position, actual := range indices {
		if actual == m.selected {
			selectedPosition = position
			break
		}
	}
	start = max(0, selectedPosition-visible+1)
	start = min(start, max(0, len(indices)-visible))
	return indices, start, visible
}

func (m Model) renderTaskPreview(width, height int) []string {
	if len(m.task.Sessions) == 0 || len(m.filteredSessionIndices()) == 0 {
		lines := []string{sectionHeading("CONVERSATION", "", width), sectionRule(width)}
		lines = append(lines, padLines([]string{"", centerLine("Select or create a Session", width)}, max(1, height-2), width)...)
		return lines
	}
	current := m.task.Sessions[m.selected]
	title := "CONVERSATION · " + taskProviderName(current) + " · " + valueOr(current.Role, "worker")
	lines := []string{sectionHeading(title, statusBadge(string(current.Status)), width), sectionRule(width)}
	innerHeight := max(1, height-2)
	if height < 9 {
		lines = append(lines, m.renderCompactTaskPreview(current, width, innerHeight)...)
		return lines
	}
	overview := m.renderTaskPreviewOverview(current, width)
	timelineHeight := max(1, innerHeight-len(overview))
	content := append(overview, m.renderTaskTimeline(current, width, timelineHeight)...)
	lines = append(lines, padLines(content, innerHeight, width)...)
	return lines
}

func (m Model) sessionAttention(current session.Session) string {
	projection := m.projections[current.ID]
	if projection != nil {
		if _, permission := projection.pendingPermission(); permission != nil {
			if !m.permissionDecisionsReady[current.ID] {
				return "Approval retained · decisions are unavailable for this worker"
			}
			return "Approval required · agent is waiting for a decision"
		}
		switch projection.latestState() {
		case turnLive:
			return "Working now · open the conversation for full detail"
		case turnFailed:
			return "Turn failed · open the structured view for details"
		case turnComplete:
			return "Response complete · Session remains available"
		case turnIncomplete:
			return "Provider turn is incomplete · open the conversation for available history"
		case turnCancelled:
			return "Turn cancelled · Session remains available"
		}
	}
	return statusBadge(string(current.Status)) + " · conversation available"
}

func joinColumns(left, right []string, leftWidth, rightWidth int) []string {
	count := max(len(left), len(right))
	result := make([]string, 0, count)
	for index := 0; index < count; index++ {
		leftLine, rightLine := "", ""
		if index < len(left) {
			leftLine = left[index]
		}
		if index < len(right) {
			rightLine = right[index]
		}
		result = append(result, padRight(fitLine(leftLine, leftWidth), leftWidth)+" │ "+fitLine(rightLine, rightWidth))
	}
	return result
}

func (m Model) taskFooter(width int) string {
	if width < 56 {
		return fitLine("Enter Open  p Message  n New  q Detach", width)
	}
	type footerItem struct {
		id   actionID
		text string
	}
	actionItem := func(id actionID) footerItem {
		action, ok := taskActionForID(id)
		if !ok {
			return footerItem{id: id}
		}
		label := valueOr(action.FooterLabel, action.Label)
		return footerItem{id: id, text: displayActionKey(action.Key) + " " + label}
	}

	// These actions remain visible at the minimum supported width. Optional
	// context is added only when it cannot push New Session or detach off-screen.
	required := []footerItem{
		actionItem(actionOpen), actionItem(actionNew),
		actionItem(actionHelp), actionItem(actionDetach),
	}
	optional := []footerItem{
		actionItem(actionFilter),
		{text: "↑/↓ Select"},
		actionItem(actionMessage),
		actionItem(actionPalette),
	}
	enabled := make(map[actionID]bool, len(required)+len(optional))
	includeNavigation := false
	for _, item := range required {
		enabled[item.id] = true
	}
	items := func() []string {
		ordered := []footerItem{
			{text: "↑/↓ Select"},
			actionItem(actionOpen), actionItem(actionNew), actionItem(actionMessage),
			actionItem(actionFilter), actionItem(actionPalette), actionItem(actionHelp), actionItem(actionDetach),
		}
		var result []string
		for _, item := range ordered {
			if item.id == "" {
				if includeNavigation {
					result = append(result, item.text)
				}
				continue
			}
			if enabled[item.id] && item.text != "" {
				result = append(result, item.text)
			}
		}
		return result
	}
	for _, candidate := range optional {
		if candidate.id == "" {
			includeNavigation = true
		} else {
			enabled[candidate.id] = true
		}
		if ansi.StringWidth(strings.Join(items(), "  ")) > width {
			if candidate.id == "" {
				includeNavigation = false
			} else {
				delete(enabled, candidate.id)
			}
		}
	}
	footer := strings.Join(items(), "  ")
	if filter := strings.TrimSpace(m.filter); filter != "" {
		filtered := "FILTER /" + safeText(filter) + "  " + footer
		if ansi.StringWidth(filtered) <= width {
			footer = filtered
		}
	}
	return fitLine(footer, width)
}

func (m Model) renderOverlay(width, height int) string {
	var title, subtitle, footer string
	var body []string
	switch m.overlay {
	case helpOverlay:
		title, subtitle, footer = "KEYBOARD HELP", "This viewer reads and controls the Session Channel; detaching never stops it.", "Esc/?/Enter closes help"
		if m.mode == taskView {
			body = append(body, "NAVIGATION", "  ↑/↓ or j/k    Select a Session", "")
			for _, action := range taskActions {
				if action.ID == actionResume && !m.selectedCanResume() {
					continue
				}
				body = append(body, fmt.Sprintf("  %-8s %-22s %s", displayActionKey(action.Key), action.Label, action.Description))
			}
		} else {
			body = []string{
				"SESSION CHANNEL", "  p / i / Enter  Compose a message", "  ↑/↓ PgUp/PgDn Scroll history", "  End            Follow live output",
				"  1-9            Choose an offered permission", "  d              Expand selected tool", "  [ / ]          Select tool", "  o              Toggle reasoning",
				"  r              Toggle raw channel frames", "  Esc / c        Cancel active turn", "  b              Back to Task", "  q              Detach viewer",
			}
		}
	case paletteOverlay:
		title, subtitle, footer = "COMMAND PALETTE", "> "+editorDisplay(m.overlayEditor), "Type to filter · ↑/↓ choose · Enter run · Esc close"
		actions := filteredTaskActions(m.overlayEditor.text(), m.selectedCanResume())
		for index, action := range actions {
			marker := "  "
			if index == m.paletteIndex {
				marker = "› "
			}
			body = append(body, fmt.Sprintf("%s%-8s %s", marker, displayActionKey(action.Key), action.Label), "    "+action.Description)
		}
		if len(actions) == 0 {
			body = []string{"", "  No matching commands"}
		}
	case filterOverlay:
		title, subtitle, footer = "FILTER SESSIONS", "/ "+editorDisplay(m.overlayEditor), "Type to filter · Ctrl+U clear · Enter apply · Esc close"
		indices := m.filteredSessionIndices()
		body = append(body, fmt.Sprintf("  %d of %d Sessions match", len(indices), len(m.task.Sessions)), "")
		for _, index := range indices {
			current := m.task.Sessions[index]
			body = append(body, fmt.Sprintf("  %-24s %-12s %-14s %s", current.ID, current.Agent, current.Role, current.Status))
		}
	}
	header := []string{topBar(title, "", width), fitLine("  "+subtitle, width), horizontalRule(width)}
	bodyHeight := max(1, height-len(header)-1)
	return renderScreen(append(append(header, padLines(body, bodyHeight, width)...), fitLine(footer, width)), width, height)
}

func displayActionKey(key string) string {
	if key == "enter" {
		return "Enter"
	}
	return key
}

func editorDisplay(editor composerState) string {
	cursor := min(max(0, editor.cursor), len(editor.value))
	return safeText(string(editor.value[:cursor]) + "▌" + string(editor.value[cursor:]))
}

func (m Model) sessionSummary(sessionID string) string {
	projection := m.projections[sessionID]
	if projection == nil {
		if m.projectionOverflow[sessionID] {
			return "Projection limit · attach Session directly"
		}
		if m.projectionClipped[sessionID] {
			return "Conversation projection clipped"
		}
		return "No conversation yet"
	}
	return projection.latestSummary()
}

func (m Model) renderSession(width, height int) string {
	current := m.current
	provider := strings.ToUpper(valueOr(current.Agent, "agent"))
	metadata := provider + " · " + valueOr(current.Role, "worker") + " · " + humanizeLabel(appservice.PermissionLabel(current))
	location := "Task " + safeText(current.TaskID) + " · " + compactSessionPath(current)
	if width >= 92 {
		metadata = fitSplitMetadata("  "+metadata, location, width)
	} else {
		metadata = "  " + metadata + " · " + location
	}
	title := "SESSION CHANNEL · " + safeText(current.ID)
	if width < 56 {
		title = "SESSION " + safeText(current.ID) + " · VIEW"
	}
	header := []string{
		topBar(title, statusBadge(string(current.Status)), width),
		fitLine(metadata, width),
		horizontalRule(width),
	}

	composer := renderComposer(m.composer, provider, width)
	reserved := len(header) + len(composer) + 2
	bodyHeight := max(1, height-reserved)
	var allBody []string
	if m.raw {
		allBody = renderRawHistory(m.histories[current.ID], width)
	} else {
		allBody = renderConversation(m.projections[current.ID], provider, width, conversationOptions{
			showReasoning:     m.showReasoning,
			expandedTool:      m.expandedTool,
			selectedTool:      m.selectedTool,
			permissionReady:   m.permissionDecisionsReady[current.ID],
			permissionBlocked: m.permissionBlockedReason[current.ID],
		})
	}
	if notice := m.replayWindowNotice(current.ID); notice != "" {
		allBody = append([]string{notice, ""}, allBody...)
	}
	if !m.raw && m.projectionClipped[current.ID] {
		notice := "PROJECTION CLIPPED · earlier conversation cards were evicted from this disposable view; use raw channel frames or on-disk JSONL"
		if m.projectionOverflow[current.ID] {
			notice = "PROJECTION LIMIT · this Task view cannot retain another actionable approval; attach this exact Session directly"
		}
		allBody = append([]string{notice, ""}, allBody...)
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

func (m Model) replayWindowNotice(sessionID string) string {
	window, clipped := m.effectiveReplayWindow(sessionID)
	if !clipped {
		return ""
	}
	return fmt.Sprintf("REPLAY CLIPPED · earliest_sequence=%d · cursor=%d · raw JSONL on disk remains authoritative", window.EarliestSequence, window.Cursor)
}

func (m Model) effectiveReplayWindow(sessionID string) (appservice.ReplayWindow, bool) {
	if !m.historyClipped[sessionID] {
		return appservice.ReplayWindow{}, false
	}
	window, exists := m.replayWindows[sessionID]
	if !exists {
		window = replayWindow(0, true)
	}
	if history := m.histories[sessionID]; len(history) > 0 &&
		(window.EarliestSequence == 0 || history[0].line.Seq > window.EarliestSequence) {
		window = replayWindow(history[0].line.Seq-1, true)
	} else if len(m.histories[sessionID]) == 0 && m.cursors[sessionID] > window.Cursor {
		window = replayWindow(m.cursors[sessionID], true)
	}
	window.Clipped = true
	return window, true
}

func (m Model) clippedReplayWindowCount() int {
	count := 0
	for _, current := range m.task.Sessions {
		if m.historyClipped[current.ID] {
			count++
		}
	}
	return count
}

func (m Model) clippedProjectionCount() int {
	count := 0
	for _, current := range m.task.Sessions {
		if m.projectionClipped[current.ID] {
			count++
		}
	}
	return count
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
	mode := "r Raw"
	if m.raw {
		mode = "r Conversation"
	}
	approval := ""
	if projection := m.projections[m.current.ID]; projection != nil && m.permissionDecisionsReady[m.current.ID] {
		if _, card := projection.pendingPermission(); card != nil && len(card.options) > 0 {
			approval = fmt.Sprintf("1-%d choose approval", min(9, len(card.options)))
		}
	}
	required := []string{"p Message", "Esc Cancel"}
	if approval != "" {
		required = []string{approval, "p Message", "Esc Cancel"}
	}
	if !m.direct {
		required = append(required, "b Back")
	}
	required = append(required, "q Detach")
	if width < 56 {
		items := []string{"p Write"}
		if approval != "" {
			items = append([]string{approval}, items...)
		} else {
			items = append(items, "Esc Cancel")
		}
		if !m.direct {
			items = append(items, "b Back")
		}
		items = append(items, "q Detach")
		for ansi.StringWidth(strings.Join(items, "  ")) > width && len(items) > 2 {
			items = append(items[:len(items)-2], items[len(items)-1])
		}
		return fitLine(strings.Join(items, "  "), width)
	}
	optional := []string{"o Reasoning", mode, "↑/↓ Scroll", "End Follow", "d Tool", "x Stop"}
	items := append([]string(nil), required...)
	for _, item := range optional {
		candidate := strings.Join(append(items, item), "  ")
		if ansi.StringWidth(candidate) <= width {
			items = append(items, item)
		}
	}
	return fitLine(strings.Join(items, "  "), width)
}

type conversationOptions struct {
	showReasoning     bool
	expandedTool      string
	selectedTool      string
	permissionReady   bool
	permissionBlocked string
}

func renderConversation(projection *transcript, agent string, width int, options conversationOptions) []string {
	if projection == nil || len(projection.requests) == 0 {
		return []string{"", centerLine("No conversation yet", width), centerLine("Messages and agent work will appear here live.", width)}
	}
	contentWidth := max(20, width-4)
	var lines []string
	if projection.clipped {
		lines = append(lines, "… earlier conversation activity omitted from this bounded terminal view", "")
	}
	for requestIndex, request := range projection.requests {
		if requestIndex > 0 {
			lines = append(lines, "", sectionRule(width))
		}
		completedResponse := request.completedResponse()
		for _, block := range request.blocks {
			switch block.kind {
			case blockUser:
				lines = append(lines, "❯ YOU")
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
			case blockAssistant:
				if block == completedResponse {
					lines = append(lines, "✓ RESPONSE · "+agent+" · complete")
				} else {
					lines = append(lines, "● "+agent+" · update")
				}
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
				if block.truncated {
					lines = append(lines, "  … earlier content omitted; the newest output is preserved")
				}
			case blockThought:
				lines = append(lines, renderThought(block, contentWidth, options.showReasoning)...)
			case blockTool:
				lines = append(lines, renderTool(block.tool, width, options)...)
			case blockPermission:
				lines = append(lines, renderPermission(block.permission, width, options.permissionReady, options.permissionBlocked)...)
			case blockError:
				lines = append(lines, "! ERROR")
				lines = append(lines, indentWrapped(block.text, contentWidth, "  ")...)
			}
		}
		if request.state == turnComplete && !request.reasoningSeen {
			lines = append(lines, "  ◇ No separate reasoning summary was available for this turn.")
		} else if request.state == turnComplete && request.reasoningSeen && !request.hasRetainedReasoning() {
			lines = append(lines, "  ◇ Earlier reasoning is outside this bounded view.")
		}
		if request.state == turnComplete && !request.assistantSeen {
			lines = append(lines, "  ! Turn completed without a visible response; inspect channel diagnostics for more detail.")
		} else if request.state == turnComplete && request.completedResponse() == nil {
			lines = append(lines, "  ! Assistant output is outside this bounded view; inspect the local Session log.")
		}
		if request.contentClipped {
			lines = append(lines, "  ◇ Older detail in this turn is hidden to keep the console responsive.")
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
		return []string{fmt.Sprintf("  ▸ Reasoning · %d line(s) · o expand", count)}
	}
	lines := []string{"  ▾ Reasoning"}
	wrapped := indentWrapped(block.text, width, "  │ ")
	if len(wrapped) > 10 {
		wrapped = append(wrapped[:10], "  ╰ … reasoning clipped in this view")
	}
	if block.truncated {
		wrapped = append(wrapped, "  ╰ … earlier reasoning omitted; the newest reasoning is preserved")
	}
	return append(lines, wrapped...)
}

func renderTool(tool *toolCard, width int, options conversationOptions) []string {
	if tool == nil {
		return nil
	}
	selected := ""
	if options.selectedTool == tool.id {
		selected = " · selected"
	}
	title := "  ├─ " + toolStatusGlyph(tool.status) + " TOOL · " + toolDisplayTitle(tool) + selected
	lines := []string{fitLine(title, width)}
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
	lines = append(lines, "  │  "+fitLine(strings.Join(metadata, " · "), max(1, width-5)))
	if tool.preview != "" {
		preview := indentWrapped(tool.preview, max(10, width-7), "  │  ")
		if len(preview) > 3 {
			preview = preview[:3]
		}
		lines = append(lines, preview...)
	}
	if options.expandedTool == tool.id {
		detail := indentWrapped(tool.detail, max(10, width-7), "  │  ")
		if len(detail) > 6 {
			detail = append(detail[:6], "  │  … details clipped; press r for raw channel frames")
		}
		if len(detail) == 0 {
			detail = []string{"  │  No provider output was attached to this tool call."}
		}
		lines = append(lines, detail...)
	}
	hint := "d details"
	if options.expandedTool == tool.id {
		hint = "d collapse"
	}
	lines = append(lines, "  ╰─ "+hint+" · [/] select")
	return lines
}

func renderPermission(card *permissionCard, width int, decisionsReady bool, blockedReason string) []string {
	if card == nil {
		return nil
	}
	title := valueOr(card.title, "Tool permission")
	if card.state != "waiting" {
		return []string{
			"  ├─ ✓ Approval · " + fitLine(title, max(1, width-19)),
			"  ╰─ resolved · " + valueOr(card.decision, "decision recorded"),
		}
	}
	lines := []string{"  ╭─ APPROVAL REQUIRED · " + fitLine(title, max(1, width-29))}
	if !decisionsReady {
		lines = append(lines, "  │  Decision unavailable · "+valueOr(blockedReason, "refresh the live Session before choosing"))
		for _, option := range card.options {
			label := valueOr(option.name, strings.ReplaceAll(option.kind, "_", " "))
			lines = append(lines, "  │  offered · "+label)
		}
		return append(lines, "  ╰─ this retained card cannot be submitted yet")
	}
	lines = append(lines, "  │  The agent is waiting; detach safely leaves this request open.")
	for index, option := range card.options {
		label := valueOr(option.name, strings.ReplaceAll(option.kind, "_", " "))
		if option.kind == "allow_always" || option.kind == "reject_always" {
			label += " · PERSISTENT"
		}
		lines = append(lines, fmt.Sprintf("  │  [%d] %s", index+1, label))
	}
	lines = append(lines, "  ╰─ choose an offered option")
	return lines
}

func renderTurnState(state turnState) string {
	switch state {
	case turnReady:
		return "  ◌ Message accepted"
	case turnLive:
		return "  ● Agent is working…"
	case turnCancelling:
		return "  ◌ Cancellation requested…"
	case turnCancelled:
		return "  × Turn cancelled · Session remains available"
	case turnFailed:
		return "  ! Turn failed"
	case turnComplete:
		return "  ✓ Turn complete"
	case turnIncomplete:
		return "  ◇ Provider turn incomplete · no final response was recorded"
	default:
		return ""
	}
}

func renderRawHistory(history []retainedChannelLine, width int) []string {
	if len(history) == 0 {
		return []string{"No raw Session history yet."}
	}
	var lines []string
	for _, retained := range history {
		line := retained.line
		raw := sanitizeDisplay(render.Activity(line, true))
		wrapped := ansi.Hardwrap(raw, max(10, width), true)
		lines = append(lines, strings.Split(wrapped, "\n")...)
	}
	return lines
}

func renderComposer(composer composerState, agent string, width int) []string {
	title := "MESSAGE " + agent
	if composer.focused {
		title += " · editing"
	} else if len(composer.value) > 0 {
		title += " · draft saved"
	}
	content := composer.text()
	if composer.focused {
		cursor := min(max(0, composer.cursor), len(composer.value))
		content = string(composer.value[:cursor]) + "▌" + string(composer.value[cursor:])
	}
	if content == "" {
		content = "Press p to write a message…"
	}
	wrapped := indentWrapped(content, max(10, width-5), "│  ")
	if len(wrapped) > 2 {
		wrapped = wrapped[len(wrapped)-2:]
	}
	for len(wrapped) < 2 {
		wrapped = append(wrapped, "│  ")
	}
	return []string{
		composerTop(title, width),
		fitLine(wrapped[0], width),
		fitLine(wrapped[1], width),
		composerBottom(composer.focused, width),
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
		footer := index == len(lines)-1
		if footer {
			lines[index] = padRight(lines[index], width)
		}
		lines[index] = decorateLine(lines[index], footer)
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

func topBar(left, right string, width int) string {
	left = "◆ " + safeText(left)
	if strings.TrimSpace(right) == "" {
		return fitLine(left, width)
	}
	return fitWithRight(left, safeText(right), width)
}

func horizontalRule(width int) string {
	if width <= 0 {
		return ""
	}
	return strings.Repeat("─", width)
}

func sectionHeading(left, right string, width int) string {
	left = "  " + safeText(left)
	if strings.TrimSpace(right) == "" {
		return fitLine(left, width)
	}
	return fitWithRight(left, safeText(right), width)
}

func sectionRule(width int) string {
	if width <= 0 {
		return ""
	}
	return strings.Repeat("─", width)
}

func composerTop(title string, width int) string {
	if width < 5 {
		return horizontalRule(width)
	}
	prefix := "╭─ " + safeText(title) + " "
	prefix = ansi.Truncate(prefix, width-1, "…")
	return fitLine(prefix+strings.Repeat("─", max(1, width-ansi.StringWidth(prefix)-1))+"╮", width)
}

func composerBottom(focused bool, width int) string {
	if width < 5 {
		return horizontalRule(width)
	}
	hint := "p compose"
	if focused {
		hint = "Enter send · Ctrl+J newline · Esc keep draft"
	}
	prefix := "╰─ " + hint + " "
	prefix = ansi.Truncate(prefix, width-1, "…")
	return fitLine(prefix+strings.Repeat("─", max(1, width-ansi.StringWidth(prefix)-1))+"╯", width)
}

func fitWithRight(left, right string, width int) string {
	rightWidth := ansi.StringWidth(right)
	if rightWidth+2 >= width {
		return fitLine(right, width)
	}
	left = ansi.Truncate(left, width-rightWidth-1, "…")
	return left + strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-rightWidth)) + right
}

func fitSplitMetadata(left, right string, width int) string {
	if width <= 0 {
		return ""
	}
	left = safeText(left)
	right = safeText(right)
	leftBudget := min(ansi.StringWidth(left), max(28, width*45/100))
	rightBudget := max(1, width-leftBudget-2)
	left = ansi.Truncate(left, leftBudget, "…")
	right = ansi.Truncate(right, rightBudget, "…")
	return fitWithRight(left, right, width)
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
	label := fmt.Sprintf("%d Sessions", sessions)
	if sessions == 1 {
		label = "1 Session"
	}
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
		if !unicode.IsPrint(r) {
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

func humanizeLabel(value string) string {
	return strings.ReplaceAll(safeText(value), "-", " ")
}
