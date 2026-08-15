package console

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

const (
	maxProjectedMessageBytes = 128 << 10
	maxProjectedThoughtBytes = 32 << 10
	maxToolDetailBytes       = 64 << 10
	maxToolPreviewBytes      = 2 << 10
)

// transcript is an incremental, viewer-only projection of append-only Session
// history. It is rebuilt from JSONL on attachment and is never persisted as a
// third domain entity.
type transcript struct {
	requests []*transcriptRequest
	byID     map[string]*transcriptRequest
	seen     map[uint64]struct{}
	metadata transcriptMetadata
}

type transcriptMetadata struct {
	lastSequence uint64
	usage        string
	mode         string
	model        string
	commands     int
}

type transcriptRequest struct {
	id         string
	state      turnState
	stop       string
	errorText  string
	blocks     []*transcriptBlock
	tools      map[string]*toolCard
	plan       *transcriptBlock
	thought    *transcriptBlock
	permission map[string]*permissionCard
}

type turnState string

const (
	turnQueued     turnState = "queued"
	turnLive       turnState = "live"
	turnCancelling turnState = "cancelling"
	turnComplete   turnState = "complete"
	turnCancelled  turnState = "cancelled"
	turnFailed     turnState = "error"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockThought
	blockTool
	blockPlan
	blockPermission
	blockError
)

type transcriptBlock struct {
	kind       blockKind
	id         string
	text       string
	legacy     bool
	truncated  bool
	tool       *toolCard
	plan       []planEntry
	permission *permissionCard
}

type planEntry struct {
	content string
	status  string
}

type toolCard struct {
	id            string
	title         string
	kind          string
	status        string
	metadata      string
	outputBytes   int
	outputLines   int
	preview       string
	detail        string
	detailLabel   string
	detailClipped bool
	sensitive     bool
	seenPayloads  map[[sha256.Size]byte]struct{}
}

type permissionCard struct {
	id         string
	toolCallID string
	title      string
	kind       string
	options    []permissionOption
	state      string
	decision   string
}

type permissionOption struct {
	id   string
	name string
	kind string
}

func newTranscript() *transcript {
	return &transcript{byID: make(map[string]*transcriptRequest), seen: make(map[uint64]struct{})}
}

func (t *transcript) apply(line sessionlog.Line) bool {
	if line.Seq == 0 {
		return false
	}
	if _, duplicate := t.seen[line.Seq]; duplicate {
		return false
	}
	t.seen[line.Seq] = struct{}{}
	if line.Seq > t.metadata.lastSequence {
		t.metadata.lastSequence = line.Seq
	}

	request := t.request(line.RequestID)
	switch line.Type {
	case "prompt_submitted":
		message := jsonString(line.Data, "display_message")
		if message == "" {
			message = jsonString(line.Data, "message")
		}
		if message != "" && request != nil {
			request.setUserMessage(message, false)
		}
		if request != nil && request.state == "" {
			request.state = turnQueued
		}
	case "prompt_queued":
		if request != nil && request.state == "" {
			request.state = turnQueued
		}
	case "turn_started":
		if request != nil {
			request.state = turnLive
		}
	case "cancel_requested":
		if request != nil {
			request.state = turnCancelling
		}
	case "turn_completed":
		if request != nil {
			request.stop = jsonString(line.Data, "stopReason")
			if request.stop == "cancelled" || request.state == turnCancelling {
				request.state = turnCancelled
			} else {
				request.state = turnComplete
			}
		}
	case "turn_error", "worker_error":
		if request == nil {
			request = t.request(syntheticRequestID(line.Seq))
		}
		request.state = turnFailed
		request.errorText = sanitizeDisplay(jsonString(line.Data, "message"))
		if request.errorText == "" {
			request.errorText = "The provider turn failed."
		}
		request.blocks = append(request.blocks, &transcriptBlock{kind: blockError, text: request.errorText})
	case "permission_resolved":
		if request != nil {
			request.resolvePermission(jsonString(line.Data, "permission_id"), jsonString(line.Data, "decision"))
		}
	case "acp":
		t.applyACP(request, line)
	}
	return true
}

