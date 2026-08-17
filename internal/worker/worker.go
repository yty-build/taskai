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
	queue                  []queuedPrompt
	active                 *queuedPrompt
	activeCancel           context.CancelFunc
	refreshPending         bool
	refreshQueueRemaining  int
	refreshing             bool
	nativeOwnerToken       string
	nativeOwnerGeneration  string
	nativeOwnerReserved    bool
	nativeRefreshBarrier   bool
	acpConnected           bool
	transportError         string
	seenRequestIDs         map[string]struct{}
	permissions            map[string]*pendingPermission
	wake                   chan struct{}
	stop                   context.CancelFunc

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
// entity. It survives a worker restart so a still-running provider-native
// process continues to exclude ACP writes.
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
	restoredRefresh := w.nativeRefreshBarrier
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
		// ensureSession/resume launches an ACP adapter and may mutate provider
		// state (for example, by selecting a provider mode). While the restored
		// native owner may still be alive, expose only the worker socket and use
		// the already-persisted identity for diagnostics.
		info, err = w.persistedRuntimeInfo()
	} else {
		info, err = w.connectACP(ctx)
		if err == nil {
			err = w.persistRuntimeInfo(ctx, info)
		}
		if err == nil {
			w.mu.Lock()
			w.acpConnected = true
			w.mu.Unlock()
		}
	}
	if err != nil {
		_, _ = w.publish("", "worker_error", map[string]string{"message": err.Error()})
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
	w.mu.Unlock()
	defer cancel()

	processorDone := make(chan struct{})
	go func() {
		defer close(processorDone)
		w.processPrompts(runContext)
	}()
	if restoredRefresh {
		w.signalWake()
	}
	acceptDone := make(chan error, 1)
	go func() { acceptDone <- w.accept(runContext) }()

	var runErr error
	select {
	case <-runContext.Done():
	case err := <-acceptDone:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			runErr = err
		}
		cancel()
	}
	_, _ = w.publish("", "worker_stopping", map[string]any{})
	_ = listener.Close()
	w.cancelActiveTurn()
	<-processorDone
	w.abandonPermissions("Session worker stopped")
	if closer, ok := w.config.ACP.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && runErr == nil {
			runErr = fmt.Errorf("close ACP runtime: %w", err)
		}
	}
	w.hub.close()
	w.handlers.Wait()
	if _, err := w.config.Repository.ClearSessionWorker(context.Background(), w.config.Session.ID, session.StatusStopped); err != nil && runErr == nil {
		runErr = err
	}
	w.setLocalStatus(session.StatusStopped)
	return runErr
}

func (w *Worker) connectACP(ctx context.Context) (acp.SessionInfo, error) {
	spec := acp.StartSpec{
		Target: w.target, ResumeACPSessionID: w.config.Session.ACPSessionID,
		ExpectedProviderConversationID: w.config.Session.ProviderConversationID,
		ExpectedNativeSessionID:        w.config.Session.NativeSessionID,
		ProviderStoreVerified:          providerStoreVerified(w.config.Session),
	}
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
	case "refresh":
		state := w.scheduleRefresh()
		writeMessage(connection, Message{Type: "refresh_scheduled", State: state})
	case "native_acquire":
		state, generation, err := w.acquireNative(command.OwnerToken, command.NativeOwnerRole, command.NativeGeneration)
		if err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_acquired", State: state, NativeOwned: true, NativeGeneration: generation, ProviderWriter: "native"})
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
			writeMessage(connection, Message{Type: "error", Message: "provider-native work owns this Session; exit the native provider before stopping its worker"})
			return
		}
		writeMessage(connection, Message{Type: "stopping"})
		go w.requestStop()
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
	if _, exists := w.seenRequestIDs[requestID]; exists {
		w.mu.Unlock()
		return "", fmt.Errorf("request_id %q is already active or queued", requestID)
	}
	state := "ready"
	if w.active != nil || w.refreshPending || w.refreshing || w.nativeOwnerToken != "" || len(w.queue) > 0 {
		state = "queued"
	}
	if strings.TrimSpace(displayMessage) == "" {
		displayMessage = message
	}
	w.queue = append(w.queue, queuedPrompt{RequestID: requestID, Message: message, DisplayMessage: displayMessage})
	w.seenRequestIDs[requestID] = struct{}{}
	w.mu.Unlock()
	if _, err := w.publish(requestID, "prompt_submitted", map[string]string{"display_message": displayMessage}); err != nil {
		return "", err
	}
	_, err := w.publish(requestID, "prompt_queued", map[string]string{"state": state})
	w.signalWake()
	return state, err
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
		w.refreshIfIdle(ctx)
	}
}

