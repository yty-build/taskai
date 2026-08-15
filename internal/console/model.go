package console

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

const refreshInterval = 750 * time.Millisecond

type viewMode uint8

const (
	taskView viewMode = iota
	sessionView
)

type composerState struct {
	focused bool
	value   []rune
	cursor  int
}

func (c composerState) text() string { return string(c.value) }

func (c *composerState) insert(runes ...rune) {
	if len(runes) == 0 {
		return
	}
	if c.cursor < 0 || c.cursor > len(c.value) {
		c.cursor = len(c.value)
	}
	next := make([]rune, 0, len(c.value)+len(runes))
	next = append(next, c.value[:c.cursor]...)
	next = append(next, runes...)
	next = append(next, c.value[c.cursor:]...)
	c.value = next
	c.cursor += len(runes)
}

func (c *composerState) backspace() {
	if c.cursor <= 0 || len(c.value) == 0 {
		return
	}
	c.value = append(c.value[:c.cursor-1], c.value[c.cursor:]...)
	c.cursor--
}

func (c *composerState) deleteWord() {
	for c.cursor > 0 && unicode.IsSpace(c.value[c.cursor-1]) {
		c.backspace()
	}
	for c.cursor > 0 && !unicode.IsSpace(c.value[c.cursor-1]) {
		c.backspace()
	}
}

type confirmation struct {
	kind      string
	sessionID string
	message   string
}

// Model is the Bubble Tea model shared by Task and direct Session attachment.
// It owns only disposable viewer state. Closing it cancels subscriptions, never
// a worker, an ACP turn, or a provider conversation.
type Model struct {
	ctx     context.Context
	backend Backend
	manager *watchManager

	mode     viewMode
	direct   bool
	task     task.Detail
	current  session.Session
	selected int

	histories   map[string][]sessionlog.Line
	projections map[string]*transcript
	cursors     map[string]uint64
	seen        map[string]map[uint64]struct{}

	raw           bool
	showReasoning bool
	expandedTool  string
	selectedTool  string
	composer      composerState
	confirm       *confirmation
	wizard        *sessionWizard
	notice        string
	scroll        int
	unread        int
	width         int
	height        int
	refreshing    bool
}

type refreshTickMsg struct{}

type refreshMsg struct {
	detail  *task.Detail
	current *session.Session
	lines   map[string][]sessionlog.Line
	err     error
}

type actionMsg struct {
	kind      string
	sessionID string
	accepted  worker.Message
	current   session.Session
	err       error
}

func newTaskModel(parent context.Context, backend Backend, taskID string) (Model, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(parent)
	detail, err := backend.Task(ctx, taskID)
	if err != nil {
		cancel()
		return Model{}, func() {}, err
	}
	m := baseModel(ctx, backend)
	m.mode = taskView
	m.task = detail
	for _, current := range detail.Sessions {
		if err := m.loadStored(current); err != nil {
			cancel()
			m.manager.cancel()
			return Model{}, func() {}, err
		}
	}
	m.watchSessions(detail.Sessions)
	return m, func() { m.manager.cancel(); cancel() }, nil
}

func newSessionModel(parent context.Context, backend Backend, sessionID string) (Model, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(parent)
	current, err := backend.Session(ctx, sessionID)
	if err != nil {
		cancel()
		return Model{}, func() {}, err
	}
	m := baseModel(ctx, backend)
	m.mode = sessionView
	m.direct = true
	m.current = current
	if err := m.loadStored(current); err != nil {
		cancel()
		m.manager.cancel()
		return Model{}, func() {}, err
	}
	m.manager.ensure(current, m.cursors[current.ID])
	return m, func() { m.manager.cancel(); cancel() }, nil
}

func baseModel(ctx context.Context, backend Backend) Model {
	return Model{
		ctx: ctx, backend: backend, manager: newWatchManager(ctx, backend),
		histories: make(map[string][]sessionlog.Line), projections: make(map[string]*transcript),
		cursors: make(map[string]uint64), seen: make(map[string]map[uint64]struct{}),
		width: 100, height: 30,
	}
}

