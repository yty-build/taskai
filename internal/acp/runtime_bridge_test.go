package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

func TestTerminateRuntimeBridgeAwaitsProviderProcessGroup(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command("sh", "-c", `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait`, "bridge-test", childPIDPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	bridge := &runtimeBridge{
		command: command, stdin: discardWriteCloser{io.Discard}, promptInput: discardWriteCloser{io.Discard},
		pending: make(map[string]chan bridgeMessage), closed: make(chan struct{}), exited: make(chan struct{}),
	}
	go bridge.wait()

	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(childPIDPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		t.Fatal("provider child PID was not recorded")
	}
	if err := bridge.terminateProcess(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bridge.exited:
	case <-time.After(time.Second):
		t.Fatal("bridge parent was not reaped")
	}
	for deadline := time.Now().Add(2 * time.Second); ; {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider child %d remained after process-group teardown: %v", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminateRuntimeBridgeKillsDescendantAfterLeaderExited(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "orphan-child.pid")
	command := exec.Command("sh", "-c", `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; exit 0`, "bridge-test", childPIDPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	bridge := &runtimeBridge{
		command: command, stdin: discardWriteCloser{io.Discard}, promptInput: discardWriteCloser{io.Discard},
		pending: make(map[string]chan bridgeMessage), closed: make(chan struct{}), exited: make(chan struct{}),
	}
	go bridge.wait()

	childPID := waitForRecordedPID(t, childPIDPath)
	select {
	case <-bridge.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge leader did not exit")
	}
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("provider child did not survive leader long enough to test teardown: %v", err)
	}
	if err := bridge.terminateProcess(); err != nil {
		t.Fatal(err)
	}
	waitForProcessGone(t, childPID)
}

func waitForRecordedPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("provider child PID was not recorded")
	return 0
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider child %d remained after process-group teardown: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBlockedRuntimeBridgeReplacementFailsClosed(t *testing.T) {
	client := &RuntimeClient{blocked: fmt.Errorf("old provider did not exit")}
	_, err := client.bridgeFor(context.Background(), Target{})
	if err == nil || !strings.Contains(err.Error(), "replacement is blocked") {
		t.Fatalf("bridgeFor() blocked error = %v", err)
	}
}
