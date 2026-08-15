// Package task defines the Task domain entity and persistence contract.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusOpen      Status = "open"
	StatusWorking   Status = "working"
	StatusReview    Status = "review"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

var validStatuses = map[Status]struct{}{
	StatusOpen: {}, StatusWorking: {}, StatusReview: {}, StatusDone: {}, StatusFailed: {}, StatusCancelled: {},
}

func (s Status) Validate() error {
	if _, ok := validStatuses[s]; !ok {
		return fmt.Errorf("invalid Task status %q", s)
	}
	return nil
}

func ParseStatus(value string) (Status, error) {
	status := Status(strings.ToLower(strings.TrimSpace(value)))
	return status, status.Validate()
}

type Task struct {
	ID           string
	Title        string
	Description  string
	Status       Status
	ExternalRef  string
	MetadataJSON json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type CreateParams struct {
	ID           string
	Title        string
	Description  string
	Status       Status
	ExternalRef  string
	MetadataJSON json.RawMessage
}

func (p CreateParams) Validate() error {
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("Task title is required")
	}
	status := p.Status
	if status == "" {
		status = StatusOpen
	}
	if err := status.Validate(); err != nil {
		return err
	}
	if len(p.MetadataJSON) > 0 && !json.Valid(p.MetadataJSON) {
		return fmt.Errorf("Task metadata_json is not valid JSON")
	}
	return nil
}

type Repository interface {
	CreateTask(context.Context, CreateParams) (Task, error)
	GetTask(context.Context, string) (Task, error)
	ListTasks(context.Context) ([]Task, error)
	UpdateTaskStatus(context.Context, string, Status) (Task, error)
}
