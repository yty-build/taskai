package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
)

func runtimeFixture(t *testing.T, scenario string) (*RuntimeClient, Target, string) {
	t.Helper()
	binary, err := filepath.Abs(filepath.Join("testdata", "fake-acpx"))
	if err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(t.TempDir(), "private-runtime")
	permissionLog := filepath.Join(t.TempDir(), "permissions.jsonl")
	client := NewRuntimeClient(binary, stateDirectory)
	if err := client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	target := Target{
		ConsoleSessionID: "S-runtime", WorkingDirectory: t.TempDir(), Permission: PermissionReadApproval,
		Profile: agents.Profile{
			Name: "fixture", Provider: "test", Command: "fixture-agent",
			Environment: map[string]string{"FAKE_ACPX_SCENARIO": scenario, "FAKE_ACPX_PERMISSION_LOG": permissionLog},
		},
	}
	return client, target, permissionLog
}

func TestRuntimeClientUsesPublicRuntimeAndPrivateState(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	info, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if info.ACPXRecordID != "acpx-record" || info.ACPSessionID != "acp-reconnected" || info.NativeSessionID != "native-reconnected" {
		t.Fatalf("Start() = %#v", info)
	}
	var events []Event
	receipt, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-runtime", Message: "hello"}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil || receipt.StopReason != "end_turn" || len(events) != 1 || events[0].Type != "agent_message_chunk" {
		t.Fatalf("Prompt() = %#v, events=%#v, error=%v", receipt, events, err)
	}
	if !strings.Contains(string(events[0].Raw), `"text":"ok"`) {
		t.Fatalf("normalized runtime event = %s", events[0].Raw)
	}
	stateInfo, err := os.Stat(client.stateDirectory)
	if err != nil || stateInfo.Mode().Perm() != 0o700 {
		t.Fatalf("runtime state mode = %v, %v", stateInfo, err)
	}
	scriptInfo, err := os.Stat(filepath.Join(client.stateDirectory, "runtime-bridge.mjs"))
	if err != nil || scriptInfo.Mode().Perm() != 0o600 {
		t.Fatalf("runtime bridge mode = %v, %v", scriptInfo, err)
	}
	cacheInfo, err := os.Stat(filepath.Join(filepath.Dir(client.stateDirectory), "npm-cache"))
	if err != nil || cacheInfo.Mode().Perm() != 0o700 {
		t.Fatalf("private npm cache mode = %v, %v", cacheInfo, err)
	}
}

func TestRuntimeProviderOptionsConfigureAfterIdentityWithGroupedCategoryOptionalOptions(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "config-arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "grouped"
	target.Profile.Environment["FAKE_ACPX_OMIT_MODEL_STATUS"] = "1"
	spec := StartSpec{Target: target, Model: "model-selected", ReasoningEffort: "high"}
	started, err := client.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(arguments)
	if strings.Contains(string(before), "--set-config") {
		t.Fatalf("provider options were applied before identity persistence boundary: %s", before)
	}
	spec.ResumeACPSessionID = started.ACPSessionID
	spec.ExpectedNativeSessionID = started.NativeSessionID
	configured, err := client.Configure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if configured.ACPSessionID != started.ACPSessionID || configured.NativeSessionID != started.NativeSessionID {
		t.Fatalf("configured identity = %#v, started = %#v", configured, started)
	}
	body, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--set-config\nmodel\nmodel-selected", "--set-config\neffort\nhigh"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("configuration log missing %q:\n%s", want, body)
		}
	}
}

