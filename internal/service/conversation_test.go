package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/native"
	"github.com/yty-build/acp-gateway/internal/presentation"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestConversationUsesProviderHistoryAndOnlyUncommittedGatewayOverlay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Logs: filepath.Join(root, "logs"), Workers: filepath.Join(root, "workers"),
	}
	for _, directory := range []string{home.Agents, home.Logs, home.Workers} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-conversation", Title: "Unified history"}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeGitRepository(t, repo)
	conversationID := "01-provider-history"
	verifiedAt := time.Now().UTC()
	providerUserAt := verifiedAt.Format(time.RFC3339Nano)
	providerAnswerAt := verifiedAt.Add(time.Millisecond).Format(time.RFC3339Nano)
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-conversation", TaskID: "T-conversation", Agent: "codex", Provider: "openai", RepoPath: repo,
		Status: session.StatusWorking, LogPath: filepath.Join(home.Logs, "S-conversation.jsonl"),
		ACPSessionID: conversationID, NativeSessionID: conversationID, ProviderConversationID: conversationID,
		ConversationIDSource: session.ConversationSourceProviderStore, ConversationIDVerifiedAt: &verifiedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerRoot := filepath.Join(root, "codex")
	t.Setenv("CODEX_HOME", providerRoot)
	providerPath := filepath.Join(providerRoot, "sessions", "2026", "08", "16", "rollout-"+conversationID+".jsonl")
	writeConversationFixture(t, providerPath,
		`{"timestamp":"2026-08-16T12:00:00Z","type":"session_meta","payload":{"id":"`+conversationID+`","cwd":"`+repo+`"}}`+"\n"+
			`{"timestamp":"`+providerUserAt+`","type":"event_msg","payload":{"type":"user_message","message":"Committed prompt"}}`+"\n"+
			`{"timestamp":"`+providerAnswerAt+`","type":"response_item","payload":{"id":"answer-1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"COMMITTED"}]}}`+"\n")

	log, err := sessionlog.Open(created.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	appendConversationLine(t, log, "REQ-committed", "prompt_submitted", map[string]string{"display_message": "Committed prompt"})
	appendConversationThought(t, log, "REQ-committed", "ACP-only reasoning")
	appendConversationACP(t, log, "REQ-committed", "COMMITTED")
	appendConversationLine(t, log, "REQ-committed", "turn_completed", map[string]string{"stopReason": "end_turn"})
	appendConversationLine(t, log, "REQ-live", "prompt_submitted", map[string]string{"display_message": "Still working"})
	appendConversationLine(t, log, "REQ-live", "turn_started", map[string]string{"request_id": "REQ-live"})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSessionService(repository, registry, home, &recordingRuntime{}).Conversation(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != ConversationSourceProvider || len(snapshot.Provider.Records) != 2 || snapshot.Provider.Checkpoint == "" || len(snapshot.Turns) != 2 {
		t.Fatalf("provider snapshot = %#v", snapshot)
	}
	if snapshot.GatewayCursor != 6 {
		t.Fatalf("gateway cursor = %d, want 6", snapshot.GatewayCursor)
	}
	if !snapshot.Turns[0].Committed || snapshot.Turns[1].Committed || len(snapshot.Turns[1].Activities) != 2 {
		t.Fatalf("merged turns = %#v", snapshot.Turns)
	}
	if !hasConversationDisplay(snapshot.Turns[0].Activities, "thought_chunk", "ACP-only reasoning") {
		t.Fatalf("gateway ACP reasoning was lost when the provider answer committed: %#v", snapshot.Turns[0])
	}
	if !hasConversationDisplayKind(snapshot.Turns[0].Activities, "assistant_chunk", "provider_transcript") ||
		!hasConversationDisplayKind(snapshot.Turns[0].Activities, "thought_chunk", "acp") {
		t.Fatalf("matched answer/provider and supplemental reasoning/ACP provenance was lost: %#v", snapshot.Turns[0])
	}
	for _, activity := range snapshot.Turns[1].Activities {
		if activity.RequestID != "REQ-live" {
			t.Fatalf("uncommitted turn = %#v", snapshot.Turns[1])
		}
	}
}

func TestConversationFallsBackToGatewayReplayWithoutCanonicalProviderHistory(t *testing.T) {
	service, _, current := nativeServiceHarness(t, "codex", "")
	log, err := sessionlog.Open(service.logPath(current))
	if err != nil {
		t.Fatal(err)
	}
	appendConversationLine(t, log, "REQ-fallback", "prompt_submitted", map[string]string{"display_message": "Gateway prompt"})
	appendConversationThought(t, log, "REQ-fallback", "Gateway reasoning")
	appendConversationACP(t, log, "REQ-fallback", "Gateway answer")
	appendConversationLine(t, log, "REQ-fallback", "turn_completed", map[string]string{"stopReason": "end_turn"})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Conversation(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != ConversationSourceGateway || snapshot.GatewayCursor != 4 || snapshot.Warning == "" || len(snapshot.Turns) != 1 {
		t.Fatalf("fallback snapshot = %#v", snapshot)
	}
	if snapshot.Turns[0].Committed || snapshot.Turns[0].Key != "REQ-fallback" ||
		!hasConversationDisplay(snapshot.Turns[0].Activities, "thought_chunk", "Gateway reasoning") ||
		!hasConversationDisplay(snapshot.Turns[0].Activities, "assistant_chunk", "Gateway answer") {
		t.Fatalf("fallback gateway turn = %#v", snapshot.Turns[0])
	}
	if len(snapshot.Diagnostics) != 4 {
		t.Fatalf("fallback diagnostics = %d, want 4", len(snapshot.Diagnostics))
	}
}

func TestConversationProviderFailureReturnsCapturedGatewayProjection(t *testing.T) {
	service, _, current := nativeServiceHarness(t, "codex", "provider-store-unavailable")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-provider-home"))
	log, err := sessionlog.Open(service.logPath(current))
	if err != nil {
		t.Fatal(err)
	}
	appendConversationLine(t, log, "REQ-provider-fallback", "prompt_submitted", map[string]string{"display_message": "Visible gateway prompt"})
	appendConversationACP(t, log, "REQ-provider-fallback", "Visible gateway answer")
	appendConversationLine(t, log, "REQ-provider-fallback", "turn_completed", map[string]string{"stopReason": "end_turn"})
	appendConversationLine(t, log, "", "worker_error", map[string]string{"message": "TOKEN=must-not-cross /private/provider/path"})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.Conversation(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != ConversationSourceGateway || snapshot.GatewayCursor != 4 || len(snapshot.Turns) != 1 || snapshot.Warning == "" {
		t.Fatalf("provider failure fallback = %#v", snapshot)
	}
	if snapshot.Turns[0].Committed || !hasConversationDisplay(snapshot.Turns[0].Activities, "assistant_chunk", "Visible gateway answer") {
		t.Fatalf("provider failure gateway turn = %#v", snapshot.Turns[0])
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-cross", "/private/provider/path", "TOKEN="} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("fallback projection leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestConversationGatewayFallbackDisclosesBoundedHistoryEviction(t *testing.T) {
	service, _, current := nativeServiceHarness(t, "codex", "")
	log, err := sessionlog.Open(service.logPath(current))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 300; index++ {
		requestID := fmt.Sprintf("REQ-bounded-%03d", index)
		appendConversationLine(t, log, requestID, "prompt_submitted", map[string]string{"display_message": "Prompt " + requestID})
		appendConversationACP(t, log, requestID, "Answer "+requestID)
		appendConversationLine(t, log, requestID, "turn_completed", map[string]string{"stopReason": "end_turn"})
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.Conversation(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != ConversationSourceGateway || !snapshot.Truncated || snapshot.GatewayCursor != 900 || len(snapshot.Turns) != conversationRetainedTurns {
		t.Fatalf("bounded gateway fallback = source %q truncated=%t cursor=%d turns=%d", snapshot.Source, snapshot.Truncated, snapshot.GatewayCursor, len(snapshot.Turns))
	}
	if snapshot.Turns[0].Key != "REQ-bounded-044" || !strings.Contains(snapshot.Warning, "Earlier gateway history was omitted") {
		t.Fatalf("bounded gateway fallback first=%q warning=%q", snapshot.Turns[0].Key, snapshot.Warning)
	}
	for _, turn := range snapshot.Turns {
		if turn.Surface != ConversationSurfaceACP {
			t.Fatalf("gateway fallback surface = %q, want ACP", turn.Surface)
		}
	}
}

func TestConversationMergeDoesNotLetARepeatedAnswerCommitTheNewerTurn(t *testing.T) {
	firstAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Hour)
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user-1", Kind: native.TranscriptUser, Text: "Repeat this", Timestamp: firstAt},
		{Key: "provider-answer-1", Kind: native.TranscriptAssistant, Text: "OK", Timestamp: firstAt},
	}}
	old := conversationGatewayFixture("REQ-old", 1, "Repeat this", "OK", true)
	newer := conversationGatewayFixture("REQ-new", 2, "Repeat this", "OK", true)
	old.answerAt = firstAt
	newer.answerAt = secondAt

	merged := mergeConversationTurns(provider, []*conversationTurn{old, newer})
	if len(merged) != 2 || !merged[0].Committed || merged[1].Committed {
		t.Fatalf("one stored repeated turn merge = %#v", merged)
	}
	if merged[0].Activities[0].RequestID != old.id || merged[1].Key != newer.id {
		t.Fatalf("newer repeated turn stole the old provider commit: %#v", merged)
	}

	provider.Records = append(provider.Records,
		native.TranscriptRecord{Key: "provider-user-2", Kind: native.TranscriptUser, Text: "Repeat this", Timestamp: secondAt},
		native.TranscriptRecord{Key: "provider-answer-2", Kind: native.TranscriptAssistant, Text: "OK", Timestamp: secondAt},
	)
	merged = mergeConversationTurns(provider, []*conversationTurn{old, newer})
	if len(merged) != 2 || !merged[0].Committed || !merged[1].Committed ||
		merged[0].Activities[0].RequestID != old.id || merged[1].Activities[0].RequestID != newer.id {
		t.Fatalf("two stored repeated turns did not reconcile monotonically: %#v", merged)
	}
}

func TestConversationCoverageDoesNotInventChunkWhitespace(t *testing.T) {
	var streamed conversationCoverage
	for _, chunk := range []string{"ACP", "-C", "ODE", "X-D116670"} {
		streamed.append(chunk)
	}
	var stored conversationCoverage
	stored.append("ACP-CODEX-D116670")
	if !streamed.equal(stored) {
		t.Fatal("ACP chunk boundaries changed the answer fingerprint")
	}

	var spaced conversationCoverage
	spaced.append("hello ")
	spaced.append("  world")
	var normalized conversationCoverage
	normalized.append("hello world")
	if !spaced.equal(normalized) {
		t.Fatal("actual whitespace was not normalized consistently")
	}
}

func TestConversationMergeRequiresPromptIdentityForShortAnswers(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "First prompt"},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "OK"},
	}}
	gateway := conversationGatewayFixture("REQ-second", 1, "Different prompt", "OK", true)
	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 2 || !merged[0].Committed || merged[1].Committed || merged[1].Key != gateway.id {
		t.Fatalf("short answer with a different prompt was falsely committed: %#v", merged)
	}
	if merged[0].Surface != ConversationSurfaceUnknown || merged[1].Surface != ConversationSurfaceACP {
		t.Fatalf("ambiguous provider/gateway surfaces = %q/%q", merged[0].Surface, merged[1].Surface)
	}
}

