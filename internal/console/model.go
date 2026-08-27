package console

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

const (
	refreshInterval               = 750 * time.Millisecond
	maxRetainedChannelFrames      = 512
	maxRetainedChannelBytes       = 4 << 20
	maxRetainedChannelTotalFrames = 2048
	maxRetainedChannelTotalBytes  = 16 << 20
	maxRetainedProjectionBytes    = 16 << 20
	maxRetainedProjectionObjects  = 12288
	initialReplayRecords          = 256
	channelLineOverhead           = 256
)

type retainedChannelLine struct {
	line    sessionlog.Line
	bytes   int
	ordinal uint64
}

type viewMode uint8

const (
	taskView viewMode = iota
	sessionView
)

type overlayMode uint8

const (
	noOverlay overlayMode = iota
	helpOverlay
	paletteOverlay
	filterOverlay
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
	kind         string
	sessionID    string
	permissionID string
	optionID     string
	message      string
}

// Model is the Bubble Tea model shared by Task and direct Session attachment.
// It owns only disposable viewer state. Closing it cancels subscriptions, never
// a worker, a provider turn, or a provider conversation.
type Model struct {
	ctx     context.Context
	backend Backend
	manager *watchManager

	mode     viewMode
	direct   bool
	task     task.Detail
	current  session.Session
	selected int

	histories                 map[string][]retainedChannelLine
	historyBytes              map[string]int
	historyClipped            map[string]bool
	replayWindows             map[string]appservice.ReplayWindow
	retainedHistoryBytes      int
	retainedHistoryFrames     int
	nextHistoryOrdinal        uint64
	projections               map[string]*transcript
	projectionBytes           map[string]int
	projectionObjects         map[string]int
	projectionOrdinals        map[string]uint64
	projectionClipped         map[string]bool
	projectionOverflow        map[string]bool
	retainedProjectionBytes   int
	retainedProjectionObjects int
	nextProjectionOrdinal     uint64
	cursors                   map[string]uint64
	permissionFrameCursors    map[string]uint64
	permissionSnapshotCursors map[string]uint64
	pendingPermissionSnapshot *queuedPermissionSnapshot
	permissionDecisionsReady  map[string]bool
	permissionBlockedReason   map[string]string
	permissionZeroAuthority   map[string]bool

	raw           bool
	showReasoning bool
	expandedTool  string
	selectedTool  string
	composer      composerState
	confirm       *confirmation
	wizard        *sessionWizard
	overlay       overlayMode
	overlayEditor composerState
	filter        string
	paletteIndex  int
	notice        string
	scroll        int
	unread        int
	width         int
	height        int
	refreshing    bool
}

type refreshTickMsg struct{}

type refreshMsg struct {
	detail                  *task.Detail
	current                 *session.Session
	permissionSessionID     string
	permissionSnapshot      *appservice.PermissionSnapshot
	permissionSnapshotError error
	err                     error
}

type permissionSnapshotMsg struct {
	sessionID string
	snapshot  appservice.PermissionSnapshot
	err       error
}

type queuedPermissionSnapshot struct {
	sessionID string
	snapshot  appservice.PermissionSnapshot
	clipped   bool
}

type actionMsg struct {
	kind      string
	sessionID string
	draft     string
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
	effective, windows, err := backend.TaskReplayWindows(ctx, taskID, nil, initialReplayRecords)
	if err != nil {
		m.manager.cancel()
		cancel()
		return Model{}, func() {}, err
	}
	for sessionID, cursor := range effective {
		m.cursors[sessionID] = cursor
	}
	for sessionID, window := range windows {
		m.replayWindows[sessionID] = window
		m.historyClipped[sessionID] = window.Clipped
	}
	if len(detail.Sessions) > 0 {
		selected := detail.Sessions[min(max(0, m.selected), len(detail.Sessions)-1)]
		snapshot, snapshotErr := backend.PermissionSnapshot(ctx, selected.ID)
		if snapshotErr != nil {
			m.notice = "Permission status unavailable: " + snapshotErr.Error()
		} else {
			m.receivePermissionSnapshot(selected.ID, snapshot)
		}
	}
	m.manager.ensureTask(taskID, effective)
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
	cursor, clipped, err := backend.FreshReplayCursor(ctx, sessionID, initialReplayRecords)
	if err != nil {
		m.manager.cancel()
		cancel()
		return Model{}, func() {}, err
	}
	m.cursors[current.ID] = cursor
	m.replayWindows[current.ID] = replayWindow(cursor, clipped)
	m.historyClipped[current.ID] = clipped
	snapshot, snapshotErr := backend.PermissionSnapshot(ctx, current.ID)
	if snapshotErr != nil {
		m.notice = "Permission status unavailable: " + snapshotErr.Error()
	} else {
		m.receivePermissionSnapshot(current.ID, snapshot)
	}
	m.manager.ensureSession(current, m.cursors[current.ID])
	return m, func() { m.manager.cancel(); cancel() }, nil
}

