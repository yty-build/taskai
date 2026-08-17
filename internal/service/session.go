// Package service contains shared Task Console application behavior used by
// CLI, terminal, API, and web transports.
package service

import (
	"context"
	"encoding/json"
	"errors"
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
	Runner                native.Runner
	Verifier              native.Verifier
	OwnerToken            string
	ReservationGeneration string
	Input                 io.Reader
	Output                io.Writer
	ErrorOutput           io.Writer
}

type NativeReservationRecoveryOptions struct {
	Verifier native.ReservationRecoveryVerifier
}

type NativeReservationRecoveryResult struct {
	State string
}

var ErrWorkerUpgradeRequired = errors.New("Session worker upgrade required")

// WorkerUpgradeRequiredError is safe to expose through local transports. It
// contains only Task Console identifiers and fixed recovery instructions—no
// provider payload, authentication state, filesystem path, or socket detail.
type WorkerUpgradeRequiredError struct {
	SessionID       string
	Operation       string
	ProtocolVersion int
}

func (e *WorkerUpgradeRequiredError) Error() string {
	return fmt.Sprintf(
		"Session %s worker upgrade required; %s. Exit any provider CLI in tmux Session %s first. Then run `acpctl session stop %s` and `acpctl session resume %s`. Worker protocol %d does not satisfy the required protocol %d and capability %s contract",
		e.SessionID, e.Operation, native.TmuxSessionName(e.SessionID), e.SessionID, e.SessionID, e.ProtocolVersion, worker.ProtocolVersion, worker.CapabilityExclusiveNativeOwner,
	)
}

