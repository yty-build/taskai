package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/native"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

var (
	ErrAlreadyRunning       = errors.New("Session worker is already running")
	errPendingPermissionCap = errors.New("pending permission limit reached")
)

const maxPendingPermissions = 128

type Repository interface {
	GetSession(context.Context, string) (session.Session, error)
	UpdateSessionStatus(context.Context, string, session.Status) (session.Session, error)
	UpdateSessionRuntimeIDs(context.Context, string, string, string, string) (session.Session, error)
	UpdateSessionWorker(context.Context, string, int, string, string, session.Status) (session.Session, error)
	ClearSessionWorker(context.Context, string, session.Status) (session.Session, error)
}

type ACPClient interface {
	Start(context.Context, acp.StartSpec) (acp.SessionInfo, error)
	Resume(context.Context, acp.StartSpec) (acp.SessionInfo, error)
	Prompt(context.Context, acp.PromptSpec, func(acp.Event) error) (acp.PromptReceipt, error)
	Cancel(context.Context, acp.Target) error
	Inspect(context.Context, acp.Target) (acp.SessionInfo, error)
}

type refreshableACPClient interface {
	Refresh(context.Context, acp.StartSpec) (acp.SessionInfo, error)
}

type configurableACPClient interface {
	Configure(context.Context, acp.StartSpec) (acp.SessionInfo, error)
}

type Config struct {
	Session                     session.Session
	Profile                     agents.Profile
	Repository                  Repository
	ACP                         ACPClient
	WorkersDirectory            string
	LogsDirectory               string
	PID                         int
	RefreshTimeout              time.Duration
	ReconcileProviderTranscript func(context.Context, agents.Profile, string, string) error
}

type queuedPrompt struct {
	RequestID      string
	Message        string
	DisplayMessage string
}

type pendingPermission struct {
	PendingPermission
	bridgeID string
	byID     map[string]PermissionOption
}

type Worker struct {
	config Config
	target acp.Target

	mu                     sync.Mutex
	status                 session.Status
	acpSessionID           string
	nativeSessionID        string
	providerConversationID string
	providerOptions        session.ProviderOptions
	queue                  []queuedPrompt
	active                 *queuedPrompt
	activeCancel           context.CancelFunc
	activeRuntimeObserved  bool
	activeCancelRequested  bool
	activeTerminalClaimed  bool
	nativeOwnerToken       string
	nativeOwnerGeneration  string
	nativeOwnerReserved    bool
	nativeOwnerStale       bool
	nativeReconcilePending bool
	nativeOwnershipBlocked bool
	nativeOwnershipReason  string
	acpNeedsRefresh        bool
	acpFreshnessEpoch      uint64
	acpConnected           bool
	transportError         string
	seenRequestIDs         map[string]struct{}
	permissions            map[string]*pendingPermission
	wake                   chan struct{}
	stop                   context.CancelFunc
	runContext             context.Context

	publishMu sync.Mutex
	log       *sessionlog.Log
	hub       *hub
	listener  net.Listener

	handlers sync.WaitGroup
}

func New(config Config) (*Worker, error) {
	if config.Repository == nil || config.ACP == nil {
		return nil, fmt.Errorf("worker repository and ACP client are required")
	}
	if config.Session.ID == "" || filepath.Base(config.Session.ID) != config.Session.ID {
		return nil, fmt.Errorf("invalid Session ID %q", config.Session.ID)
	}
	if config.WorkersDirectory == "" || config.LogsDirectory == "" {
		return nil, fmt.Errorf("worker and log directories are required")
	}
	if config.PID == 0 {
		config.PID = os.Getpid()
	}
	if config.ReconcileProviderTranscript == nil {
		config.ReconcileProviderTranscript = func(ctx context.Context, profile agents.Profile, conversationID, workingDirectory string) error {
			_, err := native.ReadProviderTranscript(ctx, profile, conversationID, workingDirectory)
			return err
		}
	}
	workingDirectory := config.Session.WorktreePath
	if workingDirectory == "" {
		workingDirectory = config.Session.RepoPath
	}
	permission, err := permissionFromMetadata(config.Session.MetadataJSON)
	if err != nil {
		return nil, err
	}
	providerOptions, err := session.ParseProviderOptions(config.Session.MetadataJSON)
	if err != nil {
		return nil, err
	}
	target := acp.Target{
		ConsoleSessionID: config.Session.ID, Profile: config.Profile,
		WorkingDirectory: workingDirectory, Permission: permission,
	}
	if workingDirectory == "" {
		return nil, fmt.Errorf("Session %s has no repo/worktree path", config.Session.ID)
	}
	return &Worker{
		config: config, target: target, status: config.Session.Status, acpSessionID: config.Session.ACPSessionID,
		nativeSessionID:        config.Session.NativeSessionID,
		providerConversationID: config.Session.ProviderConversationID,
		providerOptions:        providerOptions,
		seenRequestIDs:         make(map[string]struct{}), permissions: make(map[string]*pendingPermission), wake: make(chan struct{}, 1),
	}, nil
}

func (w *Worker) SocketPath() string {
	return filepath.Join(w.config.WorkersDirectory, w.config.Session.ID+".sock")
}

func (w *Worker) MetadataPath() string {
	return filepath.Join(w.config.WorkersDirectory, w.config.Session.ID+".json")
}

func (w *Worker) LockPath() string {
	return filepath.Join(w.config.WorkersDirectory, w.config.Session.ID+".lock")
}

// NativeOwnerPath is a private runtime marker, not a persistent domain
// entity. It survives a worker restart so native viewers can reattach to the
// same provider process and a second native process cannot be started.
func (w *Worker) NativeOwnerPath() string {
	return filepath.Join(w.config.WorkersDirectory, w.config.Session.ID+".native-owner.json")
}

func (w *Worker) LogPath() string {
	if w.config.Session.LogPath != "" {
		return w.config.Session.LogPath
	}
	return filepath.Join(w.config.LogsDirectory, w.config.Session.ID+".jsonl")
}

