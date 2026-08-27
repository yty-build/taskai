//go:build !darwin && !linux

package connector

import (
	"errors"
	"os"
	"os/exec"
)

func configureProviderProcess(command *exec.Cmd) error {
	if command == nil {
		return ErrInvalidCommand
	}
	command.Cancel = func() error { return terminateProviderProcess(command.Process) }
	return nil
}

func terminateProviderProcess(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
