package connector_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/testkit"
)

func TestCodexManagerPrefixOnceAndExactResumeWithoutReinjection(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	agentsPath := filepath.Join(project, "AGENTS.md")
	sentinel := []byte("NATIVE-PROJECT-INSTRUCTION-SENTINEL\n")
	if err := os.WriteFile(agentsPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileFingerprint(t, agentsPath)
	claudePath := filepath.Join(project, "CLAUDE.md")
	if _, err := os.Stat(claudePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLAUDE.md unexpectedly exists before test: %v", err)
	}

	startApp := &recordingAppServer{reference: "opaque-native-reference", version: connector.CodexAppServerVersion, handlerRegistered: true}
	resumeApp := &recordingAppServer{reference: "opaque-native-reference", version: connector.CodexAppServerVersion, handlerRegistered: true}
	apps := &recordingAppFactory{apps: []*recordingAppServer{startApp, resumeApp}}
	manager := connector.CodexManager{Apps: apps}
	started, err := manager.Start(context.Background(), managerStartRequest("manager-start", project, connector.NewRegistry(), &testkit.EventRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = started.Connection.Close() })
	expectedReference, err := replay.NewCodexReference(startApp.reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(started.NativeReference.Bytes(), expectedReference.Bytes()) {
		t.Fatal("Start did not return the canonical opaque reference")
	}
	resumed, err := manager.Resume(context.Background(), managerResumeRequest("manager-resume", started.NativeReference, connector.NewRegistry(), &testkit.EventRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Connection.Close() })
	if !bytes.Equal(resumed.NativeReference.Bytes(), started.NativeReference.Bytes()) {
		t.Fatal("Resume changed the opaque reference envelope")
	}
	handlers := apps.snapshotHandlers()
	if len(handlers) != 2 || handlers[0] == handlers[1] {
		t.Fatalf("Start/Resume reused a Delegator: %#v", handlers)
	}

	startApp.mu.Lock()
	if len(startApp.starts) != 1 {
		t.Fatalf("start calls=%d", len(startApp.starts))
	}
	start := startApp.starts[0]
	startApp.mu.Unlock()
	resumeApp.mu.Lock()
	if len(resumeApp.resumes) != 1 {
		t.Fatalf("resume calls=%d", len(resumeApp.resumes))
	}
	resume := resumeApp.resumes[0]
	resumeApp.mu.Unlock()
	if start.CWD != project || start.DeveloperInstructions != connector.ManagerPrefix || strings.Count(start.DeveloperInstructions, connector.ManagerPrefix) != 1 {
		t.Fatalf("Start prefix/cwd mismatch")
	}
	if strings.Contains(start.DeveloperInstructions, string(sentinel)) {
		t.Fatal("connector manually copied native project instructions into the prefix")
	}
	startApp.mu.Lock()
	composed := startApp.composed[0]
	startApp.mu.Unlock()
	if !strings.HasPrefix(composed, connector.ManagerPrefix) || strings.Index(composed, connector.ManagerPrefix) > strings.Index(composed, string(sentinel)) {
		t.Fatalf("provider instruction order is not prefix then native instructions")
	}
	if resume.ThreadID != startApp.reference {
		t.Fatal("Resume did not resolve the exact connector-native thread ID")
	}
	if len(start.DynamicTools) != 1 || start.DynamicTools[0].Type != "function" || start.DynamicTools[0].Name != connector.DelegateToolName {
		t.Fatalf("Start dynamic tools = %#v", start.DynamicTools)
	}
	encodedResume, err := json.Marshal(resume)
	if err != nil {
		t.Fatal(err)
	}
	var resumeFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedResume, &resumeFields); err != nil {
		t.Fatal(err)
	}
	if len(resumeFields) != 1 || resumeFields["threadId"] == nil {
		t.Fatalf("Resume fields = %v, want only threadId", reflect.ValueOf(resumeFields).MapKeys())
	}
	for _, forbidden := range []string{"dynamicTools", "developerInstructions", "cwd", "prompt", "userContext"} {
		if resumeFields[forbidden] != nil {
			t.Fatalf("Resume reinjected forbidden field %q", forbidden)
		}
	}

	after := fileFingerprint(t, agentsPath)
	if before != after {
		t.Fatalf("AGENTS.md changed: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(claudePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent CLAUDE.md was created: %v", err)
	}
}

func TestCodexManagerResumeNeverFallsBackToStart(t *testing.T) {
	t.Parallel()
	app := &recordingAppServer{reference: "different-reference", version: connector.CodexAppServerVersion, handlerRegistered: true}
	manager := connector.CodexManager{Apps: &recordingAppFactory{apps: []*recordingAppServer{app}}}
	ownedReference, _ := replay.NewCodexReference("owned-reference")
	_, err := manager.Resume(context.Background(), managerResumeRequest("manager", ownedReference, connector.NewRegistry(), &testkit.EventRecorder{}))
	if err == nil {
		t.Fatal("Resume accepted replacement reference")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.starts) != 0 || len(app.resumes) != 1 {
		t.Fatalf("Resume fallback calls: starts=%d resumes=%d", len(app.starts), len(app.resumes))
	}
	if strings.Contains(err.Error(), "owned-reference") || strings.Contains(err.Error(), "different-reference") {
		t.Fatalf("resume error leaked native reference: %v", err)
	}
}

func TestCodexManagerResumeRestoresAllChildrenBeforeProviderAndExactResumesChild(t *testing.T) {
	t.Parallel()
	managerReference, _ := replay.NewCodexReference("manager-restored")
	childReference, _ := replay.NewCodexReference("CHILD-REFERENCE-CANARY")
	otherReference, _ := replay.NewCodexReference("child-other")
	promptCanary := "CHILD-PROMPT-CANARY"
	promptSeen := make(chan string, 1)
	children := testkit.NewScriptedFactory(testkit.ChildScript{
		OpaqueReference: childReference, PromptSeen: promptSeen,
	})
	registry := managerResumableChildRegistry(t, children)
	events := &testkit.EventRecorder{}
	app := &recordingAppServer{
		reference: "manager-restored", version: connector.CodexAppServerVersion, handlerRegistered: true,
	}
	ownedBeforeApp := false
	apps := &recordingAppFactory{
		apps: []*recordingAppServer{app},
		inspect: func(handler connector.DynamicToolHandler) error {
			delegate, ok := handler.(*connector.Delegator)
			if !ok {
				return errors.New("unexpected dynamic handler")
			}
			for _, childID := range []string{"child-restored", "child-other"} {
				mode, err := delegate.ChildMode(childID)
				if err != nil || mode != connector.ModeAnswer {
					return errors.New("owned child missing before App Server construction")
				}
			}
			newCalls, resumeReferences := children.CallSnapshot()
			if newCalls != 0 || len(resumeReferences) != 0 {
				return errors.New("child provider I/O occurred during ownership restore")
			}
			ownedBeforeApp = true
			return nil
		},
	}
	manager := connector.CodexManager{Apps: apps}
	request := managerResumeRequest("manager-resume", managerReference, registry, events)
	request.OwnedChildren = []connector.OwnedChild{
		{SessionID: "child-restored", ProviderID: "child", NativeReference: childReference},
		{SessionID: "child-other", ProviderID: "child", NativeReference: otherReference},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resumed, err := manager.Resume(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Connection.Close() })
	if !ownedBeforeApp {
		t.Fatal("App Server was constructed before direct-child ownership restoration")
	}
	app.mu.Lock()
	starts := append([]connector.ThreadStartParams(nil), app.starts...)
	resumes := append([]connector.ThreadResumeParams(nil), app.resumes...)
	app.mu.Unlock()
	if len(starts) != 0 || len(resumes) != 1 || resumes[0].ThreadID != "manager-restored" {
		t.Fatalf("provider calls starts=%d resumes=%+v", len(starts), resumes)
	}
	encodedResume, _ := json.Marshal(resumes[0])
	if !bytes.Equal(encodedResume, json.RawMessage(`{"threadId":"manager-restored"}`)) ||
		bytes.Contains(encodedResume, childReference.Bytes()) || bytes.Contains(encodedResume, []byte(promptCanary)) {
		t.Fatalf("manager Resume request crossed ownership/content boundary: %s", encodedResume)
	}

	handlers := apps.snapshotHandlers()
	if len(handlers) != 1 {
		t.Fatalf("handlers=%d", len(handlers))
	}
	if _, err := handlers[0].Invoke(ctx, json.RawMessage(`{"operation":"follow_next","child_session_id":"child-restored","turn_token":"lost-follow-token"}`)); !errors.Is(err, connector.ErrStaleTurn) {
		t.Fatalf("lost Follow correlation error = %v", err)
	} else if strings.Contains(err.Error(), promptCanary) || strings.Contains(err.Error(), "CHILD-REFERENCE-CANARY") {
		t.Fatalf("stale error leaked child content: %v", err)
	}
	delegate := handlers[0].(*connector.Delegator)
	if mode, err := delegate.ChildMode("child-restored"); err != nil || mode != connector.ModeAnswer {
		t.Fatalf("restored mode=%q error=%v", mode, err)
	}

	startInput, _ := json.Marshal(map[string]any{
		"operation": "start", "child_session_id": "child-restored", "prompt": promptCanary, "mode": "answer",
	})
	startResult, err := handlers[0].Invoke(ctx, startInput)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(startResult, []byte(promptCanary)) || bytes.Contains(startResult, childReference.Bytes()) ||
		bytes.Contains(startResult, []byte("CHILD-REFERENCE-CANARY")) {
		t.Fatalf("start result leaked child prompt/reference: %s", startResult)
	}
	select {
	case got := <-promptSeen:
		if got != promptCanary {
			t.Fatal("child prompt changed")
		}
	case <-ctx.Done():
		t.Fatal("exact child Resume never received prompt")
	}
	answerResult, err := handlers[0].Invoke(ctx, json.RawMessage(`{"operation":"next_answer"}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(answerResult, []byte(promptCanary)) || bytes.Contains(answerResult, childReference.Bytes()) ||
		bytes.Contains(answerResult, []byte("CHILD-REFERENCE-CANARY")) {
		t.Fatalf("answer result leaked child prompt/reference: %s", answerResult)
	}
	newCalls, resumeReferences := children.CallSnapshot()
	if newCalls != 0 || len(resumeReferences) != 1 || !bytes.Equal(resumeReferences[0].Bytes(), childReference.Bytes()) {
		t.Fatalf("child calls new=%d resumes=%d exact=%t", newCalls, len(resumeReferences), len(resumeReferences) == 1 && bytes.Equal(resumeReferences[0].Bytes(), childReference.Bytes()))
	}
	if mode, err := delegate.ChildMode("child-restored"); err != nil || mode != connector.ModeAnswer {
		t.Fatalf("post-terminal mode=%q error=%v", mode, err)
	}
	gotEvents := events.Snapshot()
	if len(gotEvents) != 2 {
		t.Fatalf("restored lifecycle events=%d", len(gotEvents))
	}
	for _, event := range gotEvents {
		wire, _ := json.Marshal(event)
		if len(event.NativeReference) != 0 || bytes.Contains(wire, []byte(promptCanary)) ||
			bytes.Contains(wire, []byte("CHILD-REFERENCE-CANARY")) || bytes.Contains(wire, []byte(`"native_reference"`)) {
			t.Fatalf("restored ChildEvent leaked child prompt/reference: %s", wire)
		}
	}
}

func TestCodexManagerResumeRejectsInvalidDuplicateOrFailedChildRestoreBeforeProvider(t *testing.T) {
	t.Parallel()
	reference := func(id string) replay.OpaqueRef {
		value, err := replay.NewCodexReference(id)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	overCapacity := make([]connector.OwnedChild, connector.MaxChildrenPerManager+1)
	for index := range overCapacity {
		overCapacity[index] = connector.OwnedChild{
			SessionID: fmt.Sprintf("child-%02d", index), ProviderID: "child",
			NativeReference: reference(fmt.Sprintf("native-%02d", index)),
		}
	}
	tests := []struct {
		name     string
		children []connector.OwnedChild
		want     error
	}{
		{
			name: "invalid", want: connector.ErrInvalidOperation,
			children: []connector.OwnedChild{{SessionID: "", ProviderID: "child", NativeReference: reference("invalid")}},
		},
		{
			name: "duplicate session", want: connector.ErrOwnership,
			children: []connector.OwnedChild{
				{SessionID: "duplicate", ProviderID: "child", NativeReference: reference("first")},
				{SessionID: "duplicate", ProviderID: "child", NativeReference: reference("second")},
			},
		},
		{
			name: "duplicate reference", want: connector.ErrOwnership,
			children: []connector.OwnedChild{
				{SessionID: "first", ProviderID: "child", NativeReference: reference("duplicate-native")},
				{SessionID: "second", ProviderID: "child", NativeReference: reference("duplicate-native")},
			},
		},
		{name: "capacity", children: overCapacity, want: connector.ErrCapacity},
		{
			name: "partial restore failure", want: connector.ErrProviderUnknown,
			children: []connector.OwnedChild{
				{SessionID: "valid-first", ProviderID: "child", NativeReference: reference("valid-first")},
				{SessionID: "unknown-second", ProviderID: "PRIVATE-PROVIDER-CANARY", NativeReference: reference("PRIVATE-REFERENCE-CANARY")},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			children := testkit.NewScriptedFactory()
			registry := managerResumableChildRegistry(t, children)
			app := &recordingAppServer{
				reference: "manager-failure", version: connector.CodexAppServerVersion, handlerRegistered: true,
			}
			apps := &recordingAppFactory{apps: []*recordingAppServer{app}}
			managerReference, _ := replay.NewCodexReference("manager-failure")
			request := managerResumeRequest("manager", managerReference, registry, &testkit.EventRecorder{})
			request.OwnedChildren = test.children
			_, err := (connector.CodexManager{Apps: apps}).Resume(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "REFERENCE-CANARY") {
				t.Fatalf("restore error leaked child metadata: %v", err)
			}
			if handlers := apps.snapshotHandlers(); len(handlers) != 0 {
				t.Fatalf("invalid restore constructed %d provider handlers", len(handlers))
			}
			app.mu.Lock()
			startCalls, resumeCalls, closed := len(app.starts), len(app.resumes), app.closed
			app.mu.Unlock()
			if startCalls != 0 || resumeCalls != 0 || closed {
				t.Fatalf("invalid restore provider state starts=%d resumes=%d closed=%t", startCalls, resumeCalls, closed)
			}
			newCalls, resumeReferences := children.CallSnapshot()
			if newCalls != 0 || len(resumeReferences) != 0 {
				t.Fatalf("restore performed child provider I/O: new=%d resume=%d", newCalls, len(resumeReferences))
			}
		})
	}
}

func TestCodexManagerResumeAcceptsExactOwnedChildBoundWithoutChildIO(t *testing.T) {
	t.Parallel()
	children := testkit.NewScriptedFactory()
	registry := managerResumableChildRegistry(t, children)
	owned := make([]connector.OwnedChild, connector.MaxChildrenPerManager)
	for index := range owned {
		reference, err := replay.NewCodexReference(fmt.Sprintf("bounded-native-%02d", index))
		if err != nil {
			t.Fatal(err)
		}
		owned[index] = connector.OwnedChild{
			SessionID: fmt.Sprintf("bounded-child-%02d", index), ProviderID: "child", NativeReference: reference,
		}
	}
	app := &recordingAppServer{
		reference: "manager-bounded", version: connector.CodexAppServerVersion, handlerRegistered: true,
	}
	apps := &recordingAppFactory{apps: []*recordingAppServer{app}}
	managerReference, _ := replay.NewCodexReference("manager-bounded")
	request := managerResumeRequest("manager", managerReference, registry, &testkit.EventRecorder{})
	request.OwnedChildren = owned
	resumed, err := (connector.CodexManager{Apps: apps}).Resume(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Connection.Close() })
	handlers := apps.snapshotHandlers()
	if len(handlers) != 1 {
		t.Fatalf("handlers=%d", len(handlers))
	}
	delegate := handlers[0].(*connector.Delegator)
	for _, child := range owned {
		if mode, err := delegate.ChildMode(child.SessionID); err != nil || mode != connector.ModeAnswer {
			t.Fatalf("child %q mode=%q error=%v", child.SessionID, mode, err)
		}
	}
	newCalls, resumeReferences := children.CallSnapshot()
	if newCalls != 0 || len(resumeReferences) != 0 {
		t.Fatalf("bounded restore performed child I/O: new=%d resume=%d", newCalls, len(resumeReferences))
	}
}

func TestCodexManagerRequiresPinnedVersionAndRegisteredHandlerWithoutCapabilityProbe(t *testing.T) {
	t.Parallel()
	app := &recordingAppServer{reference: "owned-reference", version: connector.CodexAppServerVersion}
	manager := connector.CodexManager{Apps: &recordingAppFactory{apps: []*recordingAppServer{app}}}

	ownedReference, _ := replay.NewCodexReference(app.reference)
	_, err := manager.Resume(context.Background(), managerResumeRequest("manager", ownedReference, connector.NewRegistry(), &testkit.EventRecorder{}))
	if !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("Resume unavailable capability error = %v", err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if len(app.starts) != 0 || len(app.resumes) != 0 {
		t.Fatalf("Resume restoration calls: starts=%d resumes=%d", len(app.starts), len(app.resumes))
	}
	if strings.Contains(err.Error(), app.reference) {
		t.Fatalf("Resume capability error leaked native reference: %v", err)
	}
}

func TestCodexManagerRejectsUnpinnedAppServerBeforeStartOrResume(t *testing.T) {
	t.Parallel()
	startApp := &recordingAppServer{reference: "owned-reference", version: "0.151.0", handlerRegistered: true}
	resumeApp := &recordingAppServer{reference: "owned-reference", version: "0.151.0", handlerRegistered: true}
	manager := connector.CodexManager{Apps: &recordingAppFactory{apps: []*recordingAppServer{startApp, resumeApp}}}
	if _, err := manager.Start(context.Background(), managerStartRequest("manager-start", t.TempDir(), connector.NewRegistry(), &testkit.EventRecorder{})); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("unpinned Start error = %v", err)
	}
	reference, _ := replay.NewCodexReference(startApp.reference)
	if _, err := manager.Resume(context.Background(), managerResumeRequest("manager-resume", reference, connector.NewRegistry(), &testkit.EventRecorder{})); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("unpinned Resume error = %v", err)
	}
	startApp.mu.Lock()
	startCalls := len(startApp.starts)
	startApp.mu.Unlock()
	resumeApp.mu.Lock()
	resumeCalls := len(resumeApp.resumes)
	resumeApp.mu.Unlock()
	if startCalls != 0 || resumeCalls != 0 {
		t.Fatalf("unpinned boundary calls: starts=%d resumes=%d", startCalls, resumeCalls)
	}
}

func TestCodexManagerFactoryIsolatesAppAndDelegatorPerManager(t *testing.T) {
	t.Parallel()
	appA := &recordingAppServer{reference: "manager-a", version: connector.CodexAppServerVersion, handlerRegistered: true}
	appB := &recordingAppServer{reference: "manager-b", version: connector.CodexAppServerVersion, handlerRegistered: true}
	apps := &recordingAppFactory{apps: []*recordingAppServer{appA, appB}}
	manager := connector.CodexManager{Apps: apps}

	registryA := managerChildRegistry(t, testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "child-a"}))
	registryB := managerChildRegistry(t, testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "child-b"}))
	eventsA := &testkit.EventRecorder{}
	eventsB := &testkit.EventRecorder{}
	sessionA, err := manager.Start(context.Background(), managerStartRequest("parent-a", t.TempDir(), registryA, eventsA))
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := manager.Start(context.Background(), managerStartRequest("parent-b", t.TempDir(), registryB, eventsB))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sessionA.Connection.Close()
		_ = sessionB.Connection.Close()
	})

	handlers := apps.snapshotHandlers()
	if len(handlers) != 2 || handlers[0] == handlers[1] {
		t.Fatalf("per-Manager handlers = %#v, want two isolated Delegators", handlers)
	}
	for index, handler := range handlers {
		if _, err := handler.Invoke(context.Background(), json.RawMessage(`{"operation":"start","provider":"child","prompt":"opaque","mode":"answer"}`)); err != nil {
			t.Fatalf("handler %d start: %v", index, err)
		}
		if _, err := handler.Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); err != nil {
			t.Fatalf("handler %d next_answer: %v", index, err)
		}
	}
	for _, event := range eventsA.Snapshot() {
		if event.ParentSessionID != "parent-a" {
			t.Fatalf("Manager A event crossed ownership: %+v", event)
		}
	}
	for _, event := range eventsB.Snapshot() {
		if event.ParentSessionID != "parent-b" {
			t.Fatalf("Manager B event crossed ownership: %+v", event)
		}
	}
	if len(eventsA.Snapshot()) != 2 || len(eventsB.Snapshot()) != 2 {
		t.Fatalf("isolated lifecycle counts A=%d B=%d", len(eventsA.Snapshot()), len(eventsB.Snapshot()))
	}

	if err := sessionA.Connection.Close(); err != nil {
		t.Fatal(err)
	}
	appA.mu.Lock()
	closedA := appA.closed
	appA.mu.Unlock()
	appB.mu.Lock()
	closedB := appB.closed
	appB.mu.Unlock()
	if !closedA || closedB {
		t.Fatalf("isolated close state A=%t B=%t", closedA, closedB)
	}
}

func TestTerminalChildEventFailureInterruptsExactManagerComposite(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	children := testkit.NewScriptedFactory(testkit.ChildScript{NativeReference: "child", Release: release})
	registry := managerChildRegistry(t, children)
	events := &terminalFailureSink{failure: errors.New("PRIVATE-EVENT-SINK-CANARY")}
	app := &recordingAppServer{reference: "manager", version: connector.CodexAppServerVersion, handlerRegistered: true}
	apps := &recordingAppFactory{apps: []*recordingAppServer{app}}
	manager := connector.CodexManager{Apps: apps}
	session, err := manager.Start(context.Background(), managerStartRequest("parent", t.TempDir(), registry, events))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Connection.Close() })
	handlers := apps.snapshotHandlers()
	if len(handlers) != 1 {
		t.Fatalf("handlers=%d", len(handlers))
	}
	if _, err := handlers[0].Invoke(context.Background(), json.RawMessage(`{"operation":"start","provider":"child","prompt":"opaque","mode":"answer"}`)); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-session.Connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("terminal lifecycle failure did not interrupt Manager composite")
	}
	if err := session.Connection.Err(); !errors.Is(err, connector.ErrTransport) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("Manager fatal error = %v", err)
	}
	if _, err := handlers[0].Invoke(context.Background(), json.RawMessage(`{"operation":"next_answer"}`)); !errors.Is(err, connector.ErrTransport) {
		t.Fatalf("post-fatal next_answer error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		app.mu.Lock()
		closed := app.closed
		app.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fatal Manager composite did not close its exact App Server")
		}
		time.Sleep(time.Millisecond)
	}
	if got := events.snapshot(); len(got) != 2 || got[0].Lifecycle != "prompt_pending" || got[1].Lifecycle != "terminal" {
		t.Fatalf("lifecycle attempts = %+v", got)
	}
}

func TestManagerSupervisorContextOwnsExactCompositeLifetime(t *testing.T) {
	t.Parallel()
	app := &recordingAppServer{reference: "manager", version: connector.CodexAppServerVersion, handlerRegistered: true}
	manager := connector.CodexManager{Apps: &recordingAppFactory{apps: []*recordingAppServer{app}}}
	lifetime, cancel := context.WithCancel(context.Background())
	session, err := manager.Start(lifetime, managerStartRequest("parent", t.TempDir(), connector.NewRegistry(), &testkit.EventRecorder{}))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-session.Connection.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor cancellation did not stop exact Manager composite")
	}
	if err := session.Connection.Err(); !errors.Is(err, connector.ErrTransport) {
		t.Fatalf("supervisor cancellation error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		app.mu.Lock()
		closed := app.closed
		app.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor cancellation left App Server alive")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDelegateCapabilityHasOneStrictInputSchemaAndNoOutputSchema(t *testing.T) {
	t.Parallel()
	tool := connector.DelegateCapability()
	if tool.Type != "function" || tool.Name != "taskai_delegate" || !json.Valid(tool.InputSchema) {
		t.Fatalf("tool = %#v", tool)
	}
	var schema struct {
		OneOf []struct {
			AdditionalProperties bool                       `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if len(schema.OneOf) != 6 {
		t.Fatalf("schema branches = %d, want 6", len(schema.OneOf))
	}
	for index, branch := range schema.OneOf {
		if branch.AdditionalProperties {
			t.Errorf("branch %d accepts additional properties", index)
		}
	}
	type toolWire struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	}
	wire, _ := json.Marshal(toolWire{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema})
	if strings.Contains(string(wire), "outputSchema") {
		t.Fatalf("unsupported output schema advertised: %s", wire)
	}
}

func managerChildRegistry(t *testing.T, children connector.ChildFactory) *connector.Registry {
	t.Helper()
	versions := connector.Versions{ACPProtocol: 1, Connector: "manager-factory-test"}
	profile := connector.NewConformedProfile(
		"child", "Child", versions,
		connector.Conformance{Exact: versions, Proven: connector.Capabilities{Subordinate: true}},
		nil, children, nil, nil, nil, connector.ReplayBinding{},
	)
	registry := connector.NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	return registry
}

func managerResumableChildRegistry(t *testing.T, children *testkit.ScriptedFactory) *connector.Registry {
	t.Helper()
	versions := connector.Versions{ACPProtocol: 1, Connector: "manager-resume-child-test"}
	profile := connector.NewConformedProfile(
		"child", "Child", versions,
		connector.Conformance{
			Exact: versions, Proven: connector.Capabilities{Subordinate: true, Resume: true},
		},
		nil, children, children, nil, nil, connector.ReplayBinding{},
	)
	registry := connector.NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	return registry
}

type terminalFailureSink struct {
	mu      sync.Mutex
	events  []connector.ChildEvent
	failure error
}

func (s *terminalFailureSink) ChildEvent(_ context.Context, event connector.ChildEvent) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	if event.Lifecycle == "terminal" {
		return s.failure
	}
	return nil
}

func (s *terminalFailureSink) snapshot() []connector.ChildEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connector.ChildEvent(nil), s.events...)
}

