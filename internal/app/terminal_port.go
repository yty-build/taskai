package app

import (
	"context"
	"errors"
	"io"
	"sync"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/tui"
)

// terminalPort is the mechanical, content-free adapter between the local
// runtime transport and the terminal's public ports. It never retains public
// frames and never converts identifiers by title, label, or displayed text.
type terminalPort struct {
	client   runtime.ClientPort
	registry *connector.Registry
	authIO   connector.AuthIO
}

var (
	_ tui.Catalog          = (*terminalPort)(nil)
	_ tui.TaskActions      = (*terminalPort)(nil)
	_ tui.ProviderActions  = (*terminalPort)(nil)
	_ tui.ProviderAuthExec = (*terminalPort)(nil)
	_ tui.RuntimeActions   = (*terminalPort)(nil)
	_ tui.EventSource      = (*terminalPort)(nil)
)

func newTerminalPort(
	client runtime.ClientPort,
	registry *connector.Registry,
	authIO connector.AuthIO,
) (*terminalPort, error) {
	if client == nil || registry == nil {
		return nil, runtime.ErrInvalidRequest
	}
	return &terminalPort{client: client, registry: registry, authIO: authIO}, nil
}

func (port *terminalPort) Snapshot(ctx context.Context) (tui.Snapshot, error) {
	value, err := port.client.Snapshot(ctx)
	if err != nil {
		return tui.Snapshot{}, terminalError(err)
	}
	converted, err := terminalSnapshot(value)
	if err != nil {
		return tui.Snapshot{}, terminalError(err)
	}
	return converted, nil
}

func (port *terminalPort) AddTask(ctx context.Context, title string) (tui.TaskID, error) {
	id, err := port.client.AddTask(ctx, title)
	if err != nil {
		return "", terminalError(err)
	}
	if id.Validate() != nil {
		return "", runtime.ErrTransport
	}
	return tui.TaskID(id), nil
}

func (port *terminalPort) AddManager(
	ctx context.Context,
	taskValue tui.TaskID,
	providerValue tui.ProviderID,
	label string,
) (tui.SessionID, error) {
	taskID, err := domainTaskID(taskValue)
	if err != nil {
		return "", err
	}
	providerID, err := domainProviderID(providerValue)
	if err != nil {
		return "", err
	}
	id, callErr := port.client.AddManager(ctx, taskID, providerID, label)
	if callErr != nil {
		return "", terminalError(callErr)
	}
	if id.Validate() != nil {
		return "", runtime.ErrTransport
	}
	return tui.SessionID(id), nil
}

func (port *terminalPort) SetEnabled(ctx context.Context, value tui.ProviderID, enabled bool) error {
	id, err := domainProviderID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.SetEnabled(ctx, id, enabled))
}

func (port *terminalPort) Authenticate(ctx context.Context, value tui.ProviderID) error {
	command, err := port.AuthenticationCommand(ctx, value)
	if err != nil {
		return err
	}
	return command.Run()
}

// AuthenticationCommand returns the interactive command preferred by the TUI
// through its optional ProviderAuthExec seam. Bubble Tea executes this with
// tea.Exec, which exclusively releases and restores the terminal around the
// provider-owned authentication surface.
func (port *terminalPort) AuthenticationCommand(
	ctx context.Context,
	value tui.ProviderID,
) (tea.ExecCommand, error) {
	id, err := domainProviderID(value)
	if err != nil {
		return nil, err
	}
	profile, err := port.registry.Profile(string(id))
	if err != nil {
		return nil, runtime.ErrProviderUnknown
	}
	if profile.Auth == nil {
		return nil, runtime.ErrUnsupported
	}
	return &providerAuthCommand{
		ctx: ctx, auth: profile.Auth, client: port.client, provider: id,
		in: port.authIO.In, out: port.authIO.Out, err: port.authIO.Err,
	}, nil
}

func (port *terminalPort) Recheck(ctx context.Context, value tui.ProviderID) error {
	id, err := domainProviderID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Recheck(ctx, id))
}

func (port *terminalPort) Attach(ctx context.Context, value tui.TaskID) error {
	id, err := domainTaskID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Attach(ctx, id))
}

func (port *terminalPort) Detach(ctx context.Context, value tui.TaskID) error {
	id, err := domainTaskID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Detach(ctx, id))
}

func (port *terminalPort) Send(ctx context.Context, value tui.SessionID, text string) error {
	id, err := domainSessionID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Send(ctx, id, text))
}

func (port *terminalPort) Resume(ctx context.Context, value tui.SessionID) error {
	id, err := domainSessionID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Resume(ctx, id))
}

func (port *terminalPort) Stop(ctx context.Context, value tui.SessionID) error {
	id, err := domainSessionID(value)
	if err != nil {
		return err
	}
	return terminalError(port.client.Stop(ctx, id))
}

