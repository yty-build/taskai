package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

func TestRun(t *testing.T) {
	originalVersion := buildinfo.Version
	buildinfo.Version = "test-version"
	t.Cleanup(func() { buildinfo.Version = originalVersion })

	tests := []struct {
		name       string
		args       []string
		wantStatus int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "help long",
			args:       []string{"--help"},
			wantStatus: 0,
			wantStdout: helpText + "\n",
		},
		{
			name:       "help short",
			args:       []string{"-h"},
			wantStatus: 0,
			wantStdout: helpText + "\n",
		},
		{
			name:       "version",
			args:       []string{"--version"},
			wantStatus: 0,
			wantStdout: "taskai test-version\n",
		},
		{
			name:       "no arguments",
			wantStatus: 1,
			wantStderr: "taskai: interactive workspace is not implemented in this baseline\n",
		},
		{
			name:       "unknown argument",
			args:       []string{"--unknown"},
			wantStatus: 2,
			wantStderr: "taskai: invalid arguments; use --help\n",
		},
		{
			name:       "extra argument",
			args:       []string{"--help", "extra"},
			wantStatus: 2,
			wantStderr: "taskai: invalid arguments; use --help\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			status := Run(test.args, &stdout, &stderr)
			if status != test.wantStatus {
				t.Fatalf("Run() status = %d, want %d", status, test.wantStatus)
			}
			if stdout.String() != test.wantStdout {
				t.Fatalf("Run() stdout = %q, want %q", stdout.String(), test.wantStdout)
			}
			if stderr.String() != test.wantStderr {
				t.Fatalf("Run() stderr = %q, want %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

func TestInformationalCommandsDoNotCreateState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "absent-state")
	t.Setenv(StateDirEnvironment, stateDir)

	for _, args := range [][]string{{"--help"}, {"--version"}} {
		if status := Run(args, &bytes.Buffer{}, &bytes.Buffer{}); status != 0 {
			t.Fatalf("Run(%q) status = %d, want 0", args, status)
		}
	}

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("informational command created state directory: stat error = %v", err)
	}
}
