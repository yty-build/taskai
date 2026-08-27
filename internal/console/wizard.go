package console

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type wizardField uint8

const (
	wizardAgent wizardField = iota
	wizardRole
	wizardRepo
	wizardWorktree
	wizardPermission
	wizardModel
	wizardReasoningEffort
	wizardPrompt
	wizardFieldCount
)

// sessionWizard has only two UI steps. First it connects one ordinary
// Session Port so the provider can advertise its controls. Then it applies a
// selected tuple and sends the first prompt through that same Port.
type sessionWizard struct {
	agents               []agents.Profile
	agentIndex           int
	field                wizardField
	role                 composerState
	repo                 composerState
	prompt               composerState
	providerOptions      acp.ProviderOptionCatalog
	providerOptionsReady bool
	modelIndex           int
	effortIndex          int
	connectedID          string
	worktree             bool
	approveAll           bool
	submitting           bool
	errorNotice          string
}

type wizardReadyMsg struct {
	agents      []agents.Profile
	defaultRepo string
	err         error
}

type sessionConnectedMsg struct {
	result ProviderSession
	err    error
}

type providerOptionsReadyMsg struct {
	result ProviderSession
	err    error
}

type sessionStartedMsg struct {
	result appservice.LaunchResult
	err    error
}

func loadWizardCmd(ctx context.Context, backend Backend, defaultRepo string) tea.Cmd {
	return func() tea.Msg {
		profiles, err := backend.AvailableAgents(ctx)
		return wizardReadyMsg{agents: profiles, defaultRepo: defaultRepo, err: err}
	}
}

func connectWizardSessionCmd(ctx context.Context, backend Backend, params appservice.StartSessionParams) tea.Cmd {
	return func() tea.Msg {
		result, err := backend.ConnectSession(ctx, params)
		return sessionConnectedMsg{result: result, err: err}
	}
}

func loadProviderOptionsCmd(ctx context.Context, backend Backend, sessionID string) tea.Cmd {
	return func() tea.Msg {
		result, err := backend.LoadProviderOptions(ctx, sessionID)
		return providerOptionsReadyMsg{result: result, err: err}
	}
}

func retryProviderOptionsCmd(ctx context.Context, backend Backend, sessionID string) tea.Cmd {
	return func() tea.Msg {
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return providerOptionsReadyMsg{err: ctx.Err()}
		case <-timer.C:
			result, err := backend.LoadProviderOptions(ctx, sessionID)
			return providerOptionsReadyMsg{result: result, err: err}
		}
	}
}

func configureAndPromptWizardSessionCmd(
	ctx context.Context,
	backend Backend,
	sessionID string,
	options session.ProviderOptions,
	prompt string,
) tea.Cmd {
	return func() tea.Msg {
		configured, err := backend.ConfigureSession(ctx, sessionID, options)
		if err != nil {
			return sessionStartedMsg{result: appservice.LaunchResult{Session: configured.Session}, err: err}
		}
		result, err := backend.PromptSession(ctx, sessionID, prompt)
		if result.Session.ID == "" {
			result.Session = configured.Session
		}
		return sessionStartedMsg{result: result, err: err}
	}
}

func (m Model) handleWizardReady(message wizardReadyMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		m.notice = "New Session unavailable: " + message.err.Error()
		return m, nil
	}
	if len(message.agents) == 0 {
		m.notice = "New Session unavailable: no agent profiles are configured."
		return m, nil
	}
	wizard := &sessionWizard{agents: message.agents, worktree: true}
	wizard.role.value = []rune("worker")
	wizard.role.cursor = len(wizard.role.value)
	wizard.repo.value = []rune(message.defaultRepo)
	wizard.repo.cursor = len(wizard.repo.value)
	for index, profile := range wizard.agents {
		if profile.Name == "codex" {
			wizard.agentIndex = index
			break
		}
	}
	m.wizard = wizard
	m.notice = ""
	return m, nil
}

func (m Model) handleSessionConnected(message sessionConnectedMsg) (tea.Model, tea.Cmd) {
	wizard := m.wizard
	if wizard == nil {
		return m, nil
	}
	wizard.submitting = false
	current := message.result.Session
	if current.ID != "" {
		m.rememberConnectedSession(current)
		wizard.connectedID = current.ID
	}
	if message.err != nil {
		wizard.providerOptionsReady = false
		wizard.providerOptions = acp.ProviderOptionCatalog{}
		wizard.modelIndex, wizard.effortIndex = 0, 0
		wizard.errorNotice = message.err.Error()
		return m, nil
	}
	wizard.providerOptions = acp.ProviderOptionCatalog{}
	wizard.providerOptionsReady = false
	wizard.modelIndex, wizard.effortIndex = 0, 0
	wizard.errorNotice = "Connecting provider…"
	wizard.submitting = true
	return m, loadProviderOptionsCmd(m.ctx, m.backend, current.ID)
}