func (port *terminalPort) Next(ctx context.Context, cursor tui.StreamCursor) (tui.PublicEvent, error) {
	event, err := port.client.Next(ctx, runtime.Cursor{Generation: cursor.Generation, After: cursor.After})
	if err != nil {
		return tui.PublicEvent{}, terminalError(err)
	}
	converted, err := terminalEvent(event)
	if err != nil {
		return tui.PublicEvent{}, terminalError(err)
	}
	return converted, nil
}

func terminalSnapshot(value runtime.Snapshot) (tui.Snapshot, error) {
	result := tui.Snapshot{
		Tasks:            make([]tui.Task, 0, len(value.Tasks)),
		Sessions:         make([]tui.Session, 0, len(value.Sessions)),
		Providers:        make([]tui.Provider, 0, len(value.Providers)),
		StreamGeneration: value.StreamGeneration,
		ThroughSequence:  value.ThroughSequence,
	}
	for _, item := range value.Tasks {
		if item.ID.Validate() != nil || item.CreatedAt.IsZero() {
			return tui.Snapshot{}, runtime.ErrTransport
		}
		result.Tasks = append(result.Tasks, tui.Task{
			ID: tui.TaskID(item.ID), Title: item.Title, CreatedAt: item.CreatedAt,
		})
	}
	for _, item := range value.Sessions {
		converted, err := terminalSession(item)
		if err != nil {
			return tui.Snapshot{}, err
		}
		result.Sessions = append(result.Sessions, converted)
	}
	for _, item := range value.Providers {
		converted, err := terminalProvider(item)
		if err != nil {
			return tui.Snapshot{}, err
		}
		result.Providers = append(result.Providers, converted)
	}
	return result, nil
}

func terminalSession(value runtime.Session) (tui.Session, error) {
	if value.ID.Validate() != nil || value.TaskID.Validate() != nil || value.ProviderID.Validate() != nil ||
		value.CreatedAt.IsZero() || value.LastActiveAt.IsZero() {
		return tui.Session{}, runtime.ErrTransport
	}
	result := tui.Session{
		ID: tui.SessionID(value.ID), TaskID: tui.TaskID(value.TaskID), Label: value.Label,
		ProviderID: tui.ProviderID(value.ProviderID), ResumeAvailable: value.ResumeAvailable,
		CreatedAt: value.CreatedAt, LastActive: value.LastActiveAt,
	}
	if value.ParentID != "" {
		if value.ParentID.Validate() != nil {
			return tui.Session{}, runtime.ErrTransport
		}
		result.ParentID = tui.SessionID(value.ParentID)
	}
	switch value.Role {
	case session.RoleManager:
		if value.ParentID != "" || value.Mode != "" {
			return tui.Session{}, runtime.ErrTransport
		}
		result.Role = tui.RoleManager
	case session.RoleSubordinate:
		if value.ParentID == "" || value.ResumeAvailable {
			return tui.Session{}, runtime.ErrTransport
		}
		result.Role = tui.RoleSubordinate
	default:
		return tui.Session{}, runtime.ErrTransport
	}
	switch value.State {
	case session.StateStarting:
		result.State = tui.StateStarting
	case session.StateActive:
		result.State = tui.StateActive
	case session.StateDormant:
		result.State = tui.StateDormant
	case session.StateInterrupted:
		result.State = tui.StateInterrupted
	case session.StateClosed:
		result.State = tui.StateClosed
	default:
		return tui.Session{}, runtime.ErrTransport
	}
	if result.Role == tui.RoleSubordinate {
		switch value.Mode {
		case connector.ModeAnswer:
			result.Mode = tui.ModeAnswer
		case connector.ModeFollow:
			result.Mode = tui.ModeFollow
		default:
			return tui.Session{}, runtime.ErrTransport
		}
	}
	return result, nil
}

func terminalProvider(value runtime.Provider) (tui.Provider, error) {
	if value.ID.Validate() != nil || value.DisplayName == "" || !utf8.ValidString(value.DisplayName) {
		return tui.Provider{}, runtime.ErrTransport
	}
	result := tui.Provider{
		ID: tui.ProviderID(value.ID), Name: value.DisplayName, Enabled: value.Enabled,
		Capabilities: tui.Capabilities{
			Manager: value.Capabilities.Manager, Subordinate: value.Capabilities.Subordinate,
			Resume: value.Capabilities.Resume, Replay: value.Capabilities.Replay,
		},
	}
	switch value.ProbeStatus {
	case settings.ProbeUnknown:
		result.ProbeStatus = tui.ProbeUnknown
	case settings.ProbeChecking:
		result.ProbeStatus = tui.ProbeChecking
	case settings.ProbeConnected:
		result.ProbeStatus = tui.ProbeReady
	case settings.ProbeNotConnected, settings.ProbeDisconnected:
		result.ProbeStatus = tui.ProbeDisconnected
	case settings.ProbeNeedsRepair:
		result.ProbeStatus = tui.ProbeNeedsRepair
	case settings.ProbeMissingConnector:
		result.ProbeStatus = tui.ProbeMissing
	default:
		return tui.Provider{}, runtime.ErrTransport
	}
	return result, nil
}

