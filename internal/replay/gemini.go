package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
)

type geminiActive struct {
	id           string
	records      []json.RawMessage
	pendingTools bool
}

func (service *Service) prepareGemini(
	ctx context.Context,
	profile Profile,
	reference nativeReference,
) ([]invocation, error) {
	rel := filepath.Join("tmp", reference.Scope, "chats", reference.RecordKey)
	data, err := service.readExactFile(ctx, profile, ProviderGemini, rel, stableFile)
	if err != nil {
		return nil, err
	}
	var conversation struct {
		SessionID   string            `json:"sessionId"`
		ProjectHash string            `json:"projectHash"`
		StartTime   string            `json:"startTime"`
		LastUpdated string            `json:"lastUpdated"`
		Messages    []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &conversation); err != nil {
		return nil, failure(CodeCorruptHistory, profile)
	}
	if conversation.SessionID == "" || conversation.ProjectHash == "" ||
		conversation.StartTime == "" || conversation.LastUpdated == "" || conversation.Messages == nil {
		return nil, failure(CodeUnknownSchema, profile)
	}
	if conversation.SessionID != reference.SessionID || conversation.ProjectHash != reference.Scope {
		return nil, failure(CodeReferenceMismatch, profile)
	}
	if len(conversation.Messages) > service.limits.MaxRecords {
		return nil, failure(CodeBoundExceeded, profile)
	}

	completed := make([]invocation, 0)
	seenIDs := make(map[string]struct{}, len(conversation.Messages))
	var active *geminiActive
	for _, raw := range conversation.Messages {
		if err := ctx.Err(); err != nil {
			return nil, failure(CodeCancelled, profile)
		}
		if len(raw) > service.limits.MaxRecordBytes {
			return nil, failure(CodeBoundExceeded, profile)
		}
		var message struct {
			ID        string            `json:"id"`
			Type      string            `json:"type"`
			Timestamp string            `json:"timestamp"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []json.RawMessage `json:"toolCalls"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if message.ID == "" || message.Type == "" || message.Timestamp == "" || !geminiContentValid(message.Content) {
			return nil, failure(CodeUnknownSchema, profile)
		}
		if _, duplicate := seenIDs[message.ID]; duplicate {
			return nil, failure(CodeAmbiguousBoundary, profile)
		}
		seenIDs[message.ID] = struct{}{}

		switch message.Type {
		case "user":
			if active != nil {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active = &geminiActive{id: message.ID, records: []json.RawMessage{raw}}

		case "gemini":
			if active == nil || active.pendingTools {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active.records = append(active.records, raw)
			if len(message.ToolCalls) == 0 {
				completed = append(completed, invocation{
					boundary: NativeBoundary{InvocationID: active.id, Status: "gemini"},
					records:  active.records,
				})
				active = nil
				continue
			}
			pending, err := service.validateGeminiToolCalls(message.ToolCalls, profile)
			if err != nil {
				return nil, err
			}
			active.pendingTools = pending

		case "info", "warning":
			if active != nil {
				active.records = append(active.records, raw)
			}

		case "error":
			if active == nil {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active.records = append(active.records, raw)
			completed = append(completed, invocation{
				boundary: NativeBoundary{InvocationID: active.id, Status: "error"},
				records:  active.records,
			})
			active = nil

		default:
			// A provider-owned, ID-bearing message inside an unambiguous turn is
			// passed through byte-for-byte. Unknown control outside a turn fails.
			if active == nil {
				return nil, failure(CodeUnknownSchema, profile)
			}
			active.records = append(active.records, raw)
		}
	}
	// An active final turn is a valid incomplete snapshot and is excluded.
	return completed, nil
}

func (service *Service) validateGeminiToolCalls(calls []json.RawMessage, profile Profile) (bool, error) {
	seen := make(map[string]struct{}, len(calls))
	pending := false
	for _, raw := range calls {
		if len(raw) > service.limits.MaxRecordBytes {
			return false, failure(CodeBoundExceeded, profile)
		}
		var call struct {
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Status    string          `json:"status"`
			Timestamp string          `json:"timestamp"`
			Args      json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(raw, &call); err != nil {
			return false, failure(CodeCorruptHistory, profile)
		}
		if call.ID == "" || call.Name == "" || call.Status == "" || call.Timestamp == "" || len(call.Args) == 0 {
			return false, failure(CodeUnknownSchema, profile)
		}
		if _, duplicate := seen[call.ID]; duplicate {
			return false, failure(CodeAmbiguousBoundary, profile)
		}
		seen[call.ID] = struct{}{}
		switch call.Status {
		case "success", "error", "cancelled":
		case "validating", "scheduled", "executing", "awaiting_approval":
			pending = true
		default:
			return false, failure(CodeUnknownSchema, profile)
		}
	}
	return pending, nil
}

func geminiContentValid(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return true
	}
	var parts []json.RawMessage
	if json.Unmarshal(trimmed, &parts) == nil {
		return true
	}
	var part map[string]json.RawMessage
	return json.Unmarshal(trimmed, &part) == nil && part != nil
}
