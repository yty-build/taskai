package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

var ErrAlreadyRunning = errors.New("Session worker is already running")

const (
	maxPendingPermissions         = 128
	maxPermissionIDBytes          = 256
	maxPermissionDisplayBytes     = 512
	maxPermissionKindBytes        = 128
	providerStartupTimeout        = 30 * time.Second
	providerShutdownTimeout       = 5 * time.Second
	providerCancelTimeout         = 2500 * time.Millisecond
	defaultSubscriberWriteTimeout = 2 * time.Second
)

type Repository interface {
	GetSession(context.Context, string) (session.Session, error)
	CompareAndSwapSessionWorker(context.Context, string, session.WorkerState, session.WorkerState, string) (session.Session, bool, error)
	CompareAndSwapSessionProviderRef(context.Context, string, session.WorkerState, string, string) (session.Session, bool, error)
}

// SessionPort is the worker's only provider boundary. A worker opens one Port
// at startup, retains it for its lifetime, and never refreshes or resumes it
// between prompts.
type SessionPort interface {
	Probe(context.Context) error
	Start(context.Context, acp.StartSpec) (acp.SessionInfo, error)
	Resume(context.Context, acp.StartSpec) (acp.SessionInfo, error)
	Prompt(context.Context, acp.PromptSpec, func(acp.Event) error) (acp.PromptReceipt, error)
	Cancel(context.Context, acp.Target) error
	Close() error
}

type configurableSessionPort interface {
	Configure(context.Context, acp.StartSpec) (acp.SessionInfo, error)
}

type providerOptionsSessionPort interface {
	ProviderOptions(context.Context, acp.StartSpec) (acp.ProviderOptionCatalog, error)
}

type permissionSessionPort interface {
	DecidePermission(context.Context, string, string, bool) error
}

type Config struct {
	Session    session.Session
	Profile    agents.Profile
	Repository Repository
	Port       SessionPort
	// Preflight validates persisted bootstrap identity after the Session
	// Channel is addressable but before any provider Port operation.
	Preflight        func(context.Context) error
	WorkersDirectory string
	LogsDirectory    string
	PID              int
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
}

type activeTurn struct {
	requestID       string
	message         string
	cancel          context.CancelFunc
	done            chan struct{}
	cancelRequested bool
	cancelUncertain bool
	terminal        bool
	settledHealthy  bool
}

type portStartup struct {
	info acp.SessionInfo
	err  error
}

type pendingPermission struct {
	PendingPermission
	bridgeID string
	byID     map[string]PermissionOption
}

type Worker struct {
	config Config
	target acp.Target

	mu               sync.Mutex
	status           session.Status
	portReady        bool
	active           *activeTurn
	controlActive    bool
	permissions      map[string]*pendingPermission
	transportError   string
	runContext       context.Context
	stop             context.CancelFunc
	shutdownStatus   session.Status
	canStartProvider bool

	publishMu sync.Mutex
	log       *sessionlog.Log
	hub       *hub
	listener  net.Listener
	// Overridden only by focused tests; every subscriber write receives a fresh
	// deadline so an attached non-reader cannot retain one of the bounded slots.
	subscriberWriteTimeout time.Duration
	// providerFrames serializes provider callbacks, permission delivery, and
	// terminal publication into one causal journal order.
	providerFrames sync.Mutex

	handlers sync.WaitGroup
	turns    sync.WaitGroup

	portCloseOnce sync.Once
	portCloseDone chan struct{}
	portCloseErr  error

	channelObservedOnce sync.Once
	channelObserved     chan struct{}
}