func terminalEvent(value runtime.Event) (tui.PublicEvent, error) {
	if value.Generation == 0 || value.Sequence == 0 || value.TaskID.Validate() != nil || value.SessionID.Validate() != nil {
		return tui.PublicEvent{}, runtime.ErrTransport
	}
	result := tui.PublicEvent{
		Generation: value.Generation, Sequence: value.Sequence,
		TaskID: tui.TaskID(value.TaskID), SessionID: tui.SessionID(value.SessionID),
	}
	switch value.Kind {
	case runtime.EventPublicFrame:
		result.Kind = tui.EventPublicFrame
		result.Frame.PublicType = value.Frame.PublicType
		result.Frame.Payload = append([]byte(nil), value.Frame.Payload...)
		switch value.Frame.Kind {
		case runtime.FrameText:
			result.Frame.Kind = tui.FrameText
		case runtime.FrameUnknown:
			result.Frame.Kind = tui.FrameUnknown
		default:
			return tui.PublicEvent{}, runtime.ErrTransport
		}
	case runtime.EventSessionUpsert:
		result.Kind = tui.EventSessionUpsert
		converted, err := terminalSession(value.Session)
		if err != nil {
			return tui.PublicEvent{}, err
		}
		if converted.ID != result.SessionID || converted.TaskID != result.TaskID {
			return tui.PublicEvent{}, runtime.ErrTransport
		}
		result.Session = converted
	case runtime.EventSessionRemove:
		result.Kind = tui.EventSessionRemove
	default:
		return tui.PublicEvent{}, runtime.ErrTransport
	}
	return result, nil
}

func domainTaskID(value tui.TaskID) (task.ID, error) {
	id := task.ID(value)
	if id.Validate() != nil {
		return "", runtime.ErrInvalidRequest
	}
	return id, nil
}

func domainSessionID(value tui.SessionID) (session.ID, error) {
	id := session.ID(value)
	if id.Validate() != nil {
		return "", runtime.ErrInvalidRequest
	}
	return id, nil
}

func domainProviderID(value tui.ProviderID) (settings.ProviderID, error) {
	id := settings.ProviderID(value)
	if id.Validate() != nil {
		return "", runtime.ErrInvalidRequest
	}
	return id, nil
}

// terminalError is an allowlist projection. A connector, store, or local
// transport error can therefore never make its raw path, argv, environment,
// credential, prompt, or frame content render in the terminal.
func terminalError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return runtime.ErrCancelled
	}
	for _, safe := range []error{
		runtime.ErrInvalidRequest, runtime.ErrNotFound, runtime.ErrConflict, runtime.ErrUnavailable,
		runtime.ErrProviderUnknown, runtime.ErrProviderDisabled, runtime.ErrUnsupported,
		runtime.ErrInvalidState, runtime.ErrPersistence, runtime.ErrTransport, runtime.ErrBusy,
		runtime.ErrCapacity, runtime.ErrCursor, runtime.ErrEndpointBusy, runtime.ErrEndpointUnsafe,
		runtime.ErrPeerUnauthorized, runtime.ErrUnsupportedPlatform, runtime.ErrCancelled, runtime.ErrClosed,
	} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return runtime.ErrTransport
}

// providerAuthCommand is tea.ExecCommand-compatible. Authentication executes
// locally against the current viewer streams; only the subsequent mechanical
// Recheck crosses into the persistent worker. It contains no credential,
// prompt, answer, replay, or provider-history buffer.
type providerAuthCommand struct {
	ctx      context.Context
	auth     connector.AuthPort
	client   runtime.ClientPort
	provider settings.ProviderID

	mu  sync.Mutex
	ran bool
	in  io.Reader
	out io.Writer
	err io.Writer
}

func (command *providerAuthCommand) SetStdin(value io.Reader) {
	command.mu.Lock()
	defer command.mu.Unlock()
	if !command.ran {
		command.in = value
	}
}

func (command *providerAuthCommand) SetStdout(value io.Writer) {
	command.mu.Lock()
	defer command.mu.Unlock()
	if !command.ran {
		command.out = value
	}
}

func (command *providerAuthCommand) SetStderr(value io.Writer) {
	command.mu.Lock()
	defer command.mu.Unlock()
	if !command.ran {
		command.err = value
	}
}

func (command *providerAuthCommand) Run() error {
	if command == nil || command.auth == nil || command.client == nil || command.ctx == nil {
		return runtime.ErrInvalidRequest
	}
	command.mu.Lock()
	if command.ran {
		command.mu.Unlock()
		return runtime.ErrInvalidState
	}
	command.ran = true
	streams := connector.AuthIO{In: command.in, Out: command.out, Err: command.err}
	command.mu.Unlock()

	authErr := command.auth.Authenticate(command.ctx, streams)
	// Recheck runs even after provider-owned auth failure so the metadata row
	// reports the connector's current mechanical status when the TUI refreshes.
	recheckErr := command.client.Recheck(command.ctx, command.provider)
	if authErr != nil {
		return terminalError(authErr)
	}
	return terminalError(recheckErr)
}

var _ tea.ExecCommand = (*providerAuthCommand)(nil)
