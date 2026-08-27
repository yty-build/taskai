package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
)

type claudeEntry struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	SessionID         string          `json:"sessionId"`
	Version           string          `json:"version"`
	UUID              string          `json:"uuid"`
	ParentUUID        string          `json:"parentUuid"`
	IsSidechain       bool            `json:"isSidechain"`
	IsMeta            bool            `json:"isMeta"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Status            string          `json:"status"`
	StopReason        string          `json:"stopReason"`
	Message           json.RawMessage `json:"message"`
}

type claudeActive struct {
	id             string
	tip            string
	records        []json.RawMessage
	lastStopReason string
	apiError       bool
}

func (service *Service) prepareClaude(
	ctx context.Context,
	profile Profile,
	reference nativeReference,
) ([]invocation, error) {
	rel := filepath.Join("projects", reference.Scope, reference.SessionID+".jsonl")
	data, err := service.readExactFile(ctx, profile, ProviderClaude, rel, stableFile)
	if err != nil {
		return nil, err
	}
	lines, err := service.jsonLines(data, false, profile)
	if err != nil {
		return nil, err
	}

	completed := make([]invocation, 0)
	knownUUIDs := make(map[string]bool)
	identitySeen := false
	versionSeen := false
	mainTip := ""
	var active *claudeActive

	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, failure(CodeCancelled, profile)
		}
		var entry claudeEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if entry.Type == "" {
			return nil, failure(CodeUnknownSchema, profile)
		}
		if entry.SessionID != "" {
			identitySeen = true
			if entry.SessionID != reference.SessionID {
				return nil, failure(CodeReferenceMismatch, profile)
			}
		}
		if entry.Version != "" {
			versionSeen = true
			if entry.Version != profile.InstalledVersion {
				return nil, failure(CodeUnsupportedVersion, profile)
			}
		}
		if entry.ParentUUID != "" {
			parentSidechain, exists := knownUUIDs[entry.ParentUUID]
			if !exists || (!entry.IsSidechain && parentSidechain) {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
		}
		if entry.UUID != "" {
			if _, duplicate := knownUUIDs[entry.UUID]; duplicate {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			knownUUIDs[entry.UUID] = entry.IsSidechain
		}
		if entry.IsSidechain {
			continue
		}
		// The native main conversation is one exact UUID chain. Merely finding a
		// parent somewhere in the file is insufficient: an older branch must not
		// contaminate or terminate the currently active invocation.
		if entry.UUID != "" {
			if entry.ParentUUID != mainTip {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
		} else if entry.ParentUUID != "" {
			return nil, failure(CodeAmbiguousBoundary, profile)
		}

		raw := json.RawMessage(line)
		switch entry.Type {
		case "user":
			toolResult, schemaOK := claudeUserIsToolResult(entry.Message)
			if !schemaOK {
				return nil, failure(CodeUnknownSchema, profile)
			}
			if toolResult {
				if active == nil || entry.IsMeta || entry.UUID == "" || entry.ParentUUID != active.tip {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
				active.records = append(active.records, raw)
				mainTip = entry.UUID
				active.tip = entry.UUID
				continue
			}
			if entry.IsMeta {
				if active != nil {
					if entry.UUID == "" || entry.ParentUUID != active.tip {
						return nil, failure(CodeAmbiguousBoundary, profile)
					}
					active.records = append(active.records, raw)
					active.tip = entry.UUID
				}
				if entry.UUID != "" {
					mainTip = entry.UUID
				}
				continue
			}
			if active != nil || entry.UUID == "" {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active = &claudeActive{id: entry.UUID, tip: entry.UUID, records: []json.RawMessage{raw}}

		case "assistant":
			stopReason, schemaOK := claudeAssistantStopReason(entry.Message)
			if !schemaOK {
				return nil, failure(CodeUnknownSchema, profile)
			}
			if active == nil || entry.UUID == "" || entry.ParentUUID != active.tip {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active.records = append(active.records, raw)
			if stopReason != "" {
				active.lastStopReason = stopReason
			}
			active.apiError = active.apiError || entry.IsAPIErrorMessage

		case "system":
			if entry.Subtype == "" {
				return nil, failure(CodeUnknownSchema, profile)
			}
			switch entry.Subtype {
			case "compact_boundary":
				if active != nil {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
				clearInvocations(completed)
				completed = nil
			case "turn_duration":
				if active == nil || entry.UUID == "" || entry.ParentUUID != active.tip {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
				active.records = append(active.records, raw)
				status := entry.Status
				if status == "" {
					status = entry.StopReason
				}
				if status == "" {
					status = active.lastStopReason
				}
				if status == "" && (active.apiError || entry.IsAPIErrorMessage) {
					status = "api_error"
				}
				if status == "" {
					status = "turn_duration"
				}
				completed = append(completed, invocation{
					boundary: NativeBoundary{InvocationID: active.id, Status: status},
					records:  active.records,
				})
				active = nil
			default:
				if active != nil && entry.UUID != "" && entry.ParentUUID == active.tip {
					active.records = append(active.records, raw)
				} else if !knownClaudeSystemSubtype(entry.Subtype) {
					return nil, failure(CodeUnknownSchema, profile)
				} else if active != nil {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
			}

		case "file-history-snapshot", "queue-operation", "summary":
			// Outside a turn these are connector maintenance. When UUID-owned by
			// the active chain they are native invocation records and pass through.
			if active != nil {
				if entry.UUID == "" || entry.ParentUUID != active.tip {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
				active.records = append(active.records, raw)
			}

		default:
			// Pinned Claude may add provider-owned progress/tool records. They are
			// replayable without projection only while UUID-owned by an active turn.
			if active == nil || entry.UUID == "" || entry.ParentUUID != active.tip {
				return nil, failure(CodeUnknownSchema, profile)
			}
			active.records = append(active.records, raw)
		}
		if entry.UUID != "" {
			mainTip = entry.UUID
			if active != nil {
				active.tip = entry.UUID
			}
		}
	}
	if !identitySeen {
		return nil, failure(CodeReferenceMismatch, profile)
	}
	if !versionSeen {
		return nil, failure(CodeUnknownSchema, profile)
	}
	// A final active invocation is a valid partial write and is deliberately
	// excluded. Stable Claude JSONL still requires every persisted line to end
	// in a newline and parse completely.
	return completed, nil
}

func claudeUserIsToolResult(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, false
	}
	var message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil || message.Role != "user" || len(message.Content) == 0 {
		return false, false
	}
	var text string
	if err := json.Unmarshal(message.Content, &text); err == nil {
		return false, true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return false, false
	}
	if len(blocks) == 0 {
		return false, true
	}
	for _, block := range blocks {
		if block.Type == "" {
			return false, false
		}
		if block.Type != "tool_result" {
			return false, true
		}
	}
	return true, true
}

func claudeAssistantStopReason(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var message struct {
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &message); err != nil || message.Role != "assistant" {
		return "", false
	}
	return message.StopReason, true
}

func knownClaudeSystemSubtype(subtype string) bool {
	switch subtype {
	case "init", "local_command", "api_error", "api_error_message",
		"hook_started", "hook_progress", "hook_response", "stop_hook_summary":
		return true
	default:
		return false
	}
}
