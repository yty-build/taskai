package store

import "errors"

var (
	ErrClosed                     = errors.New("metadata store is closed")
	ErrNotFound                   = errors.New("metadata record not found")
	ErrConflict                   = errors.New("metadata record conflicts with existing state")
	ErrStaleUpdate                = errors.New("metadata update time is stale")
	ErrInvalidHierarchy           = errors.New("invalid same-Task Session hierarchy")
	ErrRoleImmutable              = errors.New("Session role is immutable")
	ErrProviderReferenceImmutable = errors.New("provider reference is immutable")
	ErrUnsupportedSchemaVersion   = errors.New("unsupported metadata schema version")
	ErrUnsafeStatePath            = errors.New("unsafe TaskAI state path")
)
