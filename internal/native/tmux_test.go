package native

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTmuxHarnessUsesDerivedNameAndShellQuotesOnlyPrivateEntrypoint(t *testing.T) {
	if got, want := TmuxSessionName("S-ABC_123"), "agent-S-ABC_123"; got != want {
		t.Fatalf("TmuxSessionName() = %q, want %q", got, want)
	}
	got := shellJoin("/tmp/agent's binary", "_native", "--session", "S-safe")
	want := `'/tmp/agent'"'"'s binary' '_native' '--session' 'S-safe'`
	if got != want {
		t.Fatalf("shellJoin() = %q, want %q", got, want)
	}
}

func TestTmuxRunnerCreatesAttachesAndReattachesOnePrivateHarness(t *testing.T) {
	t.Setenv("TMUX", "")
	type call struct {
		arguments []string
		input     io.Reader
	}
	var calls []call
	exists := false
	owner := ""
	runner := &TmuxRunner{
		Executable: "/private/path/acpctl", ConsoleSessionID: "S-EXACT", StateRoot: "/private/state-one",
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession: func(context.Context, string, string) (bool, error) {
			return exists, nil
		},
		runCommand: func(_ context.Context, _ string, arguments []string, _ string, input io.Reader, output, _ io.Writer) error {
			calls = append(calls, call{arguments: append([]string(nil), arguments...), input: input})
			if arguments[0] == "new-session" {
				exists = true
				owner = arguments[len(arguments)-1]
			} else if arguments[0] == "show-options" {
				_, _ = io.WriteString(output, owner+"\n")
			}
			return nil
		},
	}
	invocation := Invocation{
		Command: "provider-with-secret-argument", Args: []string{"--token", "never-in-tmux"},
		WorkingDirectory: "/private/worktree", Environment: map[string]string{"SECRET": "never-in-tmux"},
	}
	var notices bytes.Buffer
	for attempt := 0; attempt < 2; attempt++ {
		if err := runner.Run(context.Background(), invocation, strings.NewReader("input"), &notices, &notices); err != nil {
			t.Fatal(err)
		}
		if !runner.ProcessMayContinue() {
			t.Fatal("tmux viewer detach was not reported as provider-continuing")
		}
	}
	if len(calls) != 5 {
		t.Fatalf("tmux calls = %#v, want atomic create/tag+verify+attach+verify+reattach", calls)
	}
	wantCreatePrefix := []string{"new-session", "-d", "-s", "agent-S-EXACT", "-c", "/private/worktree", "-e", "AGENT_HOME=/private/state-one"}
	if !reflect.DeepEqual(calls[0].arguments[:len(wantCreatePrefix)], wantCreatePrefix) {
		t.Fatalf("create arguments = %q", calls[0].arguments)
	}
	shellIndex := len(wantCreatePrefix)
	if strings.HasPrefix(calls[0].arguments[shellIndex], "PATH=") {
		t.Fatalf("PATH environment is missing its tmux -e option: %q", calls[0].arguments)
	}
	if calls[0].arguments[shellIndex] == "-e" {
		if !strings.HasPrefix(calls[0].arguments[shellIndex+1], "PATH=") {
			t.Fatalf("tmux PATH environment = %q", calls[0].arguments)
		}
		shellIndex += 2
	}
	createCommand := calls[0].arguments[shellIndex]
	for _, forbidden := range []string{"provider-with-secret-argument", "never-in-tmux", "SECRET"} {
		if strings.Contains(createCommand, forbidden) {
			t.Fatalf("tmux metadata leaked %q in %q", forbidden, createCommand)
		}
	}
	if !strings.Contains(createCommand, "'_native' '--session' 'S-EXACT'") {
		t.Fatalf("private owner command = %q", createCommand)
	}
	if !reflect.DeepEqual(calls[0].arguments[shellIndex+1:shellIndex+5], []string{";", "set-option", "-t", "agent-S-EXACT"}) || calls[0].arguments[shellIndex+5] != tmuxOwnerOption {
		t.Fatalf("atomic owner tagging arguments = %q", calls[0].arguments)
	}
	if !strings.HasPrefix(owner, "v1:") || strings.Contains(owner, "/private/state-one") || !strings.HasSuffix(owner, ":S-EXACT") {
		t.Fatalf("private tmux owner tag = %q", owner)
	}
	for _, index := range []int{2, 4} {
		if !reflect.DeepEqual(calls[index].arguments, []string{"attach-session", "-t", "=agent-S-EXACT"}) || calls[index].input == nil {
			t.Fatalf("attach call %d = %#v", index, calls[index])
		}
	}
	if strings.Count(notices.String(), "detach with Ctrl-b d") != 2 {
		t.Fatalf("tmux notices = %q", notices.String())
	}
}

