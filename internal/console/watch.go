package console

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type activityMsg struct {
	sessionID string
	line      sessionlog.Line
}

type consoleErrorMsg struct{ err error }
type managerClosedMsg struct{}

type watchManager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  Backend
	events   chan tea.Msg
	mu       sync.Mutex
	watching map[string]bool
}

func newWatchManager(parent context.Context, backend Backend) *watchManager {
	ctx, cancel := context.WithCancel(parent)
	return &watchManager{ctx: ctx, cancel: cancel, backend: backend, events: make(chan tea.Msg, 512), watching: make(map[string]bool)}
}

func (m *watchManager) ensure(current session.Session, cursor uint64) {
	if current.WorkerSocket == "" {
		return
	}
	m.mu.Lock()
	if m.watching[current.ID] {
		m.mu.Unlock()
		return
	}
	m.watching[current.ID] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.watching, current.ID)
			m.mu.Unlock()
		}()
		err := m.backend.Follow(m.ctx, current.ID, cursor, func(line sessionlog.Line) error {
			select {
			case m.events <- activityMsg{sessionID: current.ID, line: line}:
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
