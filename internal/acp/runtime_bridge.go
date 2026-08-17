package acp

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
)

//go:embed runtime_bridge.mjs
var runtimeBridgeSource []byte

const (
	runtimeBridgeStartupTimeout = 15 * time.Second
	maxBridgeCommandFrame       = 4 << 20
	bridgeMessageQueue          = 4096
	maxBridgeStderrBytes        = 16 << 10
)

type runtimeBridge struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	promptInput io.WriteCloser

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan bridgeMessage
	closed  chan struct{}
	exited  chan struct{}
	err     error
	stderr  string
	nextID  atomic.Uint64
}

type bridgeMessage struct {
	Type         string          `json:"type"`
	CommandID    string          `json:"commandId"`
	RequestID    string          `json:"requestId"`
	PermissionID string          `json:"permissionId"`
	InferredKind string          `json:"inferredKind"`
	Reason       string          `json:"reason"`
	Message      string          `json:"message"`
	Code         string          `json:"code"`
	DetailCode   string          `json:"detailCode"`
	Retryable    bool            `json:"retryable"`
	Data         json.RawMessage `json:"data"`
	Event        json.RawMessage `json:"event"`
	Request      json.RawMessage `json:"request"`
	Result       json.RawMessage `json:"result"`
}

type bridgeRemoteCommandError struct {
	protocol   *ProtocolError
	code       string
	detailCode string
	retryable  bool
	message    string
}

func (e *bridgeRemoteCommandError) Error() string { return e.protocol.Error() }
func (e *bridgeRemoteCommandError) Unwrap() error { return e.protocol }

