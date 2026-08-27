//go:build acceptance && (darwin || linux)

package ptyacceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

const approvedManagerPrefix = `You are a TaskAI Manager Session. Delegate separable work only through the single taskai_delegate capability. Every downstream interaction uses ACP. Start each subordinate invocation in Answer unless live detail is genuinely needed. Follow applies to one invocation and resets at every terminal condition. Request Replay only after completion and only when the target profile advertises it through this same capability. When a capability result says more=true, invoke its exact next_operation with its exact next_arguments until more=false. Synthesize subordinate results for the user; never ask TaskAI core to interpret prompts, frames, tools, or answers.`

func TestCompiledBinaryAcceptance(t *testing.T) {
	taskaiBinary := os.Getenv("TASKAI_ACCEPTANCE_BINARY")
	layout := prepareLayout(t, taskaiBinary)
	trace := traceLog{path: filepath.Join(layout.providerRoot, spec.TraceFilename)}

	agentsPath := filepath.Join(layout.projectRoot, "AGENTS.md")
	claudePath := filepath.Join(layout.projectRoot, "CLAUDE.md")
	agentsContent := "# Synthetic project instructions\nPreserve this file exactly.\n" + spec.ProjectInstructionCanary + "\n"
	claudeContent := "# Synthetic Claude project instructions\nNo provider may mutate this file.\n" + spec.ProjectInstructionCanary + "\n"
	if err := os.WriteFile(agentsPath, []byte(agentsContent), 0o600); err != nil {
		t.Fatalf("write synthetic AGENTS.md: %v", err)
	}
	if err := os.WriteFile(claudePath, []byte(claudeContent), 0o600); err != nil {
		t.Fatalf("write synthetic CLAUDE.md: %v", err)
	}
	agentsHash, claudeHash := fileHash(t, agentsPath), fileHash(t, claudePath)

	assertInformationalCommandsAreSideEffectFree(t, taskaiBinary, layout, trace)

	workerOne := startWorker(t, taskaiBinary, layout.projectRoot, layout.stateRoot, layout.environment)
	workerOnePID := workerOne.pid()
	assertWorkerEndpointModes(t, layout.stateRoot)

	first := startViewer(t, taskaiBinary, layout.projectRoot, layout.environment, terminalSize{columns: 80, rows: 24})
	firstViewerPID := first.command.Process.Pid
	first.waitContains(t, "TaskAI / Tasks")
	first.waitContains(t, "No Tasks yet")
	if events := trace.snapshot(t); len(events) != 0 {
		t.Fatalf("Tasks-first launched or probed a provider:\n%s", traceSummary(events))
	}

	first.send(t, "s")
	first.waitContains(t, "TaskAI / Provider Settings")
	first.waitContains(t, "Claude Code")
	first.waitContains(t, "Codex")
	first.waitContains(t, "disabled | status:Not connected")
	if events := trace.snapshot(t); len(events) != 0 {
		t.Fatalf("opening Settings contacted a provider:\n%s", traceSummary(events))
	}
	first.send(t, "j")
	first.waitContains(t, "> Codex")
	first.send(t, " ")
	first.waitContains(t, "enabled | status:Not connected")
	if events := trace.snapshot(t); len(events) != 0 {
		t.Fatalf("enabling Codex contacted a provider:\n%s", traceSummary(events))
	}

	first.send(t, "a")
	events := trace.wait(t, "one explicit Codex authentication and its recheck", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventAuthLogin) == 1 && connectedStatusCount(events) == 1
	})
	first.waitContains(t, "status:Connected")
	assertAuthOwnership(t, events, firstViewerPID)
	first.send(t, "r")
	events = trace.wait(t, "one explicit Settings recheck", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventAuthLogin) == 1 && connectedStatusCount(events) == 2
	})
	first.waitContains(t, "status:Connected")

	const taskTitle = "Acceptance Core Replay Task"
	const managerLabel = "Core Replay Manager"
	first.send(t, "b")
	first.waitContains(t, "TaskAI / Tasks")
	first.send(t, "a")
	first.waitContains(t, "TaskAI / Tasks / Add Task")
	first.send(t, taskTitle)
	first.send(t, "\r")
	first.waitContains(t, taskTitle)
	first.send(t, "\r")
	first.waitContains(t, "TaskAI / Task / "+taskTitle)
	first.send(t, "a")
	first.waitContains(t, "TaskAI / Task / Add Manager")
	first.waitContains(t, "Codex")
	first.send(t, "\t")
	first.send(t, managerLabel)
	first.send(t, "\r")
	events = trace.wait(t, "one manager native Start", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventThreadStart) == 1
	})
	first.waitContains(t, "Working")
	assertManagerStart(t, events, layout.projectRoot, workerOnePID)

	firstPrompt := spec.PromptCanary + " :: " + spec.UserContextCanary + " []{} opaque/unchanged"
	first.send(t, "\t")
	first.send(t, firstPrompt)
	first.send(t, "\r")
	events = trace.wait(t, "blocked subordinate started through the inbound taskai_delegate path", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventSubordinateWaiting) == 1 &&
			countOperation(events, "start") == 1
	})
	first.waitContains(t, "Subordinate Answer Working")
	assertOpaqueTurnPrompt(t, eventsNamed(events, spec.EventTurnStart), firstPrompt, 0)
	assertSubordinatePrompt(t, events, spec.SubordinatePromptCanary)

	first.send(t, "\t")
	first.send(t, "b")
	first.waitContains(t, "TaskAI / Tasks")
	first.send(t, "q")
	first.waitExit(t)
	if !processAlive(workerOnePID) {
		t.Fatal("viewer quit stopped the persistent test-owned worker")
	}
	events = trace.snapshot(t)
	if countEvents(events, spec.EventThreadStart) != 1 || countEvents(events, spec.EventThreadResume) != 0 ||
		countEvents(events, spec.EventAuthLogin) != 1 {
		t.Fatalf("detach changed manager/auth lifecycle:\n%s", traceSummary(events))
	}

	second := startViewer(t, taskaiBinary, layout.projectRoot, layout.environment, terminalSize{columns: 60, rows: 18})
	second.waitContains(t, "TaskAI / Tasks")
	second.waitContains(t, taskTitle)
	events = trace.snapshot(t)
	if countEvents(events, spec.EventThreadStart) != 1 || countEvents(events, spec.EventAuthLogin) != 1 {
		t.Fatalf("viewer relaunch duplicated Start/auth:\n%s", traceSummary(events))
	}
	second.send(t, "\r")
	second.waitContains(t, "TaskAI / Task / "+taskTitle)
	second.waitContains(t, "Working")
	if countEvents(trace.snapshot(t), spec.EventThreadStart) != 1 {
		t.Fatal("reattaching to an active Task duplicated native Start")
	}

	releaseSubordinate(t, layout.releaseFIFO)
	events = trace.wait(t, "Answer completion followed by explicit Replay 1 and public manager completion", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventSubordinateAnswer) == 1 &&
			countOperation(events, "next_answer") == 1 && countOperation(events, "replay") == 1 &&
			countEvents(events, spec.EventThreadRead) == 1 && countEvents(events, spec.EventManagerOutput) == 1
	})
	second.waitContains(t, spec.ManagerOutputCanary)
	second.waitContains(t, "Answered Codex")
	assertFirstDelegation(t, events)
	second.send(t, "b")
	second.waitContains(t, "TaskAI / Tasks")
	second.send(t, "q")
	second.waitExit(t)
	if !processAlive(workerOnePID) {
		t.Fatal("second viewer quit stopped the persistent worker")
	}

	workerOne.stop(t)
	assertStoppedEndpointRemoved(t, layout.stateRoot)
	events = trace.snapshot(t)
	assertProviderProcessesStop(t, layout.providerRoot, events)

	workerTwo := startWorker(t, taskaiBinary, layout.projectRoot, layout.stateRoot, layout.environment)
	workerTwoPID := workerTwo.pid()
	if workerTwoPID == workerOnePID {
		t.Fatal("worker restart did not establish a distinct exact process identity")
	}
	assertWorkerEndpointModes(t, layout.stateRoot)
	events = trace.snapshot(t)
	if countEvents(events, spec.EventThreadStart) != 1 || countEvents(events, spec.EventThreadResume) != 0 ||
		countEvents(events, spec.EventAuthLogin) != 1 {
		t.Fatalf("worker bootstrap contacted provider or fell back to Start:\n%s", traceSummary(events))
	}

	third := startViewer(t, taskaiBinary, layout.projectRoot, layout.environment, terminalSize{columns: 40, rows: 16})
	third.waitContains(t, "TaskAI / Tasks")
	third.waitContains(t, "No active Tasks")
	third.send(t, "\t")
	third.waitContains(t, "[All]")
	third.waitContains(t, "Acceptance Core Replay")
	third.send(t, "\r")
	third.waitContains(t, "TaskAI / Task / Acceptance Core Replay")
	third.waitContains(t, "Exact Session Resume")
	third.waitContains(t, "Interrupted")
	third.send(t, "r")
	events = trace.wait(t, "exact native Manager Resume", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventThreadResume) == 1
	})
	third.waitContains(t, "Working")
	assertExactManagerResume(t, events, workerTwoPID)

	third.send(t, "\t")
	third.send(t, spec.ResumePromptCanary)
	third.send(t, "\r")
	events = trace.wait(t, "post-Resume retained delegation and Replay capability", func(events []spec.TraceEvent) bool {
		return countEvents(events, spec.EventSubordinateAnswer) == 2 &&
			countOperation(events, "start") == 2 && countOperation(events, "next_answer") == 2 &&
			countOperation(events, "replay") == 2 && countEvents(events, spec.EventThreadRead) == 2 &&
			countEvents(events, spec.EventManagerOutput) == 2
	})
	third.waitContains(t, "TASKAI_ACCEPTANCE_MANAGER_RESUME")
	third.waitContains(t, "Answered Codex")
	assertOpaqueTurnPrompt(t, eventsNamed(events, spec.EventTurnStart), spec.ResumePromptCanary, 1)
	assertAllDelegations(t, events, layout.projectRoot, workerTwoPID)
	third.send(t, "\t")
	third.send(t, "b")
	third.waitContains(t, "TaskAI / Tasks")
	third.send(t, "q")
	third.waitExit(t)

	workerTwo.stop(t)
	assertStoppedEndpointRemoved(t, layout.stateRoot)
	events = trace.snapshot(t)
	assertProviderProcessesStop(t, layout.providerRoot, events)
	assertLiteralProviderCommands(t, events)

	if got := fileHash(t, agentsPath); got != agentsHash {
		t.Fatalf("AGENTS.md changed: got %x want %x", got, agentsHash)
	}
	if got := fileHash(t, claudePath); got != claudeHash {
		t.Fatalf("CLAUDE.md changed: got %x want %x", got, claudeHash)
	}
	for _, root := range []string{
		layout.stateRoot, layout.providerRoot, layout.projectRoot,
		layout.homeRoot, layout.controlRoot, layout.fixtureBin,
	} {
		assertRestrictiveTree(t, root)
	}
	forbidden := append(spec.PersistenceCanaries(), layout.providerRoot)
	assertTreeExcludes(t, layout.stateRoot, forbidden)
	assertTreeExcludes(t, layout.homeRoot, forbidden)
	assertTreeExcludes(t, layout.controlRoot, forbidden)
	projectForbidden := make([]string, 0, len(forbidden)-1)
	for _, value := range forbidden {
		if value != spec.ProjectInstructionCanary {
			projectForbidden = append(projectForbidden, value)
		}
	}
	assertTreeExcludes(t, layout.projectRoot, projectForbidden)
	assertTreeContains(t, layout.providerRoot, spec.PersistenceCanaries())
}