func TestConversationMergeRecognizesCleanPromptInsideInjectedEnvelope(t *testing.T) {
	wrapper := "Task T-private: Wrapped task\nDescription: Visible description\nSession: S-private\nRole: reviewer\n" +
		"Repository: /private/repo\nWorktree: /private/worktree\nBranch: private/branch\n" +
		"Work in the repository context at /private/worktree. This is one Session under the Task; preserve the Task/Session boundary.\n\n" +
		"Session-specific instructions:\nReview safely."
	clean := "Start work on \"Wrapped task\" as reviewer.\n\nVisible description\n\nReview safely."
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: wrapper},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "OK"},
	}}
	gateway := conversationGatewayFixture("REQ-wrapped", 1, clean, "OK", true)

	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 1 || !merged[0].Committed || merged[0].Activities[0].RequestID != gateway.id {
		t.Fatalf("unique short wrapped prompt did not reconcile safely: %#v", merged)
	}
	providerOnly := mergeConversationTurns(provider, nil)
	if len(providerOnly) != 1 || providerOnly[0].Surface != ConversationSurfaceUnknown || providerOnly[0].Activities[0].Display == nil || providerOnly[0].Activities[0].Display.Text != clean {
		t.Fatalf("unmatched provider envelope was exposed instead of cleaned: %#v", providerOnly)
	}
	for _, secret := range []string{"T-private", "S-private", "/private/repo", "/private/worktree", "private/branch"} {
		if strings.Contains(providerOnly[0].Activities[0].Display.Text, secret) {
			t.Fatalf("provider display leaked %q: %q", secret, providerOnly[0].Activities[0].Display.Text)
		}
	}
}

