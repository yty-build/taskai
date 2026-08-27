package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/tui"
)

type portFake struct {
	snapshot runtime.Snapshot
	event    runtime.Event
	err      error

	addTaskCalls    int
	addManagerCalls int
	actionCalls     int
	authCalls       int
	recheckCalls    int
}

func (fake *portFake) Snapshot(context.Context) (runtime.Snapshot, error) {
	return fake.snapshot, fake.err
}
func (fake *portFake) AddTask(context.Context, string) (task.ID, error) {
	fake.addTaskCalls++
	return "task-created", fake.err
}
func (fake *portFake) AddManager(context.Context, task.ID, settings.ProviderID, string) (session.ID, error) {
	fake.addManagerCalls++
	return "session-created", fake.err
}
func (fake *portFake) SetEnabled(context.Context, settings.ProviderID, bool) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Authenticate(context.Context, settings.ProviderID) error {
	fake.authCalls++
	return fake.err
}
func (fake *portFake) Recheck(context.Context, settings.ProviderID) error {
	fake.actionCalls++
	fake.recheckCalls++
	return fake.err
}
func (fake *portFake) Attach(context.Context, task.ID) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Detach(context.Context, task.ID) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Send(context.Context, session.ID, string) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Resume(context.Context, session.ID) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Stop(context.Context, session.ID) error {
	fake.actionCalls++
	return fake.err
}
func (fake *portFake) Next(context.Context, runtime.Cursor) (runtime.Event, error) {
	return fake.event, fake.err
}
func (*portFake) Close() error { return nil }

func testTerminalPort(t *testing.T, fake *portFake) *terminalPort {
	t.Helper()
	port, err := newTerminalPort(fake, connector.NewRegistry(), connector.AuthIO{})
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestTerminalSnapshotConversionIsMechanicalAndDefensive(t *testing.T) {
	stamp := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	fake := &portFake{snapshot: runtime.Snapshot{
		Tasks: []runtime.Task{{ID: "task-a", Title: "Task A", CreatedAt: stamp, Active: 2, Dormant: 1}},
		Sessions: []runtime.Session{
			{
				ID: "manager-a", TaskID: "task-a", Role: session.RoleManager, Label: "Manager",
				ProviderID: "codex", State: session.StateActive, ResumeAvailable: true,
				CreatedAt: stamp, LastActiveAt: stamp,
			},
			{
				ID: "child-a", TaskID: "task-a", ParentID: "manager-a", Role: session.RoleSubordinate,
				Label: "Child", ProviderID: "claude-code", Mode: connector.ModeFollow,
				State: session.StateStarting, CreatedAt: stamp, LastActiveAt: stamp,
			},
		},
		Providers: []runtime.Provider{
			{
				ID: "codex", DisplayName: "Codex", Enabled: true, ProbeStatus: settings.ProbeConnected,
				Capabilities: settings.Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: true},
			},
			{
				ID: "claude-code", DisplayName: "Claude Code", Enabled: true,
				ProbeStatus:  settings.ProbeNotConnected,
				Capabilities: settings.Capabilities{Subordinate: true, Resume: true},
			},
		},
		StreamGeneration: 7, ThroughSequence: 19,
	}}
	port := testTerminalPort(t, fake)

	got, err := port.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.StreamGeneration != 7 || got.ThroughSequence != 19 || len(got.Tasks) != 1 || len(got.Sessions) != 2 || len(got.Providers) != 2 {
		t.Fatalf("converted snapshot = %#v", got)
	}
	if got.Sessions[0].Role != tui.RoleManager || got.Sessions[0].Mode != "" || !got.Sessions[0].ResumeAvailable {
		t.Fatalf("manager conversion = %#v", got.Sessions[0])
	}
	if got.Sessions[1].Role != tui.RoleSubordinate || got.Sessions[1].ParentID != "manager-a" || got.Sessions[1].Mode != tui.ModeFollow {
		t.Fatalf("child conversion = %#v", got.Sessions[1])
	}
	if got.Providers[0].ProbeStatus != tui.ProbeReady || got.Providers[1].ProbeStatus != tui.ProbeDisconnected {
		t.Fatalf("provider probes = %#v", got.Providers)
	}
}

func TestTerminalProviderProbeMapping(t *testing.T) {
	tests := []struct {
		from settings.ProbeStatus
		to   tui.ProbeStatus
	}{
		{settings.ProbeUnknown, tui.ProbeUnknown},
		{settings.ProbeNotConnected, tui.ProbeDisconnected},
		{settings.ProbeDisconnected, tui.ProbeDisconnected},
		{settings.ProbeChecking, tui.ProbeChecking},
		{settings.ProbeConnected, tui.ProbeReady},
		{settings.ProbeNeedsRepair, tui.ProbeNeedsRepair},
		{settings.ProbeMissingConnector, tui.ProbeMissing},
	}
	for _, test := range tests {
		t.Run(string(test.from), func(t *testing.T) {
			got, err := terminalProvider(runtime.Provider{ID: "provider", DisplayName: "Provider", ProbeStatus: test.from})
			if err != nil {
				t.Fatal(err)
			}
			if got.ProbeStatus != test.to {
				t.Fatalf("ProbeStatus = %q, want %q", got.ProbeStatus, test.to)
			}
		})
	}
}

