package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/task"
)

const (
	MaxIDBytes                = 128
	MaxLabelBytes             = 512
	MaxLabelRunes             = 256
	MaxProviderIDBytes        = 128
	MaxProviderReferenceBytes = 4096
)

var (
	ErrInvalidID                  = errors.New("invalid session ID")
	ErrInvalidRole                = errors.New("invalid session role")
	ErrInvalidLabel               = errors.New("invalid session label")
	ErrInvalidProvider            = errors.New("invalid provider ID")
	ErrInvalidProviderReference   = errors.New("invalid provider reference")
	ErrProviderReferenceImmutable = errors.New("provider reference is immutable")
	ErrInvalidState               = errors.New("invalid session state")
	ErrInvalidTime                = errors.New("invalid session time")
	ErrInvalidHierarchy           = errors.New("invalid session hierarchy")
)

// ID is TaskAI's opaque identifier for one Session metadata record.
type ID string

// NewID returns a cryptographically random, bounded Session ID.
func NewID() (ID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return ID("session_" + hex.EncodeToString(random[:])), nil
}

// ParseID validates and returns a Session ID without rewriting it.
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

// Role is the immutable position of a Session in the Task hierarchy.
type Role string

const (
	RoleManager     Role = "manager"
	RoleSubordinate Role = "subordinate"
)

// Validate rejects roles outside the finite V1 role set.
func (role Role) Validate() error {
	switch role {
	case RoleManager, RoleSubordinate:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}

// State is the finite mechanical lifecycle state persisted for a Session.
type State string

const (
	StateStarting    State = "starting"
	StateActive      State = "active"
	StateDormant     State = "dormant"
	StateInterrupted State = "interrupted"
	StateClosed      State = "closed"
)

// Validate rejects states outside the finite mechanical lifecycle set.
func (state State) Validate() error {
	switch state {
	case StateStarting, StateActive, StateDormant, StateInterrupted, StateClosed:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidState, state)
	}
}

// Activity is the aggregate class used by the Tasks screen.
type Activity string

const (
	ActivityActive  Activity = "active"
	ActivityDormant Activity = "dormant"
)

// Activity returns active for starting/active and dormant for all terminal or idle states.
func (state State) Activity() (Activity, error) {
	if err := state.Validate(); err != nil {
		return "", err
	}
	switch state {
	case StateStarting, StateActive:
		return ActivityActive, nil
	default:
		return ActivityDormant, nil
	}
}

// Session is the complete persistent Session metadata contract.
type Session struct {
	ID                ID
	TaskID            task.ID
	ParentID          *ID
	Role              Role
	Label             string
	ProviderID        string
	ProviderReference []byte
	State             State
	CreatedAt         time.Time
	LastActiveAt      time.Time
}

// NewManager constructs a top-level manager Session.
func NewManager(id ID, taskID task.ID, label, providerID string, providerReference []byte, state State, createdAt time.Time) (Session, error) {
	return newSession(id, taskID, nil, RoleManager, label, providerID, providerReference, state, createdAt)
}

// NewSubordinate constructs a depth-one subordinate Session.
func NewSubordinate(id ID, taskID task.ID, parentID ID, label, providerID string, providerReference []byte, state State, createdAt time.Time) (Session, error) {
	parentCopy := parentID
	return newSession(id, taskID, &parentCopy, RoleSubordinate, label, providerID, providerReference, state, createdAt)
}

func newSession(id ID, taskID task.ID, parentID *ID, role Role, label, providerID string, providerReference []byte, state State, createdAt time.Time) (Session, error) {
	createdAt = task.CanonicalTime(createdAt)
	value := Session{
		ID:                id,
		TaskID:            taskID,
		ParentID:          cloneParent(parentID),
		Role:              role,
		Label:             label,
		ProviderID:        providerID,
		ProviderReference: cloneBytes(providerReference),
		State:             state,
		CreatedAt:         createdAt,
		LastActiveAt:      createdAt,
	}
	if err := value.Validate(); err != nil {
		return Session{}, err
	}
	return value, nil
}

// Validate verifies finite state, bounded metadata, and role/parent shape.
func (value Session) Validate() error {
	if err := value.ID.Validate(); err != nil {
		return err
	}
	if err := value.TaskID.Validate(); err != nil {
		return err
	}
	if err := value.Role.Validate(); err != nil {
		return err
	}
	if err := ValidateLabel(value.Label); err != nil {
		return err
	}
	if err := ValidateProviderID(value.ProviderID); err != nil {
		return err
	}
	if err := ValidateProviderReference(value.ProviderReference); err != nil {
		return err
	}
	if err := value.State.Validate(); err != nil {
		return err
	}
	if value.CreatedAt.IsZero() || value.LastActiveAt.IsZero() || value.LastActiveAt.Before(value.CreatedAt) {
		return ErrInvalidTime
	}
	switch value.Role {
	case RoleManager:
		if value.ParentID != nil {
			return fmt.Errorf("%w: manager cannot have a parent", ErrInvalidHierarchy)
		}
	case RoleSubordinate:
		if value.ParentID == nil {
			return fmt.Errorf("%w: subordinate requires a parent", ErrInvalidHierarchy)
		}
		if err := value.ParentID.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidHierarchy, err)
		}
		if *value.ParentID == value.ID {
			return fmt.Errorf("%w: Session cannot parent itself", ErrInvalidHierarchy)
		}
	}
	return nil
}

