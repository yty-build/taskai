package acp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/session"
)

func TestProbeBinaryPinsVersion(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-acpx"))
	if err != nil {
		t.Fatal(err)
	}
	if err := probeBinary(context.Background(), fixture); err != nil {
		t.Fatalf("probe supported version: %v", err)
	}
	t.Setenv("FAKE_ACPX_VERSION", "0.14.0")
	if err := probeBinary(context.Background(), fixture); err == nil || !strings.Contains(err.Error(), SupportedACPXVersion) {
		t.Fatalf("unsupported version error = %v", err)
	}
}

func TestValidateProviderRef(t *testing.T) {
	info := SessionInfo{ProviderRef: "provider-1"}
	if err := ValidateProviderRef("resume", info, "provider-1"); err != nil {
		t.Fatal(err)
	}
	var identity *IdentityError
	if err := ValidateProviderRef("resume", info, "replacement"); !errors.As(err, &identity) {
		t.Fatalf("identity mismatch error = %v", err)
	}
	for name, value := range map[string]string{
		"missing":       "",
		"oversized":     strings.Repeat("p", maxProviderRefBytes+1),
		"invalid UTF-8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateProviderRef("start", SessionInfo{ProviderRef: value}, ""); !errors.As(err, &identity) {
				t.Fatalf("ValidateProviderRef(%q) error = %T %v, want IdentityError", value, err, err)
			}
		})
	}
	spaced := SessionInfo{ProviderRef: " provider-1 "}
	if err := ValidateProviderRef("resume", spaced, " provider-1 "); err != nil {
		t.Fatalf("opaque whitespace provider reference rejected: %v", err)
	}
}

func TestProviderCatalogValidatesExactLiveSelections(t *testing.T) {
	catalog := ProviderOptionCatalog{
		Model:           ProviderSelectCatalog{Supported: true, CurrentValue: "m1", Choices: []ProviderOptionChoice{{Value: "m1", Label: "Model 1"}, {Value: "m2", Label: "Model 2"}}},
		ReasoningEffort: ProviderSelectCatalog{Supported: true, CurrentValue: "high", Choices: []ProviderOptionChoice{{Value: "low", Label: "Low"}, {Value: "high", Label: "High"}}},
	}
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateSelection(session.ProviderOptions{Model: "m2", ReasoningEffort: "low"}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateSelection(session.ProviderOptions{Model: "invented"}); err == nil {
		t.Fatal("unadvertised model accepted")
	}
}
