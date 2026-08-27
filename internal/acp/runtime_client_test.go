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
	"github.com/yty-build/acp-gateway/internal/session"
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

func TestRuntimeClientRetainsOneBridgeAndStreamsUntouchedEvents(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	info, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if info.ProviderRef != "acp-reconnected" || info.RuntimeRef == "" {
		t.Fatalf("Start() = %#v", info)
	}
	var events []Event
	receipt, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-runtime", Message: "hello"}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil || receipt.StopReason != "end_turn" || len(events) != 1 {
		t.Fatalf("Prompt() = %#v, events=%#v, error=%v", receipt, events, err)
	}
	want := `{"type":"text_delta","stream":"output","text":"ok","tag":"agent_message_chunk"}`
	if events[0].Type != "text_delta" || string(events[0].Raw) != want {
		t.Fatalf("runtime event = type %q raw %s, want untouched %s", events[0].Type, events[0].Raw, want)
	}
	if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-second", Message: "again"}, nil); err != nil {
		t.Fatalf("second Prompt() on retained bridge = %v", err)
	}
	stateInfo, err := os.Stat(client.stateDirectory)
	if err != nil || stateInfo.Mode().Perm() != 0o700 {
		t.Fatalf("runtime state mode = %v, %v", stateInfo, err)
	}
}

func TestRuntimeClientNeverReconnectsInsidePromptOrControlCommands(t *testing.T) {
	t.Run("loss_between_turns", func(t *testing.T) {
		client, target, _ := runtimeFixture(t, "success")
		arguments := filepath.Join(t.TempDir(), "arguments.log")
		target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
		target.Profile.Environment["FAKE_ACPX_DROP_RETAINED_AFTER_TURN"] = "1"
		if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-first", Message: "first prompt"}, nil); err != nil {
			t.Fatal(err)
		}
		_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-second", Message: "second prompt"}, nil)
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || !strings.Contains(err.Error(), "retained provider Port") {
			t.Fatalf("second Prompt after retained loss = %T %v", err, err)
		}
		body, readErr := os.ReadFile(arguments)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(body), "second prompt") || strings.Contains(string(body), "--hidden-resume") {
			t.Fatalf("retained loss performed hidden provider work:\n%s", body)
		}
	})

	t.Run("loss_before_options", func(t *testing.T) {
		client, target, _ := runtimeFixture(t, "success")
		target.Profile.Environment["FAKE_ACPX_DROP_RETAINED_AFTER_TURN"] = "1"
		started, err := client.Start(context.Background(), StartSpec{Target: target})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-first", Message: "first prompt"}, nil); err != nil {
			t.Fatal(err)
		}
		_, err = client.ProviderOptions(context.Background(), StartSpec{Target: target, ResumeProviderRef: started.ProviderRef})
		var protocol *ProtocolError
		var unavailable *ProviderOptionsError
		if !errors.As(err, &protocol) || errors.As(err, &unavailable) {
			t.Fatalf("ProviderOptions after retained loss = %T %v", err, err)
		}
	})

	for _, test := range []struct {
		name string
		env  string
		run  func(*RuntimeClient, Target, SessionInfo) error
	}{
		{
			name: "loss_during_prompt", env: "FAKE_ACPX_DROP_RETAINED_DURING_TURN",
			run: func(client *RuntimeClient, target Target, _ SessionInfo) error {
				_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-race", Message: "must not dispatch"}, nil)
				return err
			},
		},
		{
			name: "loss_during_configure", env: "FAKE_ACPX_DROP_RETAINED_DURING_CONFIG",
			run: func(client *RuntimeClient, target Target, started SessionInfo) error {
				_, err := client.Configure(context.Background(), StartSpec{
					Target: target, ResumeProviderRef: started.ProviderRef, Model: "model-selected", ReasoningEffort: "high",
				})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, target, _ := runtimeFixture(t, "success")
			arguments := filepath.Join(t.TempDir(), "arguments.log")
			target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
			target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "grouped"
			target.Profile.Environment[test.env] = "1"
			started, err := client.Start(context.Background(), StartSpec{Target: target})
			if err != nil {
				t.Fatal(err)
			}
			err = test.run(client, target, started)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || !strings.Contains(err.Error(), "retained provider Port") {
				t.Fatalf("%s = %T %v", test.name, err, err)
			}
			body, readErr := os.ReadFile(arguments)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(body), "must not dispatch") || strings.Contains(string(body), "--set-config") || strings.Contains(string(body), "--hidden-resume") {
				t.Fatalf("%s performed hidden provider work:\n%s", test.name, body)
			}
		})
	}
}