func New(config Config) (*Worker, error) {
	if config.Repository == nil || config.Port == nil {
		return nil, fmt.Errorf("worker repository and Session Port are required")
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
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = providerStartupTimeout
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = providerShutdownTimeout
	}
	workingDirectory := config.Session.WorktreePath
	if workingDirectory == "" {
		workingDirectory = config.Session.RepoPath
	}
	if workingDirectory == "" {
		return nil, fmt.Errorf("Session %s has no repo/worktree path", config.Session.ID)
	}
	permission, err := permissionFromMetadata(config.Session.MetadataJSON)
	if err != nil {
		return nil, err
	}
	return &Worker{
		config: config,
		target: acp.Target{
			ConsoleSessionID: config.Session.ID,
			Profile:          config.Profile,
			WorkingDirectory: workingDirectory,
			Permission:       permission,
		},
		status:                 config.Session.Status,
		canStartProvider:       config.Session.Status == session.StatusStarting && config.Session.ProviderRef == "",
		permissions:            make(map[string]*pendingPermission),
		portCloseDone:          make(chan struct{}),
		channelObserved:        make(chan struct{}),
		subscriberWriteTimeout: defaultSubscriberWriteTimeout,
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

func (w *Worker) LogPath() string {
	return sessionlog.ChannelPath(w.config.LogsDirectory, w.config.Session.ID)
}

func (w *Worker) Run(ctx context.Context) (runErr error) {
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
	defer w.hub.close()
	w.canStartProvider = w.config.Session.Status == session.StatusStarting && history.LastSequence() == 0 && !history.ReplayReset() && w.config.Session.ProviderRef == ""

	runContext, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.runContext = runContext
	w.stop = cancel
	w.mu.Unlock()
	defer cancel()

	ownerless := w.config.Session.WorkerState()
	ownedStarting := session.WorkerState{Status: session.StatusStarting, PID: w.config.PID, Socket: w.SocketPath()}
	claimed, swapped, err := w.config.Repository.CompareAndSwapSessionWorker(ctx, w.config.Session.ID, ownerless, ownedStarting, w.LogPath())
	if err != nil {
		return err
	}
	if !swapped || claimed.WorkerState() != ownedStarting {
		return fmt.Errorf("Session %s lifecycle changed before worker claim (now %s); refusing provider startup", w.config.Session.ID, claimed.Status)
	}
	workerRegistered := true
	preserveWorkerOwnership := false
	defer func() {
		if workerRegistered && !preserveWorkerOwnership {
			_ = w.clearWorker(session.StatusFailed)
		}
	}()
	w.setLocalStatus(session.StatusStarting)
	if _, err := w.publish("", FrameConnection, map[string]string{"state": "connecting"}); err != nil {
		return err
	}
	// sessionlog.Open durably seeds a replay_gap record when it preserves an
	// interrupted segment. It must precede this startup's connecting record so
	// the gap and monotonic high-water survive another crash at this boundary.

	acceptDone := make(chan error, 1)
	go func() { acceptDone <- w.accept(runContext) }()
	connectionTerminalPublished := false
	acceptConsumed := false
	startupSettled := true
	var startupInterrupted error
	var outcome portStartup

	// Provider startup cannot overtake Spawn's private two-step channel
	// admission. The Session ID, socket, and replayable connecting frame are
	// observable before Start/Resume begins; ordinary pings do not release it.
	select {
	case <-w.channelObserved:
	case acceptErr := <-acceptDone:
		acceptConsumed = true
		if acceptErr != nil {
			startupInterrupted = acceptErr
		} else {
			startupInterrupted = fmt.Errorf("Session Channel stopped before provider startup")
		}
	case <-runContext.Done():
		startupInterrupted = runContext.Err()
	}

	var shutdownMu sync.Mutex
	var shutdownDeadline time.Time
	beginShutdown := func() time.Time {
		shutdownMu.Lock()
		if shutdownDeadline.IsZero() {
			shutdownDeadline = time.Now().Add(w.config.ShutdownTimeout)
		}
		deadline := shutdownDeadline
		shutdownMu.Unlock()
		w.beginPortClose()
		return deadline
	}
	stopPortClose := context.AfterFunc(runContext, func() { beginShutdown() })
	defer stopPortClose()

	if startupInterrupted == nil {
		portContext, cancelPort := context.WithTimeout(runContext, w.config.StartupTimeout)
		portDone := make(chan portStartup, 1)
		go func() {
			info, err := w.openPort(portContext)
			portDone <- portStartup{info: info, err: err}
		}()
		select {
		case outcome = <-portDone:
		case acceptErr := <-acceptDone:
			acceptConsumed = true
			cancelPort()
			deadline := beginShutdown()
			outcome, startupSettled = w.awaitPortStartup(portDone, deadline)
			if acceptErr != nil {
				startupInterrupted = acceptErr
			} else {
				startupInterrupted = fmt.Errorf("Session Channel stopped during provider startup")
			}
		case <-runContext.Done():
			cancelPort()
			deadline := beginShutdown()
			outcome, startupSettled = w.awaitPortStartup(portDone, deadline)
			startupInterrupted = runContext.Err()
		case <-portContext.Done():
			cancelPort()
			deadline := beginShutdown()
			outcome, startupSettled = w.awaitPortStartup(portDone, deadline)
			startupInterrupted = portContext.Err()
		}
		startupDeadlineExceeded := errors.Is(portContext.Err(), context.DeadlineExceeded)
		cancelPort()
		if startupInterrupted == nil && startupDeadlineExceeded {
			startupInterrupted = context.DeadlineExceeded
		}
	} else {
		outcome.err = startupInterrupted
	}
	info, startupErr := outcome.info, outcome.err
	if startupErr == nil {
		// A successful Start may have created the canonical provider identity
		// just as Stop or the parent context won the select above. Persist that
		// validated identity before honoring lifecycle cancellation so a later
		// worker exact-resumes it instead of replacement-starting.
		persistDeadline := time.Now().Add(3 * time.Second)
		shutdownMu.Lock()
		if !shutdownDeadline.IsZero() && shutdownDeadline.Before(persistDeadline) {
			persistDeadline = shutdownDeadline
		}
		shutdownMu.Unlock()
		persistContext, cancelPersist := context.WithDeadline(context.WithoutCancel(ctx), persistDeadline)
		startupErr = w.persistRuntimeInfo(persistContext, info)
		cancelPersist()
	}
	if startupErr == nil {
		startupErr = w.writeMetadata(info)
	}
	if startupErr == nil && startupInterrupted != nil {
		startupErr = startupInterrupted
	}
	if startupErr == nil && runContext.Err() != nil {
		startupErr = runContext.Err()
	}
	if startupErr == nil {
		// Admission waits on w.mu while connected is journaled and the worker
		// row advances. A client can observe the frame immediately, but no Port
		// command can overtake this readiness boundary.
		w.mu.Lock()
		_, startupErr = w.publish("", FrameConnection, map[string]string{"state": "connected"})
		if startupErr == nil {
			_, startupErr = w.transitionOwnedStatus(runContext, session.StatusStarting, session.StatusWaiting)
		}
		if startupErr == nil {
			w.status = session.StatusWaiting
			w.portReady = true
		}
		w.mu.Unlock()
	}
	if startupErr != nil {
		w.mu.Lock()
		shutdownStatus := w.shutdownStatus
		if shutdownStatus == "" {
			shutdownStatus = session.StatusFailed
			if errors.Is(startupErr, context.Canceled) && ctx.Err() != nil {
				shutdownStatus = session.StatusStopped
			}
			w.shutdownStatus = shutdownStatus
		}
		w.mu.Unlock()
		if shutdownStatus == session.StatusFailed {
			_, _ = w.publish("", FrameWorkerError, map[string]string{"code": "provider_connect_failed", "message": "The provider connection could not be opened."})
			_, _ = w.publish("", FrameConnection, map[string]string{"state": "failed"})
			connectionTerminalPublished = true
			_ = w.updateStatus(session.StatusFailed)
		}
		cancel()
		_ = listener.Close()
		if !acceptConsumed {
			_ = <-acceptDone
		}
		runErr = startupErr
	} else {
		select {
		case <-runContext.Done():
			_ = listener.Close()
			runErr = <-acceptDone
		case runErr = <-acceptDone:
			cancel()
		}
	}
	if errors.Is(runErr, net.ErrClosed) || errors.Is(runErr, context.Canceled) {
		runErr = nil
	}

	// Close the retained provider Port as soon as shutdown begins. Provider
	// calls are allowed to use Close as their interruption boundary, so waiting
	// for handlers first can deadlock Stop. Ownership is still retained until
	// every handler/turn and this one Close have completed safely.
	deadline := beginShutdown()
	abandonErr := w.fenceActiveTurnAndAbandonPermissions("Session channel closed", deadline)
	shutdownErr := w.waitForPortAndCalls(deadline)
	if abandonErr != nil {
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("durably abandon pending provider permissions: %w", abandonErr)
		} else {
			shutdownErr = fmt.Errorf("%v; durably abandon pending provider permissions: %w", shutdownErr, abandonErr)
		}
	}
	if !startupSettled && shutdownErr == nil {
		shutdownErr = fmt.Errorf("provider startup did not terminate safely")
	}
	if shutdownErr != nil {
		preserveWorkerOwnership = true
		w.mu.Lock()
		w.shutdownStatus = session.StatusFailed
		w.transportError = "provider connection did not close safely"
		w.mu.Unlock()
		_, _ = w.publish("", FrameWorkerError, map[string]string{
			"code": "provider_close_failed", "message": "The provider connection could not be proven closed. This Session is quarantined and requires manual provider-process teardown before its worker ownership can be cleared.",
		})
		_ = w.updateStatus(session.StatusFailed)
		if runErr == nil {
			runErr = shutdownErr
		}
	}
	w.mu.Lock()
	shutdownStatus := w.shutdownStatus
	w.mu.Unlock()
	if shutdownStatus == "" {
		shutdownStatus = session.StatusStopped
	}
	if !connectionTerminalPublished {
		_, _ = w.publish("", FrameConnection, map[string]string{"state": string(shutdownStatus)})
	}
	if preserveWorkerOwnership {
		workerRegistered = false
	} else if clearErr := w.clearWorker(shutdownStatus); clearErr != nil {
		// A failed guarded release means the row changed after this worker's
		// observation. Never let the deferred fallback retry with a newer local
		// status and erase retained quarantine or replacement ownership.
		preserveWorkerOwnership = true
		if runErr == nil {
			runErr = clearErr
		}
	} else {
		workerRegistered = false
	}
	w.setLocalStatus(shutdownStatus)
	return runErr
}

func (w *Worker) openPort(ctx context.Context) (acp.SessionInfo, error) {
	if w.config.Preflight != nil {
		if err := w.config.Preflight(ctx); err != nil {
			return acp.SessionInfo{}, err
		}
	}
	// Compatibility probing and binary discovery happen behind the already-
	// addressable Session Channel. A slow or missing ACP runtime is therefore
	// visible as connecting -> worker_error -> failed instead of blocking the
	// client before it can subscribe.
	if err := w.config.Port.Probe(ctx); err != nil {
		return acp.SessionInfo{}, err
	}
	// Provider identity is always established first. Persisted controls are
	// applied through Configure on this same retained Port only after the
	// returned identity has been durably recorded.
	spec := w.startSpec(w.config.Session, session.ProviderOptions{})
	if spec.ResumeProviderRef == "" {
		if !w.canStartProvider {
			return acp.SessionInfo{}, fmt.Errorf("provider identity is unavailable after an earlier worker attempt; create a new Session")
		}
		info, err := w.config.Port.Start(ctx, spec)
		if err != nil {
			return acp.SessionInfo{}, err
		}
		if err := acp.ValidateProviderRef("worker startup", info, ""); err != nil {
			return acp.SessionInfo{}, err
		}
		return info, nil
	}
	if err := acp.ValidateProviderRefValue("worker startup resume request", spec.ResumeProviderRef); err != nil {
		return acp.SessionInfo{}, err
	}
	info, err := w.config.Port.Resume(ctx, spec)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	if err := acp.ValidateProviderRef("worker startup resume", info, spec.ResumeProviderRef); err != nil {
		return acp.SessionInfo{}, err
	}
	return info, nil
}

func (w *Worker) beginPortClose() <-chan struct{} {
	w.portCloseOnce.Do(func() {
		go func() {
			w.portCloseErr = w.config.Port.Close()
			close(w.portCloseDone)
		}()
	})
	return w.portCloseDone
}

func (w *Worker) waitForPortAndCalls(deadline time.Time) error {
	portDone := w.beginPortClose()
	callsDone := make(chan struct{})
	go func() {
		w.handlers.Wait()
		w.turns.Wait()
		close(callsDone)
	}()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fmt.Errorf("close Session Port and settle provider calls: timed out after %s", w.config.ShutdownTimeout)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	portClosed := false
	callsSettled := false
	for !portClosed || !callsSettled {
		select {
		case <-portDone:
			portClosed = true
			portDone = nil
		case <-callsDone:
			callsSettled = true
			callsDone = nil
		case <-timer.C:
			return fmt.Errorf("close Session Port and settle provider calls: timed out after %s", w.config.ShutdownTimeout)
		}
	}
	if w.portCloseErr != nil {
		return fmt.Errorf("close Session Port: %w", w.portCloseErr)
	}
	return nil
}

func (w *Worker) awaitPortStartup(portDone <-chan portStartup, deadline time.Time) (portStartup, bool) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return portStartup{err: fmt.Errorf("provider startup did not stop within %s", w.config.ShutdownTimeout)}, false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case outcome := <-portDone:
		return outcome, true
	case <-timer.C:
		return portStartup{err: fmt.Errorf("provider startup did not stop within %s", w.config.ShutdownTimeout)}, false
	}
}

