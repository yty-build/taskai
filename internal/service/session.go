// Package service contains shared Task Console application behavior used by
// CLI, terminal, API, and web transports.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/identity"
	"github.com/yty-build/acp-gateway/internal/native"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type SessionRepository interface {
	task.Repository
	session.Repository
	ListSessions(context.Context) ([]session.Session, error)
	CreateTaskWithSession(context.Context, task.CreateParams, session.CreateParams) (task.Task, session.Session, error)
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
	executable string
}

type StartSessionParams struct {
	TaskID              string
	Agent               string
	Role                string
	RepoPath            string
	WorktreePath        string
	Branch              string
	Worktree            bool
	AllowSharedCheckout bool
	Prompt              string
	ApproveAll          bool
}

type LaunchResult struct {
	Session   session.Session
	RequestID string
	State     string
}

type TaskLaunchResult struct {
	Task task.Task
	LaunchResult
}

type NativeAttachOptions struct {
	Runner      native.Runner
	Verifier    native.Verifier
	Input       io.Reader
	Output      io.Writer
	ErrorOutput io.Writer
}

func NewSessionService(repository SessionRepository, registry *agents.Registry, home config.Home, runtime Runtime) *SessionService {
	service := &SessionService{repository: repository, registry: registry, home: home, runtime: runtime}
	switch typed := runtime.(type) {
	case LocalRuntime:
		service.executable = typed.Executable
	case *LocalRuntime:
		service.executable = typed.Executable
	}
	return service
}

// AgentProfiles exposes the declarative registry through the shared service
// boundary so terminal, web, and future clients do not read registry files or
// duplicate provider knowledge.
func (s *SessionService) AgentProfiles() ([]agents.Profile, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("agent registry is unavailable")
	}
	return s.registry.List(), nil
}

func (s *SessionService) Prepare(params StartSessionParams) (session.CreateParams, error) {
	return s.PrepareContext(context.Background(), params)
}

func (s *SessionService) PrepareContext(ctx context.Context, params StartSessionParams) (session.CreateParams, error) {
	profile, err := s.registry.Get(params.Agent)
	if err != nil {
		return session.CreateParams{}, err
	}
	repoPath, err := gitrepo.ValidateRepository(ctx, params.RepoPath)
	if err != nil {
		return session.CreateParams{}, fmt.Errorf("resolve repo: %w", err)
	}
	worktreePath := ""
	branch := params.Branch
	if params.Worktree && params.WorktreePath != "" {
		return session.CreateParams{}, fmt.Errorf("--worktree cannot be combined with an existing worktree path")
	}
	if params.WorktreePath != "" {
		worktreePath, err = gitrepo.ValidateRepository(ctx, params.WorktreePath)
		if err != nil {
			return session.CreateParams{}, fmt.Errorf("resolve worktree: %w", err)
		}
		same, err := gitrepo.SameRepository(ctx, repoPath, worktreePath)
		if err != nil {
			return session.CreateParams{}, err
		}
		if !same {
			return session.CreateParams{}, fmt.Errorf("worktree %q does not belong to repository %q", worktreePath, repoPath)
		}
	}
	id, err := identity.New("S")
	if err != nil {
		return session.CreateParams{}, err
	}
	if params.Worktree {
		if params.TaskID == "" {
			return session.CreateParams{}, fmt.Errorf("Task ID is required to create an isolated worktree")
		}
		planned, err := gitrepo.PlanWorktree(s.home.Worktrees, repoPath, params.TaskID, id)
		if err != nil {
			return session.CreateParams{}, err
		}
		worktreePath, branch = planned.Path, planned.Branch
	} else if worktreePath != "" {
		branch, err = gitrepo.CurrentBranch(ctx, worktreePath)
		if err != nil {
			return session.CreateParams{}, err
		}
	} else if branch == "" {
		branch, err = gitrepo.CurrentBranch(ctx, repoPath)
		if err != nil {
			return session.CreateParams{}, err
		}
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
		RepoPath: repoPath, WorktreePath: worktreePath, Branch: branch,
		Status: session.StatusStarting, LogPath: filepath.Join(s.home.Logs, id+".jsonl"), MetadataJSON: metadata,
	}, nil
}