func TestRuntimeClientTreatsNoSessionTurnResultAsLostPort(t *testing.T) {
	client, target, _ := runtimeFixture(t, "prompt-no-session")
	arguments := filepath.Join(t.TempDir(), "arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-first", Message: "first prompt"}, nil)
	var protocol *ProtocolError
	var provider *ProviderTurnError
	if !errors.As(err, &protocol) || errors.As(err, &provider) || !strings.Contains(err.Error(), "saved conversation") {
		t.Fatalf("NO_SESSION prompt result = %T %v", err, err)
	}
	if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-second", Message: "second prompt"}, nil); err == nil {
		t.Fatal("second prompt used a Port that reported NO_SESSION")
	}
	body, readErr := os.ReadFile(arguments)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(body), "second prompt") || strings.Contains(string(body), "--hidden-resume") {
		t.Fatalf("NO_SESSION performed later provider work:\n%s", body)
	}
}

func TestRuntimeClientCancelStillProvesRetainedPortAtTurnCompletion(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_DROP_RETAINED_ON_CANCEL"] = "1"
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 1)
	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := client.Prompt(context.Background(), PromptSpec{
			Target: target, RequestID: "REQ-cancel-retained", Message: "cancel this prompt",
		}, func(event Event) error {
			events <- event
			return nil
		})
		promptDone <- promptErr
	}()
	select {
	case event := <-events:
		if event.Type != "status" {
			t.Fatalf("first cancellation event = %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider turn did not start")
	}
	if err := client.Cancel(context.Background(), target); err != nil {
		t.Fatalf("Cancel() provider command = %v", err)
	}
	select {
	case err := <-promptDone:
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || !strings.Contains(err.Error(), "retained provider Port") {
			t.Fatalf("cancelled Prompt() = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Prompt did not drain its terminal Port proof")
	}
	if _, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-after-cancel", Message: "must not dispatch"}, nil); err == nil {
		t.Fatal("Prompt reused the Port after cancellation lost continuity")
	}

	replacement := NewRuntimeClient(client.binary, t.TempDir())
	if err := replacement.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	resumed, err := replacement.Resume(context.Background(), StartSpec{Target: target, ResumeProviderRef: started.ProviderRef})
	if err != nil || resumed.ProviderRef != started.ProviderRef {
		t.Fatalf("explicit exact Resume after cancellation = %#v, %v", resumed, err)
	}
	body, readErr := os.ReadFile(arguments)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(body), "must not dispatch") || strings.Contains(string(body), "--hidden-resume") {
		t.Fatalf("cancellation performed hidden provider work:\n%s", body)
	}
	if got := strings.Count(string(body), "--resume-session\n"+started.ProviderRef); got != 1 {
		t.Fatalf("exact Resume count = %d, arguments:\n%s", got, body)
	}
}

func TestRuntimeClientCancelFencesTurnActivationBeforeProviderDispatch(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "arguments.log")
	gate := filepath.Join(t.TempDir(), "release-prompt-activation")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_PROMPT_ACTIVATION_GATE"] = gate
	t.Cleanup(func() { _ = os.WriteFile(gate, []byte("release\n"), 0o600) })
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}

	type promptOutcome struct {
		receipt PromptReceipt
		err     error
	}
	promptDone := make(chan promptOutcome, 1)
	go func() {
		receipt, err := client.Prompt(context.Background(), PromptSpec{
			Target: target, RequestID: "REQ-cancel-activation", Message: "must never reach provider",
		}, nil)
		promptDone <- promptOutcome{receipt: receipt, err: err}
	}()
	entered := gate + ".entered"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prompt did not enter retained-Port activation preflight")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancelContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := client.Cancel(cancelContext, target)
	cancel()
	if err != nil {
		t.Fatalf("Cancel() during turn activation = %v", err)
	}
	if err := os.WriteFile(gate, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-promptDone:
		if outcome.err != nil || outcome.receipt.StopReason != "cancelled" {
			t.Fatalf("activation-fenced Prompt() = %#v, %v", outcome.receipt, outcome.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("activation-fenced Prompt did not settle")
	}
	body, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "must never reach provider") {
		t.Fatalf("cancelled activation dispatched provider prompt:\n%s", body)
	}
}

func TestRuntimeResumeUsesExactSavedProviderReferenceAtStartup(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	info, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeProviderRef: "acp-existing"})
	if err != nil || info.ProviderRef != "acp-existing" {
		t.Fatalf("Resume() = %#v, %v", info, err)
	}
	if _, err := client.Resume(context.Background(), StartSpec{Target: target}); err == nil {
		t.Fatal("Resume() accepted an empty provider reference")
	}
}

func TestRuntimeCloseSurfacesUnsafePriorBridgeTeardown(t *testing.T) {
	client := &RuntimeClient{blocked: errors.New("provider process group did not exit")}
	if err := client.Close(); err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("Close() blocked teardown error = %v", err)
	}
}