func (w *Worker) Run(ctx context.Context) error {
	if err := os.MkdirAll(w.config.WorkersDirectory, 0o700); err != nil {
		return fmt.Errorf("create worker directory: %w", err)
	}
	lock, err := acquireProcessLock(w.LockPath())
	if err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	if err := w.restoreNativeOwnership(); err != nil {
		return err
	}

	if err := os.Remove(w.SocketPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale worker socket: %w", err)
	}
	listener, err := net.Listen("unix", w.SocketPath())
	if err != nil {
		return fmt.Errorf("listen on worker socket %q: %w", w.SocketPath(), err)
	}
	w.listener = listener
	defer func() {
		_ = listener.Close()
		_ = os.Remove(w.SocketPath())
		_ = os.Remove(w.MetadataPath())
	}()
	if err := os.Chmod(w.SocketPath(), 0o600); err != nil {
		return fmt.Errorf("secure worker socket: %w", err)
	}

	history, err := sessionlog.Open(w.LogPath())
	if err != nil {
		return err
	}
	w.log = history
	defer history.Close()
	w.hub = newHub(history.LastSequence())
	if _, err := w.publish("", "worker_starting", map[string]any{"pid": w.config.PID}); err != nil {
		return err
	}
	w.mu.Lock()
	restoredOwner := w.nativeOwnerToken != ""
	restoredReservation := w.nativeOwnerReserved
	restoredRefresh := w.nativeReconcilePending
	w.mu.Unlock()
	if restoredOwner || restoredRefresh {
		state := nativeMarkerOwned
		if restoredReservation {
			state = nativeMarkerReserved
		} else if restoredRefresh {
			state = nativeMarkerRefreshPending
		}
		if _, err := w.publish("", "native_ownership_restored", map[string]string{"state": state}); err != nil {
			return err
		}
	}
	var info acp.SessionInfo
	if restoredOwner {
		// Starting/resuming ACP can mutate provider state. A restored native
		// owner or reservation therefore keeps ACP completely unopened until
		// its exact owner releases the durable marker.
		info, err = w.persistedRuntimeInfo()
	} else {
		info, err = w.connectACP(ctx)
		if err == nil {
			err = w.persistRuntimeInfo(ctx, info)
		}
		if err == nil {
			var current session.Session
			current, err = w.currentIdentity(ctx)
			if err == nil {
				err = w.configureProviderOptions(ctx, current)
			}
		}
		if err == nil {
			w.mu.Lock()
			w.acpConnected = true
			if !restoredRefresh {
				w.acpNeedsRefresh = false
			}
			w.mu.Unlock()
		}
	}
	if err != nil {
		failure := map[string]string{"message": err.Error()}
		var configurationError *acp.ProviderConfigurationError
		if errors.As(err, &configurationError) {
			failure["source"] = "provider"
			failure["code"] = "configuration_failed"
		}
		_, _ = w.publish("", "worker_error", failure)
		_, _ = w.config.Repository.UpdateSessionStatus(context.Background(), w.config.Session.ID, session.StatusFailed)
		return err
	}
	if _, err := w.config.Repository.UpdateSessionWorker(ctx, w.config.Session.ID, w.config.PID, w.SocketPath(), w.LogPath(), session.StatusWaiting); err != nil {
		return err
	}
	w.setLocalStatus(session.StatusWaiting)
	if err := w.writeMetadata(info); err != nil {
		return err
	}
	if _, err := w.publish("", "worker_started", map[string]any{"pid": w.config.PID, "socket": w.SocketPath()}); err != nil {
		return err
	}

	runContext, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.stop = cancel
	w.runContext = runContext
	w.mu.Unlock()
	defer cancel()
	if restoredRefresh {
		// Reconciliation updates viewer history, but the durable refresh marker
		// remains until the first exact ACP rehydration succeeds.
		w.reconcileProviderTranscriptAsync()
	}

	processorDone := make(chan struct{})
	go func() {
		defer close(processorDone)
		w.processPrompts(runContext)
	}()
	acceptDone := make(chan error, 1)
	go func() { acceptDone <- w.accept(runContext) }()

	var runErr error
	acceptJoined := false
	select {
	case <-runContext.Done():
	case err := <-acceptDone:
		acceptJoined = true
		if err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = err
		}
		cancel()
	}
	_, _ = w.publish("", "worker_stopping", map[string]any{})
	_ = listener.Close()
	if !acceptJoined {
		if err := <-acceptDone; err != nil && !errors.Is(err, net.ErrClosed) && runErr == nil {
			runErr = err
		}
	}
	w.cancelActiveTurn()
	<-processorDone
	w.abandonPermissions("Session worker stopped")
	// No connection handler may still enter Cancel/permission/runtime methods
	// once the ACP bridge begins closing. The accept loop is already joined, so
	// this is also the no-new-Add barrier for handler shutdown.
	w.handlers.Wait()
	if closer, ok := w.config.ACP.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close ACP runtime: %w", err)
		}
	}
	w.hub.close()
	if _, err := w.config.Repository.ClearSessionWorker(context.Background(), w.config.Session.ID, session.StatusStopped); err != nil && runErr == nil {
		runErr = err
	}
	w.setLocalStatus(session.StatusStopped)
	return runErr
}

func (w *Worker) connectACP(ctx context.Context) (acp.SessionInfo, error) {
	spec := w.startSpec(w.config.Session)
	if spec.ResumeACPSessionID != "" {
		info, err := w.config.ACP.Resume(ctx, spec)
		if err != nil {
			return acp.SessionInfo{}, err
		}
		if err := acp.ValidateSessionIdentity("worker resume", info, spec.ResumeACPSessionID, spec.ExpectedNativeSessionID, spec.ExpectedProviderConversationID, spec.ProviderStoreVerified); err != nil {
			return acp.SessionInfo{}, err
		}
		return info, nil
	}
	if strings.TrimSpace(spec.ExpectedProviderConversationID) != "" {
		return acp.SessionInfo{}, fmt.Errorf("Session %s has canonical provider conversation %q but no ACP Session ID; refusing to start a replacement conversation", w.config.Session.ID, spec.ExpectedProviderConversationID)
	}
	return w.config.ACP.Start(ctx, spec)
}

func (w *Worker) startSpec(current session.Session) acp.StartSpec {
	return acp.StartSpec{
		Target: w.target, ResumeACPSessionID: current.ACPSessionID,
		ExpectedProviderConversationID: current.ProviderConversationID,
		ExpectedNativeSessionID:        current.NativeSessionID,
		ProviderStoreVerified:          providerStoreVerified(current),
		Model:                          w.providerOptions.Model,
		ReasoningEffort:                w.providerOptions.ReasoningEffort,
	}
}

func (w *Worker) configureProviderOptions(ctx context.Context, current session.Session) error {
	if w.providerOptions.Model == "" && w.providerOptions.ReasoningEffort == "" {
		return nil
	}
	configurator, ok := w.config.ACP.(configurableACPClient)
	if !ok {
		return &acp.ProviderConfigurationError{}
	}
	info, err := configurator.Configure(ctx, w.startSpec(current))
	if err != nil {
		return err
	}
	if err := validateObservedIdentity("provider option configuration", info, current); err != nil {
		return err
	}
	if err := w.persistRuntimeInfo(ctx, info); err != nil {
		return err
	}
	return w.writeMetadata(info)
}

func (w *Worker) persistedRuntimeInfo() (acp.SessionInfo, error) {
	if strings.TrimSpace(w.config.Session.ACPSessionID) == "" {
		return acp.SessionInfo{}, fmt.Errorf("Session %s has durable native ownership but no persisted ACP Session ID; refusing to open ACP or discard native ownership", w.config.Session.ID)
	}
	return acp.SessionInfo{
		ACPXRecordID:    w.config.Session.ACPXRecordID,
		ACPSessionID:    w.config.Session.ACPSessionID,
		NativeSessionID: w.config.Session.NativeSessionID,
	}, nil
}

func (w *Worker) accept(ctx context.Context) error {
	for {
		connection, err := w.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept worker connection: %w", err)
		}
		w.handlers.Add(1)
		go func() {
			defer w.handlers.Done()
			defer connection.Close()
			w.handleConnection(ctx, connection)
		}()
	}
}

func (w *Worker) handleConnection(ctx context.Context, connection net.Conn) {
	stopCloseOnShutdown := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCloseOnShutdown()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	if !scanner.Scan() {
		return
	}
	var command Command
	if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
		writeMessage(connection, Message{Type: "error", Message: "invalid command JSON: " + err.Error()})
		return
	}
	switch command.Type {
	case "ping":
		writeMessage(connection, Message{
			Type: "pong", ProtocolVersion: ProtocolVersion,
			Capabilities: []string{CapabilityExclusiveNativeOwner},
		})
	case "status":
		writeMessage(connection, w.statusMessage())
	case "prompt":
		state, err := w.enqueue(command.RequestID, command.Message, command.DisplayMessage)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "accepted", RequestID: command.RequestID, State: state})
	case "cancel":
		if err := w.cancel(ctx, command.RequestID); err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "cancelled", RequestID: command.RequestID})
	case "permission_decision":
		if err := w.decidePermission(ctx, command.PermissionID, command.OptionID, command.Cancelled); err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "permission_decided", RequestID: command.RequestID})
	case "subscribe":
		w.subscribe(ctx, connection, command.From)
	case "tail":
		w.tail(connection, command.From)
	case "native_acquire":
		state, generation, err := w.acquireNative(command.OwnerToken, command.NativeOwnerRole, command.NativeGeneration)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_acquired", State: state, NativeOwned: true, NativeGeneration: generation})
	case "native_reservation_status":
		state, generation, err := w.nativeReservationStatus(command.OwnerToken)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_reservation_status", State: state, NativeOwned: state != "released", NativeGeneration: generation})
	case "native_recover_reservation":
		state, err := w.recoverNativeReservation(command.OwnerToken, command.NativeGeneration)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_reservation_recovered", State: state})
	case "native_release":
		state, err := w.releaseNative(command.OwnerToken, command.NativeGeneration, command.NativeActivity)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_released", State: state})
	case "stop":
		if w.nativeOwned() {
			writeMessage(connection, Message{Type: "error", Message: "native harness is still running; exit the native provider process before stopping the worker so lifecycle tracking is preserved"})
			return
		}
		writeMessage(connection, Message{Type: "stopping"})
		w.requestStop()
	default:
		writeMessage(connection, Message{Type: "error", Message: fmt.Sprintf("unknown command type %q", command.Type)})
	}
}

