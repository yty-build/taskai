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

const testNativeGeneration = "0123456789abcdef0123456789abcdef"

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
		Executable: "/private/path/acpctl", ConsoleSessionID: "S-EXACT", StateRoot: "/private/state-one", ReservationGeneration: testNativeGeneration,
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
		if attempt == 1 {
			runner.SetPreexistingOwnership(true)
		}
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
	stableOwner := strings.TrimSuffix(owner, ":"+testNativeGeneration)
	if !strings.Contains(createCommand, "'_native' '--session' 'S-EXACT' '--owner-token' '"+stableOwner+"' '--reservation-generation' '"+testNativeGeneration+"'") {
		t.Fatalf("private owner command = %q", createCommand)
	}
	if !reflect.DeepEqual(calls[0].arguments[shellIndex+1:shellIndex+5], []string{";", "set-option", "-t", "agent-S-EXACT"}) || calls[0].arguments[shellIndex+5] != tmuxOwnerOption {
		t.Fatalf("atomic owner tagging arguments = %q", calls[0].arguments)
	}
	if !strings.HasPrefix(owner, "v3:") || strings.Contains(owner, "/private/state-one") || !strings.HasSuffix(owner, ":S-EXACT:"+testNativeGeneration) {
		t.Fatalf("private tmux owner tag = %q", owner)
	}
	for _, index := range []int{2, 4} {
		if !reflect.DeepEqual(calls[index].arguments, []string{"attach-session", "-t", "=agent-S-EXACT"}) || calls[index].input == nil {
			t.Fatalf("attach call %d = %#v", index, calls[index])
		}
	}
	for _, index := range []int{1, 3} {
		if !reflect.DeepEqual(calls[index].arguments, []string{"show-options", "-qv", "-t", "agent-S-EXACT", tmuxOwnerOption}) {
			t.Fatalf("owner verification call %d = %#v", index, calls[index])
		}
	}
	if strings.Count(notices.String(), "detach with Ctrl-b d") != 2 {
		t.Fatalf("tmux notices = %q", notices.String())
	}
}

func TestTmuxAttachCommandIsExactAndShellSafe(t *testing.T) {
	command, err := TmuxAttachCommand("S-EXACT_123")
	if err != nil || command != "tmux attach-session -t =agent-S-EXACT_123" {
		t.Fatalf("TmuxAttachCommand() = %q, %v", command, err)
	}
	for _, unsafe := range []string{"S bad", "S;touch-pwned", "../S", "S$(id)"} {
		if command, err := TmuxAttachCommand(unsafe); err == nil || command != "" {
			t.Fatalf("TmuxAttachCommand(%q) = %q, %v", unsafe, command, err)
		}
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
	owner += ":" + testNativeGeneration
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-RACE", StateRoot: "/private/race-state", ReservationGeneration: testNativeGeneration,
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
		Executable: "acpctl", ConsoleSessionID: "S-COLLISION", StateRoot: "/private/right-state", ReservationGeneration: testNativeGeneration,
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession:    func(context.Context, string, string) (bool, error) { return true, nil },
		runCommand: func(_ context.Context, _ string, arguments []string, _ string, _ io.Reader, output, _ io.Writer) error {
			if arguments[0] == "show-options" {
				_, _ = io.WriteString(output, "v2:wrong-state:S-COLLISION")
			} else if arguments[0] == "attach-session" {
				attached = true
			}
			return nil
		},
	}
	err := runner.Run(context.Background(), Invocation{WorkingDirectory: "/repo"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "different native owner protocol, state root, or generation") || !strings.Contains(err.Error(), "Do not kill it based on this mismatch alone") || !strings.Contains(err.Error(), "inspect Session status") || !strings.Contains(err.Error(), "acpctl session recover-native S-COLLISION") || strings.Contains(err.Error(), "session stop") || attached || !runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
		t.Fatalf("collision error=%v attached=%t continuing=%t activity=%t", err, attached, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerRetainsOwnershipWhenExistingSessionInspectionIsUncertain(t *testing.T) {
	t.Setenv("TMUX", "")
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-UNCERTAIN", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession: func(context.Context, string, string) (bool, error) {
			return false, errors.New("tmux server inspection failed")
		},
	}
	err := runner.Run(context.Background(), Invocation{WorkingDirectory: t.TempDir()}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "tmux server inspection failed") || !runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
		t.Fatalf("inspection uncertainty error=%v continuing=%t activity=%t", err, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerSwitchesCurrentClientToOnePrivateHarnessInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux/default,1,2")
	exists := false
	owner := ""
	var calls [][]string
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-NESTED", StateRoot: "/private/nested-state", ReservationGeneration: testNativeGeneration,
		isInteractive: func(io.Reader, io.Writer) bool { return true },
		lookupPath:    func(string) (string, error) { return "/fake/tmux", nil },
		hasSession: func(context.Context, string, string) (bool, error) {
			return exists, nil
		},
		runCommand: func(_ context.Context, _ string, arguments []string, _ string, input io.Reader, output, _ io.Writer) error {
			calls = append(calls, append([]string(nil), arguments...))
			if arguments[0] == "new-session" {
				exists = true
				owner = arguments[len(arguments)-1]
			}
			if arguments[0] == "show-options" {
				_, _ = io.WriteString(output, owner)
			}
			if arguments[0] == "switch-client" && input != nil {
				t.Fatal("switch-client must not consume the provider terminal input")
			}
			return nil
		},
	}
	var notices bytes.Buffer
	invocation := Invocation{WorkingDirectory: "/private/worktree"}
	for range 2 {
		if err := runner.Run(context.Background(), invocation, strings.NewReader("must-not-be-consumed"), &notices, &notices); err != nil {
			t.Fatal(err)
		}
		if !runner.ProcessMayContinue() || !runner.NativeActivityOccurred() {
			t.Fatalf("nested handoff continuing=%t activity=%t", runner.ProcessMayContinue(), runner.NativeActivityOccurred())
		}
	}
	if len(calls) != 5 {
		t.Fatalf("nested tmux calls = %#v", calls)
	}
	for _, index := range []int{2, 4} {
		if !reflect.DeepEqual(calls[index], []string{"switch-client", "-t", "=agent-S-NESTED"}) {
			t.Fatalf("nested switch call %d = %q", index, calls[index])
		}
	}
	if strings.Contains(strings.Join(flattenArguments(calls), " "), "attach-session") {
		t.Fatalf("nested handoff tried to attach or launch a second provider: %#v", calls)
	}
	if strings.Count(notices.String(), "Ctrl-b L returns to this console") != 2 || !strings.Contains(notices.String(), "Ctrl-b d detaches while the provider continues") {
		t.Fatalf("nested tmux notices = %q", notices.String())
	}
}

func TestTmuxRunnerFallsBackToDirectProviderWithoutTmux(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	record := filepath.Join(t.TempDir(), "record")
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-FALLBACK", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
		lookupPath:    func(string) (string, error) { return "", os.ErrNotExist },
		isInteractive: func(io.Reader, io.Writer) bool { return true },
	}
	activated := false
	runner.SetReservationActivator(func(context.Context) error {
		activated = true
		return nil
	})
	var output bytes.Buffer
	err = runner.Run(context.Background(), Invocation{
		Command: fixture, WorkingDirectory: t.TempDir(),
		Environment: map[string]string{"FAKE_NATIVE_RECORD": record, "FAKE_NATIVE_MARKER": "tmux unavailable"},
	}, strings.NewReader(""), &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !activated {
		t.Fatal("direct fallback started provider before activating its reservation")
	}
	if runner.ProcessMayContinue() {
		t.Fatal("direct fallback incorrectly reported a continuing provider")
	}
	body, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(body), "marker=tmux unavailable") {
		t.Fatalf("direct fallback record = %q, %v", body, err)
	}
}

