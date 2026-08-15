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
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

var ErrAlreadyRunning = errors.New("Session worker is already running")

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

type Config struct {
	Session          session.Session
	Profile          agents.Profile
	Repository       Repository
	ACP              ACPClient
	WorkersDirectory string
	LogsDirectory    string
	PID              int
}

type queuedPrompt struct {
	RequestID      string
	Message        string
	DisplayMessage string
}

type Worker struct {
	config Config
	target acp.Target

	mu              sync.Mutex
	status          session.Status
	acpSessionID    string
	nativeSessionID string
	queue           []queuedPrompt
	active          *queuedPrompt
	activeCancel    context.CancelFunc
	nativeLease     bool
	seenRequestIDs  map[string]struct{}
	wake            chan struct{}
	stop            context.CancelFunc

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
		config: config, target: target, status: config.Session.Status, acpSessionID: config.Session.ACPSessionID, nativeSessionID: config.Session.NativeSessionID,
		seenRequestIDs: make(map[string]struct{}), wake: make(chan struct{}, 1),
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

	info, err := w.connectACP(ctx)
	if err != nil {
		_, _ = w.publish("", "worker_error", map[string]string{"message": err.Error()})
		_, _ = w.config.Repository.UpdateSessionStatus(context.Background(), w.config.Session.ID, session.StatusFailed)
		return err
	}
	if err := w.persistRuntimeInfo(ctx, info); err != nil {
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
	w.hub.close()
	w.handlers.Wait()
	if _, err := w.config.Repository.ClearSessionWorker(context.Background(), w.config.Session.ID, session.StatusStopped); err != nil && runErr == nil {
		runErr = err
	}
	w.setLocalStatus(session.StatusStopped)
	return runErr
}

func (w *Worker) connectACP(ctx context.Context) (acp.SessionInfo, error) {
	spec := acp.StartSpec{Target: w.target, ResumeACPSessionID: w.config.Session.ACPSessionID}
	if spec.ResumeACPSessionID != "" {
		return w.config.ACP.Resume(ctx, spec)
	}
	return w.config.ACP.Start(ctx, spec)
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
		writeMessage(connection, Message{Type: "pong"})
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
	case "subscribe":
		w.subscribe(ctx, connection, command.From)
	case "tail":
		w.tail(connection, command.From)
	case "native_acquire":
		if err := w.acquireNative(ctx); err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_acquired", State: "waiting"})
	case "native_release":
		if err := w.releaseNative(ctx); err != nil {
			writeMessage(connection, Message{Type: "error", Message: err.Error()})
			return
		}
		writeMessage(connection, Message{Type: "native_released", State: "waiting"})
	case "stop":
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
	if _, exists := w.seenRequestIDs[requestID]; exists {
		w.mu.Unlock()
		return "", fmt.Errorf("request_id %q is already active or queued", requestID)
	}
	state := "ready"
	if w.active != nil || w.nativeLease || len(w.queue) > 0 {
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
	}
}

func (w *Worker) nextPrompt(ctx context.Context) (queuedPrompt, context.Context, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.nativeLease || w.active != nil || len(w.queue) == 0 {
		return queuedPrompt{}, nil, false
	}
	prompt := w.queue[0]
	w.queue = w.queue[1:]
	w.active = &prompt
	promptContext, cancel := context.WithCancel(ctx)
	w.activeCancel = cancel
	return prompt, promptContext, true
}

func (w *Worker) executePrompt(ctx context.Context, prompt queuedPrompt) {
	_ = w.updateStatus(session.StatusWorking)
	_, _ = w.publish(prompt.RequestID, "turn_started", map[string]string{"request_id": prompt.RequestID})
	receipt, err := w.config.ACP.Prompt(ctx, acp.PromptSpec{Target: w.target, RequestID: prompt.RequestID, Message: prompt.Message}, func(event acp.Event) error {
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
	} else {
		_, _ = w.publish(prompt.RequestID, "turn_completed", receipt)
		_ = w.updateStatus(session.StatusWaiting)
		if info, inspectErr := w.config.ACP.Inspect(ctx, w.target); inspectErr != nil {
			_, _ = w.publish(prompt.RequestID, "metadata_warning", map[string]string{"message": inspectErr.Error()})
		} else if persistErr := w.persistRuntimeInfo(ctx, info); persistErr != nil {
			_, _ = w.publish(prompt.RequestID, "metadata_warning", map[string]string{"message": persistErr.Error()})
		} else {
			_ = w.writeMetadata(info)
		}
	}
	w.finishPrompt(prompt.RequestID)
}

func (w *Worker) finishPrompt(requestID string) {
	w.mu.Lock()
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.activeCancel = nil
	w.active = nil
	delete(w.seenRequestIDs, requestID)
	hasNext := len(w.queue) > 0 && !w.nativeLease
	w.mu.Unlock()
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
	return err
}

func (w *Worker) acquireNative(ctx context.Context) error {
	w.mu.Lock()
	if w.active != nil {
		activeRequestID := w.active.RequestID
		w.mu.Unlock()
		return fmt.Errorf("active ACP turn %q must finish or be cancelled before native mode", activeRequestID)
	}
	if w.nativeLease {
		w.mu.Unlock()
		return fmt.Errorf("native mode is already leased")
	}
	if w.nativeSessionID == "" {
		w.mu.Unlock()
		return fmt.Errorf("provider-native Session ID is unavailable; refusing to start an uncorrelated native conversation")
	}
	if w.acpSessionID == "" {
		w.mu.Unlock()
		return fmt.Errorf("ACP Session ID is unavailable; refusing native mode without a reconciliation target")
	}
	w.nativeLease = true
	w.mu.Unlock()
	if err := w.updateStatus(session.StatusWaiting); err != nil {
		w.mu.Lock()
		w.nativeLease = false
		w.mu.Unlock()
		return err
	}
	if _, err := w.publish("", "native_acquired", map[string]bool{"exclusive": true}); err != nil {
		w.mu.Lock()
		w.nativeLease = false
		w.mu.Unlock()
		return err
	}
	return nil
}

func (w *Worker) releaseNative(ctx context.Context) error {
	w.mu.Lock()
	if !w.nativeLease {
		w.mu.Unlock()
		return fmt.Errorf("native mode is not leased")
	}
	acpSessionID := w.acpSessionID
	expectedNativeSessionID := w.nativeSessionID
	w.mu.Unlock()

	var (
		info acp.SessionInfo
		err  error
	)
	if acpSessionID == "" {
		return fmt.Errorf("native reconciliation cannot resume without the prior ACP Session ID; Session remains waiting with its lease held")
	}
	info, err = w.config.ACP.Resume(ctx, acp.StartSpec{Target: w.target, ResumeACPSessionID: acpSessionID})
	if err != nil {
		_, _ = w.publish("", "native_reconcile_error", map[string]string{"message": err.Error()})
		return fmt.Errorf("native reconciliation failed; Session remains waiting with its lease held: %w", err)
	}
	if info.NativeSessionID == "" || info.NativeSessionID != expectedNativeSessionID {
		message := fmt.Sprintf("expected provider-native Session ID %q, received %q", expectedNativeSessionID, info.NativeSessionID)
		_, _ = w.publish("", "native_reconcile_error", map[string]string{"message": message})
		return fmt.Errorf("native reconciliation could not prove conversation identity (%s); Session remains waiting with its lease held", message)
	}
	if err := w.persistRuntimeInfo(ctx, info); err != nil {
		return fmt.Errorf("native reconciliation persistence failed; Session remains waiting with its lease held: %w", err)
	}
	if err := w.writeMetadata(info); err != nil {
		return fmt.Errorf("native reconciliation metadata failed; Session remains waiting with its lease held: %w", err)
	}
	if _, err = w.publish("", "native_released", map[string]bool{"reconciled": true}); err != nil {
		return fmt.Errorf("record native reconciliation before release; Session remains waiting with its lease held: %w", err)
	}
	w.mu.Lock()
	w.nativeLease = false
	hasQueue := len(w.queue) > 0
	w.mu.Unlock()
	if hasQueue {
		w.signalWake()
	}
	return nil
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
	message := Message{Type: "status", Status: string(w.status), Queued: len(w.queue), NativeLease: w.nativeLease}
	if w.active != nil {
		message.ActiveRequestID = w.active.RequestID
	}
	return message
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
