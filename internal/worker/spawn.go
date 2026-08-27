package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type SpawnResult struct {
	PID       int
	Socket    string
	PortReady bool
}

const (
	workerStartupTimeout   = 30 * time.Second
	startupLogTailBytes    = 1 << 20
	maxStartupErrorMessage = 8 << 10
)

// Spawn launches the hidden worker in a new session and completes its private
// channel-admission handshake. The worker cannot begin provider Start/Resume
// until Spawn has received admission_ready and acknowledged it, so unrelated
// observational pings cannot race the initiating create/Resume response.
func Spawn(ctx context.Context, executable string, home config.Home, sessionID string) (SpawnResult, error) {
	if executable == "" {
		return SpawnResult{}, fmt.Errorf("acpctl executable path is required")
	}
	socket := filepath.Join(home.Workers, sessionID+".sock")
	probeContext, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	probeErr := NewSocketClient(socket).Ping(probeContext, sessionID)
	probeCancel()
	if probeErr == nil {
		return SpawnResult{}, ErrAlreadyRunning
	}

	command := exec.Command(executable, "_worker", "--session", sessionID)
	command.Env = withEnvironment(os.Environ(), "AGENT_HOME", home.Root)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return SpawnResult{}, fmt.Errorf("open null device: %w", err)
	}
	defer null.Close()
	command.Stdin, command.Stdout, command.Stderr = null, null, null
	if err := command.Start(); err != nil {
		return SpawnResult{}, fmt.Errorf("start detached Session worker: %w", err)
	}
	pid := command.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	deadline := time.NewTimer(workerStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			if err == nil {
				err = fmt.Errorf("worker exited before becoming ready")
			}
			if detail := latestWorkerStartupError(sessionlog.ChannelPath(home.Logs, sessionID)); detail != "" {
				return SpawnResult{}, fmt.Errorf("Session worker %d failed to start: %s", pid, detail)
			}
			return SpawnResult{}, fmt.Errorf("Session worker %d failed to start: %w", pid, err)
		case <-deadline.C:
			teardownErr := terminateSpawnedWorker(command, exited)
			if detail := latestWorkerStartupError(sessionlog.ChannelPath(home.Logs, sessionID)); detail != "" {
				if teardownErr != nil {
					return SpawnResult{}, fmt.Errorf("Session worker %d did not become ready at %s: %s; teardown: %w", pid, socket, detail, teardownErr)
				}
				return SpawnResult{}, fmt.Errorf("Session worker %d did not become ready at %s: %s", pid, socket, detail)
			}
			if teardownErr != nil {
				return SpawnResult{}, fmt.Errorf("Session worker %d did not become ready at %s and did not terminate safely: %w", pid, socket, teardownErr)
			}
			return SpawnResult{}, fmt.Errorf("Session worker %d did not become ready at %s", pid, socket)
		case <-ticker.C:
			// Once the detached worker exists, a disposable caller/viewer context
			// no longer owns its lifetime. Bootstrap has its own bounded deadline.
			admissionContext, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			message, err := NewSocketClient(socket).Admit(admissionContext, sessionID)
			cancel()
			if err == nil {
				return SpawnResult{PID: pid, Socket: socket, PortReady: message.State != string(session.StatusStarting)}, nil
			}
		}
	}
}

func terminateSpawnedWorker(command *exec.Cmd, exited <-chan error) error {
	if command == nil || command.Process == nil {
		return nil
	}
	pid := command.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate Session worker process group %d: %w", pid, err)
	}
	grace := time.NewTimer(providerShutdownTimeout + time.Second)
	graceTicker := time.NewTicker(10 * time.Millisecond)
	leaderExited := false
	groupGone := false
	graceful := true
	for graceful && (!leaderExited || !groupGone) {
		select {
		case <-exited:
			leaderExited = true
			exited = nil
		case <-graceTicker.C:
			err := syscall.Kill(-pid, 0)
			switch {
			case errors.Is(err, syscall.ESRCH):
				groupGone = true
			case err == nil, errors.Is(err, syscall.EPERM):
			default:
				graceTicker.Stop()
				grace.Stop()
				return fmt.Errorf("verify Session worker process group %d teardown: %w", pid, err)
			}
		case <-grace.C:
			graceful = false
		}
	}
	graceTicker.Stop()
	if !grace.Stop() {
		select {
		case <-grace.C:
		default:
		}
	}
	if leaderExited && groupGone {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("force terminate Session worker process group %d: %w", pid, err)
	}
	forced := time.NewTimer(2 * time.Second)
	defer forced.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !leaderExited || !groupGone {
		select {
		case <-exited:
			leaderExited = true
			exited = nil
		case <-ticker.C:
			err := syscall.Kill(-pid, 0)
			switch {
			case errors.Is(err, syscall.ESRCH):
				groupGone = true
			case err == nil, errors.Is(err, syscall.EPERM):
			default:
				return fmt.Errorf("verify Session worker process group %d teardown: %w", pid, err)
			}
		case <-forced.C:
			return fmt.Errorf("Session worker process group %d did not terminate and reap within 2s", pid)
		}
	}
	return nil
}

func latestWorkerStartupError(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - startupLogTailBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, 0); err != nil {
		return ""
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), startupLogTailBytes)
	if start > 0 {
		// The seek can land in a JSONL record; discard that partial record.
		scanner.Scan()
	}
	var latest string
	for scanner.Scan() {
		var line struct {
			Type string `json:"type"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Type == "worker_error" {
			latest = strings.TrimSpace(line.Data.Message)
		}
	}
	if len(latest) > maxStartupErrorMessage {
		latest = latest[:maxStartupErrorMessage] + "..."
	}
	return latest
}

func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func ReadMetadata(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, fmt.Errorf("decode worker metadata: %w", err)
	}
	return metadata, nil
}

func withEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}
