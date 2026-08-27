package replay

import "errors"

// ErrorCode is a content-free mechanical Replay failure class.
type ErrorCode string

const (
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	CodeUnknownSchema      ErrorCode = "unknown_schema"
	CodeInvalidReference   ErrorCode = "invalid_reference"
	CodeHistoryMissing     ErrorCode = "history_missing"
	CodeReferenceMismatch  ErrorCode = "reference_mismatch"
	CodeContainment        ErrorCode = "containment_failure"
	CodeConcurrentWrite    ErrorCode = "concurrent_write"
	CodeCorruptHistory     ErrorCode = "corrupt_history"
	CodeAmbiguousBoundary  ErrorCode = "ambiguous_boundary"
	CodeRangeUnavailable   ErrorCode = "range_unavailable"
	CodeBoundExceeded      ErrorCode = "bound_exceeded"
	CodeCancelled          ErrorCode = "cancelled"
	CodeSinkFailure        ErrorCode = "sink_failure"
	CodeSourceFailure      ErrorCode = "source_failure"
)

// Error carries no provider reference, native path, provider content, or
// wrapped source error.
type Error struct {
	Code     ErrorCode
	Provider ProviderID
	Profile  ProfileID
}

func (failure *Error) Error() string {
	if failure == nil {
		return "replay: failure"
	}
	return "replay: " + string(failure.Code)
}

// Is permits errors.Is comparisons by code while keeping instances content-free.
func (failure *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && failure != nil && failure.Code == other.Code
}

var (
	ErrUnsupportedVersion = &Error{Code: CodeUnsupportedVersion}
	ErrUnknownSchema      = &Error{Code: CodeUnknownSchema}
	ErrInvalidReference   = &Error{Code: CodeInvalidReference}
	ErrHistoryMissing     = &Error{Code: CodeHistoryMissing}
	ErrReferenceMismatch  = &Error{Code: CodeReferenceMismatch}
	ErrContainment        = &Error{Code: CodeContainment}
	ErrConcurrentWrite    = &Error{Code: CodeConcurrentWrite}
	ErrCorruptHistory     = &Error{Code: CodeCorruptHistory}
	ErrAmbiguousBoundary  = &Error{Code: CodeAmbiguousBoundary}
	ErrRangeUnavailable   = &Error{Code: CodeRangeUnavailable}
	ErrBoundExceeded      = &Error{Code: CodeBoundExceeded}
	ErrCancelled          = &Error{Code: CodeCancelled}
	ErrSinkFailure        = &Error{Code: CodeSinkFailure}
	ErrSourceFailure      = &Error{Code: CodeSourceFailure}
)

func failure(code ErrorCode, profile Profile) error {
	return &Error{Code: code, Provider: profile.Provider, Profile: profile.ID}
}

// CodeOf returns a content-free failure code.
func CodeOf(err error) (ErrorCode, bool) {
	var replayError *Error
	if !errors.As(err, &replayError) {
		return "", false
	}
	return replayError.Code, true
}
