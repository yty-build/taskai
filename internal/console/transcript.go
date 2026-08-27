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
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/worker"
)

const (
	maxProjectedMessageBytes      = 128 << 10
	maxProjectedThoughtBytes      = 32 << 10
	maxProjectedErrorBytes        = 4 << 10
	maxProjectedIdentifierBytes   = 256
	maxProjectedPermissionIDBytes = 512
	maxProjectedTitleBytes        = 1 << 10
	maxProjectedMetadataBytes     = 2 << 10
	maxProjectedKindBytes         = 128
	maxProjectedStatusBytes       = 128
	maxProjectedStopBytes         = 256
	maxProjectedDecisionBytes     = 1 << 10
	maxToolDetailBytes            = 64 << 10
	maxToolPreviewBytes           = 2 << 10
	maxProjectedRequests          = 128
	maxProjectedTools             = 128
	maxProjectedPermissions       = 128
	maxProjectedPermissionOptions = 16
	maxProjectedBlocks            = 512
	maxProjectedErrorBlocks       = 16
	maxProjectedLocations         = 32
	maxToolPayloadDigests         = 64
)

// transcript is an incremental, viewer-only projection of append-only Session
// history. It is rebuilt from JSONL on attachment and is never persisted as a
// third domain entity.
type transcript struct {
	requests []*transcriptRequest
	byID     map[string]*transcriptRequest
	lastSeq  uint64
	clipped  bool
}

type transcriptRequest struct {
	id             string
	state          turnState
	stop           string
	errorText      string
	blocks         []*transcriptBlock
	tools          map[string]*toolCard
	permission     map[string]*permissionCard
	assistantBytes int
	thoughtBytes   int
	assistantSeen  bool
	reasoningSeen  bool
	contentClipped bool
}

type turnState string

const (
	turnReady      turnState = "ready"
	turnLive       turnState = "live"
	turnCancelling turnState = "cancelling"
	turnComplete   turnState = "complete"
	turnIncomplete turnState = "incomplete"
	turnCancelled  turnState = "cancelled"
	turnFailed     turnState = "error"
)

type blockKind uint8

const (
	blockUser blockKind = iota
	blockAssistant
	blockThought
	blockTool
	blockPermission
	blockError
)

type transcriptBlock struct {
	kind       blockKind
	id         string
	text       string
	truncated  bool
	tool       *toolCard
	permission *permissionCard
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
	id       string
	title    string
	kind     string
	options  []permissionOption
	state    string
	decision string
}

type permissionOption struct {
	id   string
	name string
	kind string
}

type projectionUsage struct {
	bytes   int
	objects int
}

func newTranscript() *transcript { return &transcript{byID: make(map[string]*transcriptRequest)} }

func (t *transcript) retainedUsage() projectionUsage {
	if t == nil {
		return projectionUsage{}
	}
	usage := projectionUsage{bytes: 512, objects: 1}
	for _, request := range t.requests {
		usage.add(1024, request.id, request.stop, request.errorText)
		for _, block := range request.blocks {
			usage.add(192, block.id, block.text)
		}
		for _, tool := range request.tools {
			usage.add(1024, tool.id, tool.title, tool.kind, tool.status, tool.metadata,
				tool.preview, tool.detail, tool.detailLabel)
			usage.bytes += len(tool.seenPayloads) * 64
			usage.objects += len(tool.seenPayloads)
		}
		for _, permission := range request.permission {
			usage.add(512, permission.id, permission.title, permission.kind, permission.state, permission.decision)
			for _, option := range permission.options {
				usage.add(192, option.id, option.name, option.kind)
			}
		}
	}
	return usage
}

func (u *projectionUsage) add(overhead int, values ...string) {
	u.bytes += overhead
	u.objects++
	for _, value := range values {
		u.bytes += len(value)
	}
}

