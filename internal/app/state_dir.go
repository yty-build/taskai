package app

import (
	"errors"
	"os"
	"path/filepath"
)

// StateDirEnvironment names the only supported TaskAI state-root override.
const StateDirEnvironment = "TASKAI_STATE_DIR"

var errRelativeStateDir = errors.New("TASKAI_STATE_DIR must be an absolute path")

// ResolveStateDir returns TaskAI's distinct state namespace without creating or
// inspecting it. Persistence code owns creation and permissions in M02.
func ResolveStateDir() (string, error) {
	if override := os.Getenv(StateDirEnvironment); override != "" {
		if !filepath.IsAbs(override) {
			return "", errRelativeStateDir
		}
		return filepath.Clean(override), nil
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "taskai"), nil
}