func baseModel(ctx context.Context, backend Backend) Model {
	return Model{
		ctx: ctx, backend: backend, manager: newWatchManager(ctx, backend),
		histories: make(map[string][]retainedChannelLine), projections: make(map[string]*transcript),
		historyBytes: make(map[string]int), historyClipped: make(map[string]bool), replayWindows: make(map[string]appservice.ReplayWindow),
		projectionBytes: make(map[string]int), projectionObjects: make(map[string]int), projectionOrdinals: make(map[string]uint64),
		projectionClipped: make(map[string]bool), projectionOverflow: make(map[string]bool),
		cursors: make(map[string]uint64), permissionFrameCursors: make(map[string]uint64), permissionSnapshotCursors: make(map[string]uint64),
		permissionDecisionsReady: make(map[string]bool), permissionBlockedReason: make(map[string]string),
		permissionZeroAuthority: make(map[string]bool),
		width:                   100, height: 30,
	}
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
			m.ensureVisibleSelection()
			m.manager.ensureTask(m.task.Task.ID, m.cursors)
		}
		if message.current != nil {
			m.current = *message.current
			m.manager.ensureSession(m.current, m.cursors[m.current.ID])
		}
		if message.permissionSnapshotError != nil {
			m.notice = "Permission status unavailable: " + message.permissionSnapshotError.Error()
		} else if message.permissionSnapshot != nil {
			m.receivePermissionSnapshot(message.permissionSessionID, *message.permissionSnapshot)
		}
		return m, nil
	case permissionSnapshotMsg:
		if message.err != nil {
			m.notice = "Permission status unavailable: " + message.err.Error()
			return m, nil
		}
		m.receivePermissionSnapshot(message.sessionID, message.snapshot)
		return m, nil
	case activityMsg:
		m.addActivity(message.sessionID, message.line, true)
		return m, m.manager.wait()
	case taskActivityMsg:
		m.addActivity(message.activity.SessionID, message.activity.Line, true)
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
	case sessionConnectedMsg:
		return m.handleSessionConnected(message)
	case providerOptionsReadyMsg:
		return m.handleProviderOptionsReady(message)
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
	if m.overlay != noOverlay {
		return m.handleOverlayKey(key)
	}
	if name == "?" {
		m.overlay = helpOverlay
		return m, nil
	}

	if m.mode == taskView {
		switch name {
		case "up", "k":
			m.moveTaskSelection(-1)
			return m, nil
		case "down", "j":
			m.moveTaskSelection(1)
			return m, nil
		case "s":
			return m.dispatchTaskAction(actionStop)
		case "esc":
			if m.filter != "" {
				m.filter = ""
				m.ensureVisibleSelection()
				m.notice = "Session filter cleared."
			}
			return m, nil
		}
		if action, ok := taskActionForKey(name); ok {
			return m.dispatchTaskAction(action.ID)
		}
		return m, nil
	}
	if name == "q" {
		return m, tea.Quit
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
		return m.resumeAction(m.current)
	case "x", "s":
		return m.confirmCurrent("stop")
	case "r":
		m.raw = !m.raw
		m.scroll, m.unread = 0, 0
		if m.raw {
			m.notice = "Raw Session Channel frames enabled. Press r to return to the conversation."
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
	default:
		if len(name) == 1 && name[0] >= '1' && name[0] <= '9' {
			return m.confirmPermission(int(name[0] - '1'))
		}
	}
	return m, nil
}

