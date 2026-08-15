// Package session defines the Session domain entity and persistence contract.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
	StatusStarting: {}, StatusWorking: {}, StatusWaiting: {}, StatusDone: {}, StatusFailed: {}, StatusCancelled: {}, StatusStopped: {},
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

type Session struct {
	ID              string
	TaskID          string
	Role            string
	Agent           string
	Provider        string
	RepoPath        string
	WorktreePath    string
	Branch          string
	ACPSessionID    string
	NativeSessionID string
	Status          Status
	WorkerPID       int
	WorkerSocket    string
	LogPath         string
	MetadataJSON    json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastActivityAt  *time.Time
}

type CreateParams struct {
	ID              string
	TaskID          string
	Role            string
	Agent           string
	Provider        string
	RepoPath        string
	WorktreePath    string
	Branch          string
	ACPSessionID    string
	NativeSessionID string
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
	return nil
}

type Repository interface {
	CreateSession(context.Context, CreateParams) (Session, error)
	GetSession(context.Context, string) (Session, error)
	ListSessionsByTask(context.Context, string) ([]Session, error)
	UpdateSessionStatus(context.Context, string, Status) (Session, error)
}