func (w *Worker) startSpec(current session.Session, options session.ProviderOptions) acp.StartSpec {
	return acp.StartSpec{
		Target:            w.target,
		ResumeProviderRef: current.ProviderRef,
		Model:             options.Model,
		ReasoningEffort:   options.ReasoningEffort,
	}
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
	// ctx is the worker lifetime, deliberately not the socket lifetime. Once a
	// mutating command has been decoded, loss of its response connection must not
	// cancel the provider operation halfway through and make a retry unsafe. The
	// caller context bounds only its own wait; worker operation bounds settle the
	// command and the journal/stream exposes the resulting state.
	stopCloseOnShutdown := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCloseOnShutdown()
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	if !scanner.Scan() {
		return
	}
	var command Command
	if err := json.Unmarshal(scanner.Bytes(), &command); err != nil {
		_ = writeMessage(connection, Message{Type: "error", ErrorCode: "invalid_command", Message: "invalid command JSON"})
		return
	}
	switch command.Type {
	case "_admit":
		if command.SessionID != w.config.Session.ID {
			_ = writeMessage(connection, Message{Type: "error", ErrorCode: "session_mismatch", Message: "Session worker target does not match"})
			return
		}
		w.mu.Lock()
		state := w.status
		if !w.portReady {
			state = session.StatusStarting
		}
		w.mu.Unlock()
		if err := writeMessage(connection, Message{
			Type: "admission_ready", SessionID: w.config.Session.ID, ProtocolVersion: ProtocolVersion,
			Capabilities: []string{CapabilitySessionChannel}, State: string(state),
		}); err != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		var acknowledgement Command
		if err := json.Unmarshal(scanner.Bytes(), &acknowledgement); err != nil || acknowledgement.Type != "_admit_ack" || acknowledgement.SessionID != w.config.Session.ID {
			return
		}
		w.channelObservedOnce.Do(func() { close(w.channelObserved) })
	case "ping":
		if command.SessionID != w.config.Session.ID {
			_ = writeMessage(connection, Message{Type: "error", ErrorCode: "session_mismatch", Message: "Session worker target does not match"})
			return
		}
		w.mu.Lock()
		state := w.status
		if !w.portReady {
			state = session.StatusStarting
		}
		w.mu.Unlock()
		_ = writeMessage(connection, Message{Type: "pong", SessionID: w.config.Session.ID, ProtocolVersion: ProtocolVersion, Capabilities: []string{CapabilitySessionChannel}, State: string(state)})
	case "status":
		_ = writeMessage(connection, w.statusMessage())
	case "provider_options":
		catalog, err := w.loadProviderOptions(ctx)
		if err != nil {
			_ = writeMessage(connection, workerErrorMessage(err))
			if w.channelFailed() {
				w.requestStop(session.StatusFailed)
			}
			return
		}
		_ = writeMessage(connection, Message{Type: "provider_options", ProviderOptions: &catalog})
	case "configure":
		catalog, err := w.configure(ctx, session.ProviderOptions{Model: command.Model, ReasoningEffort: command.ReasoningEffort})
		if err != nil {
			_ = writeMessage(connection, workerErrorMessage(err))
			if w.channelFailed() {
				w.requestStop(session.StatusFailed)
			}
			return
		}
		_ = writeMessage(connection, Message{Type: "configured", ProviderOptions: &catalog})
	case "prompt":
		state, err := w.admitPrompt(command.RequestID, command.Message)
		if err != nil {
			_ = writeMessage(connection, workerErrorMessage(err))
			if w.channelFailed() {
				w.requestStop(session.StatusFailed)
			}
			return
		}
		_ = writeMessage(connection, Message{Type: "accepted", RequestID: command.RequestID, State: state})
	case "cancel":
		requestID, err := w.cancel(ctx, command.RequestID)
		if err != nil {
			_ = writeMessage(connection, workerErrorMessage(err))
			if w.channelFailed() {
				w.requestStop(session.StatusFailed)
			}
			return
		}
		_ = writeMessage(connection, Message{Type: "cancelled", RequestID: requestID})
	case "permission_response":
		requestID, err := w.decidePermission(ctx, command.PermissionID, command.OptionID, command.Cancelled)
		if err != nil {
			_ = writeMessage(connection, workerErrorMessage(err))
			if w.channelFailed() {
				w.requestStop(session.StatusFailed)
			}
			return
		}
		_ = writeMessage(connection, Message{Type: "permission_responded", RequestID: requestID})
	case "subscribe":
		w.subscribe(ctx, connection, command.From)
	case "tail":
		w.tail(ctx, connection, command.From)
	case "stop":
		_ = writeMessage(connection, Message{Type: "stopping"})
		w.requestStop(session.StatusStopped)
	default:
		_ = writeMessage(connection, Message{Type: "error", ErrorCode: "unknown_command", Message: "unknown command type"})
	}
}

