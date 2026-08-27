package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

type screen uint8

const (
	screenTasks screen = iota
	screenAddTask
	screenSettings
	screenWorkspace
	screenAddManager
)

type focus uint8

const (
	focusList focus = iota
	focusInput
	focusLabel
)

type taskFilter uint8

const (
	filterActive taskFilter = iota
	filterAll
)

type actionKind uint8

const (
	actionNone actionKind = iota
	actionReload
	actionAddTask
	actionSetEnabled
	actionAuthenticate
	actionRecheck
	actionAddManager
	actionAttach
	actionDetach
	actionSend
	actionResume
	actionStop
)

type paneFrame struct {
	sequence uint64
	text     string
	bytes    int
}

type pane struct {
	frames         []paneFrame
	bytes          int
	evicted        bool
	offsetFromTail int
	followTail     bool
}

type sessionCount struct {
	active  int
	dormant int
	total   int
}

// treeNode is the content-free, pre-indexed hierarchy used by rendering and
// mouse targeting. Formatting remains viewport-local.
type treeNode struct {
	sessionID    SessionID
	managerIndex int
}

// attachAttempt and detachAttempt serialize viewer ownership without relying
// on Bubble Tea command completion order. Their once/done pairs also let Back
// and quit join the same in-flight operation without issuing duplicates.
type attachAttempt struct {
	id     uint64
	task   TaskID
	ctx    context.Context
	cancel context.CancelFunc
	before *detachAttempt
	done   chan struct{}
	once   sync.Once
	err    error
}

func (a *attachAttempt) run(runtime RuntimeActions) error {
	a.once.Do(func() {
		defer close(a.done)
		if a.before != nil {
			if err := a.before.run(runtime); err != nil {
				a.err = err
				return
			}
		}
		a.err = runtime.Attach(a.ctx, a.task)
	})
	<-a.done
	return a.err
}

type detachAttempt struct {
	task      TaskID
	ctx       context.Context
	attach    *attachAttempt
	done      chan struct{}
	once      sync.Once
	attachErr error
	err       error
}

func (d *detachAttempt) run(runtime RuntimeActions) error {
	d.once.Do(func() {
		defer close(d.done)
		if d.attach != nil {
			d.attach.cancel()
			d.attachErr = d.attach.run(runtime)
		}
		// Detach is still required when cancellation raced with an Attach;
		// only the runtime can mechanically establish that no viewer remains.
		d.err = runtime.Detach(d.ctx, d.task)
	})
	<-d.done
	return d.err
}

func newPane() *pane { return &pane{followTail: true} }

func (p *pane) append(frame paneFrame, limits Limits) {
	if !p.followTail {
		p.offsetFromTail += 1 + strings.Count(frame.text, "\n")
	}
	p.frames = append(p.frames, frame)
	p.bytes += frame.bytes
	for len(p.frames) > limits.PaneFrames || p.bytes > limits.PaneBytes {
		removed := p.frames[0]
		p.frames = p.frames[1:]
		p.bytes -= removed.bytes
		p.evicted = true
	}
	if len(p.frames) == 0 {
		p.offsetFromTail = 0
		p.followTail = true
	}
}

func (p *pane) scrollUp(lines int) {
	if lines < 1 || len(p.frames) == 0 {
		return
	}
	p.followTail = false
	p.offsetFromTail += lines
}

func (p *pane) scrollDown(lines int) {
	if lines < 1 {
		return
	}
	p.offsetFromTail -= lines
	if p.offsetFromTail <= 0 {
		p.offsetFromTail = 0
		p.followTail = true
	}
}

// Model is TaskAI's production Bubble Tea v2 model. All public ACP content,
// drafts, selection, cursor, pane, and scroll state lives only in this value.
type Model struct {
	ctx    context.Context
	cancel context.CancelFunc

	catalog   Catalog
	tasks     TaskActions
	providers ProviderActions
	runtime   RuntimeActions
	events    EventSource
	limits    Limits

	initOnce sync.Once

	width    int
	height   int
	screen   screen
	returnTo screen
	focus    focus
	filter   taskFilter

	loaded   bool
	snapshot Snapshot

	tasksByID          map[TaskID]Task
	sessionsByID       map[SessionID]Session
	providersByID      map[ProviderID]Provider
	sessionsByTask     map[TaskID][]Session
	managersByTask     map[TaskID][]Session
	childrenByManager  map[SessionID][]Session
	countsByTask       map[TaskID]sessionCount
	treeNodesByTask    map[TaskID][]treeNode
	treeIndexByManager map[SessionID]int
	managerProviderSet []Provider
	providerEnabled    bool

	selectedTask     TaskID
	selectedProvider ProviderID
	currentTask      TaskID
	selectedManager  SessionID
	addProvider      ProviderID

	taskDraft    editor
	managerLabel editor
	drafts       map[SessionID]editor
	panes        map[SessionID]*pane
	treeOffset   int

	help                 bool
	details              bool
	detailsReturnFocus   focus
	detailsReturnSession SessionID
	detailIndex          int
	detailScroll         int
	confirmStop          bool
	status               string

	actionSerial   uint64
	pending        actionKind
	pendingToken   uint64
	pendingText    string
	pendingSession SessionID
	attachedTask   TaskID
	pendingAttach  *attachAttempt
	detachOp       *detachAttempt

	streamGeneration uint64
	streamSequence   uint64
	streamStarted    bool
	streamWaiting    bool
	streamCtx        context.Context
	streamCancel     context.CancelFunc
}

var _ tea.Model = (*Model)(nil)

// New constructs a production TUI model without starting I/O. Init performs
// one metadata load; it never starts authentication or provider processes.
func New(config Config) (*Model, error) {
	if config.Catalog == nil || config.Tasks == nil || config.Providers == nil || config.Runtime == nil {
		return nil, errors.New("tui: all metadata and action ports are required")
	}
	parent := config.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	limits := normalizeLimits(config.Limits)
	m := &Model{
		ctx:          ctx,
		cancel:       cancel,
		catalog:      config.Catalog,
		tasks:        config.Tasks,
		providers:    config.Providers,
		runtime:      config.Runtime,
		events:       config.Events,
		limits:       limits,
		width:        80,
		height:       24,
		screen:       screenTasks,
		returnTo:     screenTasks,
		focus:        focusList,
		filter:       filterActive,
		taskDraft:    newEditor(limits.DraftBytes),
		managerLabel: newEditor(limits.DraftBytes),
		drafts:       make(map[SessionID]editor),
		panes:        make(map[SessionID]*pane),
	}
	return m, nil
}

// Close cancels only TUI-owned one-shot waits. It does not stop a Manager,
// close an ACP port, delete metadata, or mutate provider-owned state.
func (m *Model) Close() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
	m.cancel()
}

// Init loads metadata once. Event waiting begins only after a validated
// snapshot supplies the stream generation.
func (m *Model) Init() tea.Cmd {
	var cmd tea.Cmd
	m.initOnce.Do(func() { cmd = m.loadCmd(0, actionReload) })
	return cmd
}

