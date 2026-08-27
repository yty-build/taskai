// Package console implements interactive Task and Session attachment. A
// console is only a viewer/controller; its lifetime never owns a worker.
package console

import (
	"context"
	"errors"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type Backend interface {
	Task(context.Context, string) (task.Detail, error)
	Session(context.Context, string) (session.Session, error)
	FreshReplayCursor(context.Context, string, int) (uint64, bool, error)
	PermissionSnapshot(context.Context, string) (appservice.PermissionSnapshot, error)
	Follow(context.Context, string, uint64, func(sessionlog.Line) error) error
	TaskReplayWindows(context.Context, string, map[string]uint64, int) (map[string]uint64, map[string]appservice.ReplayWindow, error)
	FollowTask(context.Context, string, map[string]uint64, func(appservice.TaskActivity) error) error
	Prompt(context.Context, string, string) (worker.Message, error)
	DecidePermission(context.Context, string, string, string, bool) error
	Cancel(context.Context, string) error
	Resume(context.Context, string) (session.Session, error)
	Stop(context.Context, string) error
	AvailableAgents(context.Context) ([]agents.Profile, error)
	ConnectSession(context.Context, appservice.StartSessionParams) (ProviderSession, error)
	LoadProviderOptions(context.Context, string) (ProviderSession, error)
	ConfigureSession(context.Context, string, session.ProviderOptions) (ProviderSession, error)
	PromptSession(context.Context, string, string) (appservice.LaunchResult, error)
}

// ProviderSession is disposable setup data returned by one live provider Port.
// The catalog is never hard-coded or persisted as a gateway-owned model.
type ProviderSession struct {
	Session session.Session
	Options acp.ProviderOptionCatalog
}

type Services struct {
	Tasks    *task.Service
	Sessions *appservice.SessionService
}

func (s Services) Task(ctx context.Context, id string) (task.Detail, error) {
	return s.Tasks.Get(ctx, id)
}

func (s Services) Session(ctx context.Context, id string) (session.Session, error) {
	return s.Sessions.Get(ctx, id)
}

func (s Services) FreshReplayCursor(ctx context.Context, id string, maxRecords int) (uint64, bool, error) {
	return s.Sessions.FreshReplayCursor(ctx, id, maxRecords)
}

func (s Services) PermissionSnapshot(ctx context.Context, id string) (appservice.PermissionSnapshot, error) {
	return s.Sessions.PendingPermissionSnapshot(ctx, id)
}

func (s Services) Follow(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) error {
	return s.Sessions.Follow(ctx, id, cursor, consume)
}

func (s Services) TaskReplayWindows(ctx context.Context, taskID string, cursors map[string]uint64, maxRecords int) (map[string]uint64, map[string]appservice.ReplayWindow, error) {
	return s.Sessions.TaskReplayWindows(ctx, taskID, cursors, maxRecords)
}

func (s Services) FollowTask(ctx context.Context, taskID string, cursors map[string]uint64, consume func(appservice.TaskActivity) error) error {
	return s.Sessions.FollowTask(ctx, taskID, cursors, consume)
}

func (s Services) Prompt(ctx context.Context, id, message string) (worker.Message, error) {
	return s.Sessions.Prompt(ctx, id, message)
}

func (s Services) DecidePermission(ctx context.Context, id, permissionID, optionID string, cancelled bool) error {
	return s.Sessions.DecidePermission(ctx, id, permissionID, optionID, cancelled)
}

func (s Services) Cancel(ctx context.Context, id string) error {
	return s.Sessions.Cancel(ctx, id)
}

func (s Services) Resume(ctx context.Context, id string) (session.Session, error) {
	return s.Sessions.Resume(ctx, id)
}

func (s Services) Stop(ctx context.Context, id string) error {
	return s.Sessions.Stop(ctx, id)
}

func (s Services) AvailableAgents(context.Context) ([]agents.Profile, error) {
	return s.Sessions.AgentProfiles()
}

func (s Services) ConnectSession(ctx context.Context, params appservice.StartSessionParams) (ProviderSession, error) {
	result, err := s.Sessions.ConnectSession(ctx, params)
	return ProviderSession{Session: result.Session, Options: result.ProviderOptions}, err
}

func (s Services) LoadProviderOptions(ctx context.Context, sessionID string) (ProviderSession, error) {
	result, err := s.Sessions.ProviderOptions(ctx, sessionID)
	return ProviderSession{Session: result.Session, Options: result.ProviderOptions}, err
}

func (s Services) ConfigureSession(ctx context.Context, sessionID string, options session.ProviderOptions) (ProviderSession, error) {
	result, err := s.Sessions.Configure(ctx, sessionID, options)
	return ProviderSession{Session: result.Session, Options: result.ProviderOptions}, err
}

func (s Services) PromptSession(ctx context.Context, sessionID, prompt string) (appservice.LaunchResult, error) {
	current, err := s.Sessions.Get(ctx, sessionID)
	if err != nil {
		return appservice.LaunchResult{}, err
	}
	if strings.TrimSpace(prompt) == "" {
		return appservice.LaunchResult{Session: current, State: string(current.Status)}, nil
	}
	accepted, err := s.Sessions.Prompt(ctx, sessionID, prompt)
	return appservice.LaunchResult{
		Session: current, RequestID: accepted.RequestID, State: accepted.State, Started: err == nil,
	}, err
}

func AttachTask(ctx context.Context, backend Backend, taskID string, input io.Reader, output io.Writer) error {
	model, cancel, err := newTaskModel(ctx, backend, taskID)
	if err != nil {
		return err
	}
	defer cancel()
	return run(ctx, model, input, output)
}

func AttachSession(ctx context.Context, backend Backend, sessionID string, input io.Reader, output io.Writer) error {
	model, cancel, err := newSessionModel(ctx, backend, sessionID)
	if err != nil {
		return err
	}
	defer cancel()
	return run(ctx, model, input, output)
}

func run(ctx context.Context, model Model, input io.Reader, output io.Writer) error {
	program := tea.NewProgram(model,
		tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output),
		tea.WithAltScreen(), tea.WithMouseCellMotion(),
	)
	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
