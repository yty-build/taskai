// Package acp is the sole boundary around the pinned acpx command-line client.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/identity"
	"github.com/yty-build/acp-gateway/internal/session"
)

const (
	SupportedACPXVersion = "0.13.0"
	maxStructuredLine    = 2 << 20
	maxProviderRefBytes  = 512
)

type PermissionMode string

const (
	PermissionReadApproval PermissionMode = "read-approval"
	PermissionApproveAll   PermissionMode = "approve-all"
)

type Target struct {
	ConsoleSessionID string
	Profile          agents.Profile
	WorkingDirectory string
	Permission       PermissionMode
}

func (t Target) validate() error {
	if strings.TrimSpace(t.ConsoleSessionID) == "" {
		return fmt.Errorf("Task Console Session ID is required")
	}
	if err := t.Profile.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(t.WorkingDirectory) == "" {
		return fmt.Errorf("agent working directory is required")
	}
	if t.Permission != PermissionReadApproval && t.Permission != PermissionApproveAll {
		return fmt.Errorf("invalid ACP permission mode %q", t.Permission)
	}
	return nil
}

type StartSpec struct {
	Target
	ResumeProviderRef string
	Model             string
	ReasoningEffort   string
}

type SessionInfo struct {
	ConsoleSessionID string          `json:"consoleSessionId"`
	RuntimeRef       string          `json:"runtimeRef"`
	ProviderRef      string          `json:"providerRef"`
	Raw              json.RawMessage `json:"raw"`
}

const (
	maxProviderOptionChoices = 128
	maxProviderOptionLabel   = 256
	maxProviderOptionGroup   = 128
)

// ProviderOptionChoice is one provider-advertised select value. Value is the
// exact opaque token sent back through setConfigOption; Label and Group are
// display-only provider text that has passed bounded printable validation.
type ProviderOptionChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
}

// ProviderSelectCatalog describes one configurable select control advertised
// by the ACP adapter. Unsupported controls remain explicit so transports can
// disable, rather than invent, a dropdown.
type ProviderSelectCatalog struct {
	Supported         bool                   `json:"supported"`
	CurrentValue      string                 `json:"current_value,omitempty"`
	UnavailableReason string                 `json:"unavailable_reason,omitempty"`
	Choices           []ProviderOptionChoice `json:"choices"`
}

// ProviderOptionCatalog is disposable live provider state for a Session. It
// is never a persistent domain entity and contains no arbitrary status or
// authentication payload from the adapter.
type ProviderOptionCatalog struct {
	Model           ProviderSelectCatalog `json:"model"`
	ReasoningEffort ProviderSelectCatalog `json:"reasoning_effort"`
}

func (c ProviderOptionCatalog) Validate() error {
	if err := validateProviderSelectCatalog("model", c.Model, false); err != nil {
		return err
	}
	return validateProviderSelectCatalog("reasoning effort", c.ReasoningEffort, true)
}

// ValidateSelection proves that a requested tuple came from this exact live
// catalog. Empty strings mean keep the provider's current/default value.
func (c ProviderOptionCatalog) ValidateSelection(options session.ProviderOptions) error {
	if err := options.Validate(); err != nil {
		return err
	}
	if options.Model != "" && !catalogContains(c.Model, options.Model) {
		return fmt.Errorf("requested model is not advertised by this Session provider")
	}
	if options.ReasoningEffort != "" && !catalogContains(c.ReasoningEffort, options.ReasoningEffort) {
		return fmt.Errorf("requested reasoning effort is not advertised by this Session provider")
	}
	return nil
}

