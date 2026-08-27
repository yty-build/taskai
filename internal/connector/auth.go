package connector

import (
	"context"
	"errors"
	"io"
)

type AuthState string

const (
	AuthConnected        AuthState = "connected"
	AuthDisconnected     AuthState = "disconnected"
	AuthNeedsRepair      AuthState = "needs_repair"
	AuthMissingConnector AuthState = "missing_connector"
)

type AuthPort interface {
	Probe(context.Context) AuthState
	Authenticate(context.Context, AuthIO) error
}

type AuthIO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type CommandRunner interface {
	Run(context.Context, CommandSpec, io.Reader, io.Writer, io.Writer) error
}

// CommandAuth delegates both actions to provider-owned literal commands.
// Probe output is discarded and only the exit state is interpreted.
type CommandAuth struct {
	Runner       CommandRunner
	ProbeCommand CommandSpec
	AuthCommand  CommandSpec
}

func (a CommandAuth) runner() CommandRunner {
	if a.Runner != nil {
		return a.Runner
	}
	return Launcher{}
}

func (a CommandAuth) Probe(ctx context.Context) AuthState {
	if err := a.runner().Run(ctx, a.ProbeCommand, nil, io.Discard, io.Discard); err != nil {
		if errors.Is(err, ErrInvalidCommand) || errors.Is(err, ErrLaunchFailed) {
			return AuthDisconnected
		}
		return AuthNeedsRepair
	}
	return AuthConnected
}

func (a CommandAuth) Authenticate(ctx context.Context, authIO AuthIO) error {
	out := authIO.Out
	if out == nil {
		out = io.Discard
	}
	errOut := authIO.Err
	if errOut == nil {
		errOut = io.Discard
	}
	if err := a.runner().Run(ctx, a.AuthCommand, authIO.In, out, errOut); err != nil {
		return &BoundaryError{Stage: "authentication", Err: ErrProcessFailed}
	}
	return nil
}
