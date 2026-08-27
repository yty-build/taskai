package manager

import (
	"context"
	"errors"

	"github.com/yty-build/acp-gateway/internal/connector"
)

// Code is a bounded, content-free diagnostic code. Provider errors, command
// lines, native references, paths, environment, and ACP content never enter a
// Code or SafeError string.
type Code string

const (
	MaxDiagnosticCodeBytes = 64

	CodeInvalidRequest      Code = "invalid_request"
	CodeProviderUnknown     Code = "provider_unknown"
	CodeUnsupported         Code = "capability_unsupported"
	CodeAlreadyActive       Code = "manager_already_active"
	CodeUnavailable         Code = "manager_unavailable"
	CodeBusy                Code = "manager_busy"
	CodeCapacity            Code = "transient_capacity"
	CodeFrameDelivery       Code = "frame_delivery_failed"
	CodeMalformedCompletion Code = "malformed_completion"
	CodeTransport           Code = "transport_failed"
	CodeCancelled           Code = "cancelled"
	CodeStopFailed          Code = "stop_failed"
	CodeStopped             Code = "stopped"
)

// SafeError exposes only one finite mechanical code.
type SafeError struct{ code Code }

func (err SafeError) Error() string { return "manager: " + string(err.code) }

// SafeCode supports callers that render bounded diagnostics without exposing
// an underlying provider error.
func (err SafeError) SafeCode() string { return string(err.code) }

func (err SafeError) Is(target error) bool {
	other, ok := target.(SafeError)
	return ok && err.code == other.code
}

func (code Code) valid() bool {
	switch code {
	case CodeInvalidRequest, CodeProviderUnknown, CodeUnsupported, CodeAlreadyActive,
		CodeUnavailable, CodeBusy, CodeCapacity, CodeFrameDelivery,
		CodeMalformedCompletion, CodeTransport, CodeCancelled, CodeStopFailed, CodeStopped:
		return true
	default:
		return false
	}
}

var (
	ErrInvalidRequest      = SafeError{code: CodeInvalidRequest}
	ErrProviderUnknown     = SafeError{code: CodeProviderUnknown}
	ErrUnsupported         = SafeError{code: CodeUnsupported}
	ErrAlreadyActive       = SafeError{code: CodeAlreadyActive}
	ErrUnavailable         = SafeError{code: CodeUnavailable}
	ErrBusy                = SafeError{code: CodeBusy}
	ErrCapacity            = SafeError{code: CodeCapacity}
	ErrFrameDelivery       = SafeError{code: CodeFrameDelivery}
	ErrMalformedCompletion = SafeError{code: CodeMalformedCompletion}
	ErrTransport           = SafeError{code: CodeTransport}
	ErrCancelled           = SafeError{code: CodeCancelled}
	ErrStopFailed          = SafeError{code: CodeStopFailed}
)

// CodeOf returns one safe diagnostic code for an error returned by this
// package. Unknown errors collapse to transport_failed without retaining or
// formatting their text.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var safe SafeError
	if errors.As(err, &safe) {
		return safe.code
	}
	return CodeTransport
}

func sanitizeError(err error) error {
	if err == nil {
		return nil
	}
	var safe SafeError
	if errors.As(err, &safe) {
		return safe
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ErrCancelled
	case errors.Is(err, connector.ErrProviderUnknown):
		return ErrProviderUnknown
	case errors.Is(err, connector.ErrProviderUnsupported):
		return ErrUnsupported
	case errors.Is(err, connector.ErrChildBusy):
		return ErrBusy
	case errors.Is(err, connector.ErrCapacity), errors.Is(err, connector.ErrAnswerLimit):
		return ErrCapacity
	case errors.Is(err, connector.ErrMalformedCompletion):
		return ErrMalformedCompletion
	case errors.Is(err, connector.ErrInvalidOperation), errors.Is(err, connector.ErrOwnership):
		return ErrInvalidRequest
	default:
		return ErrTransport
	}
}