func workerErrorMessage(err error) Message {
	message := Message{Type: "error", Message: err.Error()}
	var busy *BusyError
	var notReady *NotReadyError
	var optionsError *acp.ProviderOptionsError
	var configurationError *acp.ProviderConfigurationError
	var permissionRejected *acp.ProviderPermissionRejectedError
	var channelUnavailable *ChannelUnavailableError
	switch {
	case errors.As(err, &busy):
		message.ErrorCode = "session_busy"
		message.State = "busy"
		message.ActiveRequestID = busy.ActiveRequestID
		message.Message = busy.Error()
	case errors.As(err, &notReady):
		message.ErrorCode = "session_starting"
		message.State = "starting"
		message.Message = notReady.Error()
	case errors.As(err, &channelUnavailable):
		message.ErrorCode = "session_channel_failed"
		message.Message = channelUnavailable.Error()
	case errors.As(err, &optionsError):
		message.ErrorCode = "provider_options_unavailable"
	case errors.As(err, &configurationError):
		message.ErrorCode = "provider_configuration_failed"
	case errors.As(err, &permissionRejected):
		message.ErrorCode = "permission_response_rejected"
	}
	return message
}

func (w *Worker) beginIdleControl() (func(), error) {
	w.mu.Lock()
	if !w.portReady {
		w.mu.Unlock()
		return nil, &NotReadyError{SessionID: w.config.Session.ID}
	}
	if w.transportError != "" {
		w.mu.Unlock()
		return nil, fmt.Errorf("Session channel is unavailable")
	}
	if w.active != nil || w.controlActive {
		activeID := ""
		if w.active != nil {
			activeID = w.active.requestID
		}
		w.mu.Unlock()
		return nil, &BusyError{SessionID: w.config.Session.ID, ActiveRequestID: activeID}
	}
	w.controlActive = true
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		w.controlActive = false
		w.mu.Unlock()
	}, nil
}

func (w *Worker) loadProviderOptions(ctx context.Context) (acp.ProviderOptionCatalog, error) {
	release, err := w.beginIdleControl()
	if err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	defer release()
	reader, ok := w.config.Port.(providerOptionsSessionPort)
	if !ok {
		return acp.ProviderOptionCatalog{}, &acp.ProviderOptionsError{}
	}
	current, err := w.config.Repository.GetSession(ctx, w.config.Session.ID)
	if err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	catalog, err := reader.ProviderOptions(ctx, w.startSpec(current, session.ProviderOptions{}))
	if err != nil {
		return acp.ProviderOptionCatalog{}, w.classifyProviderOptionsFailure(err)
	}
	if err := catalog.Validate(); err != nil {
		return acp.ProviderOptionCatalog{}, w.failChannel(
			"provider_connection_failed",
			"The provider connection returned an invalid configuration catalog. Wait for this worker to stop, then resume the Session.",
		)
	}
	return catalog, nil
}

func (w *Worker) configure(ctx context.Context, options session.ProviderOptions) (acp.ProviderOptionCatalog, error) {
	if err := options.Validate(); err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	release, err := w.beginIdleControl()
	if err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	defer release()
	reader, canRead := w.config.Port.(providerOptionsSessionPort)
	configurer, canConfigure := w.config.Port.(configurableSessionPort)
	if !canRead {
		return acp.ProviderOptionCatalog{}, &acp.ProviderOptionsError{}
	}
	if !canConfigure {
		return acp.ProviderOptionCatalog{}, &acp.ProviderConfigurationError{}
	}
	current, err := w.config.Repository.GetSession(ctx, w.config.Session.ID)
	if err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	catalog, err := reader.ProviderOptions(ctx, w.startSpec(current, session.ProviderOptions{}))
	if err != nil {
		return acp.ProviderOptionCatalog{}, w.classifyProviderOptionsFailure(err)
	}
	if err := catalog.Validate(); err != nil {
		return acp.ProviderOptionCatalog{}, w.failChannel(
			"provider_connection_failed",
			"The provider connection returned an invalid configuration catalog. Wait for this worker to stop, then resume the Session.",
		)
	}
	// Validate only values the caller selected. A provider may truthfully
	// advertise a current/default token that is not itself selectable; blank
	// still means preserve it, not reject it or send an unadvertised value.
	if err := catalog.ValidateSelection(options); err != nil {
		return acp.ProviderOptionCatalog{}, err
	}
	// Where a current value is selectable, include it in the one live tuple to
	// guard against interdependent controls. Otherwise leave that field blank
	// and prove after Configure that the provider preserved its exact value.
	effective := options
	if effective.Model == "" && catalog.Model.CurrentValue != "" {
		currentModel := session.ProviderOptions{Model: catalog.Model.CurrentValue}
		if catalog.ValidateSelection(currentModel) == nil {
			effective.Model = catalog.Model.CurrentValue
		}
	}
	if effective.ReasoningEffort == "" && catalog.ReasoningEffort.CurrentValue != "" {
		currentEffort := session.ProviderOptions{ReasoningEffort: catalog.ReasoningEffort.CurrentValue}
		if catalog.ValidateSelection(currentEffort) == nil {
			effective.ReasoningEffort = catalog.ReasoningEffort.CurrentValue
		}
	}
	previousModel := catalog.Model.CurrentValue
	previousEffort := catalog.ReasoningEffort.CurrentValue
	info, err := configurer.Configure(ctx, w.startSpec(current, effective))
	if err != nil {
		return acp.ProviderOptionCatalog{}, w.failConfiguration()
	}
	if err := w.persistRuntimeInfo(ctx, info); err != nil {
		return acp.ProviderOptionCatalog{}, w.failConfiguration()
	}
	current, err = w.config.Repository.GetSession(ctx, w.config.Session.ID)
	if err != nil {
		return acp.ProviderOptionCatalog{}, w.failConfiguration()
	}
	catalog, err = reader.ProviderOptions(ctx, w.startSpec(current, session.ProviderOptions{}))
	if err != nil || catalog.Validate() != nil {
		return acp.ProviderOptionCatalog{}, w.failConfiguration()
	}
	wantModel := options.Model
	if wantModel == "" {
		wantModel = previousModel
	}
	wantEffort := options.ReasoningEffort
	if wantEffort == "" {
		wantEffort = previousEffort
	}
	if catalog.Model.CurrentValue != wantModel || catalog.ReasoningEffort.CurrentValue != wantEffort {
		return acp.ProviderOptionCatalog{}, w.failConfiguration()
	}
	return catalog, nil
}

func (w *Worker) failConfiguration() error {
	_ = w.failChannel(
		"provider_configuration_failed",
		"The provider configuration could not be applied safely. Wait for this worker to stop, then resume the Session.",
	)
	return &acp.ProviderConfigurationError{}
}

func (w *Worker) classifyProviderOptionsFailure(err error) error {
	var unavailable *acp.ProviderOptionsError
	if errors.As(err, &unavailable) {
		return unavailable
	}
	return w.failChannel(
		"provider_connection_failed",
		"The provider connection could not safely report its configuration catalog. Wait for this worker to stop, then resume the Session.",
	)
}

