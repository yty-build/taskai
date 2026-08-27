package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TaskID and SessionID are opaque metadata identifiers. The TUI never derives
// identity from titles, labels, answers, or any other displayed content.
type TaskID string
type SessionID string
type ProviderID string

// SessionRole is the mechanically stored role of a Session.
type SessionRole string

const (
	RoleManager     SessionRole = "manager"
	RoleSubordinate SessionRole = "subordinate"
)

// SessionState is the finite metadata state rendered by the TUI.
type SessionState string

const (
	StateStarting    SessionState = "starting"
	StateActive      SessionState = "active"
	StateDormant     SessionState = "dormant"
	StateInterrupted SessionState = "interrupted"
	StateClosed      SessionState = "closed"
)

// InvocationMode is shown only when supplied mechanically for a subordinate.
type InvocationMode string

const (
	ModeAnswer InvocationMode = "answer"
	ModeFollow InvocationMode = "follow"
)

// ProbeStatus is a connector's mechanical Settings status. Authentication is
// never inferred from this value and is initiated only by Authenticate.
type ProbeStatus string

const (
	ProbeUnknown      ProbeStatus = "unknown"
	ProbeReady        ProbeStatus = "ready"
	ProbeChecking     ProbeStatus = "checking"
	ProbeDisconnected ProbeStatus = "disconnected"
	ProbeNeedsRepair  ProbeStatus = "needs-repair"
	ProbeMissing      ProbeStatus = "missing"
	ProbeUnsupported  ProbeStatus = "unsupported"
)

// Capabilities contains only conformance-derived connector claims.
type Capabilities struct {
	Manager     bool
	Subordinate bool
	Resume      bool
	Replay      bool
}

// Task is metadata needed by the Tasks screen.
type Task struct {
	ID        TaskID
	Title     string
	CreatedAt time.Time
}

// Session is metadata needed by a Task workspace. ParentID is empty for a
// Manager and identifies a same-Task Manager for a subordinate.
// ResumeAvailable is an exact-Session connector conformance result, never an
// inference from historical text or provider prose.
type Session struct {
	ID              SessionID
	TaskID          TaskID
	ParentID        SessionID
	Role            SessionRole
	Label           string
	ProviderID      ProviderID
	Mode            InvocationMode
	State           SessionState
	ResumeAvailable bool
	CreatedAt       time.Time
	LastActive      time.Time
}

// Provider is the complete Settings-facing provider surface. It intentionally
// has no credential, command, argv, environment, path, or generic-value field.
type Provider struct {
	ID           ProviderID
	Name         string
	Enabled      bool
	ProbeStatus  ProbeStatus
	Capabilities Capabilities
}

// Snapshot is a content-free metadata snapshot. StreamGeneration changes when
// a new ordered event stream supersedes the old one. ThroughSequence is the
// highest event sequence atomically reflected in Tasks, Sessions, and
// Providers. Catalog implementations must capture the metadata and watermark
// in one consistency boundary so an older refresh cannot erase a newer event.
type Snapshot struct {
	Tasks            []Task
	Sessions         []Session
	Providers        []Provider
	StreamGeneration uint64
	ThroughSequence  uint64
}

// Catalog supplies metadata. Implementations must not place ACP content in a
// Snapshot or retain content on behalf of the TUI. Snapshot must return an
// atomic StreamGeneration/ThroughSequence watermark with its metadata.
type Catalog interface {
	Snapshot(context.Context) (Snapshot, error)
}

// TaskActions owns Task and top-level Manager creation. AddManager receives
// only an enabled, Manager-conformant provider selected in Settings.
type TaskActions interface {
	AddTask(context.Context, string) (TaskID, error)
	AddManager(context.Context, TaskID, ProviderID, string) (SessionID, error)
}

// ProviderActions exposes the only three provider-setting mutations. In
// particular, there is no surface for authoring launch arguments or secrets.
type ProviderActions interface {
	SetEnabled(context.Context, ProviderID, bool) error
	Authenticate(context.Context, ProviderID) error
	Recheck(context.Context, ProviderID) error
}

