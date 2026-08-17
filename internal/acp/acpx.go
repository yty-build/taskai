// Package acp is the sole boundary around the pinned acpx command-line client.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yty-build/acp-gateway/internal/agents"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/identity"
)

const (
	SupportedACPXVersion = "0.13.0"
	RuntimeTransport     = "acpx-runtime-v1"
	maxStructuredLine    = 16 << 20
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
	ResumeACPSessionID             string
	ExpectedProviderConversationID string
	ExpectedNativeSessionID        string
	ProviderStoreVerified          bool
	Model                          string
	ReasoningEffort                string
}

type SessionInfo struct {
	ConsoleSessionID string          `json:"consoleSessionId"`
	ACPXRecordID     string          `json:"acpxRecordId"`
	ACPSessionID     string          `json:"acpSessionId"`
	NativeSessionID  string          `json:"nativeSessionId,omitempty"`
	Raw              json.RawMessage `json:"raw"`
}

// IdentityError means a transport observation cannot be proven to belong to
// the immutable provider conversation recorded for a Task Console Session.
// Callers must quarantine that transport rather than continue optimistically.
type IdentityError struct {
	Operation string
	Message   string
}

func (e *IdentityError) Error() string {
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = "provider conversation"
	}
	return fmt.Sprintf("acpx %s identity error: %s", operation, e.Message)
}

func identityError(operation, format string, arguments ...any) error {
	return &IdentityError{Operation: operation, Message: fmt.Sprintf(format, arguments...)}
}

// ValidateProviderConversation proves that adapter metadata still names the
// immutable provider conversation recorded by the Task Console Session. ACP
// and provider-native identifiers are distinct protocol concepts, so either
// exact identifier may carry the canonical value.
func ValidateProviderConversation(operation string, info SessionInfo, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if strings.TrimSpace(info.ACPSessionID) == expected || strings.TrimSpace(info.NativeSessionID) == expected {
		return nil
	}
	if strings.TrimSpace(operation) == "" {
		operation = "provider conversation"
	}
	return identityError(operation, "expected canonical provider conversation %q, received ACP %q and native %q", expected, info.ACPSessionID, info.NativeSessionID)
}

// ValidateNativeSessionEvidence prevents an adapter from silently replacing
// a previously observed native identifier while retaining the same ACP ID.
func ValidateNativeSessionEvidence(operation string, info SessionInfo, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	if strings.TrimSpace(info.NativeSessionID) == expected {
		return nil
	}
	if strings.TrimSpace(operation) == "" {
		operation = "native Session evidence"
	}
	return identityError(operation, "expected native adapter ID %q, received %q", expected, info.NativeSessionID)
}

// ValidateSessionIdentity checks every stable identity field together. Once a
// provider-store lookup has verified the canonical ID, a newly observed native
// ID must equal that canonical ID; matching ACP evidence cannot mask drift.
func ValidateSessionIdentity(operation string, info SessionInfo, expectedACP, expectedNative, canonical string, providerStoreVerified bool) error {
	expectedACP = strings.TrimSpace(expectedACP)
	if expectedACP != "" && strings.TrimSpace(info.ACPSessionID) != expectedACP {
		return identityError(operation, "expected ACP Session ID %q, received %q", expectedACP, info.ACPSessionID)
	}
	if err := ValidateProviderConversation(operation, info, canonical); err != nil {
		return err
	}
	if err := ValidateNativeSessionEvidence(operation, info, expectedNative); err != nil {
		return err
	}
	canonical = strings.TrimSpace(canonical)
	nativeID := strings.TrimSpace(info.NativeSessionID)
	if providerStoreVerified && canonical != "" && nativeID != "" && nativeID != canonical {
		return identityError(operation, "provider-store verified canonical conversation %q, received native adapter ID %q", canonical, nativeID)
	}
	return nil
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
	Raw          json.RawMessage `json:"raw"`
}

type PromptReceipt struct {
	RequestID  string `json:"requestId"`
	LastSeq    uint64 `json:"lastSeq"`
	StopReason string `json:"stopReason,omitempty"`
}