func (t *transcript) apply(line sessionlog.Line) bool {
	if line.Seq == 0 {
		return false
	}
	if line.Seq <= t.lastSeq {
		return false
	}
	if (t.lastSeq == 0 && line.Seq > 1) || (t.lastSeq != 0 && line.Seq != t.lastSeq+1) {
		t.clipped = true
	}
	t.lastSeq = line.Seq
	request := t.request(line.RequestID)
	switch line.Type {
	case worker.FrameClientCommand:
		command := jsonString(line.Data, "command")
		if request == nil {
			break
		}
		switch command {
		case "prompt":
			request.setUserMessage(jsonString(line.Data, "text"))
			request.state = turnReady
		case "cancel":
			request.state = turnCancelling
		}
	case worker.FrameProviderEvent:
		if request != nil {
			request.state = turnLive
			t.applyProviderEvent(request, line.Data)
		}
	case worker.FrameProviderResult:
		if request != nil {
			request.applyProviderResult(line.Data)
		}
	case worker.FrameWorkerError:
		if request == nil {
			request = t.request(syntheticRequestID(line.Seq))
		}
		request.state = turnFailed
		request.appendError(jsonString(line.Data, "message"), "The Session worker encountered an error.")
	case worker.FramePermissionResolved:
		if request != nil {
			request.resolvePermission(jsonString(line.Data, "permission_id"), jsonString(line.Data, "decision"))
		}
	case worker.FramePermissionRequested:
		if request != nil {
			var pending worker.PendingPermission
			if json.Unmarshal(line.Data, &pending) == nil {
				request.upsertWorkerPermission(pending)
			}
		}
	case worker.FramePermissionAbandoned:
		if request != nil {
			decision := "cancelled"
			if message := jsonString(line.Data, "message"); message != "" {
				decision = "cancelled · " + message
			}
			request.resolvePermission(jsonString(line.Data, "permission_id"), decision)
		}
	}
	return true
}

func (t *transcript) applyProviderEvent(request *transcriptRequest, data json.RawMessage) {
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return
	}
	switch stringValue(event["type"]) {
	case "text_delta":
		text := stringValue(event["text"])
		if stream := strings.ToLower(stringValue(event["stream"])); stream == "thought" || stream == "reasoning" {
			request.appendThought(text)
		} else {
			request.appendMessage(blockAssistant, text, firstNonEmpty(stringValue(event["message_id"]), stringValue(event["messageId"])), maxProjectedMessageBytes)
		}
	case "tool_call":
		request.upsertTool(normalizedToolUpdate(event))
	case "error":
		message := firstNonEmpty(stringValue(event["message"]), stringValue(object(event["error"])["message"]))
		if message != "" {
			request.state = turnFailed
			request.appendError(message, "The provider reported an error.")
		}
	}
}

func normalizedToolUpdate(event map[string]any) map[string]any {
	update := event
	for _, key := range []string{"tool_call", "toolCall", "update"} {
		if nested := object(event[key]); len(nested) > 0 {
			update = nested
			break
		}
	}
	copy := make(map[string]any, len(update)+1)
	for key, value := range update {
		copy[key] = value
	}
	copy["toolCallId"] = firstNonEmpty(
		stringValue(update["toolCallId"]), stringValue(update["tool_call_id"]), stringValue(update["id"]),
	)
	return copy
}

func (r *transcriptRequest) applyProviderResult(data json.RawMessage) {
	status := strings.ToLower(jsonString(data, "status"))
	var stopClipped bool
	r.stop, stopClipped = boundedInline(jsonString(data, "stop_reason"), maxProjectedStopBytes)
	r.contentClipped = r.contentClipped || stopClipped
	switch status {
	case "completed":
		r.state = turnComplete
	case "cancelled":
		r.state = turnCancelled
	case "failed":
		r.state = turnFailed
		r.appendError(jsonString(data, "message"), "The provider turn failed.")
	}
}

func (t *transcript) request(id string) *transcriptRequest {
	var idClipped bool
	id, idClipped = boundedIdentifier(id, maxProjectedIdentifierBytes)
	if strings.TrimSpace(id) == "" {
		return nil
	}
	t.clipped = t.clipped || idClipped
	if existing := t.byID[id]; existing != nil {
		return existing
	}
	request := &transcriptRequest{
		id: id, tools: make(map[string]*toolCard), permission: make(map[string]*permissionCard),
	}
	t.byID[id] = request
	t.requests = append(t.requests, request)
	if len(t.requests) > maxProjectedRequests {
		removed := t.requests[0]
		delete(t.byID, removed.id)
		t.requests = append([]*transcriptRequest(nil), t.requests[1:]...)
		t.clipped = true
	}
	return request
}