type fingerprint struct {
	Hash [32]byte
	Size int64
	Mode os.FileMode
}

func fileFingerprint(t *testing.T, path string) fingerprint {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint{Hash: sha256.Sum256(contents), Size: info.Size(), Mode: info.Mode()}
}

func managerStartRequest(parent, cwd string, registry *connector.Registry, events connector.ChildEventSink) connector.ManagerStartRequest {
	return connector.ManagerStartRequest{
		ParentSessionID: parent, CWD: cwd, Registry: registry, Events: events, IDs: &testkit.SequenceIDs{},
	}
}

func managerResumeRequest(parent string, reference replay.OpaqueRef, registry *connector.Registry, events connector.ChildEventSink) connector.ManagerResumeRequest {
	return connector.ManagerResumeRequest{
		ParentSessionID: parent, NativeReference: reference, Registry: registry, Events: events, IDs: &testkit.SequenceIDs{},
	}
}

type recordingAppServer struct {
	mu                sync.Mutex
	reference         string
	version           string
	handlerRegistered bool
	starts            []connector.ThreadStartParams
	resumes           []connector.ThreadResumeParams
	composed          []string
	connections       []*recordingManagerConnection
	closed            bool
}

func (a *recordingAppServer) Version() string { return a.version }

func (a *recordingAppServer) DelegateHandlerRegistered() bool { return a.handlerRegistered }

