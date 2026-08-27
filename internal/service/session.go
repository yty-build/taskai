// Package service contains the application behavior shared by the HTTP, CLI,
// and terminal transports. A Session owns one provider connection; transports
// only address that Session and observe its ordered frame journal.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
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
}

// StartSessionParams contains Session identity and checkout configuration.
// ParentSessionID is optional manager-to-child membership; it never implies a
// mailbox, workflow, or hidden prompt fan-out.
type StartSessionParams struct {
	TaskID              string
	ParentSessionID     string
	Agent               string
	Role                string
	RepoPath            string
	WorktreePath        string
	Branch              string
	Worktree            bool
	AllowSharedCheckout bool
	ApproveAll          bool
}

type LaunchResult struct {
	Session   session.Session
	RequestID string
	State     string
	Started   bool
}

// ConnectedSession is one ordinary addressable Session Channel. ProviderOptions
// is populated only by an explicit observational read after its Port connects.
type ConnectedSession struct {
	Session         session.Session
	ProviderOptions acp.ProviderOptionCatalog
}

type ConnectedTask struct {
	Task task.Task
	ConnectedSession
}

type PermissionSnapshot struct {
	Permissions    []worker.PendingPermission
	Sequence       uint64
	DecisionsReady bool
	BlockedCode    string
	BlockedMessage string
}

var ErrWorkerUpgradeRequired = errors.New("Session worker upgrade required")

type WorkerUpgradeRequiredError struct {
	SessionID          string
	Operation          string
	ProtocolVersion    int
	RequiredCapability string
}

func (e *WorkerUpgradeRequiredError) Error() string {
	return fmt.Sprintf("Session %s worker protocol %d cannot perform %s; stop and resume the Session with the current acpctl build", e.SessionID, e.ProtocolVersion, e.Operation)
}

func (e *WorkerUpgradeRequiredError) Unwrap() error { return ErrWorkerUpgradeRequired }

func NewSessionService(repository SessionRepository, registry *agents.Registry, home config.Home, runtime Runtime) *SessionService {
	return &SessionService{repository: repository, registry: registry, home: home, runtime: runtime}
}

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
	if s.registry == nil {
		return session.CreateParams{}, fmt.Errorf("agent registry is unavailable")
	}
	profile, err := s.registry.Get(params.Agent)
	if err != nil {
		return session.CreateParams{}, err
	}
	if params.ParentSessionID != "" {
		parent, err := s.repository.GetSession(ctx, params.ParentSessionID)
		if err != nil {
			return session.CreateParams{}, fmt.Errorf("load parent Session: %w", err)
		}
		if params.TaskID == "" {
			params.TaskID = parent.TaskID
		}
		if parent.TaskID != params.TaskID {
			return session.CreateParams{}, fmt.Errorf("parent Session %s belongs to a different Task", parent.ID)
		}
	}
	repoPath, err := gitrepo.ValidateRepository(ctx, params.RepoPath)
	if err != nil {
		return session.CreateParams{}, fmt.Errorf("resolve repo: %w", err)
	}
	worktreePath := ""
	branch := strings.TrimSpace(params.Branch)
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
	metadata, err := sessionMetadata(params.ApproveAll, params.AllowSharedCheckout, params.Worktree)
	if err != nil {
		return session.CreateParams{}, err
	}
	return session.CreateParams{
		ID: id, TaskID: params.TaskID, ParentSessionID: params.ParentSessionID,
		Role: role, Agent: profile.Name, Provider: profile.Provider,
		RepoPath: repoPath, WorktreePath: worktreePath, Branch: branch,
		Status: session.StatusStarting, LogPath: sessionlog.ChannelPath(s.home.Logs, id), MetadataJSON: metadata,
	}, nil
}