func TestRuntimeProviderOptionsRejectUnsupportedAndAmbiguousAdvertisingAfterIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		shape  string
		model  string
		effort string
	}{
		{name: "unsupported model", shape: "flat", model: "missing-model"},
		{name: "unsupported effort", shape: "flat", effort: "ultra"},
		{name: "ambiguous effort", shape: "ambiguous", effort: "high"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, target, _ := runtimeFixture(t, "success")
			target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = test.shape
			started, err := client.Start(context.Background(), StartSpec{Target: target, Model: test.model, ReasoningEffort: test.effort})
			if err != nil || started.ACPSessionID == "" {
				t.Fatalf("identity was not established before configuration: %#v, %v", started, err)
			}
			_, err = client.Configure(context.Background(), StartSpec{
				Target: target, ResumeACPSessionID: started.ACPSessionID,
				ExpectedNativeSessionID: started.NativeSessionID, Model: test.model, ReasoningEffort: test.effort,
			})
			var configurationError *ProviderConfigurationError
			if !errors.As(err, &configurationError) {
				t.Fatalf("Configure() error = %T %v", err, err)
			}
			for _, secret := range []string{test.model, test.effort, "exactly one", "did not advertise"} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("Configure() exposed provider-controlled detail %q: %v", secret, err)
				}
			}
			if _, promptErr := client.Prompt(context.Background(), PromptSpec{Target: target, Message: "must not use a partially configured bridge"}, nil); promptErr == nil {
				t.Fatal("Prompt() remained available after provider configuration failure")
			}
		})
	}
}

func TestRuntimeProviderOptionsPersistExplicitValuesAlreadyCurrent(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "config-arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "flat"
	spec := StartSpec{Target: target, Model: "provider-default", ReasoningEffort: "medium"}
	started, err := client.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.ResumeACPSessionID = started.ACPSessionID
	spec.ExpectedNativeSessionID = started.NativeSessionID
	if _, err := client.Configure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--set-config\nmodel\nprovider-default", "--set-config\neffort\nmedium"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("explicit current option was not persisted (%q):\n%s", want, body)
		}
	}
}

func TestRuntimeProviderOptionsVerifyFinalInterdependentConfiguration(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "flat"
	target.Profile.Environment["FAKE_ACPX_EFFORT_RESETS_MODEL"] = "1"
	spec := StartSpec{Target: target, Model: "model-selected", ReasoningEffort: "high"}
	started, err := client.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.ResumeACPSessionID = started.ACPSessionID
	spec.ExpectedNativeSessionID = started.NativeSessionID
	if _, err := client.Configure(context.Background(), spec); err == nil {
		t.Fatal("Configure() accepted an effort change that reset the requested model")
	} else {
		var configurationError *ProviderConfigurationError
		if !errors.As(err, &configurationError) {
			t.Fatalf("Configure() error = %T %v", err, err)
		}
	}
	if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, Message: "must not run with the reset model"}, nil); err == nil {
		t.Fatal("Prompt() remained available after final provider configuration drift")
	}
}

func TestRuntimeProviderConfigurationIdentityDriftQuarantinesBridge(t *testing.T) {
	client, target, _ := runtimeFixture(t, "identity-drift-on-inspect")
	target.Profile.Environment["FAKE_ACPX_NATIVE_ID"] = "native-canonical"
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "flat"
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	spec := StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID,
		ExpectedNativeSessionID: started.NativeSessionID, Model: "model-selected",
	}
	if _, err := client.Configure(context.Background(), spec); err == nil {
		t.Fatal("Configure() accepted native identity drift")
	} else {
		var identityErr *IdentityError
		if !errors.As(err, &identityErr) {
			t.Fatalf("Configure() identity error = %T %v", err, err)
		}
	}
	target.Profile.Environment["FAKE_ACPX_SCENARIO"] = "success"
	if _, err := client.Resume(context.Background(), spec); err != nil {
		t.Fatalf("Resume() after configuration quarantine = %v", err)
	}
}

func TestRuntimeProviderOptionsAreReappliedAfterExactRefresh(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "config-arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "flat"
	target.Profile.Environment["FAKE_ACPX_RESET_CONFIG_ON_ENSURE"] = "1"
	spec := StartSpec{Target: target, Model: "model-selected", ReasoningEffort: "high"}
	started, err := client.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.ResumeACPSessionID = started.ACPSessionID
	spec.ExpectedNativeSessionID = started.NativeSessionID
	if _, err := client.Configure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Refresh(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Configure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "--set-config\nmodel\nmodel-selected") != 2 || strings.Count(string(body), "--set-config\neffort\nhigh") != 2 {
		t.Fatalf("options were not applied after both ensure and refresh:\n%s", body)
	}
}