func (m *Model) loadStored(current session.Session) error {
	lines, err := m.backend.History(m.ctx, current.ID, 0)
	if err != nil {
		return fmt.Errorf("replay Session %s history: %w", current.ID, err)
	}
	for _, line := range lines {
		m.addActivity(current.ID, line, false)
	}
	return nil
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.manager.wait(), refreshTickCmd())
}

func refreshTickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		return m, nil
	case refreshTickMsg:
		commands := []tea.Cmd{refreshTickCmd()}
		if !m.refreshing {
			m.refreshing = true
			commands = append(commands, m.refreshCmd())
		}
		return m, tea.Batch(commands...)
	case refreshMsg:
		m.refreshing = false
		if message.err != nil {
			m.notice = "Refresh failed: " + message.err.Error()
			return m, nil
		}
		if message.detail != nil {
			m.task = *message.detail
			if m.selected >= len(m.task.Sessions) {
				m.selected = max(0, len(m.task.Sessions)-1)
			}
			if m.mode == sessionView && !m.direct {
				m.syncCurrentFromTask()
			}
			m.watchSessions(m.task.Sessions)
		}
		if message.current != nil {
			m.current = *message.current
			m.manager.ensure(m.current, m.cursors[m.current.ID])
		}
		for sessionID, lines := range message.lines {
			for _, line := range lines {
				m.addActivity(sessionID, line, false)
			}
		}
		return m, nil
	case activityMsg:
		m.addActivity(message.sessionID, message.line, true)
		return m, m.manager.wait()
	case consoleErrorMsg:
		m.notice = "Live stream: " + message.err.Error()
		return m, m.manager.wait()
	case managerClosedMsg:
		return m, nil
	case actionMsg:
		return m.handleAction(message)
	case wizardReadyMsg:
		return m.handleWizardReady(message)
	case sessionStartedMsg:
		return m.handleSessionStarted(message)
	case tea.MouseMsg:
		return m.handleMouse(tea.MouseEvent(message))
	case tea.KeyMsg:
		return m.handleKey(message)
	}
	return m, nil
}

func (m Model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	name := key.String()
	if name == "ctrl+c" {
		return m, tea.Quit
	}
	if m.wizard != nil {
		return m.handleWizardKey(key)
	}
	if m.confirm != nil {
		return m.handleConfirmation(name)
	}
	if m.composer.focused {
		return m.handleComposer(key)
	}
	if name == "q" {
		return m, tea.Quit
	}

	if m.mode == taskView {
		switch name {
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected+1 < len(m.task.Sessions) {
				m.selected++
			}
		case "enter":
			return m.openSelected(false)
		case "p":
			return m.openSelected(true)
		case "c", "esc":
			return m.confirmSelected("cancel")
		case "u":
			return m.selectedAction("resume")
		case "x", "s":
			return m.confirmSelected("stop")
		case "n":
			m.notice = "Loading agent profiles…"
			return m, loadWizardCmd(m.ctx, m.backend, m.defaultSessionRepo())
		}
		return m, nil
	}

	switch name {
	case "b", "backspace":
		if !m.direct {
			m.mode = taskView
			m.scroll, m.unread = 0, 0
			m.confirm = nil
		}
	case "p", "i", "enter":
		m.composer.focused = true
		m.composer.cursor = len(m.composer.value)
		m.notice = "Enter sends · Ctrl+J adds a line · Esc keeps the draft"
	case "esc", "c":
		return m.confirmCurrent("cancel")
	case "u":
		return m, sessionActionCmd(m.ctx, m.backend, "resume", m.current.ID)
	case "x", "s":
		return m.confirmCurrent("stop")
	case "r":
		m.raw = !m.raw
		m.scroll, m.unread = 0, 0
		if m.raw {
			m.notice = "Raw ACP/debug view enabled. Press r to return to the conversation."
		} else {
			m.notice = "Conversation view restored."
		}
	case "o":
		m.showReasoning = !m.showReasoning
		m.scroll = 0
	case "d":
		m.toggleToolDetail()
	case "[":
		m.selectAdjacentTool(-1)
	case "]":
		m.selectAdjacentTool(1)
	case "up", "k":
		m.scroll += 1
	case "pgup":
		m.scroll += max(4, m.height/2)
	case "home", "g":
		m.scroll = 1 << 30
	case "down", "j":
		m.scroll = max(0, m.scroll-1)
		if m.scroll == 0 {
			m.unread = 0
		}
	case "pgdown":
		m.scroll = max(0, m.scroll-max(4, m.height/2))
		if m.scroll == 0 {
			m.unread = 0
		}
	case "end", "G":
		m.scroll, m.unread = 0, 0
	}
	return m, nil
}

