package api

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"

	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
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
	ID                     string  `json:"id"`
	TaskID                 string  `json:"task_id"`
	Role                   string  `json:"role"`
	Agent                  string  `json:"agent"`
	Provider               string  `json:"provider,omitempty"`
	RepoPath               string  `json:"repo_path"`
	WorktreePath           string  `json:"worktree_path,omitempty"`
	Branch                 string  `json:"branch,omitempty"`
	Status                 string  `json:"status"`
	Permission             string  `json:"permission"`
	WorkerRunning          bool    `json:"worker_running"`
	ConversationReady      bool    `json:"conversation_ready"`
	ConversationVerified   bool    `json:"conversation_verified"`
	ConversationVerifiedAt *string `json:"conversation_verified_at,omitempty"`
	CreatedAt              string  `json:"created_at"`
	UpdatedAt              string  `json:"updated_at"`
	LastActivityAt         *string `json:"last_activity_at,omitempty"`
}

type ActivityDTO struct {
	SessionID  string         `json:"session_id"`
	Position   uint64         `json:"position"`
	RequestID  string         `json:"request_id,omitempty"`
	Kind       string         `json:"kind"`
	Timestamp  string         `json:"timestamp,omitempty"`
	Activity   string         `json:"activity"`
	Cursor     string         `json:"cursor,omitempty"`
	Permission *PermissionDTO `json:"permission,omitempty"`
}

type PermissionOptionDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type PermissionDTO struct {
	ID         string                `json:"id"`
	RequestID  string                `json:"request_id,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
	Title      string                `json:"title,omitempty"`
	Kind       string                `json:"kind,omitempty"`
	State      string                `json:"state"`
	Decision   string                `json:"decision,omitempty"`
	Options    []PermissionOptionDTO `json:"options,omitempty"`
}

func makePendingPermissionDTO(pending worker.PendingPermission) PermissionDTO {
	result := PermissionDTO{
		ID: pending.ID, RequestID: pending.RequestID, ToolCallID: pending.ToolCallID,
		Title: safePermissionText(pending.Title, 1024), Kind: safePermissionText(pending.Kind, 64), State: "waiting",
	}
	for _, option := range pending.Options {
		result.Options = append(result.Options, PermissionOptionDTO{
			ID: option.ID, Name: safePermissionText(option.Name, 1024), Kind: option.Kind,
		})
	}
	return result
}

func makeActivityPermissionDTO(line sessionlog.Line) *PermissionDTO {
	switch line.Type {
	case "permission_requested":
		var pending worker.PendingPermission
		if json.Unmarshal(line.Data, &pending) != nil || pending.ID == "" {
			return nil
		}
		result := makePendingPermissionDTO(pending)
		return &result
	case "permission_resolved", "permission_abandoned":
		var value struct {
			ID       string `json:"permission_id"`
			Decision string `json:"decision"`
			Message  string `json:"message"`
		}
		if json.Unmarshal(line.Data, &value) != nil || value.ID == "" {
			return nil
		}
		state := "resolved"
		decision := value.Decision
		if line.Type == "permission_abandoned" {
			state = "abandoned"
			decision = value.Message
		}
		return &PermissionDTO{ID: value.ID, RequestID: line.RequestID, State: state, Decision: safePermissionText(decision, 1024)}
	default:
		return nil
	}
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
		ID: current.ID, TaskID: current.TaskID, Role: current.Role, Agent: current.Agent, Provider: current.Provider,
		RepoPath: current.RepoPath, WorktreePath: current.WorktreePath, Branch: current.Branch,
		Status: string(current.Status), Permission: appservice.PermissionLabel(current), WorkerRunning: current.WorkerSocket != "",
		ConversationReady: current.ProviderConversationID != "", ConversationVerified: current.ConversationIDVerifiedAt != nil,
		CreatedAt: formatTime(current.CreatedAt), UpdatedAt: formatTime(current.UpdatedAt),
	}
	if current.ConversationIDVerifiedAt != nil {
		value := formatTime(*current.ConversationIDVerifiedAt)
		result.ConversationVerifiedAt = &value
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
