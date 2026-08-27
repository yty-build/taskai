package connector

import "errors"

var (
	ErrInvalidCommand      = errors.New("connector: invalid literal command")
	ErrLaunchFailed        = errors.New("connector: executable launch failed")
	ErrProcessFailed       = errors.New("connector: executable exited unsuccessfully")
	ErrProviderUnknown     = errors.New("connector: provider is not registered")
	ErrProviderUnsupported = errors.New("connector: provider boundary is not conformed")
	ErrInvalidOperation    = errors.New("connector: invalid taskai_delegate operation")
	ErrOwnership           = errors.New("connector: subordinate ownership rejected")
	ErrStaleTurn           = errors.New("connector: turn correlation is stale or unknown")
	ErrCapacity            = errors.New("connector: transient capacity reached")
	ErrChildBusy           = errors.New("connector: subordinate already has an active turn")
	ErrMalformedCompletion = errors.New("connector: malformed native completion")
	ErrTransport           = errors.New("connector: subordinate transport failed")
	ErrAnswerLimit         = errors.New("connector: public answer exceeds transient bound")
	ErrReplayUnavailable   = errors.New("connector: bounded Replay is unavailable")
)

// BoundaryError deliberately reports only a mechanical stage. Provider
// command lines, environment, opaque content, and native references never
// enter Error strings.
type BoundaryError struct {
	Stage string
	Err   error
}

func (e *BoundaryError) Error() string {
	if e == nil || e.Stage == "" {
		return "connector: boundary failed"
	}
	return "connector: " + e.Stage + " failed"
}

func (e *BoundaryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