// ConnectSession creates one ordinary Session and returns as soon as its
// Session Channel is attachable. Provider Start/Resume continues behind the
// channel and reports connecting/connected/failed as ordered frames.
func (s *SessionService) ConnectSession(ctx context.Context, params StartSessionParams) (ConnectedSession, error) {
	if _, err := s.repository.GetTask(ctx, params.TaskID); err != nil {
		return ConnectedSession{}, err
	}
	createParams, err := s.PrepareContext(ctx, params)
	if err != nil {
		return ConnectedSession{}, err
	}
	created, err := s.createSessionContext(ctx, createParams, params.Worktree, params.AllowSharedCheckout)
	if err != nil {
		return ConnectedSession{Session: created}, err
	}
	spawned, err := s.runtime.Spawn(ctx, created.ID)
	if err != nil {
		return ConnectedSession{Session: s.sessionAfterSpawnFailure(created)}, err
	}
	// Return the channel-admission snapshot deterministically. The provider may
	// advance SQLite to waiting or failed immediately after the successful ping,
	// but that race must not erase the client's attachable starting boundary.
	admitted := created
	admitted.WorkerPID = spawned.PID
	admitted.WorkerSocket = spawned.Socket
	admitted.Status = session.StatusStarting
	if _, err := s.repository.UpdateTaskStatus(ctx, created.TaskID, task.StatusWorking); err != nil {
		return ConnectedSession{Session: admitted}, err
	}
	return ConnectedSession{Session: admitted}, nil
}

func (s *SessionService) ProviderOptions(ctx context.Context, id string) (ConnectedSession, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return ConnectedSession{}, err
	}
	if err := requireProviderReady(current); err != nil {
		return ConnectedSession{Session: current}, err
	}
	if current.WorkerSocket == "" {
		return ConnectedSession{Session: current}, fmt.Errorf("Session %s has no live provider connection; resume it first", id)
	}
	return s.providerOptionsFromWorker(ctx, id, current.WorkerSocket)
}

func (s *SessionService) providerOptionsFromWorker(ctx context.Context, id, socket string) (ConnectedSession, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return ConnectedSession{}, err
	}
	if err := s.requireCurrentWorkerProtocolAt(ctx, current, socket, "load provider options"); err != nil {
		return ConnectedSession{Session: current}, err
	}
	message, err := s.runtime.Send(ctx, socket, worker.Command{Type: "provider_options"})
	if err != nil {
		return ConnectedSession{Session: current}, err
	}
	if message.ProviderOptions == nil {
		return ConnectedSession{Session: current}, fmt.Errorf("Session %s provider did not return a configuration catalog", id)
	}
	return ConnectedSession{Session: current, ProviderOptions: *message.ProviderOptions}, nil
}

// Configure applies a tuple chosen from this live provider's advertised
// catalog to the retained Port. Provider configuration is disposable live
// state; the gateway does not persist or reapply it after a Port restart.
func (s *SessionService) Configure(ctx context.Context, id string, options session.ProviderOptions) (ConnectedSession, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return ConnectedSession{}, err
	}
	if err := requireProviderReady(current); err != nil {
		return ConnectedSession{Session: current}, err
	}
	if current.WorkerSocket == "" {
		return ConnectedSession{Session: current}, fmt.Errorf("Session %s has no live provider connection; resume it first", id)
	}
	if err := options.Validate(); err != nil {
		return ConnectedSession{Session: current}, err
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "configure provider"); err != nil {
		return ConnectedSession{Session: current}, err
	}
	message, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{
		Type: "configure", Model: options.Model, ReasoningEffort: options.ReasoningEffort,
	})
	if err != nil {
		return ConnectedSession{Session: current}, err
	}
	updated, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return ConnectedSession{Session: current}, err
	}
	result := ConnectedSession{Session: updated}
	if message.ProviderOptions != nil {
		result.ProviderOptions = *message.ProviderOptions
	}
	return result, nil
}