func (w *Worker) failChannel(code, message string) error {
	w.mu.Lock()
	alreadyFailed := w.transportError != ""
	w.transportError = "provider connection is unavailable"
	w.mu.Unlock()
	if !alreadyFailed {
		_, _ = w.publish("", FrameWorkerError, map[string]string{"code": code, "message": message})
		_ = w.updateStatus(session.StatusFailed)
	}
	return &ChannelUnavailableError{}
}

func (w *Worker) channelFailed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.transportError != ""
}

func (w *Worker) admitPrompt(requestID, message string) (string, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "", fmt.Errorf("prompt request_id is required")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("prompt message is required")
	}
	w.mu.Lock()
	if !w.portReady {
		w.mu.Unlock()
		return "", &NotReadyError{SessionID: w.config.Session.ID}
	}
	if w.transportError != "" {
		w.mu.Unlock()
		return "", fmt.Errorf("Session channel is unavailable")
	}
	if w.active != nil || w.controlActive {
		activeID := ""
		if w.active != nil {
			activeID = w.active.requestID
		}
		w.mu.Unlock()
		return "", &BusyError{SessionID: w.config.Session.ID, ActiveRequestID: activeID}
	}
	if w.runContext == nil || w.runContext.Err() != nil {
		w.mu.Unlock()
		return "", fmt.Errorf("Session channel is stopping")
	}
	if _, err := w.publish(requestID, FrameClientCommand, map[string]string{"command": "prompt", "text": message}); err != nil {
		w.mu.Unlock()
		return "", w.failChannel(
			"channel_journal_failed",
			"The Session Channel could not durably record the prompt. The Session channel was stopped before provider dispatch.",
		)
	}
	turnContext, cancel := context.WithCancel(w.runContext)
	turn := &activeTurn{requestID: requestID, message: message, cancel: cancel, done: make(chan struct{})}
	w.active = turn
	w.mu.Unlock()

	w.turns.Add(1)
	go func() {
		defer w.turns.Done()
		w.executePrompt(turnContext, turn)
	}()
	return "ready", nil
}

func (w *Worker) executePrompt(ctx context.Context, turn *activeTurn) {
	defer close(turn.done)
	if err := w.updateStatus(session.StatusWorking); err != nil {
		_ = w.failChannel(
			"session_lifecycle_changed",
			"The Session lifecycle changed before the accepted prompt reached the provider. The prompt was not sent.",
		)
		_, _ = w.publish(turn.requestID, FrameProviderResult, map[string]any{
			"status": "failed", "source": "transport", "code": "session_lifecycle_changed",
			"message": "The Session lifecycle changed before provider dispatch; the prompt was not sent.",
		})
		w.finishTurn(turn)
		w.requestStop(session.StatusFailed)
		return
	}
	receipt, promptErr := w.config.Port.Prompt(ctx, acp.PromptSpec{
		Target: w.target, RequestID: turn.requestID, Message: turn.message,
	}, func(event acp.Event) error {
		w.providerFrames.Lock()
		defer w.providerFrames.Unlock()
		var pending *pendingPermission
		if event.Type == "session_request_permission" {
			w.mu.Lock()
			acceptPermission := w.active == turn && !turn.cancelRequested && !turn.terminal
			w.mu.Unlock()
			if !acceptPermission {
				// Cancel won the serialization boundary. Preserve the raw provider
				// event for diagnostics, but never create a new actionable callback
				// after the accepted cancel command.
				_, err := w.publish(turn.requestID, FrameProviderEvent, json.RawMessage(event.Raw))
				return err
			}
			parsed, err := parsePendingPermission(turn.requestID, event)
			if err != nil {
				return err
			}
			if err := w.registerPermission(parsed); err != nil {
				return err
			}
			pending = parsed
		}
		if _, err := w.publish(turn.requestID, FrameProviderEvent, json.RawMessage(event.Raw)); err != nil {
			if pending != nil {
				w.removePermission(pending.ID)
			}
			return err
		}
		if pending != nil {
			if _, err := w.publish(turn.requestID, FramePermissionRequested, pending.PendingPermission); err != nil {
				w.removePermission(pending.ID)
				return err
			}
		}
		return nil
	})

	w.providerFrames.Lock()
	abandonErr := w.abandonRequestPermissionsLocked(turn.requestID, "turn ended")
	terminal, healthy := w.providerTerminal(turn, receipt, promptErr, ctx.Err())
	if abandonErr != nil {
		terminal = map[string]any{
			"status": "failed", "source": "transport", "code": "channel_journal_failed",
			"message": "The Session Channel could not durably record the provider turn boundary.",
		}
		healthy = false
	}
	w.mu.Lock()
	if w.active == turn {
		turn.terminal = true
	}
	w.mu.Unlock()
	terminalDurable := true
	if _, err := w.publish(turn.requestID, FrameProviderResult, terminal); err != nil {
		terminalDurable = false
		healthy = false
	}
	w.providerFrames.Unlock()
	if abandonErr != nil {
		_ = w.failChannel("channel_journal_failed", "The Session Channel could not durably record abandoned permission callbacks. The Session channel was stopped.")
	}
	if healthy && terminalDurable {
		if err := w.updateStatus(session.StatusWaiting); err == nil {
			w.mu.Lock()
			turn.settledHealthy = true
			w.mu.Unlock()
			w.finishTurn(turn)
			return
		}
	}
	w.mu.Lock()
	if w.transportError == "" {
		w.transportError = "provider connection failed"
	}
	cancelRequested := w.active == turn && turn.cancelRequested
	w.mu.Unlock()
	_ = w.updateStatus(session.StatusFailed)
	w.finishTurn(turn)
	if !cancelRequested {
		w.requestStop(session.StatusFailed)
	}
}

func (w *Worker) providerTerminal(turn *activeTurn, receipt acp.PromptReceipt, promptErr, contextErr error) (map[string]any, bool) {
	w.mu.Lock()
	cancelRequested := w.active == turn && turn.cancelRequested
	cancelUncertain := w.active == turn && turn.cancelUncertain
	w.mu.Unlock()
	if cancelUncertain {
		return map[string]any{
			"status": "failed", "source": "transport", "code": "provider_cancel_unconfirmed",
			"message": "The provider turn could not be cancelled safely. Resume this Session before sending another prompt.",
		}, false
	}
	if cancelRequested && (promptErr == nil || errors.Is(promptErr, context.Canceled)) {
		return map[string]any{"status": "cancelled", "stop_reason": "cancelled"}, true
	}
	if errors.Is(promptErr, context.Canceled) || errors.Is(contextErr, context.Canceled) {
		return map[string]any{"status": "cancelled", "stop_reason": "cancelled"}, true
	}
	if promptErr == nil {
		return map[string]any{
			"status": "completed", "stop_reason": receipt.StopReason, "provider_sequence": receipt.LastSeq,
		}, true
	}
	var providerErr *acp.ProviderTurnError
	if errors.As(promptErr, &providerErr) {
		failure := safeProviderTurnFailure(providerErr)
		failure["status"] = "failed"
		return failure, true
	}
	return map[string]any{
		"status": "failed", "source": "transport", "code": "provider_connection_failed",
		"message": "The provider connection failed. Resume this Session before sending another prompt.",
	}, false
}