func TestConversationMergeReconcilesACPThenNativeThenRepeatedACPAnswer(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user-1", Kind: native.TranscriptUser, Text: "Injected Task and Session envelope around start work"},
		{Key: "provider-answer-1", Kind: native.TranscriptAssistant, Text: "ACP-CODEX-D116670"},
		{Key: "provider-user-2", Kind: native.TranscriptUser, Text: "What marker was used?"},
		{Key: "provider-answer-2", Kind: native.TranscriptAssistant, Text: "NATIVE-CODEX-D116670"},
		{Key: "provider-user-3", Kind: native.TranscriptUser, Text: "Repeat the immediately previous native marker"},
		{Key: "provider-answer-3", Kind: native.TranscriptAssistant, Text: "NATIVE-CODEX-D116670"},
	}}
	firstACP := conversationGatewayFixture("REQ-acp-1", 1, "Start work", "ACP-CODEX-D116670", true)
	secondACP := conversationGatewayFixture("REQ-acp-2", 2, "Repeat the immediately previous native marker", "NATIVE-CODEX-D116670", true)
	secondACP.activities = append(secondACP.activities[:1], append([]ConversationActivity{{
		RequestID: secondACP.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "ACP-only reasoning"},
	}}, secondACP.activities[1:]...)...)

	merged := mergeConversationTurns(provider, []*conversationTurn{firstACP, secondACP})
	if len(merged) != 3 || !merged[0].Committed || !merged[1].Committed || !merged[2].Committed {
		t.Fatalf("ACP/native/repeated-ACP history duplicated gateway turns: %#v", merged)
	}
	if merged[0].Activities[0].RequestID != firstACP.id || merged[1].Activities[0].RequestID != "provider-user-2" ||
		merged[2].Activities[0].RequestID != secondACP.id {
		t.Fatalf("gateway turns mapped to the wrong provider occurrence: %#v", merged)
	}
	if !hasConversationDisplay(merged[2].Activities, "thought_chunk", "ACP-only reasoning") {
		t.Fatalf("ACP-only reasoning was lost on the repeated provider answer: %#v", merged[2])
	}
	if !hasConversationDisplayKind(merged[2].Activities, "assistant_chunk", "provider_transcript") ||
		!hasConversationDisplayKind(merged[2].Activities, "thought_chunk", "acp") {
		t.Fatalf("matched answer and supplemental ACP reasoning provenance was conflated: %#v", merged[2])
	}
	if merged[0].Surface != ConversationSurfaceACP || merged[1].Surface != ConversationSurfaceNative || merged[2].Surface != ConversationSurfaceACP {
		t.Fatalf("ACP/native/ACP surfaces = %q/%q/%q", merged[0].Surface, merged[1].Surface, merged[2].Surface)
	}
	bounded := mergeConversationTurnsWithGatewayState(provider, []*conversationTurn{firstACP, secondACP}, true)
	if bounded[0].Surface != ConversationSurfaceACP || bounded[1].Surface != ConversationSurfaceUnknown || bounded[2].Surface != ConversationSurfaceACP {
		t.Fatalf("bounded ACP/unknown/ACP surfaces = %q/%q/%q", bounded[0].Surface, bounded[1].Surface, bounded[2].Surface)
	}
	ambiguousGateway := conversationGatewayFixture("REQ-ambiguous", 2, "An unmatched ACP prompt", "AN-UNMATCHED-ACP-ANSWER", true)
	withAmbiguousGateway := mergeConversationTurns(provider, []*conversationTurn{firstACP, ambiguousGateway, secondACP})
	var middleProvider *ConversationTurn
	for index := range withAmbiguousGateway {
		if withAmbiguousGateway[index].Key == "provider-user-2" {
			middleProvider = &withAmbiguousGateway[index]
			break
		}
	}
	if middleProvider == nil || middleProvider.Surface != ConversationSurfaceUnknown {
		t.Fatalf("provider turn bracketed around an unmatched gateway invocation was falsely native: %#v", withAmbiguousGateway)
	}
}

func TestConversationMergeUsesProviderSemanticHistoryWhenBothSourcesContainReasoning(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "Inspect the history"},
		{Key: "provider-reasoning", Kind: native.TranscriptReasoning, Text: "Shared reasoning summary"},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "A sufficiently long committed answer"},
	}}
	gateway := conversationGatewayFixture("REQ-matched", 1, "Inspect the history", "A sufficiently long committed answer", true)
	gateway.activities = []ConversationActivity{
		{RequestID: gateway.id, Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: "Inspect the history"}},
		{RequestID: gateway.id, Kind: "turn_started", Display: &presentation.Activity{Type: "turn_state", State: "working"}},
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "Shared reasoning"}},
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: " summary"}},
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "tool_upsert", Tool: &presentation.Tool{ID: "tool-1", Title: "Inspect", Status: "completed"}}},
		{RequestID: gateway.id, Kind: "permission_resolved", Display: &presentation.Activity{Type: "permission", State: "resolved"}, Permission: &ConversationPermission{ID: "permission-1", State: "resolved", Decision: "allow_once"}},
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "assistant_chunk", Text: "A sufficiently long committed answer"}},
		{RequestID: gateway.id, Kind: "turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete", StopReason: "end_turn"}},
	}

	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 1 || !merged[0].Committed {
		t.Fatalf("matched committed turn = %#v", merged)
	}
	if countConversationDisplayKind(merged[0].Activities, "user_message", "provider_transcript") != 1 ||
		countConversationDisplayKind(merged[0].Activities, "thought_chunk", "provider_transcript") != 1 ||
		countConversationDisplayKind(merged[0].Activities, "assistant_chunk", "provider_transcript") != 1 {
		t.Fatalf("provider semantic records were not authoritative: %#v", merged[0].Activities)
	}
	if hasConversationDisplayKind(merged[0].Activities, "thought_chunk", "acp") {
		t.Fatalf("normalized duplicate ACP reasoning remained beside provider history: %#v", merged[0].Activities)
	}
	for _, displayType := range []string{"turn_state", "tool_upsert", "permission"} {
		if !containsConversationDisplayType(merged[0].Activities, displayType) {
			t.Fatalf("gateway %s activity was lost: %#v", displayType, merged[0].Activities)
		}
	}
	if !hasConversationDisplay(merged[0].Activities, "thought_chunk", "Shared reasoning summary") {
		t.Fatalf("safe provider reasoning summary was not retained: %#v", merged[0].Activities)
	}
}

