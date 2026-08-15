// Package service contains shared Task Console application behavior used by
// CLI, terminal, API, and web transports.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/identity"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type SessionRepository interface {
	task.Repository
	session.Repository
	ListSessions(context.Context) ([]session.Session, error)
}

type Runtime interface {
	Spawn(context.Context, string) (worker.SpawnResult, error)
	Send(context.Context, string, worker.Command) (worker.Message, error)
	Subscribe(context.Context, string, uint64, func(worker.Message) error) error
}

type LocalRuntime struct {
	Executable string
	Home       config.Home
}

func (r LocalRuntime) Spawn(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
	return worker.Spawn(ctx, r.Executable, r.Home, sessionID)
}

func (r LocalRuntime) Send(ctx context.Context, socket string, command worker.Command) (worker.Message, error) {
	return worker.NewSocketClient(socket).Send(ctx, command)
}

func (r LocalRuntime) Subscribe(ctx context.Context, socket string, cursor uint64, consume func(worker.Message) error) error {
	return worker.NewSocketClient(socket).Subscribe(ctx, cursor, consume)
}

type SessionService struct {
	repository SessionRepository
	registry   *agents.Registry
	home       config.Home
	runtime    Runtime
}

type StartSessionParams struct {
	TaskID       string
	Agent        string
	Role         string
	RepoPath     string
	WorktreePath string
	Branch       string
	Prompt       string
	ApproveAll   bool
}

type LaunchResult struct {
	Session   session.Session
	RequestID string
	State     string
}

func NewSessionService(repository SessionRepository, registry *agents.Registry, home config.Home, runtime Runtime) *SessionService {
	return &SessionService{repository: repository, registry: registry, home: home, runtime: runtime}
}

func (s *SessionService) Prepare(params StartSessionParams) (session.CreateParams, error) {
	profile, err := s.registry.Get(params.Agent)
	if err != nil {
		return session.CreateParams{}, err
	}
	repoPath, err := resolveDirectory(params.RepoPath)
	if err != nil {
		return session.CreateParams{}, fmt.Errorf("resolve repo: %w", err)
	}
	worktreePath := ""
	if params.WorktreePath != "" {
		worktreePath, err = resolveDirectory(params.WorktreePath)
		if err != nil {
			return session.CreateParams{}, fmt.Errorf("resolve worktree: %w", err)
		}
	}
	id, err := identity.New("S")
	if err != nil {
		return session.CreateParams{}, err
	}
	role := strings.TrimSpace(params.Role)
	if role == "" {
		role = "worker"
	}
	metadata, err := PermissionMetadata(params.ApproveAll)
	if err != nil {
		return session.CreateParams{}, err
	}
	return session.CreateParams{
		ID: id, TaskID: params.TaskID, Role: role, Agent: profile.Name, Provider: profile.Provider,
		RepoPath: repoPath, WorktreePath: worktreePath, Branch: params.Branch,
		Status: session.StatusStarting, LogPath: filepath.Join(s.home.Logs, id+".jsonl"), MetadataJSON: metadata,
	}, nil
}

func (s *SessionService) Start(ctx context.Context, params StartSessionParams) (LaunchResult, error) {
	if _, err := s.repository.GetTask(ctx, params.TaskID); err != nil {
		return LaunchResult{}, err
	}
	createParams, err := s.Prepare(params)
	if err != nil {
		return LaunchResult{}, err
	}
	created, err := s.repository.CreateSession(ctx, createParams)
	if err != nil {
		return LaunchResult{}, err
	}
	if _, err := s.repository.UpdateTaskStatus(ctx, params.TaskID, task.StatusWorking); err != nil {
		return LaunchResult{Session: created}, err
	}
	return s.Launch(ctx, created.ID, params.Prompt)
}

// Launch starts the detached worker and submits the Task-derived initial turn.
func (s *SessionService) Launch(ctx context.Context, sessionID, extraPrompt string) (LaunchResult, error) {
	current, err := s.repository.GetSession(ctx, sessionID)
	if err != nil {
		return LaunchResult{}, err
	}
	taskItem, err := s.repository.GetTask(ctx, current.TaskID)
	if err != nil {
		return LaunchResult{}, err
	}
	spawned, err := s.runtime.Spawn(ctx, current.ID)
	if err != nil {
		_, _ = s.repository.UpdateSessionStatus(context.Background(), current.ID, session.StatusFailed)
		return LaunchResult{Session: current}, err
	}
	requestID, err := acp.NewRequestID()
	if err != nil {
		return LaunchResult{Session: current}, err
	}
	message := BuildInitialPrompt(taskItem, current, extraPrompt)
	accepted, err := s.runtime.Send(ctx, spawned.Socket, worker.Command{Type: "prompt", RequestID: requestID, Message: message})
	if err != nil {
		_, _ = s.repository.UpdateSessionStatus(context.Background(), current.ID, session.StatusFailed)
		return LaunchResult{Session: current, RequestID: requestID}, fmt.Errorf("submit initial Session prompt: %w", err)
	}
	if refreshed, refreshErr := s.repository.GetSession(ctx, current.ID); refreshErr == nil {
		current = refreshed
	}
	return LaunchResult{Session: current, RequestID: requestID, State: accepted.State}, nil
}