type snapshotMsg struct {
	token        uint64
	action       actionKind
	snapshot     Snapshot
	hasSnapshot  bool
	selected     SessionID
	selectedTask TaskID
	refreshErr   error
	err          error
}

type actionMsg struct {
	token  uint64
	action actionKind
	task   TaskID
	attach *attachAttempt
	detach *detachAttempt
	err    error
}

type streamMsg struct {
	generation uint64
	after      uint64
	event      PublicEvent
	err        error
}

type mouseActionMsg struct {
	action mouseAction
	index  int
}

type mouseWheelMsg struct{ delta int }

func (m *Model) loadCmd(token uint64, action actionKind) tea.Cmd {
	ctx := m.ctx
	catalog := m.catalog
	return func() tea.Msg {
		snapshot, err := catalog.Snapshot(ctx)
		return snapshotMsg{token: token, action: action, snapshot: snapshot, hasSnapshot: err == nil, err: err}
	}
}

func (m *Model) refreshAfter(token uint64, action actionKind, selected SessionID, fn func(context.Context) error) tea.Cmd {
	ctx := m.ctx
	catalog := m.catalog
	return func() tea.Msg {
		actionErr := fn(ctx)
		// Lifecycle operations can persist truthful interrupted metadata before
		// returning an error. Always refresh, while retaining both independent
		// failures so presentation never substitutes one raw error for another.
		snapshot, refreshErr := catalog.Snapshot(ctx)
		return snapshotMsg{
			token: token, action: action, snapshot: snapshot,
			hasSnapshot: refreshErr == nil, selected: selected,
			refreshErr: refreshErr, err: actionErr,
		}
	}
}

func (m *Model) authenticateCmd(token uint64, provider ProviderID) tea.Cmd {
	interactive, ok := m.providers.(ProviderAuthExec)
	if !ok {
		return m.refreshAfter(token, actionAuthenticate, "", func(ctx context.Context) error {
			return m.providers.Authenticate(ctx, provider)
		})
	}
	command, err := interactive.AuthenticationCommand(m.ctx, provider)
	if err != nil || command == nil {
		if err == nil {
			err = errors.New("authentication command unavailable")
		}
		return m.refreshAfter(token, actionAuthenticate, "", func(context.Context) error { return err })
	}
	ctx := m.ctx
	catalog := m.catalog
	return tea.Exec(command, func(actionErr error) tea.Msg {
		snapshot, refreshErr := catalog.Snapshot(ctx)
		return snapshotMsg{
			token: token, action: actionAuthenticate, snapshot: snapshot,
			hasSnapshot: refreshErr == nil, refreshErr: refreshErr, err: actionErr,
		}
	})
}

func (m *Model) simpleAction(token uint64, action actionKind, task TaskID, fn func(context.Context) error) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg { return actionMsg{token: token, action: action, task: task, err: fn(ctx)} }
}

func (m *Model) attachCmd(token uint64, attempt *attachAttempt) tea.Cmd {
	runtime := m.runtime
	return func() tea.Msg {
		return actionMsg{
			token: token, action: actionAttach, task: attempt.task,
			attach: attempt, err: attempt.run(runtime),
		}
	}
}

func (m *Model) detachCmd(attempt *detachAttempt) tea.Cmd {
	if attempt == nil {
		return nil
	}
	runtime := m.runtime
	return func() tea.Msg {
		return actionMsg{
			action: actionDetach, task: attempt.task,
			detach: attempt, err: attempt.run(runtime),
		}
	}
}

func (m *Model) addManagerCmd(token uint64, taskID TaskID, providerID ProviderID, label string) tea.Cmd {
	ctx := m.ctx
	actions := m.tasks
	catalog := m.catalog
	return func() tea.Msg {
		selected, actionErr := actions.AddManager(ctx, taskID, providerID, label)
		// A connector may have created mechanically truthful starting or
		// interrupted metadata before returning an error. Refresh even on error.
		snapshot, snapshotErr := catalog.Snapshot(ctx)
		if snapshotErr != nil {
			if actionErr != nil {
				return snapshotMsg{token: token, action: actionAddManager, selected: selected, err: actionErr}
			}
			return snapshotMsg{token: token, action: actionAddManager, selected: selected, err: snapshotErr}
		}
		return snapshotMsg{token: token, action: actionAddManager, snapshot: snapshot, hasSnapshot: true, selected: selected, err: actionErr}
	}
}

func (m *Model) addTaskCmd(token uint64, title string) tea.Cmd {
	ctx := m.ctx
	actions := m.tasks
	catalog := m.catalog
	return func() tea.Msg {
		selected, err := actions.AddTask(ctx, title)
		if err != nil {
			return snapshotMsg{token: token, action: actionAddTask, err: err}
		}
		snapshot, err := catalog.Snapshot(ctx)
		return snapshotMsg{token: token, action: actionAddTask, snapshot: snapshot, hasSnapshot: err == nil, selectedTask: selected, err: err}
	}
}

func (m *Model) armStream() tea.Cmd {
	if m.events == nil || !m.streamStarted || m.streamWaiting {
		return nil
	}
	m.streamWaiting = true
	generation := m.streamGeneration
	after := m.streamSequence
	ctx := m.streamCtx
	events := m.events
	return func() tea.Msg {
		event, err := events.Next(ctx, StreamCursor{Generation: generation, After: after})
		return streamMsg{generation: generation, after: after, event: event, err: err}
	}
}

func (m *Model) resetStream(generation, through uint64) {
	if m.streamCancel != nil {
		m.streamCancel()
	}
	m.streamCtx, m.streamCancel = context.WithCancel(m.ctx)
	m.streamGeneration = generation
	m.streamSequence = through
	m.streamStarted = true
	m.streamWaiting = false
	// Content belongs to the superseded live stream and must never cross into
	// a replacement provider generation.
	m.panes = make(map[SessionID]*pane)
}