// ProviderAuthExec is an optional interactive authentication boundary. When
// present, the model runs the provider-owned command through tea.Exec so the
// provider has exclusive terminal ownership and Bubble Tea releases/restores
// raw mode around it. ProviderActions.Authenticate remains the deterministic
// non-interactive fallback used by tests and ports without an interactive
// command surface.
type ProviderAuthExec interface {
	AuthenticationCommand(context.Context, ProviderID) (tea.ExecCommand, error)
}

// RuntimeActions owns viewer attachment, exact-manager input and Resume, and
// explicit Manager-tree stop. Resume must use the same opaque provider Session
// and must never fall back to Start. Detach must not stop a runtime or close its
// ACP port.
type RuntimeActions interface {
	Attach(context.Context, TaskID) error
	Detach(context.Context, TaskID) error
	Send(context.Context, SessionID, string) error
	Resume(context.Context, SessionID) error
	Stop(context.Context, SessionID) error
}

// StreamCursor identifies the next item in one connector-owned ordered stream.
type StreamCursor struct {
	Generation uint64
	After      uint64
}

// EventKind identifies content-free metadata changes and public manager ACP
// frames. It is deliberately not a semantic agent-event taxonomy.
type EventKind uint8

const (
	EventPublicFrame EventKind = iota + 1
	EventSessionUpsert
	EventSessionRemove
)

// FrameKind selects a bounded renderer for a public ACP frame. Unknown frames
// are not parsed; only PublicType and payload size are displayed.
type FrameKind uint8

const (
	FrameText FrameKind = iota + 1
	FrameUnknown
)

// PublicFrame is transient public ACP data. Payload is used only by the known
// text renderer. Unknown payload bytes are measured and discarded by the TUI.
type PublicFrame struct {
	Kind       FrameKind
	PublicType string
	Payload    []byte
}

// PublicEvent is delivered exactly once in global Sequence order. A frame's
// SessionID must name a Manager. Session changes contain no ACP content.
type PublicEvent struct {
	Generation uint64
	Sequence   uint64
	Kind       EventKind
	TaskID     TaskID
	SessionID  SessionID
	Frame      PublicFrame
	Session    Session
}

// EventSource is a push-driven one-shot wait. The model re-arms it only after
// accepting the preceding event; Generation and Sequence share the exact
// namespace used by Snapshot's watermark. Implementations may block until an
// event or context cancellation and must apply backpressure rather than drop
// frames.
type EventSource interface {
	Next(context.Context, StreamCursor) (PublicEvent, error)
}

// SafeError optionally supplies a bounded, non-content diagnostic code. Raw
// error strings are never rendered because provider errors may contain paths,
// commands, credentials, environment values, or conversation content.
type SafeError interface {
	error
	SafeCode() string
}

// Limits bounds every RAM-only content surface. Non-positive custom values use
// defaults; positive values are always clamped to package hard maxima.
type Limits struct {
	DraftBytes          int
	FrameBytes          int
	UnknownTypeBytes    int
	PaneBytes           int
	PaneFrames          int
	AggregateDraftBytes int
	AggregatePaneBytes  int
	AggregatePaneFrames int
}

const (
	hardMaxDraftBytes          = 16 << 10
	hardMaxFrameBytes          = 64 << 10
	hardMaxUnknownTypeBytes    = 1 << 10
	hardMaxPaneBytes           = 256 << 10
	hardMaxPaneFrames          = 2 << 10
	hardMaxAggregateDraftBytes = 256 << 10
	hardMaxAggregatePaneBytes  = 4 << 20
	hardMaxAggregatePaneFrames = 16 << 10
)

// DefaultLimits returns production-safe RAM bounds.
func DefaultLimits() Limits {
	return Limits{
		DraftBytes:          4 << 10,
		FrameBytes:          16 << 10,
		UnknownTypeBytes:    256,
		PaneBytes:           64 << 10,
		PaneFrames:          512,
		AggregateDraftBytes: 64 << 10,
		AggregatePaneBytes:  1 << 20,
		AggregatePaneFrames: 4 << 10,
	}
}

