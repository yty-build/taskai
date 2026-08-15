package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	info := buildinfo.Info{Version: "1.2.3", Commit: "abc", Date: "today"}
	code := Run(context.Background(), []string{"version"}, &stdout, &stderr, info)
	if code != ExitOK || stdout.String() != "agent 1.2.3 (commit abc, built today)\n" {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
}

func TestRunUnknownCommandHasStableError(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"wat"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if got, want := stderr.String(), "agent: unknown command \"wat\"; run 'agent help' for usage\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestTaskCommandLifecycle(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	t.Setenv("AGENT_HOME", home)

	code, stdout, stderr := runForTest(t, "task", "new", "Research BTC", "--description", "Compare horizons", "--external-ref=PM-42", "--metadata-json", `{"market":"btc"}`)
	if code != ExitOK || stderr != "" {
		t.Fatalf("task new: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) != 2 || fields[0] != "Created" {
		t.Fatalf("task new output = %q", stdout)
	}
	taskID := fields[1]

	code, stdout, stderr = runForTest(t, "tasks")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, taskID) || !strings.Contains(stdout, "open") || !strings.Contains(stdout, "Research BTC") {
		t.Fatalf("tasks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, stdout, stderr = runForTest(t, "task", taskID)
	for _, expected := range []string{"Research BTC", "Description: Compare horizons", "External ref: PM-42", "No Sessions."} {
		if !strings.Contains(stdout, expected) {
			t.Errorf("task show output %q does not contain %q", stdout, expected)
		}
	}
	if code != ExitOK || stderr != "" {
		t.Fatalf("task show: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr = runForTest(t, "task", "status", taskID, "review")
	if code != ExitOK || stderr != "" || stdout != "Updated "+taskID+" to review\n" {
		t.Fatalf("task status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	code, _, stderr = runForTest(t, "task", "status", taskID, "running")
	if code != ExitUsage || !strings.Contains(stderr, "invalid Task status") {
		t.Fatalf("invalid status: code=%d stderr=%q", code, stderr)
	}
}

func TestStartCreatesTaskAndSession(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	repo := filepath.Join(t.TempDir(), "repo")
	code, stdout, stderr := runForTest(t, "start", "Fix login", "--agent", "codex", "--repo", repo, "--role", "implementer", "--description", "Production regression")
	if code != ExitOK || stderr != "" {
		t.Fatalf("start: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	fields := strings.Fields(stdout)
	if len(fields) != 4 || fields[0] != "Created" || fields[2] != "Started" {
		t.Fatalf("start output = %q", stdout)
	}
	taskID, sessionID := fields[1], fields[3]
	code, stdout, stderr = runForTest(t, "task", taskID)
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, sessionID) || !strings.Contains(stdout, "implementer") || !strings.Contains(stdout, "codex") || !strings.Contains(stdout, "starting") {
		t.Fatalf("task show: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestStartValidationIsAtomic(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	code, _, stderr := runForTest(t, "start", "Invalid", "--agent", "", "--repo", ".")
	if code != ExitUsage || !strings.Contains(stderr, "usage: agent start") {
		t.Fatalf("start: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "tasks")
	if code != ExitOK || stdout != "No Tasks.\n" || stderr != "" {
		t.Fatalf("tasks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr, buildinfo.Info{Version: "test"})
	return code, stdout.String(), stderr.String()
}