func TestTmuxRunnerConcurrentCreateRaceAttachesWinner(t *testing.T) {
	t.Setenv("TMUX", "")
	hasCalls := 0
	showCalls := 0
	var attached bool
	owner, err := tmuxOwnerIdentity("/private/race-state", "S-RACE")
	if err != nil {
		t.Fatal(err)
	}
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-RACE", StateRoot: "/private/race-state",
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession: func(context.Context, string, string) (bool, error) {
			hasCalls++
			return hasCalls > 1, nil
		},
		runCommand: func(_ context.Context, _ string, arguments []string, _ string, _ io.Reader, output, _ io.Writer) error {
			if arguments[0] == "new-session" {
				return errors.New("another viewer won")
			}
			if arguments[0] == "show-options" {
				showCalls++
				if showCalls > 1 {
					_, _ = io.WriteString(output, owner)
				}
			} else if arguments[0] == "attach-session" {
				attached = true
			}
			return nil
		},
	}
	if err := runner.Run(context.Background(), Invocation{WorkingDirectory: "/repo"}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if hasCalls != 2 || showCalls != 2 || !attached || !runner.ProcessMayContinue() || !runner.NativeActivityOccurred() {
		t.Fatalf("race result hasCalls=%d showCalls=%d attached=%t continuing=%t activity=%t", hasCalls, showCalls, attached, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerRejectsUnownedNameCollision(t *testing.T) {
	t.Setenv("TMUX", "")
	attached := false
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-COLLISION", StateRoot: "/private/right-state",
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession:    func(context.Context, string, string) (bool, error) { return true, nil },
		runCommand: func(_ context.Context, _ string, arguments []string, _ string, _ io.Reader, output, _ io.Writer) error {
			if arguments[0] == "show-options" {
				_, _ = io.WriteString(output, "v1:wrong-state:S-COLLISION")
			} else if arguments[0] == "attach-session" {
				attached = true
			}
			return nil
		},
	}
	err := runner.Run(context.Background(), Invocation{WorkingDirectory: "/repo"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not owned by this Agent Task Console state root") || attached || !runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
		t.Fatalf("collision error=%v attached=%t continuing=%t activity=%t", err, attached, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerFallsBackToDirectProviderInsideTmuxOrWithoutTmux(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		tmuxEnv    string
		lookupPath func(string) (string, error)
	}{
		{name: "nested tmux", tmuxEnv: "already-inside"},
		{name: "tmux unavailable", lookupPath: func(string) (string, error) { return "", os.ErrNotExist }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TMUX", test.tmuxEnv)
			record := filepath.Join(t.TempDir(), "record")
			runner := &TmuxRunner{
				Executable: "acpctl", ConsoleSessionID: "S-FALLBACK", lookupPath: test.lookupPath,
				isInteractive: func(io.Reader, io.Writer) bool { return true },
			}
			var output bytes.Buffer
			err := runner.Run(context.Background(), Invocation{
				Command: fixture, WorkingDirectory: t.TempDir(),
				Environment: map[string]string{"FAKE_NATIVE_RECORD": record, "FAKE_NATIVE_MARKER": test.name},
			}, strings.NewReader(""), &output, &output)
			if err != nil {
				t.Fatal(err)
			}
			if runner.ProcessMayContinue() {
				t.Fatal("direct fallback incorrectly reported a continuing provider")
			}
			body, err := os.ReadFile(record)
			if err != nil || !strings.Contains(string(body), "marker="+test.name) {
				t.Fatalf("direct fallback record = %q, %v", body, err)
			}
		})
	}
}
