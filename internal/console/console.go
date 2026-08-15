// Package console implements interactive Task and Session attachment. A
// console is only a viewer/controller; its lifetime never owns a worker.
package console

import (
	"context"
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type Backend interface {
	Task(context.Context, string) (task.Detail, error)
	Session(context.Context, string) (session.Session, error)
	History(context.Context, string, uint64) ([]sessionlog.Line, error)
	Follow(context.Context, string, uint64, func(sessionlog.Line) error) error
	Prompt(context.Context, string, string) (worker.Message, error)
	Cancel(context.Context, string) error
	Resume(context.Context, string) (session.Session, error)
	Stop(context.Context, string) error
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

func (s Services) History(ctx context.Context, id string, cursor uint64) ([]sessionlog.Line, error) {
	return s.Sessions.History(ctx, id, cursor)
}

func (s Services) Follow(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) error {
	return s.Sessions.Follow(ctx, id, cursor, consume)
}

func (s Services) Prompt(ctx context.Context, id, message string) (worker.Message, error) {
	return s.Sessions.Prompt(ctx, id, message)
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