func TestTmuxRunnerDirectFallbackActivationFailureNeverStartsProvider(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	record := filepath.Join(t.TempDir(), "must-not-exist")
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-FALLBACK-BLOCKED", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
		lookupPath:    func(string) (string, error) { return "", os.ErrNotExist },
		isInteractive: func(io.Reader, io.Writer) bool { return true },
	}
	runner.SetReservationActivator(func(context.Context) error {
		return errors.New("wait for the current ACP turn to settle, then open Native CLI again")
	})
	err = runner.Run(context.Background(), Invocation{
		Command: fixture, WorkingDirectory: t.TempDir(), Environment: map[string]string{"FAKE_NATIVE_RECORD": record},
	}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "wait for the current ACP turn to settle") {
		t.Fatalf("activation error = %v", err)
	}
	if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
		t.Fatalf("provider ran after rejected activation: %v", statErr)
	}
	if runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
		t.Fatalf("rejected activation continuing=%t activity=%t", runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerInsideTmuxFailsClosedWhenTmuxExecutableIsUnavailable(t *testing.T) {
	t.Setenv("TMUX", "/private/tmp/tmux/default,1,2")
	runner := &TmuxRunner{
		Executable: "acpctl", ConsoleSessionID: "S-NO-TMUX", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
		lookupPath:    func(string) (string, error) { return "", os.ErrNotExist },
		isInteractive: func(io.Reader, io.Writer) bool { return true },
	}
	err := runner.Run(context.Background(), Invocation{Command: "must-not-run", WorkingDirectory: t.TempDir()}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing to start a second provider-native process") || runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
		t.Fatalf("nested missing-tmux error=%v continuing=%t activity=%t", err, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
	}
}

func TestTmuxRunnerInsideTmuxNeverDirectRunsWithoutInteractiveHandoffPrerequisites(t *testing.T) {
	for _, test := range []struct {
		name        string
		executable  string
		interactive bool
		want        string
	}{
		{name: "missing console executable", interactive: true, want: "Agent Task Console executable is unavailable"},
		{name: "noninteractive client", executable: "acpctl", want: "native handoff has no interactive client"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TMUX", "/private/tmp/tmux/default,1,2")
			record := filepath.Join(t.TempDir(), "must-not-exist")
			runner := &TmuxRunner{
				Executable: test.executable, ConsoleSessionID: "S-FAIL-CLOSED", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
				isInteractive: func(io.Reader, io.Writer) bool { return test.interactive },
			}
			err := runner.Run(context.Background(), Invocation{
				Command: filepath.Join("testdata", "fake-native"), WorkingDirectory: t.TempDir(),
				Environment: map[string]string{"FAKE_NATIVE_RECORD": record},
			}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "refusing to start a second provider-native process") {
				t.Fatalf("nested prerequisite error = %v", err)
			}
			if _, statErr := os.Stat(record); !os.IsNotExist(statErr) {
				t.Fatalf("nested fail-closed path launched provider; record stat = %v", statErr)
			}
			if runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
				t.Fatalf("nested fail-closed state continuing=%t activity=%t", runner.ProcessMayContinue(), runner.NativeActivityOccurred())
			}
		})
	}
}

