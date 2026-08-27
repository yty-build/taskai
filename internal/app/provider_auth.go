package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/yty-build/acp-gateway/internal/connector"
)

type pinnedAuthKind uint8

const (
	pinnedAuthCodex pinnedAuthKind = iota + 1
	pinnedAuthClaude
)

var errPinnedAuthCommandMissing = errors.New("pinned provider auth command missing")

type pinnedStatusRunner interface {
	Status(context.Context, connector.CommandSpec) (int, error)
}

type literalStatusRunner struct{}

// Status executes one exact literal status vector, discards all provider
// output, and reports only its mechanical process result. It neither invokes
// a shell nor copies an environment; connector.Command owns those guarantees.
func (literalStatusRunner) Status(ctx context.Context, spec connector.CommandSpec) (int, error) {
	if ctx == nil {
		return -1, errors.New("nil auth status context")
	}
	command, err := connector.Command(ctx, spec)
	if err != nil {
		return -1, err
	}
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	err = command.Run()
	if err == nil {
		return 0, nil
	}
	if missingExecutable(err) {
		return -1, errPinnedAuthCommandMissing
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode(), nil
	}
	return -1, err
}

func missingExecutable(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	var execError *exec.Error
	return errors.As(err, &execError) &&
		(errors.Is(execError.Err, exec.ErrNotFound) || errors.Is(execError.Err, os.ErrNotExist))
}

// pinnedProviderAuth owns only the two provider-auth command profiles proven
// for this build. Its enum prevents Registry consumers from mutating launch
// vectors through a returned Profile copy.
type pinnedProviderAuth struct {
	kind       pinnedAuthKind
	status     pinnedStatusRunner
	authRunner connector.CommandRunner
}

func (auth pinnedProviderAuth) Probe(ctx context.Context) connector.AuthState {
	runner := auth.status
	if runner == nil {
		runner = literalStatusRunner{}
	}
	exitCode, err := runner.Status(ctx, auth.probeCommand())
	if errors.Is(err, errPinnedAuthCommandMissing) {
		return connector.AuthMissingConnector
	}
	if err != nil {
		return connector.AuthNeedsRepair
	}
	switch exitCode {
	case 0:
		return connector.AuthConnected
	case 1:
		return connector.AuthDisconnected
	default:
		return connector.AuthNeedsRepair
	}
}

func (auth pinnedProviderAuth) Authenticate(ctx context.Context, streams connector.AuthIO) error {
	return (connector.CommandAuth{
		Runner: auth.authRunner, AuthCommand: auth.authenticationCommand(),
	}).Authenticate(ctx, streams)
}

func (auth pinnedProviderAuth) probeCommand() connector.CommandSpec {
	switch auth.kind {
	case pinnedAuthCodex:
		return connector.CommandSpec{Executable: codexExecutable, Args: []string{"login", "status"}}
	case pinnedAuthClaude:
		return connector.CommandSpec{Executable: "claude", Args: []string{"auth", "status"}}
	default:
		return connector.CommandSpec{}
	}
}

func (auth pinnedProviderAuth) authenticationCommand() connector.CommandSpec {
	switch auth.kind {
	case pinnedAuthCodex:
		return connector.CommandSpec{Executable: codexExecutable, Args: []string{"login"}}
	case pinnedAuthClaude:
		return connector.CommandSpec{Executable: "claude", Args: []string{"auth", "login"}}
	default:
		return connector.CommandSpec{}
	}
}

var _ connector.AuthPort = pinnedProviderAuth{}