// Update applies terminal, metadata, and connector events serially.
func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, msg.Width)
		m.height = max(1, msg.Height)
		m.ensureSelectedManagerVisible()
		return m, nil
	case snapshotMsg:
		return m, m.handleSnapshot(msg)
	case actionMsg:
		m.handleAction(msg)
		return m, nil
	case streamMsg:
		return m, m.handleStream(msg)
	case tea.PasteMsg:
		m.handlePaste(msg.Content)
		return m, nil
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	case tea.MouseClickMsg:
		return m, m.handleRawMouse(msg)
	case tea.MouseWheelMsg:
		return m, m.handleRawMouse(msg)
	case mouseActionMsg:
		return m, m.handleMouseAction(msg)
	case mouseWheelMsg:
		m.scrollPane(msg.delta)
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) handleSnapshot(msg snapshotMsg) tea.Cmd {
	if msg.action != actionReload && (m.pending == actionNone || msg.token != m.pendingToken || msg.action != m.pending) {
		return nil
	}
	if msg.action == actionReload && msg.token != 0 && (m.pending != actionReload || msg.token != m.pendingToken) {
		return nil
	}
	if msg.action != actionReload || msg.token != 0 {
		m.pending = actionNone
		m.pendingText = ""
	}
	if msg.refreshErr != nil {
		// Snapshot failure is primary because no fresh metadata can be applied;
		// a simultaneous action SafeError remains independently visible.
		m.status = safeFailure("Metadata refresh", msg.refreshErr)
		if msg.err != nil {
			m.status += "; " + safeFailure(actionLabel(msg.action), msg.err)
		}
		m.loaded = true
		return nil
	}
	hasSnapshot := msg.hasSnapshot || msg.err == nil
	if hasSnapshot {
		if err := validateSnapshot(msg.snapshot); err != nil {
			m.status = "Metadata unavailable: invalid snapshot"
			if msg.err != nil {
				m.status += "; " + safeFailure(actionLabel(msg.action), msg.err)
			}
			m.loaded = true
			return nil
		}
		if m.streamStarted && msg.snapshot.StreamGeneration == m.streamGeneration &&
			msg.snapshot.ThroughSequence < m.streamSequence {
			m.status = "Metadata refresh rejected: stale stream watermark"
			if msg.err != nil {
				m.status = safeFailure(actionLabel(msg.action), msg.err) + "; refresh rejected: stale stream watermark"
			}
			m.loaded = true
			return m.armStream()
		}
		oldGeneration := m.streamGeneration
		m.applySnapshot(msg.snapshot)
		m.loaded = true
		if !m.streamStarted || oldGeneration != msg.snapshot.StreamGeneration {
			m.resetStream(msg.snapshot.StreamGeneration, msg.snapshot.ThroughSequence)
		} else if m.events == nil && msg.snapshot.ThroughSequence > m.streamSequence {
			// Without an event port there is nothing to catch up. The atomic
			// metadata snapshot is the only sequence authority.
			m.streamSequence = msg.snapshot.ThroughSequence
		}
	} else {
		m.loaded = true
	}

	if hasSnapshot {
		switch msg.action {
		case actionAddTask:
			if msg.err == nil {
				m.taskDraft.clear()
			}
			m.screen = screenTasks
			m.focus = focusList
			if m.hasTask(msg.selectedTask) {
				m.selectedTask = msg.selectedTask
			}
		case actionAddManager:
			if msg.err == nil {
				m.managerLabel.clear()
			}
			m.screen = screenWorkspace
			m.focus = focusList
			if m.isManagerInTask(msg.selected, m.currentTask) {
				m.selectedManager = msg.selected
			}
		}
	}
	if msg.err != nil {
		m.status = safeFailure(actionLabel(msg.action), msg.err)
	} else {
		m.status = ""
	}
	return m.armStream()
}

func (m *Model) handleAction(msg actionMsg) {
	if msg.action == actionDetach && msg.detach != nil {
		current := m.detachOp == msg.detach
		if current {
			m.detachOp = nil
		}
		if msg.err != nil {
			// Conservatively retain exact ownership so quit/back can retry; a
			// failed Detach cannot prove the viewer is gone.
			if current {
				m.attachedTask = msg.task
			}
			m.status = safeFailure(actionLabel(msg.action), msg.err)
		}
		return
	}
	if msg.action == actionAttach && msg.attach != nil {
		current := m.pendingAttach == msg.attach
		if current {
			m.pendingAttach = nil
		}
		if m.pending == actionAttach && msg.token == m.pendingToken {
			m.pending = actionNone
			m.pendingText = ""
			m.pendingSession = ""
		}
		if !current {
			return
		}
		if msg.err != nil {
			if msg.attach.before != nil && msg.attach.before.err != nil {
				m.attachedTask = msg.attach.before.task
			} else {
				m.attachedTask = ""
			}
			m.status = safeFailure(actionLabel(msg.action), msg.err)
			return
		}
		m.attachedTask = msg.task
		m.status = ""
		return
	}
	if m.pending == actionNone || msg.token != m.pendingToken || msg.action != m.pending {
		return
	}
	pendingText := m.pendingText
	pendingSession := m.pendingSession
	m.pending = actionNone
	m.pendingText = ""
	m.pendingSession = ""
	if msg.err != nil {
		m.status = safeFailure(actionLabel(msg.action), msg.err)
		return
	}
	m.status = ""
	if msg.action == actionSend {
		draft := m.drafts[pendingSession]
		if draft.value == pendingText {
			delete(m.drafts, pendingSession)
		}
	}
}

func (m *Model) handleStream(msg streamMsg) tea.Cmd {
	if msg.generation != m.streamGeneration {
		return nil
	}
	m.streamWaiting = false
	if msg.after != m.streamSequence {
		m.status = "Live updates stopped: stale cursor"
		return nil
	}
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) {
			m.status = safeFailure("Live updates", msg.err)
		}
		return nil
	}
	event := msg.event
	if event.Generation != m.streamGeneration || event.Sequence != m.streamSequence+1 {
		m.status = "Live updates stopped: out-of-order event"
		return nil
	}
	if err := m.applyEvent(event); err != nil {
		m.status = "Live updates stopped: invalid event"
		return nil
	}
	m.streamSequence = event.Sequence
	if event.Sequence > m.snapshot.ThroughSequence {
		m.snapshot.ThroughSequence = event.Sequence
	}
	return m.armStream()
}

func (m *Model) applyEvent(event PublicEvent) error {
	// A same-generation metadata snapshot may legitimately be ahead of the UI
	// cursor. Continue consuming the intervening stream so public frames are
	// never dropped, but do not let an older metadata event overwrite the
	// atomically newer snapshot.
	if event.Sequence <= m.snapshot.ThroughSequence && event.Kind != EventPublicFrame {
		return validateSupersededMetadataEvent(event)
	}
	switch event.Kind {
	case EventPublicFrame:
		session, ok := m.session(event.SessionID)
		if !ok || session.Role != RoleManager || session.TaskID != event.TaskID {
			return errInvalidSnapshot
		}
		text, err := renderPublicFrame(event.Frame, m.limits)
		if err != nil {
			return err
		}
		p := m.panes[event.SessionID]
		if p == nil {
			p = newPane()
			m.panes[event.SessionID] = p
		}
		p.append(paneFrame{sequence: event.Sequence, text: text, bytes: len(text)}, m.limits)
		m.enforcePaneAggregate()
	case EventSessionUpsert:
		if event.Session.ID == "" || event.Session.ID != event.SessionID || event.Session.TaskID != event.TaskID {
			return errInvalidSnapshot
		}
		candidate := cloneSnapshot(m.snapshot)
		candidate.ThroughSequence = event.Sequence
		replaced := false
		for i := range candidate.Sessions {
			if candidate.Sessions[i].ID == event.SessionID {
				old := candidate.Sessions[i]
				if old.TaskID != event.Session.TaskID || old.ParentID != event.Session.ParentID ||
					old.Role != event.Session.Role || old.ProviderID != event.Session.ProviderID {
					return errInvalidSnapshot
				}
				candidate.Sessions[i] = event.Session
				replaced = true
				break
			}
		}
		if !replaced {
			candidate.Sessions = append(candidate.Sessions, event.Session)
		}
		if err := validateSnapshot(candidate); err != nil {
			return err
		}
		m.applySnapshot(candidate)
	case EventSessionRemove:
		candidate := cloneSnapshot(m.snapshot)
		candidate.ThroughSequence = event.Sequence
		found := false
		for i := range candidate.Sessions {
			if candidate.Sessions[i].ID == event.SessionID && candidate.Sessions[i].TaskID == event.TaskID {
				candidate.Sessions = append(candidate.Sessions[:i], candidate.Sessions[i+1:]...)
				found = true
				break
			}
		}
		if !found || validateSnapshot(candidate) != nil {
			return errInvalidSnapshot
		}
		m.applySnapshot(candidate)
		delete(m.panes, event.SessionID)
		delete(m.drafts, event.SessionID)
	default:
		return errInvalidSnapshot
	}
	return nil
}

