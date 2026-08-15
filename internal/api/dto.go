package api

import (
	"encoding/json"
	"time"

	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
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
	ID             string  `json:"id"`
	TaskID         string  `json:"task_id"`
	Role           string  `json:"role"`
	Agent          string  `json:"agent"`
	Provider       string  `json:"provider,omitempty"`
	RepoPath       string  `json:"repo_path"`
	WorktreePath   string  `json:"worktree_path,omitempty"`
	Branch         string  `json:"branch,omitempty"`
	Status         string  `json:"status"`
	Permission     string  `json:"permission"`
	WorkerRunning  bool    `json:"worker_running"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	LastActivityAt *string `json:"last_activity_at,omitempty"`
}

type ActivityDTO struct {
	SessionID string `json:"session_id"`
	Position  uint64 `json:"position"`
	RequestID string `json:"request_id,omitempty"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp,omitempty"`
	Activity  string `json:"activity"`
	Cursor    string `json:"cursor,omitempty"`
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
