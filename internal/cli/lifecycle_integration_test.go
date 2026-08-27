package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestSessionChannelLifecycleThroughBuiltCLIAndDetachedWorker(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "atc-cli-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	var sessionsToStop []string
	t.Cleanup(func() {
		for _, sessionID := range sessionsToStop {
			_, _, _ = runBinary(root, filepath.Join(root, "acpctl"), "session", "stop", sessionID)
		}
		_ = os.RemoveAll(root)
	})
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeCLIRepository(t, repo)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "acpctl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/acpctl")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build acpctl: %v\n%s", err, output)
	}

	code, output, stderr := runBinary(root, binary, "task", "new", "Investigate BTC edge", "--description", "Compare horizons")
	if code != 0 || stderr != "" {
		t.Fatalf("task new: code=%d output=%q stderr=%q", code, output, stderr)
	}
	fields := strings.Fields(output)
	if len(fields) != 2 {
		t.Fatalf("task new output = %q", output)
	}
	taskID := fields[1]
	profile := "name: codex\nprovider: openai\ncommand: codex\n"
	if err := os.WriteFile(filepath.Join(root, "agents", "codex.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr = runBinary(root, binary, "session", "start", taskID, "--agent", "codex", "--repo", repo, "--role", "researcher", "--approve-all")
	if code != 0 || stderr != "" {
		t.Fatalf("session start: code=%d output=%q stderr=%q", code, output, stderr)
	}
	fields = strings.Fields(output)
	if len(fields) != 3 || fields[0] != "Connected" || fields[2] != "(starting)" {
		t.Fatalf("session start output = %q", output)
	}
	sessionID := fields[1]
	sessionsToStop = append(sessionsToStop, sessionID)

	var detail string
	waitForCLI(t, 5*time.Second, func() bool {
		code, detail, _ = runBinary(root, binary, "session", sessionID)
		return code == 0 && strings.Contains(detail, "Status: waiting") && strings.Contains(detail, `Provider reference: "acp-reconnected"`)
	})
	for _, expected := range []string{
		"Task: " + taskID, "Role: researcher", "Permission: approve-all", `Provider reference: "acp-reconnected"`, "Worker PID:",
	} {
		if !strings.Contains(detail, expected) {
			t.Errorf("Session detail does not contain %q:\n%s", expected, detail)
		}
	}
	for _, obsolete := range []string{"Canonical provider conversation", "Native adapter ID", "Identity source"} {
		if strings.Contains(detail, obsolete) {
			t.Errorf("Session detail contains obsolete %q:\n%s", obsolete, detail)
		}
	}

	workerClient := worker.NewSocketClient(filepath.Join(root, "workers", sessionID+".sock"))
	waitForCLI(t, 5*time.Second, func() bool {
		status, statusErr := workerClient.Send(context.Background(), worker.Command{Type: "status"})
		return statusErr == nil && status.ActiveRequestID == "" && status.Status == "waiting"
	})

	code, output, stderr = runBinary(root, binary, "session", "prompt", sessionID, "Check one hour")
	if code != 0 || stderr != "" || !strings.Contains(output, "Accepted REQ-") {
		t.Fatalf("initial explicit prompt: code=%d output=%q stderr=%q", code, output, stderr)
	}
	waitForCLI(t, 5*time.Second, func() bool {
		status, statusErr := workerClient.Send(context.Background(), worker.Command{Type: "status"})
		return statusErr == nil && status.ActiveRequestID == "" && status.Status == "waiting"
	})

	argsLog, err := os.ReadFile(filepath.Join(root, "acpx-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argsLog), "Check one hour") {
		t.Fatalf("provider prompt did not contain the requested instructions:\n%s", argsLog)
	}
	for _, injected := range []string{"Task " + taskID, "Description: Compare horizons", "Role: researcher", "Repository: " + canonicalRepo} {
		if strings.Contains(string(argsLog), injected) {
			t.Errorf("provider prompt contains gateway-injected %q:\n%s", injected, argsLog)
		}
	}

	waitForCLI(t, 5*time.Second, func() bool {
		code, output, _ = runBinary(root, binary, "session", "logs", sessionID, "--raw")
		return code == 0 && strings.Contains(output, `"type":"client_command"`) &&
			strings.Contains(output, `"type":"provider_event"`) && strings.Contains(output, `"type":"provider_result"`)
	})
	if !strings.Contains(output, `"text":"ok"`) {
		t.Fatalf("initial channel replay did not contain streamed provider output: %s", output)
	}

	code, output, stderr = runBinary(root, binary, "session", "prompt", sessionID, "Follow up")
	if code != 0 || stderr != "" || !strings.Contains(output, "Accepted REQ-") || strings.Contains(strings.ToLower(output), "queued") {
		t.Fatalf("session prompt: code=%d output=%q stderr=%q", code, output, stderr)
	}
	waitForCLI(t, 5*time.Second, func() bool {
		code, output, _ = runBinary(root, binary, "session", "logs", sessionID, "--raw")
		return code == 0 && strings.Count(output, `"type":"provider_result"`) >= 2
	})

	code, taskOutput, stderr := runBinary(root, binary, "task", "logs", taskID, "--raw")
	if code != 0 || stderr != "" || !strings.Contains(taskOutput, sessionID) || !strings.Contains(taskOutput, `"type":"provider_result"`) {
		t.Fatalf("task logs: code=%d output=%q stderr=%q", code, taskOutput, stderr)
	}

	code, output, stderr = runBinary(root, binary, "session", "stop", sessionID)
	if code != 0 || stderr != "" || output != "Stopped "+sessionID+"\n" {
		t.Fatalf("session stop: code=%d output=%q stderr=%q", code, output, stderr)
	}
	waitForCLI(t, 5*time.Second, func() bool {
		code, output, _ = runBinary(root, binary, "session", "logs", sessionID, "--raw")
		return code == 0 && strings.Contains(output, `"type":"connection"`) && strings.Contains(output, `"state":"stopped"`)
	})

	code, output, stderr = runBinary(root, binary, "session", "resume", sessionID)
	if code != 0 || stderr != "" || output != "Resumed "+sessionID+"\n" {
		t.Fatalf("session resume: code=%d output=%q stderr=%q", code, output, stderr)
	}
	waitForCLI(t, 5*time.Second, func() bool {
		argsLog, err = os.ReadFile(filepath.Join(root, "acpx-args.log"))
		return err == nil && strings.Contains(string(argsLog), "--resume-session\nacp-reconnected")
	})
}

func runBinary(root, binary string, args ...string) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = binaryEnvironment(root)
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}
	return -1, stdout.String(), fmt.Sprintf("%s%s", stderr.String(), err)
}

func binaryEnvironment(root string) []string {
	fixture, _ := filepath.Abs(filepath.Join("..", "acp", "testdata", "fake-acpx"))
	return append(os.Environ(),
		"AGENT_HOME="+root,
		"ACPX_BINARY="+fixture,
		"FAKE_ACPX_SCENARIO=success",
		"FAKE_ACPX_ARGS_LOG="+filepath.Join(root, "acpx-args.log"),
	)
}

func waitForCLI(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("CLI condition did not become true")
}