func (w *Worker) enqueue(requestID, message, displayMessage string) (string, error) {
	if strings.TrimSpace(requestID) == "" {
		return "", fmt.Errorf("prompt request_id is required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("prompt message is required")
	}
	w.mu.Lock()
	if w.transportError != "" {
		message := w.transportError
		w.mu.Unlock()
		return "", fmt.Errorf("ACP transport is unavailable for Session %s: %s; resume the stopped Session to retry exact continuity", w.config.Session.ID, message)
	}
	if w.nativeOwnershipBlocked {
		reason := w.nativeOwnershipReason
		w.mu.Unlock()
		return "", fmt.Errorf("Session %s native harness ownership is uncertain because its durable marker %s; ACP prompt was not accepted. Recover the marker before retrying", w.config.Session.ID, reason)
	}
	if w.nativeOwnerToken != "" {
		phase := "is running"
		if w.nativeOwnerReserved {
			phase = "has an active launch reservation"
		}
		w.mu.Unlock()
		return "", fmt.Errorf("Session %s native harness %s; ACP prompt was not accepted. Exit or close that native provider launch, then retry", w.config.Session.ID, phase)
	}
	if _, exists := w.seenRequestIDs[requestID]; exists {
		w.mu.Unlock()
		return "", fmt.Errorf("request_id %q is already active or queued", requestID)
	}
	state := "ready"
	if w.active != nil || len(w.queue) > 0 {
		state = "queued"
	}
	if strings.TrimSpace(displayMessage) == "" {
		displayMessage = message
	}
	// Serialize durable admission with the queue mutation. Concurrent callers
	// must see the same order in replay/UI that the FIFO will execute, and a
	// request whose journal admission fails must never remain runnable.
	if _, err := w.publish(requestID, "prompt_submitted", map[string]string{"display_message": displayMessage}); err != nil {
		w.mu.Unlock()
		return "", err
	}
	lifecycle := "prompt_ready"
	if state == "queued" {
		lifecycle = "prompt_queued"
	}
	if _, err := w.publish(requestID, lifecycle, map[string]string{"state": state}); err != nil {
		w.mu.Unlock()
		return "", err
	}
	w.queue = append(w.queue, queuedPrompt{RequestID: requestID, Message: message, DisplayMessage: displayMessage})
	w.seenRequestIDs[requestID] = struct{}{}
	w.mu.Unlock()
	w.signalWake()
	return state, nil
}

func (w *Worker) processPrompts(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
		for {
			prompt, promptContext, ok := w.nextPrompt(ctx)
			if !ok {
				break
			}
			w.executePrompt(promptContext, prompt)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (w *Worker) nextPrompt(ctx context.Context) (queuedPrompt, context.Context, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.transportError != "" || w.active != nil || len(w.queue) == 0 {
		return queuedPrompt{}, nil, false
	}
	prompt := w.queue[0]
	w.queue = w.queue[1:]
	w.active = &prompt
	w.activeRuntimeObserved = false
	w.activeCancelRequested = false
	w.activeTerminalClaimed = false
	promptContext, cancel := context.WithCancel(ctx)
	w.activeCancel = cancel
	return prompt, promptContext, true
}

func (w *Worker) executePrompt(ctx context.Context, prompt queuedPrompt) {
	if err := w.prepareACPDispatch(ctx, prompt.RequestID); err != nil {
		cancelled, claimed := w.claimPromptTerminal(prompt.RequestID, errors.Is(ctx.Err(), context.Canceled))
		if !claimed {
			return
		}
		if cancelled {
			w.finishClaimedCancelledPrompt(prompt.RequestID)
			return
		}
		var providerTurnErr *acp.ProviderTurnError
		if errors.As(err, &providerTurnErr) {
			_, _ = w.publish(prompt.RequestID, "turn_error", turnFailureData(err))
			_ = w.updateStatus(session.StatusWaiting)
			w.finishPrompt(prompt.RequestID)
			return
		}
		// Failure to prove exact continuity is a transport-integrity failure,
		// not an ordinary provider rejection. Expose a bounded correlated
		// terminal message, quarantine the handle, and reject the remaining
		// FIFO without dispatching it.
		_, _ = w.publish(prompt.RequestID, "turn_error", map[string]string{
			"message": "prompt was not sent because exact provider conversation continuity could not be proven",
		})
		w.quarantineTransport(fmt.Errorf("pre-prompt exact continuity failed: %w", err))
		w.finishPrompt(prompt.RequestID)
		return
	}
	_ = w.updateStatus(session.StatusWorking)
	receipt, err := w.config.ACP.Prompt(ctx, acp.PromptSpec{Target: w.target, RequestID: prompt.RequestID, Message: prompt.Message}, func(event acp.Event) error {
		w.mu.Lock()
		if w.active != nil && w.active.RequestID == prompt.RequestID {
			w.activeRuntimeObserved = true
		}
		w.mu.Unlock()
		if event.Type == "session_request_permission" {
			pending, permissionErr := parsePendingPermission(prompt.RequestID, event)
			if permissionErr != nil {
				return permissionErr
			}
			if permissionErr := w.registerPermission(pending); permissionErr != nil {
				return permissionErr
			}
			if _, permissionErr := w.publish(prompt.RequestID, "permission_requested", pending.PendingPermission); permissionErr != nil {
				w.removePermission(pending.ID)
				return permissionErr
			}
		}
		_, publishErr := w.publish(prompt.RequestID, "acp", event)
		return publishErr
	})
	cancelled, claimed := w.claimPromptTerminal(prompt.RequestID, ctx.Err() != nil)
	if !claimed {
		return
	}
	if cancelled {
		w.finishClaimedCancelledPrompt(prompt.RequestID)
		return
	}
	if err != nil {
		var providerTurnErr *acp.ProviderTurnError
		if errors.As(err, &providerTurnErr) {
			_, _ = w.publish(prompt.RequestID, "turn_error", turnFailureData(err))
			_ = w.updateStatus(session.StatusWaiting)
		} else {
			_, _ = w.publish(prompt.RequestID, "turn_error", map[string]string{"message": err.Error()})
			_ = w.updateStatus(session.StatusFailed)
			w.recoverAfterPromptError(ctx, prompt.RequestID, err)
		}
	} else {
		_, _ = w.publish(prompt.RequestID, "turn_completed", receipt)
		_ = w.updateStatus(session.StatusWaiting)
		w.reconcileAfterTurn(ctx, prompt.RequestID)
	}
	w.finishPrompt(prompt.RequestID)
}

func turnFailureData(err error) any {
	var providerTurnErr *acp.ProviderTurnError
	if errors.As(err, &providerTurnErr) {
		return safeProviderTurnFailure(providerTurnErr)
	}
	return map[string]string{"message": err.Error()}
}

func (w *Worker) claimPromptTerminal(requestID string, cancelled bool) (bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || w.active.RequestID != requestID || w.activeTerminalClaimed {
		return false, false
	}
	w.activeTerminalClaimed = true
	return cancelled || w.activeCancelRequested, true
}

func (w *Worker) finishClaimedCancelledPrompt(requestID string) {
	_, _ = w.publish(requestID, "turn_completed", acp.PromptReceipt{RequestID: requestID, StopReason: "cancelled"})
	_ = w.updateStatus(session.StatusWaiting)
	w.finishPrompt(requestID)
}

func safeProviderTurnFailure(providerErr *acp.ProviderTurnError) map[string]any {
	code := providerTurnFailureCode(providerErr.DetailCode, providerErr.Code)
	message := map[string]string{
		"busy":           "The provider is busy with this conversation. Wait for the current provider turn to settle, then try again.",
		"conflict":       "The provider reported a conversation conflict. Wait for the other provider turn to settle, then try again.",
		"rate_limited":   "The provider rate-limited this turn. Try again shortly.",
		"rejected":       "The provider rejected this turn.",
		"provider_error": "The provider could not complete this turn.",
	}[code]
	return map[string]any{
		"source": "provider", "code": code, "retryable": providerErr.Retryable, "message": message,
	}
}

func providerTurnFailureCode(values ...string) string {
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
		switch {
		case value == "busy", value == "provider_busy", value == "conversation_busy", value == "turn_busy":
			return "busy"
		case value == "conflict", value == "provider_conflict", value == "conversation_conflict", value == "concurrent_turn":
			return "conflict"
		case value == "rate_limited", value == "rate_limit", value == "too_many_requests", value == "throttled":
			return "rate_limited"
		case value == "rejected", value == "provider_rejected", value == "refused", value == "denied":
			return "rejected"
		}
	}
	return "provider_error"
}

var errACPFreshnessChanged = errors.New("ACP conversation freshness changed before dispatch")

const maxACPDispatchPreparationAttempts = 3

func (w *Worker) prepareACPDispatch(ctx context.Context, requestID string) error {
	for range maxACPDispatchPreparationAttempts {
		epoch, err := w.rehydrateACPForPrompt(ctx, requestID)
		if errors.Is(err, errACPFreshnessChanged) {
			continue
		}
		if err != nil {
			return err
		}
		if err := w.publishACPDispatch(requestID, epoch); errors.Is(err, errACPFreshnessChanged) {
			continue
		} else if err != nil {
			return fmt.Errorf("record ACP dispatch activation: %w", err)
		}
		return nil
	}
	return fmt.Errorf("exact ACP conversation freshness changed repeatedly before dispatch; retry after native activity settles")
}

func (w *Worker) publishACPDispatch(requestID string, rehydratedEpoch uint64) error {
	// The dispatch boundary is serialized with native acquisition. Under the
	// exclusive contract an accepted ACP request and a native owner can never
	// coexist; either admission or activation wins the same lock and the other
	// surface is rejected before provider work starts.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || w.active.RequestID != requestID {
		return context.Canceled
	}
	if w.activeCancelRequested || w.activeTerminalClaimed {
		return context.Canceled
	}
	if w.acpFreshnessEpoch != rehydratedEpoch || w.acpNeedsRefresh {
		return errACPFreshnessChanged
	}
	if w.nativeOwnerToken != "" || w.nativeOwnershipBlocked {
		return fmt.Errorf("native harness became active before ACP dispatch; prompt was not sent")
	}
	_, err := w.publish(requestID, "turn_started", map[string]string{"request_id": requestID})
	return err
}

func (w *Worker) recoverAfterPromptError(ctx context.Context, requestID string, promptErr error) {
	current, err := w.currentIdentity(ctx)
	if err == nil {
		var info acp.SessionInfo
		info, err = w.resumeExact(ctx, current)
		if err == nil {
			_, _ = w.publish(requestID, "transport_recovered", map[string]string{
				"acp_session_id": info.ACPSessionID, "after": "prompt_error",
			})
			_ = w.updateStatus(session.StatusWaiting)
			return
		}
	}
	w.quarantineTransport(fmt.Errorf("exact recovery after prompt failure %q: %w", promptErr.Error(), err))
}

func (w *Worker) reconcileAfterTurn(ctx context.Context, requestID string) {
	current, err := w.currentIdentity(ctx)
	if err == nil {
		var info acp.SessionInfo
		info, err = w.config.ACP.Inspect(ctx, w.target)
		if err == nil {
			err = validateObservedIdentity("post-turn inspect", info, current)
		}
		if err == nil {
			err = w.persistRuntimeInfo(ctx, info)
		}
		if err == nil {
			if metadataErr := w.writeMetadata(info); metadataErr != nil {
				_, _ = w.publish(requestID, "metadata_warning", map[string]string{"message": metadataErr.Error()})
			}
			return
		}
	}
	_, _ = w.publish(requestID, "metadata_warning", map[string]string{"message": err.Error()})
	current, loadErr := w.currentIdentity(ctx)
	if loadErr != nil {
		w.quarantineTransport(fmt.Errorf("reload exact Session identity after metadata failure: %w", loadErr))
		return
	}
	info, recoveryErr := w.resumeExact(ctx, current)
	if recoveryErr != nil {
		w.quarantineTransport(fmt.Errorf("exact recovery after metadata failure: %w", recoveryErr))
		return
	}
	_, _ = w.publish(requestID, "metadata_recovered", map[string]string{"acp_session_id": info.ACPSessionID})
}

func (w *Worker) finishPrompt(requestID string) {
	w.mu.Lock()
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.activeCancel = nil
	w.active = nil
	w.activeRuntimeObserved = false
	w.activeCancelRequested = false
	w.activeTerminalClaimed = false
	delete(w.seenRequestIDs, requestID)
	hasNext := len(w.queue) > 0
	w.mu.Unlock()
	w.abandonRequestPermissions(requestID, "turn ended")
	if hasNext {
		w.signalWake()
	}
}

func (w *Worker) cancel(ctx context.Context, requestID string) error {
	w.mu.Lock()
	active := w.active
	if active == nil {
		queueIndex := -1
		if requestID == "" && len(w.queue) > 0 {
			queueIndex = 0
		} else {
			for index := range w.queue {
				if w.queue[index].RequestID == requestID {
					queueIndex = index
					break
				}
			}
		}
		if queueIndex < 0 {
			w.mu.Unlock()
			return fmt.Errorf("no active or accepted turn to cancel")
		}
		prompt := w.queue[queueIndex]
		if _, err := w.publish(prompt.RequestID, "turn_completed", acp.PromptReceipt{RequestID: prompt.RequestID, StopReason: "cancelled"}); err != nil {
			w.mu.Unlock()
			w.quarantineTransport(fmt.Errorf("persist cancellation for accepted prompt %s: %w", prompt.RequestID, err))
			return err
		}
		copy(w.queue[queueIndex:], w.queue[queueIndex+1:])
		w.queue[len(w.queue)-1] = queuedPrompt{}
		w.queue = w.queue[:len(w.queue)-1]
		delete(w.seenRequestIDs, prompt.RequestID)
		hasNext := len(w.queue) > 0
		w.mu.Unlock()
		_ = w.updateStatus(session.StatusWaiting)
		if hasNext {
			w.signalWake()
		}
		return nil
	}
	if requestID != "" && active.RequestID != requestID {
		w.mu.Unlock()
		return fmt.Errorf("active request is %q, not %q", active.RequestID, requestID)
	}
	if w.activeTerminalClaimed {
		w.mu.Unlock()
		return fmt.Errorf("active request %q is already settling", active.RequestID)
	}
	if w.activeCancelRequested {
		w.mu.Unlock()
		return nil
	}
	w.activeCancelRequested = true
	if _, err := w.publish(active.RequestID, "cancel_requested", map[string]string{"request_id": active.RequestID}); err != nil {
		w.activeCancelRequested = false
		w.mu.Unlock()
		return err
	}
	activeCancel := w.activeCancel
	runtimeObserved := w.activeRuntimeObserved
	w.mu.Unlock()
	if activeCancel != nil {
		activeCancel()
	}
	if runtimeObserved {
		_ = w.config.ACP.Cancel(ctx, w.target)
	}
	w.abandonRequestPermissions(active.RequestID, "turn cancelled")
	return nil
}

func (w *Worker) rehydrateACPForPrompt(ctx context.Context, requestID string) (uint64, error) {
	w.mu.Lock()
	observedEpoch := w.acpFreshnessEpoch
	w.mu.Unlock()

	current, err := w.currentIdentity(ctx)
	if err != nil {
		return 0, fmt.Errorf("load canonical Session identity before ACP prompt: %w", err)
	}
	if strings.TrimSpace(current.ACPSessionID) == "" {
		return 0, fmt.Errorf("Session %s has no canonical ACP Session ID to rehydrate before the prompt", current.ID)
	}
	refresher, ok := w.config.ACP.(refreshableACPClient)
	if !ok {
		return 0, fmt.Errorf("ACP transport does not support exact conversation rehydration")
	}

	refreshContext, cancelRefresh := context.WithTimeout(ctx, w.refreshTimeout())
	info, refreshErr := refresher.Refresh(refreshContext, w.startSpec(current))
	cancelRefresh()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if refreshErr == nil {
		refreshErr = validateObservedIdentity("pre-prompt ACP rehydration", info, current)
	}
	if refreshErr == nil {
		refreshErr = w.persistRuntimeInfo(ctx, info)
	}
	if refreshErr == nil {
		refreshErr = w.writeMetadata(info)
	}
	if refreshErr == nil {
		var refreshed session.Session
		refreshed, refreshErr = w.currentIdentity(ctx)
		if refreshErr == nil {
			refreshErr = w.configureProviderOptions(ctx, refreshed)
		}
	}
	mode := "refreshed"
	if refreshErr != nil {
		if _, resumeErr := w.resumeExact(ctx, current); resumeErr != nil {
			// Provider-level busy/conflict results are correlated turn failures,
			// even when they happen while reopening the exact conversation. Keep
			// that typed provenance so presentation can expose only the safe,
			// allowlisted reason instead of leaking the provider's raw detail.
			var refreshProviderErr, resumeProviderErr *acp.ProviderTurnError
			if errors.As(refreshErr, &refreshProviderErr) && errors.As(resumeErr, &resumeProviderErr) {
				return 0, resumeProviderErr
			}
			// A bridge/identity failure on either activation attempt dominates a
			// provider-level rejection on the other. Do not retain an unwrap path
			// to the typed provider error or presentation could misclassify a
			// transport-integrity failure as a benign busy result.
			return 0, fmt.Errorf("rehydrate exact ACP conversation before prompt: refresh failed: %v; exact resume failed: %v", refreshErr, resumeErr)
		}
		mode = "resumed"
	}

	w.mu.Lock()
	if w.acpFreshnessEpoch != observedEpoch {
		w.mu.Unlock()
		return 0, errACPFreshnessChanged
	}
	if err := w.clearNativeRefreshPendingLocked(); err != nil {
		w.blockNativeAfterRefreshCleanupLocked("changed during exact ACP rehydration")
		w.mu.Unlock()
		return 0, fmt.Errorf("clear completed ACP rehydration marker: %w", err)
	}
	w.acpConnected = true
	w.acpNeedsRefresh = false
	w.mu.Unlock()
	_, _ = w.publish(requestID, "acp_rehydrated", map[string]string{"state": mode})
	return observedEpoch, nil
}

func (w *Worker) requireACPFreshnessLocked() {
	w.acpNeedsRefresh = true
	w.acpFreshnessEpoch++
}

func (w *Worker) blockNativeAfterRefreshCleanupLocked(reason string) {
	w.nativeReconcilePending = false
	w.nativeOwnershipBlocked = true
	w.nativeOwnershipReason = reason
	w.requireACPFreshnessLocked()
}

func (w *Worker) reconcileProviderTranscript(ctx context.Context) error {
	current, err := w.currentIdentity(ctx)
	if err != nil {
		return fmt.Errorf("load exact identity for provider transcript reconciliation: %w", err)
	}
	conversationID := strings.TrimSpace(current.ProviderConversationID)
	if conversationID == "" {
		return fmt.Errorf("Session %s has no canonical provider conversation ID for transcript reconciliation", current.ID)
	}
	workingDirectory := current.WorktreePath
	if workingDirectory == "" {
		workingDirectory = current.RepoPath
	}
	if err := w.config.ReconcileProviderTranscript(ctx, w.config.Profile, conversationID, workingDirectory); err != nil {
		return fmt.Errorf("reconcile exact provider transcript: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile exact provider transcript: %w", err)
	}
	if _, err := w.publish("", "provider_transcript_reconciled", map[string]string{
		"source": "provider-store", "state": "reconciled",
	}); err != nil {
		return fmt.Errorf("record exact provider transcript reconciliation: %w", err)
	}
	return nil
}

func (w *Worker) reconcileProviderTranscriptAsync() {
	w.mu.Lock()
	baseContext := w.runContext
	w.mu.Unlock()
	if baseContext == nil {
		baseContext = context.Background()
	}
	w.handlers.Add(1)
	go func() {
		defer w.handlers.Done()
		ctx, cancel := context.WithTimeout(baseContext, w.refreshTimeout())
		defer cancel()
		if err := w.reconcileProviderTranscript(ctx); err != nil {
			_, _ = w.publish("", "provider_transcript_reconcile_warning", map[string]string{"message": err.Error()})
		}
	}()
}

func (w *Worker) refreshTimeout() time.Duration {
	if w.config.RefreshTimeout > 0 {
		return w.config.RefreshTimeout
	}
	return 15 * time.Second
}

func (w *Worker) resumeExact(ctx context.Context, current session.Session) (acp.SessionInfo, error) {
	recoveryContext, cancelRecovery := context.WithTimeout(ctx, w.refreshTimeout())
	defer cancelRecovery()
	spec := w.startSpec(current)
	var lastErr error
	for attempt := 0; attempt < 2 && recoveryContext.Err() == nil; attempt++ {
		info, err := w.config.ACP.Resume(recoveryContext, spec)
		if err == nil {
			err = validateObservedIdentity("exact recovery", info, current)
		}
		if err == nil {
			err = w.persistRuntimeInfo(recoveryContext, info)
		}
		if err == nil {
			err = w.writeMetadata(info)
		}
		if err == nil {
			var refreshed session.Session
			refreshed, err = w.currentIdentity(recoveryContext)
			if err == nil {
				err = w.configureProviderOptions(recoveryContext, refreshed)
			}
		}
		if err == nil {
			w.mu.Lock()
			w.acpConnected = true
			w.mu.Unlock()
			return info, nil
		}
		lastErr = err
	}
	if recoveryContext.Err() != nil {
		return acp.SessionInfo{}, recoveryContext.Err()
	}
	return acp.SessionInfo{}, lastErr
}

func (w *Worker) currentIdentity(ctx context.Context) (session.Session, error) {
	current, err := w.config.Repository.GetSession(ctx, w.config.Session.ID)
	if err != nil {
		return session.Session{}, fmt.Errorf("load current Session identity: %w", err)
	}
	w.mu.Lock()
	w.acpSessionID = current.ACPSessionID
	w.nativeSessionID = current.NativeSessionID
	w.providerConversationID = current.ProviderConversationID
	w.mu.Unlock()
	return current, nil
}

func providerStoreVerified(current session.Session) bool {
	return current.ConversationIDSource == session.ConversationSourceProviderStore && current.ConversationIDVerifiedAt != nil
}

func validateObservedIdentity(operation string, info acp.SessionInfo, current session.Session) error {
	return acp.ValidateSessionIdentity(
		operation, info, current.ACPSessionID, current.NativeSessionID,
		current.ProviderConversationID, providerStoreVerified(current),
	)
}

func (w *Worker) quarantineTransport(cause error) {
	message := strings.TrimSpace(cause.Error())
	if message == "" {
		message = "exact provider conversation continuity could not be proven"
	}
	w.mu.Lock()
	if w.transportError != "" {
		w.mu.Unlock()
		return
	}
	w.transportError = message
	queued := append([]queuedPrompt(nil), w.queue...)
	w.queue = nil
	for _, prompt := range queued {
		delete(w.seenRequestIDs, prompt.RequestID)
	}
	w.mu.Unlock()
	_, _ = w.publish("", "transport_unavailable", map[string]string{"message": message})
	for _, prompt := range queued {
		_, _ = w.publish(prompt.RequestID, "turn_error", map[string]string{
			"message": "prompt was not sent because exact provider conversation continuity could not be recovered",
		})
	}
	_ = w.updateStatus(session.StatusFailed)
	w.requestStop()
}

func (w *Worker) subscribe(ctx context.Context, connection net.Conn, from uint64) {
	id, cutoff, lines, ok := w.hub.add()
	if !ok {
		writeMessage(connection, Message{Type: "error", Message: "worker is stopping"})
		return
	}
	defer w.hub.remove(id)
	replay, err := w.log.ReadAfter(from, cutoff)
	if err != nil {
		writeMessage(connection, Message{Type: "error", Message: err.Error()})
		return
	}
	for _, line := range replay {
		if err := writeMessage(connection, lineMessage(line)); err != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case line, open := <-lines:
			if !open {
				return
			}
			if line.Seq <= from || line.Seq <= cutoff {
				continue
			}
			if err := writeMessage(connection, lineMessage(line)); err != nil {
				return
			}
		}
	}
}