func safeProviderTurnFailure(providerErr *acp.ProviderTurnError) map[string]any {
	code := providerTurnFailureCode(providerErr.DetailCode, providerErr.Code)
	message := map[string]string{
		"busy":           "The provider is busy with this conversation. Try again after its current turn settles.",
		"conflict":       "The provider reported a conversation conflict. Try again after the other turn settles.",
		"rate_limited":   "The provider rate-limited this turn. Try again shortly.",
		"rejected":       "The provider rejected this turn.",
		"provider_error": "The provider could not complete this turn.",
	}[code]
	return map[string]any{"source": "provider", "code": code, "retryable": providerErr.Retryable, "message": message}
}

func providerTurnFailureCode(values ...string) string {
	for _, value := range values {
		value = strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
		switch value {
		case "busy", "provider_busy", "conversation_busy", "turn_busy":
			return "busy"
		case "conflict", "provider_conflict", "conversation_conflict", "concurrent_turn":
			return "conflict"
		case "rate_limited", "rate_limit", "too_many_requests", "throttled":
			return "rate_limited"
		case "rejected", "provider_rejected", "refused", "denied":
			return "rejected"
		}
	}
	return "provider_error"
}

func (w *Worker) finishTurn(turn *activeTurn) {
	w.mu.Lock()
	if w.active == turn {
		if turn.cancel != nil {
			turn.cancel()
		}
		w.active = nil
	}
	w.mu.Unlock()
}

func (w *Worker) cancel(ctx context.Context, requestID string) (string, error) {
	w.providerFrames.Lock()
	w.mu.Lock()
	if !w.portReady {
		w.mu.Unlock()
		w.providerFrames.Unlock()
		return "", &NotReadyError{SessionID: w.config.Session.ID}
	}
	turn := w.active
	if turn == nil {
		w.mu.Unlock()
		w.providerFrames.Unlock()
		return "", fmt.Errorf("no active provider turn to cancel")
	}
	if requestID != "" && requestID != turn.requestID {
		w.mu.Unlock()
		w.providerFrames.Unlock()
		return "", fmt.Errorf("active request is %q, not %q", turn.requestID, requestID)
	}
	if turn.terminal {
		w.mu.Unlock()
		w.providerFrames.Unlock()
		return "", fmt.Errorf("no active provider turn to cancel")
	}
	if turn.cancelRequested {
		requestID = turn.requestID
		w.mu.Unlock()
		w.providerFrames.Unlock()
		duplicateContext, duplicateCancel := context.WithTimeout(context.WithoutCancel(ctx), providerCancelTimeout)
		defer duplicateCancel()
		return requestID, w.waitForCancellation(duplicateContext, turn)
	}
	requestID = turn.requestID
	if _, err := w.publish(requestID, FrameClientCommand, map[string]string{"command": "cancel"}); err != nil {
		w.mu.Unlock()
		w.providerFrames.Unlock()
		return requestID, w.failCancellation(turn, "channel_journal_failed")
	}
	turn.cancelRequested = true
	w.mu.Unlock()
	if err := w.abandonRequestPermissionsLocked(requestID, "turn cancellation accepted"); err != nil {
		w.providerFrames.Unlock()
		return requestID, w.failCancellation(turn, "channel_journal_failed")
	}
	w.providerFrames.Unlock()

	// A user cancellation is a provider command, not a teardown signal. Keep
	// the Prompt context alive so it drains the provider's cancelled result and
	// the Port implementation can prove that the same retained connection was
	// re-established after turn finalization. Only worker shutdown cancels the
	// Prompt context.
	cancelContext, cancelCall := context.WithTimeout(context.WithoutCancel(ctx), providerCancelTimeout)
	defer cancelCall()
	if err := w.config.Port.Cancel(cancelContext, w.target); err != nil {
		return requestID, w.failCancellation(turn, "provider_cancel_failed")
	}
	if err := w.waitForCancellation(cancelContext, turn); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return requestID, err
		}
		return requestID, w.failCancellation(turn, "provider_cancel_timeout")
	}
	return requestID, nil
}

func (w *Worker) waitForCancellation(ctx context.Context, turn *activeTurn) error {
	select {
	case <-turn.done:
		w.mu.Lock()
		healthy := turn.settledHealthy
		w.mu.Unlock()
		if !healthy {
			return &ChannelUnavailableError{}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) failCancellation(turn *activeTurn, code string) error {
	w.mu.Lock()
	if w.active == turn {
		turn.cancelUncertain = true
	}
	w.mu.Unlock()
	err := w.failChannel(code, "The provider turn could not be cancelled safely. The Session channel was stopped.")
	return err
}

func (w *Worker) statusMessage() Message {
	// Permission state and its sequence watermark form one snapshot. Every
	// permission mutation and its durable frame uses providerFrames, so a
	// viewer can safely reconcile only after it has applied frames through Seq.
	w.providerFrames.Lock()
	defer w.providerFrames.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	message := Message{Type: "status", Status: string(w.status), Message: w.transportError, Seq: w.log.LastSequence()}
	if w.active != nil {
		message.ActiveRequestID = w.active.requestID
	}
	for _, pending := range w.permissions {
		copyPermission := pending.PendingPermission
		copyPermission.Options = append([]PermissionOption(nil), pending.Options...)
		message.Permissions = append(message.Permissions, copyPermission)
	}
	sort.Slice(message.Permissions, func(i, j int) bool { return message.Permissions[i].ID < message.Permissions[j].ID })
	return message
}

func (w *Worker) subscribe(ctx context.Context, connection net.Conn, from uint64) {
	id, cutoff, wake, ok := w.hub.add()
	if !ok {
		_ = w.writeSubscriberMessage(connection, Message{Type: "error", Message: "Session Channel viewer capacity is unavailable or the worker is stopping"})
		return
	}
	defer w.hub.remove(id)
	ctx, stopWatching := watchSubscriberConnection(ctx, connection)
	defer stopWatching()
	delivered := from
	if cutoff == 0 {
		if from > 0 {
			_ = w.writeSubscriberMessage(connection, streamErrorMessage(&sessionlog.CursorGapError{Cursor: from, Last: 0}))
			return
		}
	} else {
		var subscriberWriteErr error
		if _, err := w.log.WalkLiveAfter(ctx, delivered, cutoff, func(line sessionlog.Line) error {
			if err := w.writeSubscriberMessage(connection, lineMessage(line)); err != nil {
				subscriberWriteErr = err
				return err
			}
			delivered = line.Seq
			return nil
		}); err != nil {
			if subscriberWriteErr == nil {
				_ = w.writeSubscriberMessage(connection, streamErrorMessage(err))
			}
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-wake:
			if !open {
				return
			}
			through := w.hub.sequence()
			if through <= delivered {
				continue
			}
			if _, err := w.log.WalkLiveAfter(ctx, delivered, through, func(line sessionlog.Line) error {
				if err := w.writeSubscriberMessage(connection, lineMessage(line)); err != nil {
					return err
				}
				delivered = line.Seq
				return nil
			}); err != nil {
				return
			}
		}
	}
}

// watchSubscriberConnection turns a quiet peer close into cancellation of
// only this disposable viewer. Session Channel subscriptions are server-only
// after their initial command, so any read completion means this subscriber is
// gone (or violated the protocol); it must not cancel the worker or its Port.
func watchSubscriberConnection(parent context.Context, connection net.Conn) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var unexpected [1]byte
		_, _ = connection.Read(unexpected[:])
		cancel()
	}()
	return ctx, func() {
		cancel()
		// The outer handler closes this connection after subscribe returns. A
		// read deadline first releases our watcher so cleanup never waits on
		// that later defer.
		_ = connection.SetReadDeadline(time.Now())
		<-done
	}
}

