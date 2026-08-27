package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"testing"

	"github.com/yty-build/acp-gateway/internal/connector"
)

type pinnedStatusRunnerFake struct {
	exit  int
	err   error
	calls []connector.CommandSpec
}

func (runner *pinnedStatusRunnerFake) Status(_ context.Context, spec connector.CommandSpec) (int, error) {
	runner.calls = append(runner.calls, connector.CommandSpec{
		Executable: spec.Executable, Args: append([]string(nil), spec.Args...), Dir: spec.Dir,
	})
	return runner.exit, runner.err
}

func TestPinnedAuthProbeUsesOnlyFiniteExitSemantics(t *testing.T) {
	for _, test := range []struct {
		name string
		exit int
		err  error
		want connector.AuthState
	}{
		{name: "connected", exit: 0, want: connector.AuthConnected},
		{name: "not logged in", exit: 1, want: connector.AuthDisconnected},
		{name: "other exit", exit: 2, want: connector.AuthNeedsRepair},
		{name: "missing executable", exit: -1, err: errPinnedAuthCommandMissing, want: connector.AuthMissingConnector},
		{name: "status failure", exit: -1, err: errors.New("private command detail"), want: connector.AuthNeedsRepair},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &pinnedStatusRunnerFake{exit: test.exit, err: test.err}
			auth := pinnedProviderAuth{kind: pinnedAuthCodex, status: runner}
			if got := auth.Probe(t.Context()); got != test.want {
				t.Fatalf("Probe = %q, want %q", got, test.want)
			}
			if len(runner.calls) != 1 || runner.calls[0].Executable != "codex" ||
				!reflect.DeepEqual(runner.calls[0].Args, []string{"login", "status"}) || runner.calls[0].Dir != "" {
				t.Fatalf("probe vector = %#v", runner.calls)
			}
		})
	}
}

type pinnedAuthRunnerFake struct {
	spec   connector.CommandSpec
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	calls  int
}

func (runner *pinnedAuthRunnerFake) Run(
	_ context.Context,
	spec connector.CommandSpec,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	runner.calls++
	runner.spec = connector.CommandSpec{
		Executable: spec.Executable, Args: append([]string(nil), spec.Args...), Dir: spec.Dir,
	}
	runner.stdin, runner.stdout, runner.stderr = stdin, stdout, stderr
	return nil
}

func TestPinnedAuthenticationUsesExactLiteralVectorAndCurrentStreams(t *testing.T) {
	runner := &pinnedAuthRunnerFake{}
	auth := pinnedProviderAuth{kind: pinnedAuthClaude, authRunner: runner}
	in := bytes.NewBufferString("provider input")
	var output bytes.Buffer
	streams := connector.AuthIO{In: in, Out: &output, Err: &output}
	if err := auth.Authenticate(t.Context(), streams); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || runner.spec.Executable != "claude" ||
		!reflect.DeepEqual(runner.spec.Args, []string{"auth", "login"}) || runner.spec.Dir != "" {
		t.Fatalf("authentication vector = %#v", runner.spec)
	}
	if runner.stdin != in || runner.stdout != &output || runner.stderr != &output {
		t.Fatalf("authentication streams were not passed through exactly")
	}
}

func TestMissingExecutableClassificationIsMechanical(t *testing.T) {
	missing := &exec.Error{Name: "synthetic-provider", Err: exec.ErrNotFound}
	if !missingExecutable(missing) {
		t.Fatal("exec.ErrNotFound was not classified as a missing connector")
	}
	if missingExecutable(errors.New("provider output canary")) {
		t.Fatal("arbitrary provider error was classified as a missing connector")
	}
}