func (t *transcript) request(id string) *transcriptRequest {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	if existing := t.byID[id]; existing != nil {
		return existing
	}
	request := &transcriptRequest{
		id: id, tools: make(map[string]*toolCard), permission: make(map[string]*permissionCard),
	}
	t.byID[id] = request
	t.requests = append(t.requests, request)
	return request
}

func (t *transcript) applyACP(request *transcriptRequest, line sessionlog.Line) {
	var event acp.Event
	if json.Unmarshal(line.Data, &event) != nil {
		return
	}
	var raw map[string]any
	if json.Unmarshal(event.Raw, &raw) != nil {
		return
	}
	if request == nil && event.RequestID != "" {
		request = t.request(event.RequestID)
	}
	if request == nil {
		t.applyACPMetadata(event.Type, raw)
		return
	}

	params := object(raw["params"])
	update := object(params["update"])
	switch event.Type {
	case "session_prompt":
		request.setUserMessage(legacyDisplayMessage(sessionPromptText(params["prompt"])), true)
	case "agent_message_chunk":
		request.appendMessage(blockAssistant, stringValue(object(update["content"])["text"]), stringValue(update["messageId"]), maxProjectedMessageBytes)
	case "agent_thought_chunk":
		request.appendThought(stringValue(object(update["content"])["text"]))
	case "user_message_chunk":
		// prompt_submitted is the truthful user-facing source. Provider echoes
		// are metadata and must not duplicate or reconstruct an injected prompt.
	case "tool_call", "tool_call_update":
		request.upsertTool(update)
	case "plan", "plan_update":
		request.upsertPlan(update)
	case "session_request_permission":
		request.upsertPermission(raw)
	case "error":
		message := stringValue(object(raw["error"])["message"])
		if message != "" {
			request.state = turnFailed
			request.errorText = sanitizeDisplay(message)
			request.blocks = append(request.blocks, &transcriptBlock{kind: blockError, text: request.errorText})
		}
	case "prompt_result":
		result := object(raw["result"])
		if stop := stringValue(result["stopReason"]); stop != "" {
			request.stop = stop
			if stop == "cancelled" {
				request.state = turnCancelled
			}
		}
	default:
		t.applyACPMetadata(event.Type, raw)
	}
}

func (t *transcript) applyACPMetadata(eventType string, raw map[string]any) {
	params := object(raw["params"])
	update := object(params["update"])
	switch eventType {
	case "usage_update":
		if used := numberValue(update["used"]); used > 0 {
			t.metadata.usage = fmt.Sprintf("%.0f", used)
		}
	case "available_commands_update":
		if commands, ok := update["availableCommands"].([]any); ok {
			t.metadata.commands = len(commands)
		}
	case "session_info_update":
		t.metadata.mode = firstNonEmpty(stringValue(update["modeId"]), t.metadata.mode)
		t.metadata.model = firstNonEmpty(stringValue(update["modelId"]), t.metadata.model)
	}
}

func (r *transcriptRequest) hasUserMessage() bool {
	for _, block := range r.blocks {
		if block.kind == blockUser {
			return true
		}
	}
	return false
}