func (w *Worker) tail(ctx context.Context, connection net.Conn, from uint64) {
	cutoff := w.log.LastSequence()
	if cutoff == 0 {
		if from > 0 {
			_ = w.writeSubscriberMessage(connection, streamErrorMessage(&sessionlog.CursorGapError{Cursor: from, Last: 0}))
			return
		}
	} else {
		var subscriberWriteErr error
		if _, err := w.log.WalkAfter(ctx, from, cutoff, func(line sessionlog.Line) error {
			subscriberWriteErr = w.writeSubscriberMessage(connection, lineMessage(line))
			return subscriberWriteErr
		}); err != nil {
			if subscriberWriteErr == nil {
				_ = w.writeSubscriberMessage(connection, streamErrorMessage(err))
			}
			return
		}
	}
	_ = w.writeSubscriberMessage(connection, Message{Type: "end", Seq: cutoff})
}

func (w *Worker) writeSubscriberMessage(connection net.Conn, message Message) error {
	timeout := w.subscriberWriteTimeout
	if timeout <= 0 {
		timeout = defaultSubscriberWriteTimeout
	}
	if err := connection.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	return writeMessage(connection, message)
}

func streamErrorMessage(err error) Message {
	message := Message{Type: "error", Message: "Session Channel replay is unavailable"}
	var gap *sessionlog.CursorGapError
	if errors.As(err, &gap) {
		message.ErrorCode = "replay_cursor_gap"
		message.Seq = gap.Last
		message.Message = gap.Error()
	}
	return message
}

func (w *Worker) publish(requestID, lineType string, data any) (sessionlog.Line, error) {
	w.publishMu.Lock()
	defer w.publishMu.Unlock()
	var (
		line sessionlog.Line
		err  error
	)
	if lineType == FrameProviderEvent {
		line, err = w.log.AppendBuffered(requestID, lineType, data)
	} else {
		line, err = w.log.Append(requestID, lineType, data)
	}
	if err != nil {
		return sessionlog.Line{}, err
	}
	w.hub.broadcast(line.Seq)
	return line, nil
}

func (w *Worker) updateStatus(status session.Status) error {
	w.mu.Lock()
	beforeStatus := w.status
	w.mu.Unlock()
	updated, err := w.transitionOwnedStatus(context.Background(), beforeStatus, status)
	if err != nil {
		return err
	}
	w.setLocalStatus(updated.Status)
	return nil
}

func (w *Worker) transitionOwnedStatus(ctx context.Context, beforeStatus, afterStatus session.Status) (session.Session, error) {
	before := session.WorkerState{Status: beforeStatus, PID: w.config.PID, Socket: w.SocketPath()}
	after := before
	after.Status = afterStatus
	updated, swapped, err := w.config.Repository.CompareAndSwapSessionWorker(ctx, w.config.Session.ID, before, after, "")
	if err != nil {
		return session.Session{}, err
	}
	if !swapped || updated.WorkerState() != after {
		return updated, fmt.Errorf("Session %s lifecycle ownership changed while transitioning %s to %s", w.config.Session.ID, beforeStatus, afterStatus)
	}
	return updated, nil
}

func (w *Worker) clearWorker(status session.Status) error {
	w.mu.Lock()
	beforeStatus := w.status
	w.mu.Unlock()
	before := session.WorkerState{Status: beforeStatus, PID: w.config.PID, Socket: w.SocketPath()}
	after := session.WorkerState{Status: status}
	_, swapped, err := w.config.Repository.CompareAndSwapSessionWorker(context.Background(), w.config.Session.ID, before, after, "")
	if err != nil {
		return err
	}
	if !swapped {
		return fmt.Errorf("Session %s lifecycle ownership changed before worker release", w.config.Session.ID)
	}
	return nil
}

func (w *Worker) setLocalStatus(status session.Status) {
	w.mu.Lock()
	w.status = status
	w.mu.Unlock()
}

func (w *Worker) persistRuntimeInfo(ctx context.Context, info acp.SessionInfo) error {
	w.mu.Lock()
	owner := session.WorkerState{Status: w.status, PID: w.config.PID, Socket: w.SocketPath()}
	w.mu.Unlock()
	updated, swapped, err := w.config.Repository.CompareAndSwapSessionProviderRef(ctx, w.config.Session.ID, owner, info.RuntimeRef, info.ProviderRef)
	if err != nil {
		return err
	}
	if !swapped || updated.WorkerState() != owner {
		return fmt.Errorf("Session %s lifecycle ownership changed before provider identity could be persisted", w.config.Session.ID)
	}
	return nil
}

func (w *Worker) writeMetadata(info acp.SessionInfo) error {
	metadata := struct {
		SessionID   string `json:"session_id"`
		PID         int    `json:"pid"`
		Socket      string `json:"socket"`
		RuntimeRef  string `json:"runtime_ref,omitempty"`
		ProviderRef string `json:"provider_ref,omitempty"`
	}{w.config.Session.ID, w.config.PID, w.SocketPath(), info.RuntimeRef, info.ProviderRef}
	body, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(w.config.WorkersDirectory, ".metadata-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, w.MetadataPath())
}

func (w *Worker) requestStop(status session.Status) {
	w.mu.Lock()
	if status != "" {
		w.shutdownStatus = status
	}
	w.portReady = false
	stop := w.stop
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// fenceActiveTurnAndAbandonPermissions establishes one shutdown boundary with
// provider callbacks: after cancelRequested is set while providerFrames is
// held, no later provider callback may become actionable, and every callback
// already exposed by the worker receives a durable abandonment frame before
// teardown waits can time out. The same shutdown deadline bounds acquisition
// of providerFrames so an uncooperative provider decision cannot deadlock the
// worker instead of entering quarantine.
func (w *Worker) fenceActiveTurnAndAbandonPermissions(reason string, deadline time.Time) error {
	w.mu.Lock()
	turn := w.active
	if turn != nil {
		turn.cancelRequested = true
	}
	w.mu.Unlock()
	if turn != nil && turn.cancel != nil {
		turn.cancel()
	}
	for !w.providerFrames.TryLock() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out while serializing pending provider permissions")
		}
		delay := min(remaining, 5*time.Millisecond)
		timer := time.NewTimer(delay)
		<-timer.C
	}
	abandonErr := w.abandonAllPermissionsLocked(reason)
	w.providerFrames.Unlock()
	return abandonErr
}

