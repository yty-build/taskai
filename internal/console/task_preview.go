package console

import (
	"fmt"
	"sort"
	"strings"

	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
)

const (
	taskPreviewProvider = "provider"
	taskPreviewLive     = "live"

	taskPreviewRequestLimit = 32
	taskPreviewBlockLimit   = 64
	taskPreviewEntryLimit   = 192
	taskPreviewTextLimit    = 2048
)

type taskPreviewEntry struct {
	kind       string
	label      string
	text       string
	provenance string
}

func (m Model) renderCompactTaskPreview(current session.Session, width, height int) []string {
	capability := nativeCapability(current)
	nativeLine := "↵ Enter  " + capability.label + " · " + capability.status
	if capability.reason != "" {
		nativeLine += ": " + capability.reason
	}
	content := []string{
		strings.ToUpper(valueOr(current.Agent, "agent")) + " · " + valueOr(current.Role, "worker") + " · " + humanizeLabel(appservice.PermissionLabel(current)),
		fitLine(nativeLine, width),
		fitLine("a        Structured ACP · transcript, tools, and approvals", width),
	}
	return padLines(content, height, width)
}

func (m Model) renderTaskPreviewOverview(current session.Session, width int) []string {
	capability := nativeCapability(current)
	identity := strings.ToUpper(valueOr(current.Agent, "agent")) + " · " + valueOr(current.Role, "worker")
	action := "↵ " + capability.label + "   a Structured ACP"
	if !capability.available {
		action = "↵ Native unavailable   a Structured ACP"
	}
	return []string{
		fitWithRight(identity, statusBadge(string(current.Status)), width),
		fitLine(action, width),
		fitLine("◆ PROVIDER COMMITTED   ● LIVE ACP", width),
		fitLine(m.sessionAttention(current), width),
	}
}

func (m Model) renderTaskContextRail(current session.Session, width, height int) []string {
	capability := nativeCapability(current)
	provider := strings.ToUpper(valueOr(current.Agent, "agent"))
	nativeStatus := capability.status
	if capability.reason != "" {
		nativeStatus += " · " + capability.reason
	}
	lines := []string{
		sectionHeading("CONTEXT", statusBadge(string(current.Status)), width),
		sectionRule(width),
		fitLine(provider+" · "+valueOr(current.Role, "worker"), width),
	}
	attention := indentWrapped(m.sessionAttention(current), max(10, width-2), "  ")
	lines = append(lines, takeLines(attention, 3)...)
	lines = append(lines,
		"",
		sectionHeading("CONTINUE", "", width),
		fitLine("↵ "+capability.label, width),
		fitLine("  "+nativeStatus, width),
		fitLine("a Structured ACP", width),
		fitLine("  transcript · tools · approvals", width),
		fitLine("p Message via ACP", width),
		"",
		sectionHeading("PROVENANCE", "", width),
		fitLine("◆ Provider committed", width),
		fitLine("● Live ACP stream", width),
		fitLine("Same provider conversation", width),
		"",
		sectionHeading("SESSION", "", width),
		fitLine(current.ID, width),
		fitLine(compactSessionPath(current), width),
		fitLine(humanizeLabel(appservice.PermissionLabel(current)), width),
	)
	return padLines(lines, height, width)
}

func (m Model) renderTaskTimeline(current session.Session, width, height int) []string {
	if height <= 0 {
		return nil
	}
	lines := []string{sectionHeading("RECENT CONVERSATION", "committed + live", width), sectionRule(width)}
	bodyHeight := max(0, height-len(lines))
	if bodyHeight == 0 {
		return padLines(lines, height, width)
	}
	entries := taskPreviewEntries(m.projections[current.ID], strings.ToUpper(valueOr(current.Agent, "agent")))
	if len(entries) == 0 {
		empty := []string{"", centerLine("No conversation yet", width), centerLine("Native and ACP continuations appear here.", width)}
		return append(lines, padLines(empty, bodyHeight, width)...)
	}
	totalEntries := len(entries)
	maxEntries := max(1, bodyHeight/2)
	entries = selectTaskPreviewEntries(entries, maxEntries)
	var body []string
	for _, entry := range entries {
		body = append(body, fitLine(taskPreviewEntryHeading(entry), width))
		body = append(body, fitLine("  "+taskPreviewSnippet(entry.text), width))
	}
	if len(body) < bodyHeight && len(entries) < totalEntries {
		body = append(body, fitLine("  ↑ earlier conversation retained", width))
	}
	return append(lines, padLines(body, bodyHeight, width)...)
}