func (a *recordingAppServer) ThreadStart(_ context.Context, params connector.ThreadStartParams) (connector.CodexThreadSession, error) {
	native, _ := os.ReadFile(filepath.Join(params.CWD, "AGENTS.md"))
	a.mu.Lock()
	a.starts = append(a.starts, params)
	a.composed = append(a.composed, params.DeveloperInstructions+"\n"+string(native))
	a.mu.Unlock()
	return connector.CodexThreadSession{ThreadID: a.reference}, nil
}

func (a *recordingAppServer) ThreadResume(_ context.Context, params connector.ThreadResumeParams) (connector.CodexThreadSession, error) {
	a.mu.Lock()
	a.resumes = append(a.resumes, params)
	a.mu.Unlock()
	return connector.CodexThreadSession{ThreadID: a.reference}, nil
}

func (a *recordingAppServer) ConnectManager(reference replay.OpaqueRef) (connector.ManagerConnection, error) {
	want, err := replay.NewCodexReference(a.reference)
	if err != nil || !bytes.Equal(reference.Bytes(), want.Bytes()) {
		return nil, connector.ErrOwnership
	}
	connection := &recordingManagerConnection{
		reference: replay.OpaqueRefFromBytes(reference.Bytes()), publicSessionID: "manager_public_fixture",
		done: make(chan struct{}),
	}
	a.mu.Lock()
	a.connections = append(a.connections, connection)
	a.mu.Unlock()
	return connection, nil
}

