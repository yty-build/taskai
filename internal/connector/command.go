package connector

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// CommandSpec is a literal executable and argv. It has no command string,
// interpolation, shell mode, or environment snapshot.
type CommandSpec struct {
	Executable string
	Args       []string
	Dir        string
}

func (s CommandSpec) validate() error {
	if s.Executable == "" || strings.IndexByte(s.Executable, 0) >= 0 {
		return ErrInvalidCommand
	}
	for _, arg := range s.Args {
		if strings.IndexByte(arg, 0) >= 0 {
			return ErrInvalidCommand
		}
	}
	if s.Dir != "" && (!filepath.IsAbs(s.Dir) || strings.IndexByte(s.Dir, 0) >= 0) {
		return ErrInvalidCommand
	}
	return nil
}

// Command constructs the exact exec.Cmd used by all connector and auth
// launches. exec.CommandContext receives the executable and argv separately;
// no shell is inserted. Env remains nil so the child reuses provider-owned
// authentication from the caller environment without TaskAI copying it.
func Command(ctx context.Context, spec CommandSpec) (*exec.Cmd, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	args := append([]string(nil), spec.Args...)
	cmd := exec.CommandContext(ctx, spec.Executable, args...)
	cmd.Dir = spec.Dir
	if err := configureProviderProcess(cmd); err != nil {
		return nil, ErrInvalidCommand
	}
	return cmd, nil
}

// Process owns the exact connector process and its ACP stdio pipes.
type Process struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	waitOnce sync.Once
	waitErr  error
}

type Launcher struct{}

func (Launcher) Start(ctx context.Context, spec CommandSpec) (*Process, error) {
	processCtx, cancel := context.WithCancel(ctx)
	cmd, err := Command(processCtx, spec)
	if err != nil {
		cancel()
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, &BoundaryError{Stage: "stdin", Err: ErrLaunchFailed}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		return nil, &BoundaryError{Stage: "stdout", Err: ErrLaunchFailed}
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		cancel()
		return nil, &BoundaryError{Stage: "launch", Err: ErrLaunchFailed}
	}
	return &Process{cmd: cmd, cancel: cancel, Stdin: stdin, Stdout: stdout}, nil
}

func (p *Process) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Wait() error {
	if p == nil || p.cmd == nil {
		return ErrProcessFailed
	}
	p.waitOnce.Do(func() {
		if err := p.cmd.Wait(); err != nil {
			p.waitErr = ErrProcessFailed
		}
	})
	return p.waitErr
}

// Close terminates and reaps the isolated provider process group. A provider
// launcher may fork, so killing only the direct child would abandon work that
// TaskAI still owns.
func (p *Process) Close() error {
	if p == nil {
		return nil
	}
	p.cancel()
	_ = terminateProviderProcess(p.cmd.Process)
	_ = p.Stdin.Close()
	_ = p.Stdout.Close()
	_ = p.Wait()
	return nil
}

// Run executes a finite mechanical command without capturing output.
func (Launcher) Run(ctx context.Context, spec CommandSpec, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd, err := Command(ctx, spec)
	if err != nil {
		return err
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return &BoundaryError{Stage: "command launch", Err: ErrLaunchFailed}
		}
		return &BoundaryError{Stage: "command", Err: ErrProcessFailed}
	}
	return nil
}