func (s *SessionService) Start(ctx context.Context, params StartSessionParams) (LaunchResult, error) {
	if _, err := s.repository.GetTask(ctx, params.TaskID); err != nil {
		return LaunchResult{}, err
	}
	createParams, err := s.PrepareContext(ctx, params)
	if err != nil {
		return LaunchResult{}, err
	}
	created, err := s.createSessionContext(ctx, createParams, params.Worktree, params.AllowSharedCheckout)
	if err != nil {
		return LaunchResult{Session: created}, err
	}
	if _, err := s.repository.UpdateTaskStatus(ctx, params.TaskID, task.StatusWorking); err != nil {
		return LaunchResult{Session: created}, err
	}
	return s.Launch(ctx, created.ID, params.Prompt)
}

// StartTask atomically creates the convenience Task -> first Session pair,
// then provisions any requested worktree before launching its worker.
func (s *SessionService) StartTask(ctx context.Context, taskParams task.CreateParams, params StartSessionParams) (TaskLaunchResult, error) {
	if taskParams.ID == "" {
		id, err := identity.New("T")
		if err != nil {
			return TaskLaunchResult{}, err
		}
		taskParams.ID = id
	}
	params.TaskID = taskParams.ID
	createParams, err := s.PrepareContext(ctx, params)
	if err != nil {
		return TaskLaunchResult{}, err
	}
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, createParams.RepoPath)
	if err != nil {
		return TaskLaunchResult{}, err
	}
	defer func() { _ = gitrepo.UnlockRepository(lock) }() // best-effort release; persistence errors remain primary
	if err := s.checkCheckout(ctx, createParams, params.Worktree, params.AllowSharedCheckout, ""); err != nil {
		return TaskLaunchResult{}, err
	}
	createdTask, createdSession, err := s.repository.CreateTaskWithSession(ctx, taskParams, createParams)
	if err != nil {
		return TaskLaunchResult{}, err
	}
	partial := TaskLaunchResult{Task: createdTask, LaunchResult: LaunchResult{Session: createdSession}}
	if params.Worktree {
		planned := gitrepo.Worktree{Repository: createdSession.RepoPath, Path: createdSession.WorktreePath, Branch: createdSession.Branch}
		if err := planned.Create(ctx); err != nil {
			failed, _ := s.repository.UpdateSessionStatus(context.Background(), createdSession.ID, session.StatusFailed)
			partial.Session = failed
			_, _ = s.repository.UpdateTaskStatus(context.Background(), createdTask.ID, task.StatusFailed)
			return partial, err
		}
	}
	if err := gitrepo.UnlockRepository(lock); err != nil {
		return partial, err
	}
	lock = nil
	launched, err := s.Launch(ctx, createdSession.ID, params.Prompt)
	partial.LaunchResult = launched
	return partial, err
}

func (s *SessionService) createSessionContext(ctx context.Context, createParams session.CreateParams, createWorktree, allowShared bool) (session.Session, error) {
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, createParams.RepoPath)
	if err != nil {
		return session.Session{}, err
	}
	defer gitrepo.UnlockRepository(lock)
	if err := s.checkCheckout(ctx, createParams, createWorktree, allowShared, ""); err != nil {
		return session.Session{}, err
	}
	created, err := s.repository.CreateSession(ctx, createParams)
	if err != nil {
		return session.Session{}, err
	}
	if createWorktree {
		planned := gitrepo.Worktree{Repository: created.RepoPath, Path: created.WorktreePath, Branch: created.Branch}
		if err := planned.Create(ctx); err != nil {
			failed, _ := s.repository.UpdateSessionStatus(context.Background(), created.ID, session.StatusFailed)
			return failed, err
		}
	}
	return created, nil
}

func (s *SessionService) checkCheckout(ctx context.Context, createParams session.CreateParams, createWorktree, allowShared bool, excludeSessionID string) error {
	if createWorktree {
		planned := gitrepo.Worktree{Repository: createParams.RepoPath, Path: createParams.WorktreePath, Branch: createParams.Branch}
		return planned.CheckAvailable(ctx)
	}
	if allowShared {
		return nil
	}
	checkout := createParams.WorktreePath
	if checkout == "" {
		checkout = createParams.RepoPath
	}
	existing, err := s.repository.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, current := range existing {
		if current.ID == excludeSessionID {
			continue
		}
		if !activeCheckoutStatus(current.Status) {
			continue
		}
		currentCheckout := current.WorktreePath
		if currentCheckout == "" {
			currentCheckout = current.RepoPath
		}
		if gitrepo.SameCheckout(checkout, currentCheckout) {
			return fmt.Errorf("checkout %q is already used by active Session %s (%s); use --worktree or --allow-shared-checkout", checkout, current.ID, current.Status)
		}
	}
	return nil
}

