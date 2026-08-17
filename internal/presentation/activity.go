// Package presentation projects append-only Session history into bounded,
// sanitized view data. The projection is disposable UI state: it is not a
// third persistent entity and never becomes part of the Task/Session model.
package presentation

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
	maxMessageBytes = 128 << 10
	maxThoughtBytes = 32 << 10
	maxToolBytes    = 64 << 10
	maxPreviewBytes = 2 << 10
)

// Activity is a safe, semantic browser projection of one Session log line.
// It deliberately omits ACP envelopes, worker fields, provider identifiers,
// environment values, and unbounded raw input.
type Activity struct {
	Type        string      `json:"type"`
	MessageID   string      `json:"message_id,omitempty"`
	Text        string      `json:"text,omitempty"`
	State       string      `json:"state,omitempty"`
	StopReason  string      `json:"stop_reason,omitempty"`
	ErrorSource string      `json:"error_source,omitempty"`
	ErrorCode   string      `json:"error_code,omitempty"`
	Retryable   *bool       `json:"retryable,omitempty"`
	Tool        *Tool       `json:"tool,omitempty"`
	Plan        []PlanEntry `json:"plan,omitempty"`
	Truncated   bool        `json:"truncated,omitempty"`
}

type Tool struct {
	ID            string `json:"id"`
	Title         string `json:"title,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Status        string `json:"status,omitempty"`
	Metadata      string `json:"metadata,omitempty"`
	Preview       string `json:"preview,omitempty"`
	Detail        string `json:"detail,omitempty"`
	DetailLabel   string `json:"detail_label,omitempty"`
	DetailClipped bool   `json:"detail_clipped,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
}

type PlanEntry struct {
	Content string `json:"content"`
	Status  string `json:"status,omitempty"`
}

// Project returns nil for activity that belongs in diagnostics rather than
// the primary conversation. The existing normalized Activity string remains
// available to clients for that advanced view.
func Project(line sessionlog.Line) *Activity {
	switch line.Type {
	case "prompt_submitted":
		text := firstJSONText(line.Data, "display_message", "message")
		text, clipped := boundedDisplay(text, maxMessageBytes)
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return &Activity{Type: "user_message", Text: text, Truncated: clipped}
	case "prompt_queued":
		return &Activity{Type: "turn_state", State: "queued"}
	case "prompt_ready":
		return &Activity{Type: "turn_state", State: "ready"}
	case "turn_started":
		return &Activity{Type: "turn_state", State: "working"}
	case "cancel_requested":
		return &Activity{Type: "turn_state", State: "cancelling"}
	case "turn_completed":
		stop := safeStopReason(firstJSONText(line.Data, "stopReason", "stop_reason"))
		state := "complete"
		if stop == "cancelled" {
			state = "cancelled"
		}
		return &Activity{Type: "turn_state", State: state, StopReason: stop}
	case "turn_error", "worker_error":
		// Worker errors may include provider stderr, local paths, or other
		// diagnostic detail. Keep that in the private log and expose only a
		// stable, actionable message at the browser boundary.
		text := "The provider turn failed. Check the native provider CLI or local server logs for details."
		if line.Type == "turn_error" {
			var failure struct {
				Source    string `json:"source"`
				Code      string `json:"code"`
				Retryable bool   `json:"retryable"`
			}
			if json.Unmarshal(line.Data, &failure) == nil && failure.Source == "provider" {
				code := safeProviderFailureCode(failure.Code)
				retryable := failure.Retryable
				return &Activity{
					Type: "error_upsert", Text: safeProviderFailureText(code, retryable), State: "error",
					ErrorSource: "provider", ErrorCode: code, Retryable: &retryable,
				}
			}
		}
		if line.Type == "worker_error" {
			text = "The Session worker encountered an error. Check local server logs for details."
		}
		return &Activity{Type: "error_upsert", Text: text, State: "error"}
	case "permission_requested":
		return &Activity{Type: "permission", State: "waiting"}
	case "permission_resolved":
		return &Activity{Type: "permission", State: "resolved"}
	case "permission_abandoned":
		return &Activity{Type: "permission", State: "abandoned"}
	case "acp":
		return projectACP(line.Data)
	default:
		return nil
	}
}

func safeProviderFailureCode(code string) string {
	switch code {
	case "busy", "conflict", "rate_limited", "rejected", "provider_error":
		return code
	default:
		return "provider_error"
	}
}

func safeProviderFailureText(code string, retryable bool) string {
	switch code {
	case "busy":
		return "The provider is busy with this conversation. Wait for the current provider turn to settle, then try again."
	case "conflict":
		return "The provider reported a conversation conflict. Wait for the other provider turn to settle, then try again."
	case "rate_limited":
		return "The provider rate-limited this turn. Try again shortly."
	case "rejected":
		if retryable {
			return "The provider rejected this turn. Try again after its current condition clears."
		}
		return "The provider rejected this turn."
	default:
		if retryable {
			return "The provider could not complete this turn. Try again."
		}
		return "The provider could not complete this turn."
	}
}

