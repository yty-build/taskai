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

func TestRuntimeBridgeReaderBackpressuresOrdinaryProviderBurst(t *testing.T) {
	pending := &bridgePending{messages: make(chan bridgeMessage, bridgeMessageQueue), done: make(chan struct{})}
	bridge := &runtimeBridge{
		pending: map[string]*bridgePending{"bridge-burst": pending},
		closed:  make(chan struct{}), exited: make(chan struct{}),
	}
	var input strings.Builder
	input.WriteString(`{"type":"ready"}` + "\n")
	for index := 0; index < bridgeMessageQueue+8; index++ {
		fmt.Fprintf(&input, `{"type":"runtime_event","commandId":"bridge-burst","requestId":"REQ","event":{"type":"text_delta","text":"%d"}}`+"\n", index)
	}
	ready := make(chan error, 1)
	go bridge.read(strings.NewReader(input.String()), ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < bridgeMessageQueue+8; index++ {
		select {
		case message := <-pending.messages:
			if message.Type != "runtime_event" || message.CommandID != "bridge-burst" {
				t.Fatalf("burst message %d = %#v", index, message)
			}
		case <-time.After(time.Second):
			t.Fatalf("provider burst stalled at message %d", index)
		}
	}
	select {
	case <-bridge.closed:
	case <-time.After(time.Second):
		t.Fatal("finite bridge input did not close")
	}
	bridge.mu.Lock()
	err := bridge.err
	bridge.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("bridge burst terminal error = %v", err)
	}
}

func TestRuntimeBridgeCommandCleanupUnblocksBackpressuredReader(t *testing.T) {
	pending := &bridgePending{messages: make(chan bridgeMessage, bridgeMessageQueue), done: make(chan struct{})}
	bridge := &runtimeBridge{
		pending: map[string]*bridgePending{"bridge-abandoned": pending},
		closed:  make(chan struct{}), exited: make(chan struct{}),
	}
	var input strings.Builder
	input.WriteString(`{"type":"ready"}` + "\n")
	for index := 0; index < bridgeMessageQueue+8; index++ {
		fmt.Fprintf(&input, `{"type":"runtime_event","commandId":"bridge-abandoned","event":{"type":"status","index":%d}}`+"\n", index)
	}
	ready := make(chan error, 1)
	go bridge.read(strings.NewReader(input.String()), ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(pending.messages) != bridgeMessageQueue {
		if time.Now().After(deadline) {
			t.Fatalf("bridge queue did not fill: %d", len(pending.messages))
		}
		time.Sleep(time.Millisecond)
	}
	bridge.remove("bridge-abandoned")
	select {
	case <-bridge.closed:
	case <-time.After(time.Second):
		t.Fatal("abandoned command did not release the bridge reader")
	}
	bridge.mu.Lock()
	err := bridge.err
	bridge.mu.Unlock()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("abandoned command terminal error = %v", err)
	}
}

