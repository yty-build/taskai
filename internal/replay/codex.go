package replay

import (
	"context"
	"encoding/json"
)

func (service *Service) prepareCodex(
	ctx context.Context,
	profile Profile,
	reference nativeReference,
) ([]invocation, error) {
	if service.dependencies.Codex == nil {
		return nil, failure(CodeSourceFailure, profile)
	}
	rawThread, err := service.dependencies.Codex.ThreadRead(ctx, reference.SessionID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, failure(CodeCancelled, profile)
		}
		return nil, failure(CodeSourceFailure, profile)
	}
	if err := ctx.Err(); err != nil {
		return nil, failure(CodeCancelled, profile)
	}
	if int64(len(rawThread)) > service.limits.MaxSourceBytes {
		return nil, failure(CodeBoundExceeded, profile)
	}

	var thread struct {
		ID    string            `json:"id"`
		Turns []json.RawMessage `json:"turns"`
	}
	if err := json.Unmarshal(rawThread, &thread); err != nil {
		return nil, failure(CodeCorruptHistory, profile)
	}
	if thread.ID == "" || thread.Turns == nil {
		return nil, failure(CodeUnknownSchema, profile)
	}
	if thread.ID != reference.SessionID {
		return nil, failure(CodeReferenceMismatch, profile)
	}
	if len(thread.Turns) > service.limits.MaxRecords {
		return nil, failure(CodeBoundExceeded, profile)
	}

	completed := make([]invocation, 0, len(thread.Turns))
	seen := make(map[string]struct{}, len(thread.Turns))
	totalRecords := 0
	incompleteSeen := false
	for _, rawTurn := range thread.Turns {
		if err := ctx.Err(); err != nil {
			return nil, failure(CodeCancelled, profile)
		}
		var turn struct {
			ID     string            `json:"id"`
			Status string            `json:"status"`
			Items  []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(rawTurn, &turn); err != nil {
			return nil, failure(CodeCorruptHistory, profile)
		}
		if turn.ID == "" || turn.Status == "" || turn.Items == nil {
			return nil, failure(CodeUnknownSchema, profile)
		}
		if _, duplicate := seen[turn.ID]; duplicate {
			return nil, failure(CodeAmbiguousBoundary, profile)
		}
		seen[turn.ID] = struct{}{}

		records := make([]json.RawMessage, 0, len(turn.Items))
		for _, rawItem := range turn.Items {
			if len(rawItem) > service.limits.MaxRecordBytes {
				return nil, failure(CodeBoundExceeded, profile)
			}
			var item struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(rawItem, &item); err != nil {
				return nil, failure(CodeCorruptHistory, profile)
			}
			if item.Type == "" {
				return nil, failure(CodeUnknownSchema, profile)
			}
			totalRecords++
			if totalRecords > service.limits.MaxRecords {
				return nil, failure(CodeBoundExceeded, profile)
			}
			records = append(records, rawItem)
		}

		switch turn.Status {
		case "completed", "failed", "interrupted":
			if incompleteSeen {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			completed = append(completed, invocation{
				boundary: NativeBoundary{InvocationID: turn.ID, Status: turn.Status},
				records:  records,
			})
		case "inProgress":
			if incompleteSeen {
				return nil, failure(CodeAmbiguousBoundary, profile)
			}
			incompleteSeen = true
		default:
			return nil, failure(CodeUnknownSchema, profile)
		}
	}
	return completed, nil
}