func parsePendingPermission(requestID string, event acp.Event) (*pendingPermission, error) {
	var request struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(event.Raw, &request); err != nil {
		return nil, fmt.Errorf("decode provider permission request: %w", err)
	}
	if !boundedPermissionField(event.PermissionID, maxPermissionIDBytes, true) ||
		!boundedPermissionField(request.ToolCall.ToolCallID, maxPermissionIDBytes, true) ||
		!boundedPermissionField(request.ToolCall.Title, maxPermissionDisplayBytes, false) ||
		!boundedPermissionField(request.ToolCall.Kind, maxPermissionKindBytes, false) ||
		len(request.Options) == 0 || len(request.Options) > 16 {
		return nil, fmt.Errorf("provider permission request is incomplete")
	}
	pending := &pendingPermission{
		PendingPermission: PendingPermission{
			ID: PermissionConsoleID(requestID, request.ToolCall.ToolCallID), RequestID: requestID,
			Title: request.ToolCall.Title, Kind: request.ToolCall.Kind,
		},
		bridgeID: event.PermissionID,
		byID:     make(map[string]PermissionOption, len(request.Options)),
	}
	seenKinds := make(map[string]struct{}, len(request.Options))
	for _, item := range request.Options {
		if !boundedPermissionField(item.OptionID, maxPermissionIDBytes, true) ||
			!boundedPermissionField(item.Name, maxPermissionDisplayBytes, true) ||
			!boundedPermissionField(item.Kind, maxPermissionKindBytes, true) ||
			!validPermissionKind(item.Kind) {
			return nil, fmt.Errorf("provider permission request contains an invalid option")
		}
		if _, exists := pending.byID[item.OptionID]; exists {
			return nil, fmt.Errorf("provider permission request contains duplicate options")
		}
		if _, exists := seenKinds[item.Kind]; exists {
			return nil, fmt.Errorf("provider permission request contains duplicate option kinds")
		}
		option := PermissionOption{ID: item.OptionID, Name: item.Name, Kind: item.Kind}
		pending.byID[option.ID] = option
		seenKinds[option.Kind] = struct{}{}
		pending.Options = append(pending.Options, option)
	}
	return pending, nil
}

func boundedPermissionField(value string, limit int, required bool) bool {
	if value == "" {
		return !required
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, item := range value {
		if !unicode.IsPrint(item) {
			return false
		}
	}
	return true
}

func PermissionConsoleID(requestID, bridgeID string) string {
	return requestID + ":" + bridgeID
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
	if len(w.permissions) >= maxPendingPermissions {
		return fmt.Errorf("pending permission limit reached")
	}
	if _, exists := w.permissions[pending.ID]; exists {
		return fmt.Errorf("permission %q is already pending", pending.ID)
	}
	w.permissions[pending.ID] = pending
	return nil
}

func (w *Worker) removePermission(permissionID string) {
	w.mu.Lock()
	delete(w.permissions, permissionID)
	w.mu.Unlock()
}

func (w *Worker) decidePermission(ctx context.Context, permissionID, optionID string, cancelled bool) (string, error) {
	w.providerFrames.Lock()
	defer w.providerFrames.Unlock()
	w.mu.Lock()
	if !w.portReady {
		w.mu.Unlock()
		return "", &NotReadyError{SessionID: w.config.Session.ID}
	}
	pending := w.permissions[permissionID]
	if pending == nil {
		w.mu.Unlock()
		return "", fmt.Errorf("permission %q is no longer pending", permissionID)
	}
	turn := w.active
	if turn == nil || turn.requestID != pending.RequestID || turn.terminal || turn.cancelRequested {
		w.mu.Unlock()
		return "", fmt.Errorf("permission %q cannot be resolved after its provider turn began cancellation or ended", permissionID)
	}
	if !cancelled {
		if _, exists := pending.byID[optionID]; !exists {
			w.mu.Unlock()
			return "", fmt.Errorf("permission option %q was not offered", optionID)
		}
	}
	w.mu.Unlock()
	responder, ok := w.config.Port.(permissionSessionPort)
	if !ok {
		return w.failPermissionContinuity(pending.RequestID)
	}
	if err := responder.DecidePermission(ctx, pending.bridgeID, optionID, cancelled); err != nil {
		var rejected *acp.ProviderPermissionRejectedError
		if errors.As(err, &rejected) {
			return pending.RequestID, rejected
		}
		return w.failPermissionContinuity(pending.RequestID)
	}
	w.mu.Lock()
	if w.permissions[permissionID] == pending {
		delete(w.permissions, permissionID)
	}
	w.mu.Unlock()
	decision := optionID
	if cancelled {
		decision = "cancelled"
	}
	if _, err := w.publish(pending.RequestID, FramePermissionResolved, map[string]string{
		"permission_id": permissionID, "decision": decision,
	}); err != nil {
		return pending.RequestID, w.failChannel(
			"channel_journal_failed",
			"The provider accepted a permission response, but the Session Channel could not record it durably. The Session channel was stopped.",
		)
	}
	return pending.RequestID, nil
}

// failPermissionContinuity is called with providerFrames held. It removes and
// durably abandons every callback before failing the retained Port boundary,
// so neither this decision nor a sibling decision can be retried ambiguously.
func (w *Worker) failPermissionContinuity(requestID string) (string, error) {
	if abandonErr := w.abandonAllPermissionsLocked("provider permission decision continuity was lost"); abandonErr != nil {
		return requestID, w.failChannel(
			"channel_journal_failed",
			"The Session Channel could not durably record abandoned permission callbacks. The Session channel was stopped.",
		)
	}
	return requestID, w.failChannel(
		"provider_permission_continuity_lost",
		"The provider permission response could not be confirmed safely. Pending permission callbacks were abandoned and the Session channel was stopped.",
	)
}

func (w *Worker) abandonRequestPermissions(requestID, reason string) error {
	w.providerFrames.Lock()
	defer w.providerFrames.Unlock()
	return w.abandonRequestPermissionsLocked(requestID, reason)
}

func (w *Worker) abandonRequestPermissionsLocked(requestID, reason string) error {
	w.mu.Lock()
	var abandoned []*pendingPermission
	for id, pending := range w.permissions {
		if pending.RequestID == requestID {
			abandoned = append(abandoned, pending)
			delete(w.permissions, id)
		}
	}
	w.mu.Unlock()
	sort.Slice(abandoned, func(i, j int) bool { return abandoned[i].ID < abandoned[j].ID })
	for _, pending := range abandoned {
		if _, err := w.publish(requestID, FramePermissionAbandoned, map[string]string{
			"permission_id": pending.ID, "reason": reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) abandonAllPermissionsLocked(reason string) error {
	w.mu.Lock()
	abandoned := make([]*pendingPermission, 0, len(w.permissions))
	for id, pending := range w.permissions {
		abandoned = append(abandoned, pending)
		delete(w.permissions, id)
	}
	w.mu.Unlock()
	sort.Slice(abandoned, func(i, j int) bool { return abandoned[i].ID < abandoned[j].ID })
	for _, pending := range abandoned {
		if _, err := w.publish(pending.RequestID, FramePermissionAbandoned, map[string]string{
			"permission_id": pending.ID, "reason": reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func permissionFromMetadata(metadata json.RawMessage) (acp.PermissionMode, error) {
	if len(metadata) == 0 {
		return acp.PermissionReadApproval, nil
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		return "", fmt.Errorf("decode Session permission metadata: %w", err)
	}
	value, _ := values["permission"].(string)
	switch acp.PermissionMode(value) {
	case "", acp.PermissionReadApproval:
		return acp.PermissionReadApproval, nil
	case acp.PermissionApproveAll:
		return acp.PermissionApproveAll, nil
	default:
		return "", fmt.Errorf("invalid Session permission metadata")
	}
}

func acquireProcessLock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("secure worker lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock Session worker: %w", err)
	}
	return lock, nil
}

func writeMessage(writer interface{ Write([]byte) (int, error) }, message Message) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	_, err = writer.Write(body)
	return err
}
