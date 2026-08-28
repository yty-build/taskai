package tui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type mouseAction uint8

const (
	mouseNone mouseAction = iota
	mouseHelp
	mouseBack
	mouseTask
	mouseTaskActive
	mouseTaskAll
	mouseTaskOpen
	mouseAddTask
	mouseSubmitTask
	mouseSettings
	mouseProvider
	mouseProviderEnable
	mouseProviderAuth
	mouseProviderRecheck
	mouseManager
	mouseInput
	mouseSend
	mouseResume
	mouseAddManager
	mouseStop
	mouseConfirmStop
	mouseCancelStop
	mouseAddProvider
	mouseLabel
	mouseCreateManager
	mouseRouteSettings
	mouseDetails
	mouseDetailsPrev
	mouseDetailsNext
	mouseDetailsClose
)

type hitbox struct {
	x1, x2 int
	y1, y2 int
	action mouseAction
	index  int
}

func (h hitbox) contains(x, y int) bool {
	return x >= h.x1 && x < h.x2 && y >= h.y1 && y < h.y2
}

type rectangle struct{ x1, x2, y1, y2 int }

func (r rectangle) contains(x, y int) bool {
	return x >= r.x1 && x < r.x2 && y >= r.y1 && y < r.y2
}

type renderResult struct {
	content string
	cursor  *tea.Cursor
	hits    []hitbox
	pane    rectangle
}

type canvas struct {
	width  int
	height int
	lines  []string
	hits   []hitbox
	cursor *tea.Cursor
	pane   rectangle
}

func newCanvas(width, height int) *canvas {
	width = max(1, width)
	height = max(1, height)
	return &canvas{width: width, height: height, lines: make([]string, height)}
}

func (c *canvas) line(y int, value string) {
	if y < 0 || y >= c.height {
		return
	}
	c.lines[y] = truncateCells(value, c.width)
}

func (c *canvas) hit(y, x, width int, action mouseAction, index int) {
	if y < 0 || y >= c.height || width <= 0 {
		return
	}
	c.hits = append(c.hits, hitbox{
		x1: clamp(x, 0, c.width), x2: clamp(x+width, 0, c.width),
		y1: y, y2: y + 1, action: action, index: index,
	})
}

func (c *canvas) buttons(y int, buttons ...button) {
	var line strings.Builder
	x := 0
	for i, button := range buttons {
		if i > 0 {
			line.WriteByte(' ')
			x++
		}
		label := "[" + button.label + "]"
		line.WriteString(label)
		c.hit(y, x, lipgloss.Width(label), button.action, button.index)
		x += lipgloss.Width(label)
	}
	c.line(y, line.String())
}

func (c *canvas) result() renderResult {
	lines := make([]string, len(c.lines))
	for i, line := range c.lines {
		lines[i] = truncateCells(line, c.width)
	}
	return renderResult{
		content: strings.Join(lines, "\n"), cursor: c.cursor,
		hits: append([]hitbox(nil), c.hits...), pane: c.pane,
	}
}

type button struct {
	label  string
	action mouseAction
	index  int
}

// View renders a bounded alternate-screen Bubble Tea v2 view.
func (m *Model) View() tea.View {
	rendered := m.render()
	view := tea.NewView(rendered.content)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.Cursor = rendered.cursor
	view.OnMouse = func(message tea.MouseMsg) tea.Cmd {
		mouse := message.Mouse()
		switch message.(type) {
		case tea.MouseClickMsg:
			if mouse.Button != tea.MouseLeft {
				return nil
			}
			for i := len(rendered.hits) - 1; i >= 0; i-- {
				hit := rendered.hits[i]
				if hit.contains(mouse.X, mouse.Y) {
					return func() tea.Msg { return mouseActionMsg{action: hit.action, index: hit.index} }
				}
			}
		case tea.MouseWheelMsg:
			if !rendered.pane.contains(mouse.X, mouse.Y) {
				return nil
			}
			switch mouse.Button {
			case tea.MouseWheelUp:
				return func() tea.Msg { return mouseWheelMsg{delta: 3} }
			case tea.MouseWheelDown:
				return func() tea.Msg { return mouseWheelMsg{delta: -3} }
			}
		}
		return nil
	}
	return view
}