func (m Model) dispatchTaskAction(id actionID) (tea.Model, tea.Cmd) {
	m.overlay = noOverlay
	m.overlayEditor = composerState{}
	switch id {
	case actionOpen:
		return m.openSelected(false)
	case actionMessage:
		return m.openSelected(true)
	case actionNew:
		m.notice = "Loading agent profiles…"
		return m, loadWizardCmd(m.ctx, m.backend, m.defaultSessionRepo())
	case actionCancel:
		return m.confirmSelected("cancel")
	case actionResume:
		return m.selectedAction("resume")
	case actionStop:
		return m.confirmSelected("stop")
	case actionFilter:
		m.overlay = filterOverlay
		m.overlayEditor.value = []rune(m.filter)
		m.overlayEditor.cursor = len(m.overlayEditor.value)
		return m, nil
	case actionPalette:
		m.overlay = paletteOverlay
		m.paletteIndex = 0
		return m, nil
	case actionHelp:
		m.overlay = helpOverlay
		return m, nil
	case actionDetach:
		return m, tea.Quit
	default:
		m.notice = "Unknown console action."
		return m, nil
	}
}

func (m Model) handleOverlayKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	name := key.String()
	if m.overlay == helpOverlay {
		if name == "?" || name == "esc" || name == "q" || name == "enter" {
			m.overlay = noOverlay
		}
		return m, nil
	}
	if name == "esc" {
		m.overlay = noOverlay
		m.overlayEditor = composerState{}
		return m, nil
	}
	if m.overlay == paletteOverlay {
		actions := filteredTaskActions(m.overlayEditor.text(), m.selectedCanResume())
		switch name {
		case "up", "ctrl+p":
			m.paletteIndex = max(0, m.paletteIndex-1)
		case "down", "ctrl+n":
			m.paletteIndex = min(max(0, len(actions)-1), m.paletteIndex+1)
		case "enter":
			if len(actions) > 0 {
				return m.dispatchTaskAction(actions[m.paletteIndex].ID)
			}
		case "backspace", "ctrl+h":
			m.overlayEditor.backspace()
			m.paletteIndex = 0
		case "ctrl+w":
			m.overlayEditor.deleteWord()
			m.paletteIndex = 0
		default:
			if key.Type == tea.KeySpace {
				m.overlayEditor.insert(' ')
				m.paletteIndex = 0
			} else if key.Type == tea.KeyRunes {
				m.overlayEditor.insert(key.Runes...)
				m.paletteIndex = 0
			}
		}
		return m, nil
	}
	if m.overlay == filterOverlay {
		switch name {
		case "enter":
			m.overlay = noOverlay
			m.notice = fmt.Sprintf("Showing %d of %d Sessions.", len(m.filteredSessionIndices()), len(m.task.Sessions))
			return m, nil
		case "backspace", "ctrl+h":
			m.overlayEditor.backspace()
		case "ctrl+w":
			m.overlayEditor.deleteWord()
		case "ctrl+u":
			m.overlayEditor = composerState{}
		default:
			if key.Type == tea.KeySpace {
				m.overlayEditor.insert(' ')
			} else if key.Type == tea.KeyRunes {
				m.overlayEditor.insert(key.Runes...)
			}
		}
		m.filter = m.overlayEditor.text()
		m.ensureVisibleSelection()
	}
	return m, nil
}

func (m Model) filteredSessionIndices() []int {
	query := strings.ToLower(strings.TrimSpace(m.filter))
	result := make([]int, 0, len(m.task.Sessions))
	for index, current := range m.task.Sessions {
		if query == "" {
			result = append(result, index)
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{
			current.ID, current.Agent, current.Role, current.Provider, string(current.Status), current.RepoPath,
			current.WorktreePath, current.Branch, m.sessionSummary(current.ID),
		}, " "))
		if strings.Contains(haystack, query) {
			result = append(result, index)
		}
	}
	return result
}

func (m *Model) ensureVisibleSelection() {
	indices := m.filteredSessionIndices()
	if len(indices) == 0 {
		return
	}
	for _, index := range indices {
		if index == m.selected {
			return
		}
	}
	m.selected = indices[0]
}

func (m *Model) moveTaskSelection(delta int) {
	indices := m.filteredSessionIndices()
	if len(indices) == 0 {
		return
	}
	position := 0
	for index, actual := range indices {
		if actual == m.selected {
			position = index
			break
		}
	}
	position = min(max(0, position+delta), len(indices)-1)
	m.selected = indices[position]
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
		if confirm.kind == "permission" {
			if !m.permissionDecisionsReady[confirm.sessionID] {
				m.notice = m.permissionUnavailableNotice(confirm.sessionID)
				return m, nil
			}
			m.notice = "Sending the selected permission decision…"
			return m, permissionDecisionCmd(m.ctx, m.backend, confirm.sessionID, confirm.permissionID, confirm.optionID)
		}
		m.notice = actionProgress(confirm.kind, confirm.sessionID)
		return m, sessionActionCmd(m.ctx, m.backend, confirm.kind, confirm.sessionID)
	case "n", "N", "esc", "enter":
		m.confirm = nil
		if confirm.kind == "permission" {
			m.notice = "Permission choice dismissed; the agent is still waiting."
		} else {
			m.notice = actionLabel(confirm.kind) + " dismissed; the Session is unchanged."
		}
	}
	return m, nil
}

