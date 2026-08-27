package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

type fakeCommandExecutor struct {
	viewCalls int
	workCalls int
	err       error
}

func (fake *fakeCommandExecutor) View(context.Context, IO) error {
	fake.viewCalls++
	return fake.err
}

func (fake *fakeCommandExecutor) Work(context.Context) error {
	fake.workCalls++
	return fake.err
}

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
		wantViews  int
		wantWorks  int
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
			wantStatus: 0,
			wantViews:  1,
		},
		{
			name:       "hidden worker",
			args:       []string{hiddenWorkerArgument},
			wantStatus: 0,
			wantWorks:  1,
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
			executor := &fakeCommandExecutor{}

			status := runCommand(t.Context(), test.args, IO{Out: &stdout, Err: &stderr}, executor)
			if status != test.wantStatus {
				t.Fatalf("Run() status = %d, want %d", status, test.wantStatus)
			}
			if stdout.String() != test.wantStdout {
				t.Fatalf("Run() stdout = %q, want %q", stdout.String(), test.wantStdout)
			}
			if stderr.String() != test.wantStderr {
				t.Fatalf("Run() stderr = %q, want %q", stderr.String(), test.wantStderr)
			}
			if executor.viewCalls != test.wantViews || executor.workCalls != test.wantWorks {
				t.Fatalf("executor calls View=%d Work=%d, want %d/%d", executor.viewCalls, executor.workCalls, test.wantViews, test.wantWorks)
			}
		})
	}
}

func TestCommandRuntimeErrorsAreSafeAndModeSpecific(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "viewer"},
		{name: "worker", args: []string{hiddenWorkerArgument}},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &fakeCommandExecutor{err: errors.New("credential=canary /provider/native/session")}
			var stderr bytes.Buffer
			status := runCommand(t.Context(), test.args, IO{Err: &stderr}, executor)
			if status != 1 || stderr.String() != "taskai: transport_failed\n" {
				t.Fatalf("status=%d stderr=%q", status, stderr.String())
			}
		})
	}
}

func TestInformationalCommandsDoNotCreateState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "absent-state")
	t.Setenv(StateDirEnvironment, stateDir)

	for _, args := range [][]string{{"--help"}, {"--version"}} {
		if status := Run(t.Context(), args, IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); status != 0 {
			t.Fatalf("Run(%q) status = %d, want 0", args, status)
		}
	}

	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("informational command created state directory: stat error = %v", err)
	}
}