func TestConversationMergeKeepsOnlyGenuinelySupplementalACPReasoning(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "Inspect the history"},
		{Key: "provider-reasoning", Kind: native.TranscriptReasoning, Text: "Shared reasoning"},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "A sufficiently long committed answer"},
	}}
	gateway := conversationGatewayFixture("REQ-supplemental", 1, "Inspect the history", "A sufficiently long committed answer", true)
	gateway.activities = append(gateway.activities[:1], append([]ConversationActivity{
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "Shared reasoning"}},
		{RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: " Additional ACP-only detail"}},
	}, gateway.activities[1:]...)...)

	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 1 ||
		!hasConversationDisplayKind(merged[0].Activities, "thought_chunk", "provider_transcript") ||
		!hasConversationDisplayKind(merged[0].Activities, "thought_chunk", "acp") {
		t.Fatalf("provider and supplemental ACP reasoning were not both retained: %#v", merged)
	}
	if countConversationDisplayKind(merged[0].Activities, "thought_chunk", "acp") != 1 ||
		!hasConversationDisplay(merged[0].Activities, "thought_chunk", "Additional ACP-only detail") {
		t.Fatalf("provider-equal ACP reasoning was not removed from the supplemental text: %#v", merged[0].Activities)
	}
}

func TestConversationMergePreservesInterleavedProviderSemanticOrderAroundGatewayTool(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "Inspect in phases"},
		{Key: "provider-reasoning-1", Kind: native.TranscriptReasoning, Text: "Reasoning before commentary"},
		{Key: "provider-assistant-1", Kind: native.TranscriptAssistant, Phase: "commentary", Text: "Commentary before tool"},
		{Key: "provider-reasoning-2", Kind: native.TranscriptReasoning, Text: "Reasoning after tool"},
		{Key: "provider-assistant-2", Kind: native.TranscriptAssistant, Phase: "final", Text: "Final committed response"},
	}}
	gateway := &conversationTurn{
		id: "REQ-interleaved", first: 1, terminal: true, committable: true,
		userMatchable: true, answerMatchable: true, retainActivities: true,
		activities: []ConversationActivity{
			{RequestID: "REQ-interleaved", Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: "Inspect in phases"}},
			{RequestID: "REQ-interleaved", Kind: "turn_started", Display: &presentation.Activity{Type: "turn_state", State: "working"}},
			{RequestID: "REQ-interleaved", Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "Reasoning before commentary"}},
			{RequestID: "REQ-interleaved", Kind: "acp", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Commentary before tool"}},
			{RequestID: "REQ-interleaved", Kind: "acp", Display: &presentation.Activity{Type: "tool_upsert", Tool: &presentation.Tool{ID: "tool-between", Title: "Inspect", Status: "completed"}}},
			{RequestID: "REQ-interleaved", Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "Reasoning after tool"}},
			{RequestID: "REQ-interleaved", Kind: "acp", Display: &presentation.Activity{Type: "assistant_chunk", Text: "Final committed response"}},
			{RequestID: "REQ-interleaved", Kind: "turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete", StopReason: "end_turn"}},
		},
	}
	gateway.user.append("Inspect in phases")
	gateway.assistant.append("Commentary before tool")
	gateway.assistant.append("Final committed response")

	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 1 || !merged[0].Committed {
		t.Fatalf("interleaved committed turn = %#v", merged)
	}
	var semantic []string
	toolIndex := -1
	for index, activity := range merged[0].Activities {
		if activity.Display == nil {
			continue
		}
		switch activity.Display.Type {
		case "user_message", "thought_chunk", "assistant_chunk":
			semantic = append(semantic, activity.Display.Type+":"+activity.Display.Text)
			if activity.Kind != "provider_transcript" {
				t.Fatalf("semantic activity was not provider authoritative: %#v", activity)
			}
		case "tool_upsert":
			toolIndex = index
		}
	}
	want := []string{
		"user_message:Inspect in phases",
		"thought_chunk:Reasoning before commentary",
		"assistant_chunk:Commentary before tool",
		"thought_chunk:Reasoning after tool",
		"assistant_chunk:Final committed response",
	}
	if strings.Join(semantic, "|") != strings.Join(want, "|") {
		t.Fatalf("provider semantic order = %#v, want %#v; activities = %#v", semantic, want, merged[0].Activities)
	}
	commentaryIndex := conversationDisplayIndex(merged[0].Activities, "assistant_chunk", "Commentary before tool")
	secondReasoningIndex := conversationDisplayIndex(merged[0].Activities, "thought_chunk", "Reasoning after tool")
	if toolIndex <= commentaryIndex || toolIndex >= secondReasoningIndex {
		t.Fatalf("gateway tool lost its observed semantic anchors: commentary=%d tool=%d next-reasoning=%d activities=%#v", commentaryIndex, toolIndex, secondReasoningIndex, merged[0].Activities)
	}
}

func TestConversationMergePreservesACPReasoningWhenProviderHasNoReadableReasoning(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "Inspect the history"},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "A sufficiently long committed answer"},
	}}
	gateway := conversationGatewayFixture("REQ-acp-fallback", 1, "Inspect the history", "A sufficiently long committed answer", true)
	gateway.activities = append(gateway.activities[:1], append([]ConversationActivity{{
		RequestID: gateway.id, Kind: "acp", Display: &presentation.Activity{Type: "thought_chunk", Text: "ACP reasoning remains visible"},
	}}, gateway.activities[1:]...)...)

	merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
	if len(merged) != 1 || !hasConversationDisplayKind(merged[0].Activities, "thought_chunk", "acp") ||
		!hasConversationDisplay(merged[0].Activities, "thought_chunk", "ACP reasoning remains visible") {
		t.Fatalf("ACP reasoning fallback was lost: %#v", merged)
	}
}