func (m Model) defaultSessionRepo() string {
	if len(m.task.Sessions) > 0 {
		index := min(max(0, m.selected), len(m.task.Sessions)-1)
		if repo := strings.TrimSpace(m.task.Sessions[index].RepoPath); repo != "" {
			return repo
		}
	}
	return "."
}

func (m Model) handleComposer(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	name := key.String()
	switch name {
	case "esc":
		m.composer.focused = false
		m.notice = "Draft preserved. Press p to continue editing."
		return m, nil
	case "enter":
		message := m.composer.text()
		if strings.TrimSpace(message) == "" {
			m.notice = "Message cannot be empty."
			return m, nil
		}
		m.composer = composerState{}
		m.notice = "Submitting message…"
		return m, promptCmd(m.ctx, m.backend, m.current.ID, message)
	case "ctrl+j":
		m.composer.insert('\n')
	case "backspace", "ctrl+h":
		m.composer.backspace()
	case "ctrl+w":
		m.composer.deleteWord()
	case "left":
		m.composer.cursor = max(0, m.composer.cursor-1)
	case "right":
		m.composer.cursor = min(len(m.composer.value), m.composer.cursor+1)
	case "home", "ctrl+a":
		m.composer.cursor = 0
	case "end", "ctrl+e":
		m.composer.cursor = len(m.composer.value)
	default:
		if key.Type == tea.KeySpace {
			m.composer.insert(' ')
		} else if key.Type == tea.KeyRunes {
			m.composer.insert(key.Runes...)
		}
	}
	return m, nil
}

func (m Model) handleConfirmation(name string) (tea.Model, tea.Cmd) {
	confirm := m.confirm
	switch name {
	case "y", "Y":
		m.confirm = nil
		m.notice = actionProgress(confirm.kind, confirm.sessionID)
		return m, sessionActionCmd(m.ctx, m.backend, confirm.kind, confirm.sessionID)
	case "n", "N", "esc", "enter":
		m.confirm = nil
		m.notice = actionLabel(confirm.kind) + " dismissed; the Session is unchanged."
	}
	return m, nil
}

func (m Model) handleMouse(event tea.MouseEvent) (tea.Model, tea.Cmd) {
	if event.Button == tea.MouseButtonWheelUp {
		m.scroll += 3
		return m, nil
	}
	if event.Button == tea.MouseButtonWheelDown {
		m.scroll = max(0, m.scroll-3)
		if m.scroll == 0 {
			m.unread = 0
		}
		return m, nil
	}
	if m.mode != taskView || event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft {
		return m, nil
	}
	const firstSessionRow = 5
	index := (event.Y - firstSessionRow) / 3
	if event.Y < firstSessionRow || index < 0 || index >= len(m.task.Sessions) {
		return m, nil
	}
	m.selected = index
	return m.openSelected(false)
}

func (m Model) openSelected(prompt bool) (tea.Model, tea.Cmd) {
	if len(m.task.Sessions) == 0 {
		m.notice = "This Task has no Sessions. Press n to add one."
		return m, nil
	}
	m.current = m.task.Sessions[m.selected]
	m.mode = sessionView
	m.scroll, m.unread = 0, 0
	m.composer.focused = prompt
	if prompt {
		m.composer.cursor = len(m.composer.value)
		m.notice = "Enter sends · Ctrl+J adds a line · Esc keeps the draft"
	}
	return m, nil
}