func (r *transcriptRequest) setUserMessage(text string, legacy bool) {
	text = sanitizeDisplay(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, block := range r.blocks {
		if block.kind != blockUser {
			continue
		}
		if block.legacy && !legacy {
			block.text = text
			block.legacy = false
		}
		return
	}
	r.blocks = append(r.blocks, &transcriptBlock{kind: blockUser, text: text, legacy: legacy})
}

func (r *transcriptRequest) appendMessage(kind blockKind, text, id string, limit int) {
	text = sanitizeDisplay(text)
	if text == "" {
		return
	}
	var block *transcriptBlock
	if len(r.blocks) > 0 {
		candidate := r.blocks[len(r.blocks)-1]
		if candidate.kind == kind && (candidate.id == id || candidate.id == "" || id == "") {
			block = candidate
		}
	}
	if block == nil {
		block = &transcriptBlock{kind: kind, id: id}
		r.blocks = append(r.blocks, block)
	}
	block.text, block.truncated = appendBounded(block.text, text, limit)
}

func (r *transcriptRequest) appendThought(text string) {
	text = sanitizeDisplay(text)
	if text == "" {
		return
	}
	if r.thought == nil {
		r.thought = &transcriptBlock{kind: blockThought}
		r.blocks = append(r.blocks, r.thought)
	}
	r.thought.text, r.thought.truncated = appendBounded(r.thought.text, text, maxProjectedThoughtBytes)
}

func (r *transcriptRequest) upsertTool(update map[string]any) {
	id := stringValue(update["toolCallId"])
	if id == "" {
		return
	}
	tool := r.tools[id]
	if tool == nil {
		tool = &toolCard{id: id, seenPayloads: make(map[[sha256.Size]byte]struct{})}
		r.tools[id] = tool
		r.blocks = append(r.blocks, &transcriptBlock{kind: blockTool, id: id, tool: tool})
	}
	tool.title = mergeField(tool.title, stringValue(update["title"]))
	tool.kind = mergeField(tool.kind, stringValue(update["kind"]))
	tool.status = mergeField(tool.status, stringValue(update["status"]))
	tool.metadata = mergeField(tool.metadata, toolMetadata(update))
	tool.consumePayloads(update)
}

func (r *transcriptRequest) upsertPlan(update map[string]any) {
	entries, _ := update["entries"].([]any)
	if r.plan == nil {
		r.plan = &transcriptBlock{kind: blockPlan}
		r.blocks = append(r.blocks, r.plan)
	}
	if entries == nil {
		return
	}
	latest := make([]planEntry, 0, len(entries))
	for _, value := range entries {
		entry := object(value)
		content := sanitizeDisplay(stringValue(entry["content"]))
		if content == "" {
			continue
		}
		latest = append(latest, planEntry{content: content, status: sanitizeInline(stringValue(entry["status"]))})
	}
	r.plan.plan = latest
}

func (r *transcriptRequest) upsertPermission(raw map[string]any) {
	params := object(raw["params"])
	tool := object(params["toolCall"])
	toolCallID := stringValue(tool["toolCallId"])
	id := permissionID(raw["id"], toolCallID)
	if id == "" {
		return
	}
	card := r.permission[id]
	if card == nil {
		card = &permissionCard{id: id, toolCallID: toolCallID, state: "waiting"}
		r.permission[id] = card
		r.blocks = append(r.blocks, &transcriptBlock{kind: blockPermission, id: id, permission: card})
	}
	card.title = mergeField(card.title, stringValue(tool["title"]))
	card.kind = mergeField(card.kind, stringValue(tool["kind"]))
	if values, ok := params["options"].([]any); ok && len(values) > 0 {
		options := make([]permissionOption, 0, len(values))
		for _, value := range values {
			option := object(value)
			options = append(options, permissionOption{
				id: stringValue(option["optionId"]), name: sanitizeInline(stringValue(option["name"])), kind: stringValue(option["kind"]),
			})
		}
		card.options = options
	}
}

func (r *transcriptRequest) resolvePermission(id, decision string) {
	if card := r.permission[id]; card != nil {
		card.state = "resolved"
		card.decision = sanitizeInline(decision)
	}
}

func (t *toolCard) consumePayloads(update map[string]any) {
	t.consumePayload("content", update["content"], contentIsSensitive(update["content"]))
	t.consumePayload("raw output", update["rawOutput"], true)
	meta := object(update["_meta"])
	if claude := object(meta["claudeCode"]); len(claude) > 0 {
		t.consumePayload("provider output", claude["toolResponse"], true)
	}
}

func (t *toolCard) consumePayload(label string, value any, sensitive bool) {
	text, linesHint := payloadText(value)
	if text == "" {
		return
	}
	digest := sha256.Sum256([]byte(text))
	if _, duplicate := t.seenPayloads[digest]; duplicate {
		return
	}
	t.seenPayloads[digest] = struct{}{}
	clean := sanitizeDisplay(text)
	if clean == "" {
		return
	}
	bytes := len([]byte(clean))
	lines := logicalLines(clean)
	if linesHint > lines {
		lines = linesHint
	}
	if bytes < t.outputBytes && lines < t.outputLines {
		return
	}
	if bytes > t.outputBytes {
		t.outputBytes = bytes
	}
	if lines > t.outputLines {
		t.outputLines = lines
	}
	t.sensitive = t.sensitive || sensitive
	t.detailLabel = label
	t.detail, t.detailClipped = truncateUTF8(clean, maxToolDetailBytes)
	if !sensitive && bytes <= maxToolPreviewBytes {
		t.preview = previewLines(clean, 3, 280)
	} else {
		t.preview = ""
	}
}

func (t *transcript) pendingPermission() (*transcriptRequest, *permissionCard) {
	for requestIndex := len(t.requests) - 1; requestIndex >= 0; requestIndex-- {
		request := t.requests[requestIndex]
		for blockIndex := len(request.blocks) - 1; blockIndex >= 0; blockIndex-- {
			card := request.blocks[blockIndex].permission
			if card != nil && card.state == "waiting" {
				return request, card
			}
		}
	}
	return nil, nil
}

func (t *transcript) tools() []*toolCard {
	var result []*toolCard
	for _, request := range t.requests {
		for _, block := range request.blocks {
			if block.tool != nil {
				result = append(result, block.tool)
			}
		}
	}
	return result
}

func (t *transcript) latestSummary() string {
	for requestIndex := len(t.requests) - 1; requestIndex >= 0; requestIndex-- {
		request := t.requests[requestIndex]
		for blockIndex := len(request.blocks) - 1; blockIndex >= 0; blockIndex-- {
			block := request.blocks[blockIndex]
			switch block.kind {
			case blockAssistant:
				if summary := firstMeaningfulLine(block.text); summary != "" {
					return summary
				}
			case blockTool:
				return toolDisplayTitle(block.tool) + statusSuffix(block.tool.status)
			case blockPermission:
				if block.permission.state == "waiting" {
					return "Approval needed: " + valueOr(block.permission.title, "tool request")
				}
			case blockError:
				return "Error: " + firstMeaningfulLine(block.text)
			}
		}
	}
	return "No conversation yet"
}

func (t *transcript) latestState() turnState {
	if len(t.requests) == 0 {
		return ""
	}
	return t.requests[len(t.requests)-1].state
}

func payloadText(value any) (string, int) {
	if value == nil {
		return "", 0
	}
	switch typed := value.(type) {
	case string:
		return typed, 0
	case []any:
		var parts []string
		var lines int
		for _, item := range typed {
			text, hint := payloadText(item)
			if text != "" {
				parts = append(parts, text)
			}
			if hint > lines {
				lines = hint
			}
		}
		return strings.Join(parts, "\n"), lines
	case map[string]any:
		for _, key := range []string{"text", "content", "output", "stdout", "stderr", "oldText", "newText"} {
			if nested, exists := typed[key]; exists {
				text, hint := payloadText(nested)
				if text != "" {
					if explicit := int(numberValue(typed["numLines"])); explicit > hint {
						hint = explicit
					}
					if total := int(numberValue(typed["totalLines"])); total > hint {
						hint = total
					}
					return text, hint
				}
			}
		}
		for _, key := range []string{"file", "diff", "terminal", "result", "data"} {
			if nested, exists := typed[key]; exists {
				if text, hint := payloadText(nested); text != "" {
					return text, hint
				}
			}
		}
		encoded, err := json.Marshal(typed)
		if err == nil && string(encoded) != "{}" {
			return string(encoded), 0
		}
	}
	return "", 0
}

func sessionPromptText(value any) string {
	items, _ := value.([]any)
	var parts []string
	for _, item := range items {
		entry := object(item)
		if text := stringValue(entry["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func legacyDisplayMessage(value string) string {
	value = sanitizeDisplay(value)
	lines := strings.Split(value, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Task ") || !strings.Contains(lines[0], ":") {
		return value
	}
	title := strings.TrimSpace(strings.SplitN(lines[0], ":", 2)[1])
	description, role := "", "worker"
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "Description: "):
			description = strings.TrimSpace(strings.TrimPrefix(line, "Description: "))
		case strings.HasPrefix(line, "Role: "):
			role = strings.TrimSpace(strings.TrimPrefix(line, "Role: "))
		}
	}
	extra := ""
	if _, tail, found := strings.Cut(value, "\nSession-specific instructions:\n"); found {
		extra = strings.TrimSpace(tail)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "Start work on %q as %s.", title, firstNonEmpty(role, "worker"))
	if description != "" && description != "(none)" {
		fmt.Fprintf(&builder, "\n\n%s", description)
	}
	if extra != "" {
		fmt.Fprintf(&builder, "\n\n%s", extra)
	}
	return builder.String()
}

func contentIsSensitive(value any) bool {
	items, _ := value.([]any)
	for _, item := range items {
		kind := stringValue(object(item)["type"])
		if kind == "diff" || kind == "terminal" {
			return true
		}
	}
	return false
}

func toolMetadata(update map[string]any) string {
	input := object(update["rawInput"])
	for _, key := range []string{"command", "path", "filePath", "query", "pattern", "url"} {
		if value := sanitizeInline(stringValue(input[key])); value != "" {
			if key == "path" || key == "filePath" {
				return filepath.Base(value)
			}
			return value
		}
	}
	if locations, ok := update["locations"].([]any); ok {
		var paths []string
		for _, value := range locations {
			path := stringValue(object(value)["path"])
			if path != "" {
				paths = append(paths, filepath.Base(path))
			}
		}
		sort.Strings(paths)
		return strings.Join(paths, ", ")
	}
	return ""
}

func toolDisplayTitle(tool *toolCard) string {
	if tool == nil {
		return "Tool"
	}
	if tool.title != "" && !genericToolTitle(tool.title) {
		return tool.title
	}
	if tool.metadata != "" {
		return tool.metadata
	}
	if tool.kind != "" {
		return strings.Title(strings.ReplaceAll(tool.kind, "_", " ")) //nolint:staticcheck
	}
	return "Tool"
}

func genericToolTitle(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tool", "tool call", "terminal", "read", "edit":
		return true
	default:
		return false
	}
}

