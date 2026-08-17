package console

import (
	"strings"

	appservice "github.com/yty-build/acp-gateway/internal/service"
)

// transcriptFromConversation reduces the shared provider-store + ACP snapshot
// into terminal viewer state. It is built off-screen and swapped as one model
// update, so reconciliation never renders an empty intermediate frame.
func transcriptFromConversation(snapshot appservice.ConversationSnapshot) *transcript {
	result := newTranscript()
	result.metadata.lastSequence = snapshot.GatewayCursor
	for _, turn := range snapshot.Turns {
		request := result.request(turn.Key)
		if request == nil {
			continue
		}
		request.committed = turn.Committed
		for _, activity := range turn.Activities {
			applyConversationActivity(request, activity)
		}
		if turn.Committed && request.state == "" {
			request.state = turnComplete
		}
	}
	return result
}

func applyConversationActivity(request *transcriptRequest, activity appservice.ConversationActivity) {
	if request == nil {
		return
	}
	if permission := activity.Permission; permission != nil {
		if permission.Pending != nil {
			request.upsertWorkerPermission(*permission.Pending)
		} else if permission.ID != "" {
			request.resolvePermission(permission.ID, permission.Decision)
		}
	}
	display := activity.Display
	if display == nil {
		return
	}
	switch display.Type {
	case "user_message", "legacy_user_message":
		request.setUserMessage(display.Text, display.Type == "legacy_user_message")
		markLatestConversationBlockSource(request, blockUser, activity.Kind)
	case "assistant_chunk":
		request.appendMessage(blockAssistant, display.Text, display.MessageID, maxProjectedMessageBytes)
		markLatestConversationBlockSource(request, blockAssistant, activity.Kind)
	case "thought_chunk":
		request.appendThought(display.Text)
		markLatestConversationBlockSource(request, blockThought, activity.Kind)
	case "tool_upsert":
		if display.Tool == nil || display.Tool.ID == "" {
			return
		}
		tool := request.tools[display.Tool.ID]
		if tool == nil {
			tool = &toolCard{id: display.Tool.ID, seenPayloads: make(map[[32]byte]struct{})}
			request.tools[tool.id] = tool
			request.blocks = append(request.blocks, &transcriptBlock{kind: blockTool, id: tool.id, tool: tool})
		}
		tool.title = mergeField(tool.title, display.Tool.Title)
		tool.kind = mergeField(tool.kind, display.Tool.Kind)
		tool.status = mergeField(tool.status, display.Tool.Status)
		tool.metadata = mergeField(tool.metadata, display.Tool.Metadata)
		if display.Tool.Detail != "" {
			tool.detail, tool.detailClipped = truncateUTF8(sanitizeDisplay(display.Tool.Detail), maxToolDetailBytes)
			tool.detailClipped = tool.detailClipped || display.Tool.DetailClipped
			tool.detailLabel = display.Tool.DetailLabel
			tool.outputBytes = len([]byte(tool.detail))
			tool.outputLines = logicalLines(tool.detail)
		}
		tool.preview = display.Tool.Preview
		tool.sensitive = display.Tool.Sensitive
		markConversationToolSource(request, tool, activity.Kind)
	case "plan_replace":
		if request.plan == nil {
			request.plan = &transcriptBlock{kind: blockPlan}
			request.blocks = append(request.blocks, request.plan)
		}
		request.plan.plan = request.plan.plan[:0]
		for _, entry := range display.Plan {
			request.plan.plan = append(request.plan.plan, planEntry{content: entry.Content, status: entry.Status})
		}
		markLatestConversationBlockSource(request, blockPlan, activity.Kind)
	case "error_upsert":
		request.state = turnFailed
		request.errorText = display.Text
		request.blocks = append(request.blocks, &transcriptBlock{kind: blockError, text: display.Text})
		markLatestConversationBlockSource(request, blockError, activity.Kind)
	case "turn_state":
		request.stop = display.StopReason
		switch display.State {
		case "ready":
			request.state = turnReady
		case "queued":
			request.state = turnQueued
		case "working":
			request.state = turnLive
		case "cancelling":
			request.state = turnCancelling
		case "cancelled":
			request.state = turnCancelled
		case "complete":
			request.state = turnComplete
		case "incomplete":
			request.state = turnIncomplete
		case "error":
			request.state = turnFailed
		}
	}
	if display.Truncated {
		request.contentClipped = true
		if len(request.blocks) > 0 {
			request.blocks[len(request.blocks)-1].truncated = true
		}
	}
	if strings.TrimSpace(display.Text) == "" {
		return
	}
}

func markConversationToolSource(request *transcriptRequest, tool *toolCard, activityKind string) {
	if request == nil || tool == nil || activityKind != "provider_transcript" {
		return
	}
	for index := len(request.blocks) - 1; index >= 0; index-- {
		if request.blocks[index].tool == tool {
			request.blocks[index].source = "provider"
			return
		}
	}
}

func markLatestConversationBlockSource(request *transcriptRequest, kind blockKind, activityKind string) {
	if activityKind != "provider_transcript" {
		return
	}
	for index := len(request.blocks) - 1; index >= 0; index-- {
		if request.blocks[index].kind == kind {
			request.blocks[index].source = "provider"
			return
		}
	}
}
