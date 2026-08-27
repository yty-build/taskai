package settings

import (
	"errors"
	"strings"
	"testing"
)

func TestProviderValidationAndDefensiveCopy(t *testing.T) {
	t.Parallel()

	config := LaunchConfig{
		Argv: []string{"/usr/local/bin/provider", "--stdio", "--profile", "work profile"},
	}
	provider, err := NewProvider("codex", true, ProbeConnected, config, Capabilities{Manager: true, Subordinate: true, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	config.Argv[0] = "changed"
	if provider.LaunchConfig.Argv[0] != "/usr/local/bin/provider" {
		t.Fatal("launch configuration was not defensively copied")
	}
	if len(provider.LaunchConfig.Argv) != 4 || provider.LaunchConfig.Argv[3] != "work profile" {
		t.Fatalf("ordered argv was changed or split: %#v", provider.LaunchConfig.Argv)
	}
	if !provider.Eligible(CapabilityManager) || provider.Eligible(CapabilityReplay) {
		t.Fatalf("unexpected eligibility: %#v", provider)
	}
	provider.Enabled = false
	if provider.Eligible(CapabilityManager) {
		t.Fatal("disabled provider remained eligible")
	}
	cloned := provider.Clone()
	cloned.LaunchConfig.Argv[0] = "clone changed"
	if provider.LaunchConfig.Argv[0] != "/usr/local/bin/provider" {
		t.Fatal("Provider.Clone aliases launch configuration")
	}
}

func TestProbeStatusAndCapabilitiesAreFinite(t *testing.T) {
	t.Parallel()

	statuses := []ProbeStatus{
		ProbeUnknown, ProbeChecking, ProbeConnected, ProbeNotConnected,
		ProbeDisconnected, ProbeNeedsRepair, ProbeMissingConnector,
	}
	for _, status := range statuses {
		if err := status.Validate(); err != nil {
			t.Errorf("status %q: %v", status, err)
		}
	}
	if err := ProbeStatus("authenticated").Validate(); !errors.Is(err, ErrInvalidProbeStatus) {
		t.Fatalf("error = %v, want ErrInvalidProbeStatus", err)
	}
	for _, capability := range []Capability{CapabilityManager, CapabilitySubordinate, CapabilityResume, CapabilityReplay} {
		if err := capability.Validate(); err != nil {
			t.Errorf("capability %q: %v", capability, err)
		}
	}
	if err := Capability("marketing").Validate(); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("error = %v, want ErrInvalidCapability", err)
	}
	capabilities := Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: true}
	for _, capability := range []Capability{CapabilityManager, CapabilitySubordinate, CapabilityResume, CapabilityReplay} {
		if !capabilities.Has(capability) {
			t.Errorf("Has(%q) = false", capability)
		}
	}
	if capabilities.Has(Capability("unknown")) {
		t.Fatal("unknown capability reported present")
	}
}

func TestLaunchConfigRejectsSecretLookingArguments(t *testing.T) {
	t.Parallel()

	allowed := LaunchConfig{Argv: []string{"/opt/provider/bin/agent", "--stdio", "--profile", "work profile"}}
	if err := allowed.Validate(); err != nil {
		t.Fatalf("allowed config rejected: %v", err)
	}
	for _, argv := range [][]string{
		{"provider", "--api-token=canary"},
		{"provider", "--password", "canary"},
		{"provider", "--auth-file", "/provider-owned/auth"},
		{"provider", "--CLIENT_SECRET=canary"},
		{"provider", "--cookie"},
	} {
		if err := (LaunchConfig{Argv: argv}).Validate(); !errors.Is(err, ErrSecretLaunchArgument) {
			t.Errorf("secret-bearing argv %#v error = %v", argv, err)
		}
	}
}

func TestLaunchConfigBounds(t *testing.T) {
	t.Parallel()

	tests := []LaunchConfig{
		{Argv: []string{""}},
		{Argv: []string{"provider", string([]byte{0xff})}},
		{Argv: []string{"provider", "bad\x00argument"}},
		{Argv: []string{"provider", strings.Repeat("a", MaxLaunchArgumentBytes+1)}},
	}
	for _, config := range tests {
		if err := config.Validate(); !errors.Is(err, ErrInvalidLaunchConfig) {
			t.Errorf("config %#v error = %v", config, err)
		}
	}
	total := LaunchConfig{Argv: []string{
		strings.Repeat("v", MaxLaunchArgumentBytes), strings.Repeat("v", MaxLaunchArgumentBytes),
		strings.Repeat("v", MaxLaunchArgumentBytes), strings.Repeat("v", MaxLaunchArgumentBytes),
		strings.Repeat("v", MaxLaunchArgumentBytes),
	}}
	if err := total.Validate(); !errors.Is(err, ErrInvalidLaunchConfig) {
		t.Fatalf("total byte bound error = %v", err)
	}
	tooManyArgv := LaunchConfig{Argv: make([]string, MaxLaunchArgvItems+1)}
	tooManyArgv.Argv[0] = "provider"
	if err := tooManyArgv.Validate(); !errors.Is(err, ErrInvalidLaunchConfig) {
		t.Fatalf("argv item bound error = %v", err)
	}
	if err := (LaunchConfig{}).Validate(); err != nil {
		t.Fatalf("zero launch configuration rejected: %v", err)
	}
}

func TestProviderIDValidation(t *testing.T) {
	t.Parallel()

	for _, valid := range []ProviderID{"codex", "Claude.Code:1", "provider-one"} {
		if err := valid.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", valid, err)
		}
	}
	for _, invalid := range []ProviderID{"", "bad provider", ProviderID(strings.Repeat("x", MaxProviderIDBytes+1))} {
		if err := invalid.Validate(); !errors.Is(err, ErrInvalidProviderID) {
			t.Errorf("Validate(%q) error = %v", invalid, err)
		}
	}
}

func TestProviderValidationRejectsInvalidFields(t *testing.T) {
	t.Parallel()

	if _, err := NewProvider("bad provider", true, ProbeConnected, LaunchConfig{}, Capabilities{}); !errors.Is(err, ErrInvalidProviderID) {
		t.Fatalf("invalid provider ID error = %v", err)
	}
	if _, err := NewProvider("codex", true, ProbeStatus("ready"), LaunchConfig{}, Capabilities{}); !errors.Is(err, ErrInvalidProbeStatus) {
		t.Fatalf("invalid probe status error = %v", err)
	}
	if _, err := NewProvider("codex", true, ProbeConnected, LaunchConfig{Argv: []string{"provider", "--auth-token=canary"}}, Capabilities{}); !errors.Is(err, ErrSecretLaunchArgument) {
		t.Fatalf("secret launch argument error = %v", err)
	}
}
