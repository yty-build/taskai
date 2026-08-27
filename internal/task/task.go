package task

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxIDBytes bounds a Task identifier while leaving room for UUID-like IDs.
	MaxIDBytes = 128
	// MaxTitleBytes bounds the stored UTF-8 representation of a Task title.
	MaxTitleBytes = 512
	// MaxTitleRunes bounds the displayed character count independently of bytes.
	MaxTitleRunes = 256
)

var (
	ErrInvalidID    = errors.New("invalid task ID")
	ErrInvalidTitle = errors.New("invalid task title")
	ErrInvalidTime  = errors.New("invalid task time")
	ErrInvalidView  = errors.New("invalid task view")
	ErrInvalidCount = errors.New("invalid task count")
)

// ID is TaskAI's opaque identifier for one Task metadata record.
type ID string

// NewID returns a cryptographically random, bounded Task ID.
func NewID() (ID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate task ID: %w", err)
	}
	return ID("task_" + hex.EncodeToString(random[:])), nil
}

// ParseID validates and returns a Task ID without rewriting it.
func ParseID(value string) (ID, error) {
	id := ID(value)
	if err := id.Validate(); err != nil {
		return "", err
	}
	return id, nil
}

// Validate verifies that an ID is non-empty, bounded, and transport-safe.
func (id ID) Validate() error {
	if len(id) == 0 || len(id) > MaxIDBytes {
		return fmt.Errorf("%w: length must be 1..%d bytes", ErrInvalidID, MaxIDBytes)
	}
	for _, char := range []byte(id) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("%w: unsupported byte 0x%02x", ErrInvalidID, char)
	}
	return nil
}

// Task is the complete persistent Task metadata contract.
type Task struct {
	ID        ID
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// New constructs validated Task metadata. The title bytes are preserved exactly.
func New(id ID, title string, createdAt time.Time) (Task, error) {
	createdAt = CanonicalTime(createdAt)
	value := Task{
		ID:        id,
		Title:     title,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := value.Validate(); err != nil {
		return Task{}, err
	}
	return value, nil
}

// Validate verifies Task metadata without trimming or normalizing its title.
func (value Task) Validate() error {
	if err := value.ID.Validate(); err != nil {
		return err
	}
	if err := ValidateTitle(value.Title); err != nil {
		return err
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: timestamps must be non-zero", ErrInvalidTime)
	}
	if value.UpdatedAt.Before(value.CreatedAt) {
		return fmt.Errorf("%w: updated time precedes creation", ErrInvalidTime)
	}
	return nil
}

// WithTitle returns Task metadata with an exact replacement title and updated time.
func (value Task) WithTitle(title string, updatedAt time.Time) (Task, error) {
	updatedAt = CanonicalTime(updatedAt)
	if err := ValidateTitle(title); err != nil {
		return Task{}, err
	}
	if updatedAt.IsZero() || updatedAt.Before(value.CreatedAt) || updatedAt.Before(value.UpdatedAt) {
		return Task{}, fmt.Errorf("%w: updated time is stale", ErrInvalidTime)
	}
	value.Title = title
	value.UpdatedAt = updatedAt
	if err := value.Validate(); err != nil {
		return Task{}, err
	}
	return value, nil
}

// ValidateTitle applies bounded validation without changing title bytes.
func ValidateTitle(title string) error {
	if !utf8.ValidString(title) {
		return fmt.Errorf("%w: invalid UTF-8", ErrInvalidTitle)
	}
	if len(title) == 0 || len(title) > MaxTitleBytes {
		return fmt.Errorf("%w: length must be 1..%d bytes", ErrInvalidTitle, MaxTitleBytes)
	}
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return fmt.Errorf("%w: exceeds %d characters", ErrInvalidTitle, MaxTitleRunes)
	}
	if strings.IndexByte(title, 0) >= 0 {
		return fmt.Errorf("%w: contains NUL", ErrInvalidTitle)
	}
	hasVisible := false
	for _, char := range title {
		if !unicode.IsSpace(char) {
			hasVisible = true
			break
		}
	}
	if !hasVisible {
		return fmt.Errorf("%w: whitespace-only title", ErrInvalidTitle)
	}
	return nil
}

// CanonicalTime removes monotonic data, converts to UTC, and keeps microsecond precision.
func CanonicalTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.Round(0).UTC().Truncate(time.Microsecond)
}

// Counts is the mechanical active/dormant Session aggregate for one Task.
type Counts struct {
	Active  int64
	Dormant int64
}

// Validate rejects impossible negative aggregates.
func (counts Counts) Validate() error {
	if counts.Active < 0 || counts.Dormant < 0 {
		return ErrInvalidCount
	}
	return nil
}

// Summary combines Task metadata with its current mechanical Session counts.
type Summary struct {
	Task
	Counts Counts
}

// View selects the mechanically defined Tasks query surface.
type View string

const (
	// ViewActive includes Tasks with an active Session and new Tasks with no Sessions.
	ViewActive View = "active"
	// ViewAll includes active, dormant, historical, and empty Tasks.
	ViewAll View = "all"
)

// Validate rejects unknown Task views.
func (view View) Validate() error {
	switch view {
	case ViewActive, ViewAll:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidView, view)
	}
}