func (m *Model) render() renderResult {
	c := newCanvas(m.width, m.height)
	if m.help {
		m.renderHelp(c)
		return c.result()
	}
	if m.details {
		m.renderSessionDetails(c)
		return c.result()
	}
	switch m.screen {
	case screenTasks:
		m.renderTasks(c)
	case screenAddTask:
		m.renderAddTask(c)
	case screenSettings:
		m.renderSettings(c)
	case screenWorkspace:
		m.renderWorkspace(c)
	case screenAddManager:
		m.renderAddManager(c)
	}
	if m.confirmStop {
		m.renderStopConfirm(c)
	}
	return c.result()
}

func (m *Model) renderTasks(c *canvas) {
	c.line(0, "TaskAI / Tasks")
	if !m.loaded {
		c.line(1, "Loading metadata…")
	} else if m.status != "" {
		c.line(1, m.status)
	}
	activeLabel, allLabel := "[Active]", "All"
	if m.filter == filterAll {
		activeLabel, allLabel = "Active", "[All]"
	}
	tabs := "Tasks  " + activeLabel + "  " + allLabel
	c.line(2, tabs)
	activeX := strings.Index(tabs, activeLabel)
	allX := strings.Index(tabs, allLabel)
	c.hit(2, activeX, lipgloss.Width(activeLabel), mouseTaskActive, 0)
	c.hit(2, allX, lipgloss.Width(allLabel), mouseTaskAll, 0)
	c.line(3, labeledColumns("", "Created ↓ / Task", "Active Dormant", c.width))

	footer := c.height - 2
	listEnd := max(4, footer)
	tasks := m.visibleTasks()
	if !m.loaded {
		// Loading is its own truthful state. Empty-task and provider setup copy
		// requires a completed metadata snapshot.
	} else if len(tasks) == 0 {
		if m.filter == filterActive && len(m.snapshot.Tasks) > 0 {
			c.line(4, "No active Tasks. Choose All for history.")
		} else {
			c.line(4, "No Tasks yet. Add Task to begin.")
		}
	} else {
		maxRows := max(0, listEnd-4)
		start := selectedWindow(indexTask(tasks, m.selectedTask), len(tasks), maxRows)
		for row, task := range tasks[start:min(len(tasks), start+maxRows)] {
			index := start + row
			active, dormant, _ := m.taskCounts(task.ID)
			marker := "  "
			if task.ID == m.selectedTask {
				marker = "> "
			}
			title := safeInlinePrefix(task.Title, 1024)
			created := ""
			if !task.CreatedAt.IsZero() {
				switch {
				case c.width < 50:
					created = task.CreatedAt.UTC().Format("01-02") + " "
				case c.width < 80:
					created = task.CreatedAt.UTC().Format("2006-01-02") + " "
				default:
					created = task.CreatedAt.UTC().Format("2006-01-02 15:04") + " "
				}
			}
			line := labeledColumns(marker+created, title, fmt.Sprintf("A:%d D:%d", active, dormant), c.width)
			y := 4 + row
			c.line(y, line)
			c.hit(y, 0, c.width, mouseTask, index)
		}
	}
	if m.loaded && !m.providerEnabled && footer-1 >= 4+min(len(tasks), max(0, listEnd-4)) {
		c.line(footer-1, "No providers enabled; setup is explicit in Settings.")
	}
	c.buttons(footer,
		button{"Open", mouseTaskOpen, 0},
		button{"Add Task", mouseAddTask, 0},
		button{"Settings", mouseSettings, 0},
		button{"Help", mouseHelp, 0},
	)
	c.line(c.height-1, "↑/↓ select  Tab Active/All  Enter open  q quit")
}

func (m *Model) renderAddTask(c *canvas) {
	c.line(0, "TaskAI / Tasks / Add Task")
	c.line(2, "Title is preserved exactly after bounded validation.")
	value, cursor := editorView(m.taskDraft, max(1, c.width-2))
	c.line(4, "> "+value)
	if m.pending == actionAddTask {
		c.line(6, "Adding Task…")
	} else if m.status != "" {
		c.line(6, m.status)
	}
	if m.pending == actionNone {
		c.cursor = tea.NewCursor(min(c.width-1, 2+cursor), 4)
	}
	c.buttons(c.height-2,
		button{"Back", mouseBack, 0},
		button{"Add Task", mouseSubmitTask, 0},
		button{"Help", mouseHelp, 0},
	)
	c.line(c.height-1, "Enter add  Esc back")
}