func (w *Worker) tail(connection net.Conn, from uint64) {
	cutoff := w.log.LastSequence()
	lines, err := w.log.ReadAfter(from, cutoff)
	if err != nil {
		writeMessage(connection, Message{Type: "error", Message: err.Error()})
		return
	}
	for _, line := range lines {
		if err := writeMessage(connection, lineMessage(line)); err != nil {
			return
		}
	}
	writeMessage(connection, Message{Type: "end", Seq: cutoff})
}

func (w *Worker) statusMessage() Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	message := Message{
		Type: "status", Status: string(w.status), Queued: len(w.queue),
		NativeOwned: w.nativeOwnerToken != "",
	}
	message.Message = w.transportError
	if w.active != nil {
		message.ActiveRequestID = w.active.RequestID
	}
	for _, pending := range w.permissions {
		copyPermission := pending.PendingPermission
		copyPermission.Options = append([]PermissionOption(nil), pending.Options...)
		message.Permissions = append(message.Permissions, copyPermission)
	}
	sort.Slice(message.Permissions, func(i, j int) bool { return message.Permissions[i].ID < message.Permissions[j].ID })
	return message
}

func (w *Worker) acquireNative(ownerToken, role, generation string) (string, string, error) {
	ownerToken, err := validateNativeOwnerToken(ownerToken)
	if err != nil {
		return "", "", err
	}
	if role != NativeOwnerRoleReservation && role != NativeOwnerRoleProvider && role != NativeOwnerRoleDirect {
		return "", "", fmt.Errorf("unknown or missing native owner role %q for worker protocol %d", role, ProtocolVersion)
	}
	if role == NativeOwnerRoleProvider {
		if err := validateNativeGeneration(generation); err != nil {
			return "", "", fmt.Errorf("activate native provider reservation: %w", err)
		}
	} else if generation != "" {
		return "", "", fmt.Errorf("only a private provider process may supply a native generation")
	}

	w.mu.Lock()
	switch {
	case w.nativeOwnershipBlocked:
		reason := w.nativeOwnershipReason
		w.mu.Unlock()
		return "", "", fmt.Errorf("native harness ownership is unavailable because its durable marker %s; both ACP and native work are unavailable until the marker is recovered explicitly", reason)
	case w.transportError != "":
		message := w.transportError
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider-native attachment is unavailable because exact ACP continuity failed: %s", message)
	case w.nativeOwnerToken == ownerToken:
		existingGeneration := w.nativeOwnerGeneration
		if role != NativeOwnerRoleProvider && w.nativeReuseIsStaleLocked() {
			exitCommand, commandErr := native.TmuxAttachCommand(w.config.Session.ID)
			w.mu.Unlock()
			if commandErr != nil {
				return "", "", fmt.Errorf("Session %s native harness is stale because ACP work was dispatched after that provider process started; return to its existing terminal and exit the provider without sending another prompt, then open Native CLI again for a fresh exact resume", w.config.Session.ID)
			}
			return "", "", fmt.Errorf("Session %s native harness is stale because ACP work was dispatched after that provider process started; return to its original provider terminal and exit without sending another prompt; if it is tmux-hosted, run `%s` to reach it; then open Native CLI again for a fresh exact resume", w.config.Session.ID, exitCommand)
		}
		acpWorkPending := w.active != nil || len(w.queue) > 0
		if acpWorkPending && (role != NativeOwnerRoleProvider || w.nativeOwnerReserved) {
			w.mu.Unlock()
			return "", "", nativeActivationWaitError(w.config.Session.ID)
		}
		if role == NativeOwnerRoleReservation {
			state := "already_acquired"
			if w.nativeOwnerReserved {
				state = "already_reserved"
			}
			w.mu.Unlock()
			return state, existingGeneration, nil
		}
		if role == NativeOwnerRoleDirect {
			if w.nativeOwnerReserved {
				w.mu.Unlock()
				return "", "", fmt.Errorf("Session %s has a tmux native reservation; direct execution cannot activate it", w.config.Session.ID)
			}
			w.mu.Unlock()
			return "already_acquired", existingGeneration, nil
		}
		if !w.nativeOwnerReserved {
			w.mu.Unlock()
			return "already_acquired", existingGeneration, nil
		}
		if generation != existingGeneration {
			w.mu.Unlock()
			return "", "", fmt.Errorf("native reservation generation does not match Session %s; refusing to start a stale provider process", w.config.Session.ID)
		}
		if err := w.persistNativeOwnership(nativeMarkerOwned, ownerToken, existingGeneration); err != nil {
			w.mu.Unlock()
			return "", "", fmt.Errorf("activate native harness ownership: %w", err)
		}
		w.nativeOwnerReserved = false
		w.nativeOwnerStale = false
		w.nativeReconcilePending = false
		if _, err := w.publish("", "native_activated", map[string]string{"state": "activated"}); err != nil {
			if rollbackErr := w.persistNativeOwnership(nativeMarkerReserved, ownerToken, existingGeneration); rollbackErr != nil {
				w.mu.Unlock()
				return "", "", fmt.Errorf("publish native activation: %v; ownership remains fail-closed as active because reservation rollback failed: %w", err, rollbackErr)
			}
			w.nativeOwnerReserved = true
			w.nativeOwnerStale = false
			w.mu.Unlock()
			return "", "", fmt.Errorf("publish native activation: %w; restored the exact pre-launch reservation and did not authorize provider start", err)
		}
		w.requireACPFreshnessLocked()
		w.mu.Unlock()
		return "activated", existingGeneration, nil
	case w.nativeOwnerToken != "":
		w.mu.Unlock()
		return "", "", fmt.Errorf("another native provider process already owns Session %s", w.config.Session.ID)
	default:
		if role == NativeOwnerRoleProvider {
			w.mu.Unlock()
			return "", "", fmt.Errorf("native provider reservation is no longer active for Session %s; refusing to start a stale provider process", w.config.Session.ID)
		}
		if w.active != nil || len(w.queue) > 0 {
			w.mu.Unlock()
			return "", "", nativeActivationWaitError(w.config.Session.ID)
		}
		// A refresh-pending marker is a durable handoff from the last native
		// generation. Validate its exact phase before atomically replacing it;
		// never create a marker-absent crash window.
		previousRefreshPending := w.nativeReconcilePending
		var previousRefreshMarker nativeOwnerMarker
		if w.nativeReconcilePending {
			var markerErr error
			previousRefreshMarker, markerErr = w.loadNativeRefreshPendingMarker()
			if markerErr != nil {
				w.blockNativeAfterRefreshCleanupLocked("changed before native activation")
				w.mu.Unlock()
				return "", "", fmt.Errorf("validate ACP rehydration marker before native activation: %w", markerErr)
			}
		}
		newGeneration, generationErr := newNativeGeneration()
		if generationErr != nil {
			w.mu.Unlock()
			return "", "", generationErr
		}
		markerState := nativeMarkerOwned
		resultState := "acquired"
		if role == NativeOwnerRoleReservation {
			markerState = nativeMarkerReserved
			resultState = "reserved"
		}
		if err := w.persistNativeOwnership(markerState, ownerToken, newGeneration); err != nil {
			w.mu.Unlock()
			return "", "", fmt.Errorf("persist native harness ownership: %w", err)
		}
		w.nativeOwnerToken = ownerToken
		w.nativeOwnerGeneration = newGeneration
		w.nativeOwnerReserved = role == NativeOwnerRoleReservation
		w.nativeOwnerStale = false
		w.nativeReconcilePending = false
		if _, err := w.publish("", "native_acquired", map[string]string{"state": resultState}); err != nil {
			var rollbackErr error
			if previousRefreshPending {
				rollbackErr = w.persistNativeOwnership(nativeMarkerRefreshPending, previousRefreshMarker.OwnerToken, previousRefreshMarker.Generation)
			} else {
				rollbackErr = w.removeNativeOwnership()
			}
			if rollbackErr != nil {
				w.mu.Unlock()
				return "", "", fmt.Errorf("publish native ownership: %v; ownership remains fail-closed because rollback failed: %w", err, rollbackErr)
			}
			w.nativeOwnerToken = ""
			w.nativeOwnerGeneration = ""
			w.nativeOwnerReserved = false
			w.nativeOwnerStale = false
			w.nativeReconcilePending = previousRefreshPending
			w.mu.Unlock()
			return "", "", err
		}
		if role == NativeOwnerRoleDirect {
			w.requireACPFreshnessLocked()
		}
		w.mu.Unlock()
		return resultState, newGeneration, nil
	}
}