// Summary preserves the legacy human-readable ActivityDTO field without
// forwarding raw tool payloads or arbitrary JSON values across the browser
// boundary. Rich clients should prefer Display.
func Summary(line sessionlog.Line, display *Activity) string {
	if display != nil {
		switch display.Type {
		case "user_message", "legacy_user_message":
			return "User: " + summaryText(display.Text)
		case "assistant_chunk":
			return summaryText(display.Text)
		case "thought_chunk":
			return "Reasoning update"
		case "tool_upsert":
			if display.Tool != nil {
				title := display.Tool.Title
				if title == "" {
					title = "Tool"
				}
				state := strings.ReplaceAll(display.Tool.Status, "_", " ")
				if state != "" {
					return title + " · " + state
				}
				return title
			}
		case "plan_replace":
			return "Plan updated"
		case "error_upsert":
			return "Error: " + summaryText(display.Text)
		case "permission":
			return "Permission " + display.State
		case "turn_state":
			return "Turn " + display.State
		}
	}
	// Do not fall back to arbitrary log data here. The legacy field crosses
	// the public API boundary and therefore has the same privacy contract as
	// the semantic projection above.
	return strings.ReplaceAll(line.Type, "_", " ")
}

// DiagnosticSummary names gateway and ACP activity without forwarding raw
// envelopes or their provider-controlled text. It gives advanced viewers a
// useful bounded timeline while keeping protocol data in the trusted log.
func DiagnosticSummary(line sessionlog.Line) string {
	if line.Type != "acp" {
		return Summary(sessionlog.Line{Type: line.Type}, nil)
	}
	var event acp.Event
	if json.Unmarshal(line.Data, &event) != nil {
		return "ACP update"
	}
	var raw map[string]any
	if json.Unmarshal(event.Raw, &raw) != nil {
		return "ACP update"
	}
	update := object(object(raw["params"])["update"])
	switch event.Type {
	case "agent_message_chunk":
		return "ACP output chunk"
	case "agent_thought_chunk":
		return "ACP reasoning chunk"
	case "tool_call":
		return "ACP tool call"
	case "tool_call_update":
		return "ACP tool update"
	case "plan", "plan_update":
		return "ACP plan update"
	case "available_commands_update":
		count := len(arrayValue(update["availableCommands"]))
		if count > 0 {
			return fmt.Sprintf("ACP capabilities updated · %d commands", count)
		}
		return "ACP capabilities updated"
	case "usage_update":
		used, usedOK := safeWholeNumber(update["used"])
		limit, limitOK := safeWholeNumber(update["size"])
		if usedOK && limitOK {
			return fmt.Sprintf("ACP context usage · %d / %d", used, limit)
		}
		if usedOK {
			return fmt.Sprintf("ACP context usage · %d", used)
		}
		return "ACP context usage updated"
	case "session_info_update":
		return "ACP session metadata updated"
	case "session_request_permission":
		return "ACP permission requested"
	case "prompt_result":
		return "ACP turn result"
	case "error":
		return "ACP error"
	case "session_prompt", "user_message_chunk":
		return "ACP user message update"
	default:
		return "ACP update"
	}
}

func arrayValue(value any) []any {
	result, _ := value.([]any)
	return result
}

func safeWholeNumber(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number < 0 || number > float64(1<<62) {
		return 0, false
	}
	return int64(number), true
}

func summaryText(value string) string {
	value, clipped := truncateUTF8(cleanDisplay(value), 2048)
	if clipped {
		return value + "…"
	}
	return value
}

func safeStopReason(value string) string {
	value, _ = truncateUTF8(value, 256)
	value = strings.ToLower(strings.ReplaceAll(cleanInline(value), "-", "_"))
	switch value {
	case "canceled":
		return "cancelled"
	case "end_turn", "complete", "completed", "cancelled", "max_tokens", "max_turns", "refusal", "error", "failed":
		return value
	case "":
		return ""
	default:
		return "other"
	}
}

