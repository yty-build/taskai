package web

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserSessionChannelIsImmediateMechanicalAndBounded(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable; browser client test requires Node")
	}
	source, err := content.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	clientSource := string(source)
	for _, removed := range []string{
		"/conversation", "/sessions/prepare", "/review", "/launch", "provider-options/reconnect",
		"launch_phase", "prepared", "sessionStorage", "reconcileConversation", ".display",
		"reconcileStreamAvailability",
		"assistant_chunk", "thought_chunk", "tool_upsert", "plan_replace", "surface-native", "surface-acp", "committed",
		"addEventListener(\"activity\"", "/permissions/decision", "queued", "Queue message",
	} {
		if strings.Contains(clientSource, removed) {
			t.Fatalf("browser client still contains removed staged or projected contract %q", removed)
		}
	}
	for _, required := range []string{
		"addEventListener(\"frame\"", "provider_event", "text_delta", "tool_call", "provider_result",
		"/provider-options", "/config", "/children", "/prompt", "/permissions/response", `if (action === "resume")`,
		"parent_session_id", "sessionRoleLabel", "child-session",
	} {
		if !strings.Contains(clientSource, required) {
			t.Fatalf("browser client is missing Session Channel contract %q", required)
		}
	}
	streamBootstrap := strings.LastIndex(clientSource, "attachSessionChannel(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);")
	permissionBootstrap := strings.LastIndex(clientSource, "refreshPermissions().then(scheduleTranscriptRender)")
	sessionBootstrap := strings.LastIndex(clientSource, `if (page === "session")`)
	if streamBootstrap < 0 || permissionBootstrap < 0 || sessionBootstrap < 0 || streamBootstrap > permissionBootstrap {
		t.Fatal("Session page does not attach its EventSource before other asynchronous bootstrap work")
	}
	if strings.Contains(clientSource[sessionBootstrap:], "loadProviderOptions(sessionID).catch") {
		t.Fatal("Session bootstrap reads provider options before replay ready and provider-connected boundaries")
	}

	root := t.TempDir()
	sourceFile := filepath.Join(root, "app.js")
	if err := os.WriteFile(sourceFile, source, 0o600); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(root, "client-test.js")
	harness := []byte(`
function frame(seq, type, data = {}, requestID = "REQ", timestamp = "") {
  return {session_id:"S",seq,request_id:requestID,type,timestamp,data};
}
function reset() {
  transcriptState = newTranscriptState();
  permissionDecisionsReady = false;
  permissionDecisionBlockedCode = "";
  permissionDecisionBlockedMessage = "";
  streamAnnouncementsReady = false;
  pendingConversationAnnouncement = "";
  pendingConversationStatus = "";
  conversationAnnouncementScheduled = false;
}

const optionForm = document.createElement("form");
const model = document.createElement("select");
const effort = document.createElement("select");
const modelBadge = document.createElement("span");
const effortBadge = document.createElement("span");
const optionPanel = document.createElement("div");
const optionStatus = document.createElement("p");
const optionSubmit = document.createElement("button");
const optionNodes = new Map([
  ['[data-provider-option="model"]',model],
  ['[data-provider-option="reasoning_effort"]',effort],
  ['[data-provider-option-badge="model"]',modelBadge],
  ['[data-provider-option-badge="reasoning_effort"]',effortBadge],
  ["[data-provider-options]",optionPanel],
  ["[data-provider-options-status]",optionStatus],
  ["button[type=submit]",optionSubmit],
]);
optionForm.querySelector = (selector) => optionNodes.get(selector) || null;
renderProviderOptions(optionForm, {provider_options:{
  model:{supported:true,current_value:"current-model",choices:[{value:"next-model",label:"Next model"}]},
  reasoning_effort:{supported:true,current_value:"medium",choices:[{value:"high",label:"High"}]}
}});
assert(!model.disabled && !effort.disabled, "advertised provider dropdowns were disabled");
assert(model.querySelectorAll("option")[0].value === "" && model.textContent.includes("Keep current (current-model)"), "configuration dropdown lost its keep-current option");
model.value = "next-model";
effort.value = "high";
assert(providerSelection(optionForm, "model") === "next-model" && providerSelection(optionForm, "reasoning_effort") === "high", "exact advertised values were not preserved");

const childRow = makeSessionRow({
  id:"S-child",parent_session_id:"S-parent",agent:"claude",role:"reviewer",repo_path:"/repo",worktree_path:"",status:"waiting",permission:"read-approval"
});
assert(childRow.className.includes("child-session"), "live Task refresh did not indent a child Session: " + childRow.className);
assert(childRow.dataset.parentSessionId === "S-parent", "live Task row lost its parent Session ID");
assert(childRow.textContent.includes("Child · reviewer"), "live Task row did not label the child relationship");

reset();
assert(applyFrame(frame(1,"client_command",{command:"prompt",text:"Inspect the repo"})), "prompt frame was rejected");
applyFrame(frame(2,"provider_event",{type:"text_delta",stream:"output",text:"Answer "}));
applyFrame(frame(3,"provider_event",{type:"text_delta",stream:"reasoning",text:"Checking"}));
applyFrame(frame(4,"provider_event",{type:"tool_call",toolCallId:"tool-1",title:"Read file",kind:"read",status:"completed",content:[{type:"content",text:"ok"}]}));
applyFrame(frame(5,"provider_event",{type:"status",tag:"progress",text:"Almost done"}));
applyFrame(frame(6,"provider_result",{status:"completed",stop_reason:"end_turn"}));
const turn = transcriptState.byRequest.get("REQ");
assert(turn.blocks.find((block)=>block.type==="user")?.text === "Inspect the repo", "client command was not rendered mechanically");
assert(turn.blocks.find((block)=>block.type==="assistant")?.text === "Answer ", "output text_delta was not rendered");
assert(turn.blocks.find((block)=>block.type==="thought")?.text === "Checking", "reasoning text_delta was not distinguished by stream");
assert(turn.tools.get("tool-1")?.title === "Read file", "tool_call was not rendered from its raw normalized fields");
assert(turn.blocks.find((block)=>block.type==="status")?.text === "Almost done", "provider status was not rendered");
assert(turn.state === "complete", "provider_result did not close the turn");
assert(turnOverviewElement(turn).attributes["aria-label"].includes("Complete"), "turn overview lost its accessible state");

const diagnosticsBeforeUnknown = transcriptState.diagnostics.length;
const turnsBeforeUnknown = transcriptState.turns.length;
applyFrame(frame(7,"provider_event",{type:"future_event",nested:{kept:true}}));
assert(transcriptState.diagnostics.length === diagnosticsBeforeUnknown + 1, "unknown provider_event was dropped from diagnostics");
assert(transcriptState.diagnostics.at(-1).data.nested.kept, "unknown provider_event payload was rewritten");
assert(transcriptState.turns.length === turnsBeforeUnknown, "unknown provider_event invented an empty activity turn");
assert(!applyFrame(frame(7,"provider_event",{type:"text_delta",stream:"output",text:"duplicate"})), "duplicate sequence was applied");

reset();
applyFrame(frame(1,"permission_requested",{id:"P",request_id:"REQ-P",title:"Write",state:"waiting",options:[{id:"allow",name:"Allow once",kind:"allow_once"}]},"REQ-P"));
applyFrame(frame(2,"permission_resolved",{permission_id:"P",decision:"allow_once"},"REQ-P"));
assert(transcriptState.permissions.get("P")?.state === "resolved" && transcriptState.permissions.get("P")?.options.length === 1, "permission frames lost options or resolution");

reset();
transcriptState.lastSequence = 2;
assert(!receivePermissionSnapshot({sequence:4,decisions_ready:true,permissions:[{id:"P-watermark",request_id:"REQ-watermark",title:"Approve",options:[{id:"allow",name:"Allow",kind:"allow_once"}]}]}), "future permission snapshot applied before its journal watermark");
assert(!transcriptState.permissions.has("P-watermark") && transcriptState.pendingPermissionSnapshot?.sequence === 4, "future permission snapshot was not held causally");
applyFrame(frame(3,"provider_event",{type:"future_event"},"REQ-watermark"));
assert(!transcriptState.permissions.has("P-watermark"), "future permission snapshot applied one frame too early");
applyFrame(frame(4,"connection",{state:"connected"},""));
assert(transcriptState.permissions.get("P-watermark")?.state === "waiting" && permissionDecisionsReady, "permission snapshot did not apply at its exact watermark");
applyFrame(frame(5,"permission_requested",{id:"P-new",request_id:"REQ-new",title:"New",options:[{id:"allow",name:"Allow",kind:"allow_once"}]},"REQ-new"));
receivePermissionSnapshot({sequence:4,decisions_ready:true,permissions:[]});
assert(transcriptState.permissions.get("P-new")?.state === "waiting", "stale empty snapshot retired a newer streamed permission");
applyFrame(frame(6,"permission_resolved",{permission_id:"P-new",decision:"allow_once"},"REQ-new"));
receivePermissionSnapshot({sequence:5,decisions_ready:true,permissions:[{id:"P-new",request_id:"REQ-new",title:"New",options:[{id:"allow",name:"Allow",kind:"allow_once"}]}]});
assert(transcriptState.permissions.get("P-new")?.state === "resolved", "stale waiting snapshot resurrected a resolved permission");
receivePermissionSnapshot({sequence:0,decisions_ready:false,blocked_code:"worker_upgrade_required",blocked_message:"Upgrade required",permissions:[]});
assert(!permissionDecisionsReady && permissionDecisionBlockedCode === "worker_upgrade_required" && transcriptState.permissions.get("P-watermark")?.state === "waiting", "offline readiness snapshot erased durable permission cards");

reset();
rememberReplayReady(JSON.stringify({cursor:200,earliest_sequence:201,clipped:true}));
receivePermissionSnapshot({sequence:80,decisions_ready:true,permissions:[{id:"P-old",request_id:"REQ-old",title:"Already resolved in clipped history",options:[{id:"allow",name:"Allow",kind:"allow_once"}]}]});
assert(!transcriptState.permissions.has("P-old"), "pre-window snapshot resurrected a permission resolved in clipped history");
receivePermissionSnapshot({sequence:200,decisions_ready:false,blocked_code:"session_channel_failed",blocked_message:"Connection failed",permissions:[]});
assert(!permissionDecisionsReady && permissionDecisionBlockedCode === "session_channel_failed", "current permission authority snapshot was not applied");
receivePermissionSnapshot({sequence:150,decisions_ready:true,permissions:[{id:"P-reversed",request_id:"REQ-reversed",title:"Stale",options:[{id:"allow",name:"Allow",kind:"allow_once"}]}]});
assert(!permissionDecisionsReady && permissionDecisionBlockedCode === "session_channel_failed" && !transcriptState.permissions.has("P-reversed"), "reversed snapshot completion overwrote newer permission authority");

reset();
applyFrame(frame(1,"provider_event",{type:"text_delta",stream:"output",text:"x".repeat(LIMITS.assistant + 100)}));
const bounded = transcriptState.turns[0].blocks.find((block)=>block.type==="assistant");
assert(bounded.bytes <= LIMITS.assistant && bounded.leadingClipped, "provider text exceeded the browser retention bound");
for (let seq = 2; seq <= LIMITS.diagnostics + 2; seq += 1) applyFrame(frame(seq,"connection",{state:"connected"},""));
assert(transcriptState.diagnostics.length === LIMITS.diagnostics, "diagnostic frame bound was exceeded");
assert(transcriptState.diagnosticsClipped && transcriptState.diagnosticBytes <= LIMITS.diagnosticBytes, "diagnostic byte/count clipping was not recorded");

reset();
const hugeMetadata = "界".repeat(12000);
const hugeRequestID = "request-" + hugeMetadata;
applyFrame(frame(1,"provider_event",{type:"text_delta",stream:"output",messageId:hugeMetadata,text:"answer"},hugeRequestID));
assert(transcriptState.diagnostics[0].request_id === hugeRequestID, "bounded transcript projection rewrote raw diagnostics");
applyFrame(frame(2,"provider_event",{
  type:"tool_call",toolCallId:hugeMetadata,title:hugeMetadata,kind:hugeMetadata,status:hugeMetadata,
  metadata:{label:hugeMetadata},content:"detail"
},hugeRequestID));
applyFrame(frame(3,"provider_event",{type:"status",tag:hugeMetadata,text:"working"},hugeRequestID));
applyFrame(frame(4,"provider_result",{status:hugeMetadata,stop_reason:hugeMetadata},hugeRequestID));
applyFrame(frame(5,"worker_error",{message:"failed",source:hugeMetadata,code:hugeMetadata},hugeRequestID));
applyFrame(frame(6,"permission_requested",{
  id:"P-safe",request_id:hugeRequestID,title:hugeMetadata,kind:hugeMetadata,state:"waiting",
  options:[{id:"allow",name:hugeMetadata,kind:"allow_once"}]
},hugeRequestID));
applyFrame(frame(7,"permission_requested",{
  id:hugeMetadata,title:"must not be retained",options:[{id:"allow",name:"Allow",kind:"allow_once"}]
},hugeRequestID));
const metadataTurn = transcriptState.byRequest.get(boundedTranscriptIdentifier(hugeRequestID));
const metadataText = metadataTurn.blocks.find((block)=>block.type==="assistant");
const metadataTool = metadataTurn.blocks.find((block)=>block.type==="tool");
const metadataStatus = metadataTurn.blocks.find((block)=>block.type==="status");
const metadataError = metadataTurn.blocks.find((block)=>block.type==="error");
const metadataPermission = transcriptState.permissions.get("P-safe");
const utf8Bytes = (value) => textEncoder.encode(String(value || "")).length;
assert(utf8Bytes(metadataTurn.id) <= LIMITS.identifier, "request ID exceeded its UTF-8 bound");
assert(utf8Bytes(metadataText.messageID) <= LIMITS.identifier, "message ID exceeded its UTF-8 bound");
assert(utf8Bytes(metadataTool.id) <= LIMITS.identifier, "tool ID exceeded its UTF-8 bound");
assert(utf8Bytes(metadataTool.title) <= LIMITS.title && utf8Bytes(metadataTool.kind) <= LIMITS.kind, "tool title/kind exceeded their UTF-8 bounds");
assert(utf8Bytes(metadataTool.status) <= LIMITS.status && utf8Bytes(metadataTool.metadata) <= LIMITS.metadata, "tool status/metadata exceeded their UTF-8 bounds");
assert(utf8Bytes(metadataStatus.tag) <= LIMITS.status, "provider status tag exceeded its UTF-8 bound");
assert(utf8Bytes(metadataError.errorSource) <= LIMITS.errorProvenance && utf8Bytes(metadataError.errorCode) <= LIMITS.errorProvenance, "error provenance exceeded its UTF-8 bounds");
assert(utf8Bytes(metadataTurn.stopReason) <= LIMITS.stopReason && metadataTurn.lifecycle.every((item)=>utf8Bytes(item.state) <= LIMITS.status), "turn state metadata exceeded its UTF-8 bounds");
assert(metadataPermission && utf8Bytes(metadataPermission.id) <= LIMITS.permissionIdentifier, "valid permission ID was not retained within its bound");
assert(utf8Bytes(metadataPermission.title) <= LIMITS.title && utf8Bytes(metadataPermission.kind) <= LIMITS.kind, "permission metadata exceeded its UTF-8 bounds");
assert(metadataPermission.options.length === 1 && utf8Bytes(metadataPermission.options[0].name) <= LIMITS.title, "permission option metadata exceeded its UTF-8 bounds");
assert(!transcriptState.permissions.has(hugeMetadata), "oversized exact permission ID was retained or made actionable");
const accountedMetadataBytes = transcriptState.turns.reduce((total,item)=>total+turnRetainedBytes(item),0) +
  Array.from(transcriptState.permissions.values()).reduce((total,item)=>total+permissionRetainedBytes(item),0);
assert(transcriptState.retainedBytes === accountedMetadataBytes && transcriptState.retainedBytes <= LIMITS.retained, "provider metadata escaped global retained-byte accounting");
const metadataToolElement = toolElement(metadataTool);
assert(utf8Bytes(metadataToolElement.dataset.detailKey) <= LIMITS.identifier + utf8Bytes("tool:"), "rendered tool key re-expanded an oversized ID");
assert(metadataToolElement.textContent.length < hugeMetadata.length, "tool rendering retained oversized provider metadata");
assert(renderBlockElement(metadataError,"Agent").textContent.length < hugeMetadata.length, "error rendering retained oversized provenance");
assert(providerStatusElement(metadataStatus).textContent.length < hugeMetadata.length, "status rendering retained an oversized tag");

const approvalShelf = document.createElement("section");
const approvalAnnouncer = document.createElement("p");
const approvalStatus = document.createElement("span");
const metadataQuerySelector = document.querySelector;
document.querySelector = (selector) => selector === "#approval-shelf" ? approvalShelf :
  selector === "#approval-announcer" ? approvalAnnouncer : selector === "#session-status" ? approvalStatus : metadataQuerySelector(selector);
permissionDecisionsReady = true;
renderedPermissionSignature = "";
renderApprovalShelf();
assert(approvalShelf.textContent.includes("Approval required") && approvalShelf.textContent.length < hugeMetadata.length, "permission rendering retained oversized metadata");
document.querySelector = metadataQuerySelector;

reset();
const retainedPayload = "x".repeat(LIMITS.assistant);
const oversizedMessageID = "m".repeat(LIMITS.identifier + 100);
for (let index = 0, seq = 1; index < 132; index += 1) {
  const requestID = "RET-" + index;
  applyFrame(frame(seq++,"provider_event",{type:"text_delta",stream:"output",messageId:oversizedMessageID,text:retainedPayload},requestID));
  applyFrame(frame(seq++,"provider_event",{type:"tool_call",toolCallId:"tool-"+index,title:"Bounded",metadata:hugeMetadata,status:"completed"},requestID));
  applyFrame(frame(seq++,"provider_result",{status:"completed",stop_reason:"end_turn"},requestID));
}
const recomputedRetainedBytes = transcriptState.turns.reduce((total,item)=>total+turnRetainedBytes(item),0) +
  Array.from(transcriptState.permissions.values()).reduce((total,item)=>total+permissionRetainedBytes(item),0);
assert(transcriptState.retainedBytes === recomputedRetainedBytes, "global retained-byte counter drifted from bounded transcript state");
assert(transcriptState.retainedBytes <= LIMITS.retained && transcriptState.turns.length < 132 && transcriptState.framesClipped, "bounded metadata flood exceeded global retention or failed to evict old turns");
assert(transcriptState.turns.every((item)=>item.blocks.filter((block)=>block.type==="tool").every((tool)=>utf8Bytes(tool.metadata)<=LIMITS.metadata)), "metadata flood retained an oversized tool field");

reset();
applyFrame(frame(45,"connection",{state:"connected"},""));
assert(transcriptState.framesClipped && transcriptState.lastSequence === 45, "bounded fresh replay did not disclose its clipped prefix");

reset();
transcriptState.lastSequence = 9;
const conversation = document.createElement("section");
conversation.append(document.createElement("div"));
const scroller = document.createElement("div");
scroller.scrollHeight = 100;
scroller.clientHeight = 100;
const promptForm = document.createElement("form");
const textarea = document.createElement("textarea");
textarea.value = "  Pending prompt\n";
const send = document.createElement("button");
const actionNotice = document.createElement("p");
const streamState = document.createElement("div");
const streamLabel = document.createElement("span");
streamState.querySelector = (selector) => selector === "span" ? streamLabel : null;
promptForm.querySelector = (selector) => selector === "textarea" ? textarea : ["button[type=submit]", 'button[type="submit"]'].includes(selector) ? send : null;
const originalQuerySelector = document.querySelector;
document.querySelector = (selector) => {
  if (selector === "#conversation") return conversation;
  if (selector === "#conversation-scroll") return scroller;
  if (selector === "#prompt-form") return promptForm;
  if (selector === "#provider-config-form") return optionForm;
  if (selector === "#action-notice") return actionNotice;
  if (selector === "#stream-state") return streamState;
  return originalQuerySelector(selector);
};

const originalLoadProviderOptions = loadProviderOptions;
let startupOptionLoads = 0;
loadProviderOptions = async (sessionID) => { startupOptionLoads += 1; assert(sessionID === "S", "connected frame loaded options for the wrong Session"); return true; };
document.body.dataset.sessionId = "S";
currentSessionRuntimeStatus = "starting";
reset();
attachSessionChannel("/api/v1/sessions/S/stream");
const boundaryConnection = eventSources.at(-1);
applyFrame(frame(1,"connection",{state:"connecting"},""));
assert(textarea.disabled && send.disabled, "connecting frame left the composer enabled");
applyFrame(frame(2,"connection",{state:"connected"},""));
assert(textarea.disabled && send.disabled, "provider connection enabled the composer before replay ready");
const preReadyOptionTimer = timers.findLast((timer) => timer.delay === 25);
if (preReadyOptionTimer) await preReadyOptionTimer.callback();
assert(startupOptionLoads === 0, "provider options loaded before replay ready");
boundaryConnection.emit("ready",{data:JSON.stringify({cursor:2,earliest_sequence:3,clipped:false})});
assert(!textarea.disabled && !send.disabled, "replay ready plus provider connection did not enable the composer");
const optionLoadTimer = timers.findLast((timer) => timer.delay === 25);
assert(optionLoadTimer, "ready connected Session did not schedule live provider-option discovery");
await optionLoadTimer.callback();
assert(startupOptionLoads === 1, "ready connected Session did not load provider options exactly once");
startupOptionLoads = 0;
setRuntimeStatus("working");
scheduleProviderOptionsAfterConnection();
const busyOptionTimer = timers.findLast((timer) => timer.delay === 25);
await busyOptionTimer.callback();
assert(startupOptionLoads === 0, "provider options loaded while the Session was working");
setRuntimeStatus("waiting");
const idleOptionTimer = timers.findLast((timer) => timer.delay === 25);
assert(idleOptionTimer && idleOptionTimer !== busyOptionTimer, "returning to idle did not retry provider-option discovery");
await idleOptionTimer.callback();
assert(startupOptionLoads === 1, "returning to idle did not load provider options exactly once");
loadProviderOptions = originalLoadProviderOptions;

const permissionApiBeforeEpochTest = apiRequest;
let resolveOldPermissionSnapshot;
permissionDecisionsReady = true;
apiRequest = async () => await new Promise((resolve) => { resolveOldPermissionSnapshot = resolve; });
const oldPermissionRequest = refreshPermissions();
setProviderConnectionState("connecting");
resolveOldPermissionSnapshot({sequence:0,decisions_ready:false,blocked_code:"session_offline",blocked_message:"Old connection"});
await oldPermissionRequest;
assert(permissionDecisionsReady, "generationless snapshot from an earlier connection disabled the current permission surface");
permissionDecisionsReady = false;
let connectedPermissionReads = 0;
apiRequest = async (path) => {
  connectedPermissionReads += 1;
  assert(path === "/api/v1/sessions/S/permissions", "connected transition read the wrong permission resource");
  return {sequence:transcriptState.lastSequence,decisions_ready:true,permissions:[]};
};
setProviderConnectionState("connected");
await Promise.resolve();
await Promise.resolve();
assert(connectedPermissionReads === 1 && permissionDecisionsReady, "connected transition did not make immediate permission decisions ready");
apiRequest = permissionApiBeforeEpochTest;

reset();
setRuntimeStatus("waiting");
setProviderConnectionState("connected");
attachSessionChannel("/api/v1/sessions/S/stream");
const staleConnection = eventSources.at(-1);
attachSessionChannel("/api/v1/sessions/S/stream");
const currentConnection = eventSources.at(-1);
assert(staleConnection.closed && textarea.disabled && send.disabled, "replacing an EventSource did not close and gate the old replay epoch");
staleConnection.onopen?.();
staleConnection.emit("ready",{data:JSON.stringify({cursor:0,earliest_sequence:1,clipped:false})});
staleConnection.emit("frame",{data:JSON.stringify(frame(1,"provider_event",{type:"text_delta",stream:"output",text:"stale"},"STALE"))});
staleConnection.onerror?.();
staleConnection.emit("channel_error",{data:JSON.stringify({code:"stream_unavailable"})});
assert(activeEventSource === currentConnection && !currentConnection.closed && transcriptState.lastSequence === 0, "a stale EventSource callback mutated the active replay epoch");
currentConnection.onopen?.();
currentConnection.emit("frame",{data:JSON.stringify(frame(1,"provider_event",{type:"text_delta",stream:"output",text:"too early"},"EARLY"))});
assert(transcriptState.lastSequence === 0 && textarea.disabled && send.disabled, "a frame or open event bypassed replay ready");
currentConnection.emit("ready",{data:JSON.stringify({cursor:0,earliest_sequence:1,clipped:false})});
assert(!textarea.disabled && !send.disabled, "the active EventSource ready epoch did not open the command boundary");

const optionsBeforeDisconnect = model.textContent;
const apiBeforeStaleOptions = apiRequest;
let resolveStaleOptions;
apiRequest = async (path) => {
  if (path === "/api/v1/sessions/S/provider-options") {
    return await new Promise((resolve) => { resolveStaleOptions = resolve; });
  }
  if (path === "/api/v1/sessions/S/permissions") return {sequence:transcriptState.lastSequence,decisions_ready:true,permissions:[]};
  throw new Error("unexpected request in ready-epoch test: " + path);
};
const staleOptionsRequest = loadProviderOptions("S", optionForm);
assert(typeof resolveStaleOptions === "function", "provider-option request did not start inside the ready boundary");
currentConnection.onerror?.();
assert(textarea.disabled && send.disabled && model.disabled && effort.disabled, "EventSource disconnect left provider-dependent controls enabled");
resolveStaleOptions({provider_options:{model:{supported:true,choices:[{value:"stale-model",label:"Stale model"}]}}});
assert(!await staleOptionsRequest && model.textContent === optionsBeforeDisconnect, "an option response from an invalidated ready epoch was rendered");
currentConnection.emit("ready",{data:JSON.stringify({cursor:0,earliest_sequence:1,clipped:false})});
assert(!textarea.disabled && !send.disabled, "automatic EventSource replay ready did not reopen the command boundary");
apiRequest = apiBeforeStaleOptions;
currentConnection.emit("channel_error",{data:JSON.stringify({code:"replay_cursor_reset",reset_cursor:true})});
const resetReplayTimer = timers.findLast((timer) => timer.delay === 50);
assert(resetReplayTimer && currentConnection.closed && activeEventSource === null, "cursor reset did not close its exact EventSource epoch");
attachSessionChannel("/api/v1/sessions/S/stream");
const replacementDuringReset = eventSources.at(-1);
const sourcesBeforeStaleReset = eventSources.length;
await resetReplayTimer.callback();
assert(activeEventSource === replacementDuringReset && eventSources.length === sourcesBeforeStaleReset, "stale cursor-reset callback replaced a newer EventSource epoch");

reset();
conversation.replaceChildren(document.createElement("div"));
attachSessionChannel("/api/v1/sessions/S/stream");
const boundedReplayConnection = eventSources.at(-1);
boundedReplayConnection.emit("ready",{data:JSON.stringify({cursor:44,earliest_sequence:45,clipped:true})});
assert(transcriptState.replayReady && transcriptState.replayCursor === 44 && transcriptState.replayEarliestSequence === 45 && transcriptState.replayClipped && transcriptState.lastSequence === 44, "Session ready metadata was not retained as the effective cursor");
assert(transcriptState.framesClipped && conversation.textContent.includes("sequence 45") && conversation.textContent.includes("effective cursor 44"), "bounded replay did not render its explicit earliest sequence and cursor");
assert(conversation.textContent.includes("Agent activity appears here as it arrives"), "bounded ready notice displaced the empty Session guidance");

reset();
conversation.replaceChildren(document.createElement("div"));
attachSessionChannel("/api/v1/sessions/S/stream");
const fallbackReplayConnection = eventSources.at(-1);
fallbackReplayConnection.emit("ready",{data:JSON.stringify({cursor:"44",earliest_sequence:45,clipped:true})});
assert(!transcriptState.replayReady && !transcriptState.framesClipped, "invalid ready metadata was trusted");
applyFrame(frame(45,"provider_event",{type:"future_event"},"FALLBACK"));
renderConversation();
assert(transcriptState.framesClipped && conversation.textContent.includes("Earlier activity is outside this bounded browser view"), "inferred replay gap lost its safe generic fallback");
assert(!conversation.textContent.includes("effective cursor"), "invalid ready metadata appeared in the replay notice");

reset();
conversation.replaceChildren(document.createElement("div"));
transcriptState.lastSequence = 9;
attachSessionChannel("/api/v1/sessions/S/stream");
const sourceConnection = eventSources.at(-1);
assert(sourceConnection.path === "/api/v1/sessions/S/stream?seed=9", "Session stream did not resume from the last applied sequence");
assert(sourceConnection.listeners.frame?.length === 1 && !sourceConnection.listeners.activity, "Session stream did not bind the mechanical frame event");
sourceConnection.emit("ready",{data:JSON.stringify({cursor:9,earliest_sequence:10,clipped:false})});
assert(transcriptState.replayReady && transcriptState.replayCursor === 9 && transcriptState.replayEarliestSequence === 10 && !transcriptState.replayClipped, "exact resume ready metadata was not retained");
assert(conversation.children.length === 1 && conversation.children[0].className === "conversation-empty", "ready did not remove the loading skeleton");
assert(conversation.textContent.includes("Agent activity appears here as it arrives"), "empty Session state lacks streaming guidance");

reset();
conversation.replaceChildren(document.createElement("div"));
let resolvePrompt;
const originalApiRequest = apiRequest;
apiRequest = async (path, method, body) => {
  assert(path === "/api/v1/sessions/S/prompt" && method === "POST" && body.message === "  Pending prompt\n", "composer changed the caller's prompt bytes");
  return await new Promise((resolve) => { resolvePrompt = resolve; });
};
bindComposer("S");
const pendingPrompt = promptForm.listeners.submit[0]({preventDefault(){}});
assert(send.disabled && typeof resolvePrompt === "function", "prompt POST did not remain pending");
sourceConnection.emit("frame",{data:JSON.stringify(frame(1,"provider_event",{type:"text_delta",stream:"output",text:"Immediate output"},"LIVE"))});
const renderTimer = timers[transcriptState.renderTimer - 1];
assert(renderTimer?.delay === 55, "frame was not scheduled for immediate rendering");
renderTimer.callback();
assert(send.disabled && conversation.textContent.includes("Immediate output"), "frame rendering waited for the prompt POST");
sourceConnection.onerror?.();
assert(textarea.disabled && send.disabled, "disconnect during a prompt POST left the command boundary enabled");
resolvePrompt({request_id:"LIVE",state:"ready"});
await pendingPrompt;
assert(transcriptState.byRequest.get("LIVE").blocks.find((block)=>block.type==="user")?.text === "  Pending prompt\n", "optimistic prompt rendering changed the caller's bytes");
assert(textarea.disabled && send.disabled, "prompt completion reopened a disconnected command boundary");
sourceConnection.emit("ready",{data:JSON.stringify({cursor:1,earliest_sequence:2,clipped:false})});
assert(!textarea.disabled && !send.disabled, "replay ready after prompt disconnect did not restore the command boundary");
apiRequest = originalApiRequest;

assert(sourceConnection.listeners.channel_error?.length === 1, "Session stream did not bind the transport error control event");
sourceConnection.emit("channel_error",{data:JSON.stringify({code:"stream_unavailable",message:"bounded safe message"})});
assert(sourceConnection.closed, "terminal channel error left a reconnect loop open");
assert(streamLabel.textContent === "Stream unavailable" && conversation.textContent.includes("bounded safe message"), "terminal stream error was not visible in the Session view");

attachSessionChannel("/api/v1/sessions/S/stream");
const corruptInitializationConnection = eventSources.at(-1);
corruptInitializationConnection.emit("channel_error",{data:JSON.stringify({code:"stream_unavailable",message:"The Session Channel stream ended before replay completed."})});
assert(corruptInitializationConnection.closed && activeEventSource === null, "pre-ready corrupt replay error left an EventSource reconnect loop open");
assert(streamLabel.textContent === "Stream unavailable" && conversation.textContent.includes("ended before replay completed"), "pre-ready corrupt replay error was not visible in the Session view");

attachSessionChannel("/api/v1/sessions/S/stream");
const unavailableConnection = eventSources.at(-1);
unavailableConnection.emit("channel_error",{data:JSON.stringify({code:"replay_unavailable",message:"Saved Session activity is unavailable. Resume the Session."})});
assert(unavailableConnection.closed && activeEventSource === null, "pre-ready replay-unavailable error left an EventSource reconnect loop open");
assert(streamLabel.textContent === "Replay unavailable" && conversation.textContent.includes("Resume the Session"), "replay-unavailable guidance was not visible in the Session view");

attachSessionChannel("/api/v1/sessions/S/stream");
const terminalConnection = eventSources.at(-1);
terminalConnection.emit("ready",{data:JSON.stringify({cursor:1,earliest_sequence:2,clipped:false})});
terminalConnection.emit("frame",{data:JSON.stringify(frame(2,"connection",{state:"stopped"},""))});
assert(terminalConnection.closed && activeEventSource === null && streamLabel.textContent === "Stopped", "terminal connection frame did not close and label its EventSource");
terminalConnection.onerror?.();
assert(streamLabel.textContent === "Stopped", "stale EOF error replaced the terminal Session label with reconnecting");

const createForm = document.createElement("form");
createForm.formValues = {agent:"codex",role:"worker",repo_path:"/repo",worktree:true};
const createSubmit = document.createElement("button");
const repoInput = document.createElement("input");
createForm.querySelector = (selector) => selector === "button[type=submit]" ? createSubmit : selector === 'input[name="repo_path"]' ? repoInput : null;
let createCall;
apiRequest = async (path, method, body) => { createCall = {path,method,body}; return {session:{id:"CHILD"}}; };
await submitSessionCreation(createForm, "/api/v1/sessions/PARENT/children", document.createElement("p"));
assert(createCall.path === "/api/v1/sessions/PARENT/children" && createCall.method === "POST", "child Session used a special review route");
assert(createCall.body.agent === "codex" && createCall.body.repo_path === "/repo" && !("prompt" in createCall.body) && !("model" in createCall.body), "Session creation mixed prompt or configuration into identity");
assert(assignedLocations.at(-1) === "/sessions/CHILD", "created child Session did not open directly");
apiRequest = originalApiRequest;
document.querySelector = originalQuerySelector;
`)
	runner := fmt.Sprintf(`
const fs = require("node:fs");
const vm = require("node:vm");
function assert(value, message) { if (!value) throw new Error(message); }

const eventSources = [];
const timers = [];
const assignedLocations = [];
class FakeElement {
  constructor(tagName) {
    this.tagName = String(tagName || "").toUpperCase();
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.parentElement = null;
    this.isConnected = true;
    this.disabled = false;
    this.hidden = false;
    this.open = false;
    this.value = "";
    this.label = "";
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.clientHeight = 0;
    this._textContent = "";
    this.classList = {
      toggle: (name, force) => {
        const classes = new Set(this.className.split(/\\s+/).filter(Boolean));
        if (force === undefined ? !classes.has(name) : force) classes.add(name); else classes.delete(name);
        this.className = [...classes].join(" ");
      },
      add: (...names) => { for (const name of names) this.classList.toggle(name, true); },
      remove: (...names) => { for (const name of names) this.classList.toggle(name, false); },
      contains: (name) => this.className.split(/\\s+/).includes(name),
    };
  }
  append(...children) {
    for (const child of children) {
      if (child instanceof FakeElement) {
        child.parentElement?.removeChild(child);
        child.parentElement = this;
      }
      this.children.push(child);
    }
  }
  insertBefore(child, before) {
    if (child instanceof FakeElement) {
      child.parentElement?.removeChild(child);
      child.parentElement = this;
    }
    const index = before ? this.children.indexOf(before) : -1;
    if (index < 0) this.children.push(child); else this.children.splice(index, 0, child);
    return child;
  }
  removeChild(child) {
    const index = this.children.indexOf(child);
    if (index >= 0) this.children.splice(index, 1);
    if (child instanceof FakeElement) child.parentElement = null;
    return child;
  }
  replaceChildren(...children) {
    for (const child of this.children) if (child instanceof FakeElement) child.parentElement = null;
    this.children = [];
    this._textContent = "";
    this.append(...children);
  }
  replaceWith(next) {
    if (!this.parentElement) return;
    const parent = this.parentElement;
    const index = parent.children.indexOf(this);
    if (index < 0) return;
    this.parentElement = null;
    if (next instanceof FakeElement) next.parentElement = parent;
    parent.children[index] = next;
  }
  remove() { this.parentElement?.removeChild(this); this.isConnected = false; }
  contains(candidate) {
    if (candidate === this) return true;
    return this.children.some((child) => child instanceof FakeElement && child.contains(candidate));
  }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  querySelectorAll(selector) {
    const result = [];
    const visit = (node) => {
      if (!(node instanceof FakeElement)) return;
      if (node.matches(selector)) result.push(node);
      for (const child of node.children) visit(child);
    };
    for (const child of this.children) visit(child);
    return result;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  matches(selector) {
    if (selector === "option") return this.tagName === "OPTION";
    if (selector === "details[data-detail-key]") return this.tagName === "DETAILS" && Boolean(this.dataset.detailKey);
    if (selector.startsWith(".")) return this.className.split(/\\s+/).includes(selector.slice(1));
    if (selector.startsWith("#")) return this.id === selector.slice(1);
    return false;
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  focus() {}
  get lastElementChild() { return [...this.children].reverse().find((child) => child instanceof FakeElement) || null; }
  set textContent(value) { this._textContent = String(value); this.children = []; }
  get textContent() { return this._textContent + this.children.map((child) => child?.textContent ?? child?.data ?? String(child)).join(""); }
}
class FakeFormData {
  constructor(form) { this.form = form; }
  get(name) { return this.form?.formValues?.[name] ?? ""; }
  has(name) { return Boolean(this.form?.formValues && Object.prototype.hasOwnProperty.call(this.form.formValues, name)); }
}
class FakeEventSource {
  constructor(path) { this.path = path; this.listeners = {}; eventSources.push(this); }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  emit(type, event = {}) { for (const listener of this.listeners[type] || []) listener(event); }
  close() { this.closed = true; }
}
class FakeAbortController {
  constructor() { this.signal = {aborted:false}; }
  abort() { this.signal.aborted = true; }
}
const body = new FakeElement("body");
body.dataset = {page:""};
const document = {
  body,
  createElement(tagName){ return new FakeElement(tagName); },
  createTextNode(value){ return {data:String(value),textContent:String(value)}; },
  createDocumentFragment(){ return new FakeElement("fragment"); },
  querySelector(){ return null; },
  querySelectorAll(){ return []; },
  getElementById(){ return null; },
  addEventListener(){},
};
const window = {
  EventSource: FakeEventSource,
  location: {assign(path){assignedLocations.push(path);},reload(){assignedLocations.push("reload");}},
  setTimeout(callback, delay = 0){ timers.push({callback,delay}); return timers.length; },
  clearTimeout(){},
  setInterval(){ return 0; },
  addEventListener(){},
  matchMedia(){ return {matches:false,addEventListener(){}}; },
};
const sandbox = {
  TextEncoder, console, document, window, EventSource:FakeEventSource, AbortController:FakeAbortController,
  eventSources, timers, assignedLocations,
  HTMLDialogElement:class {}, FormData:FakeFormData,
  assert(value, message){ if (!value) throw new Error(message); },
  fetch:async()=>({ok:false,status:500,async text(){return "{}";}}),
};
vm.createContext(sandbox);
const source = fs.readFileSync(%q, "utf8");
const tests = %q;
const result = vm.runInContext(source + "\n(async () => {\n" + tests + "\n})()", sandbox, {timeout:10000});
Promise.resolve(result).catch((error) => { console.error(error); process.exitCode = 1; });
`, sourceFile, string(harness))
	if err := os.WriteFile(testFile, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, testFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser Session Channel: %v\n%s", err, output)
	}
}