func TestRuntimeAgentConfigurationExpandsFriendlyBuiltinsOnly(t *testing.T) {
	tests := []struct {
		name        string
		profile     agents.Profile
		wantAgent   string
		wantBuiltin bool
	}{
		{name: "friendly codex", profile: agents.Profile{Name: "codex", Command: "codex"}, wantAgent: "codex", wantBuiltin: true},
		{name: "friendly alias", profile: agents.Profile{Name: "team-codex", Command: "codex"}, wantAgent: "codex", wantBuiltin: true},
		{name: "friendly grok alias", profile: agents.Profile{Name: "grok", Command: "grok-build"}, wantAgent: "grok-build", wantBuiltin: true},
		{name: "gemini compatibility override", profile: agents.Profile{Name: "gemini", Command: "gemini", Args: []string{"--experimental-acp"}}, wantAgent: "gemini"},
		{name: "raw custom", profile: agents.Profile{Name: "custom", Command: "/opt/acp/custom", Args: []string{"--stdio"}}, wantAgent: "custom"},
		{name: "built-in name with raw args", profile: agents.Profile{Name: "codex", Command: "codex", Args: []string{"--stdio"}}, wantAgent: "codex"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotAgent, gotBuiltin := runtimeAgentConfiguration(test.profile)
			if gotAgent != test.wantAgent || gotBuiltin != test.wantBuiltin {
				t.Fatalf("runtimeAgentConfiguration() = (%q, %t), want (%q, %t)", gotAgent, gotBuiltin, test.wantAgent, test.wantBuiltin)
			}
		})
	}
}