func validateSupersededMetadataEvent(event PublicEvent) error {
	if event.TaskID == "" || len(event.TaskID) > maxIDBytes ||
		event.SessionID == "" || len(event.SessionID) > maxIDBytes {
		return errInvalidSnapshot
	}
	switch event.Kind {
	case EventSessionUpsert:
		if event.Session.ID != event.SessionID || event.Session.TaskID != event.TaskID {
			return errInvalidSnapshot
		}
		return validateSessionShape(event.Session)
	case EventSessionRemove:
		return nil
	default:
		return errInvalidSnapshot
	}
}

func renderPublicFrame(frame PublicFrame, limits Limits) (string, error) {
	switch frame.Kind {
	case FrameText:
		payload := frame.Payload
		truncated := len(payload) > limits.FrameBytes
		if truncated {
			payload = payload[:limits.FrameBytes]
			for len(payload) > 0 && !utf8.Valid(payload) {
				payload = payload[:len(payload)-1]
			}
		}
		text := safeMultilineBytes(payload)
		if truncated {
			text += "\n[public frame truncated]"
		}
		return text, nil
	case FrameUnknown:
		kind := safeInlinePrefix(frame.PublicType, limits.UnknownTypeBytes)
		return fmt.Sprintf("[unknown public ACP type=%q bytes=%d]", kind, len(frame.Payload)), nil
	default:
		return "", errInvalidSnapshot
	}
}

func (m *Model) applySnapshot(snapshot Snapshot) {
	m.snapshot = cloneSnapshot(snapshot)
	m.snapshot.Tasks = sortedTasks(m.snapshot.Tasks)
	m.snapshot.Sessions = sortedSessions(m.snapshot.Sessions)
	m.rebuildDerived()

	visible := m.visibleTasks()
	if !containsTask(visible, m.selectedTask) {
		m.selectedTask = firstTask(visible)
	}
	if !m.hasProvider(m.selectedProvider) {
		m.selectedProvider = firstProvider(m.snapshot.Providers)
	}
	if m.currentTask != "" && !m.hasTask(m.currentTask) {
		m.currentTask = ""
		m.screen = screenTasks
	}
	if m.currentTask != "" && !m.isManagerInTask(m.selectedManager, m.currentTask) {
		m.selectedManager = firstManager(m.sessionsForTask(m.currentTask))
	}
	validManagers := make(map[SessionID]struct{})
	for _, session := range m.snapshot.Sessions {
		if session.Role == RoleManager {
			validManagers[session.ID] = struct{}{}
		}
	}
	for id := range m.drafts {
		if _, ok := validManagers[id]; !ok {
			delete(m.drafts, id)
		}
	}
	for id := range m.panes {
		if _, ok := validManagers[id]; !ok {
			delete(m.panes, id)
		}
	}
	if m.focus == focusInput && !m.managerInputAvailable() {
		m.focus = focusList
	}
	if m.details {
		sessions := m.detailSessions()
		if len(sessions) == 0 {
			m.closeSessionDetails()
		} else {
			m.detailIndex = clamp(m.detailIndex, 0, len(sessions)-1)
		}
	}
	providers := m.managerProviders()
	if !containsProvider(providers, m.addProvider) {
		m.addProvider = firstProvider(providers)
	}
}

func (m *Model) rebuildDerived() {
	m.tasksByID = make(map[TaskID]Task, len(m.snapshot.Tasks))
	m.sessionsByID = make(map[SessionID]Session, len(m.snapshot.Sessions))
	m.providersByID = make(map[ProviderID]Provider, len(m.snapshot.Providers))
	m.sessionsByTask = make(map[TaskID][]Session)
	m.managersByTask = make(map[TaskID][]Session)
	m.childrenByManager = make(map[SessionID][]Session)
	m.countsByTask = make(map[TaskID]sessionCount, len(m.snapshot.Tasks))
	m.treeNodesByTask = make(map[TaskID][]treeNode)
	m.treeIndexByManager = make(map[SessionID]int)
	m.managerProviderSet = m.managerProviderSet[:0]
	m.providerEnabled = false

	for _, task := range m.snapshot.Tasks {
		m.tasksByID[task.ID] = task
	}
	for _, provider := range m.snapshot.Providers {
		m.providersByID[provider.ID] = provider
		m.providerEnabled = m.providerEnabled || provider.Enabled
		if provider.Enabled && provider.Capabilities.Manager {
			m.managerProviderSet = append(m.managerProviderSet, provider)
		}
	}
	for _, session := range m.snapshot.Sessions {
		m.sessionsByID[session.ID] = session
		m.sessionsByTask[session.TaskID] = append(m.sessionsByTask[session.TaskID], session)
		count := m.countsByTask[session.TaskID]
		count.total++
		if session.State == StateStarting || session.State == StateActive {
			count.active++
		} else {
			count.dormant++
		}
		m.countsByTask[session.TaskID] = count
		if session.Role == RoleManager {
			m.managersByTask[session.TaskID] = append(m.managersByTask[session.TaskID], session)
		} else {
			m.childrenByManager[session.ParentID] = append(m.childrenByManager[session.ParentID], session)
		}
	}
	for taskID, managers := range m.managersByTask {
		nodes := make([]treeNode, 0, len(m.sessionsByTask[taskID]))
		for managerIndex, manager := range managers {
			m.treeIndexByManager[manager.ID] = len(nodes)
			nodes = append(nodes, treeNode{sessionID: manager.ID, managerIndex: managerIndex})
			for _, child := range m.childrenByManager[manager.ID] {
				nodes = append(nodes, treeNode{sessionID: child.ID, managerIndex: managerIndex})
			}
		}
		m.treeNodesByTask[taskID] = nodes
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Tasks = append([]Task(nil), snapshot.Tasks...)
	snapshot.Sessions = append([]Session(nil), snapshot.Sessions...)
	snapshot.Providers = append([]Provider(nil), snapshot.Providers...)
	return snapshot
}

func (m *Model) begin(action actionKind) uint64 {
	if m.pending != actionNone {
		return 0
	}
	m.actionSerial++
	m.pending = action
	m.pendingToken = m.actionSerial
	m.status = ""
	return m.pendingToken
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return m.quitCmd()
	}
	if m.help {
		if key == "esc" || key == "?" || key == "enter" {
			m.help = false
		}
		return nil
	}
	if m.details {
		switch key {
		case "esc", "b", "enter", "v":
			m.closeSessionDetails()
		case "left", "h":
			m.moveDetail(-1)
		case "right", "l":
			m.moveDetail(1)
		case "up", "k":
			m.detailScroll = max(0, m.detailScroll-1)
		case "down", "j":
			m.detailScroll = min(maxSnapshotMetadataBytes, m.detailScroll+1)
		case "pgup":
			m.detailScroll = max(0, m.detailScroll-max(1, m.height-5))
		case "pgdown":
			m.detailScroll = min(maxSnapshotMetadataBytes, m.detailScroll+max(1, m.height-5))
		case "f1", "?":
			m.help = true
		}
		return nil
	}
	if m.confirmStop {
		switch key {
		case "esc", "n":
			m.confirmStop = false
		case "enter", "y":
			m.confirmStop = false
			return m.stopSelected()
		}
		return nil
	}

	if key == "f1" || (key == "?" && !m.editing()) {
		m.help = true
		return nil
	}
	switch m.screen {
	case screenTasks:
		return m.keyTasks(msg)
	case screenAddTask:
		return m.keyAddTask(msg)
	case screenSettings:
		return m.keySettings(msg)
	case screenWorkspace:
		return m.keyWorkspace(msg)
	case screenAddManager:
		return m.keyAddManager(msg)
	default:
		return nil
	}
}

