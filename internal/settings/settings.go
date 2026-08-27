package settings

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	MaxProviderIDBytes     = 128
	MaxLaunchArgvItems     = 64
	MaxLaunchArgumentBytes = 2048
	MaxLaunchConfigBytes   = 8192
)

var (
	ErrInvalidProviderID    = errors.New("invalid provider ID")
	ErrInvalidProbeStatus   = errors.New("invalid provider probe status")
	ErrInvalidCapability    = errors.New("invalid provider capability")
	ErrInvalidLaunchConfig  = errors.New("invalid provider launch configuration")
	ErrSecretLaunchArgument = errors.New("secret-bearing launch argument")
)

// ProviderID identifies connector configuration, not a third product entity.
type ProviderID string

// Validate verifies a bounded connector identifier without changing it.
func (id ProviderID) Validate() error {
	if len(id) == 0 || len(id) > MaxProviderIDBytes {
		return fmt.Errorf("%w: length must be 1..%d bytes", ErrInvalidProviderID, MaxProviderIDBytes)
	}
	for _, char := range []byte(id) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("%w: unsupported byte 0x%02x", ErrInvalidProviderID, char)
	}
	return nil
}

// ProbeStatus is the finite mechanical result of a connector-owned provider probe.
type ProbeStatus string

const (
	ProbeUnknown          ProbeStatus = "unknown"
	ProbeChecking         ProbeStatus = "checking"
	ProbeConnected        ProbeStatus = "connected"
	ProbeNotConnected     ProbeStatus = "not_connected"
	ProbeDisconnected     ProbeStatus = "disconnected"
	ProbeNeedsRepair      ProbeStatus = "needs_repair"
	ProbeMissingConnector ProbeStatus = "missing_connector"
)

// Validate rejects statuses that were not mechanically defined.
func (status ProbeStatus) Validate() error {
	switch status {
	case ProbeUnknown, ProbeChecking, ProbeConnected, ProbeNotConnected,
		ProbeDisconnected, ProbeNeedsRepair, ProbeMissingConnector:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidProbeStatus, status)
	}
}

// Capability selects one conformance-derived connector capability.
type Capability string

const (
	CapabilityManager     Capability = "manager"
	CapabilitySubordinate Capability = "subordinate"
	CapabilityResume      Capability = "resume"
	CapabilityReplay      Capability = "replay"
)

// Validate rejects unknown capability selectors.
func (capability Capability) Validate() error {
	switch capability {
	case CapabilityManager, CapabilitySubordinate, CapabilityResume, CapabilityReplay:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidCapability, capability)
	}
}

// Capabilities contains only flags backed by connector conformance.
type Capabilities struct {
	Manager     bool
	Subordinate bool
	Resume      bool
	Replay      bool
}

// Has reports one already-conformed capability flag.
func (capabilities Capabilities) Has(capability Capability) bool {
	switch capability {
	case CapabilityManager:
		return capabilities.Manager
	case CapabilitySubordinate:
		return capabilities.Subordinate
	case CapabilityResume:
		return capabilities.Resume
	case CapabilityReplay:
		return capabilities.Replay
	default:
		return false
	}
}

// LaunchConfig is a bounded literal process vector, including the executable
// at index zero. Composition may persist only vectors selected from its
// registered connector profiles. Settings UI must not author argv or
// credential arguments. Consumers pass Argv[0] and Argv[1:] directly to a
// process API; they never invoke a shell or split/re-parse any element.
type LaunchConfig struct {
	Argv []string `json:"argv"`
}

// Validate verifies literal argv bounds and rejects mechanically recognizable
// credential-bearing long flags. Connector-profile registration remains the
// authority for the complete allowed vector.
func (config LaunchConfig) Validate() error {
	if len(config.Argv) > MaxLaunchArgvItems {
		return fmt.Errorf("%w: argv exceeds %d items", ErrInvalidLaunchConfig, MaxLaunchArgvItems)
	}
	if len(config.Argv) > 0 && config.Argv[0] == "" {
		return fmt.Errorf("%w: argv[0] must name an executable", ErrInvalidLaunchConfig)
	}
	total := 0
	for index, argument := range config.Argv {
		if !utf8.ValidString(argument) || len(argument) > MaxLaunchArgumentBytes || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("%w: argv[%d] is invalid or exceeds %d bytes", ErrInvalidLaunchConfig, index, MaxLaunchArgumentBytes)
		}
		if index > 0 {
			secret, hasInlineValue := secretLaunchFlag(argument)
			if secret && hasInlineValue {
				return fmt.Errorf("%w: argv[%d] uses secret-looking flag=value", ErrSecretLaunchArgument, index)
			}
			if secret && index+1 < len(config.Argv) {
				return fmt.Errorf("%w: argv[%d] is a secret-looking flag followed by a value", ErrSecretLaunchArgument, index)
			}
			if secret {
				return fmt.Errorf("%w: argv[%d] is a secret-looking flag", ErrSecretLaunchArgument, index)
			}
		}
		total += len(argument)
	}
	if total > MaxLaunchConfigBytes {
		return fmt.Errorf("%w: exceeds %d total bytes", ErrInvalidLaunchConfig, MaxLaunchConfigBytes)
	}
	return nil
}

// Clone returns a defensive copy of launch configuration.
func (config LaunchConfig) Clone() LaunchConfig {
	return LaunchConfig{Argv: append([]string{}, config.Argv...)}
}

func secretLaunchFlag(argument string) (secret, hasInlineValue bool) {
	if !strings.HasPrefix(argument, "-") {
		return false, false
	}
	key, _, hasInlineValue := strings.Cut(argument, "=")
	key = strings.TrimLeft(key, "-")
	normalized := strings.ToLower(key)
	normalized = strings.NewReplacer("-", "", "_", "", ".", "").Replace(normalized)
	for _, prohibited := range secretConfigKeyTerms {
		if strings.Contains(normalized, prohibited) {
			return true, hasInlineValue
		}
	}
	return false, hasInlineValue
}

var secretConfigKeyTerms = []string{
	"secret", "credential", "token", "password", "passwd", "cookie", "auth",
	"apikey", "privatekey", "accesskey", "bearer",
}

// Provider is the complete provider Settings configuration record.
type Provider struct {
	ID           ProviderID
	Enabled      bool
	ProbeStatus  ProbeStatus
	LaunchConfig LaunchConfig
	Capabilities Capabilities
}

// NewProvider constructs validated provider configuration.
func NewProvider(id ProviderID, enabled bool, status ProbeStatus, config LaunchConfig, capabilities Capabilities) (Provider, error) {
	provider := Provider{
		ID:           id,
		Enabled:      enabled,
		ProbeStatus:  status,
		LaunchConfig: config.Clone(),
		Capabilities: capabilities,
	}
	if err := provider.Validate(); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// Validate verifies bounded, non-secret provider configuration.
func (provider Provider) Validate() error {
	if err := provider.ID.Validate(); err != nil {
		return err
	}
	if err := provider.ProbeStatus.Validate(); err != nil {
		return err
	}
	if err := provider.LaunchConfig.Validate(); err != nil {
		return err
	}
	return nil
}

// Clone returns a defensive copy of Provider configuration.
func (provider Provider) Clone() Provider {
	provider.LaunchConfig = provider.LaunchConfig.Clone()
	return provider
}

// Eligible reports whether this explicitly enabled provider has a conformed capability.
func (provider Provider) Eligible(capability Capability) bool {
	return provider.Enabled && provider.Capabilities.Has(capability)
}