// ConnectTask atomically creates a Task and its first Session, then returns as
// soon as that Session Channel is addressable. It never waits for provider
// readiness, configures the Port, or submits a hidden first prompt.
func (s *SessionService) ConnectTask(ctx context.Context, taskParams task.CreateParams, params StartSessionParams) (ConnectedTask, error) {
	if taskParams.ID == "" {
		id, err := identity.New("T")
		if err != nil {
			return ConnectedTask{}, err
		}
		taskParams.ID = id
	}
	params.TaskID = taskParams.ID
	createParams, err := s.PrepareContext(ctx, params)
	if err != nil {
		return ConnectedTask{}, err
	}
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, createParams.RepoPath)
	if err != nil {
		return ConnectedTask{}, err
	}
	defer func() { _ = gitrepo.UnlockRepository(lock) }()
	if err := s.checkCheckout(ctx, createParams, params.Worktree, params.AllowSharedCheckout, ""); err != nil {
		return ConnectedTask{}, err
	}
	createdTask, created, err := s.repository.CreateTaskWithSession(ctx, taskParams, createParams)
	if err != nil {
		return ConnectedTask{}, err
	}
	partial := ConnectedTask{Task: createdTask, ConnectedSession: ConnectedSession{Session: created}}
	if params.Worktree {
		planned := gitrepo.Worktree{Repository: created.RepoPath, Path: created.WorktreePath, Branch: created.Branch}
		if err := planned.Create(ctx); err != nil {
			partial.Session = s.markSessionFailed(created)
			return partial, err
		}
	}
	if err := gitrepo.UnlockRepository(lock); err != nil {
		partial.Session = s.markSessionFailed(created)
		return partial, err
	}
	lock = nil
	spawned, err := s.runtime.Spawn(ctx, created.ID)
	if err != nil {
		partial.Session = s.sessionAfterSpawnFailure(created)
		return partial, err
	}
	partial.Session.WorkerPID = spawned.PID
	partial.Session.WorkerSocket = spawned.Socket
	partial.Session.Status = session.StatusStarting
	return partial, nil
}

func (s *SessionService) createSessionContext(ctx context.Context, createParams session.CreateParams, createWorktree, allowShared bool) (session.Session, error) {
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, createParams.RepoPath)
	if err != nil {
		return session.Session{}, err
	}
	defer func() { _ = gitrepo.UnlockRepository(lock) }()
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
			return s.markSessionFailed(created), err
		}
	}
	if err := gitrepo.UnlockRepository(lock); err != nil {
		return s.markSessionFailed(created), err
	}
	lock = nil
	return created, nil
}

func (s *SessionService) checkCheckout(ctx context.Context, createParams session.CreateParams, createWorktree, allowShared bool, excludeSessionID string) error {
	if createWorktree {
		return (gitrepo.Worktree{Repository: createParams.RepoPath, Path: createParams.WorktreePath, Branch: createParams.Branch}).CheckAvailable(ctx)
	}
	if allowShared {
		return nil
	}
	checkout := createParams.WorktreePath
	if checkout == "" {
		checkout = createParams.RepoPath
	}
	items, err := s.repository.ListSessions(ctx)
	if err != nil {
		return err
	}
	for _, current := range items {
		retainsWorkerOwnership := current.WorkerPID != 0 || current.WorkerSocket != ""
		if current.ID == excludeSessionID || (!activeCheckoutStatus(current.Status) && !retainsWorkerOwnership) {
			continue
		}
		other := current.WorktreePath
		if other == "" {
			other = current.RepoPath
		}
		if gitrepo.SameCheckout(checkout, other) {
			return fmt.Errorf("checkout %q is already used by active Session %s (%s); use --worktree or --allow-shared-checkout", checkout, current.ID, current.Status)
		}
	}
	return nil
}

func activeCheckoutStatus(status session.Status) bool {
	return status == session.StatusStarting || status == session.StatusWorking || status == session.StatusWaiting
}