func activeCheckoutStatus(status session.Status) bool {
	return status == session.StatusStarting || status == session.StatusWorking || status == session.StatusWaiting
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
	displayMessage := BuildInitialDisplayMessage(taskItem, current, extraPrompt)
	accepted, err := s.runtime.Send(ctx, spawned.Socket, worker.Command{
		Type: "prompt", RequestID: requestID, Message: message, DisplayMessage: displayMessage,
	})
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

// BuildInitialDisplayMessage is the clean human-facing USER block for the
// injected initial turn. The complete ACP prompt still carries IDs and checkout
// context, but the transcript does not scrape or pretend that transport text
// was typed by the user.
func BuildInitialDisplayMessage(taskItem task.Task, current session.Session, extra string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Start work on %q as %s.", taskItem.Title, valueOrRole(current.Role))
	if strings.TrimSpace(taskItem.Description) != "" {
		fmt.Fprintf(&builder, "\n\n%s", strings.TrimSpace(taskItem.Description))
	}
	if strings.TrimSpace(extra) != "" {
		fmt.Fprintf(&builder, "\n\n%s", strings.TrimSpace(extra))
	}
	return builder.String()
}

func valueOrRole(role string) string {
	if strings.TrimSpace(role) == "" {
		return "worker"
	}
	return strings.TrimSpace(role)
}

func PermissionMetadata(approveAll bool) (json.RawMessage, error) {
	permission := string(acp.PermissionReadApproval)
	if approveAll {
		permission = string(acp.PermissionApproveAll)
	}
	encoded, err := json.Marshal(map[string]string{"permission": permission, "acp_transport": acp.RuntimeTransport})
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
		return worker.Message{}, fmt.Errorf("Session %s has no live worker; run 'acpctl session resume %s'", id, id)
	}
	requestID, err := acp.NewRequestID()
	if err != nil {
		return worker.Message{}, err
	}
	return s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "prompt", RequestID: requestID, Message: message, DisplayMessage: message,
	})
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

// DecidePermission resolves one currently pending ACP permission request using
// an exact option offered by that provider. The Session worker validates both
// the request and option and makes concurrent viewer decisions first-wins.
func (s *SessionService) DecidePermission(ctx context.Context, id, permissionID, optionID string, cancelled bool) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		return fmt.Errorf("Session %s has no live worker", id)
	}
	if strings.TrimSpace(permissionID) == "" {
		return fmt.Errorf("permission ID is required")
	}
	if !cancelled && strings.TrimSpace(optionID) == "" {
		return fmt.Errorf("permission option ID is required")
	}
	_, err = s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "permission_decision", PermissionID: permissionID, OptionID: optionID, Cancelled: cancelled,
	})
	return err
}

// PendingPermissions returns the worker's current actionable permission state.
// A stopped Session has no in-memory pending callback, so it returns an empty
// slice while durable history remains available through History/Follow.
func (s *SessionService) PendingPermissions(ctx context.Context, id string) ([]worker.PendingPermission, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.WorkerSocket == "" {
		return []worker.PendingPermission{}, nil
	}
	status, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "status"})
	if err != nil {
		return nil, err
	}
	return append([]worker.PendingPermission(nil), status.Permissions...), nil
}

func (s *SessionService) Resume(ctx context.Context, id string) (session.Session, error) {
	return s.ResumeWithOptions(ctx, id, false)
}