func TestConversationMergeNeverCommitsCancelledOrTruncatedGatewayOutput(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-user", Kind: native.TranscriptUser, Text: "Do it"},
		{Key: "provider-answer", Kind: native.TranscriptAssistant, Text: "A sufficiently long response"},
	}}
	cancelled := conversationGatewayFixture("REQ-cancelled", 1, "Do it", "A sufficiently long response", false)
	truncated := conversationGatewayFixture("REQ-truncated", 2, "Do it", "A sufficiently long response", true)
	truncated.answerMatchable = false

	for _, gateway := range []*conversationTurn{cancelled, truncated} {
		merged := mergeConversationTurns(provider, []*conversationTurn{gateway})
		if len(merged) != 2 || !merged[0].Committed || merged[1].Committed || merged[1].Key != gateway.id {
			t.Fatalf("unsafe gateway turn %q was falsely committed: %#v", gateway.id, merged)
		}
	}
}

func TestConversationProviderGroupingDropsLeadingPartialTurn(t *testing.T) {
	turns := groupProviderTurns([]native.TranscriptRecord{
		{Key: "orphan-answer", Kind: native.TranscriptAssistant, Text: "must not appear"},
		{Key: "orphan-reasoning", Kind: native.TranscriptReasoning, Text: "must not appear"},
		{Key: "real-user", Kind: native.TranscriptUser, Text: "Visible prompt"},
		{Key: "real-answer", Kind: native.TranscriptAssistant, Text: "Visible answer"},
	})
	if len(turns) != 1 || len(turns[0].records) != 2 || turns[0].key != "real-user" {
		t.Fatalf("provider leading partial was rendered as a turn: %#v", turns)
	}
}