func (s *SessionService) markSessionFailed(current session.Session) session.Session {
	if current.ID == "" {
		return current
	}
	if current.Status != session.StatusStarting || current.WorkerPID != 0 || current.WorkerSocket != "" {
		if refreshed, err := s.repository.GetSession(context.Background(), current.ID); err == nil {
			return refreshed
		}
		return current
	}
	after := current.WorkerState()
	after.Status = session.StatusFailed
	if failed, _, err := s.repository.CompareAndSwapSessionWorker(context.Background(), current.ID, current.WorkerState(), after, ""); err == nil {
		return failed
	}
	if refreshed, err := s.repository.GetSession(context.Background(), current.ID); err == nil {
		return refreshed
	}
	return current
}

// sessionAfterSpawnFailure preserves a terminal state established by the
// worker or a concurrent lifecycle command. In particular, an orderly Stop can
// win while Spawn is still completing the private admission handshake; the
// losing Create call must not rewrite that stopped Session as failed.
func (s *SessionService) sessionAfterSpawnFailure(created session.Session) session.Session {
	return s.markSessionFailed(created)
}

func PermissionMetadata(approveAll bool) (json.RawMessage, error) {
	return sessionMetadata(approveAll, false, false)
}

func sessionMetadata(approveAll, allowShared, createWorktree bool) (json.RawMessage, error) {
	permission := string(acp.PermissionReadApproval)
	if approveAll {
		permission = string(acp.PermissionApproveAll)
	}
	metadata := map[string]any{
		"permission":            permission,
		"allow_shared_checkout": allowShared, "create_worktree": createWorktree,
	}
	encoded, err := json.Marshal(metadata)
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

func (s *SessionService) Children(ctx context.Context, parentID string) ([]session.Session, error) {
	parent, err := s.repository.GetSession(ctx, parentID)
	if err != nil {
		return nil, err
	}
	items, err := s.repository.ListSessionsByTask(ctx, parent.TaskID)
	if err != nil {
		return nil, err
	}
	children := make([]session.Session, 0)
	for _, item := range items {
		if item.ParentSessionID == parent.ID {
			children = append(children, item)
		}
	}
	return children, nil
}

// ConnectChild creates one ordinary child Session beneath a manager and opens
// its provider Port without sending a prompt. The caller can inspect the
// provider-advertised controls, configure the Port, and then address the child
// through the same prompt API used for every other Session.
func (s *SessionService) ConnectChild(ctx context.Context, parentID string, params StartSessionParams) (ConnectedSession, error) {
	parent, err := s.repository.GetSession(ctx, parentID)
	if err != nil {
		return ConnectedSession{}, err
	}
	params.TaskID, params.ParentSessionID = parent.TaskID, parent.ID
	if params.RepoPath == "" {
		params.RepoPath, params.WorktreePath, params.Branch = parent.RepoPath, parent.WorktreePath, parent.Branch
	}
	return s.ConnectSession(ctx, params)
}

func (s *SessionService) PromptChild(ctx context.Context, parentID, childID, message string) (worker.Message, error) {
	child, err := s.requireChild(ctx, parentID, childID)
	if err != nil {
		return worker.Message{}, err
	}
	return s.Prompt(ctx, child.ID, message)
}

func (s *SessionService) CancelChild(ctx context.Context, parentID, childID string) error {
	child, err := s.requireChild(ctx, parentID, childID)
	if err != nil {
		return err
	}
	return s.Cancel(ctx, child.ID)
}

func (s *SessionService) requireChild(ctx context.Context, parentID, childID string) (session.Session, error) {
	child, err := s.repository.GetSession(ctx, childID)
	if err != nil {
		return session.Session{}, err
	}
	if child.ParentSessionID != parentID {
		return session.Session{}, fmt.Errorf("Session %s is not a child of manager Session %s", childID, parentID)
	}
	return child, nil
}

func (s *SessionService) Prompt(ctx context.Context, id, message string) (worker.Message, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return worker.Message{}, err
	}
	if err := requireProviderReady(current); err != nil {
		return worker.Message{}, err
	}
	if current.WorkerSocket == "" {
		return worker.Message{}, fmt.Errorf("Session %s has no live provider connection; resume it first", id)
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "send prompt"); err != nil {
		return worker.Message{}, err
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
	if err := requireProviderReady(current); err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		return fmt.Errorf("Session %s has no live provider connection", id)
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "cancel provider turn"); err != nil {
		return err
	}
	_, err = s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "cancel"})
	return err
}