func (m Model) handleMouse(event tea.MouseEvent) (tea.Model, tea.Cmd) {
	// Mouse input must not reach the Task navigator through a visible modal or
	// focused editor. In particular, a click intended for the New Session wizard
	// must never select an underlying row or leave the console.
	if m.wizard != nil || m.overlay != noOverlay || m.composer.focused || m.confirm != nil {
		return m, nil
	}
	if event.Button == tea.MouseButtonWheelUp {
		if m.mode == taskView {
			m.moveTaskSelection(-1)
		} else {
			m.scroll += 3
		}
		return m, nil
	}
	if event.Button == tea.MouseButtonWheelDown {
		if m.mode == taskView {
			m.moveTaskSelection(1)
		} else {
			m.scroll = max(0, m.scroll-3)
			if m.scroll == 0 {
				m.unread = 0
			}
		}
		return m, nil
	}
	if m.mode != taskView || event.Action != tea.MouseActionPress || event.Button != tea.MouseButtonLeft {
		return m, nil
	}
	if m.width < minimumConsoleWidth || m.height < minimumConsoleHeight {
		return m, nil
	}
	layout := layoutTaskWorkspace(m.width, m.height)
	selected := -1
	for _, hit := range m.taskSessionHits(layout) {
		if hit.Rect.contains(event.X, event.Y) {
			selected = hit.SessionIndex
			break
		}
	}
	if selected < 0 {
		return m, nil
	}
	m.selected = selected
	current := m.task.Sessions[m.selected]
	m.notice = "Selected " + current.ID + ". Enter opens its Session Channel; p opens it with the composer focused."
	return m, nil
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
	return m, m.permissionSnapshotCmd(m.current.ID)
}

func (m Model) selectedAction(kind string) (tea.Model, tea.Cmd) {
	if len(m.task.Sessions) == 0 {
		m.notice = "This Task has no Sessions."
		return m, nil
	}
	current := m.task.Sessions[m.selected]
	if kind == "resume" {
		return m.resumeAction(current)
	}
	m.notice = actionProgress(kind, current.ID)
	return m, sessionActionCmd(m.ctx, m.backend, kind, current.ID)
}

func sessionCanResume(current session.Session) bool {
	return current.CanResume()
}

func (m Model) selectedCanResume() bool {
	return len(m.task.Sessions) > 0 && m.selected >= 0 && m.selected < len(m.task.Sessions) && sessionCanResume(m.task.Sessions[m.selected])
}

func (m Model) resumeAction(current session.Session) (tea.Model, tea.Cmd) {
	if sessionCanResume(current) {
		m.notice = actionProgress("resume", current.ID)
		return m, sessionActionCmd(m.ctx, m.backend, "resume", current.ID)
	}
	switch {
	case current.ProviderRef == "":
		m.notice = "This Session has no saved provider identity and cannot be resumed; create a new Session."
	case current.WorkerPID != 0 || current.WorkerSocket != "":
		if current.Status == session.StatusFailed {
			m.notice = "This Session retains quarantined worker ownership; manual provider-process teardown is required."
		} else {
			m.notice = "This Session worker is already running."
		}
	default:
		m.notice = "This Session cannot be resumed in its current state."
	}
	return m, nil
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
	message := "Cancel the active turn? The Session and provider connection stay alive."
	if kind == "stop" {
		if current.ProviderRef == "" {
			message = "Stop this Session worker? No provider identity has been established, so continuing later requires a new Session."
		} else {
			message = "Stop this Session worker? The saved provider identity can be exact-resumed after teardown completes."
		}
	}
	m.confirm = &confirmation{kind: kind, sessionID: current.ID, message: message}
	return m, nil
}

