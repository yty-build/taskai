package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

const (
	MaxTasks            = 10_000
	MaxSessions         = 50_000
	MaxProviders        = 1_024
	MaxSnapshotBytes    = 16 << 20
	MaxSubscriberCount  = 64
	MaxClientIDBytes    = 64
	MaxRequestIDBytes   = 32
	MaxWireMessageBytes = 20 << 20
	MaxSocketPathBytes  = 1024
	SocketFilename      = "runtime.sock"
	LockFilename        = "runtime.lock"
)

// MetadataStore is the metadata-only persistence boundary consumed by the
// Supervisor. It deliberately has no content, transcript, replay, event, or
// provider-history methods.
type MetadataStore interface {
	StateDir() string
	CreateTask(context.Context, task.Task) error
	GetTask(context.Context, task.ID) (task.Task, error)
	ListTasks(context.Context, task.View) ([]task.Summary, error)
	CreateSession(context.Context, session.Session) error
	GetSession(context.Context, session.ID) (session.Session, error)
	ListSessions(context.Context, task.ID) ([]session.Session, error)
	UpdateSessionState(context.Context, session.ID, session.State, time.Time) error
	UpdateOwnedTreeState(context.Context, session.ID, session.State, time.Time) error
	SetProviderReference(context.Context, session.ID, []byte, time.Time) error
	PutProviderSettings(context.Context, settings.Provider) error
	GetProviderSettings(context.Context, settings.ProviderID) (settings.Provider, error)
	ListProviderSettings(context.Context) ([]settings.Provider, error)
	SetProviderEnabled(context.Context, settings.ProviderID, bool) error
	SetProviderProbeStatus(context.Context, settings.ProviderID, settings.ProbeStatus) error
}

var _ MetadataStore = (*store.Store)(nil)

// ProviderSpec is immutable composition authority for one registered profile.
// Launch.Argv[0] must be a basename. Persisted argv and capability flags are
// never process-launch authority.
type ProviderSpec struct {
	ID     settings.ProviderID
	Launch settings.LaunchConfig
}

// Clock and ID sources are mechanical dependencies used to make lifecycle
// ordering deterministic without adding a test-only product mode.
type Clock interface{ Now() time.Time }
type TaskIDSource interface{ NewTaskID() (task.ID, error) }
type SessionIDSource interface{ NewSessionID() (session.ID, error) }

type Config struct {
	Store      MetadataStore
	Registry   *connector.Registry
	Managers   *manager.Service
	ProjectCWD string
	Providers  []ProviderSpec
	Clock      Clock
	TaskIDs    TaskIDSource
	SessionIDs SessionIDSource
}

// Task, Session, and Provider are content-free client DTOs. App composition
// may mechanically adapt them to the terminal's public port types.
type Task struct {
	ID        task.ID
	Title     string
	CreatedAt time.Time
	Active    int64
	Dormant   int64
}

type Session struct {
	ID              session.ID
	TaskID          task.ID
	ParentID        session.ID
	Role            session.Role
	Label           string
	ProviderID      settings.ProviderID
	Mode            connector.Mode
	State           session.State
	ResumeAvailable bool
	CreatedAt       time.Time
	LastActiveAt    time.Time
}

type Provider struct {
	ID           settings.ProviderID
	DisplayName  string
	Enabled      bool
	ProbeStatus  settings.ProbeStatus
	Capabilities settings.Capabilities
}

type Snapshot struct {
	Tasks            []Task
	Sessions         []Session
	Providers        []Provider
	StreamGeneration uint64
	ThroughSequence  uint64
}

type Cursor struct {
	Generation uint64
	After      uint64
}

type EventKind uint8

const (
	EventPublicFrame EventKind = iota + 1
	EventSessionUpsert
	EventSessionRemove
)

type FrameKind uint8

const (
	FrameText FrameKind = iota + 1
	FrameUnknown
)

type PublicFrame struct {
	Kind       FrameKind
	PublicType string
	Payload    []byte
}

type Event struct {
	Generation uint64
	Sequence   uint64
	Kind       EventKind
	TaskID     task.ID
	SessionID  session.ID
	Frame      PublicFrame
	Session    Session
}

// ClientPort is the complete runtime surface used by app composition. Prompt
// and public-frame content remain transient call values only.
type ClientPort interface {
	Snapshot(context.Context) (Snapshot, error)
	AddTask(context.Context, string) (task.ID, error)
	AddManager(context.Context, task.ID, settings.ProviderID, string) (session.ID, error)
	SetEnabled(context.Context, settings.ProviderID, bool) error
	Authenticate(context.Context, settings.ProviderID) error
	Recheck(context.Context, settings.ProviderID) error
	Attach(context.Context, task.ID) error
	Detach(context.Context, task.ID) error
	Send(context.Context, session.ID, string) error
	Resume(context.Context, session.ID) error
	Stop(context.Context, session.ID) error
	Next(context.Context, Cursor) (Event, error)
	Close() error
}

// FrameDecoder is intentionally not configurable. It documents the bounded
// public ACP conversion implemented inside runtime rather than exposing a
// semantic provider hook.
type FrameDecoder interface {
	DecodePublicFrame(string, json.RawMessage) (PublicFrame, error)
}