func TestRuntimeBridgeUsesBuiltinRegistryOnlyForFriendlyProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profile  agents.Profile
		expected string
	}{
		{name: "friendly built-in", profile: agents.Profile{Name: "codex", Provider: "test", Command: "codex"}, expected: "builtin"},
		{name: "raw override", profile: agents.Profile{Name: "custom", Provider: "test", Command: "/opt/acp/custom", Args: []string{"--stdio"}}, expected: "override"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, target, _ := runtimeFixture(t, "success")
			test.profile.Environment = map[string]string{
				"FAKE_ACPX_SCENARIO":        "success",
				"FAKE_ACPX_EXPECT_REGISTRY": test.expected,
			}
			target.Profile = test.profile
			if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeReadApprovalForcesRestrictiveProviderModes(t *testing.T) {
	tests := []struct {
		agent string
		mode  string
	}{
		{agent: "codex", mode: "read-only"},
		{agent: "claude", mode: "default"},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			client, target, _ := runtimeFixture(t, "success")
			target.Profile = agents.Profile{
				Name: test.agent, Provider: "test", Command: test.agent,
				Environment: map[string]string{
					"FAKE_ACPX_SCENARIO":        "success",
					"FAKE_ACPX_EXPECT_REGISTRY": "builtin",
					"FAKE_ACPX_EXPECT_MODE":     test.mode,
				},
			}
			if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeCodexApproveAllDoesNotForceReadOnlyMode(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	target.Permission = PermissionApproveAll
	target.Profile = agents.Profile{
		Name: "codex", Provider: "test", Command: "codex",
		Environment: map[string]string{
			"FAKE_ACPX_SCENARIO":        "success",
			"FAKE_ACPX_EXPECT_REGISTRY": "builtin",
			"FAKE_ACPX_REJECT_SET_MODE": "1",
		},
	}
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeOpenCodePermissionPolicyOverridesPermissiveDefaults(t *testing.T) {
	tests := []struct {
		name       string
		permission PermissionMode
		expected   string
	}{
		{name: "read approval", permission: PermissionReadApproval, expected: "read-approval"},
		{name: "approve all", permission: PermissionApproveAll, expected: "approve-all"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, target, _ := runtimeFixture(t, "success")
			target.Permission = test.permission
			target.Profile = agents.Profile{
				Name: "opencode", Provider: "test", Command: "npx", Args: []string{"opencode-ai", "acp"},
				Environment: map[string]string{
					"FAKE_ACPX_SCENARIO":                   "success",
					"FAKE_ACPX_EXPECT_REGISTRY":            "override",
					"FAKE_ACPX_EXPECT_OPENCODE_PERMISSION": test.expected,
					"OPENCODE_CONFIG_CONTENT":              `{"permission":{"read":"allow","edit":"allow","bash":"allow"},"model":"keep-me"}`,
				},
			}
			if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRuntimeOpenCodePermissionPolicyPreservesStricterDenyRules(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	target.Profile = agents.Profile{
		Name: "opencode", Provider: "test", Command: "npx", Args: []string{"opencode-ai", "acp"},
		Environment: map[string]string{
			"FAKE_ACPX_SCENARIO":                   "success",
			"FAKE_ACPX_EXPECT_REGISTRY":            "override",
			"FAKE_ACPX_EXPECT_OPENCODE_PERMISSION": "read-approval",
			"FAKE_ACPX_EXPECT_OPENCODE_DENY_RULES": "1",
			"OPENCODE_CONFIG_CONTENT":              `{"permission":{"read":"allow","edit":{"*":"allow","*.secret":"deny"},"bash":"deny"},"model":"keep-me"}`,
		},
	}
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeOpenCodeInvalidInlineConfigFailsClosedWithoutLeakingIt(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	target.Profile = agents.Profile{
		Name: "opencode", Provider: "test", Command: "npx", Args: []string{"opencode-ai", "acp"},
		Environment: map[string]string{
			"FAKE_ACPX_SCENARIO":      "success",
			"OPENCODE_CONFIG_CONTENT": `{"token":"DO_NOT_LEAK"`,
		},
	}
	_, err := client.Start(context.Background(), StartSpec{Target: target})
	if err == nil || !strings.Contains(err.Error(), "must contain a valid JSON object") {
		t.Fatalf("Start() error = %v", err)
	}
	if strings.Contains(err.Error(), "DO_NOT_LEAK") {
		t.Fatalf("Start() leaked inline config: %v", err)
	}
}

func TestRuntimePermissionStaysPendingAndFirstDecisionWins(t *testing.T) {
	client, target, permissionLog := runtimeFixture(t, "permission")
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 8)
	promptDone := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-permission", Message: "write marker"}, func(event Event) error {
			events <- event
			return nil
		})
		promptDone <- err
	}()

	permission := waitRuntimePermission(t, events)
	select {
	case err := <-promptDone:
		t.Fatalf("turn ended while permission was pending: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, optionID := range []string{"allow-once", "reject-once"} {
		optionID := optionID
		go func() {
			<-start
			results <- client.DecidePermission(context.Background(), permission, optionID, false)
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "no longer pending") {
			conflicts++
		} else {
			t.Fatalf("unexpected permission decision error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("permission race = %d success, %d conflict", successes, conflicts)
	}
	select {
	case err := <-promptDone:
		if err != nil {
			t.Fatalf("Prompt() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not continue after permission decision")
	}
	body, err := os.ReadFile(permissionLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); !strings.Contains(got, `"outcome":"allow_once"`) && !strings.Contains(got, `"outcome":"reject_once"`) {
		t.Fatalf("provider decision = %q", got)
	}
}

func TestRuntimeHostPermissionBrokerAllowsRejectsAndCancels(t *testing.T) {
	tests := []struct {
		name       string
		scenario   string
		optionID   string
		cancel     bool
		wantAnswer string
	}{
		{name: "allow filesystem", scenario: "host-permission", optionID: "host-allow-once", wantAnswer: "y"},
		{name: "reject filesystem", scenario: "host-permission", optionID: "host-reject-once", wantAnswer: "n"},
		{name: "reject terminal", scenario: "host-permission-terminal", optionID: "host-reject-once", wantAnswer: "n"},
		{name: "cancel filesystem", scenario: "host-permission", cancel: true, wantAnswer: "n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, target, permissionLog := runtimeFixture(t, test.scenario)
			if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
				t.Fatal(err)
			}
			events := make(chan Event, 8)
			done := make(chan error, 1)
			go func() {
				_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-host", Message: "write marker"}, func(event Event) error {
					events <- event
					return nil
				})
				done <- err
			}()
			permissionID := waitRuntimePermission(t, events)
			select {
			case err := <-done:
				t.Fatalf("host turn ended while permission was pending: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if test.cancel {
				if err := client.Cancel(context.Background(), target); err != nil {
					t.Fatal(err)
				}
			} else if err := client.DecidePermission(context.Background(), permissionID, test.optionID, false); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Prompt() error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("host-permission turn did not finish")
			}
			body, err := os.ReadFile(permissionLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"host_answer":"`+test.wantAnswer+`"`) {
				t.Fatalf("host permission log = %q, want answer %q", body, test.wantAnswer)
			}
		})
	}
}

func TestRuntimeUnknownHostPermissionFailsClosedAndCanBeCancelled(t *testing.T) {
	client, target, permissionLog := runtimeFixture(t, "host-permission-unknown")
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 8)
	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-unknown-host", Message: "unknown host operation"}, func(event Event) error {
			events <- event
			return nil
		})
		done <- err
	}()
	select {
	case event := <-events:
		if event.Type == "session_request_permission" {
			t.Fatalf("unknown host operation unexpectedly offered approval: %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unknown host operation did not begin")
	}
	select {
	case event := <-events:
		if event.Type == "session_request_permission" {
			t.Fatalf("unknown host operation unexpectedly offered approval: %#v", event)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if err := client.Cancel(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unknown host prompt remained stranded after cancellation")
	}
	body, err := os.ReadFile(permissionLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"host_answer":"n"`) {
		t.Fatalf("unknown host permission log = %q", body)
	}
}

func TestRuntimePermissionAutoReadDuplicateFailClosedAndCancel(t *testing.T) {
	tests := []struct {
		name           string
		scenario       string
		wantPermission bool
		wantOutcome    string
		cancel         bool
	}{
		{name: "read is automatic", scenario: "permission-read", wantOutcome: "automatic"},
		{name: "duplicate kinds fail closed", scenario: "permission-duplicate", wantOutcome: "cancel"},
		{name: "cancel pending request", scenario: "permission", wantPermission: true, wantOutcome: "cancel", cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, target, permissionLog := runtimeFixture(t, test.scenario)
			if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
				t.Fatal(err)
			}
			events := make(chan Event, 8)
			done := make(chan error, 1)
			go func() {
				_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-case", Message: "test"}, func(event Event) error {
					events <- event
					return nil
				})
				done <- err
			}()
			if test.wantPermission {
				_ = waitRuntimePermission(t, events)
				if test.cancel {
					if err := client.Cancel(context.Background(), target); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("Prompt() error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("turn did not finish")
			}
			body, err := os.ReadFile(permissionLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `"outcome":"`+test.wantOutcome+`"`) {
				t.Fatalf("permission log = %q, want %q", body, test.wantOutcome)
			}
		})
	}
}

func TestRuntimePermissionBridgeCapsPendingCallbacks(t *testing.T) {
	client, target, permissionLog := runtimeFixture(t, "permission-overflow")
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 256)
	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-overflow", Message: "overflow"}, func(event Event) error {
			events <- event
			return nil
		})
		done <- err
	}()
	for index := 0; index < 128; index++ {
		select {
		case event := <-events:
			if event.Type != "session_request_permission" {
				t.Fatalf("event %d type = %q", index, event.Type)
			}
			if strings.Contains(string(event.Raw), "ignored") || len(event.Raw) > 32<<10 {
				t.Fatalf("permission %d retained unvalidated payload: %d bytes %s", index, len(event.Raw), event.Raw)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("received %d pending permissions, want 128", index)
		}
	}
	select {
	case extra := <-events:
		t.Fatalf("bridge emitted permission beyond cap: %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}
	if err := client.Cancel(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("overflow turn did not cancel")
	}
	body, err := os.ReadFile(permissionLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), `"cancel"`) != 129 || strings.Contains(string(body), strings.Repeat("x", 1000)) {
		t.Fatalf("overflow outcomes were not bounded and cancelled: %s", body)
	}
}

func TestRuntimeResumeRequiresExactACPIdentity(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	info, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeACPSessionID: "acp-existing"})
	if err != nil || info.ACPSessionID != "acp-existing" {
		t.Fatalf("Resume() = %#v, %v", info, err)
	}
}

func TestRuntimeRefreshResumesExactACPIdentity(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	started, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeACPSessionID: "acp-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := client.Refresh(context.Background(), StartSpec{Target: target, ResumeACPSessionID: started.ACPSessionID})
	if err != nil || refreshed.ACPSessionID != started.ACPSessionID {
		t.Fatalf("Refresh() = %#v, %v", refreshed, err)
	}
}

func TestRuntimeResumeAndRefreshClassifyProviderActivationConflict(t *testing.T) {
	refreshClient, refreshTarget, _ := runtimeFixture(t, "activation-conflict-on-refresh")
	started, err := refreshClient.Start(context.Background(), StartSpec{Target: refreshTarget})
	if err != nil {
		t.Fatal(err)
	}
	_, err = refreshClient.Refresh(context.Background(), StartSpec{
		Target: refreshTarget, ResumeACPSessionID: started.ACPSessionID,
	})
	assertProviderActivationConflict(t, err)

	resumeClient, resumeTarget, _ := runtimeFixture(t, "activation-conflict")
	_, err = resumeClient.Resume(context.Background(), StartSpec{
		Target: resumeTarget, ResumeACPSessionID: "acp-existing",
	})
	assertProviderActivationConflict(t, err)
}

func assertProviderActivationConflict(t *testing.T, err error) {
	t.Helper()
	var providerErr *ProviderTurnError
	if !errors.As(err, &providerErr) || providerErr.Code != "turn_failed" || providerErr.DetailCode != "conversation_conflict" || !providerErr.Retryable {
		t.Fatalf("provider activation conflict = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "bridge_frame_too_large") {
		t.Fatalf("provider activation conflict was conflated with bridge integrity: %v", err)
	}
}

func TestRuntimeResumeAndRefreshRejectCanonicalProviderIdentityDrift(t *testing.T) {
	client, target, _ := runtimeFixture(t, "identity-drift-on-refresh")
	target.Profile.Environment["FAKE_ACPX_NATIVE_ID"] = "native-canonical"
	started, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: "acp-canonical", ExpectedProviderConversationID: "acp-canonical", ExpectedNativeSessionID: "native-canonical",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Refresh(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID, ExpectedProviderConversationID: "acp-canonical", ExpectedNativeSessionID: "native-canonical",
	}); err == nil || !strings.Contains(err.Error(), "expected native adapter ID") {
		t.Fatalf("Refresh() identity error = %v", err)
	}
	if _, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID, ExpectedProviderConversationID: "acp-canonical", ExpectedNativeSessionID: "native-canonical",
	}); err != nil {
		t.Fatalf("Resume() after discarded mismatched bridge = %v", err)
	}
	mismatchedClient, mismatchedTarget, _ := runtimeFixture(t, "success")
	mismatchedTarget.Profile.Environment["FAKE_ACPX_NATIVE_ID"] = "native-replacement"
	if _, err := mismatchedClient.Resume(context.Background(), StartSpec{
		Target: mismatchedTarget, ResumeACPSessionID: started.ACPSessionID, ExpectedProviderConversationID: "acp-canonical", ExpectedNativeSessionID: "native-canonical",
	}); err == nil || !strings.Contains(err.Error(), "expected native adapter ID") {
		t.Fatalf("Resume() identity error = %v", err)
	}
}

