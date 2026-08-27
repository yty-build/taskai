package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/session"
)

// PermissionResponder is implemented by ACP transports that can keep a real
// session/request_permission call pending while a disposable viewer chooses an
// offered option.
type PermissionResponder interface {
	DecidePermission(context.Context, string, string, bool) error
}

// RuntimeClient is one long-lived public acpx/runtime connection per Session
// worker. It does not use acpx's detached queue-owner CLI, because that process
// cannot receive permission decisions from an attached Task Console viewer.
type RuntimeClient struct {
	binary         string
	stateDirectory string
	verified       atomic.Bool

	mu      sync.Mutex
	bridge  *runtimeBridge
	target  *Target
	blocked error
	closed  bool
}

var errBridgeTeardownInProgress = fmt.Errorf("previous acpx runtime process tree is still terminating")

const maxNormalizedEventTypeBytes = 128

func NewRuntimeClient(binary, stateDirectory string) *RuntimeClient {
	return &RuntimeClient{binary: binary, stateDirectory: stateDirectory}
}

func (c *RuntimeClient) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("acpx runtime client is closed")
	}
	if c.verified.Load() {
		return nil
	}
	binary := c.binary
	if strings.TrimSpace(binary) == "" {
		var err error
		binary, err = FindBinary()
		if err != nil {
			return err
		}
	}
	if err := probeBinary(ctx, binary); err != nil {
		return err
	}
	if _, err := installedPackageRoot(binary); err != nil {
		return err
	}
	c.binary = binary
	c.verified.Store(true)
	return nil
}

func (c *RuntimeClient) Start(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	info, err := c.ensure(ctx, spec)
	if err != nil {
		return SessionInfo{}, err
	}
	return info, nil
}

func (c *RuntimeClient) Resume(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	if err := ValidateProviderRefValue("runtime resume request", spec.ResumeProviderRef); err != nil {
		return SessionInfo{}, err
	}
	info, err := c.ensure(ctx, spec)
	if err != nil {
		c.discardBridge()
		return SessionInfo{}, providerFacingTurnError(err)
	}
	if info.ProviderRef == "" {
		c.discardBridge()
		return SessionInfo{}, identityError("runtime resume", "expected provider reference %q, received %q", spec.ResumeProviderRef, info.ProviderRef)
	}
	if err := ValidateProviderRef("runtime resume", info, spec.ResumeProviderRef); err != nil {
		c.discardBridge()
		return SessionInfo{}, err
	}
	return info, nil
}

// Configure applies optional provider controls only after ensure/resume has
// returned a stable identity to the worker for durable persistence. This
// ordering prevents an invalid model or effort from orphaning a newly-created
// provider conversation whose exact ID was never recorded.
func (c *RuntimeClient) Configure(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return SessionInfo{}, err
	}
	providerOptions := session.ProviderOptions{Model: spec.Model, ReasoningEffort: spec.ReasoningEffort}
	if err := providerOptions.Validate(); err != nil {
		return SessionInfo{}, err
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return SessionInfo{}, err
	}
	var payload bridgeSession
	if err := bridge.call(ctx, bridgeCommand{
		Type: "configure", Model: providerOptions.Model, ReasoningEffort: providerOptions.ReasoningEffort,
	}, &payload); err != nil {
		var remote *bridgeRemoteCommandError
		if retainedPortUnavailableError(err) {
			c.discardBridge()
			return SessionInfo{}, &ProtocolError{Operation: "provider configuration", Message: "retained provider Port is unavailable"}
		}
		if !errors.As(err, &remote) {
			c.discardBridge()
		}
		return SessionInfo{}, &ProviderConfigurationError{}
	}
	info, err := bridgeSessionInfo(target, payload)
	if err != nil {
		c.discardBridge()
		return SessionInfo{}, err
	}
	if err := ValidateProviderRef("runtime provider configuration", info, spec.ResumeProviderRef); err != nil {
		c.discardBridge()
		return SessionInfo{}, err
	}
	return info, nil
}