func (m *Model) keyTasks(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "q":
		return m.quitCmd()
	case "tab":
		if m.filter == filterActive {
			m.filter = filterAll
		} else {
			m.filter = filterActive
		}
		m.ensureTaskSelection()
	case "up", "k":
		m.moveTask(-1)
	case "down", "j":
		m.moveTask(1)
	case "enter", "o":
		return m.openSelectedTask()
	case "a":
		m.screen = screenAddTask
		m.focus = focusInput
	case "s":
		m.returnTo = screenTasks
		m.screen = screenSettings
		m.focus = focusList
	case "r":
		return m.reload()
	}
	return nil
}

func (m *Model) keyAddTask(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.screen = screenTasks
		m.focus = focusList
		return nil
	case "enter":
		if m.taskDraft.value == "" {
			m.status = "Task title is required"
			return nil
		}
		token := m.begin(actionAddTask)
		if token == 0 {
			return nil
		}
		title := m.taskDraft.value
		return m.addTaskCmd(token, title)
	default:
		if m.pending == actionNone {
			handleEditorKey(&m.taskDraft, msg)
		}
		return nil
	}
}

func (m *Model) keySettings(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "b":
		m.screen = m.returnTo
		if m.screen == screenWorkspace && m.currentTask == "" {
			m.screen = screenTasks
		}
	case "up", "k":
		m.moveProvider(-1)
	case "down", "j":
		m.moveProvider(1)
	case " ", "space", "e":
		provider, ok := m.selectedProviderValue()
		if !ok {
			return nil
		}
		token := m.begin(actionSetEnabled)
		if token == 0 {
			return nil
		}
		return m.refreshAfter(token, actionSetEnabled, "", func(ctx context.Context) error {
			return m.providers.SetEnabled(ctx, provider.ID, !provider.Enabled)
		})
	case "a":
		provider, ok := m.selectedProviderValue()
		if !ok {
			return nil
		}
		token := m.begin(actionAuthenticate)
		if token == 0 {
			return nil
		}
		return m.authenticateCmd(token, provider.ID)
	case "r":
		provider, ok := m.selectedProviderValue()
		if !ok {
			return nil
		}
		token := m.begin(actionRecheck)
		if token == 0 {
			return nil
		}
		return m.refreshAfter(token, actionRecheck, "", func(ctx context.Context) error {
			return m.providers.Recheck(ctx, provider.ID)
		})
	}
	return nil
}

func (m *Model) keyWorkspace(msg tea.KeyPressMsg) tea.Cmd {
	if m.focus == focusInput {
		if !m.managerInputAvailable() {
			m.focus = focusList
			return nil
		}
		switch msg.String() {
		case "esc", "tab":
			m.focus = focusList
			return nil
		case "enter":
			return m.sendDraft()
		default:
			if m.pending == actionNone {
				m.handleManagerEditorKey(msg)
			}
			return nil
		}
	}

	switch msg.String() {
	case "esc", "b", "d":
		return m.leaveWorkspace()
	case "tab", "i":
		if m.managerInputAvailable() {
			m.focus = focusInput
		} else if manager, ok := m.session(m.selectedManager); ok {
			m.status = "Manager input unavailable in " + sessionStateLabel(manager) + " state"
		}
	case "up", "k":
		m.moveManager(-1)
	case "down", "j":
		m.moveManager(1)
	case "enter", "v":
		m.openSessionDetails(m.selectedManager)
	case "a":
		m.openAddManager()
	case "x":
		if m.selectedManager != "" {
			m.confirmStop = true
		}
	case "pgup", "u":
		m.scrollPane(max(1, m.paneHeight()/2))
	case "pgdown":
		m.scrollPane(-max(1, m.paneHeight()/2))
	case "end", "G":
		if p := m.panes[m.selectedManager]; p != nil {
			p.offsetFromTail = 0
			p.followTail = true
		}
	case "r":
		return m.resumeSelected()
	}
	return nil
}

func (m *Model) detailSessions() []Session {
	manager, ok := m.session(m.selectedManager)
	if !ok || manager.Role != RoleManager || manager.TaskID != m.currentTask {
		return nil
	}
	return append([]Session{manager}, m.childrenOf(manager.ID)...)
}

func (m *Model) openSessionDetails(id SessionID) {
	sessions := m.detailSessions()
	if len(sessions) == 0 {
		return
	}
	if !m.details {
		m.detailsReturnFocus = m.focus
		m.detailsReturnSession = m.selectedManager
	}
	m.detailIndex = 0
	for index, session := range sessions {
		if session.ID == id {
			m.detailIndex = index
			break
		}
	}
	m.detailScroll = 0
	m.details = true
	m.focus = focusList
}

func (m *Model) closeSessionDetails() {
	returnFocus := m.detailsReturnFocus
	returnSession := m.detailsReturnSession
	m.details = false
	m.detailsReturnSession = ""
	m.detailIndex = 0
	m.detailScroll = 0
	if returnFocus == focusInput && m.selectedManager == returnSession && m.managerInputAvailable() {
		m.focus = focusInput
		return
	}
	m.focus = focusList
}

func (m *Model) moveDetail(delta int) {
	sessions := m.detailSessions()
	if len(sessions) == 0 {
		m.closeSessionDetails()
		return
	}
	m.detailIndex = clamp(m.detailIndex+delta, 0, len(sessions)-1)
	m.detailScroll = 0
}