func TestRuntimeProviderStoreVerificationRejectsNativeDriftMaskedByACP(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	target.Profile.Environment["FAKE_ACPX_NATIVE_ID"] = "native-replacement"
	_, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: "canonical-acp",
		ExpectedProviderConversationID: "canonical-acp", ProviderStoreVerified: true,
	})
	var identityErr *IdentityError
	if !errors.As(err, &identityErr) || !strings.Contains(err.Error(), "provider-store verified") {
		t.Fatalf("provider-store drift error = %T %v", err, err)
	}
}

func TestRuntimeInspectQuarantinesIdentityDriftBeforeExactResume(t *testing.T) {
	client, target, _ := runtimeFixture(t, "identity-drift-on-inspect")
	target.Profile.Environment["FAKE_ACPX_NATIVE_ID"] = "native-canonical"
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inspect(context.Background(), target); err == nil {
		t.Fatal("Inspect() accepted identity drift")
	} else {
		var identityErr *IdentityError
		if !errors.As(err, &identityErr) {
			t.Fatalf("Inspect() identity drift error = %T %v", err, err)
		}
	}
	target.Profile.Environment["FAKE_ACPX_SCENARIO"] = "success"
	if _, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID,
		ExpectedProviderConversationID: started.NativeSessionID, ExpectedNativeSessionID: started.NativeSessionID,
	}); err != nil {
		t.Fatalf("exact Resume() after inspect quarantine = %v", err)
	}
}