// ProviderOptions returns only bounded select choices advertised for this
// already-established provider conversation. Callers persist identity before
// invoking this method; discovery never creates a second hidden conversation.
func (c *RuntimeClient) ProviderOptions(ctx context.Context, spec StartSpec) (ProviderOptionCatalog, error) {
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return ProviderOptionCatalog{}, err
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return ProviderOptionCatalog{}, err
	}
	var payload bridgeSession
	if err := bridge.call(ctx, bridgeCommand{Type: "provider_options"}, &payload); err != nil {
		var remote *bridgeRemoteCommandError
		if retainedPortUnavailableError(err) {
			c.discardBridge()
			return ProviderOptionCatalog{}, &ProtocolError{Operation: "provider option discovery", Message: "retained provider Port is unavailable"}
		}
		if errors.As(err, &remote) {
			return ProviderOptionCatalog{}, &ProviderOptionsError{}
		}
		c.discardBridge()
		return ProviderOptionCatalog{}, &ProtocolError{Operation: "provider option discovery", Message: "provider connection failed"}
	}
	info, err := bridgeSessionInfo(target, payload)
	if err != nil {
		c.discardBridge()
		return ProviderOptionCatalog{}, err
	}
	if err := ValidateProviderRef("runtime provider option discovery", info, spec.ResumeProviderRef); err != nil {
		c.discardBridge()
		return ProviderOptionCatalog{}, err
	}
	if err := payload.ProviderOptions.Validate(); err != nil {
		c.discardBridge()
		return ProviderOptionCatalog{}, &ProtocolError{Operation: "provider option discovery", Message: "invalid provider configuration catalog"}
	}
	return payload.ProviderOptions, nil
}

// providerFacingTurnError classifies only explicit provider-facing
// contention/rejection codes while opening an exact provider
// conversation. Generic bridge, framing, permission, and identity failures
// remain protocol errors so the worker retains its recovery/quarantine path.
func providerFacingTurnError(err error) error {
	var remote *bridgeRemoteCommandError
	if !errors.As(err, &remote) || (!providerActivationFailureCode(remote.code) && !providerActivationFailureCode(remote.detailCode)) {
		return err
	}
	return &ProviderTurnError{
		Code: remote.code, DetailCode: remote.detailCode, Message: remote.message, Retryable: remote.retryable,
	}
}

func providerActivationFailureCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	value = strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(value))
	switch value {
	case "busy", "provider_busy", "conversation_busy", "turn_busy",
		"conflict", "provider_conflict", "conversation_conflict", "concurrent_turn",
		"rate_limited", "rate_limit", "too_many_requests", "throttled",
		"rejected", "provider_rejected", "refused", "denied", "provider_error":
		return true
	default:
		return false
	}
}

func providerPermissionRejectionError(err error) bool {
	var remote *bridgeRemoteCommandError
	if !errors.As(err, &remote) {
		return false
	}
	return providerPermissionRejectionCode(remote.code) || providerPermissionRejectionCode(remote.detailCode)
}

func providerPermissionRejectionCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	value = strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(value))
	switch value {
	case "permission_rejected", "permission_response_rejected", "provider_rejected":
		return true
	default:
		return false
	}
}

func retainedPortUnavailableError(err error) bool {
	var remote *bridgeRemoteCommandError
	return errors.As(err, &remote) && remote.code == "retained_port_unavailable"
}

func (c *RuntimeClient) ensure(ctx context.Context, spec StartSpec) (SessionInfo, error) {
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return SessionInfo{}, err
	}
	bridge, err := c.bridgeFor(ctx, target)
	if err != nil {
		return SessionInfo{}, err
	}
	agentName, useBuiltin := runtimeAgentConfiguration(target.Profile)
	var payload bridgeSession
	err = bridge.call(ctx, bridgeCommand{
		Type: "ensure", SessionKey: target.ConsoleSessionID, Agent: agentName, UseBuiltin: useBuiltin,
		Argv: bridgeAgentArgv(target.Profile), Environment: cloneProfileEnvironment(target.Profile.Environment),
		CWD: target.WorkingDirectory, PermissionMode: target.Permission, ResumeSessionID: spec.ResumeProviderRef,
	}, &payload)
	if err != nil {
		return SessionInfo{}, err
	}
	return bridgeSessionInfo(target, payload)
}

// runtimeAgentConfiguration distinguishes the friendly acpx built-in names in
// embedded profiles from user-supplied raw ACP server commands. The public
// runtime registry, rather than a native provider CLI, must expand a friendly
// name such as "codex" into its pinned ACP adapter command.
func runtimeAgentConfiguration(profile agents.Profile) (string, bool) {
	if isBuiltIn(profile) {
		return strings.TrimSpace(profile.Command), true
	}
	return profile.Name, false
}

