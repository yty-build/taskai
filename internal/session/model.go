// Package session defines the Session domain entity and persistence contract.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MetadataAllowSharedKey    = "allow_shared_checkout"
	MetadataCreateWorktreeKey = "create_worktree"
)

// ProviderOptions are optional controls selected from the live provider's
// advertised catalog. Empty values preserve provider defaults.
type ProviderOptions struct {
	Model           string
	ReasoningEffort string
}

func (o ProviderOptions) Validate() error {
	if err := validateProviderOption("model", o.Model, 256); err != nil {
		return err
	}
	return validateProviderOption("reasoning effort", o.ReasoningEffort, 128)
}

func validateProviderOption(name, value string, limit int) error {
	if value == "" {
		return nil
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return fmt.Errorf("Session %s must be a valid provider value of at most %d bytes", name, limit)
	}
	for _, item := range value {
		if !unicode.IsPrint(item) {
			return fmt.Errorf("Session %s must contain only printable characters", name)
		}
	}
	return nil
}

type Status string

const (
	StatusStarting  Status = "starting"
	StatusWorking   Status = "working"
	StatusWaiting   Status = "waiting"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusStopped   Status = "stopped"
)

var validStatuses = map[Status]struct{}{
	StatusStarting: {}, StatusWorking: {}, StatusWaiting: {}, StatusDone: {},
	StatusFailed: {}, StatusCancelled: {}, StatusStopped: {},
}

func (s Status) Validate() error {
	if _, ok := validStatuses[s]; !ok {
		return fmt.Errorf("invalid Session status %q", s)
	}
	return nil
}

func ParseStatus(value string) (Status, error) {
	status := Status(strings.ToLower(strings.TrimSpace(value)))
	return status, status.Validate()
}

// Session is the gateway address for one provider-owned conversation. The
// provider reference is opaque; the gateway does not parse provider history
// or model native/ACP evidence as separate identities.
type Session struct {
	ID              string
	TaskID          string
	ParentSessionID string
	Role            string
	Agent           string
	Provider        string
	RepoPath        string
	WorktreePath    string
	Branch          string
	RuntimeRef      string
	ProviderRef     string
	Status          Status
	WorkerPID       int
	WorkerSocket    string
	LogPath         string
	MetadataJSON    json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastActivityAt  *time.Time
}

// WorkerState is the complete compare-and-swap guard for Session lifecycle
// ownership. Status alone is not an ownership token: a stopped/failed worker
// can overlap a newly claimed worker unless every lifecycle write also proves
// the exact PID and socket it observed.
type WorkerState struct {
	Status Status
	PID    int
	Socket string
}

func (s Session) WorkerState() WorkerState {
	return WorkerState{Status: s.Status, PID: s.WorkerPID, Socket: s.WorkerSocket}
}

// CanResume is true only for an ownerless terminal Session with an established
// provider identity. In particular, the ownerless starting window between a
// lifecycle admission CAS and worker claim is not a second Resume opportunity.
func (s Session) CanResume() bool {
	if s.ProviderRef == "" || s.WorkerPID != 0 || s.WorkerSocket != "" {
		return false
	}
	switch s.Status {
	case StatusStopped, StatusFailed, StatusDone, StatusCancelled:
		return true
	default:
		return false
	}
}

type CreateParams struct {
	ID              string
	TaskID          string
	ParentSessionID string
	Role            string
	Agent           string
	Provider        string
	RepoPath        string
	WorktreePath    string
	Branch          string
	RuntimeRef      string
	ProviderRef     string
	Status          Status
	WorkerPID       int
	WorkerSocket    string
	LogPath         string
	MetadataJSON    json.RawMessage
}

func (p CreateParams) Validate() error {
	if strings.TrimSpace(p.TaskID) == "" {
		return fmt.Errorf("Session Task ID is required")
	}
	if strings.TrimSpace(p.Agent) == "" {
		return fmt.Errorf("Session agent is required")
	}
	if p.ParentSessionID != "" && (strings.TrimSpace(p.ParentSessionID) != p.ParentSessionID || p.ParentSessionID == p.ID || strings.ContainsAny(p.ParentSessionID, `/\\`)) {
		return fmt.Errorf("Session parent ID is invalid")
	}
	status := p.Status
	if status == "" {
		status = StatusStarting
	}
	if err := status.Validate(); err != nil {
		return err
	}
	if len(p.MetadataJSON) > 0 && !json.Valid(p.MetadataJSON) {
		return fmt.Errorf("Session metadata_json is not valid JSON")
	}
	if strings.TrimSpace(p.RuntimeRef) != p.RuntimeRef {
		return fmt.Errorf("Session runtime reference must not contain surrounding whitespace")
	}
	return nil
}

type Repository interface {
	CreateSession(context.Context, CreateParams) (Session, error)
	GetSession(context.Context, string) (Session, error)
	ListSessionsByTask(context.Context, string) ([]Session, error)
	CompareAndSwapSessionWorker(context.Context, string, WorkerState, WorkerState, string) (Session, bool, error)
	CompareAndSwapSessionProviderRef(context.Context, string, WorkerState, string, string) (Session, bool, error)
}