func (m Model) handleProviderOptionsReady(message providerOptionsReadyMsg) (tea.Model, tea.Cmd) {
	wizard := m.wizard
	if wizard == nil {
		return m, nil
	}
	wizard.submitting = false
	if message.err != nil {
		var starting *worker.NotReadyError
		if errors.As(message.err, &starting) {
			wizard.submitting = true
			wizard.errorNotice = "Connecting provider…"
			return m, retryProviderOptionsCmd(m.ctx, m.backend, wizard.connectedID)
		}
		wizard.providerOptionsReady = false
		wizard.providerOptions = acp.ProviderOptionCatalog{}
		wizard.modelIndex, wizard.effortIndex = 0, 0
		wizard.errorNotice = "Provider controls could not be loaded: " + message.err.Error()
		return m, nil
	}
	if message.result.Session.ID != wizard.connectedID {
		wizard.errorNotice = "Provider controls were returned for a different Session."
		return m, nil
	}
	wizard.providerOptions = message.result.Options
	wizard.providerOptionsReady = true
	wizard.modelIndex, wizard.effortIndex = 0, 0
	wizard.field = wizardModel
	wizard.errorNotice = ""
	m.rememberConnectedSession(message.result.Session)
	return m, nil
}

func (m *Model) rememberConnectedSession(current session.Session) {
	for index := range m.task.Sessions {
		if m.task.Sessions[index].ID == current.ID {
			m.task.Sessions[index] = current
			m.selected = index
			if m.direct {
				m.manager.ensureSession(current, m.cursors[current.ID])
			}
			return
		}
	}
	m.task.Sessions = append(m.task.Sessions, current)
	m.selected = len(m.task.Sessions) - 1
	if m.direct {
		m.manager.ensureSession(current, m.cursors[current.ID])
	}
}

func (m Model) handleWizardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	wizard := m.wizard
	if wizard == nil || wizard.submitting {
		return m, nil
	}
	name := key.String()
	switch name {
	case "esc":
		m.wizard = nil
		if wizard.connectedID != "" {
			m.notice = "Session " + wizard.connectedID + " remains connected; no initial prompt was sent."
		} else {
			m.notice = "New Session dismissed; no Session was created."
		}
		return m, nil
	case "tab":
		wizard.moveField(1)
		return m, nil
	case "shift+tab":
		wizard.moveField(-1)
		return m, nil
	case "ctrl+s":
		return m.submitWizard()
	case "enter":
		if wizard.field == wizardPrompt {
			return m.submitWizard()
		}
		wizard.moveField(1)
		return m, nil
	}

	switch wizard.field {
	case wizardAgent:
		if wizard.connectedID != "" {
			return m, nil
		}
		switch name {
		case "up", "left", "k":
			wizard.agentIndex = (wizard.agentIndex + len(wizard.agents) - 1) % len(wizard.agents)
		case "down", "right", "j", " ":
			wizard.agentIndex = (wizard.agentIndex + 1) % len(wizard.agents)
		default:
			if key.Type == tea.KeyRunes && len(key.Runes) > 0 {
				wizard.selectAgentPrefix(string(key.Runes))
			}
		}
	case wizardRole:
		if wizard.connectedID == "" {
			handleWizardTextKey(&wizard.role, key, false)
		}
	case wizardModel:
		wizard.modelIndex = moveProviderChoice(wizard.modelIndex, len(wizard.providerOptions.Model.Choices), name)
	case wizardReasoningEffort:
		wizard.effortIndex = moveProviderChoice(wizard.effortIndex, len(wizard.providerOptions.ReasoningEffort.Choices), name)
	case wizardRepo:
		if wizard.connectedID == "" {
			handleWizardTextKey(&wizard.repo, key, false)
		}
	case wizardWorktree:
		if wizard.connectedID == "" && (name == " " || name == "left" || name == "right") {
			wizard.worktree = !wizard.worktree
		}
	case wizardPermission:
		if wizard.connectedID == "" && (name == " " || name == "left" || name == "right") {
			wizard.approveAll = !wizard.approveAll
		}
	case wizardPrompt:
		handleWizardTextKey(&wizard.prompt, key, true)
	}
	return m, nil
}

func (w *sessionWizard) moveField(delta int) {
	if w.connectedID != "" {
		fields := []wizardField{wizardModel, wizardReasoningEffort, wizardPrompt}
		position := 0
		for index, field := range fields {
			if field == w.field {
				position = index
				break
			}
		}
		w.field = fields[(position+delta+len(fields))%len(fields)]
		return
	}
	w.field = wizardField((int(w.field) + delta + int(wizardFieldCount)) % int(wizardFieldCount))
}