func nativeActivationWaitError(sessionID string) error {
	return fmt.Errorf("Session %s has accepted ACP work; wait for the current ACP turn to settle, then open Native CLI again", sessionID)
}

func (w *Worker) nativeReuseIsStaleLocked() bool {
	if w.nativeOwnerToken == "" || w.nativeOwnerReserved {
		return false
	}
	return w.nativeOwnerStale
}

func (w *Worker) nativeReservationStatus(ownerToken string) (string, string, error) {
	ownerToken, err := validateNativeOwnerToken(ownerToken)
	if err != nil {
		return "", "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nativeOwnershipBlocked {
		return "", "", fmt.Errorf("native harness ownership is unavailable because its durable marker %s; both ACP and native work are unavailable until the marker is recovered explicitly", w.nativeOwnershipReason)
	}
	if w.nativeOwnerToken == "" {
		return "released", "", nil
	}
	if w.nativeOwnerToken != ownerToken {
		return "", "", fmt.Errorf("native provider ownership token does not match Session %s", w.config.Session.ID)
	}
	if w.nativeOwnerReserved {
		return "reserved", w.nativeOwnerGeneration, nil
	}
	return "active", w.nativeOwnerGeneration, nil
}

func (w *Worker) releaseNative(ownerToken, generation string, activity bool) (string, error) {
	return w.releaseNativeOwnership(ownerToken, generation, activity, false)
}