func (m *Model) renderSettings(c *canvas) {
	c.line(0, "TaskAI / Provider Settings")
	if m.status != "" {
		c.line(1, m.status)
	} else if m.pending != actionNone {
		c.line(1, actionLabel(m.pending)+"…")
	} else {
		c.line(1, "Enable, authenticate, and recheck are explicit actions only.")
	}
	c.line(2, strings.Repeat("─", c.width))
	providers := m.snapshot.Providers
	footer := c.height - 2
	detailLines := 3
	listRows := max(0, footer-3-detailLines)
	if len(providers) == 0 {
		c.line(3, "No provider connectors registered.")
	} else {
		start := selectedWindow(indexProvider(providers, m.selectedProvider), len(providers), listRows)
		for row, provider := range providers[start:min(len(providers), start+listRows)] {
			index := start + row
			marker := "  "
			if provider.ID == m.selectedProvider {
				marker = "> "
			}
			enabled := "disabled"
			if provider.Enabled {
				enabled = "enabled"
			}
			line := labeledColumns(marker, safeInlinePrefix(provider.Name, 512),
				enabled+" | status:"+probeStatusLabel(provider.ProbeStatus), c.width)
			y := 3 + row
			c.line(y, line)
			c.hit(y, 0, c.width, mouseProvider, index)
		}
		if provider, ok := m.selectedProviderValue(); ok {
			y := max(3, footer-detailLines)
			caps := provider.Capabilities
			c.line(y, "Capabilities are connector-conformance claims:")
			if c.width < 60 {
				c.line(y+1, fmt.Sprintf("Manager:%s  Subordinate:%s", yesNo(caps.Manager), yesNo(caps.Subordinate)))
				c.line(y+2, fmt.Sprintf("Resume:%s  Replay:%s", yesNo(caps.Resume), yesNo(caps.Replay)))
			} else {
				c.line(y+1, fmt.Sprintf("Manager:%s  Subordinate:%s  Resume:%s  Replay:%s",
					yesNo(caps.Manager), yesNo(caps.Subordinate), yesNo(caps.Resume), yesNo(caps.Replay)))
			}
			if m.status != "" && c.width >= 60 {
				c.line(y+2, m.status)
			}
		}
	}
	provider, selected := m.selectedProviderValue()
	if !selected {
		c.buttons(footer, button{"Back", mouseBack, 0}, button{"Help", mouseHelp, 0})
		c.line(c.height-1, "No provider selected; Enable, Authenticate, and Recheck unavailable.")
		return
	}
	toggle := "Enable"
	if provider.Enabled {
		toggle = "Disable"
	}
	if c.width < 60 {
		c.buttons(footer,
			button{"Back", mouseBack, 0}, button{toggle, mouseProviderEnable, 0},
			button{"Auth", mouseProviderAuth, 0}, button{"Check", mouseProviderRecheck, 0},
			button{"?", mouseHelp, 0},
		)
	} else {
		c.buttons(footer,
			button{"Back", mouseBack, 0}, button{toggle, mouseProviderEnable, 0},
			button{"Authenticate", mouseProviderAuth, 0}, button{"Recheck", mouseProviderRecheck, 0},
			button{"Help", mouseHelp, 0},
		)
	}
	c.line(c.height-1, "↑/↓ select  Space enable/disable  a authenticate  r recheck")
}