func TestRuntimeProviderOptionsAndConfigureUseRetainedBridge(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "grouped"
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	spec := StartSpec{Target: target, ResumeProviderRef: started.ProviderRef}
	catalog, err := client.ProviderOptions(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	selection := session.ProviderOptions{Model: "model-selected", ReasoningEffort: "high"}
	if err := catalog.ValidateSelection(selection); err != nil {
		t.Fatalf("provider catalog rejected advertised values: %v; %#v", err, catalog)
	}
	spec.Model, spec.ReasoningEffort = selection.Model, selection.ReasoningEffort
	configured, err := client.Configure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if configured.ProviderRef != started.ProviderRef {
		t.Fatalf("Configure changed provider reference: %#v then %#v", started, configured)
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

func TestRuntimeProviderOptionsPreserveOpaqueTokensExactly(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	arguments := filepath.Join(t.TempDir(), "arguments.log")
	target.Profile.Environment["FAKE_ACPX_ARGS_LOG"] = arguments
	target.Profile.Environment["FAKE_ACPX_CONFIG_OPTIONS"] = "opaque"
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	spec := StartSpec{
		Target: target, ResumeProviderRef: started.ProviderRef,
		Model: " model @ 2 ", ReasoningEffort: " effort + 2 ",
	}
	catalog, err := client.ProviderOptions(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateSelection(session.ProviderOptions{Model: spec.Model, ReasoningEffort: spec.ReasoningEffort}); err != nil {
		t.Fatalf("opaque advertised selection rejected: %v; %#v", err, catalog)
	}
	if _, err := client.Configure(context.Background(), spec); err != nil {
		body, _ := os.ReadFile(arguments)
		t.Fatalf("Configure opaque tokens: %v\n%s", err, body)
	}
	body, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{"--set-config\nmodel\n model @ 2 \n", "--set-config\neffort\n effort + 2 \n"} {
		if !strings.Contains(string(body), exact) {
			t.Fatalf("configuration log changed opaque token %q:\n%s", exact, body)
		}
	}
}

func TestRuntimePermissionPayloadIsUntouchedAndDecisionUsesBridgeID(t *testing.T) {
	client, target, permissionLog := runtimeFixture(t, "permission")
	if _, err := client.Start(context.Background(), StartSpec{Target: target}); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 4)
	done := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-permission", Message: "write"}, func(event Event) error {
			events <- event
			return nil
		})
		done <- err
	}()
	var permission Event
	select {
	case permission = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("permission event was not streamed")
	}
	if permission.Type != "session_request_permission" || permission.PermissionID == "" {
		t.Fatalf("permission event = %#v", permission)
	}
	var raw map[string]any
	if err := json.Unmarshal(permission.Raw, &raw); err != nil || raw["toolCall"] == nil || raw["options"] == nil || raw["jsonrpc"] != nil {
		t.Fatalf("permission raw payload = %s, %v", permission.Raw, err)
	}
	if err := client.DecidePermission(context.Background(), permission.PermissionID, "allow-once", false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt() after decision = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not continue after permission decision")
	}
	body, err := os.ReadFile(permissionLog)
	if err != nil || !strings.Contains(string(body), `"outcome":"allow_once"`) {
		t.Fatalf("permission log = %s, %v", body, err)
	}
}

func TestRuntimePermissionProtocolFailureDiscardsRetainedBridge(t *testing.T) {
	client, target, _ := runtimeFixture(t, "permission")
	started, err := client.Start(context.Background(), StartSpec{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 4)
	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := client.Prompt(context.Background(), PromptSpec{Target: target, RequestID: "REQ-permission-protocol", Message: "write"}, func(event Event) error {
			events <- event
			return nil
		})
		promptDone <- promptErr
	}()
	var permission Event
	select {
	case permission = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("permission event was not streamed")
	}
	decisionErr := client.DecidePermission(context.Background(), permission.PermissionID, "not-offered", false)
	var protocol *ProtocolError
	var rejected *ProviderPermissionRejectedError
	if !errors.As(decisionErr, &protocol) || errors.As(decisionErr, &rejected) {
		t.Fatalf("invalid permission protocol response = %T %v", decisionErr, decisionErr)
	}
	client.mu.Lock()
	bridge := client.bridge
	client.mu.Unlock()
	if bridge != nil {
		t.Fatal("permission protocol failure retained an ambiguous provider Port")
	}
	if _, err := client.ProviderOptions(context.Background(), StartSpec{Target: target, ResumeProviderRef: started.ProviderRef}); err == nil {
		t.Fatal("discarded permission Port remained reusable")
	}
	select {
	case <-promptDone:
	case <-time.After(2 * time.Second):
		t.Fatal("discarding the permission Port did not release its active prompt")
	}
}