func projectACP(data json.RawMessage) *Activity {
	var event acp.Event
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal(event.Raw, &raw) != nil {
		return nil
	}
	params := object(raw["params"])
	update := object(params["update"])
	switch event.Type {
	case "session_prompt":
		prompt, sourceClipped := sessionPromptText(params["prompt"])
		text := CleanUserMessage(prompt)
		text, clipped := boundedDisplay(text, maxMessageBytes)
		if text == "" {
			return nil
		}
		return &Activity{Type: "legacy_user_message", Text: text, Truncated: clipped || sourceClipped}
	case "agent_message_chunk":
		text, clipped := boundedDisplay(stringValue(object(update["content"])["text"]), maxMessageBytes)
		if text == "" {
			return nil
		}
		return &Activity{Type: "assistant_chunk", MessageID: opaqueKey(stringValue(update["messageId"])), Text: text, Truncated: clipped}
	case "agent_thought_chunk":
		text, clipped := boundedDisplay(stringValue(object(update["content"])["text"]), maxThoughtBytes)
		if text == "" {
			return nil
		}
		return &Activity{Type: "thought_chunk", Text: text, Truncated: clipped}
	case "tool_call", "tool_call_update":
		tool := projectTool(update)
		if tool == nil {
			return nil
		}
		return &Activity{Type: "tool_upsert", Tool: tool}
	case "plan", "plan_update":
		return &Activity{Type: "plan_replace", Plan: projectPlan(update)}
	case "error":
		return &Activity{Type: "error_upsert", Text: "The provider reported an error. Check the native provider CLI or local server logs for details.", State: "error"}
	case "prompt_result":
		stop := safeStopReason(stringValue(object(raw["result"])["stopReason"]))
		if stop == "" {
			return nil
		}
		state := "complete"
		if stop == "cancelled" {
			state = "cancelled"
		}
		return &Activity{Type: "turn_state", State: state, StopReason: stop}
	default:
		return nil
	}
}

func projectTool(update map[string]any) *Tool {
	id := opaqueKey(stringValue(update["toolCallId"]))
	if id == "" {
		return nil
	}
	tool := &Tool{
		ID: id, Title: boundedInline(stringValue(update["title"]), 1024), Kind: boundedInline(stringValue(update["kind"]), 64),
		Status: boundedInline(stringValue(update["status"]), 64), Metadata: toolMetadata(update),
	}
	label, detail, sensitive, sourceClipped := toolDetail(update)
	if detail != "" {
		tool.DetailLabel = label
		tool.Detail, tool.DetailClipped = truncateUTF8(cleanDisplay(detail), maxToolBytes)
		tool.DetailClipped = tool.DetailClipped || sourceClipped
		tool.Sensitive = sensitive
		if !sensitive && len(tool.Detail) <= maxPreviewBytes {
			tool.Preview = previewLines(tool.Detail, 3, 320)
		}
	}
	return tool
}

func projectPlan(update map[string]any) []PlanEntry {
	values, _ := update["entries"].([]any)
	if len(values) > 128 {
		values = values[:128]
	}
	entries := make([]PlanEntry, 0, len(values))
	remaining := maxMessageBytes
	for _, value := range values {
		entry := object(value)
		content, _ := boundedDisplay(stringValue(entry["content"]), remaining)
		if strings.TrimSpace(content) == "" {
			continue
		}
		entries = append(entries, PlanEntry{Content: content, Status: boundedInline(stringValue(entry["status"]), 64)})
		remaining -= len(content)
		if remaining <= 0 {
			break
		}
	}
	return entries
}

func toolMetadata(update map[string]any) string {
	input := object(update["rawInput"])
	for _, key := range []string{"path", "filePath"} {
		value := cleanInline(stringValue(input[key]))
		if value == "" {
			continue
		}
		return boundedInline(filepath.Base(value), 512)
	}
	if locations, ok := update["locations"].([]any); ok {
		if len(locations) > 8 {
			locations = locations[:8]
		}
		paths := make([]string, 0, len(locations))
		for _, value := range locations {
			if path := stringValue(object(value)["path"]); path != "" {
				paths = append(paths, boundedInline(filepath.Base(path), 128))
			}
		}
		sort.Strings(paths)
		return boundedInline(strings.Join(paths, ", "), 512)
	}
	return ""
}

func toolDetail(update map[string]any) (string, string, bool, bool) {
	if content, exists := update["content"]; exists {
		if text, sensitive, clipped := knownToolContent(content); text != "" {
			return "result", text, sensitive, clipped
		}
		if content != nil {
			return "result", "Additional output is available in the native provider CLI.", false, false
		}
	}
	return "", "", false, false
}

func knownToolContent(value any) (string, bool, bool) {
	items, _ := value.([]any)
	var output boundedBuilder
	output.remaining = maxToolBytes
	sensitive := false
	for index, item := range items {
		if output.remaining == 0 {
			output.clipped = index < len(items)
			break
		}
		entry := object(item)
		switch stringValue(entry["type"]) {
		case "content":
			content := object(entry["content"])
			if stringValue(content["type"]) == "text" {
				if text := stringValue(content["text"]); text != "" {
					output.appendPart(text)
				}
			}
		case "diff":
			path := boundedInline(filepath.Base(stringValue(entry["path"])), 512)
			output.appendPart(path + "\n--- before")
			output.appendPart(stringValue(entry["oldText"]))
			output.appendPart("+++ after")
			output.appendPart(stringValue(entry["newText"]))
			sensitive = true
		case "terminal":
			for _, key := range []string{"stdout", "stderr", "output", "text"} {
				if text := stringValue(entry[key]); text != "" {
					output.appendPart(text)
				}
			}
			sensitive = true
		}
	}
	return output.String(), sensitive, output.clipped
}

