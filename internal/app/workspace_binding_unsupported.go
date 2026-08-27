//go:build !darwin && !linux

package app

import "github.com/yty-build/acp-gateway/internal/runtime"

func inspectWorkspaceBindingFile(string, []byte, bool) error {
	return runtime.ErrUnsupportedPlatform
}

func establishWorkspaceBindingFile(string, []byte) error {
	return runtime.ErrUnsupportedPlatform
}