func (e *WorkerUpgradeRequiredError) Unwrap() error { return ErrWorkerUpgradeRequired }

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
	if err := s.requireCurrentWorkerProtocol(ctx, current, "prompt not sent"); err != nil {
		if errors.Is(err, ErrWorkerUpgradeRequired) {
			return worker.Message{}, err
		}
		return worker.Message{}, fmt.Errorf("Session %s worker capability check failed; prompt not sent: %w", id, err)
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
	if err := s.requireCurrentWorkerProtocol(ctx, current, "permission not sent"); err != nil {
		if errors.Is(err, ErrWorkerUpgradeRequired) {
			return err
		}
		return fmt.Errorf("Session %s worker capability check failed; permission not sent: %w", id, err)
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
		protocolErr := s.requireCurrentWorkerProtocol(ctx, current, "resume not performed")
		if protocolErr == nil {
			return current, fmt.Errorf("Session %s worker is already running", id)
		}
		if errors.Is(protocolErr, ErrWorkerUpgradeRequired) {
			return session.Session{}, protocolErr
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
// canonical provider conversation. The worker grants one exclusive provider
// writer before the provider starts; attachment itself remains a disposable
// viewer and never owns a tmux-hosted provider lifecycle.
func (s *SessionService) AttachNative(ctx context.Context, id string, options NativeAttachOptions) error {
	explicitOwnerProcess := strings.TrimSpace(options.OwnerToken) != ""
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
	if current.WorkerSocket == "" {
		return fmt.Errorf("Session %s has no live worker to coordinate exclusive provider access; resume the Session before native attachment", id)
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
	ownerToken, err := native.OwnerToken(s.home.Root, current.ID)
	if err != nil {
		return err
	}
	if options.OwnerToken != "" && options.OwnerToken != ownerToken {
		return fmt.Errorf("Session %s native owner token does not match this Agent Task Console state root", id)
	}
	options.OwnerToken = ownerToken
	if err := s.requireExclusiveNativeWorker(ctx, current); err != nil {
		return err
	}
	if options.Runner == nil {
		if strings.TrimSpace(s.executable) != "" {
			options.Runner = &native.TmuxRunner{
				Executable: s.executable, ConsoleSessionID: current.ID, StateRoot: s.home.Root, OwnerToken: ownerToken,
			}
		} else {
			options.Runner = &native.ExecRunner{}
		}
	} else if runner, ok := options.Runner.(*native.TmuxRunner); ok {
		if runner.OwnerToken != "" && runner.OwnerToken != ownerToken {
			return fmt.Errorf("Session %s native runner owner token does not match", id)
		}
		runner.OwnerToken = ownerToken
	}
	ownerRole := worker.NativeOwnerRoleDirect
	reservationRunner, usesReservation := options.Runner.(native.ReservationRunner)
	if usesReservation && !explicitOwnerProcess {
		ownerRole = worker.NativeOwnerRoleReservation
	} else if explicitOwnerProcess {
		ownerRole = worker.NativeOwnerRoleProvider
	}
	acquired, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: ownerRole,
		NativeGeneration: options.ReservationGeneration,
	})
	if err != nil {
		return fmt.Errorf("Session %s native attachment could not acquire exclusive provider access: %w", id, err)
	}
	if acquired.Type != "native_acquired" {
		return fmt.Errorf("Session %s native attachment received unexpected worker response %q", id, acquired.Type)
	}
	validAcquisition := false
	switch ownerRole {
	case worker.NativeOwnerRoleReservation:
		validAcquisition = acquired.State == "reserved" || acquired.State == "already_reserved" || acquired.State == "already_acquired"
	case worker.NativeOwnerRoleProvider:
		validAcquisition = acquired.State == "activated"
	case worker.NativeOwnerRoleDirect:
		validAcquisition = acquired.State == "acquired" || acquired.State == "already_acquired"
	}
	if !validAcquisition {
		return fmt.Errorf("Session %s native attachment received unsafe acquisition state %q for role %q; preserving ownership and refusing to start a provider process", id, acquired.State, ownerRole)
	}
	if acquired.NativeGeneration == "" {
		return fmt.Errorf("Session %s native attachment received no ownership generation; preserving ownership and refusing to start a provider process", id)
	}
	if options.ReservationGeneration != "" && acquired.NativeGeneration != options.ReservationGeneration {
		return fmt.Errorf("Session %s native ownership generation changed; preserving ownership and refusing a stale provider process", id)
	}
	options.ReservationGeneration = acquired.NativeGeneration
	preexistingOuterOwnership := (acquired.State == "already_reserved" || acquired.State == "already_acquired") && !explicitOwnerProcess
	ownershipAware, ownershipAwareOK := options.Runner.(native.PreexistingOwnershipAwareRunner)
	if preexistingOuterOwnership && !ownershipAwareOK {
		return fmt.Errorf("Session %s already has native ownership, but this outer viewer cannot prove attach-only behavior; preserving ownership and refusing to start a second provider process", id)
	}
	if ownershipAwareOK {
		if stateAware, ok := options.Runner.(native.PreexistingOwnershipStateAwareRunner); ok {
			phase := ""
			switch acquired.State {
			case "already_reserved":
				phase = native.PreexistingOwnershipReserved
			case "already_acquired":
				phase = native.PreexistingOwnershipActive
			}
			stateAware.SetPreexistingOwnershipState(phase)
		} else {
			ownershipAware.SetPreexistingOwnership(preexistingOuterOwnership)
		}
	}
	if usesReservation && acquired.State == "reserved" {
		reservationRunner.SetReservationGeneration(acquired.NativeGeneration)
		reservationRunner.SetReservationActivator(func(activateContext context.Context) error {
			activated, activateErr := s.runtime.Send(activateContext, current.WorkerSocket, worker.Command{
				Type: "native_acquire", OwnerToken: ownerToken, NativeOwnerRole: worker.NativeOwnerRoleProvider,
				NativeGeneration: acquired.NativeGeneration,
			})
			if activateErr != nil {
				return activateErr
			}
			if activated.Type != "native_acquired" || activated.State != "activated" {
				return fmt.Errorf("unexpected reservation activation response %q/%q; refusing to start a duplicate provider process", activated.Type, activated.State)
			}
			return nil
		})
	} else if usesReservation {
		reservationRunner.SetReservationGeneration(acquired.NativeGeneration)
	}
	runErr := options.Runner.Run(ctx, invocation, options.Input, options.Output, options.ErrorOutput)
	mayContinue := false
	if aware, ok := options.Runner.(native.MayContinueRunner); ok {
		mayContinue = aware.ProcessMayContinue()
	}
	activityOccurred := true
	if aware, ok := options.Runner.(native.ActivityAwareRunner); ok {
		activityOccurred = aware.NativeActivityOccurred()
	}
	// An outer viewer that receives an existing generation did not create its
	// ownership and must never release it after its own attach failure. The
	// private _native process supplies both the stable token and exact generation;
	// it is the lifecycle owner and releases after the provider process exits.
	shouldRelease := acquired.State == "acquired" || acquired.State == "reserved" || explicitOwnerProcess
	if !mayContinue && shouldRelease {
		releaseErr := s.releaseNativeOwnership(current, ownerToken, acquired.NativeGeneration, activityOccurred, options.ErrorOutput)
		if releaseErr != nil && runErr == nil {
			return releaseErr
		}
		if releaseErr != nil {
			return fmt.Errorf("native harness for Session %s: %v; additionally failed to release exclusive provider access: %w", id, runErr, releaseErr)
		}
	}
	if runErr != nil {
		return fmt.Errorf("native harness for Session %s: %w", id, runErr)
	}
	return nil
}

// RecoverNativeReservation clears only an outer tmux reservation that never
// became an active provider owner. The exact stable tmux harness must first be
// proven absent; an active marker remains fail-closed even when tmux is gone.
func (s *SessionService) RecoverNativeReservation(ctx context.Context, id string, options NativeReservationRecoveryOptions) (NativeReservationRecoveryResult, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return NativeReservationRecoveryResult{}, err
	}
	if current.WorkerSocket == "" {
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s has no live worker; resume it before native reservation recovery", id)
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "native reservation not recovered"); err != nil {
		return NativeReservationRecoveryResult{}, err
	}
	ownerToken, err := native.OwnerToken(s.home.Root, current.ID)
	if err != nil {
		return NativeReservationRecoveryResult{}, err
	}
	reservation, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "native_reservation_status", OwnerToken: ownerToken,
	})
	if err != nil {
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery refused: %w", id, err)
	}
	if reservation.Type != "native_reservation_status" {
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery received unexpected worker response %q", id, reservation.Type)
	}
	switch reservation.State {
	case "reserved":
		if reservation.NativeGeneration == "" {
			return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation has no generation; recovery is unsafe and remains blocked", id)
		}
	case "active":
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s has active provider-native ownership. Missing tmux Session %s does not prove provider exit; recovery is refused and ACP remains blocked", id, native.TmuxSessionName(id))
	case "released":
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s has no orphaned native reservation to recover", id)
	default:
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery received unknown ownership phase %q", id, reservation.State)
	}
	verifier := options.Verifier
	if verifier == nil {
		verifier = native.TmuxReservationRecoveryVerifier{}
	}
	if err := verifier.VerifyReservationAbsent(ctx, current.ID); err != nil {
		return NativeReservationRecoveryResult{}, err
	}
	recovered, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "native_recover_reservation", OwnerToken: ownerToken, NativeGeneration: reservation.NativeGeneration,
	})
	if err != nil {
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery refused: %w", id, err)
	}
	if recovered.Type != "native_reservation_recovered" {
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery received unexpected worker response %q", id, recovered.Type)
	}
	switch recovered.State {
	case "released", "already_released", "refresh_scheduled":
		return NativeReservationRecoveryResult{State: recovered.State}, nil
	default:
		return NativeReservationRecoveryResult{}, fmt.Errorf("Session %s native reservation recovery received unknown state %q", id, recovered.State)
	}
}

