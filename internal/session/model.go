// Package session defines the Session domain entity and persistence contract.
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MetadataModelKey           = "model"
	MetadataReasoningEffortKey = "reasoning_effort"
)

var (
	reasoningEffortPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ProviderOptions are optional, immutable provider controls stored in a
// Session's metadata. They are ordinary Session configuration, not another
// persistent entity. Empty values deliberately preserve the provider's own
// defaults.
type ProviderOptions struct {
	Model           string
	ReasoningEffort string
}

func (o ProviderOptions) Validate() error {
	if o.Model != "" {
		if len(o.Model) > 256 || !utf8.ValidString(o.Model) {
			return fmt.Errorf("Session model must be a valid provider model ID of at most 256 bytes")
		}
		if strings.HasPrefix(o.Model, "-") {
			return fmt.Errorf("Session model must not begin with '-'")
		}
		for _, value := range o.Model {
			if !unicode.IsPrint(value) {
				return fmt.Errorf("Session model must contain only printable characters")
			}
		}
	}
	if o.ReasoningEffort != "" && !reasoningEffortPattern.MatchString(o.ReasoningEffort) {
		return fmt.Errorf("Session reasoning effort must be a provider value using only letters, numbers, '.', '_', or '-'")
	}
	return nil
}

// ParseProviderOptions reads the bounded public provider controls without
// exposing unrelated Session metadata such as permission policy.
func ParseProviderOptions(metadata json.RawMessage) (ProviderOptions, error) {
	if len(metadata) == 0 {
		return ProviderOptions{}, nil
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		return ProviderOptions{}, fmt.Errorf("decode Session provider options: %w", err)
	}
	read := func(key string) (string, error) {
		value, exists := values[key]
		if !exists || value == nil {
			return "", nil
		}
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("Session %s metadata must be a string", key)
		}
		return strings.TrimSpace(text), nil
	}
	model, err := read(MetadataModelKey)
	if err != nil {
		return ProviderOptions{}, err
	}
	effort, err := read(MetadataReasoningEffortKey)
	if err != nil {
		return ProviderOptions{}, err
	}
	options := ProviderOptions{Model: model, ReasoningEffort: effort}
	return options, options.Validate()
}

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