func TestRuntimeRefreshTimeoutDiscardsBridgeAndAllowsExactResume(t *testing.T) {
	client, target, _ := runtimeFixture(t, "refresh-hang")
	started, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: "acp-timeout", ExpectedProviderConversationID: "native-reconnected",
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = client.Refresh(refreshContext, StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID, ExpectedProviderConversationID: "native-reconnected",
	})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Refresh() timeout error = %v", err)
	}
	target.Profile.Environment["FAKE_ACPX_SCENARIO"] = "success"
	if _, err := client.Resume(context.Background(), StartSpec{
		Target: target, ResumeACPSessionID: started.ACPSessionID, ExpectedProviderConversationID: "native-reconnected",
	}); err != nil {
		t.Fatalf("exact Resume() after timed-out refresh = %v", err)
	}
}

func TestRuntimeTurnFailureIsCorrelatedButBridgeErrorIsProtocolFailure(t *testing.T) {
	receipt := PromptReceipt{RequestID: "REQ-provider-failed", LastSeq: 7}
	returned, err := runtimeTurnResult(receipt, json.RawMessage(`{"status":"failed","error":{"code":"turn_failed","detailCode":"provider_busy","message":"conversation is busy","retryable":true}}`))
	var providerErr *ProviderTurnError
	if !errors.As(err, &providerErr) || providerErr.Code != "turn_failed" || providerErr.DetailCode != "provider_busy" || !providerErr.Retryable || returned != receipt {
		t.Fatalf("failed turn result = %#v, %T %v", returned, err, err)
	}

	err = bridgeRemoteError(bridgeMessage{Type: "error", Code: "bridge_frame_too_large", Message: "invalid frame"})
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("bridge error = %T %v, want ProtocolError", err, err)
	}
}