func (a *recordingAppServer) Close() error {
	a.mu.Lock()
	a.closed = true
	connections := append([]*recordingManagerConnection(nil), a.connections...)
	a.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
	return nil
}

type recordingAppFactory struct {
	mu       sync.Mutex
	apps     []*recordingAppServer
	handlers []connector.DynamicToolHandler
	inspect  func(connector.DynamicToolHandler) error
}

func (f *recordingAppFactory) New(_ context.Context, handler connector.DynamicToolHandler) (connector.CodexAppServer, error) {
	f.mu.Lock()
	if len(f.apps) == 0 {
		f.mu.Unlock()
		return nil, errors.New("no disposable App Server")
	}
	app := f.apps[0]
	f.apps = f.apps[1:]
	f.handlers = append(f.handlers, handler)
	inspect := f.inspect
	f.mu.Unlock()
	if inspect != nil {
		if err := inspect(handler); err != nil {
			return nil, err
		}
	}
	return app, nil
}

func (f *recordingAppFactory) snapshotHandlers() []connector.DynamicToolHandler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connector.DynamicToolHandler(nil), f.handlers...)
}

type recordingManagerConnection struct {
	reference       replay.OpaqueRef
	publicSessionID string
	done            chan struct{}
	closeOnce       sync.Once
}

func (c *recordingManagerConnection) NativeReference() replay.OpaqueRef {
	return replay.OpaqueRefFromBytes(c.reference.Bytes())
}

func (c *recordingManagerConnection) PublicSessionID() string { return c.publicSessionID }

func (*recordingManagerConnection) Prompt(context.Context, string, func(json.RawMessage) error) (json.RawMessage, error) {
	return nil, errors.New("unused manager prompt")
}

func (*recordingManagerConnection) Interrupt(context.Context) error {
	return errors.New("unused manager interrupt")
}

func (c *recordingManagerConnection) Done() <-chan struct{} { return c.done }
func (*recordingManagerConnection) Err() error              { return nil }
func (c *recordingManagerConnection) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}