type Status struct {
	State           string          `json:"state"`
	Summary         string          `json:"summary,omitempty"`
	ACPXRecordID    string          `json:"acpxRecordId,omitempty"`
	ACPSessionID    string          `json:"acpSessionId,omitempty"`
	NativeSessionID string          `json:"nativeSessionId,omitempty"`
	Raw             json.RawMessage `json:"raw"`
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

// ProviderConfigurationError is the bounded public failure returned when an
// ACP adapter cannot apply an explicitly requested model or reasoning effort.
// It intentionally does not unwrap the provider/bridge error: provider stderr,
// credentials, paths, and opaque payloads must not escape through worker JSONL
// or local HTTP/CLI transports.
type ProviderConfigurationError struct{}

func (e *ProviderConfigurationError) Error() string {
	return "provider could not apply the requested model or reasoning effort; leave the controls on Provider default or choose values advertised by the provider"
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

type Client struct {
	binary   string
	verified atomic.Bool
}

func NewClient(binary string) *Client {
	return &Client{binary: binary}
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

// Probe pins the supported pre-1.0 acpx contract before any operation runs.
func (c *Client) Probe(ctx context.Context) error {
	if strings.TrimSpace(c.binary) == "" {
		return fmt.Errorf("acpx binary path is required")
	}
	command := exec.CommandContext(ctx, c.binary, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		return &CommandError{Operation: "version probe", ExitCode: exitCode(err), Stderr: string(output), Cause: err}
	}
	version := strings.TrimSpace(string(output))
	if version != SupportedACPXVersion {
		return fmt.Errorf("unsupported acpx version %q; Agent Task Console V1 requires %s", version, SupportedACPXVersion)
	}
	c.verified.Store(true)
	return nil
}

func NewRequestID() (string, error) { return identity.New("REQ") }

func (c *Client) Start(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	target, err := c.ready(spec.Target)
	if err != nil {
		return SessionInfo{}, err
	}
	spec.Target = target
	operation := []string{"sessions", "ensure", "--name", spec.ConsoleSessionID}
	if spec.ResumeACPSessionID != "" {
		operation = append(operation, "--resume-session", spec.ResumeACPSessionID)
	}
	raw, err := c.runControl(ctx, "session ensure", spec.Target, operation...)
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfo(spec.ConsoleSessionID, raw)
}

func (c *Client) Resume(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	if strings.TrimSpace(spec.ResumeACPSessionID) == "" {
		return SessionInfo{}, fmt.Errorf("ACP Session ID is required to resume")
	}
	return c.Start(ctx, spec)
}

func (c *Client) Inspect(ctx context.Context, target Target) (SessionInfo, error) {
	target, err := c.ready(target)
	if err != nil {
		return SessionInfo{}, err
	}
	raw, err := c.runControl(ctx, "session inspect", target, "sessions", "show", target.ConsoleSessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfo(target.ConsoleSessionID, raw)
}

func (c *Client) Status(ctx context.Context, target Target) (Status, error) {
	target, err := c.ready(target)
	if err != nil {
		return Status{}, err
	}
	raw, err := c.runControl(ctx, "status", target, "status", "--session", target.ConsoleSessionID)
	if err != nil {
		return Status{}, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Status{}, &ProtocolError{Operation: "status", Message: err.Error()}
	}
	if stringField(payload, "status") == "" {
		return Status{}, &ProtocolError{Operation: "status", Message: "missing status"}
	}
	return Status{
		State: stringField(payload, "status"), Summary: stringField(payload, "summary"),
		ACPXRecordID: stringField(payload, "acpxRecordId"), ACPSessionID: firstString(payload, "acpSessionId", "acpxSessionId"),
		NativeSessionID: stringField(payload, "agentSessionId"), Raw: cloneRaw(raw),
	}, nil
}

func (c *Client) Cancel(ctx context.Context, target Target) error {
	target, err := c.ready(target)
	if err != nil {
		return err
	}
	_, err = c.runControl(ctx, "cancel", target, "cancel", "--session", target.ConsoleSessionID)
	return err
}

func (c *Client) Prompt(ctx context.Context, spec PromptSpec, emit func(Event) error) (PromptReceipt, error) {
	target, err := c.ready(spec.Target)
	if err != nil {
		return PromptReceipt{}, err
	}
	spec.Target = target
	if strings.TrimSpace(spec.Message) == "" {
		return PromptReceipt{}, fmt.Errorf("prompt message is required")
	}
	if spec.RequestID == "" {
		requestID, err := NewRequestID()
		if err != nil {
			return PromptReceipt{}, err
		}
		spec.RequestID = requestID
	}
	if emit == nil {
		emit = func(Event) error { return nil }
	}

	args := c.commandArgs(spec.Target, "prompt", "--session", spec.ConsoleSessionID, "--", spec.Message)
	command := exec.CommandContext(ctx, c.binary, args...)
	command.Dir = spec.WorkingDirectory
	command.Env = commandEnvironment(spec.Profile.Environment)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return PromptReceipt{}, fmt.Errorf("open acpx prompt stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return PromptReceipt{}, fmt.Errorf("open acpx prompt stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		return PromptReceipt{}, &CommandError{Operation: "prompt", ExitCode: -1, Cause: err}
	}

	var stderrBuffer bytes.Buffer
	var stderrWait sync.WaitGroup
	stderrWait.Add(1)
	go func() {
		defer stderrWait.Done()
		_, _ = io.Copy(&stderrBuffer, stderr)
	}()

	receipt := PromptReceipt{RequestID: spec.RequestID}
	var streamErr error
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxStructuredLine)
	for scanner.Scan() {
		receipt.LastSeq++
		raw := cloneRaw(scanner.Bytes())
		if !json.Valid(raw) {
			streamErr = &ProtocolError{Operation: "prompt", Message: fmt.Sprintf("malformed JSON at stream line %d", receipt.LastSeq)}
			break
		}
		event, remoteErr := eventFromRaw(spec, receipt.LastSeq, raw)
		if err := emit(event); err != nil {
			streamErr = fmt.Errorf("consume acpx prompt event: %w", err)
			break
		}
		if remoteErr != nil {
			streamErr = remoteErr
			break
		}
		if reason := stopReason(raw); reason != "" {
			receipt.StopReason = reason
		}
	}
	if err := scanner.Err(); err != nil && streamErr == nil {
		streamErr = &ProtocolError{Operation: "prompt", Message: err.Error()}
	}
	if streamErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	stderrWait.Wait()
	if streamErr != nil {
		return receipt, streamErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return receipt, ctx.Err()
		}
		return receipt, &CommandError{Operation: "prompt", ExitCode: exitCode(waitErr), Stderr: stderrBuffer.String(), Cause: waitErr}
	}
	return receipt, nil
}

func (c *Client) ready(target Target) (Target, error) {
	if !c.verified.Load() {
		return Target{}, fmt.Errorf("acpx compatibility has not been probed")
	}
	if err := target.validate(); err != nil {
		return Target{}, err
	}
	workingDirectory, err := gitrepo.CanonicalPath(target.WorkingDirectory)
	if err != nil {
		return Target{}, fmt.Errorf("resolve agent working directory %q: %w", target.WorkingDirectory, err)
	}
	target.WorkingDirectory = workingDirectory
	return target, nil
}

func (c *Client) runControl(ctx context.Context, label string, target Target, operation ...string) (json.RawMessage, error) {
	args := c.commandArgs(target, operation...)
	command := exec.CommandContext(ctx, c.binary, args...)
	command.Dir = target.WorkingDirectory
	command.Env = commandEnvironment(target.Profile.Environment)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if raw, parseErr := singleJSONLine(stdout.Bytes()); parseErr == nil {
			if remote := jsonRPCError(label, raw); remote != nil {
				return nil, remote
			}
		}
		detail := stderr.String()
		if strings.TrimSpace(detail) == "" {
			detail = stdout.String()
		}
		return nil, &CommandError{Operation: label, ExitCode: exitCode(err), Stderr: detail, Cause: err}
	}
	raw, err := singleJSONLine(stdout.Bytes())
	if err != nil {
		return nil, &ProtocolError{Operation: label, Message: err.Error()}
	}
	if remote := jsonRPCError(label, raw); remote != nil {
		return nil, remote
	}
	return raw, nil
}

func (c *Client) commandArgs(target Target, operation ...string) []string {
	args := []string{"--cwd", target.WorkingDirectory, "--format", "json", "--json-strict", "--auth-policy", "skip", "--ttl", "300"}
	if target.Permission == PermissionApproveAll {
		args = append(args, "--approve-all")
	} else {
		args = append(args, "--approve-reads", "--non-interactive-permissions", "fail")
	}
	if isBuiltIn(target.Profile) {
		args = append(args, target.Profile.Command)
	} else {
		args = append(args, "--agent", encodeAgentCommand(target.Profile.Argv()))
	}
	return append(args, operation...)
}

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

// encodeAgentCommand produces the quoting understood by acpx's parser. The
// value is passed as one argv item to acpx; no shell evaluates it.
func encodeAgentCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		arg = strings.ReplaceAll(arg, `\`, `\\`)
		arg = strings.ReplaceAll(arg, `"`, `\"`)
		quoted[i] = `"` + arg + `"`
	}
	return strings.Join(quoted, " ")
}