func TestProviderPermissionRejectionClassificationIsExplicit(t *testing.T) {
	for _, message := range []bridgeMessage{
		{Type: "error", Code: "permission_rejected"},
		{Type: "error", DetailCode: "permission-response-rejected"},
		{Type: "error", Code: "provider_rejected"},
	} {
		if !providerPermissionRejectionError(bridgeRemoteError(message)) {
			t.Fatalf("explicit permission rejection was not classified: %#v", message)
		}
	}
	for _, message := range []bridgeMessage{
		{Type: "error", Code: "rejected"},
		{Type: "error", Code: "provider_busy"},
		{Type: "error", Code: "retained_port_unavailable"},
		{Type: "error", Code: "permission_rejected", DetailCode: strings.Repeat("x", 129)},
	} {
		if message.Code == "permission_rejected" {
			message.Code = ""
		}
		if providerPermissionRejectionError(bridgeRemoteError(message)) {
			t.Fatalf("non-definite permission failure was retryable: %#v", message)
		}
	}
}

func TestRuntimeEventPreservesKnownAndFutureNormalizedTypes(t *testing.T) {
	raw := json.RawMessage(`{"type":"tool_call","tag":"tool_call_update","toolCallId":"t-1","title":"Run"}`)
	event, err := runtimeEvent(PromptSpec{Target: Target{ConsoleSessionID: "S"}, RequestID: "REQ"}, 2, raw)
	if err != nil || string(event.Raw) != string(raw) || event.Type != "tool_call" {
		t.Fatalf("runtimeEvent() = %#v, %v", event, err)
	}
	future := json.RawMessage(`{"type":"provider_future_progress","percent":42,"providerField":{"kept":true}}`)
	event, err = runtimeEvent(PromptSpec{Target: Target{ConsoleSessionID: "S"}, RequestID: "REQ"}, 3, future)
	if err != nil || event.Type != "provider_future_progress" || string(event.Raw) != string(future) {
		t.Fatalf("future runtimeEvent() = %#v, %v", event, err)
	}
}

func TestRuntimeEventRejectsMissingMalformedOrUnsafeType(t *testing.T) {
	oversized, err := json.Marshal(map[string]any{"type": strings.Repeat("x", maxNormalizedEventTypeBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]json.RawMessage{
		"missing":                json.RawMessage(`{"text":"hello"}`),
		"non-string":             json.RawMessage(`{"type":7}`),
		"empty":                  json.RawMessage(`{"type":""}`),
		"whitespace-only":        json.RawMessage(`{"type":"   "}`),
		"surrounding whitespace": json.RawMessage(`{"type":" status "}`),
		"control character":      json.RawMessage(`{"type":"status\nforged"}`),
		"oversized":              oversized,
		"invalid JSON":           json.RawMessage(`{"type":`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runtimeEvent(PromptSpec{}, 1, raw); err == nil {
				t.Fatalf("runtimeEvent accepted unsafe event: %s", raw)
			}
		})
	}
}

func TestRuntimePermissionKeepsPrintableWhitespaceTokensOpaque(t *testing.T) {
	request := json.RawMessage(`{"toolCall":{"toolCallId":" ","title":"Write","kind":"edit"},"options":[{"optionId":" ","name":"Allow","kind":"allow_once"}]}`)
	event, err := runtimePermissionEvent(PromptSpec{Target: Target{ConsoleSessionID: "S"}, RequestID: "REQ"}, 1, bridgeMessage{
		PermissionID: " ", Request: request,
	})
	if err != nil || event.PermissionID != " " || string(event.Raw) != string(request) {
		t.Fatalf("runtimePermissionEvent whitespace tokens = %#v, %v", event, err)
	}
	client := &RuntimeClient{}
	err = client.DecidePermission(context.Background(), " ", " ", false)
	if err == nil || strings.Contains(err.Error(), "required") {
		t.Fatalf("DecidePermission rejected a nonempty opaque whitespace token: %v", err)
	}
}

func TestRuntimeTurnAndBridgeFailuresRemainDistinct(t *testing.T) {
	receipt := PromptReceipt{RequestID: "REQ-provider-failed", LastSeq: 7}
	returned, err := runtimeTurnResult(receipt, json.RawMessage(`{"status":"failed","error":{"code":"turn_failed","detailCode":"provider_busy","message":"conversation is busy","retryable":true}}`))
	var providerErr *ProviderTurnError
	if !errors.As(err, &providerErr) || providerErr.DetailCode != "provider_busy" || returned != receipt {
		t.Fatalf("failed turn result = %#v, %T %v", returned, err, err)
	}
	err = bridgeRemoteError(bridgeMessage{Type: "error", Code: "bridge_frame_too_large", Message: "invalid frame"})
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("bridge error = %T %v, want ProtocolError", err, err)
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