func (c *RuntimeClient) Prompt(ctx context.Context, spec PromptSpec, emit func(Event) error) (PromptReceipt, error) {
	if err := ctx.Err(); err != nil {
		return PromptReceipt{RequestID: spec.RequestID}, err
	}
	target, err := c.readyTarget(spec.Target)
	if err != nil {
		return PromptReceipt{}, err
	}
	if strings.TrimSpace(spec.Message) == "" {
		return PromptReceipt{}, fmt.Errorf("prompt message is required")
	}
	if spec.RequestID == "" {
		spec.RequestID, err = NewRequestID()
		if err != nil {
			return PromptReceipt{}, err
		}
	}
	if emit == nil {
		emit = func(Event) error { return nil }
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return PromptReceipt{}, err
	}
	channel, cleanup, err := bridge.start(bridgeCommand{Type: "turn", RequestID: spec.RequestID, Text: spec.Message})
	if err != nil {
		return PromptReceipt{}, err
	}
	defer cleanup()
	receipt := PromptReceipt{RequestID: spec.RequestID}
	for {
		select {
		case <-ctx.Done():
			cancelContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = bridge.call(cancelContext, bridgeCommand{Type: "cancel", RequestID: spec.RequestID, Reason: "prompt context cancelled"}, nil)
			cancel()
			return receipt, ctx.Err()
		case <-bridge.closed:
			return receipt, bridge.closedError()
		case message := <-channel:
			switch message.Type {
			case "turn_started":
				continue
			case "runtime_event":
				receipt.LastSeq++
				event, eventErr := runtimeEvent(spec, receipt.LastSeq, message.Event)
				if eventErr != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "invalid runtime event")
					return receipt, eventErr
				}
				if err := emit(event); err != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "event consumer failed")
					return receipt, fmt.Errorf("consume acpx runtime event: %w", err)
				}
			case "permission_request":
				receipt.LastSeq++
				event, eventErr := runtimePermissionEvent(spec, receipt.LastSeq, message)
				if eventErr != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "invalid permission event")
					return receipt, eventErr
				}
				if err := emit(event); err != nil {
					cancelRuntimeTurn(bridge, spec.RequestID, "permission consumer failed")
					return receipt, fmt.Errorf("consume acpx permission request: %w", err)
				}
			case "permission_abandoned":
				continue
			case "turn_result":
				completed, resultErr := runtimeTurnResult(receipt, message.Result)
				var protocolErr *ProtocolError
				if errors.As(resultErr, &protocolErr) {
					c.discardBridge()
				}
				return completed, resultErr
			case "error":
				remoteErr := bridgeRemoteError(message)
				if retainedPortUnavailableError(remoteErr) {
					c.discardBridge()
					return receipt, &ProtocolError{Operation: "prompt dispatch", Message: "retained provider Port is unavailable"}
				}
				return receipt, providerFacingTurnError(remoteErr)
			}
		}
	}
}

func cancelRuntimeTurn(bridge *runtimeBridge, requestID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = bridge.call(ctx, bridgeCommand{Type: "cancel", RequestID: requestID, Reason: reason}, nil)
}

func (c *RuntimeClient) Cancel(ctx context.Context, target Target) error {
	target, err := c.readyTarget(target)
	if err != nil {
		return err
	}
	bridge, err := c.existingBridge(target)
	if err != nil {
		return err
	}
	return bridge.call(ctx, bridgeCommand{Type: "cancel", Reason: "cancelled by Task Console"}, nil)
}

func (c *RuntimeClient) DecidePermission(ctx context.Context, permissionID, optionID string, cancelled bool) error {
	if permissionID == "" {
		return fmt.Errorf("permission ID is required")
	}
	if !cancelled && optionID == "" {
		return fmt.Errorf("permission option ID is required")
	}
	c.mu.Lock()
	bridge := c.bridge
	closed := c.closed
	c.mu.Unlock()
	if closed || bridge == nil {
		return fmt.Errorf("acpx runtime bridge is unavailable")
	}
	err := bridge.call(ctx, bridgeCommand{
		Type: "permission_decision", PermissionID: permissionID, OptionID: optionID, Cancelled: cancelled,
	}, nil)
	if err == nil {
		return nil
	}
	if providerPermissionRejectionError(err) {
		return &ProviderPermissionRejectedError{}
	}
	// A decision may have reached the provider even when its acknowledgement
	// did not reach us. The retained Port can no longer prove whether this
	// callback is pending, so it must never be reused in-process.
	c.discardBridge()
	return &ProtocolError{Operation: "permission decision", Message: "retained provider Port continuity is unavailable"}
}

func (c *RuntimeClient) Close() error {
	c.mu.Lock()
	if c.closed {
		blocked := c.blocked
		c.mu.Unlock()
		return blocked
	}
	c.closed = true
	bridge := c.bridge
	blocked := c.blocked
	c.mu.Unlock()
	if bridge == nil {
		return blocked
	}
	if err := bridge.closeProcess(); err != nil {
		return err
	}
	return blocked
}