func (r *transcriptRequest) setUserMessage(text string) {
	text = sanitizeDisplay(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	var clipped bool
	text, clipped = truncateUTF8(text, maxProjectedMessageBytes)
	r.contentClipped = r.contentClipped || clipped
	for _, block := range r.blocks {
		if block.kind != blockUser {
			continue
		}
		block.text = text
		block.truncated = block.truncated || clipped
		return
	}
	r.appendBlock(&transcriptBlock{kind: blockUser, text: text, truncated: clipped})
}

func (r *transcriptRequest) appendMessage(kind blockKind, text, id string, limit int) {
	text = sanitizeDisplay(text)
	if text == "" {
		return
	}
	if kind == blockAssistant && strings.TrimSpace(text) != "" {
		r.assistantSeen = true
	}
	var idClipped bool
	id, idClipped = boundedInline(id, maxProjectedIdentifierBytes)
	r.contentClipped = r.contentClipped || idClipped
	text, clipped := truncateUTF8Tail(text, limit)
	r.contentClipped = r.contentClipped || clipped
	r.makeRollingTextRoom(kind, len(text), limit)
	var block *transcriptBlock
	if len(r.blocks) > 0 {
		candidate := r.blocks[len(r.blocks)-1]
		if candidate.kind == kind && (candidate.id == id || candidate.id == "" || id == "") {
			block = candidate
		}
	}
	if block == nil {
		block = &transcriptBlock{kind: kind, id: id}
		if !r.appendBlock(block) {
			return
		}
	}
	block.text += text
	block.truncated = block.truncated || clipped
	r.addRetainedBytes(kind, len(text))
}

func (r *transcriptRequest) appendThought(text string) {
	text = sanitizeDisplay(text)
	if text == "" {
		return
	}
	if strings.TrimSpace(text) != "" {
		r.reasoningSeen = true
	}
	text, clipped := truncateUTF8Tail(text, maxProjectedThoughtBytes)
	r.contentClipped = r.contentClipped || clipped
	r.makeRollingTextRoom(blockThought, len(text), maxProjectedThoughtBytes)
	var block *transcriptBlock
	if len(r.blocks) > 0 && r.blocks[len(r.blocks)-1].kind == blockThought {
		block = r.blocks[len(r.blocks)-1]
	}
	if block == nil {
		block = &transcriptBlock{kind: blockThought}
		if !r.appendBlock(block) {
			return
		}
	}
	block.text += text
	block.truncated = block.truncated || clipped
	r.addRetainedBytes(blockThought, len(text))
}

func (r *transcriptRequest) appendBlock(block *transcriptBlock) bool {
	if len(r.blocks) >= maxProjectedBlocks {
		r.contentClipped = true
		return false
	}
	r.blocks = append(r.blocks, block)
	return true
}

func (r *transcriptRequest) appendError(message, fallback string) {
	message = sanitizeDisplay(message)
	if strings.TrimSpace(message) == "" {
		message = fallback
	}
	message, clipped := truncateUTF8(message, maxProjectedErrorBytes)
	r.errorText = message
	r.contentClipped = r.contentClipped || clipped
	errorBlocks := 0
	lastError := -1
	for index, block := range r.blocks {
		if block.kind == blockError {
			errorBlocks++
			lastError = index
		}
	}
	if errorBlocks >= maxProjectedErrorBlocks || len(r.blocks) >= maxProjectedBlocks {
		r.contentClipped = true
		if lastError >= 0 {
			r.blocks[lastError].text = message
			r.blocks[lastError].truncated = true
		}
		return
	}
	r.appendBlock(&transcriptBlock{kind: blockError, text: message, truncated: clipped})
}

func (r *transcriptRequest) retainedBytes(kind blockKind) int {
	if kind == blockThought {
		return r.thoughtBytes
	}
	return r.assistantBytes
}

func (r *transcriptRequest) addRetainedBytes(kind blockKind, amount int) {
	if kind == blockThought {
		r.thoughtBytes += amount
		return
	}
	r.assistantBytes += amount
}

func (r *transcriptRequest) makeRollingTextRoom(kind blockKind, incomingBytes, limit int) {
	needed := max(0, r.retainedBytes(kind)+incomingBytes-limit)
	for index := 0; needed > 0 && index < len(r.blocks); {
		block := r.blocks[index]
		if block.kind != kind || block.text == "" {
			index++
			continue
		}
		trimmed, dropped := dropUTF8Prefix(block.text, min(needed, len(block.text)))
		block.text = trimmed
		block.truncated = true
		r.contentClipped = true
		r.addRetainedBytes(kind, -dropped)
		needed = max(0, r.retainedBytes(kind)+incomingBytes-limit)
		if block.text == "" {
			r.blocks = append(r.blocks[:index], r.blocks[index+1:]...)
			continue
		}
		index++
	}
}

func (r *transcriptRequest) completedResponse() *transcriptBlock {
	if r.state != turnComplete {
		return nil
	}
	for index := len(r.blocks) - 1; index >= 0; index-- {
		block := r.blocks[index]
		if block.kind == blockAssistant && strings.TrimSpace(block.text) != "" {
			return block
		}
	}
	return nil
}

func (r *transcriptRequest) hasRetainedReasoning() bool {
	for _, block := range r.blocks {
		if block.kind == blockThought && strings.TrimSpace(block.text) != "" {
			return true
		}
	}
	return false
}

func (r *transcriptRequest) upsertTool(update map[string]any) {
	id, idClipped := boundedInline(stringValue(update["toolCallId"]), maxProjectedIdentifierBytes)
	r.contentClipped = r.contentClipped || idClipped
	if id == "" {
		return
	}
	tool := r.tools[id]
	if tool == nil {
		if len(r.tools) >= maxProjectedTools {
			r.contentClipped = true
			return
		}
		tool = &toolCard{id: id, seenPayloads: make(map[[sha256.Size]byte]struct{})}
		if !r.appendBlock(&transcriptBlock{kind: blockTool, id: id, tool: tool}) {
			return
		}
		r.tools[id] = tool
	}
	tool.title = r.mergeProjectedField(tool.title, stringValue(update["title"]), maxProjectedTitleBytes)
	tool.kind = r.mergeProjectedField(tool.kind, stringValue(update["kind"]), maxProjectedKindBytes)
	tool.status = r.mergeProjectedField(tool.status, stringValue(update["status"]), maxProjectedStatusBytes)
	metadata, metadataClipped := toolMetadata(update)
	r.contentClipped = r.contentClipped || metadataClipped
	tool.metadata = r.mergeProjectedField(tool.metadata, metadata, maxProjectedMetadataBytes)
	tool.consumePayloads(update)
	r.contentClipped = r.contentClipped || tool.detailClipped
}

func (r *transcriptRequest) upsertWorkerPermission(pending worker.PendingPermission) {
	id, idClipped := boundedIdentifier(pending.ID, maxProjectedPermissionIDBytes)
	r.contentClipped = r.contentClipped || idClipped
	if id == "" {
		return
	}
	card := r.permission[id]
	if card == nil {
		if len(r.permission) >= maxProjectedPermissions {
			r.contentClipped = true
			return
		}
		card = &permissionCard{id: id, state: "waiting"}
		if !r.appendBlock(&transcriptBlock{kind: blockPermission, id: id, permission: card}) {
			return
		}
		r.permission[id] = card
	}
	card.state = "waiting"
	card.decision = ""
	card.title = r.mergeProjectedField(card.title, pending.Title, maxProjectedTitleBytes)
	card.kind = r.mergeProjectedField(card.kind, pending.Kind, maxProjectedKindBytes)
	optionCount := min(len(pending.Options), maxProjectedPermissionOptions)
	card.options = make([]permissionOption, 0, optionCount)
	if len(pending.Options) > optionCount {
		r.contentClipped = true
	}
	for _, option := range pending.Options[:optionCount] {
		optionID, optionIDClipped := boundedIdentifier(option.ID, maxProjectedIdentifierBytes)
		name, nameClipped := boundedInline(option.Name, maxProjectedTitleBytes)
		kind, kindClipped := boundedInline(option.Kind, maxProjectedKindBytes)
		r.contentClipped = r.contentClipped || optionIDClipped || nameClipped || kindClipped
		card.options = append(card.options, permissionOption{id: optionID, name: name, kind: kind})
	}
}

func (r *transcriptRequest) resolvePermission(id, decision string) {
	var idClipped bool
	id, idClipped = boundedIdentifier(id, maxProjectedPermissionIDBytes)
	r.contentClipped = r.contentClipped || idClipped
	if card := r.permission[id]; card != nil {
		card.state = "resolved"
		var decisionClipped bool
		card.decision, decisionClipped = boundedInline(decision, maxProjectedDecisionBytes)
		r.contentClipped = r.contentClipped || decisionClipped
	}
}

func (r *transcriptRequest) mergeProjectedField(existing, next string, limit int) string {
	next, clipped := boundedInline(next, limit)
	r.contentClipped = r.contentClipped || clipped
	if next == "" {
		return existing
	}
	return next
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
	if len(t.seenPayloads) >= maxToolPayloadDigests {
		t.detailClipped = true
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
	var clipped bool
	t.detail, clipped = truncateUTF8(clean, maxToolDetailBytes)
	t.detailClipped = t.detailClipped || clipped
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

func boundedPendingPermissions(snapshot []worker.PendingPermission) ([]worker.PendingPermission, bool) {
	count := min(len(snapshot), maxProjectedPermissions)
	result := make([]worker.PendingPermission, 0, count)
	clipped := len(snapshot) > count
	for _, pending := range snapshot[:count] {
		bounded := worker.PendingPermission{}
		var fieldClipped bool
		bounded.ID, fieldClipped = boundedIdentifier(pending.ID, maxProjectedPermissionIDBytes)
		clipped = clipped || fieldClipped
		bounded.RequestID, fieldClipped = boundedIdentifier(pending.RequestID, maxProjectedIdentifierBytes)
		clipped = clipped || fieldClipped
		bounded.Title, fieldClipped = boundedInline(pending.Title, maxProjectedTitleBytes)
		clipped = clipped || fieldClipped
		bounded.Kind, fieldClipped = boundedInline(pending.Kind, maxProjectedKindBytes)
		clipped = clipped || fieldClipped
		optionCount := min(len(pending.Options), maxProjectedPermissionOptions)
		if len(pending.Options) > optionCount {
			clipped = true
		}
		bounded.Options = make([]worker.PermissionOption, 0, optionCount)
		for _, option := range pending.Options[:optionCount] {
			boundedOption := worker.PermissionOption{}
			boundedOption.ID, fieldClipped = boundedIdentifier(option.ID, maxProjectedIdentifierBytes)
			clipped = clipped || fieldClipped
			boundedOption.Name, fieldClipped = boundedInline(option.Name, maxProjectedTitleBytes)
			clipped = clipped || fieldClipped
			boundedOption.Kind, fieldClipped = boundedInline(option.Kind, maxProjectedKindBytes)
			clipped = clipped || fieldClipped
			bounded.Options = append(bounded.Options, boundedOption)
		}
		result = append(result, bounded)
	}
	return result, clipped
}

// reconcilePendingPermissions merges the worker's bounded status snapshot into
// this disposable projection. The snapshot is authoritative for which cards
// are still waiting; it does not synthesize Session Channel frames or advance
// the replay cursor.
func (t *transcript) reconcilePendingPermissions(snapshot []worker.PendingPermission) {
	if t == nil {
		return
	}
	limit := min(len(snapshot), maxProjectedPermissions)
	if len(snapshot) > limit {
		t.clipped = true
	}
	waiting := make(map[*permissionCard]struct{}, limit)
	for _, pending := range snapshot[:limit] {
		request := t.request(pending.RequestID)
		if request == nil {
			t.clipped = true
			continue
		}
		request.upsertWorkerPermission(pending)
		id, _ := boundedIdentifier(pending.ID, maxProjectedPermissionIDBytes)
		if card := request.permission[id]; card != nil {
			waiting[card] = struct{}{}
		}
	}
	for _, request := range t.requests {
		for _, card := range request.permission {
			if card.state != "waiting" {
				continue
			}
			if _, current := waiting[card]; current {
				continue
			}
			card.state = "resolved"
			card.decision = ""
		}
	}
}

func (t *transcript) compactToPendingPermissions() bool {
	type retainedRequest struct {
		request *transcriptRequest
		cards   []*permissionCard
	}
	retained := make([]retainedRequest, 0, len(t.requests))
	alreadyCompact := true
	for _, request := range t.requests {
		cards := make([]*permissionCard, 0, len(request.permission))
		for _, block := range request.blocks {
			if block.permission != nil && block.permission.state == "waiting" {
				cards = append(cards, block.permission)
			}
		}
		if len(cards) == 0 {
			alreadyCompact = false
			continue
		}
		retained = append(retained, retainedRequest{request: request, cards: cards})
		if len(request.blocks) != len(cards) || len(request.tools) != 0 || len(request.permission) != len(cards) ||
			request.stop != "" || request.errorText != "" || request.assistantBytes != 0 || request.thoughtBytes != 0 {
			alreadyCompact = false
			continue
		}
		for index, block := range request.blocks {
			if block.permission != cards[index] {
				alreadyCompact = false
				break
			}
		}
	}
	if len(retained) == 0 || alreadyCompact && len(retained) == len(t.requests) {
		return false
	}

	requests := make([]*transcriptRequest, 0, len(retained))
	byID := make(map[string]*transcriptRequest, len(retained))
	for _, item := range retained {
		request := item.request
		blocks := make([]*transcriptBlock, 0, len(item.cards))
		permissions := make(map[string]*permissionCard, len(item.cards))
		for _, card := range item.cards {
			blocks = append(blocks, &transcriptBlock{kind: blockPermission, id: card.id, permission: card})
			permissions[card.id] = card
		}
		request.stop = ""
		request.errorText = ""
		request.blocks = blocks
		request.tools = make(map[string]*toolCard)
		request.permission = permissions
		request.assistantBytes = 0
		request.thoughtBytes = 0
		request.contentClipped = true
		requests = append(requests, request)
		byID[request.id] = request
	}
	t.requests = requests
	t.byID = byID
	t.clipped = true
	return true
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

func toolMetadata(update map[string]any) (string, bool) {
	input := object(update["rawInput"])
	for _, key := range []string{"command", "path", "filePath", "query", "pattern", "url"} {
		if value := sanitizeInline(stringValue(input[key])); value != "" {
			if key == "path" || key == "filePath" {
				value = filepath.Base(value)
			}
			value, clipped := truncateUTF8(value, maxProjectedMetadataBytes)
			return value, clipped
		}
	}
	if locations, ok := update["locations"].([]any); ok {
		locationCount := min(len(locations), maxProjectedLocations)
		paths := make([]string, 0, locationCount)
		clipped := len(locations) > locationCount
		for _, value := range locations[:locationCount] {
			path := sanitizeInline(stringValue(object(value)["path"]))
			if path == "" {
				continue
			}
			path, pathClipped := boundedInline(filepath.Base(path), maxProjectedTitleBytes)
			clipped = clipped || pathClipped
			if path != "" {
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		metadata, metadataClipped := truncateUTF8(strings.Join(paths, ", "), maxProjectedMetadataBytes)
		return metadata, clipped || metadataClipped
	}
	return "", false
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
		if !unicode.IsPrint(r) {
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

func boundedInline(value string, limit int) (string, bool) {
	return truncateUTF8(sanitizeInline(value), limit)
}

func boundedIdentifier(value string, limit int) (string, bool) {
	safe := strings.Map(func(item rune) rune {
		if !unicode.IsPrint(item) {
			return -1
		}
		return item
	}, value)
	bounded, clipped := truncateUTF8(safe, limit)
	return bounded, clipped || safe != value
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

func truncateUTF8Tail(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	if limit <= 0 {
		return "", true
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:], true
}

func dropUTF8Prefix(value string, minimumBytes int) (string, int) {
	if minimumBytes <= 0 || value == "" {
		return value, 0
	}
	dropped := 0
	for dropped < len(value) && dropped < minimumBytes {
		_, size := utf8.DecodeRuneInString(value[dropped:])
		dropped += size
	}
	return value[dropped:], dropped
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
