package console

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/render"
	appservice "github.com/yty-build/acp-gateway/internal/service"
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

type taggedActivity struct {
	sessionID string
	line      sessionlog.Line
}

// Model is the Bubble Tea model shared by Task and direct Session attachment.
// It owns only viewer state. Closing it cancels subscriptions, never a worker.
type Model struct {
	ctx        context.Context
	backend    Backend
	manager    *watchManager
	mode       viewMode
	direct     bool
	task       task.Detail
	current    session.Session
	selected   int
	histories  map[string][]sessionlog.Line
	cursors    map[string]uint64
	seen       map[string]map[uint64]struct{}
	aggregate  []taggedActivity
	raw        bool
	inputMode  bool
	input      []rune
	notice     string
	scroll     int
	width      int
	height     int
	refreshing bool
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
	m.sortAggregate()
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
		histories: make(map[string][]sessionlog.Line), cursors: make(map[string]uint64),
		seen: make(map[string]map[uint64]struct{}), width: 100, height: 30,
	}
}

func (m *Model) loadStored(current session.Session) error {
	lines, err := m.backend.History(m.ctx, current.ID, 0)
	if err != nil {
		return fmt.Errorf("replay Session %s history: %w", current.ID, err)
	}
	for _, line := range lines {
		m.addActivity(current.ID, line)
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
				m.addActivity(sessionID, line)
			}
		}
		m.sortAggregate()
		return m, nil
	case activityMsg:
		m.addActivity(message.sessionID, message.line)
		m.sortAggregate()
		return m, m.manager.wait()
	case consoleErrorMsg:
		m.notice = "Live stream: " + message.err.Error()
		return m, m.manager.wait()
	case managerClosedMsg:
		return m, nil
	case actionMsg:
		return m.handleAction(message)
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
	if m.inputMode {
		switch name {
		case "enter":
			message := strings.TrimSpace(string(m.input))
			if message == "" {
				m.notice = "Prompt cannot be empty."
				return m, nil
			}
			m.input = nil
			m.inputMode = false
			m.notice = "Submitting prompt…"
			return m, promptCmd(m.ctx, m.backend, m.current.ID, message)
		case "esc":
			if m.current.Status == session.StatusWorking {
				m.notice = "Requesting cancellation; typed prompt is preserved."
				return m, sessionActionCmd(m.ctx, m.backend, "cancel", m.current.ID)
			}
			m.inputMode = false
			m.input = nil
			m.notice = "Prompt dismissed."
			return m, nil
		case "backspace", "ctrl+h":
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
			return m, nil
		default:
			if key.Type == tea.KeyRunes {
				m.input = append(m.input, key.Runes...)
			}
			return m, nil
		}
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
			return m, nil
		case "down", "j":
			if m.selected+1 < len(m.task.Sessions) {
				m.selected++
			}
			return m, nil
		case "enter":
			return m.openSelected(false)
		case "p":
			return m.openSelected(true)
		case "c":
			return m.selectedAction("cancel")
		case "u":
			return m.selectedAction("resume")
		case "x", "s":
			return m.selectedAction("stop")
		case "r":
			m.raw = !m.raw
			return m, nil
		}
		return m, nil
	}

	switch name {
	case "esc":
		if m.current.Status == session.StatusWorking {
			m.notice = "Requesting cancellation of the active turn…"
			return m, sessionActionCmd(m.ctx, m.backend, "cancel", m.current.ID)
		}
		if !m.direct {
			m.mode = taskView
			m.scroll = 0
		}
		return m, nil
	case "b", "backspace":
		if !m.direct {
			m.mode = taskView
			m.scroll = 0
		}
		return m, nil
	case "p":
		m.inputMode = true
		m.notice = "Prompt targets " + m.current.ID + "."
		return m, nil
	case "c":
		return m, sessionActionCmd(m.ctx, m.backend, "cancel", m.current.ID)
	case "u":
		return m, sessionActionCmd(m.ctx, m.backend, "resume", m.current.ID)
	case "x", "s":
		return m, sessionActionCmd(m.ctx, m.backend, "stop", m.current.ID)
	case "r":
		m.raw = !m.raw
		return m, nil
	case "up", "k", "pgup":
		m.scroll += scrollAmount(name)
		return m, nil
	case "down", "j", "pgdown":
		m.scroll = max(0, m.scroll-scrollAmount(name))
		return m, nil
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
		return m, nil
	}
	if m.mode != taskView || event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft {
		return m, nil
	}
	const firstSessionRow = 4
	index := (event.Y - firstSessionRow) / 2
	if event.Y < firstSessionRow || index < 0 || index >= len(m.task.Sessions) {
		return m, nil
	}
	m.selected = index
	return m.openSelected(false)
}