func taskPreviewEntries(projection *transcript, agent string) []taskPreviewEntry {
	if projection == nil {
		return nil
	}
	var entries []taskPreviewEntry
	appendEntry := func(kind, label, text, provenance string) {
		text, _ = truncateUTF8(text, taskPreviewTextLimit)
		text = strings.TrimSpace(safeText(text))
		if text == "" {
			return
		}
		entries = append(entries, taskPreviewEntry{kind: kind, label: label, text: text, provenance: provenance})
	}
	requests := projection.requests
	if len(requests) > taskPreviewRequestLimit {
		requests = requests[len(requests)-taskPreviewRequestLimit:]
	}
	for _, request := range requests {
		blocks := request.blocks
		if len(blocks) > taskPreviewBlockLimit {
			blocks = blocks[len(blocks)-taskPreviewBlockLimit:]
			// User text is normally the first block. Preserve that one anchor even
			// when a tool-heavy turn exceeds the bounded recent-block window.
			if request.blocks[0].kind == blockUser {
				blocks = append([]*transcriptBlock{request.blocks[0]}, blocks...)
			}
		}
		for _, block := range blocks {
			provenance := taskPreviewProvenance(block.source)
			switch block.kind {
			case blockUser:
				if request.committed {
					provenance = taskPreviewProvider
				}
				appendEntry("user", "YOU", block.text, provenance)
			case blockThought:
				appendEntry("reasoning", "REASONING", block.text, provenance)
			case blockAssistant:
				appendEntry("assistant", agent, block.text, provenance)
			case blockTool:
				if block.tool == nil {
					continue
				}
				appendEntry("tool", "TOOL · "+toolDisplayTitle(block.tool), taskPreviewToolSummary(block.tool), provenance)
			case blockPlan:
				appendEntry("plan", "PLAN", taskPreviewPlanSummary(block.plan), provenance)
			case blockPermission:
				if block.permission == nil {
					continue
				}
				state := valueOr(block.permission.state, "pending")
				appendEntry("approval", "APPROVAL", valueOr(block.permission.title, "Tool permission")+" · "+state, provenance)
			case blockError:
				appendEntry("error", "ERROR", block.text, provenance)
			}
		}
		if text := taskPreviewStateText(request.state); text != "" {
			appendEntry("status", "STATUS", text, taskPreviewRequestProvenance(request))
		}
		if len(entries) > taskPreviewEntryLimit {
			entries = append([]taskPreviewEntry(nil), entries[len(entries)-taskPreviewEntryLimit:]...)
		}
	}
	return entries
}

func selectTaskPreviewEntries(entries []taskPreviewEntry, limit int) []taskPreviewEntry {
	if limit <= 0 || len(entries) == 0 {
		return nil
	}
	if len(entries) <= limit {
		return append([]taskPreviewEntry(nil), entries...)
	}
	selected := make(map[int]struct{}, limit)
	for _, kind := range []string{"user", "reasoning", "tool", "assistant", "status"} {
		if index := latestTaskPreviewEntry(entries, func(entry taskPreviewEntry) bool { return entry.kind == kind }); index >= 0 {
			selected[index] = struct{}{}
		}
	}
	for _, provenance := range []string{taskPreviewProvider, taskPreviewLive} {
		if len(selected) >= limit {
			break
		}
		if index := latestTaskPreviewEntry(entries, func(entry taskPreviewEntry) bool { return entry.provenance == provenance }); index >= 0 {
			selected[index] = struct{}{}
		}
	}
	for index := len(entries) - 1; index >= 0 && len(selected) < limit; index-- {
		selected[index] = struct{}{}
	}
	indices := make([]int, 0, len(selected))
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	if len(indices) > limit {
		indices = indices[len(indices)-limit:]
	}
	result := make([]taskPreviewEntry, 0, len(indices))
	for _, index := range indices {
		result = append(result, entries[index])
	}
	return result
}

