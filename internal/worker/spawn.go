package worker

import (
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
)

type SpawnResult struct {
	PID    int
	Socket string
}

// Spawn launches the hidden worker in a new session and waits until its socket
// answers ping. The worker is not tied to the caller's terminal lifetime.
func Spawn(ctx context.Context, executable string, home config.Home, sessionID string) (SpawnResult, error) {
	if executable == "" {
		return SpawnResult{}, fmt.Errorf("agent executable path is required")
	}
	socket := filepath.Join(home.Workers, sessionID+".sock")
	probeContext, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	probeErr := NewSocketClient(socket).Ping(probeContext)
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

	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = command.Process.Signal(syscall.SIGTERM)
			return SpawnResult{}, ctx.Err()
		case err := <-exited:
			if err == nil {
				err = fmt.Errorf("worker exited before becoming ready")
			}
			return SpawnResult{}, fmt.Errorf("Session worker %d failed to start: %w", pid, err)
		case <-deadline.C:
			_ = command.Process.Signal(syscall.SIGTERM)
			return SpawnResult{}, fmt.Errorf("Session worker %d did not become ready at %s", pid, socket)
		case <-ticker.C:
			pingContext, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			err := NewSocketClient(socket).Ping(pingContext)
			cancel()
			if err == nil {
				return SpawnResult{PID: pid, Socket: socket}, nil
			}
		}
	}
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