func TestTerminalEventConversionCopiesFrameAndChecksIdentity(t *testing.T) {
	payload := []byte("transient frame")
	fake := &portFake{event: runtime.Event{
		Generation: 3, Sequence: 4, Kind: runtime.EventPublicFrame,
		TaskID: "task-a", SessionID: "manager-a",
		Frame: runtime.PublicFrame{Kind: runtime.FrameText, PublicType: "agent_message_chunk", Payload: payload},
	}}
	port := testTerminalPort(t, fake)
	event, err := port.Next(t.Context(), tui.StreamCursor{Generation: 3, After: 3})
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	if event.Kind != tui.EventPublicFrame || event.Frame.Kind != tui.FrameText || string(event.Frame.Payload) != "transient frame" {
		t.Fatalf("event = %#v", event)
	}

	fake.event = runtime.Event{
		Generation: 3, Sequence: 5, Kind: runtime.EventSessionUpsert,
		TaskID: "task-a", SessionID: "manager-a",
		Session: runtime.Session{
			ID: "other-manager", TaskID: "task-a", Role: session.RoleManager, ProviderID: "codex",
			State: session.StateActive, CreatedAt: time.Now(), LastActiveAt: time.Now(),
		},
	}
	if _, err := port.Next(t.Context(), tui.StreamCursor{}); !errors.Is(err, runtime.ErrTransport) {
		t.Fatalf("identity mismatch error = %v, want transport code", err)
	}
}

func TestTerminalActionsRejectInvalidIDsBeforeRuntime(t *testing.T) {
	fake := &portFake{}
	port := testTerminalPort(t, fake)
	if _, err := port.AddManager(t.Context(), "bad task", "codex", "label"); !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("AddManager invalid task error = %v", err)
	}
	if err := port.Send(t.Context(), "bad session", "prompt"); !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("Send invalid session error = %v", err)
	}
	if err := port.Recheck(t.Context(), "bad provider"); !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("Recheck invalid provider error = %v", err)
	}
	if fake.addManagerCalls != 0 || fake.actionCalls != 0 {
		t.Fatalf("runtime contacted for invalid identifiers: %#v", fake)
	}
}

func TestTerminalErrorsNeverExposeRawDetails(t *testing.T) {
	fake := &portFake{err: errors.New("credential=canary /private/provider/session.jsonl")}
	port := testTerminalPort(t, fake)
	if _, err := port.Snapshot(t.Context()); !errors.Is(err, runtime.ErrTransport) || err.Error() != runtime.ErrTransport.Error() {
		t.Fatalf("raw error was not reduced to safe transport code: %v", err)
	}

	fake.err = runtime.ErrProviderDisabled
	if err := port.SetEnabled(t.Context(), "codex", true); !errors.Is(err, runtime.ErrProviderDisabled) {
		t.Fatalf("safe runtime code not preserved: %v", err)
	}
}

type recordingAuth struct {
	calls int
	io    connector.AuthIO
	err   error
}

func (*recordingAuth) Probe(context.Context) connector.AuthState {
	return connector.AuthConnected
}

func (auth *recordingAuth) Authenticate(_ context.Context, streams connector.AuthIO) error {
	auth.calls++
	auth.io = streams
	if streams.Out != nil {
		_, _ = streams.Out.Write([]byte("provider-owned auth surface"))
	}
	return auth.err
}

func TestTerminalAuthenticationUsesCurrentViewerIOThenWorkerRecheck(t *testing.T) {
	fake := &portFake{}
	registry := connector.NewRegistry()
	auth := &recordingAuth{}
	if err := registry.Register(connector.Profile{ID: "codex", DisplayName: "Codex", Auth: auth}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	streams := connector.AuthIO{In: bytes.NewBufferString("viewer input"), Out: &out, Err: &out}
	port, err := newTerminalPort(fake, registry, streams)
	if err != nil {
		t.Fatal(err)
	}
	if err := port.Authenticate(t.Context(), "codex"); err != nil {
		t.Fatal(err)
	}
	if auth.calls != 1 || auth.io.In != streams.In || auth.io.Out != streams.Out || auth.io.Err != streams.Err {
		t.Fatalf("viewer auth trace = %#v", auth)
	}
	if fake.authCalls != 0 || fake.recheckCalls != 1 {
		t.Fatalf("worker auth=%d recheck=%d, want 0/1", fake.authCalls, fake.recheckCalls)
	}
	if out.String() != "provider-owned auth surface" {
		t.Fatalf("auth output = %q", out.String())
	}

	auth.err = errors.New("credential=secret provider path=/private/state")
	if err := port.Authenticate(t.Context(), "codex"); !errors.Is(err, runtime.ErrTransport) || err.Error() != runtime.ErrTransport.Error() {
		t.Fatalf("raw auth error escaped: %v", err)
	}
	if fake.authCalls != 0 || fake.recheckCalls != 2 {
		t.Fatalf("failed auth worker trace auth=%d recheck=%d", fake.authCalls, fake.recheckCalls)
	}
}
