package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
)

func fixtureClient(t *testing.T, scenario string) (*Client, Target) {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-acpx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_ACPX_SCENARIO", scenario)
	client := NewClient(fixture)
	if err := client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	target := Target{
		ConsoleSessionID: "S-fixture", WorkingDirectory: t.TempDir(), Permission: PermissionReadApproval,
		Profile: agents.Profile{Name: "codex", Provider: "openai", Command: "codex"},
	}
	return client, target
}

func TestProbePinsVersion(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "fake-acpx"))
	t.Setenv("FAKE_ACPX_VERSION", "0.14.0")
	client := NewClient(fixture)
	err := client.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires 0.13.0") {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestOperationsRequireProbe(t *testing.T) {
	client := NewClient("unused")
	_, err := client.Start(context.Background(), StartSpec{Target: Target{}})
	if err == nil || !strings.Contains(err.Error(), "has not been probed") {
		t.Fatalf("Start() error = %v", err)
	}
}

func TestStartInspectStatusCancelAndResume(t *testing.T) {
	client, target := fixtureClient(t, "success")
	info, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if info.ACPXRecordID != "acpx-record" || info.ACPSessionID != "acp-session" || info.NativeSessionID != "native-session" {
		t.Fatalf("Start() = %#v", info)
	}
	inspected, err := client.Inspect(context.Background(), target)
	if err != nil || inspected.ACPSessionID != "acp-reconnected" || inspected.NativeSessionID != "native-reconnected" {
		t.Fatalf("Inspect() = %#v, %v", inspected, err)
	}
	status, err := client.Status(context.Background(), target)
	if err != nil || status.State != "idle" || status.ACPSessionID != "acp-session" {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
	if err := client.Cancel(context.Background(), target); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ACPX_ARGS_FILE", argsFile)
	if _, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeACPSessionID: "acp-prior"}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), "--resume-session\nacp-prior\n") {
		t.Fatalf("resume argv = %q, err = %v", args, err)
	}
}

func TestPromptWrapsRawACPWithRequestCorrelation(t *testing.T) {
	client, target := fixtureClient(t, "success")
	var events []Event
	receipt, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-42", Message: "hello"}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RequestID != "req-42" || receipt.LastSeq != 3 || receipt.StopReason != "end_turn" {
		t.Fatalf("receipt = %#v", receipt)
	}
	if got := []string{events[0].Type, events[1].Type, events[2].Type}; !reflect.DeepEqual(got, []string{"initialize", "agent_message_chunk", "prompt_result"}) {
		t.Fatalf("event types = %v", got)
	}
	for index, event := range events {
		if event.EventVersion != 1 || event.RequestID != "req-42" || event.Seq != uint64(index+1) || !json.Valid(event.Raw) {
			t.Errorf("event %d = %#v", index, event)
		}
	}
}

func TestPromptAcceptsFinalPartialLine(t *testing.T) {
	client, target := fixtureClient(t, "partial")
	receipt, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-partial", Message: "hello"}, nil)
	if err != nil || receipt.LastSeq != 1 || receipt.StopReason != "end_turn" {
		t.Fatalf("Prompt() = %#v, %v", receipt, err)
	}
}

func TestPromptRejectsMalformedOutputAfterDeliveringValidPrefix(t *testing.T) {
	client, target := fixtureClient(t, "malformed")
	var delivered int
	receipt, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-malformed", Message: "hello"}, func(Event) error {
		delivered++
		return nil
	})
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || delivered != 1 || receipt.LastSeq != 2 {
		t.Fatalf("Prompt() = %#v, delivered=%d, err=%v", receipt, delivered, err)
	}
}

func TestPromptReportsNonzeroExit(t *testing.T) {
	client, target := fixtureClient(t, "nonzero")
	_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-fail", Message: "hello"}, nil)
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 7 || !strings.Contains(commandErr.Error(), "fixture prompt failed") {
		t.Fatalf("Prompt() error = %#v", err)
	}
}

func TestControlReportsNonzeroExit(t *testing.T) {
	client, target := fixtureClient(t, "nonzero")
	_, err := client.Start(context.Background(), StartSpec{Target: target})
	var commandErr *CommandError
	if !errors.As(err, &commandErr) || commandErr.ExitCode != 7 || !strings.Contains(commandErr.Error(), "fixture ensure failed") {
		t.Fatalf("Start() error = %#v", err)
	}
}

func TestPromptReportsRemoteError(t *testing.T) {
	client, target := fixtureClient(t, "remote-error")
	_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-rpc", Message: "hello"}, nil)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || !strings.Contains(err.Error(), "fixture remote failure") {
		t.Fatalf("Prompt() error = %#v", err)
	}
}

func TestPromptCancellation(t *testing.T) {
	client, target := fixtureClient(t, "cancellation")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Prompt(ctx, PromptSpec{Target: target, RequestID: "req-cancel", Message: "hello"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Prompt() error = %v", err)
	}
}

func TestMissingNativeIDRemainsDistinctAndOptional(t *testing.T) {
	client, target := fixtureClient(t, "missing-native")
	info, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil || info.ACPXRecordID == "" || info.ACPSessionID == "" || info.NativeSessionID != "" {
		t.Fatalf("Start() = %#v, %v", info, err)
	}
}

func TestCustomCommandIsPassedAsOneLiteralAgentArgument(t *testing.T) {
	client, target := fixtureClient(t, "success")
	argsFile := filepath.Join(t.TempDir(), "args")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("FAKE_ACPX_ARGS_FILE", argsFile)
	target.Profile = agents.Profile{
		Name: "custom", Provider: "test", Command: "/tmp/ACP server", Args: []string{"--value", "$(touch " + marker + ")", `quote"and\\slash`},
	}
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	var rawAgent string
	for index, line := range lines {
		if line == "--agent" && index+1 < len(lines) {
			rawAgent = lines[index+1]
		}
	}
	if rawAgent == "" || !strings.Contains(rawAgent, "$(touch "+marker+")") || !strings.Contains(rawAgent, `"/tmp/ACP server"`) {
		t.Fatalf("--agent value = %q (all argv: %q)", rawAgent, lines)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("custom argument was unexpectedly evaluated; marker stat error = %v", err)
	}
}

func TestCommandsCanonicalizeSymlinkedWorkingDirectory(t *testing.T) {
	client, target := fixtureClient(t, "success")
	realDirectory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "cwd-alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	target.WorkingDirectory = alias

	argsFile := filepath.Join(t.TempDir(), "args")
	cwdFile := filepath.Join(t.TempDir(), "cwd")
	t.Setenv("FAKE_ACPX_ARGS_FILE", argsFile)
	t.Setenv("FAKE_ACPX_CWD_FILE", cwdFile)
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}

	argsBody, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBody)), "\n")
	var passedCWD string
	for index, arg := range args {
		if arg == "--cwd" && index+1 < len(args) {
			passedCWD = args[index+1]
			break
		}
	}
	commandCWD, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDirectory, err := filepath.EvalSymlinks(realDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if passedCWD != canonicalDirectory || strings.TrimSpace(string(commandCWD)) != canonicalDirectory {
		t.Fatalf("acpx cwd = argument %q, process %q; want canonical %q", passedCWD, strings.TrimSpace(string(commandCWD)), canonicalDirectory)
	}
}
