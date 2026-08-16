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

type ConversationIDSource string

const (
	StatusStarting  Status = "starting"
	StatusWorking   Status = "working"
	StatusWaiting   Status = "waiting"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusStopped   Status = "stopped"
)

const (
	ConversationSourceACP           ConversationIDSource = "acp"
	ConversationSourceNative        ConversationIDSource = "native"
	ConversationSourceProviderStore ConversationIDSource = "provider-store"
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

func (s ConversationIDSource) Validate() error {
	switch s {
	case ConversationSourceACP, ConversationSourceNative, ConversationSourceProviderStore:
		return nil
	default:
		return fmt.Errorf("invalid conversation ID source %q", s)
	}
}

type Session struct {
	ID                       string
	TaskID                   string
	Role                     string
	Agent                    string
	Provider                 string
	RepoPath                 string
	WorktreePath             string
	Branch                   string
	ACPXRecordID             string
	ACPSessionID             string
	NativeSessionID          string
	ProviderConversationID   string
	ConversationIDSource     ConversationIDSource
	ConversationIDVerifiedAt *time.Time
	Status                   Status
	WorkerPID                int
	WorkerSocket             string
	LogPath                  string
	MetadataJSON             json.RawMessage
	CreatedAt                time.Time
	UpdatedAt                time.Time
	LastActivityAt           *time.Time
}

type CreateParams struct {
	ID                       string
	TaskID                   string
	Role                     string
	Agent                    string
	Provider                 string
	RepoPath                 string
	WorktreePath             string
	Branch                   string
	ACPXRecordID             string
	ACPSessionID             string
	NativeSessionID          string
	ProviderConversationID   string
	ConversationIDSource     ConversationIDSource
	ConversationIDVerifiedAt *time.Time
	Status                   Status
	WorkerPID                int
	WorkerSocket             string
	LogPath                  string
	MetadataJSON             json.RawMessage
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
	if strings.TrimSpace(p.ProviderConversationID) == "" {
		if p.ConversationIDSource != "" || p.ConversationIDVerifiedAt != nil {
			return fmt.Errorf("canonical provider conversation ID is required when identity metadata is set")
		}
	} else {
		if err := p.ConversationIDSource.Validate(); err != nil {
			return err
		}
		providerStore := p.ConversationIDSource == ConversationSourceProviderStore
		if providerStore && p.ConversationIDVerifiedAt == nil {
			return fmt.Errorf("provider-store conversation identity requires a verification timestamp")
		}
		if !providerStore && p.ConversationIDVerifiedAt != nil {
			return fmt.Errorf("conversation verification timestamp requires provider-store identity source")
		}
		if providerStore && strings.TrimSpace(p.NativeSessionID) != "" && strings.TrimSpace(p.NativeSessionID) != strings.TrimSpace(p.ProviderConversationID) {
			return fmt.Errorf("native adapter ID must match the provider-store verified canonical conversation")
		}
	}
	return nil
}

type Repository interface {
	CreateSession(context.Context, CreateParams) (Session, error)
	GetSession(context.Context, string) (Session, error)
	ListSessionsByTask(context.Context, string) ([]Session, error)
	UpdateSessionStatus(context.Context, string, Status) (Session, error)
	UpdateSessionRuntimeIDs(context.Context, string, string, string, string) (Session, error)
	UpdateSessionConversation(context.Context, string, string, ConversationIDSource, *time.Time) (Session, error)
	UpdateSessionWorker(context.Context, string, int, string, string, Status) (Session, error)
	ClearSessionWorker(context.Context, string, Status) (Session, error)
}
