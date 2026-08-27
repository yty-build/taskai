package console

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type activityMsg struct {
	sessionID string
	line      sessionlog.Line
}

type taskActivityMsg struct{ activity appservice.TaskActivity }

type consoleErrorMsg struct{ err error }
type managerClosedMsg struct{}

type watchManager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	backend   Backend
	events    chan tea.Msg
	mu        sync.Mutex
	watching  map[string]bool
	completed map[string]watchCompletion
	taskID    string
	taskLive  bool
}

type watchCompletion struct {
	socket string
	cursor uint64
}

func newWatchManager(parent context.Context, backend Backend) *watchManager {
	ctx, cancel := context.WithCancel(parent)
	return &watchManager{
		ctx: ctx, cancel: cancel, backend: backend, events: make(chan tea.Msg, 1),
		watching: make(map[string]bool), completed: make(map[string]watchCompletion),
	}
}

func (m *watchManager) ensureSession(current session.Session, cursor uint64) {
	m.mu.Lock()
	if m.taskID != "" {
		m.mu.Unlock()
		return
	}
	completed, completedExists := m.completed[current.ID]
	if m.watching[current.ID] || (completedExists && completed == (watchCompletion{socket: current.WorkerSocket, cursor: cursor})) {
		m.mu.Unlock()
		return
	}
	m.watching[current.ID] = true
	m.mu.Unlock()
	go func() {
		delivered := cursor
		defer func() {
			m.mu.Lock()
			delete(m.watching, current.ID)
			m.mu.Unlock()
		}()
		err := m.backend.Follow(m.ctx, current.ID, cursor, func(line sessionlog.Line) error {
			delivered = line.Seq
			select {
			case m.events <- activityMsg{sessionID: current.ID, line: line}:
				return nil
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		})
		if err == nil {
			m.mu.Lock()
			m.completed[current.ID] = watchCompletion{socket: current.WorkerSocket, cursor: delivered}
			m.mu.Unlock()
		}
		if err != nil && m.ctx.Err() == nil {
			select {
			case m.events <- consoleErrorMsg{err: err}:
			case <-m.ctx.Done():
			}
		}
	}()
}

func (m *watchManager) ensureTask(taskID string, cursors map[string]uint64) {
	if taskID == "" {
		return
	}
	m.mu.Lock()
	if m.taskLive || (m.taskID != "" && m.taskID != taskID) || len(m.watching) > 0 {
		m.mu.Unlock()
		return
	}
	m.taskID = taskID
	m.taskLive = true
	m.mu.Unlock()

	initial := make(map[string]uint64, len(cursors))
	for sessionID, cursor := range cursors {
		initial[sessionID] = cursor
	}
	go func() {
		defer func() {
			m.mu.Lock()
			m.taskLive = false
			m.mu.Unlock()
		}()
		err := m.backend.FollowTask(m.ctx, taskID, initial, func(activity appservice.TaskActivity) error {
			select {
			case m.events <- taskActivityMsg{activity: activity}:
				return nil
			case <-m.ctx.Done():
				return m.ctx.Err()
			}
		})
		if err != nil && m.ctx.Err() == nil {
			select {
			case m.events <- consoleErrorMsg{err: err}:
			case <-m.ctx.Done():
			}
		}
	}()
}

func (m *watchManager) wait() tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-m.events:
			return event
		case <-m.ctx.Done():
			return managerClosedMsg{}
		}
	}
}
