package console

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/agents"
	appservice "github.com/yty-build/acp-gateway/internal/service"
)

type wizardField uint8

const (
	wizardAgent wizardField = iota
	wizardRole
	wizardRepo
	wizardWorktree
	wizardPermission
	wizardPrompt
	wizardFieldCount
)

type sessionWizard struct {
	agents      []agents.Profile
	agentIndex  int
	field       wizardField
	role        composerState
	repo        composerState
	prompt      composerState
	worktree    bool
	approveAll  bool
	submitting  bool
	errorNotice string
}

type wizardReadyMsg struct {
	agents      []agents.Profile
	defaultRepo string
	err         error
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

func startWizardSessionCmd(ctx context.Context, backend Backend, params appservice.StartSessionParams) tea.Cmd {
	return func() tea.Msg {
		result, err := backend.StartSession(ctx, params)
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

func (m Model) handleWizardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo
	wizard := m.wizard
	if wizard == nil || wizard.submitting {
		return m, nil
	}
	name := key.String()
	switch name {
	case "esc":
		m.wizard = nil
		m.notice = "New Session dismissed; no Session was created."
		return m, nil
	case "tab":
		wizard.field = (wizard.field + 1) % wizardFieldCount
		return m, nil
	case "shift+tab":
		wizard.field = (wizard.field + wizardFieldCount - 1) % wizardFieldCount
		return m, nil
	case "ctrl+s":
		return m.submitWizard()
	case "enter":
		if wizard.field == wizardPrompt {
			return m.submitWizard()
		}
		wizard.field++
		return m, nil
	}

	switch wizard.field {
	case wizardAgent:
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
		handleWizardTextKey(&wizard.role, key, false)
	case wizardRepo:
		handleWizardTextKey(&wizard.repo, key, false)
	case wizardWorktree:
		if name == " " || name == "left" || name == "right" {
			wizard.worktree = !wizard.worktree
		}
	case wizardPermission:
		if name == " " || name == "left" || name == "right" {
			wizard.approveAll = !wizard.approveAll
		}
	case wizardPrompt:
		handleWizardTextKey(&wizard.prompt, key, true)
	}
	return m, nil
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
	repo = expandUserPath(repo)
	wizard.errorNotice = ""
	wizard.submitting = true
	profile := wizard.agents[wizard.agentIndex]
	params := appservice.StartSessionParams{
		TaskID: m.task.Task.ID, Agent: profile.Name, Role: role, RepoPath: repo,
		Prompt: wizard.prompt.text(), Worktree: wizard.worktree, ApproveAll: wizard.approveAll,
	}
	return m, startWizardSessionCmd(m.ctx, m.backend, params)
}

func (m Model) handleSessionStarted(message sessionStartedMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		if m.wizard != nil {
			m.wizard.submitting = false
			m.wizard.errorNotice = message.err.Error()
		}
		return m, nil
	}
	created := message.result.Session
	found := false
	for index, current := range m.task.Sessions {
		if current.ID == created.ID {
			m.task.Sessions[index] = created
			m.selected = index
			found = true
			break
		}
	}
	if !found {
		m.task.Sessions = append(m.task.Sessions, created)
		m.selected = len(m.task.Sessions) - 1
	}
	m.wizard = nil
	m.current = created
	m.mode = sessionView
	m.direct = false
	m.scroll, m.unread = 0, 0
	m.notice = fmt.Sprintf("Started %s with %s. Initial message %s.", created.ID, created.Agent, valueOr(message.result.State, "accepted"))
	m.manager.ensure(created, m.cursors[created.ID])
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