func latestTaskPreviewEntry(entries []taskPreviewEntry, matches func(taskPreviewEntry) bool) int {
	for index := len(entries) - 1; index >= 0; index-- {
		if matches(entries[index]) {
			return index
		}
	}
	return -1
}

func taskPreviewEntryHeading(entry taskPreviewEntry) string {
	glyph := "●"
	provenance := "LIVE ACP"
	if entry.provenance == taskPreviewProvider {
		glyph = "◆"
		provenance = "PROVIDER COMMITTED"
	}
	switch entry.kind {
	case "user":
		glyph = "❯"
	case "reasoning":
		glyph = "◇"
	case "tool":
		glyph = "▸"
	case "status":
		glyph = taskPreviewStateGlyph(entry.text)
	case "error":
		glyph = "!"
	}
	return fmt.Sprintf("%s %s · %s", glyph, entry.label, provenance)
}

func taskPreviewStateGlyph(text string) string {
	switch text {
	case "Turn complete":
		return "✓"
	case "Turn failed":
		return "!"
	case "Turn cancelled":
		return "×"
	case "Message queued", "Cancellation requested":
		return "○"
	default:
		return "●"
	}
}

func taskPreviewSnippet(value string) string {
	return strings.Join(strings.Fields(safeText(value)), " ")
}

func taskPreviewProvenance(source string) string {
	if source == "provider" {
		return taskPreviewProvider
	}
	return taskPreviewLive
}

func taskPreviewRequestProvenance(request *transcriptRequest) string {
	if len(request.blocks) > 0 && request.blocks[0].source == "provider" {
		return taskPreviewProvider
	}
	blocks := request.blocks
	if len(blocks) > taskPreviewBlockLimit {
		blocks = blocks[len(blocks)-taskPreviewBlockLimit:]
	}
	for _, block := range blocks {
		if block.source == "provider" {
			return taskPreviewProvider
		}
	}
	return taskPreviewLive
}

func taskPreviewToolSummary(tool *toolCard) string {
	if tool == nil {
		return ""
	}
	if summary := firstMeaningfulLine(tool.preview); summary != "" {
		return summary
	}
	var parts []string
	if tool.status != "" {
		parts = append(parts, humanizeLabel(tool.status))
	}
	if tool.kind != "" {
		parts = append(parts, humanizeLabel(tool.kind))
	}
	if tool.outputLines > 0 {
		parts = append(parts, fmt.Sprintf("%d lines", tool.outputLines))
	} else if tool.outputBytes > 0 {
		parts = append(parts, humanBytes(tool.outputBytes))
	}
	return strings.Join(parts, " · ")
}

func taskPreviewPlanSummary(entries []planEntry) string {
	if len(entries) == 0 {
		return ""
	}
	latest := entries[len(entries)-1]
	return valueOr(latest.content, "Plan updated") + statusSuffix(latest.status)
}

func taskPreviewStateText(state turnState) string {
	switch state {
	case turnQueued:
		return "Message queued"
	case turnLive:
		return "Agent is working"
	case turnCancelling:
		return "Cancellation requested"
	case turnCancelled:
		return "Turn cancelled"
	case turnFailed:
		return "Turn failed"
	case turnComplete:
		return "Turn complete"
	default:
		return ""
	}
}

func takeLines(lines []string, limit int) []string {
	if len(lines) <= limit {
		return lines
	}
	return lines[:limit]
}