func TestConversationOpenCodeMultipartUserMessageDoesNotFabricateIncompleteTurn(t *testing.T) {
	const conversationID = "opencode-multipart-conversation"
	service, _, current := nativeServiceHarness(t, "opencode", conversationID)
	if current.ProviderConversationID != conversationID {
		t.Fatalf("canonical OpenCode conversation = %q, want %q", current.ProviderConversationID, conversationID)
	}
	databasePath := filepath.Join(t.TempDir(), "opencode.db")
	t.Setenv("OPENCODE_DB", databasePath)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session (id, directory) VALUES ('opencode-multipart-conversation', '` + strings.ReplaceAll(current.RepoPath, "'", "''") + `')`,
		`INSERT INTO message VALUES ('user-message', 'opencode-multipart-conversation', 1000, '{"role":"user"}')`,
		`INSERT INTO message VALUES ('assistant-message', 'opencode-multipart-conversation', 2000, '{"role":"assistant","time":{"completed":2100},"finish":"stop"}')`,
		`INSERT INTO part VALUES ('user-text-1', 'user-message', 'opencode-multipart-conversation', 1001, '{"type":"text","text":"first fragment"}')`,
		`INSERT INTO part VALUES ('user-reasoning', 'user-message', 'opencode-multipart-conversation', 1002, '{"type":"reasoning","text":"must not render"}')`,
		`INSERT INTO part VALUES ('user-text-2', 'user-message', 'opencode-multipart-conversation', 1003, '{"type":"text","text":"second fragment"}')`,
		`INSERT INTO part VALUES ('assistant-text', 'assistant-message', 'opencode-multipart-conversation', 2001, '{"type":"text","text":"complete answer"}')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			t.Fatalf("OpenCode conversation fixture: %v", err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.Conversation(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != ConversationSourceProvider || len(snapshot.Provider.Records) != 2 || len(snapshot.Turns) != 1 {
		t.Fatalf("OpenCode multipart conversation = %#v", snapshot)
	}
	turn := snapshot.Turns[0]
	if !turn.Committed || !hasConversationDisplay(turn.Activities, "user_message", "first fragment\nsecond fragment") ||
		!hasConversationDisplay(turn.Activities, "assistant_chunk", "complete answer") ||
		hasConversationActivityKind(turn.Activities, "provider_turn_incomplete") {
		t.Fatalf("OpenCode multipart provider message fabricated a partial turn: %#v", turn)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must not render") {
		t.Fatalf("OpenCode user reasoning part crossed the conversation boundary: %s", encoded)
	}
}

func TestConversationProviderGroupingAnchorsTrailingPartialWithoutDuplicatingLiveACP(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "complete-user", Kind: native.TranscriptUser, Text: "Earlier prompt"},
		{Key: "complete-answer", Kind: native.TranscriptAssistant, Text: "Earlier answer"},
		{Key: "active-user", Kind: native.TranscriptUser, Text: "Active prompt"},
		{Key: "active-reasoning", Kind: native.TranscriptReasoning, Text: "Partial reasoning"},
		{Key: "active-answer", Kind: native.TranscriptAssistant, Phase: "commentary", Text: "Partial answer"},
	}}
	active := &conversationTurn{
		id: "REQ-active", first: 2, userMatchable: true, answerMatchable: true, retainActivities: true,
		activities: []ConversationActivity{{
			RequestID: "REQ-active", Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: "Active prompt"},
		}},
	}
	active.user.append("Active prompt")

	grouped := groupProviderTurns(provider.Records)
	if len(grouped) != 2 || grouped[0].key != "complete-user" || grouped[1].key != "active-user" || grouped[1].complete {
		t.Fatalf("trailing partial provider turn was not retained as an incomplete anchor: %#v", grouped)
	}
	merged := mergeConversationTurns(provider, []*conversationTurn{active})
	if len(merged) != 2 || !merged[0].Committed || merged[1].Committed || merged[1].Key != active.id {
		t.Fatalf("trailing provider partial duplicated the live ACP turn: %#v", merged)
	}
	if merged[1].Surface != ConversationSurfaceACP || hasConversationActivityKind(merged[1].Activities, "provider_turn_incomplete") {
		t.Fatalf("matched live ACP turn was replaced by an inferred provider failure: %#v", merged[1])
	}
}

func TestConversationProviderGroupingUsesInterruptedMiddlePartialAsOrderAnchor(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "user-1", Kind: native.TranscriptUser, Text: "First prompt"},
		{Key: "answer-1", Kind: native.TranscriptAssistant, Text: "First durable answer"},
		{Key: "failed-user", Kind: native.TranscriptUser, Text: "Interrupted prompt"},
		{Key: "failed-reasoning", Kind: native.TranscriptReasoning, Text: "Partial reasoning"},
		{Key: "user-3", Kind: native.TranscriptUser, Text: "Third prompt"},
		{Key: "answer-3", Kind: native.TranscriptAssistant, Text: "Third durable answer"},
	}}
	first := conversationGatewayFixture("REQ-1", 1, "First prompt", "First durable answer", true)
	failed := conversationGatewayFixture("REQ-failed", 2, "Interrupted prompt", "partial", false)
	failed.activities = append(failed.activities, ConversationActivity{
		RequestID: failed.id, Kind: "turn_error", Display: &presentation.Activity{Type: "error_upsert", State: "error", Text: "Provider turn failed"},
	})
	third := conversationGatewayFixture("REQ-3", 3, "Third prompt", "Third durable answer", true)

	grouped := groupProviderTurns(provider.Records)
	if len(grouped) != 3 || grouped[0].key != "user-1" || grouped[1].key != "failed-user" || grouped[1].complete || grouped[2].key != "user-3" {
		t.Fatalf("interrupted provider partial was not retained as an order anchor: %#v", grouped)
	}
	merged := mergeConversationTurns(provider, []*conversationTurn{first, failed, third})
	if len(merged) != 3 || !merged[0].Committed || merged[1].Committed || !merged[2].Committed || merged[1].Key != failed.id {
		t.Fatalf("interrupted provider partial duplicated or reordered the gateway failure: %#v", merged)
	}
}

func TestConversationMergeOrdersMatchedACPFailureBeforeNativeAndLaterACP(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "provider-A-user", Kind: native.TranscriptUser, Text: "Prompt A"},
		{Key: "provider-A-answer", Kind: native.TranscriptAssistant, Text: "A durable answer"},
		{Key: "provider-B-user", Kind: native.TranscriptUser, Text: "Prompt B"},
		{Key: "provider-B-reasoning", Kind: native.TranscriptReasoning, Text: "B partial reasoning"},
		{Key: "provider-B-answer", Kind: native.TranscriptAssistant, Phase: "commentary", Text: "B partial"},
		{Key: "provider-C-user", Kind: native.TranscriptUser, Text: "Native prompt C"},
		{Key: "provider-C-answer", Kind: native.TranscriptAssistant, Text: "C durable native answer"},
		{Key: "provider-D-user", Kind: native.TranscriptUser, Text: "Prompt D"},
		{Key: "provider-D-answer", Kind: native.TranscriptAssistant, Text: "D durable answer"},
	}}
	a := conversationGatewayFixture("REQ-A", 1, "Prompt A", "A durable answer", true)
	b := conversationGatewayFixture("REQ-B", 2, "Prompt B", "B partial output visible over ACP", false)
	b.activities = append(b.activities, ConversationActivity{
		RequestID: b.id, Kind: "turn_error", Display: &presentation.Activity{Type: "error_upsert", State: "error", Text: "Provider turn failed"},
	})
	d := conversationGatewayFixture("REQ-D", 3, "Prompt D", "D durable answer", true)

	merged := mergeConversationTurns(provider, []*conversationTurn{a, b, d})
	if len(merged) != 4 {
		t.Fatalf("A/B/C/D merge length = %d, want 4: %#v", len(merged), merged)
	}
	wantKeys := []string{"provider-A-user", b.id, "provider-C-user", "provider-D-user"}
	wantSurfaces := []ConversationSurface{ConversationSurfaceACP, ConversationSurfaceACP, ConversationSurfaceNative, ConversationSurfaceACP}
	for index := range wantKeys {
		if merged[index].Key != wantKeys[index] || merged[index].Surface != wantSurfaces[index] {
			t.Fatalf("A/B/C/D turn %d = key %q surface %q, want %q/%q: %#v", index, merged[index].Key, merged[index].Surface, wantKeys[index], wantSurfaces[index], merged)
		}
	}
	if merged[1].Committed || !hasConversationDisplay(merged[1].Activities, "error_upsert", "Provider turn failed") ||
		hasConversationActivityKind(merged[1].Activities, "provider_turn_incomplete") {
		t.Fatalf("failed ACP turn was not emitted exactly once from gateway history: %#v", merged[1])
	}
	if !merged[0].Committed || !merged[2].Committed || !merged[3].Committed {
		t.Fatalf("A/C/D provider commits were lost: %#v", merged)
	}
}

func TestConversationMergeRendersUnmatchedProviderPartialsAsIncompleteUnknownTurns(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "leading-orphan", Kind: native.TranscriptAssistant, Text: "must be omitted"},
		{Key: "complete-user", Kind: native.TranscriptUser, Text: "Complete prompt"},
		{Key: "complete-answer", Kind: native.TranscriptAssistant, Text: "Complete answer"},
		{Key: "middle-user", Kind: native.TranscriptUser, Text: "Interrupted middle prompt"},
		{Key: "middle-reasoning", Kind: native.TranscriptReasoning, Text: "Partial middle reasoning"},
		{Key: "later-user", Kind: native.TranscriptUser, Text: "Later complete prompt"},
		{Key: "later-answer", Kind: native.TranscriptAssistant, Text: "Later complete answer"},
		{Key: "trailing-user", Kind: native.TranscriptUser, Text: "Trailing incomplete prompt"},
		{Key: "trailing-answer", Kind: native.TranscriptAssistant, Phase: "commentary", Text: "Trailing partial output"},
	}}

	merged := mergeConversationTurns(provider, nil)
	if len(merged) != 4 {
		t.Fatalf("unmatched provider turns = %d, want 4: %#v", len(merged), merged)
	}
	for _, index := range []int{1, 3} {
		turn := merged[index]
		if turn.Committed || turn.Surface != ConversationSurfaceUnknown ||
			!hasConversationTurnState(turn.Activities, "incomplete", "provider_history_incomplete") ||
			hasConversationTurnState(turn.Activities, "complete", "") {
			t.Fatalf("provider partial %d was presented as final or attributed to a surface: %#v", index, turn)
		}
	}
	for _, turn := range merged {
		if hasConversationDisplay(turn.Activities, "assistant_chunk", "must be omitted") {
			t.Fatalf("leading assistant-only truncation became a fabricated turn: %#v", merged)
		}
	}
}

func TestConversationMergeRefusesAmbiguousOrOpenBoundedPartialMatches(t *testing.T) {
	provider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "A-user", Kind: native.TranscriptUser, Text: "A"},
		{Key: "A-answer", Kind: native.TranscriptAssistant, Text: "A durable answer"},
		{Key: "partial-user", Kind: native.TranscriptUser, Text: "Repeat partial"},
		{Key: "C-user", Kind: native.TranscriptUser, Text: "Native C"},
		{Key: "C-answer", Kind: native.TranscriptAssistant, Text: "C durable answer"},
		{Key: "D-user", Kind: native.TranscriptUser, Text: "D"},
		{Key: "D-answer", Kind: native.TranscriptAssistant, Text: "D durable answer"},
	}}
	a := conversationGatewayFixture("REQ-A", 1, "A", "A durable answer", true)
	b1 := conversationGatewayFixture("REQ-B1", 2, "Repeat partial", "", false)
	b2 := conversationGatewayFixture("REQ-B2", 3, "Repeat partial", "", false)
	d := conversationGatewayFixture("REQ-D", 4, "D", "D durable answer", true)

	ambiguous := mergeConversationTurns(provider, []*conversationTurn{a, b1, b2, d})
	if len(ambiguous) != 6 || countConversationTurnKey(ambiguous, b1.id) != 1 || countConversationTurnKey(ambiguous, b2.id) != 1 ||
		countConversationTurnKey(ambiguous, "partial-user") != 1 {
		t.Fatalf("ambiguous repeated partial was guessed or suppressed: %#v", ambiguous)
	}
	for _, turn := range ambiguous {
		if turn.Key == "C-user" && turn.Surface != ConversationSurfaceUnknown {
			t.Fatalf("ambiguous gateway gap falsely attributed provider C to native: %#v", ambiguous)
		}
	}

	trailingProvider := native.TranscriptSnapshot{Records: []native.TranscriptRecord{
		{Key: "A-user", Kind: native.TranscriptUser, Text: "A"},
		{Key: "A-answer", Kind: native.TranscriptAssistant, Text: "A durable answer"},
		{Key: "partial-user", Kind: native.TranscriptUser, Text: "Trailing active"},
	}}
	active := conversationGatewayFixture("REQ-active", 2, "Trailing active", "", false)
	bounded := mergeConversationTurnsWithGatewayState(trailingProvider, []*conversationTurn{a, active}, true)
	if len(bounded) != 3 || countConversationTurnKey(bounded, "partial-user") != 1 || countConversationTurnKey(bounded, active.id) != 1 {
		t.Fatalf("open-ended bounded history falsely deduplicated a partial match: %#v", bounded)
	}
}

func TestIncompleteProviderGatewayMatchRequiresExactSafeCompatibleEvidence(t *testing.T) {
	started := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	providerFixture := func(user, answer string, truncated bool) *providerConversationTurn {
		records := []native.TranscriptRecord{{Key: "partial-user", Kind: native.TranscriptUser, Text: user, Timestamp: started, Truncated: truncated}}
		if answer != "" {
			records = append(records, native.TranscriptRecord{Key: "partial-answer", Kind: native.TranscriptAssistant, Phase: "commentary", Text: answer, Timestamp: started.Add(time.Second)})
		}
		return groupProviderTurns(records)[0]
	}
	gatewayFixture := func(user, answer string, timestamp time.Time) *conversationTurn {
		turn := conversationGatewayFixture("REQ-partial", 1, user, answer, false)
		turn.firstAt = timestamp
		turn.answerAt = timestamp.Add(2 * time.Second)
		return turn
	}
	compatible := func(provider *providerConversationTurn, gateway *conversationTurn) bool {
		return incompleteProviderGatewayCompatible(provider, gateway, map[*providerConversationTurn]string{}, map[*conversationTurn]string{})
	}

	if !compatible(providerFixture("Exact prompt", "partial", false), gatewayFixture("Exact   prompt", "partial response", started)) {
		t.Fatal("normalized exact prompt with a compatible assistant prefix did not match")
	}
	if compatible(providerFixture("Exact prompt", "partial", false), gatewayFixture("Different prompt", "partial response", started)) {
		t.Fatal("different prompt matched an incomplete provider group")
	}
	if compatible(providerFixture("Exact prompt", "partial", true), gatewayFixture("Exact prompt", "partial response", started)) {
		t.Fatal("truncated provider prompt matched an incomplete gateway turn")
	}
	if compatible(providerFixture("Exact prompt", "partial", false), gatewayFixture("Exact prompt", "different output", started)) {
		t.Fatal("incompatible partial assistant output matched")
	}
	if compatible(providerFixture("Exact prompt", "partial", false), gatewayFixture("Exact prompt", "partial response", started.Add(conversationMatchWindow+time.Second))) {
		t.Fatal("timestamp-incompatible repeated occurrence matched")
	}
}

func TestConversationRetainsACPOnlyReasoningBeyondSixteenTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	log, err := sessionlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	provider := native.TranscriptSnapshot{}
	for index := 0; index < 24; index++ {
		requestID := fmt.Sprintf("REQ-%02d", index)
		prompt := fmt.Sprintf("Prompt %02d", index)
		answer := fmt.Sprintf("ANSWER-%02d-LONG", index)
		appendConversationLine(t, log, requestID, "prompt_submitted", map[string]string{"display_message": prompt})
		appendConversationThought(t, log, requestID, fmt.Sprintf("ACP reasoning %02d", index))
		appendConversationACP(t, log, requestID, answer)
		appendConversationLine(t, log, requestID, "turn_completed", map[string]string{"stopReason": "end_turn"})
		provider.Records = append(provider.Records,
			native.TranscriptRecord{Key: fmt.Sprintf("provider-user-%02d", index), Kind: native.TranscriptUser, Text: prompt},
			native.TranscriptRecord{Key: fmt.Sprintf("provider-answer-%02d", index), Kind: native.TranscriptAssistant, Text: answer},
		)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	gateway, diagnostics, _, truncated, err := (&SessionService{}).gatewayConversationState(context.Background(), "S", path)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("small gateway conversation was unexpectedly marked truncated")
	}
	merged := mergeConversationTurns(provider, gateway)
	if len(merged) != 24 {
		t.Fatalf("merged turns = %d, want 24", len(merged))
	}
	for index, turn := range merged {
		if !turn.Committed || !hasConversationDisplay(turn.Activities, "thought_chunk", fmt.Sprintf("ACP reasoning %02d", index)) {
			t.Fatalf("ACP-only reasoning for turn %d was lost: %#v", index, turn)
		}
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Display != nil || diagnostic.Permission != nil || diagnostic.Summary == "" {
			t.Fatalf("diagnostics retained duplicate semantic or permission payload: %#v", diagnostic)
		}
	}
}

func TestGatewayConversationRetentionUsesOneStrictAggregateBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bounded.jsonl")
	log, err := sessionlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	largeAnswer := strings.Repeat("x", 128<<10)
	for index := 0; index < 140; index++ {
		requestID := fmt.Sprintf("REQ-large-%03d", index)
		appendConversationLine(t, log, requestID, "prompt_submitted", map[string]string{"display_message": requestID})
		appendConversationACP(t, log, requestID, largeAnswer)
		appendConversationLine(t, log, requestID, "turn_completed", map[string]string{"stopReason": "end_turn"})
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	turns, diagnostics, _, truncated, err := (&SessionService{}).gatewayConversationState(context.Background(), "S", path)
	if err != nil {
		t.Fatal(err)
	}
	var retained int
	for _, turn := range turns {
		retained += turn.bytes
	}
	if retained > conversationRetainedActivityBudget {
		t.Fatalf("retained activity bytes = %d, limit %d", retained, conversationRetainedActivityBudget)
	}
	if !truncated {
		t.Fatal("aggregate activity eviction was not disclosed as truncated")
	}
	if len(turns) != 140 || len(turns[0].activities) != 0 || len(turns[len(turns)-1].activities) == 0 {
		t.Fatalf("retention did not evict oldest completed details first: turns=%d first=%d last=%d", len(turns), len(turns[0].activities), len(turns[len(turns)-1].activities))
	}
	var diagnosticBytes int
	for _, diagnostic := range diagnostics {
		diagnosticBytes += conversationActivityBytes(diagnostic)
		if diagnostic.Display != nil || diagnostic.Permission != nil {
			t.Fatalf("diagnostic duplicated a turn payload: %#v", diagnostic)
		}
	}
	if diagnosticBytes > conversationDiagnosticBudget || len(diagnostics) > conversationDiagnostics {
		t.Fatalf("diagnostic retention = %d bytes/%d records", diagnosticBytes, len(diagnostics))
	}
}

func conversationGatewayFixture(id string, first uint64, user, answer string, committable bool) *conversationTurn {
	turn := &conversationTurn{
		id: id, first: first, terminal: true, committable: committable,
		userMatchable: true, answerMatchable: true, retainActivities: true,
		activities: []ConversationActivity{
			{RequestID: id, Kind: "prompt_submitted", Display: &presentation.Activity{Type: "user_message", Text: user}},
			{RequestID: id, Kind: "acp", Display: &presentation.Activity{Type: "assistant_chunk", Text: answer}},
		},
	}
	turn.user.append(user)
	turn.assistant.append(answer)
	return turn
}

func appendConversationLine(t *testing.T, log *sessionlog.Log, requestID, kind string, data any) {
	t.Helper()
	if _, err := log.Append(requestID, kind, data); err != nil {
		t.Fatal(err)
	}
}

func appendConversationACP(t *testing.T, log *sessionlog.Log, requestID, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"params": map[string]any{"update": map[string]any{"content": map[string]any{"type": "text", "text": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	appendConversationLine(t, log, requestID, "acp", acp.Event{Type: "agent_message_chunk", RequestID: requestID, Raw: raw})
}

func appendConversationThought(t *testing.T, log *sessionlog.Log, requestID, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"params": map[string]any{"update": map[string]any{"content": map[string]any{"type": "text", "text": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	appendConversationLine(t, log, requestID, "acp", acp.Event{Type: "agent_thought_chunk", RequestID: requestID, Raw: raw})
}

func hasConversationDisplay(activities []ConversationActivity, kind, text string) bool {
	for _, activity := range activities {
		if activity.Display != nil && activity.Display.Type == kind && activity.Display.Text == text {
			return true
		}
	}
	return false
}

func hasConversationDisplayKind(activities []ConversationActivity, displayType, activityKind string) bool {
	for _, activity := range activities {
		if activity.Kind == activityKind && activity.Display != nil && activity.Display.Type == displayType {
			return true
		}
	}
	return false
}

func countConversationDisplayKind(activities []ConversationActivity, displayType, activityKind string) int {
	count := 0
	for _, activity := range activities {
		if activity.Kind == activityKind && activity.Display != nil && activity.Display.Type == displayType {
			count++
		}
	}
	return count
}

func containsConversationDisplayType(activities []ConversationActivity, displayType string) bool {
	for _, activity := range activities {
		if activity.Display != nil && activity.Display.Type == displayType {
			return true
		}
	}
	return false
}

func hasConversationActivityKind(activities []ConversationActivity, kind string) bool {
	for _, activity := range activities {
		if activity.Kind == kind {
			return true
		}
	}
	return false
}

func hasConversationTurnState(activities []ConversationActivity, state, stopReason string) bool {
	for _, activity := range activities {
		if activity.Display == nil || activity.Display.Type != "turn_state" || activity.Display.State != state {
			continue
		}
		if stopReason == "" || activity.Display.StopReason == stopReason {
			return true
		}
	}
	return false
}

func countConversationTurnKey(turns []ConversationTurn, key string) int {
	count := 0
	for _, turn := range turns {
		if turn.Key == key {
			count++
		}
	}
	return count
}

func conversationDisplayIndex(activities []ConversationActivity, displayType, text string) int {
	for index, activity := range activities {
		if activity.Display != nil && activity.Display.Type == displayType && activity.Display.Text == text {
			return index
		}
	}
	return -1
}

func writeConversationFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