func (s *SessionService) requireCurrentWorkerProtocol(ctx context.Context, current session.Session, operation string) error {
	response, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "ping"})
	if err != nil {
		return err
	}
	if response.Type == "pong" && response.ProtocolVersion >= worker.ProtocolVersion && workerHasCapability(response, worker.CapabilityExclusiveNativeOwner) {
		return nil
	}
	return &WorkerUpgradeRequiredError{SessionID: current.ID, Operation: operation, ProtocolVersion: response.ProtocolVersion}
}

func (s *SessionService) requireExclusiveNativeWorker(ctx context.Context, current session.Session) error {
	err := s.requireCurrentWorkerProtocol(ctx, current, "native not started")
	if err == nil || errors.Is(err, ErrWorkerUpgradeRequired) {
		return err
	}
	return fmt.Errorf("Session %s worker capability check failed; native not started: %w", current.ID, err)
}

func workerHasCapability(response worker.Message, capability string) bool {
	for _, candidate := range response.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func (s *SessionService) releaseNativeOwnership(current session.Session, ownerToken, generation string, activity bool, notices io.Writer) error {
	releaseContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	message, err := s.runtime.Send(releaseContext, current.WorkerSocket, worker.Command{
		Type: "native_release", OwnerToken: ownerToken, NativeGeneration: generation, NativeActivity: activity,
	})
	if err != nil {
		return fmt.Errorf("release native provider ownership: %w", err)
	}
	if message.Type != "native_released" {
		return fmt.Errorf("unexpected worker response %q while releasing native provider ownership", message.Type)
	}
	if message.State == "refresh_scheduled" {
		fmt.Fprintln(notices, "Agent Task Console: native process exited; exact ACP refresh scheduled before queued prompts.")
	}
	return nil
}

func (s *SessionService) History(ctx context.Context, id string, cursor uint64) ([]sessionlog.Line, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return sessionlog.ReadFile(s.logPath(current), cursor)
}

// Replay streams durable history without materializing a long-lived Session's
// entire JSONL file. It returns the last position delivered so a transport can
// hand off to Follow without a gap.
func (s *SessionService) Replay(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) (uint64, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return cursor, err
	}
	return sessionlog.WalkFile(ctx, s.logPath(current), cursor, 0, func(line sessionlog.Line) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return consume(line)
	})
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