func (m Model) confirmPermission(index int) (tea.Model, tea.Cmd) {
	projection := m.projections[m.current.ID]
	if projection == nil {
		return m, nil
	}
	_, card := projection.pendingPermission()
	if card == nil || index < 0 || index >= len(card.options) {
		return m, nil
	}
	if !m.permissionDecisionsReady[m.current.ID] {
		m.notice = m.permissionUnavailableNotice(m.current.ID)
		return m, nil
	}
	option := card.options[index]
	label := valueOr(option.name, strings.ReplaceAll(option.kind, "_", " "))
	message := fmt.Sprintf("Choose %q for %s?", label, valueOr(card.title, "this tool request"))
	if option.kind == "allow_always" {
		message = fmt.Sprintf("ALLOW ALWAYS for %s? Future matching requests may be approved automatically.", valueOr(card.title, "this tool request"))
	} else if option.kind == "reject_always" {
		message = fmt.Sprintf("REJECT ALWAYS for %s? Future matching requests may be rejected automatically.", valueOr(card.title, "this tool request"))
	}
	m.confirm = &confirmation{
		kind: "permission", sessionID: m.current.ID, permissionID: card.id,
		optionID: option.id, message: message,
	}
	return m, nil
}

func (m Model) permissionUnavailableNotice(sessionID string) string {
	reason := strings.TrimSpace(m.permissionBlockedReason[sessionID])
	if reason == "" {
		return "Permission decision unavailable; refresh the live Session before choosing an option."
	}
	return "Permission decision unavailable: " + reason
}

func sessionIsActive(current session.Session, projection *transcript) bool {
	if current.Status == session.StatusWorking {
		return true
	}
	if projection == nil {
		return false
	}
	state := projection.latestState()
	return state == turnReady || state == turnLive || state == turnCancelling
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
		m.notice = "Showing bounded tool details. Raw Session Channel frames remain available with r."
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
		if message.kind == "prompt" && strings.TrimSpace(message.draft) != "" {
			m.composer.value = []rune(message.draft)
			m.composer.cursor = len(m.composer.value)
			m.composer.focused = true
		}
		m.notice = actionLabel(message.kind) + " failed: " + message.err.Error()
		return m, nil
	}
	switch message.kind {
	case "prompt":
		m.notice = "Message accepted by " + message.sessionID + "."
	case "cancel":
		m.notice = "Cancellation requested. This ends only the active turn; it does not pause or stop the Session."
	case "resume":
		m.notice = "Session worker resumed."
		m.permissionZeroAuthority[message.sessionID] = false
		m.permissionDecisionsReady[message.sessionID] = false
		m.permissionBlockedReason[message.sessionID] = "Waiting for the resumed worker's permission status."
		if message.current.ID == m.current.ID {
			m.current = message.current
		}
		if m.direct {
			m.manager.ensureSession(message.current, m.cursors[message.current.ID])
		}
	case "stop":
		m.notice = "Session worker stopped."
	case "permission":
		m.notice = "Permission decision accepted. The agent turn continues in this Session."
	}
	return m, m.refreshCmd()
}

func promptCmd(ctx context.Context, backend Backend, sessionID, message string) tea.Cmd {
	return func() tea.Msg {
		accepted, err := backend.Prompt(ctx, sessionID, message)
		return actionMsg{kind: "prompt", sessionID: sessionID, draft: message, accepted: accepted, err: err}
	}
}