func (m *Model) renderWorkspace(c *canvas) {
	task, _ := m.task(m.currentTask)
	c.line(0, "TaskAI / Task / "+safeInlinePrefix(task.Title, 1024))
	active, dormant, _ := m.taskCounts(m.currentTask)
	geometry := workspaceGeometry(c.width, c.height)
	manager, selected := m.session(m.selectedManager)
	if selected && manager.Role == RoleManager && manager.TaskID == m.currentTask {
		label := sessionLabel(manager)
		provider := m.providerLabel(manager.ProviderID)
		children := len(m.childrenByManager[manager.ID])
		state := sessionStateLabel(manager)
		if geometry.wide {
			c.line(1, fmt.Sprintf("Manager %s | %s | %s | %s | subordinates:%d",
				safeInlinePrefix(string(manager.ID), maxIDBytes), label, provider, state, children))
			c.line(2, fmt.Sprintf("Sessions active:%d dormant:%d", active, dormant))
		} else {
			c.line(1, fmt.Sprintf("Manager %s | %s", safeInlinePrefix(string(manager.ID), maxIDBytes), label))
			c.line(2, fmt.Sprintf("%s | %s | sub:%d | A:%d D:%d", provider, state, children, active, dormant))
		}
	} else {
		c.line(1, "Selected Manager pane: unavailable")
		c.line(2, fmt.Sprintf("Sessions active:%d dormant:%d", active, dormant))
	}
	tree := m.treeLines()
	paneLines := m.selectedPaneLines(max(1, geometry.paneWidth))

	if geometry.wide {
		for row := 0; row < geometry.mainHeight; row++ {
			left := ""
			if row < len(tree) {
				left = truncateCells(tree[row].text, geometry.treeWidth)
				if tree[row].managerIndex >= 0 {
					c.hit(geometry.mainY+row, 0, geometry.treeWidth, mouseManager, tree[row].lineIndex)
				}
			}
			right := ""
			if row < len(paneLines) {
				right = paneLines[row]
			}
			line := padCells(left, geometry.treeWidth) + " │ " + right
			c.line(geometry.mainY+row, line)
		}
		c.pane = rectangle{geometry.treeWidth + 3, c.width, geometry.mainY, geometry.mainY + geometry.mainHeight}
	} else {
		c.line(geometry.treeY-1, "Session hierarchy")
		for row := 0; row < geometry.treeHeight && row < len(tree); row++ {
			c.line(geometry.treeY+row, tree[row].text)
			if tree[row].managerIndex >= 0 {
				c.hit(geometry.treeY+row, 0, c.width, mouseManager, tree[row].lineIndex)
			}
		}
		c.line(geometry.paneY-1, "Selected Manager public ACP")
		for row := 0; row < geometry.paneHeight && row < len(paneLines); row++ {
			c.line(geometry.paneY+row, paneLines[row])
		}
		c.pane = rectangle{0, c.width, geometry.paneY, geometry.paneY + geometry.paneHeight}
	}

	m.renderManagerInput(c, geometry.inputY)
	if m.status != "" && geometry.inputY-1 >= 2 {
		c.line(geometry.inputY-1, m.status)
	}
	if c.width < 60 {
		c.buttons(c.height-2,
			button{"Back", mouseBack, 0}, button{"Add", mouseAddManager, 0},
			button{"Info", mouseDetails, 0}, button{"Stop", mouseStop, 0}, button{"?", mouseHelp, 0},
		)
	} else {
		c.buttons(c.height-2,
			button{"Back/Detach", mouseBack, 0}, button{"Add Manager", mouseAddManager, 0},
			button{"Session Details", mouseDetails, 0}, button{"Stop", mouseStop, 0}, button{"Help", mouseHelp, 0},
		)
	}
	c.line(c.height-1, "↑/↓ Manager  v/Enter details  Tab input  PgUp/PgDn scroll  r resume  x stop")
}

type workspaceLayout struct {
	wide                 bool
	mainY, mainHeight    int
	treeWidth, paneWidth int
	treeY, treeHeight    int
	paneY, paneHeight    int
	inputY               int
}

func workspaceGeometry(width, height int) workspaceLayout {
	inputY := max(2, height-4)
	if width >= 72 {
		treeWidth := clamp(width*5/8, 32, 52)
		return workspaceLayout{
			wide: true, mainY: 3, mainHeight: max(0, inputY-3),
			treeWidth: treeWidth, paneWidth: max(1, width-treeWidth-3), inputY: inputY,
		}
	}
	treeY := 4
	available := max(0, inputY-treeY)
	treeHeight := min(4, max(1, (available-1)/2))
	paneY := treeY + treeHeight + 1
	paneHeight := max(0, inputY-paneY)
	return workspaceLayout{
		treeY: treeY, treeHeight: treeHeight,
		paneY: paneY, paneHeight: paneHeight,
		paneWidth: width, inputY: inputY,
	}
}