func commandEnvironment(overrides map[string]string) []string {
	environment := make(map[string]string)
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			environment[name] = value
		}
	}
	for name, value := range overrides {
		environment[name] = value
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	// Stable environments make subprocess fixture assertions deterministic.
	sortStrings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func singleJSONLine(output []byte) (json.RawMessage, error) {
	lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
	if len(lines) != 1 || len(lines[0]) == 0 {
		return nil, fmt.Errorf("expected one JSON object, received %d lines", len(lines))
	}
	if !json.Valid(lines[0]) {
		return nil, fmt.Errorf("malformed JSON output")
	}
	return cloneRaw(lines[0]), nil
}

func parseSessionInfo(consoleID string, raw json.RawMessage) (SessionInfo, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return SessionInfo{}, &ProtocolError{Operation: "session metadata", Message: err.Error()}
	}
	acpxRecordID := stringField(payload, "acpxRecordId")
	if acpxRecordID == "" {
		return SessionInfo{}, &ProtocolError{Operation: "session metadata", Message: "missing acpxRecordId"}
	}
	return SessionInfo{
		ConsoleSessionID: consoleID, ACPXRecordID: acpxRecordID,
		ACPSessionID:    firstString(payload, "acpSessionId", "acpxSessionId"),
		NativeSessionID: stringField(payload, "agentSessionId"), Raw: cloneRaw(raw),
	}, nil
}