func catalogContains(catalog ProviderSelectCatalog, value string) bool {
	if !catalog.Supported {
		return false
	}
	for _, choice := range catalog.Choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

func validateProviderSelectCatalog(name string, catalog ProviderSelectCatalog, reasoning bool) error {
	if !catalog.Supported {
		if catalog.CurrentValue != "" || len(catalog.Choices) != 0 {
			return fmt.Errorf("unsupported provider %s catalog contains values", name)
		}
		switch catalog.UnavailableReason {
		case "", "not_advertised", "ambiguous_or_invalid":
		default:
			return fmt.Errorf("unsupported provider %s catalog contains an invalid reason", name)
		}
		return nil
	}
	if catalog.UnavailableReason != "" {
		return fmt.Errorf("supported provider %s catalog contains an unavailable reason", name)
	}
	if len(catalog.Choices) == 0 || len(catalog.Choices) > maxProviderOptionChoices {
		return fmt.Errorf("provider %s catalog must contain between 1 and %d choices", name, maxProviderOptionChoices)
	}
	seen := make(map[string]struct{}, len(catalog.Choices))
	for _, choice := range catalog.Choices {
		options := session.ProviderOptions{Model: choice.Value}
		if reasoning {
			options = session.ProviderOptions{ReasoningEffort: choice.Value}
		}
		if err := options.Validate(); err != nil || choice.Value == "" {
			return fmt.Errorf("provider %s catalog contains an invalid choice", name)
		}
		if _, exists := seen[choice.Value]; exists {
			return fmt.Errorf("provider %s catalog contains duplicate choices", name)
		}
		seen[choice.Value] = struct{}{}
		if choice.Label == "" || !boundedPrintable(choice.Label, maxProviderOptionLabel) || !boundedPrintable(choice.Group, maxProviderOptionGroup) {
			return fmt.Errorf("provider %s catalog contains invalid display text", name)
		}
	}
	if catalog.CurrentValue != "" {
		options := session.ProviderOptions{Model: catalog.CurrentValue}
		if reasoning {
			options = session.ProviderOptions{ReasoningEffort: catalog.CurrentValue}
		}
		if err := options.Validate(); err != nil {
			return fmt.Errorf("provider %s catalog contains an invalid current value", name)
		}
	}
	return nil
}

func boundedPrintable(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, item := range value {
		if !unicode.IsPrint(item) {
			return false
		}
	}
	return true
}

// IdentityError means a resumed transport returned a different opaque provider
// reference. Callers must fail closed instead of silently replacing it.
type IdentityError struct {
	Operation string
	Message   string
}

func (e *IdentityError) Error() string {
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = "provider Session"
	}
	return fmt.Sprintf("acpx %s identity error: %s", operation, e.Message)
}

func identityError(operation, format string, arguments ...any) error {
	return &IdentityError{Operation: operation, Message: fmt.Sprintf(format, arguments...)}
}

// ValidateProviderRef proves that a restarted Port exact-resumed the provider
// reference recorded for this Session.
func ValidateProviderRef(operation string, info SessionInfo, expected string) error {
	if err := ValidateProviderRefValue(operation, info.ProviderRef); err != nil {
		return err
	}
	if expected == "" {
		return nil
	}
	if issue := providerRefValidationIssue(expected); issue != "" {
		return identityError(operation, "invalid expected provider reference: %s", issue)
	}
	if info.ProviderRef == expected {
		return nil
	}
	return identityError(operation, "expected provider reference %q, received %q", expected, info.ProviderRef)
}

// ValidateProviderRefValue validates only the bounded opaque token shape. It
// deliberately does not parse or normalize provider-owned identity.
func ValidateProviderRefValue(operation, value string) error {
	if issue := providerRefValidationIssue(value); issue != "" {
		return identityError(operation, "%s", issue)
	}
	return nil
}

func providerRefValidationIssue(value string) string {
	switch {
	case value == "":
		return "missing provider reference"
	case len(value) > maxProviderRefBytes:
		return fmt.Sprintf("provider reference exceeds %d bytes", maxProviderRefBytes)
	case !utf8.ValidString(value):
		return "provider reference is not valid UTF-8"
	default:
		return ""
	}
}

type PromptSpec struct {
	Target
	RequestID string
	Message   string
}

type Event struct {
	EventVersion int             `json:"eventVersion"`
	SessionID    string          `json:"sessionId,omitempty"`
	RequestID    string          `json:"requestId"`
	Seq          uint64          `json:"seq"`
	Stream       string          `json:"stream"`
	Type         string          `json:"type"`
	PermissionID string          `json:"permissionId,omitempty"`
	Raw          json.RawMessage `json:"raw"`
}