func (m Model) selectedAction(kind string) (tea.Model, tea.Cmd) {
	if len(m.task.Sessions) == 0 {
		m.notice = "This Task has no Sessions."
		return m, nil
	}
	current := m.task.Sessions[m.selected]
	m.notice = actionProgress(kind, current.ID)
	return m, sessionActionCmd(m.ctx, m.backend, kind, current.ID)
}

func (m Model) confirmSelected(kind string) (tea.Model, tea.Cmd) {
	if len(m.task.Sessions) == 0 {
		m.notice = "This Task has no Sessions."
		return m, nil
	}
	current := m.task.Sessions[m.selected]
	return m.withConfirmation(kind, current)
}

func (m Model) confirmCurrent(kind string) (tea.Model, tea.Cmd) {
	return m.withConfirmation(kind, m.current)
}

func (m Model) withConfirmation(kind string, current session.Session) (tea.Model, tea.Cmd) {
	if kind == "cancel" && !sessionIsActive(current, m.projections[current.ID]) {
		m.notice = "There is no active turn to cancel."
		return m, nil
	}
	message := "Cancel the active turn? The Session stays alive and queued steering can continue."
	if kind == "stop" {
		message = "Stop this Session worker? The conversation remains resumable."
	}
	m.confirm = &confirmation{kind: kind, sessionID: current.ID, message: message}
	return m, nil
}

func sessionIsActive(current session.Session, projection *transcript) bool {
	if current.Status == session.StatusWorking {
		return true
	}
	if projection == nil {
		return false
	}
	state := projection.latestState()
	return state == turnLive || state == turnCancelling
}

func (m *Model) toggleToolDetail() {
	tool := m.currentTool()
	if tool == nil {
		m.notice = "No tool call is available in this conversation."
		return
	}
	m.selectedTool = tool.id
	if m.expandedTool == tool.id {
		m.expandedTool = ""
		m.notice = "Tool details collapsed."
	} else {
		m.expandedTool = tool.id
		m.notice = "Showing bounded tool details. Raw ACP remains available with r."
	}
	m.scroll = 0
}

func (m *Model) selectAdjacentTool(delta int) {
	projection := m.projections[m.current.ID]
	if projection == nil {
		return
	}
	tools := projection.tools()
	if len(tools) == 0 {
		m.notice = "No tool calls are available."
		return
	}
	index := len(tools) - 1
	for candidate, tool := range tools {
		if tool.id == m.selectedTool {
			index = candidate
			break
		}
	}
	index = (index + delta + len(tools)) % len(tools)
	m.selectedTool = tools[index].id
	m.expandedTool = ""
	m.notice = fmt.Sprintf("Selected tool %d of %d: %s", index+1, len(tools), toolDisplayTitle(tools[index]))
}

func (m *Model) currentTool() *toolCard {
	projection := m.projections[m.current.ID]
	if projection == nil {
		return nil
	}
	tools := projection.tools()
	if len(tools) == 0 {
		return nil
	}
	for _, tool := range tools {
		if tool.id == m.selectedTool {
			return tool
		}
	}
	return tools[len(tools)-1]
}

func (m Model) handleAction(message actionMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		m.notice = actionLabel(message.kind) + " failed: " + message.err.Error()
		return m, nil
	}
	switch message.kind {
	case "prompt":
		if message.accepted.State == "queued" {
			m.notice = "Message queued; it will run after the active turn."
		} else {
			m.notice = "Message accepted by " + message.sessionID + "."
		}
	case "cancel":
		m.notice = "Cancellation requested. This ends only the active turn; it does not pause or stop the Session."
	case "resume":
		m.notice = "Session worker resumed."
		if message.current.ID == m.current.ID {
			m.current = message.current
		}
		m.manager.ensure(message.current, m.cursors[message.current.ID])
	case "stop":
		m.notice = "Session worker stopped; the conversation remains resumable."
	}
	return m, m.refreshCmd()
}