func eventFromRaw(spec PromptSpec, sequence uint64, raw json.RawMessage) (Event, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Event{}, err
	}
	event := Event{
		EventVersion: 1, SessionID: findSessionID(payload), RequestID: spec.RequestID,
		Seq: sequence, Stream: "prompt", Type: eventType(payload), Raw: cloneRaw(raw),
	}
	return event, jsonRPCError("prompt", raw)
}

func eventType(payload map[string]any) string {
	if value := stringField(payload, "type"); value != "" {
		return value
	}
	if method := stringField(payload, "method"); method != "" {
		if method == "session/update" {
			if params, ok := payload["params"].(map[string]any); ok {
				if update, ok := params["update"].(map[string]any); ok {
					if kind := stringField(update, "sessionUpdate"); kind != "" {
						return kind
					}
				}
			}
			return "session_update"
		}
		return strings.ReplaceAll(method, "/", "_")
	}
	if _, ok := payload["error"]; ok {
		return "error"
	}
	if result, ok := payload["result"].(map[string]any); ok {
		if _, exists := result["stopReason"]; exists {
			return "prompt_result"
		}
	}
	return "rpc"
}

func findSessionID(payload map[string]any) string {
	if id := stringField(payload, "sessionId"); id != "" {
		return id
	}
	for _, key := range []string{"params", "result"} {
		if nested, ok := payload[key].(map[string]any); ok {
			if id := stringField(nested, "sessionId"); id != "" {
				return id
			}
		}
	}
	return ""
}

func stopReason(raw json.RawMessage) string {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	result, _ := payload["result"].(map[string]any)
	return stringField(result, "stopReason")
}

func jsonRPCError(operation string, raw json.RawMessage) error {
	var payload struct {
		Error *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Error == nil {
		return nil
	}
	message := fmt.Sprintf("RPC %d: %s", payload.Error.Code, payload.Error.Message)
	return &ProtocolError{Operation: operation, Message: message}
}

func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return value
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringField(payload, key); value != "" {
			return value
		}
	}
	return ""
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
