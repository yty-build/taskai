package cli

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
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestSessionLifecycleThroughBuiltCLIAndDetachedWorker(t *testing.T) {
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
	nativeFixture, err := filepath.Abs(filepath.Join("..", "native", "testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	nativeRecord := filepath.Join(root, "native-record")
	codexHome := filepath.Join(root, "codex-home")
	profile := fmt.Sprintf("name: codex\nprovider: openai\ncommand: %q\nenvironment:\n  CODEX_HOME: %q\n  FAKE_NATIVE_RECORD: %q\n  FAKE_NATIVE_MARKER: lifecycle\n", nativeFixture, codexHome, nativeRecord)
	if err := os.WriteFile(filepath.Join(root, "agents", "codex.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	code, output, stderr = runBinary(root, binary, "session", "start", taskID, "--agent", "codex", "--repo", repo, "--role", "researcher", "--prompt", "Check one hour", "--approve-all")
	if code != 0 || stderr != "" {
		t.Fatalf("session start: code=%d output=%q stderr=%q", code, output, stderr)
	}
	fields = strings.Fields(output)
	if len(fields) < 2 || fields[0] != "Started" {
		t.Fatalf("session start output = %q", output)
	}
	sessionID := fields[1]
	sessionsToStop = append(sessionsToStop, sessionID)

	var detail string
	waitForCLI(t, 4*time.Second, func() bool {
		code, detail, _ = runBinary(root, binary, "session", sessionID)
		return code == 0 && strings.Contains(detail, "Status: waiting") && strings.Contains(detail, "ACP Session ID: acp-reconnected")
	})
	for _, expected := range []string{"Task: " + taskID, "Role: researcher", "Permission: approve-all", "acpx record ID: acpx-record", "Canonical provider conversation: native-reconnected", "Native adapter ID: native-reconnected", "Worker PID:"} {
		if !strings.Contains(detail, expected) {
			t.Errorf("Session detail does not contain %q:\n%s", expected, detail)
		}
	}
	workerClient := worker.NewSocketClient(filepath.Join(root, "workers", sessionID+".sock"))
	waitForCLI(t, 4*time.Second, func() bool {
		status, statusErr := workerClient.Send(context.Background(), worker.Command{Type: "status"})
		return statusErr == nil && status.ActiveRequestID == "" && status.Queued == 0 && status.Status == "waiting"
	})

	conversationPath := filepath.Join(codexHome, "sessions", "2026", "08", "15", "rollout-native-reconnected.jsonl")
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	conversation := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"native-reconnected\",\"cwd\":%q}}\n", canonicalRepo)
	if err := os.WriteFile(conversationPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}
	code, output, stderr = runBinary(root, binary, "session", "attach", sessionID)
	if code != 0 || !strings.Contains(output, "fake native attached") || !strings.Contains(stderr, "provider transcript reload requested") {
		t.Fatalf("native attach: code=%d output=%q stderr=%q", code, output, stderr)
	}
	nativeInvocation, err := os.ReadFile(nativeRecord)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"cwd=" + canonicalRepo, "arg=resume", "arg=native-reconnected", "marker=lifecycle"} {
		if !strings.Contains(string(nativeInvocation), expected+"\n") {
			t.Errorf("native invocation missing %q:\n%s", expected, nativeInvocation)
		}
	}
	code, output, stderr = runBinary(root, binary, "session", "logs", sessionID)
	if code != 0 || stderr != "" || !strings.Contains(output, "provider_transcript_reconciled") {
		t.Fatalf("provider transcript history: code=%d output=%q stderr=%q", code, output, stderr)
	}

	argsLog, err := os.ReadFile(filepath.Join(root, "acpx-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Task " + taskID + ": Investigate BTC edge", "Description: Compare horizons", "Role: researcher", "Repository: " + canonicalRepo, "Check one hour"} {
		if !strings.Contains(string(argsLog), expected) {
			t.Errorf("acpx argv log does not contain %q", expected)
		}
	}

	followContext, stopFollowing := context.WithCancel(context.Background())
	followCommand := exec.CommandContext(followContext, binary, "session", "logs", sessionID, "--follow")
	followCommand.Env = binaryEnvironment(root)
	followStdout, err := followCommand.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := followCommand.Start(); err != nil {
		t.Fatal(err)
	}
	followLines := make(chan string, 100)
	go func() {
		scanner := bufio.NewScanner(followStdout)
		for scanner.Scan() {
			followLines <- scanner.Text()
		}
	}()
	waitForCLI(t, 2*time.Second, func() bool { return len(followLines) > 0 })

	code, output, stderr = runBinary(root, binary, "session", "prompt", sessionID, "Follow up")
	if code != 0 || stderr != "" || !strings.Contains(output, "Accepted REQ-") {
		t.Fatalf("session prompt: code=%d output=%q stderr=%q", code, output, stderr)
	}
	waitForCLI(t, 4*time.Second, func() bool {
		code, output, _ = runBinary(root, binary, "session", "logs", sessionID)
		return code == 0 && strings.Contains(output, "turn completed") && strings.Contains(output, "ok")
	})
	waitForCLI(t, 2*time.Second, func() bool {
		for len(followLines) > 0 {
			if strings.Contains(<-followLines, "ok") {
				return true
			}
		}
		return false
	})
	stopFollowing()
	_ = followCommand.Wait()
	if !strings.Contains(output, "ok") {
		t.Fatalf("normalized logs do not contain agent output: %s", output)
	}

	code, rawOutput, stderr := runBinary(root, binary, "session", "logs", sessionID, "--raw", "--from", "0")
	if code != 0 || stderr != "" {
		t.Fatalf("raw logs: code=%d stderr=%q", code, stderr)
	}
	var rawLines int
	for _, line := range strings.Split(strings.TrimSpace(rawOutput), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("invalid raw JSONL output: %q", line)
		}
		rawLines++
	}
	if rawLines < 8 || !strings.Contains(rawOutput, `"request_id":"REQ-`) {
		t.Fatalf("raw logs missing correlated history (%d lines): %s", rawLines, rawOutput)
	}

	code, output, stderr = runBinary(root, binary, "task", "logs", taskID)
	if code != 0 || stderr != "" || !strings.Contains(output, sessionID) || !strings.Contains(output, "turn completed") {
		t.Fatalf("task logs: code=%d output=%q stderr=%q", code, output, stderr)
	}

	code, output, stderr = runBinary(root, binary, "session", "stop", sessionID)
	if code != 0 || stderr != "" || output != "Stopped "+sessionID+"\n" {
		t.Fatalf("session stop: code=%d output=%q stderr=%q", code, output, stderr)
	}
	code, fallback, stderr := runBinary(root, binary, "session", "logs", sessionID, "--raw")
	if code != 0 || stderr != "" || !strings.HasPrefix(fallback, rawOutput) || !strings.Contains(fallback, `"type":"worker_stopping"`) {
		t.Fatalf("stopped JSONL fallback is incomplete: code=%d stderr=%q\nprior=%q\ngot=%q", code, stderr, rawOutput, fallback)
	}

	code, output, stderr = runBinary(root, binary, "session", "review", sessionID, "--agent", "claude")
	if code != 0 || stderr != "" {
		t.Fatalf("session review: code=%d output=%q stderr=%q", code, output, stderr)
	}
	fields = strings.Fields(output)
	if len(fields) < 6 || fields[0] != "Started" || fields[1] != "reviewer" || fields[3] != "for" || fields[4] != sessionID {
		t.Fatalf("review output = %q", output)
	}
	reviewerID := fields[2]
	sessionsToStop = append(sessionsToStop, reviewerID)
	reviewerWorker := worker.NewSocketClient(filepath.Join(root, "workers", reviewerID+".sock"))
	waitForCLI(t, 4*time.Second, func() bool {
		code, detail, _ = runBinary(root, binary, "session", reviewerID)
		status, statusErr := reviewerWorker.Send(context.Background(), worker.Command{Type: "status"})
		return code == 0 && strings.Contains(detail, "Role: reviewer") && strings.Contains(detail, "Status: waiting") &&
			statusErr == nil && status.ActiveRequestID == "" && status.Queued == 0
	})
	if !strings.Contains(detail, "Task: "+taskID) || !strings.Contains(detail, "Repo: "+canonicalRepo) {
		t.Fatalf("reviewer detail = %q", detail)
	}
	argsLog, err = os.ReadFile(filepath.Join(root, "acpx-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Producer Session ID: " + sessionID, "Producer JSONL log (read-only evidence): " + filepath.Join(root, "logs", sessionID+".jsonl"),
		"independent, skeptical review", "ignored test failures", "selective tests presented as complete tests", "security regressions",
	} {
		if !strings.Contains(string(argsLog), expected) {
			t.Errorf("review prompt argv missing %q", expected)
		}
	}
	code, output, stderr = runBinary(root, binary, "task", taskID)
	if code != 0 || stderr != "" || !strings.Contains(output, reviewerID) || !strings.Contains(output, "reviewer") {
		t.Fatalf("Task reviewer aggregate: code=%d output=%q stderr=%q", code, output, stderr)
	}
	code, _, stderr = runBinary(root, binary, "session", "stop", reviewerID)
	if code != 0 || stderr != "" {
		t.Fatalf("stop reviewer: code=%d stderr=%q", code, stderr)
	}

	code, output, stderr = runBinary(root, binary, "session", "resume", sessionID)
	if code != 0 || stderr != "" || output != "Resumed "+sessionID+"\n" {
		t.Fatalf("session resume: code=%d output=%q stderr=%q", code, output, stderr)
	}
	argsLog, err = os.ReadFile(filepath.Join(root, "acpx-args.log"))
	if err != nil || !strings.Contains(string(argsLog), "--resume-session\nacp-reconnected") {
		t.Fatalf("resume did not target prior ACP Session: err=%v\n%s", err, argsLog)
	}
	code, _, stderr = runBinary(root, binary, "session", "stop", sessionID)
	if code != 0 || stderr != "" {
		t.Fatalf("final stop: code=%d stderr=%q", code, stderr)
	}
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
