package task

import (
	"context"

	"github.com/yty-build/acp-gateway/internal/session"
)

type persistence interface {
	Repository
	ListSessionsByTask(context.Context, string) ([]session.Session, error)
	CreateTaskWithSession(context.Context, CreateParams, session.CreateParams) (Task, session.Session, error)
}

// Service is the shared application boundary for Task behavior.
type Service struct {
	repository persistence
}

type Detail struct {
	Task     Task
	Sessions []session.Session
}

func NewService(repository persistence) *Service {
	return &Service{repository: repository}
}

func (s *Service) Create(ctx context.Context, params CreateParams) (Task, error) {
	return s.repository.CreateTask(ctx, params)
}

func (s *Service) Start(ctx context.Context, taskParams CreateParams, sessionParams session.CreateParams) (Task, session.Session, error) {
	return s.repository.CreateTaskWithSession(ctx, taskParams, sessionParams)
}

func (s *Service) Get(ctx context.Context, id string) (Detail, error) {
	result, err := s.repository.GetTask(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	sessions, err := s.repository.ListSessionsByTask(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Task: result, Sessions: sessions}, nil
}

func (s *Service) List(ctx context.Context) ([]Task, error) {
	return s.repository.ListTasks(ctx)
}

func (s *Service) SetStatus(ctx context.Context, id string, status Status) (Task, error) {
	return s.repository.UpdateTaskStatus(ctx, id, status)
}