func permissionID(value any, toolCallID string) string {
	var rpc string
	switch typed := value.(type) {
	case string:
		rpc = typed
	case float64:
		rpc = fmt.Sprintf("%.0f", typed)
	case json.Number:
		rpc = typed.String()
	}
	if rpc != "" && toolCallID != "" {
		return rpc + ":" + toolCallID
	}
	return firstNonEmpty(rpc, toolCallID)
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func numberValue(value any) float64 {
	result, _ := value.(float64)
	return result
}

func jsonString(raw json.RawMessage, key string) string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return stringValue(value[key])
}

func sanitizeDisplay(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		}
		if unicode.IsControl(r) || r == '\u007f' {
			return -1
		}
		return r
	}, value)
}

func sanitizeInline(value string) string {
	value = sanitizeDisplay(value)
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func appendBounded(existing, addition string, limit int) (string, bool) {
	if len(existing) >= limit {
		return existing, true
	}
	remaining := limit - len(existing)
	if len(addition) <= remaining {
		return existing + addition, false
	}
	addition, _ = truncateUTF8(addition, remaining)
	return existing + addition, true
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	if limit <= 0 {
		return "", true
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func logicalLines(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func previewLines(value string, lineLimit, byteLimit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) > lineLimit {
		lines = lines[:lineLimit]
	}
	preview := strings.Join(lines, "\n")
	preview, clipped := truncateUTF8(preview, byteLimit)
	if clipped {
		preview += "…"
	}
	return preview
}

func firstMeaningfulLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func mergeField(existing, next string) string {
	next = sanitizeInline(next)
	if next == "" {
		return existing
	}
	return next
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func statusSuffix(status string) string {
	if strings.TrimSpace(status) == "" {
		return ""
	}
	return " · " + strings.ReplaceAll(status, "_", " ")
}

func syntheticRequestID(sequence uint64) string {
	return fmt.Sprintf("local-%d", sequence)
}