func BuildInitialPrompt(taskItem task.Task, current session.Session, extra string) string {
	workingPath := current.WorktreePath
	if workingPath == "" {
		workingPath = current.RepoPath
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Task %s: %s\n", taskItem.ID, taskItem.Title)
	if taskItem.Description != "" {
		fmt.Fprintf(&builder, "Description: %s\n", taskItem.Description)
	} else {
		builder.WriteString("Description: (none)\n")
	}
	fmt.Fprintf(&builder, "Session: %s\nRole: %s\nRepository: %s\n", current.ID, current.Role, current.RepoPath)
	if current.WorktreePath != "" {
		fmt.Fprintf(&builder, "Worktree: %s\n", current.WorktreePath)
	}
	if current.Branch != "" {
		fmt.Fprintf(&builder, "Branch: %s\n", current.Branch)
	}
	fmt.Fprintf(&builder, "Work in the repository context at %s. This is one Session under the Task; preserve the Task/Session boundary.\n", workingPath)
	if strings.TrimSpace(extra) != "" {
		fmt.Fprintf(&builder, "\nSession-specific instructions:\n%s\n", strings.TrimSpace(extra))
	}
	return builder.String()
}

func PermissionMetadata(approveAll bool) (json.RawMessage, error) {
	permission := string(acp.PermissionReadApproval)
	if approveAll {
		permission = string(acp.PermissionApproveAll)
	}
	encoded, err := json.Marshal(map[string]string{"permission": permission})
	return json.RawMessage(encoded), err
}

func PermissionLabel(current session.Session) string {
	var metadata map[string]any
	if json.Unmarshal(current.MetadataJSON, &metadata) == nil {
		if value, ok := metadata["permission"].(string); ok && value != "" {
			return value
		}
	}
	return string(acp.PermissionReadApproval)
}

func (s *SessionService) Get(ctx context.Context, id string) (session.Session, error) {
	return s.repository.GetSession(ctx, id)
}

func (s *SessionService) ListByTask(ctx context.Context, taskID string) ([]session.Session, error) {
	if _, err := s.repository.GetTask(ctx, taskID); err != nil {
		return nil, err
	}
	return s.repository.ListSessionsByTask(ctx, taskID)
}

func (s *SessionService) Prompt(ctx context.Context, id, message string) (worker.Message, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return worker.Message{}, err
	}
	if current.WorkerSocket == "" {
		return worker.Message{}, fmt.Errorf("Session %s has no live worker; run 'agent session resume %s'", id, id)
	}
	requestID, err := acp.NewRequestID()
	if err != nil {
		return worker.Message{}, err
	}
	return s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "prompt", RequestID: requestID, Message: message})
}

func (s *SessionService) Cancel(ctx context.Context, id string) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		return fmt.Errorf("Session %s has no live worker", id)
	}
	_, err = s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "cancel"})
	return err
}

func (s *SessionService) Resume(ctx context.Context, id string) (session.Session, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if current.WorkerSocket != "" {
		if _, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "ping"}); err == nil {
			return current, fmt.Errorf("Session %s worker is already running", id)
		}
	}
	spawned, err := s.runtime.Spawn(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	_ = spawned
	return s.repository.GetSession(ctx, id)
}

func (s *SessionService) Stop(ctx context.Context, id string) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		if current.Status == session.StatusStopped {
			return nil
		}
		return fmt.Errorf("Session %s has no live worker", id)
	}
	if _, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "stop"}); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		refreshed, err := s.repository.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if refreshed.WorkerPID == 0 && refreshed.Status == session.StatusStopped {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("Session %s worker did not stop in time", id)
}

func (s *SessionService) History(ctx context.Context, id string, cursor uint64) ([]sessionlog.Line, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return sessionlog.ReadFile(s.logPath(current), cursor)
}

func (s *SessionService) Follow(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		lines, err := sessionlog.ReadFile(s.logPath(current), cursor)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if err := consume(line); err != nil {
				return err
			}
		}
		return nil
	}
	return s.runtime.Subscribe(ctx, current.WorkerSocket, cursor, func(message worker.Message) error {
		if message.Type != "line" {
			return nil
		}
		return consume(sessionlog.Line{Seq: message.Seq, Timestamp: message.Timestamp, RequestID: message.RequestID, Type: message.LineType, Data: message.Data})
	})
}

func (s *SessionService) logPath(current session.Session) string {
	if current.LogPath != "" {
		return current.LogPath
	}
	return filepath.Join(s.home.Logs, current.ID+".jsonl")
}

func resolveDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", absPath)
	}
	return filepath.Clean(absPath), nil
}