func (s *SessionService) ResumeWithOptions(ctx context.Context, id string, allowSharedCheckout bool) (session.Session, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if current.WorkerSocket != "" {
		if _, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "ping"}); err == nil {
			return current, fmt.Errorf("Session %s worker is already running", id)
		}
	}
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, current.RepoPath)
	if err != nil {
		return session.Session{}, err
	}
	defer gitrepo.UnlockRepository(lock)
	if err := s.checkCheckout(ctx, session.CreateParams{
		ID: current.ID, RepoPath: current.RepoPath, WorktreePath: current.WorktreePath,
	}, false, allowSharedCheckout, current.ID); err != nil {
		return session.Session{}, err
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

// AttachNative opens the real provider harness for the Session's exact
// canonical provider conversation. ACP work is deliberately left untouched;
// cross-surface coordination belongs to the user.
func (s *SessionService) AttachNative(ctx context.Context, id string, options NativeAttachOptions) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if options.ErrorOutput == nil {
		options.ErrorOutput = io.Discard
	}
	if options.Output == nil {
		options.Output = io.Discard
	}
	if options.Input == nil {
		options.Input = strings.NewReader("")
	}
	conversationID := strings.TrimSpace(current.ProviderConversationID)
	if conversationID == "" {
		return fmt.Errorf("Session %s has no canonical provider conversation ID; native attachment is unavailable and no replacement conversation was started", id)
	}
	if s.registry == nil {
		return fmt.Errorf("agent registry is required for provider-native attachment")
	}
	profile, err := s.registry.Get(current.Agent)
	if err != nil {
		return err
	}
	workingDirectory := current.WorktreePath
	if workingDirectory == "" {
		workingDirectory = current.RepoPath
	}
	if options.Verifier == nil {
		options.Verifier = native.StoreVerifier{}
	}
	if _, err := options.Verifier.Verify(ctx, profile, conversationID, workingDirectory); err != nil {
		return fmt.Errorf("Session %s native attachment: %w", id, err)
	}
	verifiedAt := time.Now().UTC()
	current, err = s.repository.UpdateSessionConversation(ctx, current.ID, conversationID, session.ConversationSourceProviderStore, &verifiedAt)
	if err != nil {
		return err
	}
	invocation, err := native.BuildResume(profile, conversationID, workingDirectory)
	if err != nil {
		return fmt.Errorf("Session %s native attachment: %w", id, err)
	}
	if options.Runner == nil {
		if strings.TrimSpace(s.executable) != "" {
			options.Runner = &native.TmuxRunner{Executable: s.executable, ConsoleSessionID: current.ID, StateRoot: s.home.Root}
		} else {
			options.Runner = &native.ExecRunner{}
		}
	}
	s.writeNativeConcurrencyNotice(ctx, current, options.ErrorOutput)
	runErr := options.Runner.Run(ctx, invocation, options.Input, options.Output, options.ErrorOutput)
	mayContinue := false
	if aware, ok := options.Runner.(native.MayContinueRunner); ok {
		mayContinue = aware.ProcessMayContinue()
	}
	activityOccurred := true
	if aware, ok := options.Runner.(native.ActivityAwareRunner); ok {
		activityOccurred = aware.NativeActivityOccurred()
	}
	if activityOccurred && !mayContinue {
		s.requestNativeRefresh(current, options.ErrorOutput)
	}
	if runErr != nil {
		return fmt.Errorf("native harness for Session %s: %w", id, runErr)
	}
	return nil
}

func (s *SessionService) writeNativeConcurrencyNotice(ctx context.Context, current session.Session, notices io.Writer) {
	if current.WorkerSocket == "" {
		return
	}
	status, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "status"})
	if err != nil {
		return
	}
	if status.ActiveRequestID != "" || status.Queued > 0 || status.Refreshing {
		fmt.Fprintln(notices, "Agent Task Console: ACP activity is also live. Native attachment will not pause or cancel it; avoid steering both surfaces concurrently unless the provider supports it.")
	}
}

func (s *SessionService) requestNativeRefresh(current session.Session, notices io.Writer) {
	if current.WorkerSocket == "" {
		return
	}
	refreshContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	message, err := s.runtime.Send(refreshContext, current.WorkerSocket, worker.Command{Type: "refresh"})
	if err != nil {
		fmt.Fprintf(notices, "Agent Task Console: native process exited, but ACP refresh could not be scheduled: %v\n", err)
		return
	}
	if message.State == "unavailable" {
		fmt.Fprintln(notices, "Agent Task Console: ACP refresh was not scheduled because the exact transport is unavailable; resume the stopped Session before sending more work.")
	} else if message.State == "deferred" {
		fmt.Fprintln(notices, "Agent Task Console: ACP refresh is deferred until the active turn and queued prompts finish.")
	} else {
		fmt.Fprintln(notices, "Agent Task Console: ACP refresh scheduled at the idle boundary.")
	}
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