func (w *Worker) recoverNativeReservation(ownerToken, generation string) (string, error) {
	// A foreign collision may have written the provider conversation. Recovery
	// therefore requests the same asynchronous transcript reconciliation as a
	// real native turn even though this generation never started our provider.
	return w.releaseNativeOwnership(ownerToken, generation, true, true)
}

func (w *Worker) releaseNativeOwnership(ownerToken, generation string, activity, reservationOnly bool) (string, error) {
	ownerToken, err := validateNativeOwnerToken(ownerToken)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	if w.nativeOwnershipBlocked {
		reason := w.nativeOwnershipReason
		w.mu.Unlock()
		return "", fmt.Errorf("native harness ownership is unavailable because its durable marker %s; both ACP and native work are unavailable until the marker is recovered explicitly", reason)
	}
	if w.nativeOwnerToken == "" {
		w.mu.Unlock()
		return "already_released", nil
	}
	if w.nativeOwnerToken != ownerToken {
		w.mu.Unlock()
		return "", fmt.Errorf("native provider ownership token does not match Session %s", w.config.Session.ID)
	}
	if generation != w.nativeOwnerGeneration {
		w.mu.Unlock()
		return "", fmt.Errorf("native ownership generation does not match Session %s; refusing a stale release", w.config.Session.ID)
	}
	if reservationOnly && !w.nativeOwnerReserved {
		w.mu.Unlock()
		return "", fmt.Errorf("Session %s has active provider-native ownership; missing tmux alone cannot prove that provider exited, so reservation recovery is refused", w.config.Session.ID)
	}
	if activity {
		if err := w.persistNativeOwnership(nativeMarkerRefreshPending, ownerToken, generation); err != nil {
			w.mu.Unlock()
			return "", fmt.Errorf("persist ACP rehydration marker before native release: %w", err)
		}
	} else if err := w.removeNativeOwnership(); err != nil {
		w.mu.Unlock()
		return "", fmt.Errorf("release native harness ownership: %w", err)
	}
	w.nativeOwnerToken = ""
	w.nativeOwnerGeneration = ""
	w.nativeOwnerReserved = false
	w.nativeOwnerStale = false
	if activity {
		w.requireACPFreshnessLocked()
		w.nativeReconcilePending = true
	}
	w.mu.Unlock()
	eventType := "native_released"
	if reservationOnly {
		eventType = "native_reservation_recovered"
	}
	_, publishErr := w.publish("", eventType, map[string]any{
		"activity": activity, "state": "released",
	})
	if publishErr == nil && activity {
		w.reconcileProviderTranscriptAsync()
	}
	return "released", publishErr
}