func TestPreexistingOwnershipNeverFallsBackOrCreatesReplacementProvider(t *testing.T) {
	for _, test := range []struct {
		name        string
		interactive bool
		lookupPath  func(string) (string, error)
		hasSession  func(context.Context, string, string) (bool, error)
		state       string
		want        string
	}{
		{
			name: "noninteractive", want: "requires an interactive tmux attachment",
		},
		{
			name: "tmux unavailable", interactive: true,
			lookupPath: func(string) (string, error) { return "", os.ErrNotExist },
			want:       "existing tmux harness, but the tmux executable is unavailable",
		},
		{
			name: "active session missing", interactive: true,
			lookupPath: func(string) (string, error) { return "/fake/tmux", nil },
			hasSession: func(context.Context, string, string) (bool, error) { return false, nil },
			state:      PreexistingOwnershipActive,
			want:       "active provider-native ownership exists, but tmux Session \"agent-S-PREEXISTING\" is missing",
		},
		{
			name: "reservation missing", interactive: true,
			lookupPath: func(string) (string, error) { return "/fake/tmux", nil },
			hasSession: func(context.Context, string, string) (bool, error) { return false, nil },
			state:      PreexistingOwnershipReserved,
			want:       "acpctl session recover-native S-PREEXISTING",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TMUX", "")
			runCalls := 0
			runner := &TmuxRunner{
				Executable: "acpctl", ConsoleSessionID: "S-PREEXISTING", StateRoot: t.TempDir(), ReservationGeneration: testNativeGeneration,
				isInteractive: func(io.Reader, io.Writer) bool { return test.interactive },
				lookupPath:    test.lookupPath,
				hasSession:    test.hasSession,
				runCommand: func(context.Context, string, []string, string, io.Reader, io.Writer, io.Writer) error {
					runCalls++
					return nil
				},
			}
			if test.state != "" {
				runner.SetPreexistingOwnershipState(test.state)
			} else {
				runner.SetPreexistingOwnership(true)
			}
			err := runner.Run(context.Background(), Invocation{Command: "must-not-run", WorkingDirectory: t.TempDir()}, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "refusing to") {
				t.Fatalf("preexisting %s error = %v", test.name, err)
			}
			if runCalls != 0 || !runner.ProcessMayContinue() || runner.NativeActivityOccurred() {
				t.Fatalf("preexisting %s runCalls=%d continuing=%t activity=%t", test.name, runCalls, runner.ProcessMayContinue(), runner.NativeActivityOccurred())
			}
		})
	}
}

func TestTmuxReservationRecoveryVerifierRequiresExactHarnessAbsence(t *testing.T) {
	for _, test := range []struct {
		name       string
		lookupErr  error
		exists     bool
		inspectErr error
		want       string
	}{
		{name: "absent"},
		{name: "harness still exists", exists: true, want: `exact tmux Session "agent-S-RECOVER" still exists`},
		{name: "tmux unavailable", lookupErr: os.ErrNotExist, want: "tmux executable is unavailable"},
		{name: "inspection uncertain", inspectErr: errors.New("tmux server unavailable"), want: "tmux server unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var inspectedName string
			verifier := TmuxReservationRecoveryVerifier{
				lookupPath: func(string) (string, error) {
					if test.lookupErr != nil {
						return "", test.lookupErr
					}
					return "/fake/tmux", nil
				},
				hasSession: func(_ context.Context, _, name string) (bool, error) {
					inspectedName = name
					return test.exists, test.inspectErr
				},
			}
			err := verifier.VerifyReservationAbsent(context.Background(), "S-RECOVER")
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("recovery verification error = %v, want %q", err, test.want)
			}
			if test.lookupErr == nil && inspectedName != "agent-S-RECOVER" {
				t.Fatalf("recovery inspected %q", inspectedName)
			}
		})
	}
}

func flattenArguments(calls [][]string) []string {
	var flattened []string
	for _, call := range calls {
		flattened = append(flattened, call...)
	}
	return flattened
}
