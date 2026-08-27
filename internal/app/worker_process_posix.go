//go:build darwin || linux

package app

import (
	"os/exec"
	"syscall"
)

// configureDetachedWorker isolates the persistent worker from the viewer's
// foreground process group. Its standard streams remain the null device, so a
// viewer exit or terminal close cannot signal it or leave stale TTY owners.
func configureDetachedWorker(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}
