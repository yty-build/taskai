package console

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
	nativeLine := "Enter " + capability.label
	if !capability.available {
		nativeLine = "Native unavailable"
	}
	content := []string{
		fitLine(m.sessionAttention(current), width),
		fitWithRight(taskCheckoutLabel(current), taskContinuationReadiness(capability), width),
		fitLine(nativeLine+" · a Structured view", width),
	}
	return padLines(content, height, width)
}

func (m Model) renderTaskPreviewOverview(current session.Session, width int) []string {
	capability := nativeCapability(current)
	action := "Enter " + capability.label + " · a Structured view · p Message"
	if !capability.available {
		action = "Native unavailable · a Structured view · p Message"
	}
	return []string{
		fitLine(m.sessionAttention(current), width),
		fitWithRight(taskCheckoutLabel(current), taskContinuationReadiness(capability), width),
		fitLine(action, width),
	}
}

func (m Model) renderTaskTimeline(current session.Session, width, height int) []string {
	if height <= 0 {
		return nil
	}
	entries := taskPreviewEntries(m.projections[current.ID], taskProviderName(current))
	if len(entries) == 0 {
		empty := []string{"", centerLine("No conversation yet", width), centerLine("Messages appear here as work continues.", width)}
		return padLines(empty, height, width)
	}
	totalEntries := len(entries)
	maxEntries := max(1, height/2)
	entries = selectTaskPreviewEntries(entries, maxEntries)
	var body []string
	for _, entry := range entries {
		body = append(body, fitLine(taskPreviewEntryHeading(entry), width))
		if entry.kind != "status" {
			body = append(body, fitLine("  "+taskPreviewSnippet(entry.text), width))
		}
	}
	if len(body) < height && len(entries) < totalEntries {
		body = append(body, fitLine("  ↑ earlier conversation retained", width))
	}
	return padLines(body, height, width)
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
				appendEntry("user", "You", block.text, provenance)
			case blockThought:
				appendEntry("reasoning", "Reasoning", block.text, provenance)
			case blockAssistant:
				appendEntry("assistant", agent, block.text, provenance)
			case blockTool:
				if block.tool == nil {
					continue
				}
				appendEntry("tool", "Tool · "+toolDisplayTitle(block.tool), taskPreviewToolSummary(block.tool), provenance)
			case blockPlan:
				appendEntry("plan", "Plan", taskPreviewPlanSummary(block.plan), provenance)
			case blockPermission:
				if block.permission == nil {
					continue
				}
				state := valueOr(block.permission.state, "pending")
				appendEntry("approval", "Approval", valueOr(block.permission.title, "Tool permission")+" · "+state, provenance)
			case blockError:
				appendEntry("error", "Error", block.text, provenance)
			}
		}
		if text := taskPreviewStateText(request.state); text != "" {
			appendEntry("status", "Status", text, taskPreviewRequestProvenance(request))
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
	if entry.kind == "status" {
		return fmt.Sprintf("%s %s", glyph, entry.text)
	}
	return fmt.Sprintf("%s %s", glyph, entry.label)
}

func taskProviderName(current session.Session) string {
	label := nativeCapability(current).label
	if provider := strings.TrimSpace(strings.TrimSuffix(label, " CLI")); provider != "" && provider != "Native" {
		return provider
	}
	return providerDisplayName(strings.ToLower(strings.TrimSpace(current.Agent)))
}

func taskContinuationReadiness(capability nativeCLICapability) string {
	if !capability.available {
		return "Native unavailable"
	}
	if strings.HasPrefix(capability.status, "identity verified") {
		return "Exact conversation verified"
	}
	return "Conversation linked · verified on open"
}

func taskCheckoutLabel(current session.Session) string {
	path := strings.TrimSpace(current.RepoPath)
	if path == "" {
		path = strings.TrimSpace(current.WorktreePath)
	}
	name := filepath.Base(strings.TrimSuffix(path, string(filepath.Separator)))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = valueOr(path, "Checkout")
	}
	if strings.TrimSpace(current.WorktreePath) != "" {
		return safeText(name) + " · isolated worktree"
	}
	return safeText(name) + " · shared checkout"
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