func (s *SessionService) DecidePermission(ctx context.Context, id, permissionID, optionID string, cancelled bool) error {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if err := requireProviderReady(current); err != nil {
		return err
	}
	if current.WorkerSocket == "" {
		return fmt.Errorf("Session %s has no live provider connection", id)
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "respond to provider permission"); err != nil {
		return err
	}
	if permissionID == "" || (!cancelled && optionID == "") {
		return fmt.Errorf("permission and offered option IDs are required")
	}
	_, err = s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "permission_response", PermissionID: permissionID, OptionID: optionID, Cancelled: cancelled})
	return err
}

func (s *SessionService) PendingPermissions(ctx context.Context, id string) ([]worker.PendingPermission, error) {
	snapshot, err := s.PendingPermissionSnapshot(ctx, id)
	return snapshot.Permissions, err
}

func (s *SessionService) PendingPermissionSnapshot(ctx context.Context, id string) (PermissionSnapshot, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return PermissionSnapshot{}, err
	}
	if current.Status == session.StatusStarting {
		return startingPermissionSnapshot(), nil
	}
	if current.WorkerSocket == "" {
		return PermissionSnapshot{Permissions: []worker.PendingPermission{}}, nil
	}
	if err := s.requireCurrentWorkerProtocol(ctx, current, "observe pending provider permissions"); err != nil {
		var upgrade *WorkerUpgradeRequiredError
		if errors.As(err, &upgrade) {
			return PermissionSnapshot{
				Permissions:    []worker.PendingPermission{},
				DecisionsReady: false,
				BlockedCode:    "worker_upgrade_required",
				BlockedMessage: "The older Session worker must be stopped and resumed before permission decisions are safe.",
			}, nil
		}
		return PermissionSnapshot{}, err
	}
	status, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "status"})
	if err != nil {
		return PermissionSnapshot{}, err
	}
	workerStatus := status.Status
	if workerStatus == "" {
		// Compatibility for application-runtime test doubles; the v7 worker uses
		// Status, while State belongs to command responses.
		workerStatus = status.State
	}
	result := PermissionSnapshot{
		Permissions: append([]worker.PendingPermission(nil), status.Permissions...),
		Sequence:    status.Seq,
	}
	switch session.Status(workerStatus) {
	case session.StatusWaiting, session.StatusWorking:
		if status.Message == "" {
			result.DecisionsReady = true
			break
		}
		result.BlockedCode = "session_channel_failed"
		result.BlockedMessage = "The provider connection is unavailable, so permission decisions are disabled."
	default:
		result.BlockedCode = "session_starting"
		result.BlockedMessage = "The provider connection is still starting, so permission decisions are not available yet."
	}
	return result, nil
}

func (s *SessionService) Resume(ctx context.Context, id string) (session.Session, error) {
	return s.ResumeWithOptions(ctx, id, false)
}