func TestRuntimePromptClassifiesOnlyAllowlistedProviderBridgeErrors(t *testing.T) {
	providerClient, providerTarget, _ := runtimeFixture(t, "prompt-provider-conflict")
	if _, err := providerClient.Start(context.Background(), StartSpec{Target: providerTarget}); err != nil {
		t.Fatal(err)
	}
	_, err := providerClient.Prompt(context.Background(), PromptSpec{
		Target: providerTarget, RequestID: "REQ-provider-bridge", Message: "continue",
	}, nil)
	var providerErr *ProviderTurnError
	if !errors.As(err, &providerErr) || providerErr.Code != "turn_failed" || providerErr.DetailCode != "provider_busy" || !providerErr.Retryable {
		t.Fatalf("provider prompt bridge error = %T %v", err, err)
	}

	bridgeClient, bridgeTarget, _ := runtimeFixture(t, "prompt-bridge-frame-error")
	if _, err := bridgeClient.Start(context.Background(), StartSpec{Target: bridgeTarget}); err != nil {
		t.Fatal(err)
	}
	_, err = bridgeClient.Prompt(context.Background(), PromptSpec{
		Target: bridgeTarget, RequestID: "REQ-bridge-integrity", Message: "continue",
	}, nil)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("bridge integrity prompt error = %T %v, want ProtocolError", err, err)
	}
	if errors.As(err, &providerErr) {
		t.Fatalf("bridge integrity prompt error was classified as provider failure: %v", err)
	}
}

func waitRuntimePermission(t *testing.T, events <-chan Event) string {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type != "session_request_permission" {
				continue
			}
			var envelope struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(event.Raw, &envelope); err != nil || envelope.ID == "" {
				t.Fatalf("permission event = %s, %v", event.Raw, err)
			}
			return envelope.ID
		case <-timer.C:
			t.Fatal("permission request was not emitted")
		}
	}
}

func TestInstalledPackageRootRejectsUnpinnedRuntime(t *testing.T) {
	root := t.TempDir()
	manifest := `{"name":"acpx","version":"0.14.0","exports":{"./runtime":"./runtime.js"}}`
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "bin", "acpx")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := installedPackageRoot(binary)
	if err == nil || !strings.Contains(err.Error(), SupportedACPXVersion) {
		t.Fatalf("installedPackageRoot() error = %v", err)
	}
}