func TestTerminateRuntimeBridgeAwaitsProviderProcessGroup(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command("sh", "-c", `sleep 30 & child=$!; printf '%s\n' "$child" > "$1"; wait`, "bridge-test", childPIDPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	bridge := &runtimeBridge{
		command: command, stdin: discardWriteCloser{io.Discard}, promptInput: discardWriteCloser{io.Discard},
		pending: make(map[string]*bridgePending), closed: make(chan struct{}), exited: make(chan struct{}),
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
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("provider child %d remained when process-group teardown returned: %v", childPID, err)
	}
}

func TestCloseRuntimeBridgeAcceptsProvenForcedTeardownAfterGracefulFailure(t *testing.T) {
	command := exec.Command("sh", "-c", "sleep 30")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	close(closed)
	bridge := &runtimeBridge{
		command: command, stdin: discardWriteCloser{io.Discard}, promptInput: discardWriteCloser{io.Discard},
		pending: make(map[string]*bridgePending), closed: closed, exited: make(chan struct{}), err: errors.New("fixture bridge failure"),
	}
	go bridge.wait()
	if err := bridge.closeProcess(); err != nil {
		t.Fatalf("closeProcess() after proven forced teardown = %v", err)
	}
	select {
	case <-bridge.exited:
	case <-time.After(time.Second):
		t.Fatal("forced teardown did not reap bridge")
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
		pending: make(map[string]*bridgePending), closed: make(chan struct{}), exited: make(chan struct{}),
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
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("orphan provider child %d remained when process-group teardown returned: %v", childPID, err)
	}
}

func TestCloseRuntimeBridgeVerifiesProcessGroupAfterGracefulLeaderExit(t *testing.T) {
	childPIDPath := filepath.Join(t.TempDir(), "graceful-orphan-child.pid")
	command := exec.Command("sh", "-c", `
sleep 30 & child=$!
printf '%s\n' "$child" > "$1"
printf '%s\n' '{"type":"ready"}'
IFS= read -r _
printf '%s\n' '{"type":"response","commandId":"bridge-1"}'
sleep 0.1
exit 0
`, "bridge-test", childPIDPath)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	bridge := &runtimeBridge{
		command: command, stdin: stdin, promptInput: discardWriteCloser{io.Discard},
		pending: make(map[string]*bridgePending), closed: make(chan struct{}), exited: make(chan struct{}),
	}
	go bridge.wait()
	ready := make(chan error, 1)
	go bridge.read(stdout, ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	childPID := waitForRecordedPID(t, childPIDPath)
	if err := bridge.closeProcess(); err != nil {
		t.Fatalf("graceful close with process-group verification = %v", err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("provider child %d survived successful graceful close: %v", childPID, err)
	}
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

func TestBlockedRuntimeBridgeReplacementFailsClosed(t *testing.T) {
	client := &RuntimeClient{blocked: fmt.Errorf("old provider did not exit")}
	_, err := client.bridgeFor(context.Background(), Target{})
	if err == nil || !strings.Contains(err.Error(), "replacement is blocked") {
		t.Fatalf("bridgeFor() blocked error = %v", err)
	}
}

func TestBridgeSessionInfoRequiresBoundedProviderReference(t *testing.T) {
	target, payload := validBridgeSessionPayload(t, "S-bridge")
	valid := strings.Repeat("p", maxProviderRefBytes)
	payload.Handle.BackendSessionID = valid
	info, err := bridgeSessionInfo(target, payload)
	if err != nil || info.ProviderRef != valid {
		t.Fatalf("bounded bridge Session info = %#v, %v", info, err)
	}

	for name, providerRef := range map[string]string{
		"missing":   "",
		"oversized": strings.Repeat("p", maxProviderRefBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			candidate := payload
			candidate.Handle.BackendSessionID = providerRef
			_, err := bridgeSessionInfo(target, candidate)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) {
				t.Fatalf("bridgeSessionInfo(%q) error = %T %v, want ProtocolError", providerRef, err, err)
			}
		})
	}
	payload.Handle.BackendSessionID = " provider-1 "
	spaced, err := bridgeSessionInfo(target, payload)
	if err != nil || spaced.ProviderRef != " provider-1 " {
		t.Fatalf("opaque whitespace provider reference = %#v, %v", spaced, err)
	}
}

func TestBridgeSessionInfoUsesLiveHandleIdentityAndRejectsStatusDrift(t *testing.T) {
	target, matching := validBridgeSessionPayload(t, "S-bridge-identity")
	matching.Handle.BackendSessionID = " provider-1 "
	matching.Status = bridgeStatus{ACPXRecordID: "record-1", BackendSessionID: " provider-1 "}
	info, err := bridgeSessionInfo(target, matching)
	if err != nil || info.RuntimeRef != "record-1" || info.ProviderRef != " provider-1 " {
		t.Fatalf("matching literal identities = %#v, %v", info, err)
	}

	runtimeDrift := matching
	runtimeDrift.Handle.ACPXRecordID = "record-live"
	runtimeDrift.Status = bridgeStatus{ACPXRecordID: "record-other", BackendSessionID: " provider-1 "}
	providerDrift := matching
	providerDrift.Handle.BackendSessionID = "provider-live"
	providerDrift.Status = bridgeStatus{ACPXRecordID: "record-1", BackendSessionID: "provider-other"}
	providerWhitespaceDrift := matching
	providerWhitespaceDrift.Status.BackendSessionID = "provider-1"
	statusOnly := matching
	statusOnly.Handle = bridgeHandle{}
	for name, payload := range map[string]bridgeSession{
		"runtime":                     runtimeDrift,
		"provider":                    providerDrift,
		"provider_literal_whitespace": providerWhitespaceDrift,
		"status_only":                 statusOnly,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := bridgeSessionInfo(target, payload)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) {
				t.Fatalf("identity drift error = %T %v, want ProtocolError", err, err)
			}
		})
	}
}

func TestBridgeSessionInfoRequiresExactTargetBindingAndBoundedRuntimeRef(t *testing.T) {
	target, valid := validBridgeSessionPayload(t, "S-bridge-target")
	// The bridge may report an equivalent non-canonical spelling, but it must
	// resolve to the exact checkout already bound to the RuntimeClient target.
	valid.Handle.CWD += string(os.PathSeparator) + "."
	valid.Handle.ACPXRecordID = strings.Repeat("r", maxRuntimeRefBytes)
	info, err := bridgeSessionInfo(target, valid)
	if err != nil || info.ConsoleSessionID != target.ConsoleSessionID || info.RuntimeRef != valid.Handle.ACPXRecordID {
		t.Fatalf("valid exact bridge target = %#v, %v", info, err)
	}

	tests := map[string]func(*bridgeSession){
		"missing_session_key": func(payload *bridgeSession) { payload.Handle.SessionKey = "" },
		"wrong_session_key":   func(payload *bridgeSession) { payload.Handle.SessionKey = "S-other" },
		"missing_cwd":         func(payload *bridgeSession) { payload.Handle.CWD = "" },
		"wrong_cwd":           func(payload *bridgeSession) { payload.Handle.CWD = t.TempDir() },
		"missing_runtime_ref": func(payload *bridgeSession) { payload.Handle.ACPXRecordID = "" },
		"spaced_runtime_ref":  func(payload *bridgeSession) { payload.Handle.ACPXRecordID = " record-1 " },
		"oversized_runtime_ref": func(payload *bridgeSession) {
			payload.Handle.ACPXRecordID = strings.Repeat("r", maxRuntimeRefBytes+1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			payload := valid
			mutate(&payload)
			info, err := bridgeSessionInfo(target, payload)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || info.ConsoleSessionID != "" || info.RuntimeRef != "" || info.ProviderRef != "" || len(info.Raw) != 0 {
				t.Fatalf("invalid bridge target = %#v, %T %v; want empty info and ProtocolError", info, err, err)
			}
		})
	}
}

func validBridgeSessionPayload(t *testing.T, sessionID string) (Target, bridgeSession) {
	t.Helper()
	workingDirectory := t.TempDir()
	return Target{ConsoleSessionID: sessionID, WorkingDirectory: workingDirectory}, bridgeSession{
		Handle: bridgeHandle{
			SessionKey: sessionID, ACPXRecordID: "record-1", BackendSessionID: "provider-1", CWD: workingDirectory,
		},
	}
}