func (w *Worker) nextPrompt(ctx context.Context) (queuedPrompt, context.Context, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.transportError != "" || w.active != nil || w.refreshing || w.nativeOwnerToken != "" || len(w.queue) == 0 || (w.refreshPending && w.refreshQueueRemaining == 0) {
		return queuedPrompt{}, nil, false
	}
	prompt := w.queue[0]
	w.queue = w.queue[1:]
	if w.refreshPending && w.refreshQueueRemaining > 0 {
		w.refreshQueueRemaining--
	}
	w.active = &prompt
	promptContext, cancel := context.WithCancel(ctx)
	w.activeCancel = cancel
	return prompt, promptContext, true
}

func (w *Worker) executePrompt(ctx context.Context, prompt queuedPrompt) {
	_ = w.updateStatus(session.StatusWorking)
	_, _ = w.publish(prompt.RequestID, "turn_started", map[string]string{"request_id": prompt.RequestID})
	receipt, err := w.config.ACP.Prompt(ctx, acp.PromptSpec{Target: w.target, RequestID: prompt.RequestID, Message: prompt.Message}, func(event acp.Event) error {
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
	if ctx.Err() != nil {
		w.finishPrompt(prompt.RequestID)
		return
	}
	if err != nil {
		_, _ = w.publish(prompt.RequestID, "turn_error", map[string]string{"message": err.Error()})
		_ = w.updateStatus(session.StatusFailed)
		w.recoverAfterPromptError(ctx, prompt.RequestID, err)
	} else {
		_, _ = w.publish(prompt.RequestID, "turn_completed", receipt)
		_ = w.updateStatus(session.StatusWaiting)
		w.reconcileAfterTurn(ctx, prompt.RequestID)
	}
	w.finishPrompt(prompt.RequestID)
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
	delete(w.seenRequestIDs, requestID)
	hasNext := len(w.queue) > 0 || w.refreshPending
	w.mu.Unlock()
	w.abandonRequestPermissions(requestID, "turn ended")
	if hasNext {
		w.signalWake()
	}
}

func (w *Worker) cancel(ctx context.Context, requestID string) error {
	w.mu.Lock()
	active := w.active
	w.mu.Unlock()
	if active == nil {
		return fmt.Errorf("no active turn to cancel")
	}
	if requestID != "" && active.RequestID != requestID {
		return fmt.Errorf("active request is %q, not %q", active.RequestID, requestID)
	}
	if err := w.config.ACP.Cancel(ctx, w.target); err != nil {
		return err
	}
	_, err := w.publish(active.RequestID, "cancel_requested", map[string]string{"request_id": active.RequestID})
	w.abandonRequestPermissions(active.RequestID, "turn cancelled")
	return err
}

func (w *Worker) scheduleRefresh() string {
	w.mu.Lock()
	if w.transportError != "" {
		w.mu.Unlock()
		return "unavailable"
	}
	if !w.refreshPending {
		w.refreshQueueRemaining = len(w.queue)
	}
	w.refreshPending = true
	deferred := w.active != nil || w.refreshing || w.nativeOwnerToken != "" || w.refreshQueueRemaining > 0
	w.mu.Unlock()
	w.signalWake()
	if deferred {
		return "deferred"
	}
	return "scheduled"
}

func (w *Worker) refreshIfIdle(ctx context.Context) {
	w.mu.Lock()
	if !w.refreshPending || w.refreshing || w.active != nil || w.nativeOwnerToken != "" || w.refreshQueueRemaining > 0 {
		w.mu.Unlock()
		return
	}
	w.refreshPending = false
	w.refreshQueueRemaining = 0
	w.refreshing = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.refreshing = false
		hasQueue := len(w.queue) > 0 || w.refreshPending
		w.mu.Unlock()
		if hasQueue {
			w.signalWake()
		}
	}()

	current, err := w.currentIdentity(ctx)
	if err != nil {
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
		w.quarantineTransport(err)
		return
	}
	if strings.TrimSpace(current.ACPSessionID) == "" {
		err := errors.New("ACP Session ID is unavailable")
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
		w.quarantineTransport(err)
		return
	}
	_, _ = w.publish("", "native_refresh_started", map[string]string{"acp_session_id": current.ACPSessionID})
	w.mu.Lock()
	connected := w.acpConnected
	w.mu.Unlock()
	if !connected {
		// A worker restored under a durable native owner deliberately did not
		// open an ACP adapter. Now that native ownership has been released, an
		// exact Resume is the first provider-facing ACP operation.
		info, resumeErr := w.resumeExact(ctx, current)
		if resumeErr != nil {
			_, _ = w.publish("", "native_refresh_recovery_warning", map[string]string{"message": resumeErr.Error()})
			w.quarantineTransport(fmt.Errorf("exact ACP reconnect after native owner release: %w", resumeErr))
			return
		}
		if barrierErr := w.completeNativeRefreshBarrier(ctx); barrierErr != nil {
			_, _ = w.publish("", "native_refresh_recovery_warning", map[string]string{"message": barrierErr.Error()})
			w.quarantineTransport(barrierErr)
			return
		}
		_, _ = w.publish("", "native_refreshed", map[string]string{"acp_session_id": info.ACPSessionID, "connection": "resumed"})
		return
	}
	refresher, ok := w.config.ACP.(refreshableACPClient)
	if !ok {
		err := errors.New("ACP transport does not support idle refresh")
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
		w.quarantineTransport(err)
		return
	}
	refreshContext, cancelRefresh := context.WithTimeout(ctx, w.refreshTimeout())
	info, err := refresher.Refresh(refreshContext, acp.StartSpec{
		Target: w.target, ResumeACPSessionID: current.ACPSessionID,
		ExpectedProviderConversationID: current.ProviderConversationID,
		ExpectedNativeSessionID:        current.NativeSessionID,
		ProviderStoreVerified:          providerStoreVerified(current),
	})
	cancelRefresh()
	if err == nil {
		err = validateObservedIdentity("worker refresh", info, current)
	}
	if err == nil {
		err = w.persistRuntimeInfo(ctx, info)
	}
	if err == nil {
		w.mu.Lock()
		w.acpConnected = true
		w.mu.Unlock()
	}
	if err != nil {
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
		recovered, recoveryErr := w.resumeExact(ctx, current)
		if recoveryErr != nil {
			_, _ = w.publish("", "native_refresh_recovery_warning", map[string]string{"message": recoveryErr.Error()})
			w.quarantineTransport(fmt.Errorf("exact recovery after native refresh failure: %w", recoveryErr))
			return
		}
		if barrierErr := w.completeNativeRefreshBarrier(ctx); barrierErr != nil {
			_, _ = w.publish("", "native_refresh_recovery_warning", map[string]string{"message": barrierErr.Error()})
			w.quarantineTransport(barrierErr)
			return
		}
		_, _ = w.publish("", "native_refresh_recovered", map[string]string{"acp_session_id": recovered.ACPSessionID})
		return
	}
	if err := w.writeMetadata(info); err != nil {
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
	}
	if err := w.completeNativeRefreshBarrier(ctx); err != nil {
		_, _ = w.publish("", "native_refresh_warning", map[string]string{"message": err.Error()})
		w.quarantineTransport(err)
		return
	}
	_, _ = w.publish("", "native_refreshed", map[string]string{"acp_session_id": info.ACPSessionID})
}

func (w *Worker) completeNativeRefreshBarrier(ctx context.Context) error {
	w.mu.Lock()
	if !w.nativeRefreshBarrier {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	// Re-opening the exact ACP identity is necessary but not sufficient: a
	// provider-native turn is not safe to follow with queued ACP work until the
	// provider's durable transcript can also be verified and parsed for that
	// same canonical conversation and checkout. Keep the private ownership
	// marker in refresh_pending for the entire read. Any failure therefore
	// survives worker shutdown and is retryable through the normal Session
	// resume path without ever dispatching the queued ACP prompt.
	reconcileContext, cancel := context.WithTimeout(ctx, w.refreshTimeout())
	defer cancel()
	current, err := w.currentIdentity(reconcileContext)
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
	if err := w.config.ReconcileProviderTranscript(reconcileContext, w.config.Profile, conversationID, workingDirectory); err != nil {
		return fmt.Errorf("reconcile exact provider transcript before releasing ACP queue: %w", err)
	}
	if err := reconcileContext.Err(); err != nil {
		return fmt.Errorf("reconcile exact provider transcript before releasing ACP queue: %w", err)
	}
	if _, err := w.publish("", "provider_transcript_reconciled", map[string]string{
		"source": "provider-store", "state": "reconciled",
	}); err != nil {
		return fmt.Errorf("record exact provider transcript reconciliation before releasing ACP queue: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.nativeRefreshBarrier {
		return fmt.Errorf("native refresh barrier changed during provider transcript reconciliation")
	}
	if err := w.removeNativeOwnership(); err != nil {
		return fmt.Errorf("complete exact native-to-ACP transcript barrier: %w", err)
	}
	w.nativeRefreshBarrier = false
	return nil
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
	spec := acp.StartSpec{
		Target: w.target, ResumeACPSessionID: current.ACPSessionID,
		ExpectedProviderConversationID: current.ProviderConversationID,
		ExpectedNativeSessionID:        current.NativeSessionID,
		ProviderStoreVerified:          providerStoreVerified(current),
	}
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
	w.refreshPending = false
	w.refreshQueueRemaining = 0
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
		RefreshPending: w.refreshPending, Refreshing: w.refreshing, NativeOwned: w.nativeOwnerToken != "",
	}
	switch {
	case w.active != nil:
		message.ProviderWriter = "acp"
	case w.refreshing:
		message.ProviderWriter = "refresh"
	case w.nativeOwnerToken != "" && w.nativeOwnerReserved:
		message.ProviderWriter = "native-reservation"
	case w.nativeOwnerToken != "":
		message.ProviderWriter = "native"
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
	case w.transportError != "":
		message := w.transportError
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider-native attachment is unavailable because exact ACP continuity failed: %s", message)
	case w.nativeOwnerToken == ownerToken:
		existingGeneration := w.nativeOwnerGeneration
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
			return "", "", fmt.Errorf("activate exclusive native provider ownership: %w", err)
		}
		w.nativeOwnerReserved = false
		w.mu.Unlock()
		if _, err := w.publish("", "native_activated", map[string]string{"state": "activated"}); err != nil {
			w.mu.Lock()
			if w.nativeOwnerToken == ownerToken && w.nativeOwnerGeneration == existingGeneration && !w.nativeOwnerReserved {
				if rollbackErr := w.persistNativeOwnership(nativeMarkerReserved, ownerToken, existingGeneration); rollbackErr != nil {
					w.mu.Unlock()
					return "", "", fmt.Errorf("publish native activation: %v; ownership remains fail-closed as active because reservation rollback failed: %w", err, rollbackErr)
				}
				w.nativeOwnerReserved = true
				w.mu.Unlock()
				return "", "", fmt.Errorf("publish native activation: %w; restored the exact pre-launch reservation and did not authorize provider start", err)
			}
			w.mu.Unlock()
			return "", "", fmt.Errorf("publish native activation: %w; ownership changed concurrently and was not rewritten", err)
		}
		return "activated", existingGeneration, nil
	case w.nativeOwnerToken != "":
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider writer is busy: another native provider process owns Session %s", w.config.Session.ID)
	case w.active != nil:
		requestID := w.active.RequestID
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider writer is busy: ACP request %s is active; retry native attachment after that turn settles", requestID)
	case w.refreshing:
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider writer is busy: exact ACP identity refresh is active; retry native attachment after refresh completes")
	case w.refreshPending:
		w.mu.Unlock()
		return "", "", fmt.Errorf("provider writer is busy: exact ACP identity refresh is scheduled; retry native attachment after refresh completes")
	default:
		if role == NativeOwnerRoleProvider {
			w.mu.Unlock()
			return "", "", fmt.Errorf("native provider reservation is no longer active for Session %s; refusing to start a stale provider process", w.config.Session.ID)
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
			return "", "", fmt.Errorf("persist exclusive native provider ownership: %w", err)
		}
		w.nativeOwnerToken = ownerToken
		w.nativeOwnerGeneration = newGeneration
		w.nativeOwnerReserved = role == NativeOwnerRoleReservation
		w.mu.Unlock()
		if _, err := w.publish("", "native_acquired", map[string]string{"state": resultState}); err != nil {
			w.mu.Lock()
			if w.nativeOwnerToken == ownerToken && w.nativeOwnerGeneration == newGeneration {
				if removeErr := w.removeNativeOwnership(); removeErr != nil {
					w.mu.Unlock()
					return "", "", fmt.Errorf("publish native ownership: %v; ownership remains fail-closed because rollback failed: %w", err, removeErr)
				}
				w.nativeOwnerToken = ""
				w.nativeOwnerGeneration = ""
				w.nativeOwnerReserved = false
			}
			w.mu.Unlock()
			return "", "", err
		}
		return resultState, newGeneration, nil
	}
}

func (w *Worker) nativeReservationStatus(ownerToken string) (string, string, error) {
	ownerToken, err := validateNativeOwnerToken(ownerToken)
	if err != nil {
		return "", "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
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
	// therefore crosses the same exact refresh/reconciliation barrier as a real
	// native turn even though this generation never started our provider.
	return w.releaseNativeOwnership(ownerToken, generation, true, true)
}

func (w *Worker) releaseNativeOwnership(ownerToken, generation string, activity, reservationOnly bool) (string, error) {
	ownerToken, err := validateNativeOwnerToken(ownerToken)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	if w.nativeOwnerToken == "" {
		if w.nativeRefreshBarrier {
			w.mu.Unlock()
			return "refresh_scheduled", nil
		}
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
	state := "released"
	needsConnectionBarrier := !w.acpConnected
	if activity || needsConnectionBarrier {
		if err := w.persistNativeOwnership(nativeMarkerRefreshPending, ownerToken, generation); err != nil {
			w.mu.Unlock()
			return "", fmt.Errorf("persist exact-refresh barrier before releasing native provider ownership: %w", err)
		}
		// Every prompt accepted while native owned must remain behind this exact
		// identity refresh. This is a hard barrier, not the older refresh policy
		// that allowed pre-existing queued prompts to run first.
		w.nativeRefreshBarrier = true
		w.refreshPending = true
		w.refreshQueueRemaining = 0
		state = "refresh_scheduled"
	} else if err := w.removeNativeOwnership(); err != nil {
		w.mu.Unlock()
		return "", fmt.Errorf("release exclusive native provider ownership: %w", err)
	}
	w.nativeOwnerToken = ""
	w.nativeOwnerGeneration = ""
	w.nativeOwnerReserved = false
	w.mu.Unlock()
	eventType := "native_released"
	if reservationOnly {
		eventType = "native_reservation_recovered"
	}
	_, publishErr := w.publish("", eventType, map[string]any{
		"activity": activity, "connection_deferred": needsConnectionBarrier, "state": state,
	})
	w.signalWake()
	return state, publishErr
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
