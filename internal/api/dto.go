package api

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/yty-build/acp-gateway/internal/acp"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type TaskDTO struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description,omitempty"`
	Status       string         `json:"status"`
	ExternalRef  string         `json:"external_ref,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SessionCount int            `json:"session_count"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

type SessionDTO struct {
	ID              string  `json:"id"`
	TaskID          string  `json:"task_id"`
	ParentSessionID string  `json:"parent_session_id,omitempty"`
	Role            string  `json:"role"`
	Agent           string  `json:"agent"`
	Provider        string  `json:"provider,omitempty"`
	RepoPath        string  `json:"repo_path"`
	WorktreePath    string  `json:"worktree_path,omitempty"`
	Branch          string  `json:"branch,omitempty"`
	Status          string  `json:"status"`
	Permission      string  `json:"permission"`
	WorkerRunning   bool    `json:"worker_running"`
	Resumable       bool    `json:"resumable"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	LastActivityAt  *string `json:"last_activity_at,omitempty"`
}

// FrameDTO is a mechanical transport envelope around one durable Session
// Channel line. Data remains the normalized provider/runtime payload; the API
// does not project it into a second conversation model.
type FrameDTO struct {
	SessionID string          `json:"session_id"`
	Seq       uint64          `json:"seq"`
	RequestID string          `json:"request_id,omitempty"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	Cursor    string          `json:"cursor,omitempty"`
	Data      json.RawMessage `json:"data"`
}

type PermissionOptionDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type ProviderOptionChoiceDTO struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
}

type ProviderSelectCatalogDTO struct {
	Supported         bool                      `json:"supported"`
	CurrentValue      string                    `json:"current_value,omitempty"`
	UnavailableReason string                    `json:"unavailable_reason,omitempty"`
	Choices           []ProviderOptionChoiceDTO `json:"choices"`
}

type ProviderOptionCatalogDTO struct {
	Model           ProviderSelectCatalogDTO `json:"model"`
	ReasoningEffort ProviderSelectCatalogDTO `json:"reasoning_effort"`
}

func makeProviderOptionCatalogDTO(catalog acp.ProviderOptionCatalog) ProviderOptionCatalogDTO {
	convert := func(source acp.ProviderSelectCatalog) ProviderSelectCatalogDTO {
		result := ProviderSelectCatalogDTO{
			Supported: source.Supported, CurrentValue: source.CurrentValue,
			UnavailableReason: source.UnavailableReason, Choices: make([]ProviderOptionChoiceDTO, 0, len(source.Choices)),
		}
		for _, choice := range source.Choices {
			result.Choices = append(result.Choices, ProviderOptionChoiceDTO{Value: choice.Value, Label: choice.Label, Group: choice.Group})
		}
		return result
	}
	return ProviderOptionCatalogDTO{Model: convert(catalog.Model), ReasoningEffort: convert(catalog.ReasoningEffort)}
}

type PermissionDTO struct {
	ID        string                `json:"id"`
	RequestID string                `json:"request_id,omitempty"`
	Title     string                `json:"title,omitempty"`
	Kind      string                `json:"kind,omitempty"`
	State     string                `json:"state"`
	Decision  string                `json:"decision,omitempty"`
	Options   []PermissionOptionDTO `json:"options,omitempty"`
}

func makePendingPermissionDTO(pending worker.PendingPermission) PermissionDTO {
	result := PermissionDTO{
		ID: pending.ID, RequestID: pending.RequestID,
		Title: safePermissionText(pending.Title, 1024), Kind: safePermissionText(pending.Kind, 64), State: "waiting",
	}
	for _, option := range pending.Options {
		result.Options = append(result.Options, PermissionOptionDTO{
			ID: option.ID, Name: safePermissionText(option.Name, 1024), Kind: option.Kind,
		})
	}
	return result
}

func safePermissionText(value string, limit int) string {
	clean := strings.Map(func(value rune) rune {
		if value == '\n' || value == '\t' || !unicode.IsControl(value) {
			return value
		}
		return -1
	}, value)
	runes := []rune(clean)
	if len(runes) <= limit {
		return clean
	}
	return string(runes[:limit]) + "…"
}

func makeTaskDTO(item task.Task, sessionCount int) TaskDTO {
	var metadata map[string]any
	if len(item.MetadataJSON) > 0 {
		_ = json.Unmarshal(item.MetadataJSON, &metadata)
	}
	return TaskDTO{
		ID: item.ID, Title: item.Title, Description: item.Description, Status: string(item.Status),
		ExternalRef: item.ExternalRef, Metadata: metadata, SessionCount: sessionCount,
		CreatedAt: formatTime(item.CreatedAt), UpdatedAt: formatTime(item.UpdatedAt),
	}
}

func makeSessionDTO(current session.Session) SessionDTO {
	result := SessionDTO{
		ID: current.ID, TaskID: current.TaskID, ParentSessionID: current.ParentSessionID, Role: current.Role, Agent: current.Agent, Provider: current.Provider,
		RepoPath: current.RepoPath, WorktreePath: current.WorktreePath, Branch: current.Branch,
		Status: string(current.Status), Permission: appservice.PermissionLabel(current), WorkerRunning: current.WorkerSocket != "",
		Resumable: current.CanResume(),
		CreatedAt: formatTime(current.CreatedAt), UpdatedAt: formatTime(current.UpdatedAt),
	}
	if current.LastActivityAt != nil {
		value := formatTime(*current.LastActivityAt)
		result.LastActivityAt = &value
	}
	return result
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