func (m *Model) keyAddManager(msg tea.KeyPressMsg) tea.Cmd {
	providers := m.managerProviders()
	if len(providers) == 0 {
		switch msg.String() {
		case "esc", "b":
			m.screen = screenWorkspace
		case "enter", "s":
			m.returnTo = screenAddManager
			m.screen = screenSettings
			m.focus = focusList
		}
		return nil
	}
	if m.focus == focusLabel {
		switch msg.String() {
		case "esc":
			m.screen = screenWorkspace
			m.focus = focusList
		case "tab":
			m.focus = focusList
		case "enter":
			return m.createManager()
		default:
			if m.pending == actionNone {
				handleEditorKey(&m.managerLabel, msg)
			}
		}
		return nil
	}
	switch msg.String() {
	case "esc", "b":
		m.screen = screenWorkspace
	case "tab", "l":
		m.focus = focusLabel
	case "up", "k":
		m.moveAddProvider(-1)
	case "down", "j":
		m.moveAddProvider(1)
	case "enter":
		return m.createManager()
	}
	return nil
}

func handleEditorKey(editor *editor, msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "left":
		editor.left()
	case "right":
		editor.right()
	case "home":
		editor.home()
	case "end":
		editor.end()
	case "backspace":
		editor.backspace()
	case "delete":
		editor.delete()
	default:
		text := msg.Key().Text
		if text == "" {
			return false
		}
		editor.insert(text)
	}
	return true
}

func (m *Model) handlePaste(content string) {
	if m.pending != actionNone {
		return
	}
	switch {
	case m.screen == screenAddTask:
		m.taskDraft.insert(content)
	case m.screen == screenAddManager && m.focus == focusLabel:
		m.managerLabel.insert(content)
	case m.screen == screenWorkspace && m.focus == focusInput && m.managerInputAvailable():
		m.insertManagerText(content)
	}
}

func (m *Model) editing() bool {
	return m.screen == screenAddTask ||
		(m.screen == screenAddManager && m.focus == focusLabel) ||
		(m.screen == screenWorkspace && m.focus == focusInput)
}

func (m *Model) reload() tea.Cmd {
	token := m.begin(actionReload)
	if token == 0 {
		return nil
	}
	return m.loadCmd(token, actionReload)
}

