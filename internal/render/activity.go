// Package render converts Session Channel frames into compact terminal output.
// It never reconstructs a provider transcript or assigns semantics that the
// provider did not emit.
package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func Activity(line sessionlog.Line, raw bool) string {
	if raw {
		encoded, err := json.Marshal(line)
		if err != nil {
			return fmt.Sprintf(`{"seq":%d,"type":"render_error"}`, line.Seq)
		}
		return string(encoded)
	}
	prefix := fmt.Sprintf("%6d", line.Seq)
	if !line.Timestamp.IsZero() {
		prefix = line.Timestamp.Local().Format("15:04:05")
	}
	request := ""
	if line.RequestID != "" {
		request = " [" + line.RequestID + "]"
	}
	detail := frameDetail(line)
	if detail == "" {
		detail = strings.ReplaceAll(line.Type, "_", " ")
	}
	return terminalSafeLine(fmt.Sprintf("%s%s %s", prefix, request, detail))
}

// terminalSafeLine is the single non-raw log rendering boundary. A journal
// value may describe terminal output, but it never gains control of the
// viewer's terminal or the surrounding one-frame-per-line layout.
func terminalSafeLine(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, value)
}

func frameDetail(line sessionlog.Line) string {
	var payload map[string]any
	if json.Unmarshal(line.Data, &payload) != nil {
		return ""
	}
	switch line.Type {
	case "connection":
		return "connection: " + stringValue(payload, "state")
	case "client_command":
		command := stringValue(payload, "command")
		if command == "prompt" {
			return "user: " + stringValue(payload, "text")
		}
		return "client: " + command
	case "provider_event":
		return providerEventDetail(payload)
	case "provider_result":
		status := stringValue(payload, "status")
		reason := stringValue(payload, "stop_reason")
		if reason != "" {
			return "provider result: " + status + " (" + reason + ")"
		}
		if message := stringValue(payload, "message"); message != "" {
			return "provider result: " + status + " — " + message
		}
		return "provider result: " + status
	case "permission_requested":
		return permissionDetail(payload)
	case "permission_resolved":
		return "permission resolved: " + firstString(payload, "decision", "option_id", "permission_id")
	case "permission_abandoned":
		return "permission abandoned: " + firstString(payload, "reason", "permission_id")
	case "worker_error":
		return "worker error: " + firstString(payload, "message", "code")
	default:
		return firstString(payload, "message", "state", "status")
	}
}

func providerEventDetail(payload map[string]any) string {
	typeName := stringValue(payload, "type")
	switch typeName {
	case "text_delta":
		text := stringValue(payload, "text")
		if stream := stringValue(payload, "stream"); stream != "" && stream != "output" {
			return stream + ": " + text
		}
		return text
	case "tool_call":
		label := firstString(payload, "title", "name", "toolCallId", "tool_call_id")
		status := stringValue(payload, "status")
		if status != "" {
			return "tool " + strings.ReplaceAll(status, "_", " ") + ": " + label
		}
		return "tool: " + label
	case "status":
		return "provider status: " + firstString(payload, "message", "status", "state")
	case "error":
		return "provider error: " + firstString(payload, "message", "code")
	default:
		if typeName == "" {
			return "provider event"
		}
		return "provider: " + strings.ReplaceAll(typeName, "_", " ")
	}
}

func permissionDetail(payload map[string]any) string {
	title := firstString(payload, "title", "kind", "id")
	choices := make([]string, 0)
	if options, ok := payload["options"].([]any); ok {
		for _, item := range options {
			option, _ := item.(map[string]any)
			if label := firstString(option, "name", "kind", "id"); label != "" {
				choices = append(choices, label)
			}
		}
	}
	if len(choices) == 0 {
		return "permission requested: " + title
	}
	return "permission requested: " + title + " (" + strings.Join(choices, ", ") + ")"
}

func stringValue(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(payload, key); value != "" {
			return value
		}
	}
	return ""
}