// Config supplies consumer-owned ports to the production Bubble Tea model.
type Config struct {
	Context   context.Context
	Catalog   Catalog
	Tasks     TaskActions
	Providers ProviderActions
	Runtime   RuntimeActions
	Events    EventSource
	Limits    Limits
}

var errInvalidSnapshot = errors.New("invalid metadata snapshot")

const (
	maxIDBytes               = 512
	maxTaskTitleBytes        = 4 << 10
	maxSessionLabelBytes     = 4 << 10
	maxProviderNameBytes     = 1 << 10
	maxSnapshotTasks         = 10_000
	maxSnapshotSessions      = 50_000
	maxSnapshotProviders     = 1_024
	maxSnapshotMetadataBytes = 16 << 20
)

func normalizeLimits(in Limits) Limits {
	out := DefaultLimits()
	if in.DraftBytes > 0 {
		out.DraftBytes = min(in.DraftBytes, hardMaxDraftBytes)
	}
	if in.FrameBytes > 0 {
		out.FrameBytes = min(in.FrameBytes, hardMaxFrameBytes)
	}
	if in.UnknownTypeBytes > 0 {
		out.UnknownTypeBytes = min(in.UnknownTypeBytes, hardMaxUnknownTypeBytes)
	}
	if in.PaneBytes > 0 {
		out.PaneBytes = min(in.PaneBytes, hardMaxPaneBytes)
	}
	if in.PaneFrames > 0 {
		out.PaneFrames = min(in.PaneFrames, hardMaxPaneFrames)
	}
	if in.AggregateDraftBytes > 0 {
		out.AggregateDraftBytes = min(in.AggregateDraftBytes, hardMaxAggregateDraftBytes)
	}
	if in.AggregatePaneBytes > 0 {
		out.AggregatePaneBytes = min(in.AggregatePaneBytes, hardMaxAggregatePaneBytes)
	}
	if in.AggregatePaneFrames > 0 {
		out.AggregatePaneFrames = min(in.AggregatePaneFrames, hardMaxAggregatePaneFrames)
	}
	out.DraftBytes = min(out.DraftBytes, out.AggregateDraftBytes)
	out.PaneBytes = min(out.PaneBytes, out.AggregatePaneBytes)
	out.PaneFrames = min(out.PaneFrames, out.AggregatePaneFrames)
	return out
}

