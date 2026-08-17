package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This focused reducer test executes the embedded browser client without a
// browser. It protects transcript semantics that HTML string assertions
// cannot cover while keeping the production UI dependency-free.
func TestBrowserTranscriptReducerIsBoundedAndDeterministic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable; browser reducer test requires the same Node runtime as acpx")
	}
	source, err := content.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	harness := []byte(`
function testAssert(value, message) { if (!value) throw new Error(message); }
function resetTranscript() {
  transcriptState.turns.length = 0;
  transcriptState.byRequest.clear();
  transcriptState.permissions.clear();
  transcriptState.diagnostics.length = 0;
  transcriptState.lastPosition = 0;
  transcriptState.historyClipped = false;
  transcriptState.permissionOverflow = false;
  transcriptState.retainedBytes = 0;
}
function activity(position, type, display, permission, requestID = "REQ", timestamp = "") {
  return {session_id:"S", position, request_id:requestID, kind:type, activity:type, timestamp, display, permission};
}

const savedHistorySummary = conversationHistorySummary({turns:[{committed:true,blocks:[]},{committed:true,blocks:[]},{committed:true,blocks:[]}],snapshotTruncated:false,historyClipped:false});
testAssert(savedHistorySummary.short === "3 saved turns" && savedHistorySummary.detail.includes("all available history is loaded"), "complete provider history was not explained clearly");
const liveHistorySummary = conversationHistorySummary({turns:[{committed:false,blocks:[{type:"tool"},{type:"tool"}]}],snapshotTruncated:false,historyClipped:false});
testAssert(liveHistorySummary.short === "1 live turn · 2 steps" && liveHistorySummary.detail.includes("can be saved after successful completion"), "unfinished provider history was not explained clearly");
const stoppedHistorySummary = conversationHistorySummary({turns:[{committed:false,state:"cancelled",blocks:[]},{committed:false,state:"error",blocks:[]},{committed:false,state:"failed",blocks:[]}],snapshotTruncated:false,historyClipped:false});
testAssert(stoppedHistorySummary.short === "3 unsaved" && stoppedHistorySummary.active === 0 && stoppedHistorySummary.stopped === 3, "terminal unsaved history was misclassified as live");
testAssert(stoppedHistorySummary.detail.includes("remain unsaved history") && !stoppedHistorySummary.detail.includes("can be saved"), "terminal unsaved history promised an impossible provider save");
const incompleteHistorySummary = conversationHistorySummary({turns:[{committed:false,state:"incomplete",blocks:[]}],snapshotTruncated:false,historyClipped:false});
testAssert(incompleteHistorySummary.active === 0 && incompleteHistorySummary.stopped === 1, "incomplete provider history was misrepresented as a live invocation");
const liveWithoutReasoning = {surface:"acp",state:"working",reasoningSeen:false,permissionIDs:new Set(),tools:new Map(),lifecycle:[{state:"queued"},{state:"working"}],firstTimestamp:1000,lastTimestamp:2300,committed:false};
const liveOverview = turnOverviewElement(liveWithoutReasoning);
testAssert(liveOverview.textContent.includes("ACP turn") && liveOverview.textContent.includes("Working") && liveOverview.textContent.includes("No tools") && liveOverview.textContent.includes("Reasoning not provided"), "live invocation overview was incomplete");
testAssert(liveOverview.textContent.includes("Queued → Working") && !liveOverview.textContent.includes("1.3s"), "live invocation lifecycle used a misleading frozen duration");
const finalResponse = messageElement({source:"provider",text:"done",revision:1}, "codex", {final:true});
testAssert(finalResponse.textContent.includes("Final response · saved"), "committed assistant output was not identified as the final response");
transcriptState.permissions.set("PENDING", {id:"PENDING",state:"waiting"});
const approvalOverview = turnOverviewElement({surface:"acp",state:"working",reasoningSeen:true,permissionIDs:new Set(["PENDING"]),tools:new Map([["tool",{}]]),lifecycle:[{state:"working"}],firstTimestamp:0,lastTimestamp:0,committed:false});
testAssert(approvalOverview.textContent.includes("Approval required") && approvalOverview.className.includes("surface-acp"), "pending approval was hidden from the invocation overview");
testAssert(approvalOverview.children[0].children[1].className.includes("status-approval"), "pending approval lacked its warning state");
const failedOverview = turnOverviewElement({surface:"unknown",state:"error",reasoningSeen:false,permissionIDs:new Set(),tools:new Map(),lifecycle:[{state:"working"},{state:"error"}],firstTimestamp:0,lastTimestamp:0,committed:false});
testAssert(failedOverview.textContent.includes("Provider turn") && failedOverview.textContent.includes("Failed") && failedOverview.textContent.includes("Working → Failed"), "failed provider invocation was misrepresented");
testAssert(!failedOverview.textContent.includes("REQ-") && !failedOverview.textContent.includes("provider_transcript"), "invocation overview exposed internal identifiers");
resetTranscript();
applyActivity(activity(1, "turn_error", {type:"error_upsert",state:"error",text:"The provider is busy with this conversation.",error_source:"provider",error_code:"busy",retryable:true}, null, "PROVIDER-BUSY"));
const providerBusyTurn = transcriptState.byRequest.get("PROVIDER-BUSY");
const providerBusyCard = renderBlockElement(providerBusyTurn.error, "codex");
testAssert(providerBusyTurn.error.errorCode === "busy" && providerBusyTurn.error.retryable && providerBusyCard.textContent.includes("Provider busy") && providerBusyCard.textContent.includes("Retryable"), "provider busy provenance was not visible in the Web conversation");
resetTranscript();
applyActivity(activity(0, "provider_transcript", {type:"user_message",text:"Interrupted native prompt"}, null, "PROVIDER-PARTIAL"));
applyActivity(activity(0, "provider_turn_incomplete", {type:"turn_state",state:"incomplete",stop_reason:"provider_history_incomplete"}, null, "PROVIDER-PARTIAL"));
const providerPartialTurn = transcriptState.byRequest.get("PROVIDER-PARTIAL");
testAssert(providerPartialTurn.state === "incomplete" && turnStatusLabel(providerPartialTurn) === "Incomplete" && turnLifecycleLabel(providerPartialTurn) === "Incomplete", "provider partial history synthesized a complete or live turn");
const stableOverviewTurn = {surface:"acp",state:"working",reasoningSeen:false,permissionIDs:new Set(),tools:new Map(),lifecycle:[{state:"working"}],firstTimestamp:0,lastTimestamp:0,committed:false,overviewElement:null};
const stableOverview = turnOverviewElement(stableOverviewTurn);
stableOverviewTurn.state = "complete";
stableOverviewTurn.lifecycle.push({state:"complete"});
testAssert(turnOverviewElement(stableOverviewTurn) === stableOverview && stableOverview.textContent.includes("Working → Complete"), "stream updates replaced the invocation overview DOM surface");
testAssert(turnDurationLabel({state:"complete",firstTimestamp:0,lastTimestamp:119800}) === "2m 0s", "completed duration rounded to an invalid 60-second remainder");
transcriptState.permissions.clear();

testAssert(
  permissionActionErrorMessage({status:409, code:"worker_upgrade_required", message:"Exact worker recovery instructions"}) === "Exact worker recovery instructions",
  "worker upgrade conflict hid its actionable recovery message"
);
testAssert(
  permissionActionErrorMessage({status:409, code:"session_conflict", message:"internal race detail"}) === "That permission was already decided by another viewer.",
  "ordinary permission conflict exposed transport detail"
);
let upgradeFetchError;
try {
  await apiRequest("/stale-worker", "POST", {});
} catch (error) {
  upgradeFetchError = error;
}
testAssert(
  upgradeFetchError?.status === 409 && upgradeFetchError?.code === "worker_upgrade_required" && upgradeFetchError?.message === "Recover the worker",
  "browser API client discarded the typed worker-upgrade conflict"
);
fetch = async () => ({
  ok:false, status:422,
  async text(){ return JSON.stringify({error:{code:"repository_selection_required",message:"Choose the nested repository",suggested_repo_path:"/projects/site"}}); },
});
let repositoryChoice;
try {
  await apiRequest("/nested-repository", "POST", {});
} catch (error) {
  repositoryChoice = error;
}
testAssert(repositoryChoice?.details?.suggested_repo_path === "/projects/site", "browser API client discarded the repository suggestion");
const repositoryNotice = document.createElement("p");
const repositoryInput = {value:"/projects", focused:false, focus(){this.focused = true;}};
testAssert(offerRepositorySuggestion(repositoryNotice, repositoryInput, repositoryChoice), "repository suggestion was not offered");
testAssert(repositoryInput.value === "/projects" && repositoryNotice.children[1]?.textContent === "Use /projects/site", "repository suggestion silently rewrote the path or hid the explicit action");
repositoryNotice.children[1].listeners.click[0]();
testAssert(repositoryInput.value === "/projects/site" && repositoryInput.focused, "explicit repository choice did not fill and focus the path");
const failedStartResult = {session:{id:"S-FAILED"},warning:{code:"session_start_failed",message:"SECRET provider stderr"}};
testAssert(rememberSessionStartWarning(failedStartResult), "safe partial-Session warning was not retained across navigation");
testAssert(sessionStorageData.get("session-start-warning:S-FAILED") === "session_start_failed", "partial-Session handoff retained raw provider detail");
const failedStartNotice = document.createElement("p");
testAssert(restoreSessionStartWarning("S-FAILED", failedStartNotice), "partial-Session warning was not restored on its Session page");
testAssert(failedStartNotice.textContent.includes("could not start") && !failedStartNotice.textContent.includes("SECRET"), "partial-Session warning was unsafe or unhelpful");
testAssert(!sessionStorageData.has("session-start-warning:S-FAILED"), "partial-Session warning was not one-shot");
testAssert(!rememberSessionStartWarning({session:{id:"S-UNKNOWN"},warning:{code:"provider_raw",message:"SECRET"}}), "unknown partial-Session warning code was retained");
fetch = async () => ({
  ok:false, status:409,
  async text(){ return JSON.stringify({error:{code:"session_conflict",message:"Native harness is running; ACP prompt was not accepted. Exit it, then retry."}}); },
});
resetTranscript();
let nativeConflict;
try {
  await apiRequest("/native-owned-prompt", "POST", {message:"must not be queued"});
} catch (error) {
  nativeConflict = error;
}
testAssert(
  nativeConflict?.status === 409 && nativeConflict?.code === "session_conflict" && nativeConflict?.message.includes("prompt was not accepted"),
  "browser API client hid the actionable native-ownership conflict"
);
testAssert(transcriptState.turns.length === 0, "rejected native-open prompt created an optimistic conversation turn");

resetTranscript();
applyActivity(activity(0, "acp", {type:"legacy_user_message", text:"Legacy injected prompt"}));
applyActivity(activity(1, "prompt_submitted", {type:"user_message", text:"Do the work"}));
applyActivity(activity(2, "acp", {type:"assistant_chunk", message_id:"m", text:"hello "}));
applyActivity(activity(3, "acp", {type:"assistant_chunk", message_id:"m", text:"world"}));
testAssert(transcriptState.turns[0].blocks.filter((block) => block.type === "user").length === 1, "user message duplicated");
testAssert(transcriptState.turns[0].blocks.find((block) => block.type === "user").text === "Do the work", "truthful prompt did not replace legacy fallback");
testAssert(transcriptState.turns[0].blocks.find((block) => block.type === "assistant").text === "hello world", "assistant chunks did not coalesce");
applyActivity(activity(3, "acp", {type:"assistant_chunk", message_id:"m", text:" duplicate"}));
testAssert(transcriptState.turns[0].blocks.find((block) => block.type === "assistant").text === "hello world", "duplicate stream position appended twice");

applyActivity(activity(4, "acp", {type:"tool_upsert", tool:{id:"tool", title:"Read file", status:"in_progress", preview:"safe"}}));
applyActivity(activity(5, "acp", {type:"assistant_chunk", message_id:"m", text:"after tool"}));
testAssert(transcriptState.turns[0].blocks.filter((block) => block.type === "assistant").length === 2, "intervening tool did not split assistant segments");
applyActivity(activity(6, "acp", {type:"tool_upsert", tool:{id:"tool", status:"completed", sensitive:true, detail:"private diff"}}));
const mergedTool = transcriptState.turns[0].tools.get("tool");
testAssert(mergedTool.title === "Read file" && mergedTool.status === "completed", "tool patches did not merge");
testAssert(mergedTool.preview === "", "sensitive tool update retained preview");

applyActivity(activity(7, "acp", {type:"plan_replace", plan:[{content:"First",status:"completed"}]}));
applyActivity(activity(8, "acp", {type:"plan_replace", plan:[]}));
testAssert(transcriptState.turns[0].plan.entries.length === 0, "empty plan update did not clear plan");

for (let position = 9; position < 10009; position += 1) {
  applyActivity(activity(position, "acp", {type:"assistant_chunk", message_id:"long", text:"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}));
}
const assistantBlocks = transcriptState.turns[0].blocks.filter((block) => block.type === "assistant");
const longBlock = assistantBlocks[assistantBlocks.length - 1];
testAssert(transcriptState.turns[0].assistantBytes <= LIMITS.assistant && transcriptState.turns[0].contentClipped, "assistant cumulative limit failed");
applyActivity(activity(10009, "turn_completed", {type:"turn_state", state:"complete", stop_reason:"end_turn"}));
testAssert(transcriptState.turns[0].state === "complete", "lifecycle update was lost after clipping");
testAssert(completedResponseBlock(transcriptState.turns[0]) === longBlock, "completed response was not identified from the final ACP assistant segment");
testAssert(!applyOptimisticPromptState(transcriptState.turns[0], "queued") && transcriptState.turns[0].state === "complete", "HTTP receipt regressed streamed terminal state");

applyActivity(activity(10010, "permission_requested", {type:"permission", state:"waiting"}, {id:"permission", state:"waiting", options:[{id:"allow",name:"Allow once",kind:"allow_once"}]}));
applyActivity(activity(10011, "permission_resolved", {type:"permission", state:"resolved"}, {id:"permission", state:"resolved", decision:"allow_once"}));
const permission = transcriptState.permissions.get("permission");
testAssert(permission.state === "resolved" && permission.options.length === 1, "permission resolution lost offered options");

resetTranscript();
applyActivity(activity(1, "worker_started", null));
applyActivity(activity(2, "notice", null));
testAssert(transcriptState.turns.length === 0, "diagnostic-only activity created empty turns");

resetTranscript();
applyActivity(activity(1, "acp", {type:"assistant_chunk", message_id:"exact", text:"x".repeat(LIMITS.assistant)}));
let exact = transcriptState.turns[0].blocks[0];
testAssert(exact.bytes === LIMITS.assistant && !blockWasClipped(exact), "exact assistant limit was falsely clipped");
applyActivity(activity(2, "acp", {type:"assistant_chunk", message_id:"exact", text:"x"}));
testAssert(exact.leadingClipped && exact.text.endsWith("x"), "assistant overflow did not preserve and mark the newest output");

resetTranscript();
applyActivity(activity(1, "acp", {type:"thought_chunk", text:"reasoning A"}));
applyActivity(activity(2, "acp", {type:"assistant_chunk", message_id:"commentary", text:"commentary"}));
applyActivity(activity(3, "acp", {type:"tool_upsert", tool:{id:"ordered-tool", title:"Inspect", status:"completed"}}));
applyActivity(activity(4, "acp", {type:"thought_chunk", text:"reasoning B"}));
applyActivity(activity(5, "acp", {type:"assistant_chunk", message_id:"response", text:"FINAL-RESPONSE"}));
applyActivity(activity(6, "turn_completed", {type:"turn_state", state:"complete", stop_reason:"end_turn"}));
const ordered = transcriptState.turns[0];
testAssert(ordered.blocks.map((block) => block.type).join(",") === "thought,assistant,tool,thought,assistant", "interleaved ACP reasoning chronology was collapsed");
testAssert(ordered.blocks.filter((block) => block.type === "thought").map((block) => block.text).join("|") === "reasoning A|reasoning B", "separate reasoning phases were merged");
testAssert(completedResponseBlock(ordered)?.text === "FINAL-RESPONSE", "final ACP assistant segment was not presented as the completed response");
testAssert(turnHasReasoning(ordered), "reasoning availability was not detected");

resetTranscript();
applyActivity(activity(1, "acp", {type:"assistant_chunk", message_id:"verbose", text:"a".repeat(LIMITS.assistant)}));
applyActivity(activity(2, "acp", {type:"assistant_chunk", message_id:"verbose", text:"FINAL-MARKER"}));
const rollingAssistant = transcriptState.turns[0].blocks.find((block) => block.type === "assistant");
testAssert(rollingAssistant.text.endsWith("FINAL-MARKER") && rollingAssistant.leadingClipped, "verbose ACP output dropped the newest response marker");
testAssert(transcriptState.turns[0].assistantBytes <= LIMITS.assistant, "rolling assistant budget exceeded its bound");

resetTranscript();
applyActivity(activity(1, "acp", {type:"thought_chunk", text:"r".repeat(LIMITS.thought)}));
applyActivity(activity(2, "acp", {type:"assistant_chunk", message_id:"separator", text:"status"}));
applyActivity(activity(3, "acp", {type:"thought_chunk", text:"TAIL-REASONING"}));
const rollingThoughts = transcriptState.turns[0].blocks.filter((block) => block.type === "thought");
testAssert(rollingThoughts.some((block) => block.text.endsWith("TAIL-REASONING")), "reasoning budget dropped the newest ACP thought marker");
testAssert(transcriptState.turns[0].thoughtBytes <= LIMITS.thought && transcriptState.turns[0].contentClipped, "rolling reasoning budget exceeded its bound or hid truncation");

resetTranscript();
for (let index = 0; index < LIMITS.blocksPerTurn / 2; index += 1) {
  applyActivity(activity((index * 2) + 1, "acp", {type:"assistant_chunk", message_id:"progress-" + index, text:"progress"}));
  applyActivity(activity((index * 2) + 2, "acp", {type:"thought_chunk", text:"reasoning"}));
}
applyActivity(activity(LIMITS.blocksPerTurn + 1, "acp", {type:"assistant_chunk", message_id:"response", text:"UNIQUE-FINAL"}));
applyActivity(activity(LIMITS.blocksPerTurn + 2, "turn_completed", {type:"turn_state", state:"complete", stop_reason:"end_turn"}));
const saturated = transcriptState.turns[0];
testAssert(saturated.blocks.length <= LIMITS.blocksPerTurn, "rolling text slot exceeded the per-turn block cap");
testAssert(completedResponseBlock(saturated)?.text === "UNIQUE-FINAL", "block cap discarded or misidentified the newest completed response");
testAssert(saturated.contentClipped, "block-cap eviction was not disclosed");

resetTranscript();
applyActivity(activity(1, "acp", {type:"thought_chunk", text:"ONLY-REASONING"}));
for (let index = 0; index < 255; index += 1) {
  applyActivity(activity((index * 2) + 2, "acp", {type:"assistant_chunk", message_id:"progress-" + index, text:"progress"}));
  applyActivity(activity((index * 2) + 3, "acp", {type:"tool_upsert", tool:{id:"saturation-tool-" + index, title:"Inspect", status:"completed"}}));
}
applyActivity(activity(512, "acp", {type:"plan_replace", plan:[{content:"Saturate display slots",status:"completed"}]}));
applyActivity(activity(513, "acp", {type:"assistant_chunk", message_id:"response", text:"FINAL"}));
applyActivity(activity(514, "turn_completed", {type:"turn_state", state:"complete", stop_reason:"end_turn"}));
const evictedReasoning = transcriptState.turns[0];
testAssert(!evictedReasoning.blocks.some((block) => block.type === "thought"), "reasoning saturation fixture did not evict its oldest thought block");
testAssert(turnHasReasoning(evictedReasoning), "bounded eviction falsely claimed no reasoning was exposed");
testAssert(evictedReasoning.assistantSeen && completedResponseBlock(evictedReasoning)?.text === "FINAL", "assistant-seen state or final response was lost at saturation");

resetTranscript();
applyActivity(activity(1, "acp", {type:"assistant_chunk", message_id:"plain", text:"answer"}));
applyActivity(activity(2, "turn_completed", {type:"turn_state", state:"complete", stop_reason:"end_turn"}));
testAssert(!turnHasReasoning(transcriptState.turns[0]), "provider-without-thought turn falsely advertised reasoning");

resetTranscript();
for (let position = 1; position <= 2000; position += 1) {
  applyActivity(activity(position, "acp", {type:"tool_upsert", tool:{id:"tool-" + position, title:"Tool", status:"completed", detail:"x".repeat(2048)}}));
}
testAssert(transcriptState.turns[0].blocks.length <= LIMITS.blocksPerTurn, "turn block bound failed");
testAssert(transcriptState.turns[0].tools.size <= LIMITS.toolsPerTurn, "tool count bound failed");
testAssert(transcriptState.turns[0].toolBytes <= LIMITS.toolTotal, "cumulative tool detail bound failed");
testAssert(transcriptState.diagnostics.length <= LIMITS.diagnostics, "diagnostic bound failed");
for (let index = 0; index < 400; index += 1) {
  applyActivity(activity(2001 + index, "turn_started", {type:"turn_state", state:"working"}, null, "REQ-" + index));
}
testAssert(transcriptState.turns.length <= LIMITS.turns && transcriptState.byRequest.size <= LIMITS.turns, "turn/request bound failed");
testAssert(transcriptState.historyClipped, "bounded history was not disclosed");

resetTranscript();
const fullMessage = "y".repeat(LIMITS.assistant);
for (let index = 0; index < 180; index += 1) {
  applyActivity(activity(index + 1, "acp", {type:"assistant_chunk", message_id:"message", text:fullMessage}, null, "BIG-" + index));
}
testAssert(transcriptState.retainedBytes <= LIMITS.retained, "global transcript byte budget failed");
testAssert(transcriptState.turns.length < 180 && transcriptState.historyClipped, "global transcript budget did not evict old display state");

resetTranscript();
for (let index = 0; index < 120; index += 1) {
  const requestID = "PENDING-BIG-" + index;
  const base = 1 + (index * 3);
  applyActivity(activity(base, "acp", {type:"assistant_chunk", message_id:"message", text:"a".repeat(LIMITS.assistant)}, null, requestID));
  applyActivity(activity(base + 1, "acp", {type:"thought_chunk", text:"t".repeat(LIMITS.thought)}, null, requestID));
  applyActivity(activity(base + 2, "permission_requested", {type:"permission", state:"waiting"}, {id:"WAIT-" + index, state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]}, requestID));
}
testAssert(transcriptState.retainedBytes <= LIMITS.retained, "waiting permissions bypassed the transcript byte budget");
testAssert(transcriptState.permissions.size === 120, "evicting heavy turn display discarded actionable permissions");
testAssert(transcriptState.turns.length < 120, "waiting permission turns were not evicted from heavy display state");

resetTranscript();
for (let index = 0; index <= LIMITS.permissions; index += 1) {
  applyActivity(activity(index + 1, "permission_requested", {type:"permission", state:"waiting"}, {id:"P-" + index, state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]}, "PERM-" + index));
}
testAssert(transcriptState.permissions.has("P-0") && !transcriptState.permissions.has("P-" + LIMITS.permissions), "browser permission overflow was not rejected safely");
testAssert(transcriptState.permissions.size === LIMITS.permissions, "browser permission limit was exceeded");
testAssert(transcriptState.permissionOverflow, "permission overflow was not flagged");
transcriptState.permissions.set("P-0", {...transcriptState.permissions.get("P-0"), state:"resolved"});
applyActivity(activity(LIMITS.permissions + 2, "permission_requested", {type:"permission", state:"waiting"}, {id:"P-next", state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]}, "PERM-next"));
testAssert(!transcriptState.permissions.has("P-0") && transcriptState.permissions.has("P-next"), "resolved permission did not make room for a new actionable request");
testAssert(!transcriptState.permissionOverflow, "permission overflow did not clear after safe capacity became available");

resetTranscript();
for (let index = 0; index < LIMITS.permissions * 3; index += 1) {
  const id = "SEQUENTIAL-" + index;
  applyActivity(activity((index * 2) + 1, "permission_requested", {type:"permission", state:"waiting"}, {id, state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]}, "SEQ-" + index));
  applyActivity(activity((index * 2) + 2, "permission_resolved", {type:"permission", state:"resolved"}, {id, state:"resolved", decision:"reject_once"}, "SEQ-" + index));
}
testAssert(transcriptState.permissions.size <= LIMITS.permissions, "sequential resolved permissions exceeded the browser cap");
testAssert(transcriptState.permissions.has("SEQUENTIAL-" + ((LIMITS.permissions * 3) - 1)), "latest sequential permission was dropped");
testAssert(!transcriptState.permissionOverflow, "resolved permission history falsely triggered overflow");

resetTranscript();
const authoritative = [];
for (let index = 0; index < LIMITS.permissions; index += 1) {
  const permission = {id:"FULL-" + index, state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]};
  applyActivity(activity(index + 1, "permission_requested", {type:"permission", state:"waiting"}, permission, "FULL-REQ-" + index));
  if (index > 0) authoritative.push(permission);
}
authoritative.push({id:"FULL-new", state:"waiting", options:[{id:"reject",name:"Reject",kind:"reject_once"}]});
testAssert(reconcilePermissionSnapshot(authoritative), "authoritative exact-cap replacement reported no change");
testAssert(transcriptState.permissions.has("FULL-new") && transcriptState.permissions.get("FULL-0")?.state !== "waiting", "authoritative exact-cap replacement hid the new approval");
testAssert(transcriptState.permissions.size <= LIMITS.permissions && !transcriptState.permissionOverflow, "authoritative replacement left the browser overflowed");

streamAnnouncementsReady = false;
pendingConversationAnnouncement = "";
pendingConversationStatus = "";
conversationAnnouncementScheduled = false;
noteStreamActivity(activity(1, "acp", {type:"assistant_chunk", text:"replayed"}));
testAssert(pendingConversationAnnouncement === "", "history replay was announced as live output");
streamAnnouncementsReady = true;
noteStreamActivity(activity(2, "acp", {type:"assistant_chunk", text:"z".repeat(4096)}));
noteStreamActivity(activity(3, "turn_completed", {type:"turn_state", state:"complete"}));
testAssert(textEncoder.encode(pendingConversationAnnouncement).length <= LIMITS.announcement, "conversation announcement buffer exceeded its bound");
testAssert(pendingConversationStatus.includes("complete"), "terminal response announcement was dropped after long output");
noteStreamActivity(activity(4, "turn_started", {type:"turn_state", state:"working"}, null, "REQ-next"));
testAssert(pendingConversationStatus.includes("complete") && pendingConversationStatus.includes("working"), "next queued turn erased the prior completion announcement");

resetTranscript();
turnFor({request_id:"new", position:Number.POSITIVE_INFINITY});
applyActivity(activity(1, "turn_started", {type:"turn_state", state:"working"}, null, "old"));
testAssert(orderedTurns()[0].id === "old", "late history replay was ordered after an optimistic prompt");

resetTranscript();
applyActivity(activity(1, "prompt_submitted", {type:"user_message",text:"send while native is open"}, null, "READY-NATIVE"));
applyActivity(activity(2, "prompt_ready", {type:"turn_state",state:"ready"}, null, "READY-NATIVE"));
const readyNativeTurn = transcriptState.byRequest.get("READY-NATIVE");
testAssert(readyNativeTurn?.state === "ready" && turnStatusLabel(readyNativeTurn) === "Accepted", "ready ACP request was presented as queued");
testAssert(turnLifecycleLabel(readyNativeTurn) === "Accepted" && !turnLifecycleLabel(readyNativeTurn).includes("Queued"), "ready ACP lifecycle invented queueing");
resetTranscript();
const optimisticReadyTurn = turnFor({request_id:"OPTIMISTIC-READY", position:Number.POSITIVE_INFINITY});
setRuntimeStatus("waiting");
testAssert(applyOptimisticPromptReceipt(optimisticReadyTurn, "ready"), "optimistic ready state was not applied");
testAssert(optimisticReadyTurn.state === "ready" && turnStatusLabel(optimisticReadyTurn) === "Accepted" && turnLifecycleLabel(optimisticReadyTurn) === "Accepted", "API ready receipt invented provider work before turn_started");
testAssert(currentSessionRuntimeStatus === "waiting", "API ready receipt fabricated a Working Session before turn_started");
testAssert(sessionActionVisible("cancel", ["starting", "working"], "waiting"), "Accepted prompt preparation did not expose Stop response while runtime status was still waiting");
testAssert(!sessionActionVisible("stop", ["stopped"], "waiting"), "Accepted prompt preparation exposed an unrelated Session action");
optimisticReadyTurn.state = "cancelled";
testAssert(!sessionActionVisible("cancel", ["starting", "working"], "waiting"), "terminal local prompt kept the preparation Stop response visible");

resetTranscript();
transcriptState.lastPosition = 99;
attachActivityStream("/api/v1/sessions/S/stream");
testAssert(createdEventSources[0] === "/api/v1/sessions/S/stream?seed=99", "stream reopen did not use a one-time cursor seed");

const form = {dataset:{}};
testAssert(beginFormSubmission(form), "first form submission was rejected");
testAssert(!beginFormSubmission(form), "duplicate form submission was accepted");
endFormSubmission(form);
testAssert(beginFormSubmission(form), "form did not become reusable after completion");

resetTranscript();
const committedSnapshot = {
  source:"provider-store", checkpoint:"pt1-three-turns", gateway_cursor:40,
  turns:[
    {key:"u1",committed:true,surface:"acp",activities:[activity(0,"provider_transcript",{type:"user_message",text:"ACP one"},null,"REQ-1","2026-08-16T00:00:00Z"),activity(1,"prompt_queued",{type:"turn_state",state:"queued"},null,"REQ-1","2026-08-16T00:00:00Z"),activity(2,"turn_started",{type:"turn_state",state:"working"},null,"REQ-1","2026-08-16T00:00:01Z"),activity(0,"provider_transcript",{type:"assistant_chunk",text:"ONE"},null,"REQ-1","2026-08-16T00:00:03Z"),activity(3,"turn_completed",{type:"turn_state",state:"complete"},null,"REQ-1","2026-08-16T00:00:04Z")]},
    {key:"u2",committed:true,surface:"native",activities:[activity(0,"provider_transcript",{type:"user_message",text:"Native middle"},null,"provider-2","2026-08-16T00:01:00Z"),activity(0,"provider_transcript",{type:"thought_chunk",text:"native reasoning"},null,"provider-2","2026-08-16T00:01:01Z"),activity(0,"provider_transcript",{type:"assistant_chunk",text:"TWO"},null,"provider-2","2026-08-16T00:01:02.2Z"),activity(0,"provider_turn_completed",{type:"turn_state",state:"complete"},null,"provider-2","")]},
    {key:"u3",committed:true,surface:"acp",activities:[activity(0,"provider_transcript",{type:"user_message",text:"ACP three"}),activity(0,"provider_transcript",{type:"assistant_chunk",text:"THREE"}),activity(0,"provider_turn_completed",{type:"turn_state",state:"complete"})]}
  ], diagnostics:[]
};
const savedOnlyCandidate = buildConversationSnapshot(committedSnapshot, []);
const savedOnlySummary = conversationHistorySummary(savedOnlyCandidate);
testAssert(savedOnlyCandidate.turns.length === 3 && savedOnlyCandidate.turns.every((turn) => turn.committed), "ACP-native-ACP provider history was not fully loaded");
testAssert(savedOnlyCandidate.turns[1].blocks.some((block) => block.type === "user" && block.text === "Native middle"), "native middle turn disappeared from the structured conversation");
testAssert(savedOnlyCandidate.turns.map((turn) => turn.surface).join(",") === "acp,native,acp", "turn invocation surfaces were not preserved");
const savedInvocationOverviews = savedOnlyCandidate.turns.map((turn) => turnOverviewElement(turn).textContent);
testAssert(savedInvocationOverviews[0].includes("ACP turn") && savedInvocationOverviews[0].includes("Queued → Working → Complete") && savedInvocationOverviews[0].includes("4.0s"), "saved ACP invocation lifecycle was hidden");
testAssert(savedInvocationOverviews[1].includes("Native CLI turn") && savedInvocationOverviews[1].includes("Provider history → Complete") && savedInvocationOverviews[1].includes("2.2s"), "saved native invocation lifecycle was hidden");
testAssert(savedOnlyCandidate.turns[0].overviewElement.attributes["aria-label"].includes("Duration 4.0 seconds"), "completed invocation timing was omitted from its accessible label");
testAssert(savedOnlySummary.short === "3 saved turns" && !savedOnlySummary.bounded, "complete three-turn history was not disclosed as fully loaded");
const candidate = buildConversationSnapshot(committedSnapshot, [activity(41, "turn_started", {type:"turn_state",state:"working"}, null, "LIVE")]);
testAssert(candidate.source === "provider-store" && candidate.checkpoint === "pt1-three-turns", "provider snapshot identity was not retained");
testAssert(candidate.turns.length === 4 && candidate.turns[1].blocks.some((block) => block.type === "thought" && block.text === "native reasoning"), "provider ACP-native-ACP history was not grouped in order");
testAssert(candidate.turns[1].blocks.filter((block) => ["thought","assistant"].includes(block.type)).every((block) => block.source === "provider"), "provider-backed blocks lost their stored-history provenance");
testAssert(candidate.lastPosition === 41 && candidate.byRequest.get("LIVE")?.state === "working", "live activity crossing snapshot capture was lost");
testAssert(candidate.turns.slice(0, 3).every((turn) => turn.state === "complete" && turn.committed), "committed provider turns were not marked complete");
testAssert(!candidate.byRequest.get("LIVE").committed, "buffered ACP turn was falsely marked provider-committed");

resetTranscript();
for (let toolIndex = 0; toolIndex < 36; toolIndex += 1) {
  for (let patchIndex = 0; patchIndex < 5; patchIndex += 1) {
    applyActivity(activity(toolIndex * 5 + patchIndex + 1, "acp", {type:"tool_upsert",tool:{id:"tool-" + toolIndex,title:"Tool " + toolIndex,status:patchIndex === 4 ? "completed" : "in_progress"}}));
  }
}
const richToolTurn = transcriptState.turns[0];
const richToolOverview = turnOverviewElement(richToolTurn);
testAssert(richToolTurn.tools.size === 36 && richToolOverview.textContent.includes("36 tools"), "tool update patches were not reported as logical invocations");

const providerReasoningCard = thoughtElement({source:"provider",text:"First stored line\nSecond stored line",revision:1}, {streaming:false});
testAssert(providerReasoningCard.open, "stored provider reasoning was not open by default");
testAssert(providerReasoningCard.className.includes("provider-history"), "stored reasoning card lacked unmistakable provider-history styling");
testAssert(providerReasoningCard.children[0].className === "thought-summary", "reasoning card lacked a structured visible header");
testAssert(providerReasoningCard.textContent.includes("Saved"), "reasoning card did not expose its saved state");
testAssert(providerReasoningCard.textContent.includes("2 lines") && providerReasoningCard.textContent.includes("6 words"), "reasoning card did not summarize its readable content");
const liveReasoningCard = thoughtElement({source:"acp",text:"Streaming thought",revision:1}, {streaming:true});
testAssert(liveReasoningCard.className.includes("live-acp"), "live ACP reasoning card lacked distinct source styling");
testAssert(liveReasoningCard.textContent.includes("Live"), "live reasoning state was not visible");
testAssert(providerReasoningCard.tagName === "DETAILS" && providerReasoningCard.children[0].tagName === "SUMMARY", "reasoning disclosure lost native keyboard semantics");
providerReasoningCard.open = false;
providerReasoningCard.dataset.userToggled = "true";
const reconciledReasoningBlock = {type:"thought",source:"provider",text:"First stored line\nSecond stored line",revision:2,renderedRevision:1,renderedContext:"acp:stored:update",element:providerReasoningCard};
const reconciledReasoningCard = patchBlockElement(reconciledReasoningBlock, "grok", {streaming:false});
testAssert(!reconciledReasoningCard.open && reconciledReasoningCard.dataset.userToggled === "true", "provider reconciliation discarded the user's reasoning disclosure state");

const provisional = turnFor({request_id:"PROVISIONAL",position:42});
const provisionalAssistantElement = {id:"stable-assistant"};
provisional.blocks.push(
  {type:"user",text:"ACP three",bytes:9,revision:1},
  {type:"assistant",text:"EE",bytes:2,revision:1,element:provisionalAssistantElement,renderedRevision:1,renderedContext:"stored:final"}
);
provisional.element = {id:"stable-turn"};
candidate.turns[2].element = null;
adoptConversationPresentation(transcriptState, candidate);
testAssert(candidate.turns[2].element === provisional.element, "provider reconciliation did not preserve a matching provisional turn surface");
const committedAssistant = candidate.turns[2].blocks.find((block) => block.type === "assistant");
testAssert(committedAssistant.element === provisionalAssistantElement, "provider reconciliation discarded the stable assistant DOM surface");
testAssert(committedAssistant.renderedRevision === -1, "changed provider text reused a stale provisional render revision");

const repeatedOldTurns = [
  {id:"OLD-A",blocks:[{type:"user",text:"Repeat"},{type:"assistant",text:"partial"}]},
  {id:"OLD-B",blocks:[{type:"user",text:"Repeat"},{type:"assistant",text:"partial"}]},
];
const repeatedCandidate = {id:"PROVIDER",blocks:[{type:"user",text:"Repeat"},{type:"assistant",text:"completed partial"}]};
testAssert(matchingOldTurn(repeatedCandidate, repeatedOldTurns, new Set()) === null, "ambiguous repeated turns reused the wrong DOM surface");
testAssert(matchingOldBlock(
  {type:"assistant",text:"completed partial"},
  [{type:"assistant",text:"partial"},{type:"assistant",text:"partial"}],
  new Set()
) === null, "ambiguous repeated blocks reused the wrong DOM surface");

const permissionPrevious = newTranscriptState();
for (let index = 0; index < LIMITS.permissions; index += 1) {
  permissionPrevious.permissions.set("OLD-WAIT-" + index, {id:"OLD-WAIT-" + index,state:"waiting"});
}
const permissionCandidate = newTranscriptState();
permissionCandidate.permissions.set("CURRENT-WAIT", {id:"CURRENT-WAIT",state:"waiting"});
adoptConversationPresentation(permissionPrevious, permissionCandidate);
testAssert(permissionCandidate.permissions.size === LIMITS.permissions, "atomic snapshot adoption exceeded the permission bound");
testAssert(permissionCandidate.permissionOverflow && permissionCandidate.historyClipped, "permission adoption overflow was not disclosed");

transcriptState.shouldStick = false;
let anchorTop = 40;
const anchorElement = {
	  isConnected:true,
	  getBoundingClientRect(){ return {top:anchorTop,bottom:anchorTop + 80}; }
};
	const anchorBlock = {type:"assistant",element:anchorElement};
	const savedTurns = transcriptState.turns;
	transcriptState.turns = [{firstPosition:1,blocks:[anchorBlock]}];
const fakeScroller = {
  scrollTop:120,
  style:{scrollBehavior:"smooth"},
  getBoundingClientRect(){ return {top:0,bottom:300}; }
};
const scrollAnchor = captureConversationScrollAnchor(fakeScroller, {children:[anchorElement]});
anchorTop = 85;
	anchorElement.isConnected = false;
	anchorBlock.element = {
	  isConnected:true,
	  getBoundingClientRect(){ return {top:anchorTop,bottom:anchorTop + 80}; }
	};
restoreConversationScrollAnchor(fakeScroller, scrollAnchor, 120);
testAssert(fakeScroller.scrollTop === 165, "atomic reconciliation did not preserve the visible content anchor");
testAssert(fakeScroller.style.scrollBehavior === "smooth", "scroll-anchor restoration changed the viewer's scroll behavior");
	transcriptState.turns = savedTurns;

const readinessApiRequest = apiRequest;
apiRequest = async () => ({
  permissions:[{id:"UPGRADE-PERMISSION",state:"waiting",title:"Write scratch test",options:[{id:"allow",name:"Allow once",kind:"allow_once"}]}],
  decisions_ready:false,
  blocked_code:"worker_upgrade_required",
  blocked_message:"Exit agent-S, then stop and resume this Session."
});
testAssert(await refreshPermissions(), "permission readiness refresh did not report a presentation change");
testAssert(!permissionDecisionsReady && permissionDecisionBlockedCode === "worker_upgrade_required" && permissionDecisionBlockedMessage.includes("stop and resume"), "stale worker permission controls were left actionable");
apiRequest = readinessApiRequest;

const originalApiRequest = apiRequest;
const gatewaySnapshot = {
  source:"gateway-log", gateway_cursor:17, warning:"Provider history is not committed yet; showing live gateway history.",
  turns:[
    {key:"GATEWAY-REQ",committed:false,activities:[
      activity(11,"prompt_submitted",{type:"user_message",text:"Gateway prompt"},null,"GATEWAY-REQ"),
      activity(12,"acp",{type:"thought_chunk",text:"Gateway reasoning"},null,"GATEWAY-REQ"),
      activity(13,"acp",{type:"assistant_chunk",text:"Gateway answer"},null,"GATEWAY-REQ"),
      activity(14,"turn_completed",{type:"turn_state",state:"complete"},null,"GATEWAY-REQ")
    ]}
  ], diagnostics:[]
};
resetTranscript();
conversationSnapshotSignature = "";
conversationReconcileInFlight = false;
conversationReconcilePending = false;
conversationReconcileBuffer = [];
providerAuthorityRetryTimer = 0;
scheduledTimeouts.length = 0;
apiRequest = async () => gatewaySnapshot;
testAssert(await reconcileConversation("S", {initial:true}), "initial gateway fallback snapshot was not adopted");
testAssert(transcriptState.source === "gateway-log" && transcriptState.lastPosition === 17, "initial gateway fallback lost its source or captured cursor");
testAssert(transcriptState.byRequest.get("GATEWAY-REQ")?.blocks.some((block) => block.type === "assistant" && block.text === "Gateway answer"), "initial gateway fallback lost its bounded semantic turn");
testAssert(scheduledTimeouts.includes(10000) && providerAuthorityRetryTimer !== 0, "idle-capable gateway fallback did not schedule an independent provider-authority retry");
const gatewayStreamCount = createdEventSources.length;
attachActivityStream("/api/v1/sessions/S/stream");
testAssert(createdEventSources[gatewayStreamCount] === "/api/v1/sessions/S/stream?seed=17", "initial gateway fallback did not seed SSE after its captured cursor");

let resolveConversationRequest;
apiRequest = () => new Promise((resolve) => { resolveConversationRequest = resolve; });
resetTranscript();
transcriptState.shouldStick = false;
draftTextarea.value = "keep this draft";
conversationSnapshotSignature = "";
conversationReconcileInFlight = false;
conversationReconcilePending = false;
conversationReconcileBuffer = [];
scheduledTimeouts.length = 0;
const inFlightReconcile = reconcileConversation("S");
conversationReconcileBuffer.push(
  activity(40, "acp", {type:"assistant_chunk",text:"stale"}, null, "STALE"),
  activity(41, "turn_started", {type:"turn_state",state:"working"}, null, "LIVE-RACE"),
  activity(42, "acp", {type:"assistant_chunk",message_id:"live-rehydrate",text:"Provisional ACP output"}, null, "LIVE-RACE")
);
const coalescedReconcile = reconcileConversation("S");
resolveConversationRequest(committedSnapshot);
await Promise.all([inFlightReconcile, coalescedReconcile]);
testAssert(transcriptState.turns.length === 4 && transcriptState.byRequest.has("LIVE-RACE"), "SSE event crossing provider reload was lost");
testAssert(transcriptState.byRequest.get("LIVE-RACE")?.blocks.some((block) => block.type === "assistant" && block.text === "Provisional ACP output"), "provider reload dropped provisional ACP output");
testAssert(!transcriptState.byRequest.has("STALE"), "provider reload replayed an event at the captured gateway cursor");
testAssert(!transcriptState.shouldStick && draftTextarea.value === "keep this draft", "provider reload changed scroll intent or the composer draft");
testAssert(scheduledTimeouts.includes(120), "overlapping provider reload was not coalesced after the active fetch");

let resolveOverflowRequest;
apiRequest = () => new Promise((resolve) => { resolveOverflowRequest = resolve; });
resetTranscript();
transcriptState.shouldStick = false;
draftTextarea.value = "keep overflow draft";
conversationSnapshotSignature = "";
conversationReconcileInFlight = false;
conversationReconcilePending = false;
conversationReconcileBuffer = [];
conversationReconcileBufferBytes = 0;
conversationReconcileOverflow = false;
scheduledTimeouts.length = 0;
const overflowReconcile = reconcileConversation("S");
for (let index = 0; index <= LIMITS.reconcileEvents; index += 1) {
  const live = activity(1000 + index, "acp", {type:"assistant_chunk",message_id:"overflow-stream",text:"x"}, null, "OVERFLOW-LIVE");
  bufferConversationActivity(live);
  applyActivity(live);
}
resolveOverflowRequest(committedSnapshot);
await overflowReconcile;
testAssert(conversationReconcileBuffer.length === 0 && conversationReconcileBufferBytes === 0, "overflowed reconciliation buffer was retained");
testAssert(transcriptState.byRequest.has("OVERFLOW-LIVE") && !transcriptState.byRequest.has("REQ-1"), "overflow adopted a snapshot that omitted live cursor positions");
testAssert(transcriptState.sourceWarning.includes("live activity was busy") && scheduledTimeouts.includes(120), "overflow did not preserve live state and schedule a clean retry");
testAssert(!transcriptState.shouldStick && draftTextarea.value === "keep overflow draft", "overflow retry changed scroll intent or composer draft");

conversationSnapshotSignature = snapshotSignature(committedSnapshot);
transcriptState.sourceWarning = "Provider history refresh is delayed; live ACP output remains current.";
apiRequest = async () => committedSnapshot;
await reconcileConversation("S");
testAssert(transcriptState.sourceWarning === "", "successful unchanged provider snapshot left a stale refresh warning");
const turnsBeforeReconcileWarning = transcriptState.turns;
providerAuthorityRetryTimer = 0;
scheduledTimeouts.length = 0;
testAssert(noteProviderTranscriptReconcileWarning("provider_transcript_reconcile_warning", "S"), "provider transcript warning was not recognized");
testAssert(transcriptState.turns === turnsBeforeReconcileWarning && transcriptState.sourceWarning.includes("delayed"), "provider transcript warning replaced conversation state or was not disclosed");
testAssert(scheduledTimeouts.includes(10000), "provider transcript reconciliation fallback did not schedule an independent authority retry");
testAssert(shouldReconcileConversationKind("provider_transcript_reconciled"), "provider transcript reconciliation does not trigger a committed history reload");
testAssert(shouldReconcileConversationKind("acp_rehydrated"), "ACP rehydration does not trigger a committed provider history reload");
testAssert(!shouldReconcileConversationKind("provider_transcript_reconcile_warning"), "provider transcript warning incorrectly replaced the current transcript");

const turnsBeforeFallbackRefresh = transcriptState.turns;
const cursorBeforeFallbackRefresh = transcriptState.lastPosition;
providerAuthorityRetryTimer = 0;
scheduledTimeouts.length = 0;
apiRequest = async () => ({...gatewaySnapshot, gateway_cursor:999, turns:[{key:"STALE-FALLBACK",committed:false,activities:[activity(998,"acp",{type:"assistant_chunk",text:"must not replace current view"},null,"STALE-FALLBACK")]}]});
await reconcileConversation("S");
testAssert(transcriptState.turns === turnsBeforeFallbackRefresh && transcriptState.lastPosition === cursorBeforeFallbackRefresh, "later provider-store fallback replaced the current provider/live view");
testAssert(transcriptState.source === "provider-store" && !transcriptState.byRequest.has("STALE-FALLBACK"), "later fallback downgraded provider authority or injected stale history");
testAssert(scheduledTimeouts.includes(10000) && providerAuthorityRetryTimer !== 0, "later native/provider fallback did not keep polling for restored provider authority");

resetTranscript();
conversationSnapshotSignature = "";
conversationReconcileInFlight = false;
conversationReconcilePending = false;
providerAuthorityRetryTimer = 0;
scheduledTimeouts.length = 0;
apiRequest = async () => { throw new Error("provider authority temporarily unavailable"); };
await reconcileConversation("S", {initial:true});
testAssert(transcriptState.turns.length === 0 && scheduledTimeouts.includes(10000), "empty idle provider-authority failure had no background recovery path");

resetTranscript();
applyActivity(activity(1, "turn_started", {type:"turn_state",state:"working"}, null, "RETRY"));
applyActivity(activity(2, "turn_completed", {type:"turn_state",state:"complete"}, null, "RETRY"));
conversationReconcileAttempts = 0;
scheduledTimeouts.length = 0;
apiRequest = async () => { throw new Error("temporary provider-store failure"); };
await reconcileConversation("S");
testAssert(transcriptState.sourceWarning.includes("delayed"), "transient provider failure was not disclosed without replacing live ACP output");
testAssert(scheduledTimeouts.some((delay) => delay === 220), "transient provider failure did not schedule a commit retry");
conversationReconcileAttempts = 6;
scheduledTimeouts.length = 0;
scheduleProviderCommitRetry("S");
testAssert(scheduledTimeouts.some((delay) => delay === 10000), "late provider commit lost its bounded background retry");
apiRequest = originalApiRequest;
`)

	root := t.TempDir()
	sourceFile := filepath.Join(root, "app.js")
	if err := os.WriteFile(sourceFile, source, 0o600); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(root, "client-reducer-test.js")
	runner := fmt.Sprintf(`
const fs = require("node:fs");
const vm = require("node:vm");
const createdEventSources = [];
const scheduledTimeouts = [];
const sessionStorageData = new Map();
const draftTextarea = {value:""};
class FakeElement {
  constructor(tagName) {
    this.tagName = String(tagName || "").toUpperCase();
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.className = "";
    this.classList = {
      toggle: (name, force) => {
        const classes = new Set(this.className.split(/\s+/).filter(Boolean));
        if (force === undefined ? !classes.has(name) : force) classes.add(name); else classes.delete(name);
        this.className = [...classes].join(" ");
      }
    };
    this.open = false;
    this._textContent = "";
  }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = [...children]; this._textContent = ""; }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  matches(selector) { return selector === "details[data-detail-key]" && this.tagName === "DETAILS" && Boolean(this.dataset.detailKey); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  set textContent(value) { this._textContent = String(value); this.children = []; }
  get textContent() { return this._textContent + this.children.map((child) => child?.textContent ?? child?.data ?? String(child)).join(""); }
}
class FakeEventSource {
  constructor(path) { createdEventSources.push(path); }
  addEventListener() {}
  close() {}
}
const sandbox = {
  TextEncoder, console, EventSource: FakeEventSource, createdEventSources, scheduledTimeouts, sessionStorageData, draftTextarea,
  fetch: async () => ({
    ok:false, status:409,
    async text(){ return JSON.stringify({error:{code:"worker_upgrade_required",message:"Recover the worker"}}); },
  }),
  document: {
    body: {dataset:{page:""}, classList:{add(){},remove(){},contains(){return false;}}},
    createElement(tagName){return new FakeElement(tagName);}, createTextNode(value){return {data:String(value),textContent:String(value)};},
    querySelectorAll(){return [];}, querySelector(){return null;}, getElementById(){return null;}, addEventListener(){},
  },
  window: {
    EventSource:FakeEventSource,
    sessionStorage:{setItem(key,value){sessionStorageData.set(String(key),String(value));},getItem(key){return sessionStorageData.get(String(key)) ?? null;},removeItem(key){sessionStorageData.delete(String(key));}},
    setTimeout(_callback, delay = 0){scheduledTimeouts.push(delay);return scheduledTimeouts.length;}, clearTimeout(){}, setInterval(){return 0;}, addEventListener(){}, matchMedia(){return {matches:false};}
  },
  HTMLDialogElement: class {}, FormData: class {},
};
vm.createContext(sandbox);
const source = fs.readFileSync(%q, "utf8");
const result = vm.runInContext(source + "\n(async () => {\n" + %q + "\n})()", sandbox, {timeout:10000});
Promise.resolve(result).catch((error) => { console.error(error); process.exitCode = 1; });
`, sourceFile, string(harness))
	if err := os.WriteFile(testFile, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, testFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser transcript reducer: %v\n%s", err, output)
	}
}