func (c *RuntimeClient) readyTarget(target Target) (Target, error) {
	if !c.verified.Load() {
		return Target{}, fmt.Errorf("acpx runtime compatibility has not been probed")
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

func (c *RuntimeClient) bridgeFor(ctx context.Context, target Target) (*runtimeBridge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("acpx runtime client is closed")
	}
	if c.blocked != nil {
		return nil, fmt.Errorf("acpx runtime bridge replacement is blocked after unsafe teardown: %w", c.blocked)
	}
	if c.target != nil && !sameRuntimeTarget(*c.target, target) {
		return nil, fmt.Errorf("acpx runtime client is already bound to Session %s", c.target.ConsoleSessionID)
	}
	if c.bridge == nil {
		bridge, err := startRuntimeBridge(ctx, c.binary, c.stateDirectory)
		if err != nil {
			return nil, err
		}
		c.bridge = bridge
		copyTarget := target
		c.target = &copyTarget
	}
	return c.bridge, nil
}

func (c *RuntimeClient) existingBridge(target Target) (*runtimeBridge, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.bridge == nil || c.target == nil {
		return nil, fmt.Errorf("acpx runtime Session is not connected")
	}
	if !sameRuntimeTarget(*c.target, target) {
		return nil, fmt.Errorf("acpx runtime client is bound to a different Session")
	}
	return c.bridge, nil
}

// discardBridge tears down a bridge whose provider identity can no longer be
// trusted. A worker never replaces it in-process; recovery starts a new worker
// and performs one startup Resume.
func (c *RuntimeClient) discardBridge() {
	c.mu.Lock()
	bridge := c.bridge
	c.bridge = nil
	if bridge != nil {
		c.blocked = errBridgeTeardownInProgress
	}
	c.mu.Unlock()
	if bridge != nil {
		err := bridge.terminateProcess()
		c.mu.Lock()
		if err != nil {
			c.blocked = err
		} else if c.blocked == errBridgeTeardownInProgress {
			c.blocked = nil
		}
		c.mu.Unlock()
	}
}

func sameRuntimeTarget(left, right Target) bool {
	return left.ConsoleSessionID == right.ConsoleSessionID &&
		left.WorkingDirectory == right.WorkingDirectory &&
		left.Profile.Name == right.Profile.Name &&
		left.Permission == right.Permission
}

func cloneProfileEnvironment(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func runtimeEvent(spec PromptSpec, sequence uint64, encoded json.RawMessage) (Event, error) {
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return Event{}, &ProtocolError{Operation: "runtime event", Message: err.Error()}
	}
	typeName, _ := payload["type"].(string)
	if typeName == "" || strings.TrimSpace(typeName) != typeName || !boundedPrintable(typeName, maxNormalizedEventTypeBytes) {
		return Event{}, &ProtocolError{Operation: "runtime event", Message: "invalid event type"}
	}
	return Event{
		EventVersion: 1, SessionID: spec.ConsoleSessionID, RequestID: spec.RequestID,
		Seq: sequence, Stream: "prompt", Type: typeName, Raw: cloneRaw(encoded),
	}, nil
}

func runtimePermissionEvent(spec PromptSpec, sequence uint64, message bridgeMessage) (Event, error) {
	if message.PermissionID == "" || !json.Valid(message.Request) {
		return Event{}, &ProtocolError{Operation: "runtime permission", Message: "invalid permission envelope"}
	}
	return Event{
		EventVersion: 1, SessionID: spec.ConsoleSessionID, RequestID: spec.RequestID,
		Seq: sequence, Stream: "prompt", Type: "session_request_permission",
		PermissionID: message.PermissionID, Raw: cloneRaw(message.Request),
	}, nil
}

func runtimeTurnResult(receipt PromptReceipt, encoded json.RawMessage) (PromptReceipt, error) {
	var result struct {
		Status     string `json:"status"`
		StopReason string `json:"stopReason"`
		Error      struct {
			Message    string `json:"message"`
			Code       string `json:"code"`
			DetailCode string `json:"detailCode"`
			Retryable  bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return receipt, &ProtocolError{Operation: "runtime turn result", Message: err.Error()}
	}
	switch result.Status {
	case "completed":
		receipt.StopReason = firstNonBlankString(result.StopReason, "end_turn")
		return receipt, nil
	case "cancelled":
		receipt.StopReason = "cancelled"
		return receipt, nil
	case "failed":
		if strings.EqualFold(strings.TrimSpace(result.Error.Code), "NO_SESSION") {
			return receipt, &ProtocolError{
				Operation: "runtime turn result",
				Message:   "provider Port no longer addresses the saved conversation",
			}
		}
		return receipt, &ProviderTurnError{
			Code: result.Error.Code, DetailCode: result.Error.DetailCode,
			Message: firstNonBlankString(result.Error.Message, "provider turn failed"), Retryable: result.Error.Retryable,
		}
	default:
		return receipt, &ProtocolError{Operation: "runtime turn result", Message: fmt.Sprintf("unknown status %q", result.Status)}
	}
}

func firstNonBlankString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var _ PermissionResponder = (*RuntimeClient)(nil)
