package runtime

import "errors"

// Code is a finite, content-free diagnostic suitable for the terminal and
// local transport. Raw provider, store, command, environment, path, prompt,
// and frame data never enters a Code or SafeError.
type Code string

const (
	MaxDiagnosticCodeBytes = 64

	CodeInvalidRequest      Code = "invalid_request"
	CodeNotFound            Code = "not_found"
	CodeConflict            Code = "conflict"
	CodeUnavailable         Code = "unavailable"
	CodeProviderUnknown     Code = "provider_unknown"
	CodeProviderDisabled    Code = "provider_disabled"
	CodeUnsupported         Code = "capability_unsupported"
	CodeInvalidState        Code = "invalid_state"
	CodePersistence         Code = "persistence_failed"
	CodeTransport           Code = "transport_failed"
	CodeBusy                Code = "busy"
	CodeCapacity            Code = "capacity_exceeded"
	CodeCursor              Code = "invalid_event_cursor"
	CodeEndpointBusy        Code = "endpoint_busy"
	CodeEndpointUnsafe      Code = "unsafe_endpoint"
	CodePeerUnauthorized    Code = "peer_unauthorized"
	CodeUnsupportedPlatform Code = "unsupported_platform"
	CodeCancelled           Code = "cancelled"
	CodeClosed              Code = "closed"
)

// SafeError exposes only one bounded mechanical code.
type SafeError struct{ code Code }

func (err SafeError) Error() string    { return "runtime: " + string(err.code) }
func (err SafeError) SafeCode() string { return string(err.code) }
func (err SafeError) Code() Code       { return err.code }

func (err SafeError) Is(target error) bool {
	other, ok := target.(SafeError)
	return ok && err.code == other.code
}

var (
	ErrInvalidRequest      = SafeError{code: CodeInvalidRequest}
	ErrNotFound            = SafeError{code: CodeNotFound}
	ErrConflict            = SafeError{code: CodeConflict}
	ErrUnavailable         = SafeError{code: CodeUnavailable}
	ErrProviderUnknown     = SafeError{code: CodeProviderUnknown}
	ErrProviderDisabled    = SafeError{code: CodeProviderDisabled}
	ErrUnsupported         = SafeError{code: CodeUnsupported}
	ErrInvalidState        = SafeError{code: CodeInvalidState}
	ErrPersistence         = SafeError{code: CodePersistence}
	ErrTransport           = SafeError{code: CodeTransport}
	ErrBusy                = SafeError{code: CodeBusy}
	ErrCapacity            = SafeError{code: CodeCapacity}
	ErrCursor              = SafeError{code: CodeCursor}
	ErrEndpointBusy        = SafeError{code: CodeEndpointBusy}
	ErrEndpointUnsafe      = SafeError{code: CodeEndpointUnsafe}
	ErrPeerUnauthorized    = SafeError{code: CodePeerUnauthorized}
	ErrUnsupportedPlatform = SafeError{code: CodeUnsupportedPlatform}
	ErrCancelled           = SafeError{code: CodeCancelled}
	ErrClosed              = SafeError{code: CodeClosed}
)

func codeError(code Code) error {
	if !validRuntimeCode(code, false) {
		return ErrTransport
	}
	return SafeError{code: code}
}

func codeOf(err error) Code {
	if err == nil {
		return ""
	}
	var safe interface{ SafeCode() string }
	if errors.As(err, &safe) {
		code := Code(safe.SafeCode())
		if validRuntimeCode(code, false) {
			return code
		}
	}
	return CodeTransport
}

func validRuntimeCode(code Code, allowEmpty bool) bool {
	if code == "" {
		return allowEmpty
	}
	if len(code) > MaxDiagnosticCodeBytes {
		return false
	}
	switch code {
	case CodeInvalidRequest, CodeNotFound, CodeConflict, CodeUnavailable,
		CodeProviderUnknown, CodeProviderDisabled, CodeUnsupported, CodeInvalidState,
		CodePersistence, CodeTransport, CodeBusy, CodeCapacity, CodeCursor,
		CodeEndpointBusy, CodeEndpointUnsafe, CodePeerUnauthorized,
		CodeUnsupportedPlatform, CodeCancelled, CodeClosed:
		return true
	default:
		return false
	}
}