func promptCmd(ctx context.Context, backend Backend, sessionID, message string) tea.Cmd {
	return func() tea.Msg {
		accepted, err := backend.Prompt(ctx, sessionID, message)
		return actionMsg{kind: "prompt", sessionID: sessionID, accepted: accepted, err: err}
	}
}

func sessionActionCmd(ctx context.Context, backend Backend, kind, sessionID string) tea.Cmd {
	return func() tea.Msg {
		result := actionMsg{kind: kind, sessionID: sessionID}
		switch kind {
		case "cancel":
			result.err = backend.Cancel(ctx, sessionID)
		case "resume":
			result.current, result.err = backend.Resume(ctx, sessionID)
		case "stop":
			result.err = backend.Stop(ctx, sessionID)
		default:
			result.err = fmt.Errorf("unknown console action %q", kind)
		}
		return result
	}
}

func actionProgress(kind, sessionID string) string {
	switch kind {
	case "cancel":
		return "Requesting cancellation for " + sessionID + "…"
	case "resume":
		return "Resuming " + sessionID + "…"
	case "stop":
		return "Stopping " + sessionID + "…"
	default:
		return "Updating " + sessionID + "…"
	}
}

func actionLabel(kind string) string {
	if kind == "" {
		return "Action"
	}
	runes := []rune(kind)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func (m Model) refreshCmd() tea.Cmd {
	mode, direct := m.mode, m.direct
	taskID, sessionID := m.task.Task.ID, m.current.ID
	backend, ctx := m.backend, m.ctx
	cursors := make(map[string]uint64, len(m.cursors))
	for id, cursor := range m.cursors {
		cursors[id] = cursor
	}
	return func() tea.Msg {
		result := refreshMsg{lines: make(map[string][]sessionlog.Line)}
		if mode == taskView || !direct {
			detail, err := backend.Task(ctx, taskID)
			if err != nil {
				result.err = err
				return result
			}
			result.detail = &detail
			for _, current := range detail.Sessions {
				if current.WorkerSocket != "" && cursors[current.ID] != 0 {
					continue
				}
				lines, err := backend.History(ctx, current.ID, cursors[current.ID])
				if err != nil {
					result.err = err
					return result
				}
				result.lines[current.ID] = lines
			}
			return result
		}
		current, err := backend.Session(ctx, sessionID)
		if err != nil {
			result.err = err
			return result
		}
		result.current = &current
		if current.WorkerSocket == "" {
			result.lines[current.ID], result.err = backend.History(ctx, current.ID, cursors[current.ID])
		}
		return result
	}
}

func (m *Model) addActivity(sessionID string, line sessionlog.Line, live bool) {
	if line.Seq == 0 {
		return
	}
	if m.seen[sessionID] == nil {
		m.seen[sessionID] = make(map[uint64]struct{})
	}
	if _, duplicate := m.seen[sessionID][line.Seq]; duplicate {
		return
	}
	m.seen[sessionID][line.Seq] = struct{}{}
	m.histories[sessionID] = append(m.histories[sessionID], line)
	sort.SliceStable(m.histories[sessionID], func(i, j int) bool {
		return m.histories[sessionID][i].Seq < m.histories[sessionID][j].Seq
	})
	m.cursors[sessionID] = max(m.cursors[sessionID], line.Seq)
	if m.projections[sessionID] == nil {
		m.projections[sessionID] = newTranscript()
	}
	m.projections[sessionID].apply(line)
	if live && m.mode == sessionView && m.current.ID == sessionID && m.scroll > 0 {
		m.unread++
	}
}

func (m *Model) watchSessions(sessions []session.Session) {
	for _, current := range sessions {
		m.manager.ensure(current, m.cursors[current.ID])
	}
}

func (m *Model) syncCurrentFromTask() {
	for _, current := range m.task.Sessions {
		if current.ID == m.current.ID {
			m.current = current
			return
		}
	}
}