type bridgeCommand struct {
	Type            string            `json:"type"`
	CommandID       string            `json:"commandId"`
	SessionKey      string            `json:"sessionKey,omitempty"`
	Agent           string            `json:"agent,omitempty"`
	Argv            []string          `json:"argv,omitempty"`
	UseBuiltin      bool              `json:"useBuiltin,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	CWD             string            `json:"cwd,omitempty"`
	PermissionMode  PermissionMode    `json:"permissionMode,omitempty"`
	ResumeSessionID string            `json:"resumeSessionId,omitempty"`
	RequestID       string            `json:"requestId,omitempty"`
	Text            string            `json:"text,omitempty"`
	PermissionID    string            `json:"permissionId,omitempty"`
	OptionID        string            `json:"optionId,omitempty"`
	Cancelled       bool              `json:"cancelled,omitempty"`
	Reason          string            `json:"reason,omitempty"`
}

type bridgeSession struct {
	Handle bridgeHandle `json:"handle"`
	Status bridgeStatus `json:"status"`
}

type bridgeHandle struct {
	SessionKey         string `json:"sessionKey"`
	RuntimeSessionName string `json:"runtimeSessionName"`
	ACPXRecordID       string `json:"acpxRecordId"`
	BackendSessionID   string `json:"backendSessionId"`
	AgentSessionID     string `json:"agentSessionId"`
	CWD                string `json:"cwd"`
}

type bridgeStatus struct {
	Summary          string `json:"summary"`
	ACPXRecordID     string `json:"acpxRecordId"`
	BackendSessionID string `json:"backendSessionId"`
	AgentSessionID   string `json:"agentSessionId"`
}

func startRuntimeBridge(ctx context.Context, binary, stateDirectory string) (*runtimeBridge, error) {
	packageRoot, err := installedPackageRoot(binary)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(stateDirectory) == "" {
		return nil, fmt.Errorf("acpx runtime state directory is required")
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create acpx runtime state directory: %w", err)
	}
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("secure acpx runtime state directory: %w", err)
	}
	scriptPath := filepath.Join(stateDirectory, "runtime-bridge.mjs")
	if err := writePrivateBridgeScript(scriptPath); err != nil {
		return nil, err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("Node.js required by acpx runtime was not found: %w", err)
	}
	if err := requireSupportedNode(ctx, node); err != nil {
		return nil, err
	}

	// The caller context bounds startup only. Worker shutdown is an explicit
	// bridge command so pending permission callbacks are cancelled cleanly;
	// stdin EOF is the crash fallback.
	command := exec.Command(node, scriptPath, packageRoot, stateDirectory)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	promptInput, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open acpx runtime permission input: %w", err)
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		_ = promptInput.Close()
		return nil, fmt.Errorf("open acpx runtime bridge control pipe: %w", err)
	}
	command.ExtraFiles = []*os.File{controlRead}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = promptInput.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, fmt.Errorf("open acpx runtime bridge stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = promptInput.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, fmt.Errorf("open acpx runtime bridge stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = promptInput.Close()
		_ = controlRead.Close()
		_ = controlWrite.Close()
		return nil, fmt.Errorf("start acpx runtime bridge: %w", err)
	}
	_ = controlRead.Close()
	bridge := &runtimeBridge{
		command: command, stdin: controlWrite, promptInput: promptInput,
		pending: make(map[string]chan bridgeMessage), closed: make(chan struct{}), exited: make(chan struct{}),
	}
	ready := make(chan error, 1)
	go bridge.read(stdout, ready)
	go bridge.captureStderr(stderr)
	go bridge.wait()
	timer := time.NewTimer(runtimeBridgeStartupTimeout)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			_ = bridge.closeProcess()
			return nil, err
		}
		return bridge, nil
	case <-timer.C:
		_ = bridge.closeProcess()
		return nil, fmt.Errorf("acpx runtime bridge did not become ready within %s", runtimeBridgeStartupTimeout)
	case <-ctx.Done():
		_ = bridge.closeProcess()
		return nil, ctx.Err()
	}
}

func writePrivateBridgeScript(path string) error {
	temporary := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(temporary, runtimeBridgeSource, 0o600); err != nil {
		return fmt.Errorf("write acpx runtime bridge: %w", err)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("secure acpx runtime bridge: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish acpx runtime bridge: %w", err)
	}
	return nil
}

func installedPackageRoot(binary string) (string, error) {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("resolve acpx binary %q: %w", binary, err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve acpx binary path: %w", err)
	}
	for candidate := filepath.Dir(resolved); ; candidate = filepath.Dir(candidate) {
		manifestPath := filepath.Join(candidate, "package.json")
		body, readErr := os.ReadFile(manifestPath)
		if readErr == nil {
			var manifest struct {
				Name    string         `json:"name"`
				Version string         `json:"version"`
				Exports map[string]any `json:"exports"`
			}
			if json.Unmarshal(body, &manifest) == nil && manifest.Name == "acpx" {
				if manifest.Version != SupportedACPXVersion {
					return "", fmt.Errorf("unsupported acpx runtime package %q; requires %s", manifest.Version, SupportedACPXVersion)
				}
				if _, exported := manifest.Exports["./runtime"]; !exported {
					return "", fmt.Errorf("acpx %s does not export acpx/runtime", manifest.Version)
				}
				return candidate, nil
			}
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return "", fmt.Errorf("acpx runtime package could not be resolved from %q", binary)
}

func requireSupportedNode(ctx context.Context, node string) error {
	command := exec.CommandContext(ctx, node, "--version")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("probe Node.js for acpx runtime: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("unrecognized Node.js version %q", version)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 22 || (major == 22 && minor < 13) {
		return fmt.Errorf("Node.js %s is unsupported; acpx runtime requires Node.js 22.13 or newer", version)
	}
	return nil
}

func (b *runtimeBridge) read(reader io.Reader, ready chan<- error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxStructuredLine)
	readySent := false
	for scanner.Scan() {
		var message bridgeMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			if !readySent {
				ready <- fmt.Errorf("decode acpx runtime bridge startup: %w", err)
				readySent = true
			}
			b.fail(fmt.Errorf("decode acpx runtime bridge output: %w", err))
			return
		}
		if !readySent {
			if message.Type != "ready" {
				ready <- fmt.Errorf("acpx runtime bridge emitted %q before readiness", message.Type)
				readySent = true
				b.fail(fmt.Errorf("invalid acpx runtime bridge startup sequence"))
				return
			}
			ready <- nil
			readySent = true
			continue
		}
		b.mu.Lock()
		channel := b.pending[message.CommandID]
		b.mu.Unlock()
		if channel == nil {
			continue
		}
		select {
		case channel <- message:
		case <-b.closed:
			return
		default:
			b.fail(fmt.Errorf("acpx runtime bridge consumer overflow for command %s", message.CommandID))
			return
		}
	}
	if !readySent {
		ready <- fmt.Errorf("acpx runtime bridge exited before readiness")
	}
	if err := scanner.Err(); err != nil {
		b.fail(fmt.Errorf("read acpx runtime bridge: %w", err))
	} else {
		b.fail(io.EOF)
	}
}

func (b *runtimeBridge) captureStderr(reader io.Reader) {
	buffer := make([]byte, 4096)
	var captured strings.Builder
	for {
		count, err := reader.Read(buffer)
		if count > 0 && captured.Len() < maxBridgeStderrBytes {
			remaining := maxBridgeStderrBytes - captured.Len()
			captured.Write(buffer[:min(count, remaining)])
			b.mu.Lock()
			b.stderr = strings.TrimSpace(captured.String())
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *runtimeBridge) wait() {
	err := b.command.Wait()
	close(b.exited)
	_ = b.stdin.Close()
	_ = b.promptInput.Close()
	if err == nil {
		err = io.EOF
	} else if detail := b.stderrDetail(); detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	b.fail(err)
}

func (b *runtimeBridge) fail(err error) {
	b.mu.Lock()
	if b.err == nil || errors.Is(b.err, io.EOF) {
		b.err = err
	}
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	clear(b.pending)
	b.mu.Unlock()
}

func (b *runtimeBridge) commandID() string {
	return fmt.Sprintf("bridge-%d", b.nextID.Add(1))
}

func (b *runtimeBridge) start(command bridgeCommand) (<-chan bridgeMessage, func(), error) {
	command.CommandID = b.commandID()
	encoded, err := json.Marshal(command)
	if err != nil {
		return nil, nil, err
	}
	if len(encoded) > maxBridgeCommandFrame {
		return nil, nil, fmt.Errorf("acpx runtime bridge command exceeds %d bytes", maxBridgeCommandFrame)
	}
	channel := make(chan bridgeMessage, bridgeMessageQueue)
	b.mu.Lock()
	select {
	case <-b.closed:
		err := b.err
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("acpx runtime bridge is closed: %w", err)
	default:
	}
	b.pending[command.CommandID] = channel
	b.mu.Unlock()
	b.writeMu.Lock()
	_, err = b.stdin.Write(append(encoded, '\n'))
	b.writeMu.Unlock()
	if err != nil {
		b.remove(command.CommandID)
		return nil, nil, fmt.Errorf("send acpx runtime bridge command: %w", err)
	}
	cleanup := func() { b.remove(command.CommandID) }
	return channel, cleanup, nil
}

func (b *runtimeBridge) remove(commandID string) {
	b.mu.Lock()
	delete(b.pending, commandID)
	b.mu.Unlock()
}

func (b *runtimeBridge) call(ctx context.Context, command bridgeCommand, result any) error {
	channel, cleanup, err := b.start(command)
	if err != nil {
		return err
	}
	defer cleanup()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.closed:
			return b.closedError()
		case message := <-channel:
			switch message.Type {
			case "response":
				if result == nil || len(message.Data) == 0 {
					return nil
				}
				if err := json.Unmarshal(message.Data, result); err != nil {
					return fmt.Errorf("decode acpx runtime bridge response: %w", err)
				}
				return nil
			case "error":
				return bridgeRemoteError(message)
			}
		}
	}
}

func (b *runtimeBridge) closedError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		return fmt.Errorf("acpx runtime bridge closed")
	}
	if b.stderr != "" && !strings.Contains(b.err.Error(), b.stderr) {
		return fmt.Errorf("acpx runtime bridge closed: %w: %s", b.err, b.stderr)
	}
	return fmt.Errorf("acpx runtime bridge closed: %w", b.err)
}

func (b *runtimeBridge) stderrDetail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stderr
}

func (b *runtimeBridge) closeProcess() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := b.call(ctx, bridgeCommand{Type: "shutdown"}, nil)
	_ = b.stdin.Close()
	_ = b.promptInput.Close()
	if err == nil {
		select {
		case <-b.exited:
			return nil
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if terminateErr := b.terminateProcess(); err == nil {
		err = terminateErr
	}
	return err
}

func (b *runtimeBridge) terminateProcess() error {
	_ = b.stdin.Close()
	_ = b.promptInput.Close()
	if b.command.Process == nil {
		return nil
	}
	pid := b.command.Process.Pid
	// startRuntimeBridge establishes PGID=PID. Signal that group even when the
	// Node leader has already exited: surviving provider descendants retain the
	// group and remain addressable as -PID after Getpgid(PID) returns ESRCH.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate acpx runtime process group %d: %w", pid, err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-b.exited:
		return nil
	case <-timer.C:
		return fmt.Errorf("acpx runtime process group %d did not exit within 3s", pid)
	}
}

func bridgeRemoteError(message bridgeMessage) error {
	protocolMessage := message.Message
	if message.Code != "" {
		protocolMessage = message.Code + ": " + message.Message
	}
	return &bridgeRemoteCommandError{
		protocol: &ProtocolError{Operation: "runtime bridge", Message: protocolMessage},
		code:     strings.TrimSpace(message.Code), detailCode: strings.TrimSpace(message.DetailCode),
		retryable: message.Retryable, message: message.Message,
	}
}

func bridgeSessionInfo(target Target, payload bridgeSession) (SessionInfo, error) {
	acpxRecordID := firstNonEmptyString(payload.Status.ACPXRecordID, payload.Handle.ACPXRecordID)
	acpSessionID := firstNonEmptyString(payload.Status.BackendSessionID, payload.Handle.BackendSessionID)
	nativeSessionID := firstNonEmptyString(payload.Status.AgentSessionID, payload.Handle.AgentSessionID)
	if acpxRecordID == "" {
		return SessionInfo{}, &ProtocolError{Operation: "runtime session metadata", Message: "missing acpxRecordId"}
	}
	raw, _ := json.Marshal(payload)
	return SessionInfo{
		ConsoleSessionID: target.ConsoleSessionID, ACPXRecordID: acpxRecordID,
		ACPSessionID: acpSessionID, NativeSessionID: nativeSessionID, Raw: raw,
	}, nil
}

func bridgeAgentArgv(profile agents.Profile) []string {
	return append([]string(nil), profile.Argv()...)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