func (m *Model) paneHeight() int {
	geometry := workspaceGeometry(m.width, m.height)
	if geometry.wide {
		return geometry.mainHeight
	}
	return geometry.paneHeight
}

type treeLine struct {
	text         string
	managerIndex int
	sessionID    SessionID
	lineIndex    int
}

func (m *Model) allTreeLines() []treeLine {
	nodes := m.treeNodesByTask[m.currentTask]
	if len(nodes) == 0 {
		return []treeLine{{text: "No Manager Sessions. Add Manager.", managerIndex: -1}}
	}
	lines := make([]treeLine, 0, len(nodes))
	for index, node := range nodes {
		if line, ok := m.formatTreeNode(node, index); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

func (m *Model) treeLines() []treeLine {
	nodes := m.treeNodesByTask[m.currentTask]
	if len(nodes) == 0 {
		return []treeLine{{text: "No Manager Sessions. Add Manager.", managerIndex: -1}}
	}
	start := clamp(m.treeOffset, 0, max(0, len(nodes)-1))
	end := min(len(nodes), start+max(1, m.treeVisibleHeight()))
	lines := make([]treeLine, 0, end-start)
	for index := start; index < end; index++ {
		if line, ok := m.formatTreeNode(nodes[index], index); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

func (m *Model) formatTreeNode(node treeNode, lineIndex int) (treeLine, bool) {
	session, ok := m.session(node.sessionID)
	if !ok {
		return treeLine{}, false
	}
	if session.Role == RoleManager {
		marker := "  "
		if session.ID == m.selectedManager {
			marker = "> "
		}
		return treeLine{
			text: fmt.Sprintf("%sManager %s %s %s", marker,
				sessionStateLabel(session), m.providerLabel(session.ProviderID), sessionLabel(session)),
			managerIndex: node.managerIndex, sessionID: session.ID, lineIndex: lineIndex,
		}, true
	}
	return treeLine{
		text: fmt.Sprintf("  └ Subordinate %s %s %s %s",
			titleCaseMode(session.Mode), sessionStateLabel(session), m.providerLabel(session.ProviderID), sessionLabel(session)),
		managerIndex: node.managerIndex, sessionID: session.ID, lineIndex: lineIndex,
	}, true
}

func (m *Model) selectedPaneLines(width int) []string {
	if m.selectedManager == "" {
		return []string{"No selected Manager; subordinate input is unavailable."}
	}
	p := m.panes[m.selectedManager]
	if p == nil {
		return []string{"No public ACP frames received for this Manager."}
	}
	const evictedMarker = "[older frames evicted]"
	if len(p.frames) == 0 {
		if p.evicted {
			return []string{evictedMarker}
		}
		return []string{"No public ACP frames received for this Manager."}
	}
	var all []string
	for _, frame := range p.frames {
		all = append(all, wrapSafe(frame.text, width)...)
	}
	height := max(1, m.paneHeight())
	contentHeight := height
	if p.evicted {
		contentHeight = max(0, height-1)
	}
	maxOffset := max(0, len(all)-1)
	offset := clamp(p.offsetFromTail, 0, maxOffset)
	end := len(all) - offset
	start := max(0, end-contentHeight)
	visible := append([]string(nil), all[start:end]...)
	if p.evicted {
		visible = append([]string{evictedMarker}, visible...)
	}
	if len(visible) == 0 {
		return []string{" "}
	}
	return visible
}

func (m *Model) renderManagerInput(c *canvas, y int) {
	manager, ok := m.session(m.selectedManager)
	if !ok || manager.Role != RoleManager {
		c.line(y, "Manager input unavailable: select or add a Manager.")
		return
	}
	if manager.State != StateActive {
		if manager.ResumeAvailable && manager.State != StateStarting {
			line := "Exact Session Resume [Resume] | state=" + sessionStateLabel(manager)
			c.line(y, line)
			if x := strings.Index(line, "[Resume]"); x >= 0 {
				c.hit(y, lipgloss.Width(line[:x]), lipgloss.Width("[Resume]"), mouseResume, 0)
			}
		} else {
			c.line(y, "Resume unavailable | Manager state="+sessionStateLabel(manager))
		}
		return
	}
	draft := m.drafts[manager.ID]
	value, cursor := editorView(draft, max(1, c.width-8))
	prefix := "Input> "
	line := prefix + value
	if c.width >= 18 {
		line = padCells(line, c.width-lipgloss.Width("[Send]")) + "[Send]"
	}
	c.line(y, line)
	c.hit(y, 0, c.width, mouseInput, 0)
	if c.width >= 18 {
		label := "[Send]"
		x := max(0, c.width-lipgloss.Width(label))
		c.hit(y, x, lipgloss.Width(label), mouseSend, 0)
	}
	if m.focus == focusInput && m.pending == actionNone {
		c.cursor = tea.NewCursor(min(c.width-1, lipgloss.Width(prefix)+cursor), y)
	}
}

func (m *Model) renderAddManager(c *canvas) {
	c.line(0, "TaskAI / Task / Add Manager")
	providers := m.managerProviders()
	if len(providers) == 0 {
		c.line(2, "No enabled Manager-capable provider is available.")
		c.line(3, "Enable, authenticate if needed, and recheck in Settings.")
		c.line(4, "Subordinate-only providers cannot start a top-level Session.")
		c.buttons(c.height-2,
			button{"Back", mouseBack, 0},
			button{"Open Settings", mouseRouteSettings, 0},
			button{"Help", mouseHelp, 0},
		)
		c.line(c.height-1, "Enter Settings  Esc back")
		return
	}
	c.line(1, "Only enabled providers with a conformed Manager claim are offered.")
	c.line(2, "Subordinate-only providers are delegated by the Manager, not started here.")
	maxRows := max(0, c.height-8)
	start := selectedWindow(indexProvider(providers, m.addProvider), len(providers), maxRows)
	for row, provider := range providers[start:min(len(providers), start+maxRows)] {
		index := start + row
		marker := "  "
		if provider.ID == m.addProvider {
			marker = "> "
		}
		suffix := fmt.Sprintf("M:%s R:%s P:%s | %s", yesNo(provider.Capabilities.Manager),
			yesNo(provider.Capabilities.Resume), yesNo(provider.Capabilities.Replay), probeStatusLabel(provider.ProbeStatus))
		if c.width < 60 {
			suffix = fmt.Sprintf("M:%s R:%s P:%s %s", shortYesNo(provider.Capabilities.Manager),
				shortYesNo(provider.Capabilities.Resume), shortYesNo(provider.Capabilities.Replay), probeStatusLabel(provider.ProbeStatus))
		}
		line := labeledColumns(marker, safeInlinePrefix(provider.Name, 512), suffix, c.width)
		y := 3 + row
		c.line(y, line)
		c.hit(y, 0, c.width, mouseAddProvider, index)
	}
	labelY := max(4, c.height-5)
	value, cursor := editorView(m.managerLabel, max(1, c.width-16))
	c.line(labelY, "Optional label> "+value)
	c.hit(labelY, 0, c.width, mouseLabel, 0)
	if m.focus == focusLabel && m.pending == actionNone {
		c.cursor = tea.NewCursor(min(c.width-1, 16+cursor), labelY)
	}
	if m.status != "" {
		c.line(labelY+1, m.status)
	}
	c.buttons(c.height-2,
		button{"Back", mouseBack, 0},
		button{"Create Manager", mouseCreateManager, 0},
		button{"Help", mouseHelp, 0},
	)
	c.line(c.height-1, "↑/↓ provider  Tab label  Enter create")
}

func (m *Model) renderHelp(c *canvas) {
	c.line(0, "TaskAI / Help")
	c.line(2, "Tasks: ↑/↓ select, Tab Active/All, Enter open, a add, s Settings")
	c.line(3, "Workspace: ↑/↓ Manager, v/Enter exact Session details, Tab input")
	c.line(4, "Lifecycle: Back detaches viewer; Stop is explicit and confirms")
	c.line(5, "Settings: Space enable, a authenticate, r recheck")
	c.line(6, "Historical Manager: r exact Resume only when advertised")
	c.line(7, "Manager input is sent only to the selected top-level Manager.")
	c.line(8, "Subordinates are depth one and never have direct input.")
	c.line(10, "No screen authenticates implicitly or stores displayed content.")
	c.buttons(c.height-2, button{"Close Help", mouseHelp, 0})
	c.line(c.height-1, "? / Esc / Enter close")
}

func (m *Model) renderSessionDetails(c *canvas) {
	sessions := m.detailSessions()
	if len(sessions) == 0 {
		c.line(0, "TaskAI / Session details unavailable")
		return
	}
	index := clamp(m.detailIndex, 0, len(sessions)-1)
	session := sessions[index]
	c.line(0, "TaskAI / Exact Session Details")
	c.line(1, fmt.Sprintf("Session %d of %d in selected Manager tree", index+1, len(sessions)))
	fields := []string{
		"Task ID: " + safeInlinePrefix(string(session.TaskID), maxIDBytes),
		"Session ID: " + safeInlinePrefix(string(session.ID), maxIDBytes),
		"Parent Session ID: " + safeInlinePrefix(string(session.ParentID), maxIDBytes),
		"Role: " + safeInlinePrefix(string(session.Role), 32),
		"Label: " + safeInlinePrefix(session.Label, maxSessionLabelBytes),
		"Provider ID: " + safeInlinePrefix(string(session.ProviderID), maxIDBytes),
		"Provider: " + m.providerLabel(session.ProviderID),
		"Mode: " + safeInlinePrefix(string(session.Mode), 32),
		"State: " + sessionStateLabel(session),
		"Resume available: " + yesNo(session.ResumeAvailable),
		"Created: " + session.CreatedAt.Format("2006-01-02T15:04:05.000000000Z07:00"),
		"Last active: " + session.LastActive.Format("2006-01-02T15:04:05.000000000Z07:00"),
	}
	var lines []string
	for _, field := range fields {
		lines = append(lines, wrapSafe(field, c.width)...)
	}
	rows := max(0, c.height-5)
	maxScroll := max(0, len(lines)-rows)
	scroll := clamp(m.detailScroll, 0, maxScroll)
	for row, line := range lines[scroll:min(len(lines), scroll+rows)] {
		c.line(3+row, line)
	}
	c.buttons(c.height-2,
		button{"Previous", mouseDetailsPrev, 0}, button{"Next", mouseDetailsNext, 0},
		button{"Close", mouseDetailsClose, 0}, button{"Help", mouseHelp, 0},
	)
	c.line(c.height-1, "←/→ Session  ↑/↓ scroll  Esc/Enter close")
}

func (m *Model) renderStopConfirm(c *canvas) {
	y := clamp(c.height/2, 1, c.height-2)
	manager, _ := m.session(m.selectedManager)
	c.line(y-1, strings.Repeat("─", c.width))
	c.line(y, "Selected Manager: "+sessionLabel(manager))
	message := "Stop selected Manager tree? [Yes] [No]"
	y++
	c.line(y, message)
	visible := truncateCells(message, c.width)
	if x := strings.Index(visible, "[Yes]"); x >= 0 {
		c.hit(y, lipgloss.Width(visible[:x]), lipgloss.Width("[Yes]"), mouseConfirmStop, 0)
	}
	if x := strings.Index(visible, "[No]"); x >= 0 {
		c.hit(y, lipgloss.Width(visible[:x]), lipgloss.Width("[No]"), mouseCancelStop, 0)
	}
	c.line(y+1, strings.Repeat("─", c.width))
}

func (m *Model) providerLabel(id ProviderID) string {
	provider, ok := m.provider(id)
	if !ok {
		return safeInlinePrefix(string(id), 256)
	}
	return safeInlinePrefix(provider.Name, 512)
}

func sessionLabel(session Session) string {
	if session.Label != "" {
		return safeInlinePrefix(session.Label, 1024)
	}
	return safeInlinePrefix(string(session.ID), 256)
}

func probeStatusLabel(status ProbeStatus) string {
	switch status {
	case ProbeReady:
		return "Connected"
	case ProbeUnknown:
		return "Not connected"
	case ProbeChecking:
		return "Checking"
	case ProbeDisconnected:
		return "Disconnected"
	case ProbeNeedsRepair:
		return "Needs repair"
	case ProbeMissing:
		return "Missing connector"
	case ProbeUnsupported:
		return "Unsupported"
	default:
		return "Unavailable"
	}
}

func titleCaseMode(mode InvocationMode) string {
	switch mode {
	case ModeAnswer:
		return "Answer"
	case ModeFollow:
		return "Follow"
	default:
		return "Unavailable"
	}
}

func sessionStateLabel(session Session) string {
	switch session.State {
	case StateStarting:
		return "Starting"
	case StateActive:
		return "Working"
	case StateDormant:
		if session.Role == RoleSubordinate && session.Mode == ModeAnswer {
			return "Answered"
		}
		return "Dormant"
	case StateInterrupted:
		return "Interrupted"
	case StateClosed:
		return "Closed"
	default:
		return "Unavailable"
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func selectedWindow(selected, length, rows int) int {
	if rows <= 0 || length <= rows {
		return 0
	}
	if selected < 0 {
		selected = 0
	}
	start := selected - rows/2
	return clamp(start, 0, length-rows)
}

// labeledColumns truncates only the untrusted label and reserves the complete
// mechanical suffix at every supported width.
func labeledColumns(prefix, label, suffix string, width int) string {
	suffixWidth := lipgloss.Width(suffix)
	prefix = truncateCells(prefix, max(0, width-suffixWidth-2))
	separator := " "
	fixed := lipgloss.Width(prefix) + lipgloss.Width(separator) + suffixWidth
	labelWidth := max(1, width-fixed)
	return prefix + padCells(label, labelWidth) + separator + suffix
}

func shortYesNo(value bool) string {
	if value {
		return "Y"
	}
	return "N"
}

func editorView(editor editor, width int) (string, int) {
	if width <= 0 {
		return "", 0
	}
	value := editor.value
	cursor := clamp(editor.cursor, 0, len(value))
	if lipgloss.Width(value) <= width {
		return value, lipgloss.Width(value[:cursor])
	}
	start := 0
	for start < cursor && lipgloss.Width(value[start:cursor]) > width-1 {
		_, size := utf8.DecodeRuneInString(value[start:])
		start += size
	}
	prefix := ""
	if start > 0 {
		prefix = "‹"
	}
	display := prefix + truncateCells(value[start:], width-lipgloss.Width(prefix))
	cursorX := lipgloss.Width(prefix) + lipgloss.Width(value[start:cursor])
	return display, min(cursorX, width-1)
}

func safeInlinePrefix(value string, byteLimit int) string {
	if byteLimit < 0 {
		byteLimit = 0
	}
	data := []byte(value)
	if len(data) > byteLimit {
		data = data[:byteLimit]
	}
	return strings.ReplaceAll(safeBytes(data, false), "\n", "\\n")
}

func safeMultilineBytes(value []byte) string { return safeBytes(value, true) }

func safeBytes(value []byte, multiline bool) string {
	var out strings.Builder
	for len(value) > 0 {
		r, size := utf8.DecodeRune(value)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&out, "\\x%02x", value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		switch {
		case r == '\n' && multiline:
			out.WriteRune('\n')
		case r == '\n':
			out.WriteString("\\n")
		case r == '\t' && multiline:
			out.WriteString("    ")
		case r == '\r' || r == '\t' || unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			if r <= 0xff {
				fmt.Fprintf(&out, "\\x%02x", r)
			} else {
				fmt.Fprintf(&out, "\\u%04x", r)
			}
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func truncateCells(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	limit := width - 1
	var out strings.Builder
	used := 0
	for _, r := range value {
		cells := lipgloss.Width(string(r))
		if used+cells > limit {
			break
		}
		out.WriteRune(r)
		used += cells
	}
	out.WriteRune('…')
	return out.String()
}

func padCells(value string, width int) string {
	value = truncateCells(value, width)
	return value + strings.Repeat(" ", max(0, width-lipgloss.Width(value)))
}

func wrapSafe(value string, width int) []string {
	if width <= 0 {
		return nil
	}
	var output []string
	for _, logical := range strings.Split(value, "\n") {
		if logical == "" {
			output = append(output, "")
			continue
		}
		var line strings.Builder
		cells := 0
		for _, r := range logical {
			runeWidth := lipgloss.Width(string(r))
			if cells > 0 && cells+runeWidth > width {
				output = append(output, line.String())
				line.Reset()
				cells = 0
			}
			line.WriteRune(r)
			cells += runeWidth
		}
		output = append(output, line.String())
	}
	return output
}