func (w *Worker) nativeOwned() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.nativeOwnerToken != ""
}

func validateNativeOwnerToken(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("native owner_token is required")
	}
	if len(value) > 512 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("native owner_token is invalid")
	}
	return value, nil
}

func (w *Worker) publish(requestID, lineType string, data any) (sessionlog.Line, error) {
	w.publishMu.Lock()
	defer w.publishMu.Unlock()
	line, err := w.log.Append(requestID, lineType, data)
	if err != nil {
		return sessionlog.Line{}, err
	}
	w.hub.broadcast(line)
	return line, nil
}

func (w *Worker) updateStatus(status session.Status) error {
	if _, err := w.config.Repository.UpdateSessionStatus(context.Background(), w.config.Session.ID, status); err != nil {
		return err
	}
	w.setLocalStatus(status)
	return nil
}

func (w *Worker) setLocalStatus(status session.Status) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}

func (w *Worker) persistRuntimeInfo(ctx context.Context, info acp.SessionInfo) error {
	updated, err := w.config.Repository.UpdateSessionRuntimeIDs(ctx, w.config.Session.ID, info.ACPXRecordID, info.ACPSessionID, info.NativeSessionID)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.acpSessionID = updated.ACPSessionID
	w.nativeSessionID = updated.NativeSessionID
	w.providerConversationID = updated.ProviderConversationID
	w.mu.Unlock()
	return nil
}