func (m Model) openSelected(prompt bool) (tea.Model, tea.Cmd) {
	if len(m.task.Sessions) == 0 {
		m.notice = "This Task has no Sessions."
		return m, nil
	}
	m.current = m.task.Sessions[m.selected]
	m.mode = sessionView
	m.scroll = 0
	m.inputMode = prompt
	if prompt {
		m.notice = "Prompt targets " + m.current.ID + "."
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

func (m Model) handleAction(message actionMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		m.notice = actionLabel(message.kind) + " failed: " + message.err.Error()
		return m, nil
	}
	switch message.kind {
	case "prompt":
		state := message.accepted.State
		if state == "queued" {
			m.notice = fmt.Sprintf("Prompt %s queued for %s; it will run after the active turn.", message.accepted.RequestID, message.sessionID)
		} else {
			m.notice = fmt.Sprintf("Prompt %s accepted by %s (%s).", message.accepted.RequestID, message.sessionID, valueOr(state, "ready"))
		}
	case "cancel":
		m.notice = "Cancellation requested for " + message.sessionID + "; this is not suspension."
	case "resume":
		m.notice = "Resumed worker for " + message.sessionID + "."
		if message.current.ID == m.current.ID {
			m.current = message.current
		}
		m.manager.ensure(message.current, m.cursors[message.current.ID])
	case "stop":
		m.notice = "Stopped worker for " + message.sessionID + "."
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
				// A live subscription replays from its cursor. Poll logs only for
				// new and inactive Sessions so the two paths cannot reorder data.
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

func (m *Model) addActivity(sessionID string, line sessionlog.Line) {
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
	m.aggregate = append(m.aggregate, taggedActivity{sessionID: sessionID, line: line})
}

func (m *Model) sortAggregate() {
	sort.SliceStable(m.aggregate, func(i, j int) bool {
		left, right := m.aggregate[i], m.aggregate[j]
		if left.line.Timestamp.Equal(right.line.Timestamp) {
			if left.sessionID == right.sessionID {
				return left.line.Seq < right.line.Seq
			}
			return left.sessionID < right.sessionID
		}
		if left.line.Timestamp.IsZero() {
			return true
		}
		if right.line.Timestamp.IsZero() {
			return false
		}
		return left.line.Timestamp.Before(right.line.Timestamp)
	})
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

func (m Model) View() string {
	if m.mode == sessionView {
		return m.sessionView()
	}
	return m.taskView()
}

func (m Model) taskView() string {
	var builder strings.Builder
	item := m.task.Task
	fmt.Fprintf(&builder, "Task %s — %s\n", item.ID, safeText(item.Title))
	fmt.Fprintf(&builder, "Status: %s  Sessions: %d\n\n", item.Status, len(m.task.Sessions))
	builder.WriteString("Sessions  ↑/↓ select · Enter/click open · p prompt selected\n")
	for index, current := range m.task.Sessions {
		marker := " "
		if index == m.selected {
			marker = ">"
		}
		latest := "no activity"
		if history := m.histories[current.ID]; len(history) > 0 {
			latest = render.Activity(history[len(history)-1], false)
		}
		fmt.Fprintf(&builder, "%s %-12s %-10s %-14s %s/%s\n", marker, current.ID, current.Status, valueOr(current.Role, "worker"), current.Agent, valueOr(current.Provider, "ACP"))
		fmt.Fprintf(&builder, "  %s · %s\n", clipText(sessionPath(current), max(18, m.width/2)), clipText(safeText(latest), max(18, m.width/2)))
	}

	aggregateHeader := "Aggregate activity (normalized)"
	if m.raw {
		aggregateHeader = "Aggregate activity (raw ACP/debug)"
	}
	builder.WriteString("\n" + aggregateHeader + "\n")
	activity := make([]string, 0, len(m.aggregate))
	for _, tagged := range m.aggregate {
		activity = append(activity, fmt.Sprintf("[%s] %s", tagged.sessionID, render.Activity(tagged.line, m.raw)))
	}
	available := max(3, m.height-(9+2*len(m.task.Sessions)))
	for _, line := range visibleLines(wrapAll(activity, max(20, m.width)), available, m.scroll) {
		builder.WriteString(line + "\n")
	}
	if len(activity) == 0 {
		builder.WriteString("No activity yet.\n")
	}
	if m.notice != "" {
		builder.WriteString("\n" + clipText(safeText(m.notice), max(20, m.width)) + "\n")
	}
	builder.WriteString("\nControls: Enter open · p prompt · c cancel · u resume · x stop · r raw · q detach\n")
	return builder.String()
}

func (m Model) sessionView() string {
	var builder strings.Builder
	current := m.current
	fmt.Fprintf(&builder, "Session %s — %s / %s\n", current.ID, valueOr(current.Role, "worker"), current.Agent)
	fmt.Fprintf(&builder, "Task: %s  Status: %s  Permission: %s\n", current.TaskID, current.Status, appservice.PermissionLabel(current))
	fmt.Fprintf(&builder, "Repo: %s\n", safeText(current.RepoPath))
	if current.WorktreePath != "" || current.Branch != "" {
		fmt.Fprintf(&builder, "Worktree: %s  Branch: %s\n", valueOr(current.WorktreePath, "—"), valueOr(current.Branch, "—"))
	}
	fmt.Fprintf(&builder, "ACP: %s  Native: %s\n\n", valueOr(current.ACPSessionID, "—"), valueOr(current.NativeSessionID, "—"))
	mode := "normalized"
	if m.raw {
		mode = "raw ACP/debug"
	}
	fmt.Fprintf(&builder, "Activity (%s; ↑/↓ scroll)\n", mode)
	activity := make([]string, 0, len(m.histories[current.ID]))
	for _, line := range m.histories[current.ID] {
		activity = append(activity, render.Activity(line, m.raw))
	}
	reserved := 12
	if current.WorktreePath != "" || current.Branch != "" {
		reserved++
	}
	available := max(4, m.height-reserved)
	for _, line := range visibleLines(wrapAll(activity, max(20, m.width)), available, m.scroll) {
		builder.WriteString(line + "\n")
	}
	if len(activity) == 0 {
		builder.WriteString("No ACP-visible history yet.\n")
	}
	if m.inputMode {
		fmt.Fprintf(&builder, "\nPrompt %s > %s█\n", current.ID, safeText(string(m.input)))
	} else if m.notice != "" {
		builder.WriteString("\n" + clipText(safeText(m.notice), max(20, m.width)) + "\n")
	}
	back := ""
	if !m.direct {
		back = " · b back"
	}
	builder.WriteString("\nControls: p prompt · Esc/c cancel turn · u resume · x stop · r raw" + back + " · q detach\n")
	return builder.String()
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

func visibleLines(lines []string, count, scroll int) []string {
	if len(lines) <= count {
		return lines
	}
	end := max(count, len(lines)-min(scroll, len(lines)-count))
	start := max(0, end-count)
	return lines[start:end]
}

func wrapAll(lines []string, width int) []string {
	var result []string
	for _, line := range lines {
		line = safeText(line)
		runes := []rune(line)
		if len(runes) == 0 {
			result = append(result, "")
			continue
		}
		for len(runes) > width {
			result = append(result, string(runes[:width]))
			runes = runes[width:]
		}
		result = append(result, string(runes))
	}
	return result
}

func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return '↵'
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func clipText(value string, width int) string {
	value = safeText(value)
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func scrollAmount(key string) int {
	if strings.HasPrefix(key, "pg") {
		return 8
	}
	return 1
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return safeText(value)
}
