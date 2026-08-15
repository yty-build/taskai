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

func TestRuntimeResumeRequiresExactACPIdentity(t *testing.T) {
	client, target, _ := runtimeFixture(t, "success")
	info, err := client.Resume(context.Background(), StartSpec{Target: target, ResumeACPSessionID: "acp-existing"})
	if err != nil || info.ACPSessionID != "acp-existing" {
		t.Fatalf("Resume() = %#v, %v", info, err)
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