// boundedBuilder applies the public projection limit while walking provider
// content, instead of first constructing a potentially multi-megabyte string.
type boundedBuilder struct {
	strings.Builder
	remaining int
	wrote     bool
	clipped   bool
}

func (b *boundedBuilder) appendPart(value string) {
	if value == "" {
		return
	}
	if b.remaining <= 0 {
		b.clipped = true
		return
	}
	if b.wrote {
		b.append("\n")
	}
	b.append(value)
	b.wrote = true
}

func (b *boundedBuilder) append(value string) {
	if value == "" {
		return
	}
	if b.remaining <= 0 {
		b.clipped = true
		return
	}
	scanLimit := b.remaining * 4
	if scanLimit < b.remaining || scanLimit > maxToolBytes*4 {
		scanLimit = maxToolBytes * 4
	}
	raw, sourceClipped := truncateUTF8(value, scanLimit)
	clean := cleanDisplay(raw)
	part, clipped := truncateUTF8(clean, b.remaining)
	_, _ = b.WriteString(part)
	b.remaining -= len(part)
	b.clipped = b.clipped || sourceClipped || clipped
}

func firstJSONText(raw json.RawMessage, keys ...string) string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range keys {
		if text := stringValue(value[key]); text != "" {
			return text
		}
	}
	return ""
}

func sessionPromptText(value any) (string, bool) {
	items, _ := value.([]any)
	var output boundedBuilder
	output.remaining = maxMessageBytes
	for _, item := range items {
		if text := stringValue(object(item)["text"]); text != "" {
			output.appendPart(text)
		}
	}
	return output.String(), output.clipped
}

// CleanUserMessage converts only the Agent Task Console's known injected
// Task/Session envelope into its human-facing prompt. Arbitrary provider user
// text is returned unchanged. This keeps Session IDs, checkout paths, and
// branches out of provider-backed UI history without weakening the provider
// record used internally for exact reconciliation.
func CleanUserMessage(value string) string {
	value = cleanDisplay(value)
	header, extra, hasExtra := strings.Cut(value, "\nSession-specific instructions:\n")
	lines := strings.Split(header, "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Task ") || !strings.Contains(lines[0], ":") {
		return value
	}
	title := strings.TrimSpace(strings.SplitN(lines[0], ":", 2)[1])
	description, role := "", "worker"
	seenSession, seenRole, seenRepository := false, false, false
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "Description: "):
			description = strings.TrimSpace(strings.TrimPrefix(line, "Description: "))
		case strings.HasPrefix(line, "Session: "):
			seenSession = strings.TrimSpace(strings.TrimPrefix(line, "Session: ")) != ""
		case strings.HasPrefix(line, "Role: "):
			role = strings.TrimSpace(strings.TrimPrefix(line, "Role: "))
			seenRole = role != ""
		case strings.HasPrefix(line, "Repository: "):
			seenRepository = strings.TrimSpace(strings.TrimPrefix(line, "Repository: ")) != ""
		}
	}
	if title == "" || !seenSession || !seenRole || !seenRepository {
		return value
	}
	if hasExtra {
		extra = strings.TrimSpace(extra)
	} else {
		extra = ""
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

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func boundedDisplay(value string, limit int) (string, bool) {
	scanLimit := limit * 4
	if scanLimit < limit {
		scanLimit = limit
	}
	raw, sourceClipped := truncateUTF8(value, scanLimit)
	result, clipped := truncateUTF8(cleanDisplay(raw), limit)
	return result, sourceClipped || clipped
}

func cleanDisplay(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == '\u007f' {
			return -1
		}
		return r
	}, value)
}

func cleanInline(value string) string {
	return strings.Join(strings.Fields(cleanDisplay(value)), " ")
}

func boundedInline(value string, limit int) string {
	scanLimit := limit * 4
	if scanLimit < limit {
		scanLimit = limit
	}
	value, _ = truncateUTF8(value, scanLimit)
	value, _ = truncateUTF8(cleanInline(value), limit)
	return value
}

func opaqueKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:12])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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

func previewLines(value string, lineLimit, byteLimit int) string {
	lines := strings.Split(value, "\n")
	if len(lines) > lineLimit {
		lines = lines[:lineLimit]
	}
	preview, clipped := truncateUTF8(strings.Join(lines, "\n"), byteLimit)
	if clipped {
		preview += "…"
	}
	return preview
}