func assertInformationalCommandsAreSideEffectFree(
	t *testing.T,
	binary string,
	layout acceptanceLayout,
	trace traceLog,
) {
	t.Helper()
	exit, stdout, stderr := runCommand(t, binary, layout.projectRoot, layout.environment, []string{"--help"})
	if exit != 0 || !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "taskai --version") || stderr != "" {
		t.Fatalf("taskai --help = exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	exit, stdout, stderr = runCommand(t, binary, layout.projectRoot, layout.environment, []string{"--version"})
	if exit != 0 || !strings.HasPrefix(stdout, "taskai ") || stderr != "" {
		t.Fatalf("taskai --version = exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	exit, stdout, stderr = runCommand(t, binary, layout.projectRoot, layout.environment, []string{"legacy"})
	if exit != 2 || stdout != "" || stderr != "taskai: invalid arguments; use --help\n" {
		t.Fatalf("retired command surface = exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if _, err := os.Lstat(layout.stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("informational command created TaskAI state: %v", err)
	}
	if events := trace.snapshot(t); len(events) != 0 {
		t.Fatalf("informational command contacted provider:\n%s", traceSummary(events))
	}
}

func connectedStatusCount(events []spec.TraceEvent) int {
	count := 0
	for _, event := range eventsNamed(events, spec.EventAuthStatus) {
		if event.Connected != nil && *event.Connected {
			count++
		}
	}
	return count
}

func countOperation(events []spec.TraceEvent, operation string) int {
	count := 0
	for _, event := range eventsNamed(events, spec.EventToolCall) {
		if event.Operation == operation {
			count++
		}
	}
	return count
}

func assertAuthOwnership(t *testing.T, events []spec.TraceEvent, viewerPID int) {
	t.Helper()
	logins := eventsNamed(events, spec.EventAuthLogin)
	if len(logins) != 1 || logins[0].ParentPID != viewerPID {
		t.Fatalf("authentication was not owned by current viewer pid %d: %#v", viewerPID, logins)
	}
	for _, event := range eventsNamed(events, spec.EventProcessStart) {
		if event.Executable == "codex" && reflect.DeepEqual(event.Argv, []string{"login"}) && event.ParentPID != viewerPID {
			t.Fatalf("Codex login parent pid = %d, want viewer %d", event.ParentPID, viewerPID)
		}
	}
}

func assertManagerStart(t *testing.T, events []spec.TraceEvent, project string, workerPID int) {
	t.Helper()
	starts := eventsNamed(events, spec.EventThreadStart)
	if len(starts) != 1 {
		t.Fatalf("native manager Starts = %d, want 1", len(starts))
	}
	var params struct {
		CWD                   string `json:"cwd"`
		DeveloperInstructions string `json:"developerInstructions"`
		DynamicTools          []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"dynamicTools"`
	}
	if json.Unmarshal(starts[0].Params, &params) != nil {
		t.Fatalf("decode native manager Start: %s", starts[0].Params)
	}
	if params.CWD != project || params.DeveloperInstructions != approvedManagerPrefix ||
		strings.Count(params.DeveloperInstructions, approvedManagerPrefix) != 1 || len(params.DynamicTools) != 1 ||
		params.DynamicTools[0].Type != "function" || params.DynamicTools[0].Name != "taskai_delegate" {
		t.Fatalf("native manager Start contract changed: %#v", params)
	}
	for _, event := range eventsNamed(events, spec.EventProcessStart) {
		if event.Executable == "codex" && reflect.DeepEqual(event.Argv, []string{"app-server"}) &&
			event.PID == starts[0].PID && event.ParentPID != workerPID {
			t.Fatalf("manager provider parent pid = %d, want worker %d", event.ParentPID, workerPID)
		}
	}
}

func assertOpaqueTurnPrompt(t *testing.T, turns []spec.TraceEvent, want string, index int) {
	t.Helper()
	if index < 0 || index >= len(turns) {
		t.Fatalf("turn index %d unavailable in %d turn Starts", index, len(turns))
	}
	var params struct {
		Input []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"input"`
	}
	if json.Unmarshal(turns[index].Params, &params) != nil || len(params.Input) != 1 ||
		params.Input[0].Type != "text" || params.Input[0].Text != want {
		t.Fatalf("opaque prompt changed at turn %d: %#v, want %q", index, params, want)
	}
}

func assertSubordinatePrompt(t *testing.T, events []spec.TraceEvent, want string) {
	t.Helper()
	for _, event := range eventsNamed(events, spec.EventACPRequest) {
		if event.Method != "session/prompt" {
			continue
		}
		var params struct {
			Prompt []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if json.Unmarshal(event.Params, &params) == nil && len(params.Prompt) == 1 &&
			params.Prompt[0].Type == "text" && params.Prompt[0].Text == want {
			return
		}
	}
	t.Fatalf("exact subordinate prompt %q not observed", want)
}

func assertFirstDelegation(t *testing.T, events []spec.TraceEvent) {
	t.Helper()
	if countOperation(events, "start") != 1 || countOperation(events, "next_answer") != 1 ||
		countOperation(events, "replay") != 1 {
		t.Fatalf("first delegation operation counts changed:\n%s", traceSummary(events))
	}
	answers := toolResults(events, "next_answer")
	replays := toolResults(events, "replay")
	if len(answers) != 1 || !bytes.Contains(answers[0].Params, []byte(spec.AnswerCanary)) {
		t.Fatalf("next_answer did not carry the synthetic native Answer: %#v", answers)
	}
	if len(replays) != 1 || !bytes.Contains(replays[0].Params, []byte(spec.ReplayCanary)) {
		t.Fatalf("Replay did not carry the exact provider-native replay item: %#v", replays)
	}
	assertReplayCalls(t, events)
}

func assertAllDelegations(t *testing.T, events []spec.TraceEvent, project string, workerPID int) {
	t.Helper()
	if countOperation(events, "start") != 2 || countOperation(events, "next_answer") != 2 ||
		countOperation(events, "replay") != 2 {
		t.Fatalf("delegation operation counts changed after Resume:\n%s", traceSummary(events))
	}
	if len(toolResults(events, "next_answer")) != 2 || len(toolResults(events, "replay")) != 2 {
		t.Fatalf("delegation results missing after Resume:\n%s", traceSummary(events))
	}
	assertExactSubordinateResume(t, events, project, workerPID)
	assertReplayCalls(t, events)
}

func assertExactSubordinateResume(t *testing.T, events []spec.TraceEvent, project string, workerPID int) {
	t.Helper()
	created := eventsNamed(events, spec.EventSessionNew)
	resumed := eventsNamed(events, spec.EventSessionResume)
	if len(created) != 1 || len(resumed) != 1 || created[0].SessionID == "" ||
		resumed[0].SessionID != created[0].SessionID {
		t.Fatalf("subordinate exact Resume contract changed: new=%#v resume=%#v", created, resumed)
	}
	var fields map[string]json.RawMessage
	var params struct {
		SessionID  string            `json:"sessionId"`
		CWD        string            `json:"cwd"`
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(resumed[0].Params, &fields) != nil || json.Unmarshal(resumed[0].Params, &params) != nil ||
		len(fields) != 3 || params.SessionID != created[0].SessionID || params.CWD != project ||
		params.MCPServers == nil || len(params.MCPServers) != 0 {
		t.Fatalf("subordinate Resume was not exact: %s", resumed[0].Params)
	}
	for _, event := range eventsNamed(events, spec.EventProcessStart) {
		if event.Executable == "codex-acp" && event.PID == resumed[0].PID && event.ParentPID != workerPID {
			t.Fatalf("resumed subordinate parent pid = %d, want restarted worker %d", event.ParentPID, workerPID)
		}
	}
	starts := make([]spec.TraceEvent, 0, 2)
	for _, event := range eventsNamed(events, spec.EventToolCall) {
		if event.Operation == "start" {
			starts = append(starts, event)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("subordinate Start calls = %d, want 2", len(starts))
	}
	var second map[string]json.RawMessage
	if json.Unmarshal(starts[1].Params, &second) != nil || len(second) != 4 ||
		second["child_session_id"] == nil || second["provider"] != nil {
		t.Fatalf("second delegation did not target one already-owned child: %s", starts[1].Params)
	}
}

func toolResults(events []spec.TraceEvent, operation string) []spec.TraceEvent {
	var result []spec.TraceEvent
	for _, event := range eventsNamed(events, spec.EventToolResult) {
		if event.Operation == operation {
			result = append(result, event)
		}
	}
	return result
}

func assertReplayCalls(t *testing.T, events []spec.TraceEvent) {
	t.Helper()
	sessions := make(map[string]bool)
	for _, event := range eventsNamed(events, spec.EventSessionNew) {
		sessions[event.SessionID] = true
	}
	reads := eventsNamed(events, spec.EventThreadRead)
	replayCalls := make([]spec.TraceEvent, 0)
	for _, event := range eventsNamed(events, spec.EventToolCall) {
		if event.Operation == "replay" {
			replayCalls = append(replayCalls, event)
		}
	}
	if len(reads) != len(replayCalls) || len(reads) == 0 {
		t.Fatalf("Replay/read count mismatch: replay=%d reads=%d", len(replayCalls), len(reads))
	}
	for _, read := range reads {
		var fields map[string]json.RawMessage
		var params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		}
		if json.Unmarshal(read.Params, &fields) != nil || json.Unmarshal(read.Params, &params) != nil ||
			len(fields) != 2 || !params.IncludeTurns || !sessions[params.ThreadID] || read.ThreadID != params.ThreadID {
			t.Fatalf("thread/read was not one exact provider-native Session: %s", read.Params)
		}
	}
	for _, call := range replayCalls {
		var params struct {
			Operation       string   `json:"operation"`
			ChildSessionIDs []string `json:"child_session_ids"`
			N               int      `json:"n"`
		}
		if json.Unmarshal(call.Params, &params) != nil || params.Operation != "replay" ||
			len(params.ChildSessionIDs) != 1 || params.N != 1 {
			t.Fatalf("Replay was not explicit bounded N=1: %s", call.Params)
		}
	}
	for _, event := range eventsNamed(events, spec.EventAppRequest) {
		if strings.Contains(strings.ToLower(event.Method), "list") {
			t.Fatalf("provider history enumeration method observed: %s", event.Method)
		}
	}
}

func assertExactManagerResume(t *testing.T, events []spec.TraceEvent, workerPID int) {
	t.Helper()
	starts := eventsNamed(events, spec.EventThreadStart)
	resumes := eventsNamed(events, spec.EventThreadResume)
	if len(starts) != 1 || len(resumes) != 1 || starts[0].ThreadID == "" || resumes[0].ThreadID != starts[0].ThreadID {
		t.Fatalf("exact Manager reference changed across restart: start=%#v resume=%#v", starts, resumes)
	}
	var fields map[string]json.RawMessage
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if json.Unmarshal(resumes[0].Params, &fields) != nil || json.Unmarshal(resumes[0].Params, &params) != nil ||
		len(fields) != 1 || params.ThreadID != starts[0].ThreadID {
		t.Fatalf("Resume params were not only exact threadId: %s", resumes[0].Params)
	}
	if countEvents(events, spec.EventThreadStart) != 1 {
		t.Fatal("Resume fell back to native Start")
	}
	for _, event := range eventsNamed(events, spec.EventProcessStart) {
		if event.Executable == "codex" && reflect.DeepEqual(event.Argv, []string{"app-server"}) &&
			event.PID == resumes[0].PID && event.ParentPID != workerPID {
			t.Fatalf("resumed manager parent pid = %d, want restarted worker %d", event.ParentPID, workerPID)
		}
	}
}

func assertLiteralProviderCommands(t *testing.T, events []spec.TraceEvent) {
	t.Helper()
	for _, event := range eventsNamed(events, spec.EventProcessStart) {
		valid := false
		switch event.Executable {
		case "codex":
			valid = reflect.DeepEqual(event.Argv, []string{"login"}) ||
				reflect.DeepEqual(event.Argv, []string{"login", "status"}) ||
				reflect.DeepEqual(event.Argv, []string{"app-server"})
		case "codex-acp":
			valid = len(event.Argv) == 0
		}
		if !valid {
			t.Fatalf("unexpected provider launch vector: executable=%q argv=%q", event.Executable, event.Argv)
		}
	}
}