func (s *SessionService) ResumeWithOptions(ctx context.Context, id string, allowSharedCheckout bool) (session.Session, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	lock, err := gitrepo.LockRepository(ctx, s.home.Root, current.RepoPath)
	if err != nil {
		return session.Session{}, err
	}
	defer func() { _ = gitrepo.UnlockRepository(lock) }()
	current, err = s.repository.GetSession(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if current.WorkerSocket != "" {
		if err := s.requireCurrentWorkerProtocol(ctx, current, "resume"); err == nil {
			return current, fmt.Errorf("Session %s worker is already running", id)
		} else {
			var upgrade *WorkerUpgradeRequiredError
			if errors.As(err, &upgrade) {
				return current, err
			}
			if current.Status == session.StatusFailed {
				return current, fmt.Errorf("Session %s retains quarantined worker ownership because provider teardown was not proven; manual provider-process teardown is required before clearing ownership", id)
			}
			return current, fmt.Errorf("Session %s retains unresponsive worker ownership; stale-worker reconciliation will quarantine it, and manual provider-process teardown is required before ownership can be cleared", id)
		}
	}
	if current.WorkerPID != 0 {
		if current.Status == session.StatusFailed {
			return current, fmt.Errorf("Session %s retains quarantined worker ownership because provider teardown was not proven; manual provider-process teardown is required before clearing ownership", id)
		}
		return current, fmt.Errorf("Session %s retains unresponsive worker ownership; stale-worker reconciliation will quarantine it, and manual provider-process teardown is required before ownership can be cleared", id)
	}
	if current.ProviderRef == "" {
		return current, fmt.Errorf("Session %s has no saved provider reference and cannot be resumed; create a new Session", id)
	}
	if err := acp.ValidateProviderRefValue("resume Session", current.ProviderRef); err != nil {
		return current, err
	}
	if !current.CanResume() {
		if current.Status == session.StatusStarting {
			return current, &worker.NotReadyError{SessionID: current.ID}
		}
		return current, fmt.Errorf("Session %s is %s and is not in an ownerless terminal state that can Resume", id, current.Status)
	}
	if !allowSharedCheckout {
		allowSharedCheckout = metadataBool(current.MetadataJSON, "allow_shared_checkout")
	}
	if err := s.checkCheckout(ctx, session.CreateParams{ID: current.ID, RepoPath: current.RepoPath, WorktreePath: current.WorktreePath}, false, allowSharedCheckout, current.ID); err != nil {
		return session.Session{}, err
	}
	starting := current.WorkerState()
	starting.Status = session.StatusStarting
	transitioned, swapped, err := s.repository.CompareAndSwapSessionWorker(ctx, id, current.WorkerState(), starting, "")
	if err != nil {
		return session.Session{}, err
	}
	if !swapped {
		return transitioned, fmt.Errorf("Session %s lifecycle changed while Resume was starting; retry against its current state", id)
	}
	if transitioned.WorkerState() != starting {
		return transitioned, fmt.Errorf("Session %s lifecycle changed after Resume admission; retry against its current state", id)
	}
	current = transitioned
	spawned, err := s.runtime.Spawn(ctx, id)
	if err != nil {
		return s.sessionAfterSpawnFailure(current), err
	}
	current.WorkerPID = spawned.PID
	current.WorkerSocket = spawned.Socket
	current.Status = session.StatusStarting
	return current, nil
}

func (s *SessionService) Stop(ctx context.Context, id string) error {
	for {
		current, err := s.repository.GetSession(ctx, id)
		if err != nil {
			return err
		}
		if current.WorkerSocket == "" {
			if current.WorkerPID != 0 {
				return fmt.Errorf("Session %s retains unresponsive worker ownership; stale-worker reconciliation will quarantine it, and manual provider-process teardown is required before ownership can be cleared", id)
			}
			if current.Status == session.StatusStopped {
				return nil
			}
			stopped := current.WorkerState()
			stopped.Status = session.StatusStopped
			_, swapped, err := s.repository.CompareAndSwapSessionWorker(ctx, id, current.WorkerState(), stopped, "")
			if err != nil {
				return err
			}
			if swapped {
				return nil
			}
			// A worker may have claimed the ownerless starting row between our
			// read and CAS. Reload and stop that exact owner instead of reporting
			// success while provider startup continues.
			continue
		}
		if _, err := s.runtime.Send(ctx, current.WorkerSocket, worker.Command{Type: "stop"}); err != nil {
			refreshed, loadErr := s.repository.GetSession(ctx, id)
			if loadErr != nil {
				return loadErr
			}
			if refreshed.WorkerState() != current.WorkerState() {
				if refreshed.WorkerPID == 0 && refreshed.WorkerSocket == "" && refreshed.Status == session.StatusStopped {
					return nil
				}
				continue
			}
			if current.Status == session.StatusFailed {
				return fmt.Errorf("Session %s retains quarantined worker ownership because provider teardown was not proven; manual provider-process teardown is required before clearing ownership", id)
			}
			return err
		}
		break
	}
	// Runtime Port teardown is bounded below the worker's shutdown budget; the
	// service waits one outer budget so a successful Stop never reports timeout
	// while the worker is still proving provider-process termination.
	deadline := time.Now().Add(7 * time.Second)
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

func requireProviderReady(current session.Session) error {
	if current.Status == session.StatusStarting {
		return &worker.NotReadyError{SessionID: current.ID}
	}
	return nil
}

func startingPermissionSnapshot() PermissionSnapshot {
	return PermissionSnapshot{
		Permissions:    []worker.PendingPermission{},
		DecisionsReady: false,
		BlockedCode:    "session_starting",
		BlockedMessage: "The provider connection is still starting, so permission decisions are not available yet.",
	}
}

func (s *SessionService) requireCurrentWorkerProtocol(ctx context.Context, current session.Session, operation string) error {
	return s.requireCurrentWorkerProtocolAt(ctx, current, current.WorkerSocket, operation)
}

func (s *SessionService) requireCurrentWorkerProtocolAt(ctx context.Context, current session.Session, socket, operation string) error {
	response, err := s.runtime.Send(ctx, socket, worker.Command{Type: "ping", SessionID: current.ID})
	if err != nil {
		return err
	}
	if response.Type == "pong" && response.SessionID == current.ID && response.ProtocolVersion >= worker.ProtocolVersion && workerHasCapability(response, worker.CapabilitySessionChannel) {
		return nil
	}
	return &WorkerUpgradeRequiredError{SessionID: current.ID, Operation: operation, ProtocolVersion: response.ProtocolVersion, RequiredCapability: worker.CapabilitySessionChannel}
}

func workerHasCapability(response worker.Message, capability string) bool {
	for _, current := range response.Capabilities {
		if current == capability {
			return true
		}
	}
	return false
}

func metadataBool(encoded json.RawMessage, key string) bool {
	var metadata map[string]any
	if json.Unmarshal(encoded, &metadata) != nil {
		return false
	}
	switch value := metadata[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	default:
		return false
	}
}

func (s *SessionService) Replay(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) (uint64, error) {
	if consume == nil {
		return cursor, fmt.Errorf("Session replay consumer is required")
	}
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return cursor, err
	}
	available, err := s.replayPathAvailable(current, cursor)
	if err != nil || !available {
		return cursor, err
	}
	path := s.logPath(current)
	reader := sessionlog.NewReplayCursor(path, cursor, 0)
	defer reader.Close()
	delivered := cursor
	for {
		line, ok, err := reader.Next(ctx)
		if err != nil {
			return delivered, err
		}
		if !ok {
			unread, unreadErr := reader.HasUnreadBytes()
			if unreadErr != nil {
				return delivered, unreadErr
			}
			if unread && !s.sessionReplayWriterLive(ctx, current) {
				if err := reader.SetStrictEOF(true); err != nil {
					return delivered, err
				}
				continue
			}
			return delivered, nil
		}
		if err := consume(line); err != nil {
			return delivered, err
		}
		delivered = line.Seq
	}
}

// FreshReplayCursor returns the bounded initial viewer window without reading
// provider history or materializing the retained channel. An explicit saved
// nonzero cursor bypasses this policy and resumes exactly.
func (s *SessionService) FreshReplayCursor(ctx context.Context, id string, maxRecords int) (uint64, bool, error) {
	if maxRecords <= 0 {
		return 0, false, fmt.Errorf("tail replay record limit must be positive")
	}
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return 0, false, err
	}
	available, err := s.replayPathAvailable(current, 0)
	if err != nil || !available {
		return 0, false, err
	}
	return sessionlog.TailCursor(s.logPath(current), maxRecords)
}

