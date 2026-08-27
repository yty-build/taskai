//go:build darwin || linux

package connector

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const providerProcessWaitDelay = 2 * time.Second

// configureProviderProcess gives each provider launch its own process group.
// The worker and viewer therefore can never be collateral targets when an
// owned provider tree must be terminated.
func configureProviderProcess(command *exec.Cmd) error {
	if command == nil || command.SysProcAttr != nil {
		return ErrInvalidCommand
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return terminateProviderProcess(command.Process) }
	command.WaitDelay = providerProcessWaitDelay
	return nil
}

func terminateProviderProcess(process *os.Process) error {
	if process == nil || process.Pid <= 0 {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