type PromptReceipt struct {
	RequestID  string `json:"requestId"`
	LastSeq    uint64 `json:"lastSeq"`
	StopReason string `json:"stopReason,omitempty"`
}

type CommandError struct {
	Operation string
	ExitCode  int
	Stderr    string
	Cause     error
}

func (e *CommandError) Error() string {
	detail := strings.TrimSpace(e.Stderr)
	if detail == "" && e.Cause != nil {
		detail = e.Cause.Error()
	}
	if detail == "" {
		detail = "no error detail"
	}
	return fmt.Sprintf("acpx %s failed (exit %d): %s", e.Operation, e.ExitCode, detail)
}

func (e *CommandError) Unwrap() error { return e.Cause }

type ProtocolError struct {
	Operation string
	Message   string
}

// ProviderTurnError is a correlated, terminal result for one prompt. It does
// not mean the ACP transport or canonical conversation identity was lost.
type ProviderTurnError struct {
	Code       string
	DetailCode string
	Message    string
	Retryable  bool
}

// ProviderPermissionRejectedError means the retained provider Port returned a
// definite, correlated rejection before accepting one permission decision.
// The caller may retry the same pending callback on that same Port. Every
// transport, framing, continuity, or ambiguous response failure remains a
// ProtocolError and makes the retained Port unusable.
type ProviderPermissionRejectedError struct{}

func (e *ProviderPermissionRejectedError) Error() string {
	return "provider rejected the permission response; retry an offered response on the same pending permission"
}

// ProviderConfigurationError is the bounded public failure returned when an
// ACP adapter cannot apply an explicitly requested model or reasoning effort.
// It intentionally does not unwrap the provider/bridge error: provider stderr,
// credentials, paths, and opaque payloads must not escape through worker JSONL
// or local HTTP/CLI transports.
type ProviderConfigurationError struct{}

func (e *ProviderConfigurationError) Error() string {
	return "provider could not apply the requested model or reasoning effort; leave the controls on Provider default or choose values advertised by the provider"
}

// ProviderOptionsError is a bounded discovery failure. Provider status is
// deliberately not wrapped because it may contain paths, credentials, or
// adapter-specific payloads.
type ProviderOptionsError struct{}

func (e *ProviderOptionsError) Error() string {
	return "provider model and reasoning options are unavailable for this Session; retry discovery on the same Session"
}

func (e *ProviderTurnError) Error() string {
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = "provider turn failed"
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		return code + ": " + detail
	}
	return detail
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("acpx %s protocol error: %s", e.Operation, e.Message)
}

// FindBinary locates acpx from ACPX_BINARY, PATH, or the documented user-local
// installation path, in that order.
func FindBinary() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("ACPX_BINARY")); configured != "" {
		return configured, nil
	}
	if path, err := exec.LookPath("acpx"); err == nil {
		return path, nil
	}
	userHome, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(userHome, ".local", "bin", "acpx")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("acpx %s was not found; install with 'npm install --global --prefix ~/.local acpx@%s'", SupportedACPXVersion, SupportedACPXVersion)
}

// probeBinary pins the supported pre-1.0 acpx contract before the runtime
// bridge starts. The gateway has no second detached acpx CLI transport.
func probeBinary(ctx context.Context, binary string) error {
	if strings.TrimSpace(binary) == "" {
		return fmt.Errorf("acpx binary path is required")
	}
	command := exec.CommandContext(ctx, binary, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		return &CommandError{Operation: "version probe", ExitCode: exitCode(err), Stderr: string(output), Cause: err}
	}
	version := strings.TrimSpace(string(output))
	if version != SupportedACPXVersion {
		return fmt.Errorf("unsupported acpx version %q; Agent Task Console V1 requires %s", version, SupportedACPXVersion)
	}
	return nil
}

func NewRequestID() (string, error) { return identity.New("REQ") }

func isBuiltIn(profile agents.Profile) bool {
	if len(profile.Args) != 0 {
		return false
	}
	switch profile.Command {
	case "codex", "claude", "gemini", "grok-build", "opencode":
		return true
	default:
		return false
	}
}

func cloneRaw(value []byte) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
