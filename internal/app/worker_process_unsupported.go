//go:build !darwin && !linux

package app

import (
	"os/exec"

	"github.com/yty-build/acp-gateway/internal/runtime"
)

func configureDetachedWorker(*exec.Cmd) error {
	return runtime.ErrUnsupportedPlatform
}