// ValidateHierarchy proves that child is directly owned by parent in the same Task.
func ValidateHierarchy(child, parent Session) error {
	if err := child.Validate(); err != nil {
		return err
	}
	if err := parent.Validate(); err != nil {
		return err
	}
	if child.Role != RoleSubordinate || child.ParentID == nil {
		return fmt.Errorf("%w: child is not subordinate", ErrInvalidHierarchy)
	}
	if parent.Role != RoleManager || parent.ParentID != nil {
		return fmt.Errorf("%w: parent is not a top-level manager", ErrInvalidHierarchy)
	}
	if child.TaskID != parent.TaskID {
		return fmt.Errorf("%w: Task IDs differ", ErrInvalidHierarchy)
	}
	if *child.ParentID != parent.ID || child.ID == parent.ID {
		return fmt.Errorf("%w: parent identity differs", ErrInvalidHierarchy)
	}
	return nil
}

// WithState returns metadata with a finite state and monotonic last-active time.
func (value Session) WithState(state State, lastActiveAt time.Time) (Session, error) {
	if err := state.Validate(); err != nil {
		return Session{}, err
	}
	lastActiveAt = task.CanonicalTime(lastActiveAt)
	if lastActiveAt.IsZero() || lastActiveAt.Before(value.CreatedAt) || lastActiveAt.Before(value.LastActiveAt) {
		return Session{}, fmt.Errorf("%w: last-active time is stale", ErrInvalidTime)
	}
	value.State = state
	value.LastActiveAt = lastActiveAt
	if err := value.Validate(); err != nil {
		return Session{}, err
	}
	return value, nil
}

// WithProviderReference sets an initially absent opaque provider reference or accepts an idempotent repeat.
func (value Session) WithProviderReference(reference []byte, lastActiveAt time.Time) (Session, error) {
	if err := ValidateProviderReference(reference); err != nil {
		return Session{}, err
	}
	if len(reference) == 0 {
		return Session{}, fmt.Errorf("%w: empty reference", ErrInvalidProviderReference)
	}
	if len(value.ProviderReference) != 0 && !bytes.Equal(value.ProviderReference, reference) {
		return Session{}, ErrProviderReferenceImmutable
	}
	lastActiveAt = task.CanonicalTime(lastActiveAt)
	if lastActiveAt.IsZero() || lastActiveAt.Before(value.CreatedAt) || lastActiveAt.Before(value.LastActiveAt) {
		return Session{}, fmt.Errorf("%w: last-active time is stale", ErrInvalidTime)
	}
	value.ProviderReference = cloneBytes(reference)
	value.LastActiveAt = lastActiveAt
	if err := value.Validate(); err != nil {
		return Session{}, err
	}
	return value, nil
}

// ValidateLabel accepts an empty optional label and otherwise preserves bounded UTF-8 bytes exactly.
func ValidateLabel(label string) error {
	if label == "" {
		return nil
	}
	if !utf8.ValidString(label) {
		return fmt.Errorf("%w: invalid UTF-8", ErrInvalidLabel)
	}
	if len(label) > MaxLabelBytes || utf8.RuneCountInString(label) > MaxLabelRunes {
		return fmt.Errorf("%w: exceeds bounds", ErrInvalidLabel)
	}
	if strings.IndexByte(label, 0) >= 0 {
		return fmt.Errorf("%w: contains NUL", ErrInvalidLabel)
	}
	hasVisible := false
	for _, char := range label {
		if !unicode.IsSpace(char) {
			hasVisible = true
			break
		}
	}
	if !hasVisible {
		return fmt.Errorf("%w: whitespace-only label", ErrInvalidLabel)
	}
	return nil
}

// ValidateProviderID verifies a bounded connector identifier without changing it.
func ValidateProviderID(providerID string) error {
	if len(providerID) == 0 || len(providerID) > MaxProviderIDBytes {
		return fmt.Errorf("%w: length must be 1..%d bytes", ErrInvalidProvider, MaxProviderIDBytes)
	}
	for _, char := range []byte(providerID) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("%w: unsupported byte 0x%02x", ErrInvalidProvider, char)
	}
	return nil
}

// ValidateProviderReference verifies only size; the BLOB remains semantically opaque.
func ValidateProviderReference(reference []byte) error {
	if len(reference) > MaxProviderReferenceBytes {
		return fmt.Errorf("%w: exceeds %d bytes", ErrInvalidProviderReference, MaxProviderReferenceBytes)
	}
	return nil
}

// Clone returns a defensive copy of Session metadata containing slice/pointer fields.
func (value Session) Clone() Session {
	value.ParentID = cloneParent(value.ParentID)
	value.ProviderReference = cloneBytes(value.ProviderReference)
	return value
}

func cloneParent(parent *ID) *ID {
	if parent == nil {
		return nil
	}
	copy := *parent
	return &copy
}

func cloneBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return bytes.Clone(value)
}
