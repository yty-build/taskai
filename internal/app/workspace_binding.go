package app

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/runtime"
)

const (
	workspaceBindingFilename = "workspace.v1"
	workspaceBindingDomain   = "taskai-workspace-binding-v1\x00"
	workspaceBindingPrefix   = "taskai-workspace-binding-v1 sha256:"
)

// workspaceBindingValue is deliberately one-way: persistent TaskAI state can
// prove that a worker belongs to the same exact project without storing its
// path. The domain and file name version changes if this contract ever does.
func workspaceBindingValue(projectCWD string) ([]byte, error) {
	if projectCWD == "" || !filepath.IsAbs(projectCWD) || filepath.Clean(projectCWD) != projectCWD ||
		len(projectCWD) > manager.MaxProjectCWDBytes || !utf8.ValidString(projectCWD) ||
		strings.IndexByte(projectCWD, 0) >= 0 {
		return nil, runtime.ErrInvalidRequest
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(workspaceBindingDomain))
	_, _ = digest.Write([]byte(projectCWD))
	encoded := make([]byte, hex.EncodedLen(sha256.Size))
	hex.Encode(encoded, digest.Sum(nil))
	result := make([]byte, 0, len(workspaceBindingPrefix)+len(encoded)+1)
	result = append(result, workspaceBindingPrefix...)
	result = append(result, encoded...)
	result = append(result, '\n')
	return result, nil
}

// inspectWorkspaceBinding performs the viewer's read-only pre/post-acquire
// check. Missing state is allowed only before acquire, when a new worker may
// still need to create the root and its binding.
func inspectWorkspaceBinding(stateDir, projectCWD string, required bool) error {
	expected, err := workspaceBindingValue(projectCWD)
	if err != nil {
		return err
	}
	return inspectWorkspaceBindingFile(stateDir, expected, required)
}

// establishWorkspaceBinding is the worker's first state mutation. It happens
// before Store, endpoint, provider registry, or Supervisor construction.
func establishWorkspaceBinding(stateDir, projectCWD string) error {
	expected, err := workspaceBindingValue(projectCWD)
	if err != nil {
		return err
	}
	return establishWorkspaceBindingFile(stateDir, expected)
}
