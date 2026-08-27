package replay

import (
	"context"
	"encoding/json"
	"path/filepath"
)

type grokEnvelope struct {
	Method string     `json:"method"`
	Params grokParams `json:"params"`
}

type grokParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
	Meta      struct {
		PromptID string `json:"promptId"`
	} `json:"_meta"`
}

type grokUpdate struct {
	Tag        string `json:"sessionUpdate"`
	PromptID   string `json:"prompt_id"`
	StopReason string `json:"stop_reason"`
	Meta       struct {
		PromptIndex *int `json:"promptIndex"`
		HostTurn    bool `json:"hostTurn"`
	} `json:"_meta"`

	TargetPromptIndex     *int   `json:"target_prompt_index"`
	CheckpointID          string `json:"checkpoint_id"`
	PromptIndexCompaction *int   `json:"prompt_index_at_compaction"`
	CheckpointFile        string `json:"checkpoint_file"`
}

type grokActive struct {
	index    int
	promptID string
	records  []json.RawMessage
}

func (service *Service) prepareGrok(
	ctx context.Context,
	profile Profile,
	reference nativeReference,
) ([]invocation, error) {
	rel := filepath.Join("sessions", reference.Scope, reference.SessionID, "updates.jsonl")
	data, err := service.readExactFile(ctx, profile, ProviderGrok, rel, appendOnlyFile)
	if err != nil {
		return nil, err
	}
	lines, err := service.jsonLines(data, true, profile)
	if err != nil {
		return nil, err
	}

	completed := make([]invocation, 0)
	var active *grokActive
	seenPromptIndexes := make(map[int]struct{})
	maxPromptIndex := -1
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, failure(CodeCancelled, profile)
		}
		var envelope grokEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if envelope.Method != "session/update" && envelope.Method != "_x.ai/session/update" {
			return nil, failure(CodeUnknownSchema, profile)
		}
		if envelope.Params.SessionID == "" || len(envelope.Params.Update) == 0 {
			return nil, failure(CodeUnknownSchema, profile)
		}
		if envelope.Params.SessionID != reference.SessionID {
			return nil, failure(CodeReferenceMismatch, profile)
		}
		var update grokUpdate
		if err := json.Unmarshal(envelope.Params.Update, &update); err != nil {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if update.Tag == "" {
			return nil, failure(CodeUnknownSchema, profile)
		}
		raw := json.RawMessage(line)

		switch update.Tag {
		case "user_message_chunk":
			if envelope.Method != "session/update" {
				return nil, failure(CodeUnknownSchema, profile)
			}
			if update.Meta.HostTurn {
				if active != nil {
					active.records = append(active.records, raw)
				}
				continue
			}
			if update.Meta.PromptIndex == nil || *update.Meta.PromptIndex < 0 || envelope.Params.Meta.PromptID == "" {
				return nil, failure(CodeUnknownSchema, profile)
			}
			index := *update.Meta.PromptIndex
			if active == nil {
				if _, duplicate := seenPromptIndexes[index]; duplicate {
					return nil, failure(CodeAmbiguousBoundary, profile)
				}
				seenPromptIndexes[index] = struct{}{}
				if index > maxPromptIndex {
					maxPromptIndex = index
				}
				active = &grokActive{
					index:    index,
					promptID: envelope.Params.Meta.PromptID,
					records:  []json.RawMessage{raw},
				}
				continue
			}
			if active.index != index || active.promptID != envelope.Params.Meta.PromptID {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active.records = append(active.records, raw)

		case "turn_completed":
			if envelope.Method != "_x.ai/session/update" || active == nil || update.PromptID == "" || update.StopReason == "" {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			if active.promptID != update.PromptID {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			active.records = append(active.records, raw)
			index := active.index
			completed = append(completed, invocation{
				boundary: NativeBoundary{InvocationID: update.PromptID, Status: update.StopReason},
				records:  active.records,
				index:    &index,
			})
			active = nil

		case "rewind_marker":
			if envelope.Method != "_x.ai/session/update" || active != nil ||
				update.TargetPromptIndex == nil || *update.TargetPromptIndex < 0 ||
				*update.TargetPromptIndex > maxPromptIndex {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			target := *update.TargetPromptIndex
			kept := make([]invocation, 0, len(completed))
			for index := range completed {
				item := completed[index]
				if item.index != nil && *item.index < target {
					kept = append(kept, item)
					completed[index] = invocation{}
				} else {
					clearInvocations(completed[index : index+1])
				}
			}
			completed = kept
			for index := range seenPromptIndexes {
				if index >= target {
					delete(seenPromptIndexes, index)
				}
			}
			maxPromptIndex = target - 1

		case "compaction_checkpoint":
			if envelope.Method != "_x.ai/session/update" || active != nil ||
				update.CheckpointID == "" || update.PromptIndexCompaction == nil ||
				*update.PromptIndexCompaction < 0 || update.CheckpointFile == "" {
				return nil, failure(CodeUnknownSchema, profile)
			}
			clearInvocations(completed)
			completed = nil

		default:
			if active != nil {
				active.records = append(active.records, raw)
				continue
			}
			if !knownGrokMaintenance(envelope.Method, update.Tag) {
				return nil, failure(CodeUnknownSchema, profile)
			}
		}
	}
	// A final active turn and any unterminated JSONL tail are live writes and
	// are deliberately excluded from the exact completed range.
	return completed, nil
}

func knownGrokMaintenance(method, tag string) bool {
	if method == "session/update" {
		switch tag {
		case "available_commands_update", "plan", "current_mode_update":
			return true
		}
		return false
	}
	switch tag {
	case "model_changed", "session_summary_generated", "last_turn_summary",
		"hooks_changed", "task_completed", "task_backgrounded":
		return true
	default:
		return false
	}
}
