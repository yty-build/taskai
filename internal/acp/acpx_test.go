package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestGeminiProfilePassesExperimentalACPAsLiteralAgentArgv(t *testing.T) {
	client, target := fixtureClient(t, "success")
	registry, err := agents.Load(filepath.Join(t.TempDir(), "missing-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	target.Profile, err = registry.Get("gemini")
	if err != nil {
		t.Fatal(err)
	}

	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ACPX_ARGS_FILE", argsFile)
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "--agent\n\"gemini\" \"--experimental-acp\"\nsessions\nensure\n"
	if !strings.Contains(string(args), want) {
		t.Fatalf("Gemini acpx argv =\n%s\nwant contiguous literal agent argv =\n%s", args, want)
	}
}

func TestOpenCodePromptContinuesDurableSessionAcrossProcessesAndFailsClosed(t *testing.T) {
	client, target := fixtureClient(t, "durable-opencode")
	registry, err := agents.Load(filepath.Join(t.TempDir(), "missing-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	target.Profile, err = registry.Get("opencode")
	if err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(t.TempDir(), "durable-session")
	pidFile := filepath.Join(t.TempDir(), "pids")
	argsLog := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_ACPX_DURABLE_STATE", stateFile)
	t.Setenv("FAKE_ACPX_PID_LOG", pidFile)
	t.Setenv("FAKE_ACPX_ARGS_LOG", argsLog)

	info, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if info.ACPXRecordID != "acpx-opencode-durable" || info.ACPSessionID != "acp-opencode-durable" || info.NativeSessionID != "native-opencode-durable" {
		t.Fatalf("Start() = %#v", info)
	}

	var messages []string
	var sessionIDs []string
	for turn := 1; turn <= 2; turn++ {
		if turn == 2 {
			resumed, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeACPSessionID: info.ACPSessionID})
			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if resumed.ACPXRecordID != info.ACPXRecordID || resumed.ACPSessionID != info.ACPSessionID || resumed.NativeSessionID != info.NativeSessionID {
				t.Fatalf("Resume() = %#v, want exact conversation identity %#v", resumed, info)
			}
		}
		_, err := client.Prompt(context.Background(), PromptSpec{
			Target: target, RequestID: fmt.Sprintf("req-%d", turn), Message: fmt.Sprintf("turn %d", turn),
		}, func(event Event) error {
			if event.Type != "agent_message_chunk" {
				return nil
			}
			var raw struct {
				Params struct {
					Update struct {
						Content struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"update"`
				} `json:"params"`
			}
			if err := json.Unmarshal(event.Raw, &raw); err != nil {
				return err
			}
			messages = append(messages, raw.Params.Update.Content.Text)
			sessionIDs = append(sessionIDs, event.SessionID)
			return nil
		})
		if err != nil {
			t.Fatalf("Prompt(turn %d) error = %v", turn, err)
		}
	}
	wantMessages := []string{"turn-1 continued acp-opencode-durable", "turn-2 continued acp-opencode-durable"}
	if !reflect.DeepEqual(messages, wantMessages) {
		t.Fatalf("messages = %q, want %q", messages, wantMessages)
	}
	if wantSessionIDs := []string{"acp-opencode-durable", "acp-opencode-durable"}; !reflect.DeepEqual(sessionIDs, wantSessionIDs) {
		t.Fatalf("ACP Session IDs = %q, want %q", sessionIDs, wantSessionIDs)
	}

	pids, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pidLines := strings.Fields(string(pids))
	seenPIDs := make(map[string]bool, len(pidLines))
	for _, pid := range pidLines {
		if seenPIDs[pid] {
			t.Fatalf("operational process IDs = %q, want distinct processes", pidLines)
		}
		seenPIDs[pid] = true
	}
	if len(pidLines) != 4 {
		t.Fatalf("operational process IDs = %q, want start, first prompt, resume, and second prompt", pidLines)
	}

	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if occurrences := strings.Count(string(args), "--agent\n\"npx\" \"-y\" \"opencode-ai@1.18.18\" \"acp\"\n"); occurrences != 4 {
		t.Fatalf("pinned OpenCode --agent occurrences = %d, want 4; argv log:\n%s", occurrences, args)
	}
	if !strings.Contains(string(args), "--resume-session\nacp-opencode-durable\n") {
		t.Fatalf("resume argv did not preserve ACP Session ID; argv log:\n%s", args)
	}

	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	_, err = client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "req-lost", Message: "must not fork"}, nil)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || !strings.Contains(err.Error(), "durable ACP session unavailable; refusing a replacement conversation") {
		t.Fatalf("Prompt() after durable state loss error = %#v", err)
	}
	if _, statErr := os.Stat(stateFile); !os.IsNotExist(statErr) {
		t.Fatalf("lost durable state was unexpectedly replaced; stat error = %v", statErr)
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