func (s *SessionService) Follow(ctx context.Context, id string, cursor uint64, consume func(sessionlog.Line) error) error {
	delivered := cursor
	for {
		current, err := s.repository.GetSession(ctx, id)
		if err != nil {
			return err
		}
		available, replayErr := s.replayPathAvailable(current, delivered)
		if replayErr != nil {
			return replayErr
		}
		if !available {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}
		if current.WorkerSocket == "" {
			_, err := sessionlog.WalkFile(ctx, s.logPath(current), delivered, 0, func(line sessionlog.Line) error {
				if err := consume(line); err != nil {
					return err
				}
				delivered = line.Seq
				return nil
			})
			return err
		}
		socket := current.WorkerSocket
		var consumerErr error
		protocolErr := s.requireCurrentWorkerProtocolAt(ctx, current, socket, "follow Session Channel")
		var upgrade *WorkerUpgradeRequiredError
		if errors.As(protocolErr, &upgrade) {
			return protocolErr
		}
		if protocolErr != nil {
			err = protocolErr
		} else {
			err = s.runtime.Subscribe(ctx, socket, delivered, func(message worker.Message) error {
				if message.Type != "line" || message.Seq <= delivered {
					return nil
				}
				line := sessionlog.Line{Seq: message.Seq, Timestamp: message.Timestamp, RequestID: message.RequestID, Type: message.LineType, Data: message.Data}
				if consumeErr := consume(line); consumeErr != nil {
					consumerErr = consumeErr
					return consumeErr
				}
				delivered = message.Seq
				return nil
			})
		}
		if consumerErr != nil {
			return consumerErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A live subscription may end just before worker teardown clears its
		// socket from SQLite (or because the bounded hub shed a slow viewer).
		// Replay the suffix first so a terminal startup frame is never lost in
		// that ownership window. Replay uses a live-safe cursor: a concurrent
		// writer's unterminated syscall suffix is withheld until ownership ends,
		// rather than being misreported as durable corruption.
		replayed, replayErr := s.Replay(ctx, id, delivered, consume)
		delivered = replayed
		if replayErr != nil {
			return replayErr
		}
		refreshed, refreshErr := s.repository.GetSession(ctx, id)
		if refreshErr != nil {
			return refreshErr
		}
		if refreshed.WorkerSocket == "" || refreshed.WorkerSocket != socket || err == nil {
			// Ownership changed after the live subscription boundary. Continue
			// from the last applied sequence so terminal startup frames are
			// replayed without gaps or duplicates.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}
		// A still-responsive worker returning a classified stream error is a
		// protocol failure rather than a teardown race; surface it instead of
		// retrying forever. If ping also fails, let the worker row settle and
		// retry from the last durable cursor.
		pingContext, cancelPing := context.WithTimeout(ctx, 250*time.Millisecond)
		pingErr := s.requireCurrentWorkerProtocolAt(pingContext, current, socket, "verify Session Channel after stream failure")
		cancelPing()
		if pingErr == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *SessionService) replayPathAvailable(current session.Session, cursor uint64) (bool, error) {
	_, err := os.Stat(s.logPath(current))
	if err == nil {
		return true, nil
	}
	if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat Session Channel replay: %w", err)
	}
	if cursor == 0 && current.Status == session.StatusStarting && current.WorkerPID == 0 && current.WorkerSocket == "" {
		return false, nil
	}
	return false, &ReplayUnavailableError{}
}

func (s *SessionService) logPath(current session.Session) string {
	return sessionlog.ChannelPath(s.home.Logs, current.ID)
}