func (w *Worker) writeMetadata(info acp.SessionInfo) error {
	metadata := struct {
		SessionID       string `json:"session_id"`
		PID             int    `json:"pid"`
		Socket          string `json:"socket"`
		ACPXRecordID    string `json:"acpx_record_id,omitempty"`
		ACPSessionID    string `json:"acp_session_id,omitempty"`
		NativeSessionID string `json:"native_session_id,omitempty"`
	}{w.config.Session.ID, w.config.PID, w.SocketPath(), info.ACPXRecordID, info.ACPSessionID, info.NativeSessionID}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	temporary := w.MetadataPath() + ".tmp-" + strconv.Itoa(w.config.PID)
	if err := os.WriteFile(temporary, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write worker metadata: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, w.MetadataPath()); err != nil {
		return fmt.Errorf("publish worker metadata: %w", err)
	}
	return nil
}

func (w *Worker) signalWake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) requestStop() {
	w.mu.Lock()
	stop := w.stop
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (w *Worker) cancelActiveTurn() {
	w.mu.Lock()
	cancel := w.activeCancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func parsePendingPermission(requestID string, event acp.Event) (*pendingPermission, error) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Params struct {
			ToolCall struct {
				ID    string `json:"toolCallId"`
				Title string `json:"title"`
				Kind  string `json:"kind"`
			} `json:"toolCall"`
			Options []struct {
				ID   string `json:"optionId"`
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"options"`
		} `json:"params"`
	}
	if err := json.Unmarshal(event.Raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode ACP permission request: %w", err)
	}
	bridgeID := permissionRPCID(envelope.ID)
	if bridgeID == "" || len(bridgeID) > 256 {
		return nil, fmt.Errorf("ACP permission request has an invalid ID")
	}
	if envelope.Params.ToolCall.ID == "" || len(envelope.Params.ToolCall.ID) > 512 || len(envelope.Params.ToolCall.Title) > 4096 || len(envelope.Params.ToolCall.Kind) > 64 {
		return nil, fmt.Errorf("ACP permission request contains invalid tool metadata")
	}
	if len(envelope.Params.Options) == 0 || len(envelope.Params.Options) > 16 {
		return nil, fmt.Errorf("ACP permission request has an invalid option count")
	}
	pending := &pendingPermission{
		PendingPermission: PendingPermission{
			ID: PermissionConsoleID(requestID, bridgeID), RequestID: requestID,
			Title: envelope.Params.ToolCall.Title, Kind: envelope.Params.ToolCall.Kind,
		},
		bridgeID: bridgeID, byID: make(map[string]PermissionOption),
	}
	kinds := make(map[string]struct{})
	for _, rawOption := range envelope.Params.Options {
		if rawOption.ID == "" || len(rawOption.ID) > 256 || rawOption.Name == "" || len(rawOption.Name) > 1024 || !validPermissionKind(rawOption.Kind) {
			return nil, fmt.Errorf("ACP permission request contains an invalid option")
		}
		if _, duplicate := pending.byID[rawOption.ID]; duplicate {
			return nil, fmt.Errorf("ACP permission request repeats option ID %q", rawOption.ID)
		}
		if _, duplicate := kinds[rawOption.Kind]; duplicate {
			return nil, fmt.Errorf("ACP permission request repeats option kind %q", rawOption.Kind)
		}
		option := PermissionOption{ID: rawOption.ID, Name: rawOption.Name, Kind: rawOption.Kind}
		pending.byID[option.ID] = option
		pending.Options = append(pending.Options, option)
		kinds[option.Kind] = struct{}{}
	}
	return pending, nil
}

// PermissionConsoleID derives a stable gateway-opaque ID for one permission
// callback without exposing the provider's tool-call or RPC identifiers.
func PermissionConsoleID(requestID, bridgeID string) string {
	digest := sha256.Sum256([]byte(requestID + "\x00" + bridgeID))
	return fmt.Sprintf("permission-%x", digest[:12])
}

func permissionRPCID(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func validPermissionKind(kind string) bool {
	switch kind {
	case "allow_once", "allow_always", "reject_once", "reject_always":
		return true
	default:
		return false
	}
}

func (w *Worker) registerPermission(pending *pendingPermission) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || w.active.RequestID != pending.RequestID {
		return fmt.Errorf("permission %s does not belong to the active request", pending.ID)
	}
	if _, exists := w.permissions[pending.ID]; exists {
		return fmt.Errorf("permission %s is already pending", pending.ID)
	}
	if len(w.permissions) >= maxPendingPermissions {
		return errPendingPermissionCap
	}
	w.permissions[pending.ID] = pending
	return nil
}

func (w *Worker) removePermission(permissionID string) {
	w.mu.Lock()
	delete(w.permissions, permissionID)
	w.mu.Unlock()
}

func (w *Worker) decidePermission(ctx context.Context, permissionID, optionID string, cancelled bool) error {
	responder, ok := w.config.ACP.(acp.PermissionResponder)
	if !ok {
		return fmt.Errorf("ACP transport does not support interactive permission decisions")
	}
	w.mu.Lock()
	pending := w.permissions[permissionID]
	if pending == nil {
		w.mu.Unlock()
		return fmt.Errorf("permission %s is no longer pending", permissionID)
	}
	if w.active == nil || w.active.RequestID != pending.RequestID {
		delete(w.permissions, permissionID)
		w.mu.Unlock()
		return fmt.Errorf("permission %s no longer belongs to the active request", permissionID)
	}
	decision := "cancel"
	if !cancelled {
		option, offered := pending.byID[optionID]
		if !offered {
			w.mu.Unlock()
			return fmt.Errorf("permission option %q was not offered", optionID)
		}
		decision = option.Kind
	}
	delete(w.permissions, permissionID) // first valid viewer wins before I/O
	w.mu.Unlock()

	if err := responder.DecidePermission(ctx, pending.bridgeID, optionID, cancelled); err != nil {
		cancelContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = responder.DecidePermission(cancelContext, pending.bridgeID, "", true)
		cancel()
		_, _ = w.publish(pending.RequestID, "permission_abandoned", map[string]string{
			"permission_id": pending.ID, "message": "permission decision transport unavailable",
		})
		return err
	}
	_, err := w.publish(pending.RequestID, "permission_resolved", map[string]string{
		"permission_id": pending.ID, "option_id": optionID, "decision": decision,
	})
	return err
}

func (w *Worker) abandonRequestPermissions(requestID, reason string) {
	w.mu.Lock()
	var abandoned []*pendingPermission
	for id, pending := range w.permissions {
		if pending.RequestID == requestID {
			abandoned = append(abandoned, pending)
			delete(w.permissions, id)
		}
	}
	w.mu.Unlock()
	for _, pending := range abandoned {
		_, _ = w.publish(requestID, "permission_abandoned", map[string]string{
			"permission_id": pending.ID, "message": reason,
		})
	}
}

func (w *Worker) abandonPermissions(reason string) {
	w.mu.Lock()
	var abandoned []*pendingPermission
	for id, pending := range w.permissions {
		abandoned = append(abandoned, pending)
		delete(w.permissions, id)
	}
	w.mu.Unlock()
	for _, pending := range abandoned {
		_, _ = w.publish(pending.RequestID, "permission_abandoned", map[string]string{
			"permission_id": pending.ID, "message": reason,
		})
	}
}

func acquireProcessLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock Session worker: %w", err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		_ = file.Sync()
	}
	return file, nil
}

func writeMessage(writer interface{ Write([]byte) (int, error) }, message Message) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(encoded, '\n'))
	return err
}

func permissionFromMetadata(metadata json.RawMessage) (acp.PermissionMode, error) {
	if len(metadata) == 0 {
		return acp.PermissionReadApproval, nil
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		return "", fmt.Errorf("decode Session permissions: %w", err)
	}
	permission, _ := values["permission"].(string)
	switch permission {
	case "", string(acp.PermissionReadApproval), "read":
		return acp.PermissionReadApproval, nil
	case string(acp.PermissionApproveAll):
		return acp.PermissionApproveAll, nil
	default:
		return "", fmt.Errorf("unsupported Session permission %q", permission)
	}
}
