// Package render converts Session JSONL into human-readable console output.
package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yty-build/acp-gateway/internal/acp"
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
	detail := normalizedDetail(line)
	if detail == "" {
		detail = line.Type
	}
	return fmt.Sprintf("%s%s %s", prefix, request, detail)
}

func normalizedDetail(line sessionlog.Line) string {
	if line.Type != "acp" {
		var payload map[string]any
		if json.Unmarshal(line.Data, &payload) == nil {
			if message, ok := payload["message"].(string); ok && message != "" {
				return line.Type + ": " + message
			}
			if state, ok := payload["state"].(string); ok && state != "" {
				return line.Type + " (" + state + ")"
			}
		}
		return strings.ReplaceAll(line.Type, "_", " ")
	}
	var event acp.Event
	if json.Unmarshal(line.Data, &event) != nil {
		return "ACP activity"
	}
	var payload map[string]any
	if json.Unmarshal(event.Raw, &payload) != nil {
		return "ACP " + event.Type
	}
	if params, ok := payload["params"].(map[string]any); ok {
		if update, ok := params["update"].(map[string]any); ok {
			if detail := sessionUpdateDetail(event.Type, update); detail != "" {
				return detail
			}
		}
		if strings.Contains(event.Type, "permission") {
			return permissionDetail(params)
		}
	}
	if rpcError, ok := payload["error"].(map[string]any); ok {
		if message, ok := rpcError["message"].(string); ok {
			return "error: " + message
		}
	}
	if result, ok := payload["result"].(map[string]any); ok {
		if reason, ok := result["stopReason"].(string); ok {
			return "turn ended: " + reason
		}
	}
	return "ACP " + strings.ReplaceAll(event.Type, "_", " ")
}

func sessionUpdateDetail(eventType string, update map[string]any) string {
	if content, ok := update["content"].(map[string]any); ok {
		if text, ok := content["text"].(string); ok && text != "" {
			switch eventType {
			case "agent_thought_chunk":
				return "reasoning: " + text
			case "user_message_chunk":
				return "user: " + text
			default:
				return text
			}
		}
	}
	if strings.HasPrefix(eventType, "tool_call") {
		return toolDetail(update)
	}
	if eventType == "plan" || eventType == "plan_update" {
		if entries, ok := update["entries"].([]any); ok {
			parts := make([]string, 0, len(entries))
			for _, item := range entries {
				entry, _ := item.(map[string]any)
				content, _ := entry["content"].(string)
				status, _ := entry["status"].(string)
				if content != "" {
					parts = append(parts, strings.TrimSpace(content)+valueSuffix(status))
				}
			}
			if len(parts) > 0 {
				return "plan: " + strings.Join(parts, "; ")
			}
		}
		return strings.ReplaceAll(eventType, "_", " ")
	}
	if eventType == "usage_update" {
		if used, ok := update["used"].(float64); ok {
			return fmt.Sprintf("usage: %.0f", used)
		}
	}
	return ""
}

func toolDetail(update map[string]any) string {
	title, _ := update["title"].(string)
	status, _ := update["status"].(string)
	kind, _ := update["kind"].(string)
	if title == "" {
		title, _ = update["toolCallId"].(string)
	}
	if title == "" {
		title = "tool call"
	}
	prefix := "tool"
	if kind != "" {
		prefix += " " + strings.ReplaceAll(kind, "_", " ")
	}
	if status != "" {
		prefix += " " + strings.ReplaceAll(status, "_", " ")
	}
	details := toolContent(update["content"])
	if details == "" {
		if output, exists := update["rawOutput"]; exists {
			details = compactJSON(output)
		}
	}
	result := prefix + ": " + title
	if details != "" {
		result += " — " + details
	}
	return result
}

func toolContent(value any) string {
	items, _ := value.([]any)
	var details []string
	for _, item := range items {
		entry, _ := item.(map[string]any)
		switch entry["type"] {
		case "content":
			content, _ := entry["content"].(map[string]any)
			if text, _ := content["text"].(string); text != "" {
				details = append(details, text)
			}
		case "diff":
			path, _ := entry["path"].(string)
			if path == "" {
				path = "file"
			}
			details = append(details, "diff "+path)
		case "terminal":
			terminalID, _ := entry["terminalId"].(string)
			details = append(details, "command output "+terminalID)
		}
	}
	return strings.Join(details, "; ")
}

func permissionDetail(params map[string]any) string {
	title := "tool call"
	if tool, ok := params["toolCall"].(map[string]any); ok {
		if value, ok := tool["title"].(string); ok && value != "" {
			title = value
		}
	}
	var choices []string
	if options, ok := params["options"].([]any); ok {
		for _, item := range options {
			option, _ := item.(map[string]any)
			if name, _ := option["name"].(string); name != "" {
				choices = append(choices, name)
			} else if kind, _ := option["kind"].(string); kind != "" {
				choices = append(choices, strings.ReplaceAll(kind, "_", " "))
			}
		}
	}
	if len(choices) > 0 {
		return "permission requested: " + title + " (" + strings.Join(choices, ", ") + ")"
	}
	return "permission requested: " + title
}

func compactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func valueSuffix(value string) string {
	if value == "" {
		return ""
	}
	return " (" + strings.ReplaceAll(value, "_", " ") + ")"
}
