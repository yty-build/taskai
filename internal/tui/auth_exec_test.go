package tui

import (
	"context"
	"errors"
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type authExecBackend struct {
	*fakeBackend
	command    tea.ExecCommand
	commandErr error
	requests   []ProviderID
}

func (backend *authExecBackend) AuthenticationCommand(_ context.Context, provider ProviderID) (tea.ExecCommand, error) {
	backend.requests = append(backend.requests, provider)
	return backend.command, backend.commandErr
}

type inertAuthCommand struct{ ran bool }

func (command *inertAuthCommand) Run() error  { command.ran = true; return nil }
func (*inertAuthCommand) SetStdin(io.Reader)  {}
func (*inertAuthCommand) SetStdout(io.Writer) {}
func (*inertAuthCommand) SetStderr(io.Writer) {}

func loadedAuthExecModel(t *testing.T, backend *authExecBackend) *Model {
	t.Helper()
	model, err := New(Config{
		Catalog: backend.fakeBackend, Tasks: backend.fakeBackend, Providers: backend,
		Runtime: backend.fakeBackend,
	})
	if err != nil {
		t.Fatal(err)
	}
	message := model.Init()()
	model.Update(message)
	return model
}

func TestSettingsAuthenticationPrefersExclusiveExecCommand(t *testing.T) {
	command := &inertAuthCommand{}
	backend := &authExecBackend{fakeBackend: newFake(baseSnapshot()), command: command}
	model := loadedAuthExecModel(t, backend)
	defer model.Close()

	updateKey(model, "s")
	cmd := updateKey(model, "a")
	if cmd == nil {
		t.Fatal("Settings authentication returned no command")
	}
	// Evaluating tea.Exec constructs Bubble Tea's private execution message;
	// the Program, not an ordinary background Cmd, owns command.Run.
	if message := cmd(); message == nil {
		t.Fatal("exclusive authentication command returned no execution message")
	}
	if len(backend.requests) != 1 || backend.requests[0] != "codex" {
		t.Fatalf("AuthenticationCommand requests = %#v", backend.requests)
	}
	if command.ran {
		t.Fatal("provider command ran inside the ordinary model Cmd")
	}
	if calls := backend.callsOf("authenticate"); len(calls) != 0 {
		t.Fatalf("interactive path also invoked fallback Authenticate: %#v", calls)
	}
}

func TestSettingsAuthenticationCommandFailureUsesSafeRefreshPath(t *testing.T) {
	backend := &authExecBackend{
		fakeBackend: newFake(baseSnapshot()),
		commandErr:  errors.New("private provider command detail"),
	}
	model := loadedAuthExecModel(t, backend)
	defer model.Close()

	updateKey(model, "s")
	runCmd(t, model, updateKey(model, "a"))
	if model.pending != actionNone {
		t.Fatalf("authentication remained pending after command construction failure: %v", model.pending)
	}
	if model.status != "Authentication failed" {
		t.Fatalf("safe status = %q, want %q", model.status, "Authentication failed")
	}
	if calls := backend.callsOf("authenticate"); len(calls) != 0 {
		t.Fatalf("construction failure invoked fallback Authenticate: %#v", calls)
	}
}