func moveProviderChoice(current, advertised int, key string) int {
	if advertised <= 0 {
		return 0
	}
	switch key {
	case "up", "left", "k":
		return (current + advertised) % (advertised + 1)
	case "down", "right", "j", " ":
		return (current + 1) % (advertised + 1)
	default:
		return current
	}
}

func handleWizardTextKey(editor *composerState, key tea.KeyMsg, multiline bool) {
	switch key.String() {
	case "ctrl+j":
		if multiline {
			editor.insert('\n')
		}
	case "backspace", "ctrl+h":
		editor.backspace()
	case "ctrl+w":
		editor.deleteWord()
	case "left":
		editor.cursor = max(0, editor.cursor-1)
	case "right":
		editor.cursor = min(len(editor.value), editor.cursor+1)
	case "home", "ctrl+a":
		editor.cursor = 0
	case "end", "ctrl+e":
		editor.cursor = len(editor.value)
	default:
		if key.Type == tea.KeySpace {
			editor.insert(' ')
		} else if key.Type == tea.KeyRunes {
			editor.insert(key.Runes...)
		}
	}
}

func (w *sessionWizard) selectAgentPrefix(prefix string) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return
	}
	for offset := 1; offset <= len(w.agents); offset++ {
		index := (w.agentIndex + offset) % len(w.agents)
		if strings.HasPrefix(strings.ToLower(w.agents[index].Name), prefix) {
			w.agentIndex = index
			return
		}
	}
}

func (m Model) submitWizard() (tea.Model, tea.Cmd) {
	wizard := m.wizard
	if wizard == nil || len(wizard.agents) == 0 {
		return m, nil
	}
	if wizard.connectedID != "" {
		wizard.errorNotice = ""
		wizard.submitting = true
		if !wizard.providerOptionsReady {
			return m, loadProviderOptionsCmd(m.ctx, m.backend, wizard.connectedID)
		}
		return m, configureAndPromptWizardSessionCmd(
			m.ctx,
			m.backend,
			wizard.connectedID,
			session.ProviderOptions{
				Model:           providerChoiceValue(wizard.providerOptions.Model, wizard.modelIndex),
				ReasoningEffort: providerChoiceValue(wizard.providerOptions.ReasoningEffort, wizard.effortIndex),
			},
			wizard.prompt.text(),
		)
	}
	role := strings.TrimSpace(wizard.role.text())
	repo := strings.TrimSpace(wizard.repo.text())
	if role == "" {
		wizard.errorNotice = "Role is required."
		wizard.field = wizardRole
		return m, nil
	}
	if repo == "" {
		wizard.errorNotice = "Repository path is required."
		wizard.field = wizardRepo
		return m, nil
	}
	wizard.errorNotice = ""
	wizard.submitting = true
	profile := wizard.agents[wizard.agentIndex]
	return m, connectWizardSessionCmd(m.ctx, m.backend, appservice.StartSessionParams{
		TaskID: m.task.Task.ID, Agent: profile.Name, Role: role, RepoPath: expandUserPath(repo),
		Worktree: wizard.worktree, ApproveAll: wizard.approveAll,
	})
}

func providerChoiceValue(catalog acp.ProviderSelectCatalog, index int) string {
	if index <= 0 || index > len(catalog.Choices) {
		return ""
	}
	return catalog.Choices[index-1].Value
}

func providerChoiceLabel(catalog acp.ProviderSelectCatalog, index int) string {
	if index <= 0 || index > len(catalog.Choices) {
		if current := strings.TrimSpace(catalog.CurrentValue); current != "" {
			return "Provider default (currently " + current + ")"
		}
		if !catalog.Supported || len(catalog.Choices) == 0 {
			return "Provider default only"
		}
		return "Provider default"
	}
	choice := catalog.Choices[index-1]
	label := strings.TrimSpace(choice.Label)
	if label == "" {
		label = choice.Value
	}
	if group := strings.TrimSpace(choice.Group); group != "" {
		label = group + " · " + label
	}
	return label
}

func (m Model) handleSessionStarted(message sessionStartedMsg) (tea.Model, tea.Cmd) {
	wizard := m.wizard
	created := message.result.Session
	if message.err != nil {
		if wizard != nil {
			wizard.submitting = false
			wizard.errorNotice = message.err.Error()
			if created.ID != "" {
				wizard.connectedID = created.ID
				m.rememberConnectedSession(created)
			}
		}
		return m, nil
	}
	if created.ID == "" {
		if wizard != nil {
			wizard.submitting = false
			wizard.errorNotice = "The Session started without returning its identity."
		}
		return m, nil
	}
	m.rememberConnectedSession(created)
	m.wizard = nil
	m.current = created
	m.mode = sessionView
	m.direct = false
	m.scroll, m.unread = 0, 0
	m.notice = fmt.Sprintf("Started %s with %s. Initial message %s.", created.ID, created.Agent, valueOr(message.result.State, "accepted"))
	return m, m.refreshCmd()
}

func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