func permissionDecisionCmd(ctx context.Context, backend Backend, sessionID, permissionID, optionID string) tea.Cmd {
	return func() tea.Msg {
		err := backend.DecidePermission(ctx, sessionID, permissionID, optionID, false)
		return actionMsg{kind: "permission", sessionID: sessionID, err: err}
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
	if mode == taskView && len(m.task.Sessions) > 0 {
		index := min(max(0, m.selected), len(m.task.Sessions)-1)
		sessionID = m.task.Sessions[index].ID
	}
	backend, ctx := m.backend, m.ctx
	return func() tea.Msg {
		result := refreshMsg{permissionSessionID: sessionID}
		if mode == taskView || !direct {
			detail, err := backend.Task(ctx, taskID)
			if err != nil {
				result.err = err
				return result
			}
			result.detail = &detail
		} else {
			current, err := backend.Session(ctx, sessionID)
			if err != nil {
				result.err = err
				return result
			}
			result.current = &current
		}
		if sessionID != "" {
			snapshot, err := backend.PermissionSnapshot(ctx, sessionID)
			if err != nil {
				result.permissionSnapshotError = err
			} else {
				result.permissionSnapshot = &snapshot
			}
		}
		return result
	}
}

func (m Model) permissionSnapshotCmd(sessionID string) tea.Cmd {
	if sessionID == "" {
		return nil
	}
	backend, ctx := m.backend, m.ctx
	return func() tea.Msg {
		snapshot, err := backend.PermissionSnapshot(ctx, sessionID)
		return permissionSnapshotMsg{sessionID: sessionID, snapshot: snapshot, err: err}
	}
}

func (m *Model) receivePermissionSnapshot(sessionID string, snapshot appservice.PermissionSnapshot) {
	if sessionID == "" || !m.permissionSnapshotRelevant(sessionID) {
		return
	}
	var clipped bool
	snapshot.Permissions, clipped = boundedPendingPermissions(snapshot.Permissions)
	var fieldClipped bool
	snapshot.BlockedCode, fieldClipped = boundedInline(snapshot.BlockedCode, maxProjectedKindBytes)
	clipped = clipped || fieldClipped
	snapshot.BlockedMessage, fieldClipped = boundedInline(snapshot.BlockedMessage, maxProjectedErrorBytes)
	clipped = clipped || fieldClipped
	if snapshot.Sequence == 0 {
		queuedPositive := m.pendingPermissionSnapshot != nil && m.pendingPermissionSnapshot.sessionID == sessionID &&
			m.pendingPermissionSnapshot.snapshot.Sequence > 0
		if m.permissionSnapshotCursors[sessionID] > 0 || m.permissionFrameCursors[sessionID] > 0 || queuedPositive {
			return
		}
		m.applyPermissionReadiness(sessionID, snapshot)
		m.permissionZeroAuthority[sessionID] = !snapshot.DecisionsReady
		return
	}
	if snapshot.Sequence < m.permissionSnapshotCursors[sessionID] {
		return
	}
	if m.permissionFrameCursors[sessionID] > snapshot.Sequence {
		return
	}
	if m.cursors[sessionID] >= snapshot.Sequence {
		m.pendingPermissionSnapshot = nil
		m.applyPermissionSnapshot(sessionID, snapshot, clipped)
		return
	}
	if queued := m.pendingPermissionSnapshot; queued != nil && queued.sessionID == sessionID && queued.snapshot.Sequence > snapshot.Sequence {
		return
	}
	m.pendingPermissionSnapshot = &queuedPermissionSnapshot{sessionID: sessionID, snapshot: snapshot, clipped: clipped}
}

func (m Model) permissionSnapshotRelevant(sessionID string) bool {
	if m.mode == sessionView {
		return m.current.ID == sessionID
	}
	if len(m.task.Sessions) == 0 {
		return false
	}
	index := min(max(0, m.selected), len(m.task.Sessions)-1)
	return m.task.Sessions[index].ID == sessionID
}

func (m *Model) applyQueuedPermissionSnapshot(sessionID string) {
	queued := m.pendingPermissionSnapshot
	if queued == nil || queued.sessionID != sessionID {
		return
	}
	if !m.permissionSnapshotRelevant(sessionID) || m.permissionFrameCursors[sessionID] > queued.snapshot.Sequence ||
		queued.snapshot.Sequence < m.permissionSnapshotCursors[sessionID] {
		m.pendingPermissionSnapshot = nil
		return
	}
	if m.cursors[sessionID] < queued.snapshot.Sequence {
		return
	}
	m.pendingPermissionSnapshot = nil
	m.applyPermissionSnapshot(sessionID, queued.snapshot, queued.clipped)
}

func (m *Model) applyPermissionSnapshot(sessionID string, snapshot appservice.PermissionSnapshot, clipped bool) {
	if sessionID == "" {
		return
	}
	m.applyPermissionReadiness(sessionID, snapshot)
	m.permissionZeroAuthority[sessionID] = false
	m.permissionSnapshotCursors[sessionID] = max(m.permissionSnapshotCursors[sessionID], snapshot.Sequence)
	projection := m.projections[sessionID]
	if projection == nil {
		if len(snapshot.Permissions) == 0 {
			return
		}
		projection = newTranscript()
		projection.clipped = m.projectionClipped[sessionID]
		m.projections[sessionID] = projection
	}
	projection.reconcilePendingPermissions(snapshot.Permissions)
	projection.clipped = projection.clipped || clipped
	m.updateProjectionRetention(sessionID)
	if m.projections[sessionID] != nil {
		delete(m.projectionOverflow, sessionID)
	}
}

func (m *Model) applyPermissionReadiness(sessionID string, snapshot appservice.PermissionSnapshot) {
	m.permissionDecisionsReady[sessionID] = snapshot.DecisionsReady
	reason := firstNonEmpty(snapshot.BlockedMessage, humanizeLabel(snapshot.BlockedCode))
	if snapshot.DecisionsReady {
		reason = ""
	}
	m.permissionBlockedReason[sessionID] = reason
}

func (m *Model) addActivity(sessionID string, line sessionlog.Line, live bool) {
	if line.Seq == 0 {
		return
	}
	if line.Seq <= m.cursors[sessionID] {
		return
	}
	if m.cursors[sessionID] == 0 && line.Seq > 1 {
		m.historyClipped[sessionID] = true
		if window, exists := m.replayWindows[sessionID]; !exists || !window.Clipped {
			m.replayWindows[sessionID] = appservice.ReplayWindow{
				Cursor: line.Seq - 1, EarliestSequence: line.Seq, Clipped: true,
			}
		}
	}
	if _, exists := m.replayWindows[sessionID]; !exists {
		m.replayWindows[sessionID] = replayWindow(0, false)
	}
	m.cursors[sessionID] = max(m.cursors[sessionID], line.Seq)
	if isPermissionFrame(line.Type) {
		m.permissionFrameCursors[sessionID] = line.Seq
		if line.Type == worker.FramePermissionRequested && !m.permissionZeroAuthority[sessionID] {
			m.permissionDecisionsReady[sessionID] = true
			m.permissionBlockedReason[sessionID] = ""
		}
	}
	if m.projections[sessionID] == nil {
		m.projections[sessionID] = newTranscript()
		m.projections[sessionID].clipped = m.projectionClipped[sessionID]
	}
	m.projections[sessionID].apply(line)
	m.updateProjectionRetention(sessionID)
	retainedBytes := retainedChannelLineBytes(line)
	if retainedBytes > maxRetainedChannelBytes {
		m.historyClipped[sessionID] = true
	} else {
		m.nextHistoryOrdinal++
		m.histories[sessionID] = append(m.histories[sessionID], retainedChannelLine{
			line: line, bytes: retainedBytes, ordinal: m.nextHistoryOrdinal,
		})
		m.historyBytes[sessionID] += retainedBytes
		m.retainedHistoryBytes += retainedBytes
		m.retainedHistoryFrames++
		for len(m.histories[sessionID]) > maxRetainedChannelFrames || m.historyBytes[sessionID] > maxRetainedChannelBytes {
			m.dropOldestRetainedLine(sessionID)
		}
		for m.retainedHistoryFrames > maxRetainedChannelTotalFrames || m.retainedHistoryBytes > maxRetainedChannelTotalBytes {
			oldestSession, ok := m.oldestRetainedSession()
			if !ok {
				m.retainedHistoryBytes = 0
				m.retainedHistoryFrames = 0
				break
			}
			m.dropOldestRetainedLine(oldestSession)
		}
	}
	if live && m.mode == sessionView && m.current.ID == sessionID && m.scroll > 0 {
		m.unread++
	}
	m.applyQueuedPermissionSnapshot(sessionID)
}

func isPermissionFrame(lineType string) bool {
	return lineType == worker.FramePermissionRequested || lineType == worker.FramePermissionResolved || lineType == worker.FramePermissionAbandoned
}

func retainedChannelLineBytes(line sessionlog.Line) int {
	return channelLineOverhead + len(line.RequestID) + len(line.Type) + len(line.Data)
}

func replayWindow(cursor uint64, clipped bool) appservice.ReplayWindow {
	earliest := cursor
	if cursor != ^uint64(0) {
		earliest++
	}
	return appservice.ReplayWindow{Cursor: cursor, EarliestSequence: earliest, Clipped: clipped}
}

func (m *Model) updateProjectionRetention(sessionID string) {
	projection := m.projections[sessionID]
	if projection == nil {
		return
	}
	usage := projection.retainedUsage()
	m.retainedProjectionBytes = max(0, m.retainedProjectionBytes-m.projectionBytes[sessionID]) + usage.bytes
	m.retainedProjectionObjects = max(0, m.retainedProjectionObjects-m.projectionObjects[sessionID]) + usage.objects
	m.projectionBytes[sessionID] = usage.bytes
	m.projectionObjects[sessionID] = usage.objects
	m.nextProjectionOrdinal++
	m.projectionOrdinals[sessionID] = m.nextProjectionOrdinal
	if m.retainedProjectionBytes > maxRetainedProjectionBytes || m.retainedProjectionObjects > maxRetainedProjectionObjects {
		m.compactPendingProjections()
	}
	for m.retainedProjectionBytes > maxRetainedProjectionBytes || m.retainedProjectionObjects > maxRetainedProjectionObjects {
		oldestSession, ok := m.oldestProjectionSession()
		if !ok {
			// A conforming Task stream has at most 512 Sessions, whose compact
			// pending permission cards fit this budget. If a Backend violates that
			// contract, remain bounded and disclose that the exact Session must be
			// attached directly instead of silently falsifying accounting.
			overflowSession := sessionID
			_, ok = m.projections[overflowSession]
			if !ok {
				overflowSession, ok = m.oldestProjectionSessionIncludingPending()
			}
			if !ok || m.projections[overflowSession] == nil {
				break
			}
			m.dropProjection(overflowSession)
			m.projectionOverflow[overflowSession] = true
			continue
		}
		m.dropProjection(oldestSession)
	}
}

func (m *Model) oldestProjectionSessionIncludingPending() (string, bool) {
	var (
		oldestSession string
		oldestOrdinal uint64
		found         bool
	)
	for sessionID := range m.projections {
		ordinal := m.projectionOrdinals[sessionID]
		if !found || ordinal < oldestOrdinal {
			oldestSession = sessionID
			oldestOrdinal = ordinal
			found = true
		}
	}
	return oldestSession, found
}

func (m *Model) oldestProjectionSession() (string, bool) {
	var (
		oldestSession string
		oldestOrdinal uint64
		found         bool
	)
	for sessionID, projection := range m.projections {
		if _, pending := projection.pendingPermission(); pending != nil {
			continue
		}
		ordinal := m.projectionOrdinals[sessionID]
		if !found || ordinal < oldestOrdinal {
			oldestSession = sessionID
			oldestOrdinal = ordinal
			found = true
		}
	}
	return oldestSession, found
}

func (m *Model) compactPendingProjections() {
	for sessionID, projection := range m.projections {
		if !projection.compactToPendingPermissions() {
			continue
		}
		usage := projection.retainedUsage()
		m.retainedProjectionBytes = max(0, m.retainedProjectionBytes-m.projectionBytes[sessionID]) + usage.bytes
		m.retainedProjectionObjects = max(0, m.retainedProjectionObjects-m.projectionObjects[sessionID]) + usage.objects
		m.projectionBytes[sessionID] = usage.bytes
		m.projectionObjects[sessionID] = usage.objects
		m.projectionClipped[sessionID] = true
	}
}

func (m *Model) dropProjection(sessionID string) {
	if m.projections[sessionID] == nil {
		return
	}
	m.retainedProjectionBytes = max(0, m.retainedProjectionBytes-m.projectionBytes[sessionID])
	m.retainedProjectionObjects = max(0, m.retainedProjectionObjects-m.projectionObjects[sessionID])
	delete(m.projections, sessionID)
	delete(m.projectionBytes, sessionID)
	delete(m.projectionObjects, sessionID)
	delete(m.projectionOrdinals, sessionID)
	m.projectionClipped[sessionID] = true
}

func (m *Model) dropOldestRetainedLine(sessionID string) {
	history := m.histories[sessionID]
	if len(history) == 0 {
		return
	}
	dropped := history[0]
	m.historyBytes[sessionID] = max(0, m.historyBytes[sessionID]-dropped.bytes)
	m.retainedHistoryBytes = max(0, m.retainedHistoryBytes-dropped.bytes)
	m.retainedHistoryFrames = max(0, m.retainedHistoryFrames-1)
	history[0] = retainedChannelLine{}
	history = history[1:]
	if len(history) == 0 {
		delete(m.histories, sessionID)
		delete(m.historyBytes, sessionID)
	} else {
		m.histories[sessionID] = history
	}
	m.historyClipped[sessionID] = true
}

func (m *Model) oldestRetainedSession() (string, bool) {
	var (
		oldestSession string
		oldestOrdinal uint64
		found         bool
	)
	for sessionID, history := range m.histories {
		if len(history) == 0 {
			continue
		}
		if !found || history[0].ordinal < oldestOrdinal {
			oldestSession = sessionID
			oldestOrdinal = history[0].ordinal
			found = true
		}
	}
	return oldestSession, found
}

func (m *Model) syncCurrentFromTask() {
	for _, current := range m.task.Sessions {
		if current.ID == m.current.ID {
			m.current = current
			return
		}
	}
}