func (m *Model) openSelectedTask() tea.Cmd {
	if m.selectedTask == "" || !m.hasTask(m.selectedTask) {
		return nil
	}
	taskID := m.selectedTask
	alreadyAttached := m.attachedTask == taskID && m.pendingAttach == nil
	var token uint64
	if !alreadyAttached {
		// Reserve the lifecycle slot before exposing a workspace. A pending
		// action must leave Tasks and currentTask completely unchanged.
		token = m.begin(actionAttach)
		if token == 0 {
			return nil
		}
	}
	var before *detachAttempt
	if !alreadyAttached {
		if m.attachedTask != "" {
			before = m.startDetach()
		} else {
			before = m.detachOp
		}
	}
	changedTask := m.currentTask != taskID
	m.currentTask = taskID
	m.screen = screenWorkspace
	m.focus = focusList
	if changedTask {
		m.treeOffset = 0
		m.selectedManager = firstManager(m.sessionsForTask(m.currentTask))
	} else if !m.isManagerInTask(m.selectedManager, m.currentTask) {
		m.selectedManager = firstManager(m.sessionsForTask(m.currentTask))
	}
	if alreadyAttached {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	attempt := &attachAttempt{
		id: token, task: taskID, ctx: ctx, cancel: cancel,
		before: before, done: make(chan struct{}),
	}
	m.pendingAttach = attempt
	return m.attachCmd(token, attempt)
}

func (m *Model) leaveWorkspace() tea.Cmd {
	if m.currentTask == "" {
		m.screen = screenTasks
		return nil
	}
	m.screen = screenTasks
	m.focus = focusList
	return m.detachCmd(m.startDetach())
}

func (m *Model) quitCmd() tea.Cmd {
	attempt := m.startDetach()
	runtime := m.runtime
	return func() tea.Msg {
		// Terminal ownership must be released even when a broken transport does
		// not answer Detach. Cancelling the model first makes the best-effort
		// Detach finite without stopping the worker-owned Manager tree.
		m.Close()
		if attempt != nil {
			_ = attempt.run(runtime)
		}
		return tea.Quit()
	}
}

func (m *Model) startDetach() *detachAttempt {
	var taskID TaskID
	var attach *attachAttempt
	if m.pendingAttach != nil {
		attach = m.pendingAttach
		taskID = attach.task
		attach.cancel()
		m.pendingAttach = nil
		if m.pending == actionAttach && m.pendingToken == attach.id {
			m.pending = actionNone
			m.pendingText = ""
			m.pendingSession = ""
		}
	} else if m.attachedTask != "" {
		taskID = m.attachedTask
	} else {
		return m.detachOp
	}
	m.attachedTask = ""
	attempt := &detachAttempt{
		task: taskID, ctx: m.ctx, attach: attach, done: make(chan struct{}),
	}
	m.detachOp = attempt
	return attempt
}

func (m *Model) openAddManager() {
	m.screen = screenAddManager
	m.focus = focusList
	providers := m.managerProviders()
	if !containsProvider(providers, m.addProvider) {
		m.addProvider = firstProvider(providers)
	}
}

func (m *Model) createManager() tea.Cmd {
	provider, ok := m.provider(m.addProvider)
	if !ok || !provider.Enabled || !provider.Capabilities.Manager || m.currentTask == "" {
		m.status = "Manager provider unavailable; open Settings"
		return nil
	}
	token := m.begin(actionAddManager)
	if token == 0 {
		return nil
	}
	return m.addManagerCmd(token, m.currentTask, provider.ID, m.managerLabel.value)
}

func (m *Model) sendDraft() tea.Cmd {
	manager, ok := m.session(m.selectedManager)
	if !ok || manager.Role != RoleManager {
		return nil
	}
	if manager.State != StateActive {
		m.status = "Manager input unavailable in " + sessionStateLabel(manager) + " state"
		return nil
	}
	draft := m.drafts[m.selectedManager]
	if draft.value == "" {
		return nil
	}
	token := m.begin(actionSend)
	if token == 0 {
		return nil
	}
	managerID := m.selectedManager
	m.pendingText = draft.value
	m.pendingSession = managerID
	text := draft.value
	return m.simpleAction(token, actionSend, "", func(ctx context.Context) error {
		return m.runtime.Send(ctx, managerID, text)
	})
}

func (m *Model) resumeSelected() tea.Cmd {
	manager, ok := m.session(m.selectedManager)
	if !ok || manager.Role != RoleManager {
		return nil
	}
	if !manager.ResumeAvailable {
		m.status = "Resume unavailable for this Manager"
		return nil
	}
	if manager.State == StateActive || manager.State == StateStarting {
		m.status = "Resume unavailable in " + sessionStateLabel(manager) + " state"
		return nil
	}
	token := m.begin(actionResume)
	if token == 0 {
		return nil
	}
	managerID := manager.ID
	return m.refreshAfter(token, actionResume, "", func(ctx context.Context) error {
		return m.runtime.Resume(ctx, managerID)
	})
}

func (m *Model) stopSelected() tea.Cmd {
	manager, ok := m.session(m.selectedManager)
	if !ok || manager.Role != RoleManager {
		return nil
	}
	token := m.begin(actionStop)
	if token == 0 {
		return nil
	}
	managerID := manager.ID
	return m.refreshAfter(token, actionStop, "", func(ctx context.Context) error {
		return m.runtime.Stop(ctx, managerID)
	})
}

func (m *Model) scrollPane(delta int) {
	p := m.panes[m.selectedManager]
	if p == nil {
		return
	}
	if delta > 0 {
		p.scrollUp(delta)
	} else {
		p.scrollDown(-delta)
	}
}

func (m *Model) managerInputAvailable() bool {
	manager, ok := m.session(m.selectedManager)
	return ok && manager.Role == RoleManager && manager.TaskID == m.currentTask && manager.State == StateActive
}

func (m *Model) handleManagerEditorKey(msg tea.KeyPressMsg) {
	id := m.selectedManager
	if id == "" || !m.managerInputAvailable() {
		return
	}
	draft := m.drafts[id]
	otherBytes := m.totalDraftBytes() - len(draft.value)
	draft.limit = min(m.limits.DraftBytes, max(0, m.limits.AggregateDraftBytes-otherBytes))
	handleEditorKey(&draft, msg)
	if draft.value == "" && draft.cursor == 0 {
		delete(m.drafts, id)
		return
	}
	m.drafts[id] = draft
}

func (m *Model) insertManagerText(text string) {
	id := m.selectedManager
	if id == "" || !m.managerInputAvailable() {
		return
	}
	draft := m.drafts[id]
	otherBytes := m.totalDraftBytes() - len(draft.value)
	draft.limit = min(m.limits.DraftBytes, max(0, m.limits.AggregateDraftBytes-otherBytes))
	draft.insert(text)
	if draft.value != "" {
		m.drafts[id] = draft
	}
}

func (m *Model) totalDraftBytes() int {
	total := 0
	for _, draft := range m.drafts {
		total += len(draft.value)
	}
	return total
}

func (m *Model) enforcePaneAggregate() {
	for {
		bytes, frames := m.totalPaneUsage()
		if bytes <= m.limits.AggregatePaneBytes && frames <= m.limits.AggregatePaneFrames {
			return
		}
		var oldestID SessionID
		var oldestSequence uint64
		found := false
		for id, candidate := range m.panes {
			if candidate == nil {
				delete(m.panes, id)
				continue
			}
			if len(candidate.frames) == 0 {
				if !candidate.evicted {
					delete(m.panes, id)
				}
				continue
			}
			sequence := candidate.frames[0].sequence
			if !found || sequence < oldestSequence || (sequence == oldestSequence && id < oldestID) {
				found = true
				oldestID = id
				oldestSequence = sequence
			}
		}
		if !found {
			return
		}
		candidate := m.panes[oldestID]
		removed := candidate.frames[0]
		candidate.frames = candidate.frames[1:]
		candidate.bytes -= removed.bytes
		candidate.evicted = true
	}
}

func (m *Model) totalPaneUsage() (bytes, frames int) {
	for _, candidate := range m.panes {
		if candidate == nil {
			continue
		}
		bytes += candidate.bytes
		frames += len(candidate.frames)
	}
	return bytes, frames
}

func (m *Model) handleRawMouse(message tea.MouseMsg) tea.Cmd {
	view := m.View()
	if view.OnMouse == nil {
		return nil
	}
	return view.OnMouse(message)
}

func (m *Model) handleMouseAction(msg mouseActionMsg) tea.Cmd {
	if m.help {
		if msg.action == mouseHelp {
			m.help = false
		}
		return nil
	}
	if m.confirmStop {
		switch msg.action {
		case mouseConfirmStop:
			m.confirmStop = false
			return m.stopSelected()
		case mouseCancelStop:
			m.confirmStop = false
		}
		return nil
	}
	if m.details {
		switch msg.action {
		case mouseHelp:
			m.help = true
		case mouseDetailsPrev:
			m.moveDetail(-1)
		case mouseDetailsNext:
			m.moveDetail(1)
		case mouseDetailsClose:
			m.closeSessionDetails()
		}
		return nil
	}
	switch msg.action {
	case mouseHelp:
		m.help = !m.help
	case mouseBack:
		switch m.screen {
		case screenWorkspace:
			return m.leaveWorkspace()
		case screenSettings:
			m.screen = m.returnTo
			if m.screen == screenWorkspace && m.currentTask == "" {
				m.screen = screenTasks
			}
			m.focus = focusList
		case screenAddTask:
			m.screen = screenTasks
			m.focus = focusList
		case screenAddManager:
			m.screen = screenWorkspace
			m.focus = focusList
		}
	case mouseTask:
		tasks := m.visibleTasks()
		if msg.index >= 0 && msg.index < len(tasks) {
			m.selectedTask = tasks[msg.index].ID
		}
	case mouseTaskActive:
		m.filter = filterActive
		m.ensureTaskSelection()
	case mouseTaskAll:
		m.filter = filterAll
		m.ensureTaskSelection()
	case mouseTaskOpen:
		return m.openSelectedTask()
	case mouseAddTask:
		m.screen = screenAddTask
		m.focus = focusInput
	case mouseSubmitTask:
		return m.keyAddTask(keyMessage("enter"))
	case mouseSettings:
		m.returnTo = m.screen
		m.screen = screenSettings
		m.focus = focusList
	case mouseProvider:
		if msg.index >= 0 && msg.index < len(m.snapshot.Providers) {
			m.selectedProvider = m.snapshot.Providers[msg.index].ID
		}
	case mouseProviderEnable:
		return m.keySettings(keyMessage("e"))
	case mouseProviderAuth:
		return m.keySettings(keyMessage("a"))
	case mouseProviderRecheck:
		return m.keySettings(keyMessage("r"))
	case mouseManager:
		nodes := m.treeNodesByTask[m.currentTask]
		if msg.index >= 0 && msg.index < len(nodes) {
			session, ok := m.session(nodes[msg.index].sessionID)
			if !ok {
				return nil
			}
			if session.Role == RoleManager {
				// A pointer click is an explicit tree selection, not permission to
				// silently retarget an active composer and its draft.
				m.focus = focusList
				m.selectedManager = session.ID
			} else {
				// Pointer selection of a subordinate is a Session-tree
				// invocation, even when the composer previously had focus.
				// Capture list focus before changing the selected Manager so
				// Details can never restore composer focus on close.
				m.focus = focusList
				m.selectedManager = session.ParentID
				m.openSessionDetails(session.ID)
			}
			if !m.managerInputAvailable() {
				m.focus = focusList
			}
		}
	case mouseInput:
		if m.managerInputAvailable() {
			m.focus = focusInput
		}
	case mouseSend:
		return m.sendDraft()
	case mouseResume:
		return m.resumeSelected()
	case mouseAddManager:
		m.openAddManager()
	case mouseStop:
		if m.selectedManager != "" {
			m.confirmStop = true
		}
	case mouseAddProvider:
		providers := m.managerProviders()
		if msg.index >= 0 && msg.index < len(providers) {
			m.addProvider = providers[msg.index].ID
		}
	case mouseLabel:
		m.focus = focusLabel
	case mouseCreateManager:
		return m.createManager()
	case mouseRouteSettings:
		m.returnTo = screenAddManager
		m.screen = screenSettings
	case mouseDetails:
		m.openSessionDetails(m.selectedManager)
	}
	return nil
}

func keyMessage(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Text: text, Code: []rune(text)[0]})
}