func validateSnapshot(s Snapshot) error {
	if len(s.Tasks) > maxSnapshotTasks || len(s.Sessions) > maxSnapshotSessions || len(s.Providers) > maxSnapshotProviders {
		return fmt.Errorf("%w: metadata count exceeds hard limit", errInvalidSnapshot)
	}
	metadataBytes := 0
	tasks := make(map[TaskID]struct{}, len(s.Tasks))
	for _, task := range s.Tasks {
		metadataBytes += len(task.ID) + len(task.Title)
		if metadataBytes > maxSnapshotMetadataBytes {
			return fmt.Errorf("%w: metadata bytes exceed hard limit", errInvalidSnapshot)
		}
		if task.ID == "" || len(task.ID) > maxIDBytes || len(task.Title) > maxTaskTitleBytes {
			return fmt.Errorf("%w: empty task id", errInvalidSnapshot)
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("%w: duplicate task id", errInvalidSnapshot)
		}
		tasks[task.ID] = struct{}{}
	}

	sessions := make(map[SessionID]Session, len(s.Sessions))
	for _, session := range s.Sessions {
		metadataBytes += len(session.ID) + len(session.TaskID) + len(session.ParentID) + len(session.Label) + len(session.ProviderID)
		if metadataBytes > maxSnapshotMetadataBytes {
			return fmt.Errorf("%w: metadata bytes exceed hard limit", errInvalidSnapshot)
		}
		if err := validateSessionShape(session); err != nil {
			return err
		}
		if _, exists := sessions[session.ID]; exists {
			return fmt.Errorf("%w: duplicate session id", errInvalidSnapshot)
		}
		if _, exists := tasks[session.TaskID]; !exists {
			return fmt.Errorf("%w: session task not found", errInvalidSnapshot)
		}
		sessions[session.ID] = session
	}
	for _, session := range s.Sessions {
		if session.Role != RoleSubordinate {
			continue
		}
		parent, exists := sessions[session.ParentID]
		if !exists || parent.Role != RoleManager || parent.TaskID != session.TaskID {
			return fmt.Errorf("%w: subordinate parent is not same-task manager", errInvalidSnapshot)
		}
	}

	providers := make(map[ProviderID]Provider, len(s.Providers))
	for _, provider := range s.Providers {
		metadataBytes += len(provider.ID) + len(provider.Name)
		if metadataBytes > maxSnapshotMetadataBytes {
			return fmt.Errorf("%w: metadata bytes exceed hard limit", errInvalidSnapshot)
		}
		if provider.ID == "" || len(provider.ID) > maxIDBytes || len(provider.Name) > maxProviderNameBytes {
			return fmt.Errorf("%w: empty provider id", errInvalidSnapshot)
		}
		if _, exists := providers[provider.ID]; exists {
			return fmt.Errorf("%w: duplicate provider id", errInvalidSnapshot)
		}
		if !validProbe(provider.ProbeStatus) {
			return fmt.Errorf("%w: invalid provider probe status", errInvalidSnapshot)
		}
		providers[provider.ID] = provider
	}
	for _, session := range s.Sessions {
		if !session.ResumeAvailable {
			continue
		}
		provider, exists := providers[session.ProviderID]
		if !exists || !provider.Capabilities.Resume {
			return fmt.Errorf("%w: Session Resume is not connector-conformant", errInvalidSnapshot)
		}
	}
	return nil
}

func validateSessionShape(session Session) error {
	if session.ID == "" || len(session.ID) > maxIDBytes || session.TaskID == "" || len(session.TaskID) > maxIDBytes ||
		len(session.ParentID) > maxIDBytes || session.ProviderID == "" || len(session.ProviderID) > maxIDBytes ||
		len(session.Label) > maxSessionLabelBytes {
		return fmt.Errorf("%w: invalid Session identity", errInvalidSnapshot)
	}
	if !validState(session.State) {
		return fmt.Errorf("%w: invalid session state", errInvalidSnapshot)
	}
	switch session.Role {
	case RoleManager:
		if session.ParentID != "" || session.Mode != "" {
			return fmt.Errorf("%w: invalid manager hierarchy", errInvalidSnapshot)
		}
	case RoleSubordinate:
		if session.ParentID == "" || !validMode(session.Mode) || session.ResumeAvailable {
			return fmt.Errorf("%w: invalid subordinate metadata", errInvalidSnapshot)
		}
	default:
		return fmt.Errorf("%w: invalid session role", errInvalidSnapshot)
	}
	return nil
}

func validState(state SessionState) bool {
	switch state {
	case StateStarting, StateActive, StateDormant, StateInterrupted, StateClosed:
		return true
	default:
		return false
	}
}

func validMode(mode InvocationMode) bool {
	return mode == ModeAnswer || mode == ModeFollow
}

func validProbe(status ProbeStatus) bool {
	switch status {
	case ProbeUnknown, ProbeReady, ProbeChecking, ProbeDisconnected, ProbeNeedsRepair, ProbeMissing, ProbeUnsupported:
		return true
	default:
		return false
	}
}

func taskLess(a, b Task) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID > b.ID
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func sortedTasks(tasks []Task) []Task {
	out := append([]Task(nil), tasks...)
	sort.SliceStable(out, func(i, j int) bool { return taskLess(out[i], out[j]) })
	return out
}

func sessionLess(a, b Session) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

func sortedSessions(sessions []Session) []Session {
	out := append([]Session(nil), sessions...)
	sort.SliceStable(out, func(i, j int) bool { return sessionLess(out[i], out[j]) })
	return out
}
