package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
)

func TestProviderSettingsRoundTripEligibilityAndDeterministicOrder(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	codex := mustProvider(t, "codex", true, settings.ProbeConnected,
		settings.LaunchConfig{Argv: []string{"codex-acp", "--stdio", "--profile", "work profile"}},
		settings.Capabilities{Manager: true, Subordinate: true, Resume: true})
	claude := mustProvider(t, "claude", true, settings.ProbeDisconnected,
		settings.LaunchConfig{Argv: []string{"claude-agent-acp"}},
		settings.Capabilities{Subordinate: true, Replay: true})
	gemini := mustProvider(t, "gemini", false, settings.ProbeUnknown,
		settings.LaunchConfig{}, settings.Capabilities{Manager: true})
	for _, provider := range []settings.Provider{gemini, codex, claude} {
		if err := store.PutProviderSettings(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
	}

	all, err := store.ListProviderSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "claude" || all[1].ID != "codex" || all[2].ID != "gemini" {
		t.Fatalf("provider order = %#v", all)
	}
	stored, err := store.GetProviderSettings(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.LaunchConfig.Argv) != 4 || stored.LaunchConfig.Argv[3] != "work profile" || stored.Capabilities != codex.Capabilities {
		t.Fatalf("round trip = %#v, want %#v", stored, codex)
	}
	stored.LaunchConfig.Argv[1] = "mutated"
	again, err := store.GetProviderSettings(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if again.LaunchConfig.Argv[1] != "--stdio" {
		t.Fatal("returned launch configuration aliases persisted state")
	}

	managers, err := store.ListEnabledProviders(context.Background(), settings.CapabilityManager)
	if err != nil {
		t.Fatal(err)
	}
	if len(managers) != 1 || managers[0].ID != "codex" {
		t.Fatalf("enabled managers = %#v", managers)
	}
	subordinates, err := store.ListEnabledProviders(context.Background(), settings.CapabilitySubordinate)
	if err != nil {
		t.Fatal(err)
	}
	if len(subordinates) != 2 || subordinates[0].ID != "claude" || subordinates[1].ID != "codex" {
		t.Fatalf("enabled subordinates = %#v", subordinates)
	}
	replay, err := store.ListEnabledProviders(context.Background(), settings.CapabilityReplay)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) != 1 || replay[0].ID != "claude" {
		t.Fatalf("enabled replay providers = %#v", replay)
	}
	if _, err := store.ListEnabledProviders(context.Background(), settings.Capability("invented")); !errors.Is(err, settings.ErrInvalidCapability) {
		t.Fatalf("invalid capability error = %v", err)
	}
}

func TestProviderEnablementAndProbeUpdatesDoNotMutateSessionMetadata(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	provider := mustProvider(t, "codex", true, settings.ProbeConnected,
		settings.LaunchConfig{Argv: []string{"codex-acp"}},
		settings.Capabilities{Manager: true, Resume: true})
	if err := store.PutProviderSettings(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	createTask(t, store, "task_existing", "existing", fixedTime)
	manager := createManager(t, store, "manager_existing", "task_existing", session.StateActive, fixedTime)

	if err := store.SetProviderEnabled(context.Background(), provider.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProviderProbeStatus(context.Background(), provider.ID, settings.ProbeNeedsRepair); err != nil {
		t.Fatal(err)
	}
	eligible, err := store.ListEnabledProviders(context.Background(), settings.CapabilityManager)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 0 {
		t.Fatalf("disabled provider remains eligible: %#v", eligible)
	}
	updated, err := store.GetProviderSettings(context.Background(), provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.ProbeStatus != settings.ProbeNeedsRepair || updated.Capabilities != provider.Capabilities {
		t.Fatalf("unexpected provider update: %#v", updated)
	}
	if _, err := store.GetSession(context.Background(), manager.ID); err != nil {
		t.Fatalf("disable/probe update changed existing Session: %v", err)
	}
}

func TestProviderSettingsDeleteIsMetadataOnly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	providerState := filepath.Join(root, "connector-owned-state")
	providerBytes := []byte("must remain outside TaskAI ownership")
	if err := os.WriteFile(providerState, providerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), filepath.Join(root, "taskai-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	provider := mustProvider(t, "codex", true, settings.ProbeConnected, settings.LaunchConfig{}, settings.Capabilities{Manager: true})
	if err := store.PutProviderSettings(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	createTask(t, store, "task_existing", "existing", fixedTime)
	manager := createManager(t, store, "manager_existing", "task_existing", session.StateActive, fixedTime)

	if err := store.DeleteProviderSettings(context.Background(), provider.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProviderSettings(context.Background(), provider.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Settings lookup error = %v", err)
	}
	if _, err := store.GetSession(context.Background(), manager.ID); err != nil {
		t.Fatalf("Settings delete changed existing Session: %v", err)
	}
	if got, err := os.ReadFile(providerState); err != nil || !bytes.Equal(got, providerBytes) {
		t.Fatalf("provider-owned state changed: %q, %v", got, err)
	}
}

func TestStoreRejectsSecretLaunchCanariesBeforePersistence(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	const canary = "TASKAI_M02_DO_NOT_PERSIST_7b9665c9"
	prohibitedArgv := [][]string{
		{"provider", "--api-token=" + canary},
		{"provider", "--password", canary},
		{"provider", "--credential-file", canary},
	}
	for index, argv := range prohibitedArgv {
		provider := settings.Provider{
			ID:           settings.ProviderID("provider_secret_" + string(rune('a'+index))),
			Enabled:      true,
			ProbeStatus:  settings.ProbeUnknown,
			LaunchConfig: settings.LaunchConfig{Argv: argv},
		}
		if err := store.PutProviderSettings(context.Background(), provider); !errors.Is(err, settings.ErrSecretLaunchArgument) {
			t.Errorf("argv %#v error = %v, want ErrSecretLaunchArgument", argv, err)
		}
	}
	entries, err := os.ReadDir(store.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(store.StateDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(canary)) {
			t.Errorf("content canary persisted in %s", entry.Name())
		}
	}
	providers, err := store.ListProviderSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("rejected provider Settings were persisted: %#v", providers)
	}
}

func TestLaunchArgvPersistsAsExactOrderedArrayAndRejectsCommandStrings(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	wantArgv := []string{"/opt/provider path/agent", "--stdio", "--profile", "work profile", "literal * $HOME"}
	provider := mustProvider(t, "structured", true, settings.ProbeConnected,
		settings.LaunchConfig{Argv: wantArgv},
		settings.Capabilities{Subordinate: true})
	if err := store.PutProviderSettings(context.Background(), provider); err != nil {
		t.Fatal(err)
	}

	var configJSON string
	if err := store.db.QueryRowContext(context.Background(), `
		SELECT launch_config_json FROM provider_settings WHERE provider_id = 'structured'`).Scan(&configJSON); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Argv []string `json:"argv"`
	}
	if err := json.Unmarshal([]byte(configJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Argv) != len(wantArgv) {
		t.Fatalf("stored argv length = %d, want %d: %s", len(decoded.Argv), len(wantArgv), configJSON)
	}
	for index := range wantArgv {
		if decoded.Argv[index] != wantArgv[index] {
			t.Errorf("argv[%d] = %q, want %q", index, decoded.Argv[index], wantArgv[index])
		}
	}
	if bytes.Contains([]byte(configJSON), []byte(`"command"`)) || bytes.Contains([]byte(configJSON), []byte(`"args"`)) {
		t.Fatalf("shell command field persisted: %s", configJSON)
	}

	legacyShapes := []string{
		`{"command":"provider --stdio"}`,
		`{"argv":"provider --stdio"}`,
		`{"argv":[],"command":"provider --stdio"}`,
	}
	for _, legacy := range legacyShapes {
		if _, err := store.db.ExecContext(context.Background(), `
			UPDATE provider_settings SET launch_config_json = ? WHERE provider_id = 'structured'`, legacy); err == nil {
			t.Errorf("legacy command shape unexpectedly persisted: %s", legacy)
		}
	}
}

func TestLaunchConfigSchemaRejectsGenericPathAndUnknownFieldCanaries(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	provider := mustProvider(t, "shape_canary", true, settings.ProbeConnected,
		settings.LaunchConfig{Argv: []string{"registered-provider", "--stdio"}},
		settings.Capabilities{Subordinate: true})
	if err := store.PutProviderSettings(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		canary string
		json   string
	}{
		{name: "generic values", canary: "M02_GENERIC_VALUE_CANARY_91f3", json: `{"argv":[],"values":{"model":"M02_GENERIC_VALUE_CANARY_91f3"}}`},
		{name: "working directory", canary: "M02_WORKING_DIRECTORY_CANARY_82e4", json: `{"argv":[],"working_directory":"M02_WORKING_DIRECTORY_CANARY_82e4"}`},
		{name: "unknown field", canary: "M02_UNKNOWN_FIELD_CANARY_73d5", json: `{"argv":[],"benign":"M02_UNKNOWN_FIELD_CANARY_73d5"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(context.Background(), `
				UPDATE provider_settings SET launch_config_json = ? WHERE provider_id = 'shape_canary'`, test.json); err == nil {
				t.Fatalf("prohibited JSON shape persisted: %s", test.json)
			}
		})
	}
	entries, err := os.ReadDir(store.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(store.StateDir(), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range cases {
			if bytes.Contains(contents, []byte(test.canary)) {
				t.Errorf("%s canary persisted in %s", test.name, entry.Name())
			}
		}
	}
	stored, err := store.GetProviderSettings(context.Background(), provider.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.LaunchConfig.Argv) != 2 || stored.LaunchConfig.Argv[0] != "registered-provider" || stored.LaunchConfig.Argv[1] != "--stdio" {
		t.Fatalf("rejected shape changed original config: %#v", stored.LaunchConfig)
	}
}

func TestProviderSettingsMissingUpdatesReturnNotFound(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	if err := store.SetProviderEnabled(context.Background(), "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enable error = %v", err)
	}
	if err := store.SetProviderProbeStatus(context.Background(), "missing", settings.ProbeConnected); !errors.Is(err, ErrNotFound) {
		t.Fatalf("probe error = %v", err)
	}
	if err := store.DeleteProviderSettings(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete error = %v", err)
	}
}

func mustProvider(t *testing.T, id settings.ProviderID, enabled bool, status settings.ProbeStatus, config settings.LaunchConfig, capabilities settings.Capabilities) settings.Provider {
	t.Helper()
	provider, err := settings.NewProvider(id, enabled, status, config, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}
