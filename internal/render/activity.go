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
			if content, ok := update["content"].(map[string]any); ok {
				if text, ok := content["text"].(string); ok && text != "" {
					return text
				}
			}
			if title, ok := update["title"].(string); ok && title != "" {
				status, _ := update["status"].(string)
				if status != "" {
					return fmt.Sprintf("tool %s: %s", status, title)
				}
				return "tool: " + title
			}
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