func (m *Model) visibleTasks() []Task {
	tasks := m.snapshot.Tasks
	if m.filter == filterAll {
		return append([]Task(nil), tasks...)
	}
	visible := tasks[:0:0]
	for _, task := range tasks {
		active, _, total := m.taskCounts(task.ID)
		if total == 0 || active > 0 {
			visible = append(visible, task)
		}
	}
	return visible
}

func (m *Model) taskCounts(taskID TaskID) (active, dormant, total int) {
	count := m.countsByTask[taskID]
	return count.active, count.dormant, count.total
}

func (m *Model) sessionsForTask(taskID TaskID) []Session {
	return m.sessionsByTask[taskID]
}

func (m *Model) managersForTask(taskID TaskID) []Session {
	return m.managersByTask[taskID]
}

func (m *Model) childrenOf(parent SessionID) []Session {
	return m.childrenByManager[parent]
}

func (m *Model) managerProviders() []Provider {
	return m.managerProviderSet
}

func (m *Model) moveTask(delta int) {
	tasks := m.visibleTasks()
	if len(tasks) == 0 {
		m.selectedTask = ""
		return
	}
	index := indexTask(tasks, m.selectedTask)
	index = clamp(index+delta, 0, len(tasks)-1)
	m.selectedTask = tasks[index].ID
}

func (m *Model) moveProvider(delta int) {
	if len(m.snapshot.Providers) == 0 {
		m.selectedProvider = ""
		return
	}
	index := indexProvider(m.snapshot.Providers, m.selectedProvider)
	index = clamp(index+delta, 0, len(m.snapshot.Providers)-1)
	m.selectedProvider = m.snapshot.Providers[index].ID
}

func (m *Model) moveAddProvider(delta int) {
	providers := m.managerProviders()
	if len(providers) == 0 {
		m.addProvider = ""
		return
	}
	index := indexProvider(providers, m.addProvider)
	index = clamp(index+delta, 0, len(providers)-1)
	m.addProvider = providers[index].ID
}

func (m *Model) moveManager(delta int) {
	managers := m.managersForTask(m.currentTask)
	if len(managers) == 0 {
		m.selectedManager = ""
		return
	}
	index := indexSession(managers, m.selectedManager)
	index = clamp(index+delta, 0, len(managers)-1)
	m.selectedManager = managers[index].ID
	m.ensureSelectedManagerVisible()
}

func (m *Model) ensureSelectedManagerVisible() {
	height := m.treeVisibleHeight()
	if height <= 0 {
		return
	}
	selectedLine, ok := m.treeIndexByManager[m.selectedManager]
	if !ok {
		return
	}
	if selectedLine < m.treeOffset {
		m.treeOffset = selectedLine
	} else if selectedLine >= m.treeOffset+height {
		m.treeOffset = selectedLine - height + 1
	}
}

func (m *Model) treeVisibleHeight() int {
	geometry := workspaceGeometry(m.width, m.height)
	if geometry.wide {
		return geometry.mainHeight
	}
	return geometry.treeHeight
}

func (m *Model) ensureTaskSelection() {
	tasks := m.visibleTasks()
	if !containsTask(tasks, m.selectedTask) {
		m.selectedTask = firstTask(tasks)
	}
}

func (m *Model) selectedProviderValue() (Provider, bool) {
	return m.provider(m.selectedProvider)
}

func (m *Model) provider(id ProviderID) (Provider, bool) {
	provider, ok := m.providersByID[id]
	return provider, ok
}

func (m *Model) session(id SessionID) (Session, bool) {
	session, ok := m.sessionsByID[id]
	return session, ok
}

func (m *Model) task(id TaskID) (Task, bool) {
	task, ok := m.tasksByID[id]
	return task, ok
}

func (m *Model) hasTask(id TaskID) bool { _, ok := m.task(id); return ok }

func (m *Model) hasProvider(id ProviderID) bool { _, ok := m.provider(id); return ok }

func (m *Model) isManagerInTask(id SessionID, taskID TaskID) bool {
	session, ok := m.session(id)
	return ok && session.TaskID == taskID && session.Role == RoleManager
}

func firstTask(tasks []Task) TaskID {
	if len(tasks) == 0 {
		return ""
	}
	return tasks[0].ID
}

func firstProvider(providers []Provider) ProviderID {
	if len(providers) == 0 {
		return ""
	}
	return providers[0].ID
}

func firstManager(sessions []Session) SessionID {
	for _, session := range sessions {
		if session.Role == RoleManager {
			return session.ID
		}
	}
	return ""
}

func containsTask(tasks []Task, id TaskID) bool { return indexTask(tasks, id) >= 0 }

func containsProvider(providers []Provider, id ProviderID) bool {
	return indexProvider(providers, id) >= 0
}

func indexTask(tasks []Task, id TaskID) int {
	for i := range tasks {
		if tasks[i].ID == id {
			return i
		}
	}
	return -1
}

func indexProvider(providers []Provider, id ProviderID) int {
	for i := range providers {
		if providers[i].ID == id {
			return i
		}
	}
	return -1
}

func indexSession(sessions []Session, id SessionID) int {
	for i := range sessions {
		if sessions[i].ID == id {
			return i
		}
	}
	return -1
}

func actionLabel(action actionKind) string {
	switch action {
	case actionReload:
		return "Metadata refresh"
	case actionAddTask:
		return "Add Task"
	case actionSetEnabled:
		return "Provider enablement"
	case actionAuthenticate:
		return "Authentication"
	case actionRecheck:
		return "Provider recheck"
	case actionAddManager:
		return "Add Manager"
	case actionAttach:
		return "Viewer attach"
	case actionDetach:
		return "Viewer detach"
	case actionSend:
		return "Manager input"
	case actionResume:
		return "Manager resume"
	case actionStop:
		return "Manager stop"
	default:
		return "Action"
	}
}

func safeFailure(action string, err error) string {
	code := ""
	var safe SafeError
	if errors.As(err, &safe) {
		code = safeInlinePrefix(safe.SafeCode(), 80)
	}
	if code == "" {
		return action + " failed"
	}
	return action + " failed (" + code + ")"
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
