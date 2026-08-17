package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/yty-build/acp-gateway/internal/native"
	"github.com/yty-build/acp-gateway/internal/presentation"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/worker"
)

const (
	conversationRetainedTurns          = 256
	conversationMatchTurns             = 2048
	conversationActiveTurns            = 256
	conversationDiagnostics            = 1000
	conversationActivities             = 2048
	conversationActivityBudget         = 1 << 20
	conversationRetainedActivityBudget = 16 << 20
	conversationDiagnosticBudget       = 4 << 20
	conversationMatchWindow            = 30 * time.Second
)

// ConversationSource describes which durable source supplied the base
// conversation. It is disposable presentation state, not another domain
// entity.
type ConversationSource string

const (
	ConversationSourceProvider ConversationSource = "provider-store"
	ConversationSourceGateway  ConversationSource = "gateway-log"
)

// ConversationActivity is the bounded, browser-safe projection of one gateway
// log record retained around an uncommitted ACP turn. Raw ACP envelopes and
// provider payloads never cross this service boundary.
type ConversationActivity struct {
	Position   uint64
	RequestID  string
	Kind       string
	Timestamp  time.Time
	Summary    string
	Display    *presentation.Activity
	Permission *ConversationPermission
}

// ConversationPermission carries only the fields required to rebuild the
// approval shelf while a provider transcript is used as the conversation base.
type ConversationPermission struct {
	Pending  *worker.PendingPermission
	ID       string
	State    string
	Decision string
}

// ConversationSnapshot combines one verified provider-owned transcript with
// gateway-only provisional/lifecycle state captured at GatewayCursor. Clients
// can prepare it off-screen and publish it atomically.
type ConversationSnapshot struct {
	Source        ConversationSource
	Provider      native.TranscriptSnapshot
	Turns         []ConversationTurn
	Diagnostics   []ConversationActivity
	GatewayCursor uint64
	Truncated     bool
	Warning       string
}

// ConversationTurn is an ordered, viewer-only turn assembled from either a
// committed provider turn or an ACP turn that is not committed yet. Key is
// opaque and stable for reconciliation; it is never persisted by the gateway.
type ConversationTurn struct {
	Key        string
	Committed  bool
	Activities []ConversationActivity
}

type conversationTurn struct {
	id               string
	first            uint64
	firstAt          time.Time
	answerAt         time.Time
	activities       []ConversationActivity
	user             conversationCoverage
	userTruthful     bool
	assistant        conversationCoverage
	terminal         bool
	committable      bool
	failed           bool
	userMatchable    bool
	answerMatchable  bool
	retainActivities bool
	bytes            int
}

// conversationCoverage keeps only a normalized SHA-256 fingerprint and byte
// length. Long-lived Sessions therefore retain enough completed-turn identity
// for conservative reconciliation without retaining a second unbounded copy
// of every prompt and answer.
type conversationCoverage struct {
	digest       hash.Hash
	bytes        int
	set          bool
	pendingSpace bool
}

// Conversation returns the best complete conversation projection available at
// one captured gateway cursor. Provider history is preferred only after exact
// canonical-ID and checkout verification. Every fallback is assembled here as
// the same bounded gateway projection, so HTTP, terminal, and future transports
// can publish it atomically and continue strictly after GatewayCursor.
func (s *SessionService) Conversation(ctx context.Context, id string) (ConversationSnapshot, error) {
	current, err := s.repository.GetSession(ctx, id)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	workingDirectory := current.WorktreePath
	if workingDirectory == "" {
		workingDirectory = current.RepoPath
	}
	if s.registry == nil || strings.TrimSpace(current.ProviderConversationID) == "" {
		return s.gatewayConversationSnapshot(ctx, current.ID, s.logPath(current), "Provider history is not committed yet; showing live gateway history.")
	}
	profile, err := s.registry.Get(current.Agent)
	if err != nil {
		return s.gatewayConversationSnapshot(ctx, current.ID, s.logPath(current), "Provider history is unavailable; showing gateway history.")
	}
	provider, err := native.ReadProviderTranscript(ctx, profile, current.ProviderConversationID, workingDirectory)
	if err != nil {
		if ctx.Err() != nil {
			return ConversationSnapshot{}, ctx.Err()
		}
		return s.gatewayConversationSnapshot(ctx, current.ID, s.logPath(current), "Provider history is unavailable; showing gateway history.")
	}

	turns, diagnostics, cursor, gatewayTruncated, err := s.gatewayConversationState(ctx, current.ID, s.logPath(current))
	if err != nil {
		if ctx.Err() != nil {
			return ConversationSnapshot{}, ctx.Err()
		}
		return ConversationSnapshot{}, fmt.Errorf("project gateway history for provider reconciliation: %w", err)
	}
	return ConversationSnapshot{
		Source: ConversationSourceProvider, Provider: provider, Turns: mergeConversationTurns(provider, turns),
		Diagnostics: diagnostics, GatewayCursor: cursor, Truncated: provider.Truncated || gatewayTruncated,
	}, nil
}

func (s *SessionService) gatewayConversationSnapshot(ctx context.Context, sessionID, path, warning string) (ConversationSnapshot, error) {
	turns, diagnostics, cursor, truncated, err := s.gatewayConversationState(ctx, sessionID, path)
	if err != nil {
		if ctx.Err() != nil {
			return ConversationSnapshot{}, ctx.Err()
		}
		return ConversationSnapshot{}, fmt.Errorf("project gateway conversation: %w", err)
	}
	projection := make([]ConversationTurn, 0, len(turns))
	for _, turn := range turns {
		if turn == nil || len(turn.activities) == 0 {
			continue
		}
		projection = append(projection, ConversationTurn{
			Key: turn.id, Committed: false, Activities: append([]ConversationActivity(nil), turn.activities...),
		})
	}
	if truncated {
		warning = strings.TrimSpace(warning + " Earlier gateway history was omitted by bounded viewer limits.")
	}
	return ConversationSnapshot{
		Source: ConversationSourceGateway, Turns: projection, Diagnostics: diagnostics,
		GatewayCursor: cursor, Truncated: truncated, Warning: warning,
	}, nil
}

func (s *SessionService) gatewayConversationState(ctx context.Context, _ string, path string) ([]*conversationTurn, []ConversationActivity, uint64, bool, error) {
	active := make(map[string]*conversationTurn)
	completedByID := make(map[string]*conversationTurn)
	var completed []*conversationTurn
	var diagnostics []ConversationActivity
	var retainedActivityBytes int
	var diagnosticBytes int
	var cursor uint64
	var truncated bool
	releaseActivities := func(turn *conversationTurn) {
		if len(turn.activities) > 0 {
			truncated = true
		}
		retainedActivityBytes -= turn.bytes
		if retainedActivityBytes < 0 {
			retainedActivityBytes = 0
		}
		turn.activities = nil
		turn.bytes = 0
		turn.retainActivities = false
	}
	releaseOldestCompleted := func() bool {
		for _, turn := range completed {
			if turn.retainActivities && len(turn.activities) > 0 {
				releaseActivities(turn)
				return true
			}
		}
		return false
	}
	_, err := sessionlog.WalkFile(ctx, path, 0, 0, func(line sessionlog.Line) error {
		cursor = line.Seq
		display := presentation.Project(line)
		activity := ConversationActivity{
			Position: line.Seq, RequestID: line.RequestID, Kind: line.Type,
			Timestamp: line.Timestamp, Display: display, Permission: conversationPermission(line),
		}
		diagnostic := ConversationActivity{
			Position: line.Seq, RequestID: line.RequestID, Kind: line.Type, Timestamp: line.Timestamp,
			Summary: presentation.Summary(sessionlog.Line{Type: line.Type}, nil),
		}
		diagnosticSize := conversationActivityBytes(diagnostic)
		for len(diagnostics) > 0 && (len(diagnostics) >= conversationDiagnostics || diagnosticBytes+diagnosticSize > conversationDiagnosticBudget) {
			truncated = true
			diagnosticBytes -= conversationActivityBytes(diagnostics[0])
			diagnostics[0] = ConversationActivity{}
			diagnostics = diagnostics[1:]
		}
		if diagnosticSize <= conversationDiagnosticBudget {
			diagnostics = append(diagnostics, diagnostic)
			diagnosticBytes += diagnosticSize
		} else {
			truncated = true
		}
		activityBytes := conversationActivityBytes(activity)
		if strings.TrimSpace(line.RequestID) == "" || (display == nil && activity.Permission == nil) {
			return nil
		}
		turn := active[line.RequestID]
		if turn == nil {
			turn = completedByID[line.RequestID]
		}
		if turn == nil {
			if len(active) >= conversationActiveTurns {
				return fmt.Errorf("conversation projection exceeds %d active gateway turns", conversationActiveTurns)
			}
			turn = &conversationTurn{
				id: line.RequestID, first: line.Seq, firstAt: line.Timestamp,
				userMatchable: true, answerMatchable: true, retainActivities: true,
			}
			active[line.RequestID] = turn
		}
		if display != nil && display.Truncated {
			truncated = true
		}
		if turn.retainActivities && len(turn.activities) < conversationActivities && turn.bytes+activityBytes <= conversationActivityBudget {
			for retainedActivityBytes+activityBytes > conversationRetainedActivityBudget && releaseOldestCompleted() {
			}
			if retainedActivityBytes+activityBytes <= conversationRetainedActivityBudget {
				turn.activities = append(turn.activities, activity)
				turn.bytes += activityBytes
				retainedActivityBytes += activityBytes
			} else {
				truncated = true
			}
		} else {
			truncated = true
		}
		if display != nil {
			switch display.Type {
			case "user_message":
				turn.userMatchable = !display.Truncated
				turn.user.reset()
				turn.user.append(display.Text)
				turn.userTruthful = true
			case "legacy_user_message":
				if !turn.userTruthful {
					turn.userMatchable = turn.userMatchable && !display.Truncated
					turn.user.append(display.Text)
				}
			case "assistant_chunk":
				turn.answerMatchable = turn.answerMatchable && !display.Truncated
				turn.assistant.append(display.Text)
				if !line.Timestamp.IsZero() {
					turn.answerAt = line.Timestamp
				}
			case "turn_state":
				switch display.State {
				case "complete":
					turn.terminal = true
					turn.committable = !turn.failed
				case "cancelled":
					turn.terminal = true
					turn.failed = true
					turn.committable = false
				}
			case "error_upsert":
				turn.terminal = true
				turn.failed = true
				turn.committable = false
			}
		}
		if turn.terminal && active[line.RequestID] != nil {
			delete(active, line.RequestID)
			completed = append(completed, turn)
			completedByID[line.RequestID] = turn
			if len(completed) > conversationRetainedTurns {
				releaseActivities(completed[len(completed)-conversationRetainedTurns-1])
			}
			if len(completed) > conversationMatchTurns {
				truncated = true
				removed := completed[0]
				completed[0] = nil
				completed = completed[1:]
				if removed.retainActivities {
					releaseActivities(removed)
				}
				delete(completedByID, removed.id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, cursor, truncated, err
	}
	order := append([]*conversationTurn(nil), completed...)
	for _, turn := range active {
		order = append(order, turn)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].first < order[j].first })
	return order, diagnostics, cursor, truncated, nil
}

type providerConversationTurn struct {
	key             string
	records         []native.TranscriptRecord
	user            conversationCoverage
	displayUser     conversationCoverage
	wrappedUser     bool
	assistant       conversationCoverage
	userMatchable   bool
	answerMatchable bool
	firstAt         time.Time
	answerAt        time.Time
	hasAssistant    bool
	hasPhase        bool
	lastFinal       bool
	complete        bool
}

func mergeConversationTurns(provider native.TranscriptSnapshot, gateway []*conversationTurn) []ConversationTurn {
	providerTurns := groupProviderTurns(provider.Records)
	matchedProvider := make(map[int]*conversationTurn)
	matchedGateway := make(map[*conversationTurn]bool)
	providerCursor := 0
	for _, candidate := range gateway {
		if !candidate.committable {
			continue
		}
		providerIndex := selectProviderConversationTurn(providerTurns, providerCursor, candidate)
		if providerIndex >= 0 {
			matchedProvider[providerIndex] = candidate
			matchedGateway[candidate] = true
			providerCursor = providerIndex + 1
		}
	}

	result := make([]ConversationTurn, 0, len(providerTurns)+len(gateway))
	gatewayIndex := make(map[*conversationTurn]int, len(gateway))
	for index, turn := range gateway {
		gatewayIndex[turn] = index
	}
	nextGateway := 0
	appendUnmatchedGatewayBefore := func(limit int) {
		for nextGateway < limit {
			turn := gateway[nextGateway]
			nextGateway++
			if matchedGateway[turn] || len(turn.activities) == 0 {
				continue
			}
			result = append(result, ConversationTurn{Key: turn.id, Committed: false, Activities: append([]ConversationActivity(nil), turn.activities...)})
		}
	}
	for index, turn := range providerTurns {
		if gatewayTurn := matchedProvider[index]; gatewayTurn != nil {
			appendUnmatchedGatewayBefore(gatewayIndex[gatewayTurn])
			nextGateway = gatewayIndex[gatewayTurn] + 1
			activities := append([]ConversationActivity(nil), gatewayTurn.activities...)
			if len(activities) == 0 {
				activities = providerTurnActivities(turn, true)
			} else {
				activities = matchedCommittedActivities(turn, gatewayTurn)
			}
			result = append(result, ConversationTurn{Key: turn.key, Committed: true, Activities: activities})
			continue
		}
		result = append(result, ConversationTurn{Key: turn.key, Committed: turn.complete, Activities: providerTurnActivities(turn, turn.complete)})
	}
	appendUnmatchedGatewayBefore(len(gateway))
	return result
}

func groupProviderTurns(records []native.TranscriptRecord) []*providerConversationTurn {
	var result []*providerConversationTurn
	var current *providerConversationTurn
	for _, record := range records {
		if record.Kind == native.TranscriptUser {
			current = &providerConversationTurn{
				key: record.Key, firstAt: record.Timestamp, userMatchable: true, answerMatchable: true,
			}
			result = append(result, current)
		}
		// A bounded provider snapshot can begin in the middle of a turn after
		// old records are evicted. Never manufacture an assistant-only turn.
		if current == nil {
			continue
		}
		current.records = append(current.records, record)
		switch record.Kind {
		case native.TranscriptUser:
			current.userMatchable = current.userMatchable && !record.Truncated
			current.user.append(record.Text)
			clean := presentation.CleanUserMessage(record.Text)
			current.displayUser.append(clean)
			current.wrappedUser = clean != record.Text
		case native.TranscriptAssistant:
			current.answerMatchable = current.answerMatchable && !record.Truncated
			current.assistant.append(record.Text)
			current.hasAssistant = true
			if record.Phase != "" {
				current.hasPhase = true
				current.lastFinal = record.Phase == "final"
			}
			current.complete = current.hasAssistant && (!current.hasPhase || current.lastFinal)
			if !record.Timestamp.IsZero() {
				current.answerAt = record.Timestamp
			}
		}
	}
	// Provider stores write user/reasoning records before a final response
	// exists, and interrupted native turns can leave such groups in the middle
	// of history. They are not durable completed conversation turns. ACP
	// failures remain visible through the gateway overlay; native-only failures
	// remain available in the native harness rather than being misrepresented
	// here as successful provider turns.
	complete := result[:0]
	for _, turn := range result {
		if turn.complete {
			complete = append(complete, turn)
		}
	}
	for index := len(complete); index < len(result); index++ {
		result[index] = nil
	}
	return complete
}

func selectProviderConversationTurn(provider []*providerConversationTurn, start int, gateway *conversationTurn) int {
	var answerMatches []int
	for index := start; index < len(provider); index++ {
		candidate := provider[index]
		if !candidate.complete || !candidate.answerMatchable || !gateway.answerMatchable || !candidate.assistant.equal(gateway.assistant) ||
			!conversationTimesCompatible(candidate, gateway) {
			continue
		}
		answerMatches = append(answerMatches, index)
	}
	if len(answerMatches) == 0 {
		return -1
	}
	var exactUser []int
	if gateway.userMatchable {
		for _, index := range answerMatches {
			candidate := provider[index]
			if candidate.userMatchable && (candidate.user.equal(gateway.user) ||
				(candidate.wrappedUser && candidate.displayUser.equal(gateway.user))) {
				exactUser = append(exactUser, index)
			}
		}
	}
	if len(exactUser) == 1 {
		return exactUser[0]
	}
	if len(exactUser) > 1 || gateway.assistant.bytes < 12 {
		return -1
	}
	// A unique long answer bridges the known initial-prompt difference:
	// provider stores may retain an injected Task/Session envelope while the
	// gateway log deliberately retains only the user's clean display text.
	if len(answerMatches) == 1 {
		return answerMatches[0]
	}
	return -1
}

func conversationTimesCompatible(provider *providerConversationTurn, gateway *conversationTurn) bool {
	providerTime := provider.answerAt
	if providerTime.IsZero() {
		providerTime = provider.firstAt
	}
	gatewayTime := gateway.answerAt
	if gatewayTime.IsZero() {
		gatewayTime = gateway.firstAt
	}
	if providerTime.IsZero() || gatewayTime.IsZero() {
		return true
	}
	difference := providerTime.Sub(gatewayTime)
	if difference < 0 {
		difference = -difference
	}
	return difference <= conversationMatchWindow
}

func providerTurnActivities(turn *providerConversationTurn, complete bool) []ConversationActivity {
	capacity := len(turn.records)
	if complete {
		capacity++
	}
	activities := make([]ConversationActivity, 0, capacity)
	for _, record := range turn.records {
		display := providerRecordDisplay(record)
		if display == nil {
			continue
		}
		activities = append(activities, ConversationActivity{
			RequestID: turn.key, Kind: "provider_transcript", Timestamp: record.Timestamp, Display: display,
		})
	}
	if complete {
		activities = append(activities, ConversationActivity{
			RequestID: turn.key, Kind: "provider_turn_completed", Display: &presentation.Activity{Type: "turn_state", State: "complete", StopReason: "end_turn"},
		})
	}
	return activities
}

func providerRecordDisplay(record native.TranscriptRecord) *presentation.Activity {
	switch record.Kind {
	case native.TranscriptUser:
		return &presentation.Activity{Type: "user_message", Text: presentation.CleanUserMessage(record.Text), Truncated: record.Truncated}
	case native.TranscriptAssistant:
		return &presentation.Activity{Type: "assistant_chunk", MessageID: record.Key, Text: record.Text, Truncated: record.Truncated}
	case native.TranscriptReasoning:
		return &presentation.Activity{Type: "thought_chunk", MessageID: record.Key, Text: record.Text, Truncated: record.Truncated}
	default:
		return nil
	}
}

// matchedCommittedActivities makes the verified provider store authoritative
// for committed user, reasoning, and assistant content. Gateway records remain
// authoritative for live-only structure such as lifecycle, tools, plans, and
// permissions. ACP reasoning is retained only when it adds readable content
// that is not the provider's normalized committed reasoning.
func matchedCommittedActivities(turn *providerConversationTurn, gateway *conversationTurn) []ConversationActivity {
	provider := make([]ConversationActivity, 0, len(turn.records))
	for _, record := range turn.records {
		display := providerRecordDisplay(record)
		if display == nil {
			continue
		}
		provider = append(provider, ConversationActivity{
			RequestID: gateway.id, Kind: "provider_transcript", Timestamp: record.Timestamp, Display: display,
		})
	}
	groups := matchedGatewayActivityGroups(gateway.activities)
	supplementalReasoning := supplementalACPReasoning(groups, turn)
	result := make([]ConversationActivity, 0, len(gateway.activities)+len(turn.records))
	providerIndex := 0
	emitThroughType := func(displayType string) bool {
		match := -1
		for index := providerIndex; index < len(provider); index++ {
			if provider[index].Display != nil && provider[index].Display.Type == displayType {
				match = index
				break
			}
		}
		if match < 0 {
			return false
		}
		end := match + 1
		for end < len(provider) && provider[end].Display != nil && provider[end].Display.Type == displayType {
			end++
		}
		result = append(result, provider[providerIndex:end]...)
		providerIndex = end
		return true
	}
	emitRemaining := func() {
		result = append(result, provider[providerIndex:]...)
		providerIndex = len(provider)
	}

	for _, group := range groups {
		if group.displayType != "" {
			emitThroughType(group.displayType)
			if group.displayType == "thought_chunk" {
				result = append(result, supplementalReasoning[group.start]...)
			}
			continue
		}
		for _, activity := range group.activities {
			if activity.Display != nil && activity.Display.Type == "turn_state" && activity.Display.State == "complete" {
				emitRemaining()
			}
			result = append(result, activity)
		}
	}
	emitRemaining()
	return result
}

type matchedGatewayActivityGroup struct {
	start       int
	displayType string
	activities  []ConversationActivity
}

func matchedGatewayActivityGroups(activities []ConversationActivity) []matchedGatewayActivityGroup {
	groups := make([]matchedGatewayActivityGroup, 0, len(activities))
	for index := 0; index < len(activities); {
		displayType := providerSemanticDisplayType(activities[index])
		if displayType == "" {
			groups = append(groups, matchedGatewayActivityGroup{start: index, activities: []ConversationActivity{activities[index]}})
			index++
			continue
		}
		end := index + 1
		for end < len(activities) && providerSemanticDisplayType(activities[end]) == displayType {
			end++
		}
		groups = append(groups, matchedGatewayActivityGroup{
			start: index, displayType: displayType, activities: activities[index:end],
		})
		index = end
	}
	return groups
}

func providerSemanticDisplayType(activity ConversationActivity) string {
	if activity.Display == nil {
		return ""
	}
	switch activity.Display.Type {
	case "user_message", "legacy_user_message":
		return "user_message"
	case "thought_chunk", "assistant_chunk":
		return activity.Display.Type
	default:
		return ""
	}
}

func supplementalACPReasoning(groups []matchedGatewayActivityGroup, turn *providerConversationTurn) map[int][]ConversationActivity {
	result := make(map[int][]ConversationActivity)
	var providerRecords []native.TranscriptRecord
	for _, record := range turn.records {
		if record.Kind == native.TranscriptReasoning && strings.TrimSpace(record.Text) != "" {
			providerRecords = append(providerRecords, record)
		}
	}
	type reasoningTokens struct {
		group  matchedGatewayActivityGroup
		tokens []string
		flat   []int
	}
	var reasoningGroups []reasoningTokens
	var gatewayTokens []string
	for _, group := range groups {
		if group.displayType != "thought_chunk" {
			continue
		}
		var text strings.Builder
		for _, activity := range group.activities {
			if activity.Display != nil {
				text.WriteString(activity.Display.Text)
			}
		}
		tokens := strings.Fields(text.String())
		flat := make([]int, len(tokens))
		for index, token := range tokens {
			flat[index] = len(gatewayTokens)
			gatewayTokens = append(gatewayTokens, token)
		}
		reasoningGroups = append(reasoningGroups, reasoningTokens{group: group, tokens: tokens, flat: flat})
	}
	if len(providerRecords) == 0 {
		for _, group := range reasoningGroups {
			result[group.group.start] = append([]ConversationActivity(nil), group.group.activities...)
		}
		return result
	}

	removed := make([]bool, len(gatewayTokens))
	cursor := 0
	for _, record := range providerRecords {
		providerTokens := strings.Fields(record.Text)
		match := tokenSequenceIndex(gatewayTokens, providerTokens, cursor)
		if match < 0 {
			continue
		}
		for index := match; index < match+len(providerTokens); index++ {
			removed[index] = true
		}
		cursor = match + len(providerTokens)
	}
	for _, group := range reasoningGroups {
		var remaining []string
		removedFromGroup := false
		for index, token := range group.tokens {
			if removed[group.flat[index]] {
				removedFromGroup = true
				continue
			}
			remaining = append(remaining, token)
		}
		if !removedFromGroup {
			result[group.group.start] = append([]ConversationActivity(nil), group.group.activities...)
			continue
		}
		if len(remaining) == 0 || len(group.group.activities) == 0 {
			continue
		}
		supplement := group.group.activities[0]
		if supplement.Display == nil {
			continue
		}
		display := *supplement.Display
		display.Text = strings.Join(remaining, " ")
		for _, activity := range group.group.activities[1:] {
			if activity.Display != nil {
				display.Truncated = display.Truncated || activity.Display.Truncated
			}
		}
		supplement.Display = &display
		result[group.group.start] = []ConversationActivity{supplement}
	}
	return result
}

func tokenSequenceIndex(values, pattern []string, start int) int {
	if len(pattern) == 0 || start < 0 || len(pattern) > len(values)-start {
		return -1
	}
	for index := start; index+len(pattern) <= len(values); index++ {
		matched := true
		for offset := range pattern {
			if values[index+offset] != pattern[offset] {
				matched = false
				break
			}
		}
		if matched {
			return index
		}
	}
	return -1
}

func (c *conversationCoverage) append(value string) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		if c.set && strings.TrimSpace(value) == "" && value != "" {
			c.pendingSpace = true
		}
		return
	}
	if c.digest == nil {
		c.digest = sha256.New()
	}
	if c.set && len(strings.TrimLeftFunc(value, unicode.IsSpace)) < len(value) {
		c.pendingSpace = true
	}
	for _, field := range fields {
		if c.set && c.pendingSpace {
			_, _ = c.digest.Write([]byte{' '})
			c.bytes++
		}
		_, _ = c.digest.Write([]byte(field))
		c.bytes += len(field)
		c.set = true
		c.pendingSpace = true
	}
	// Fields within one value are separated, but a chunk boundary is not.
	// Preserve a pending separator only when the value itself ended in space.
	c.pendingSpace = len(strings.TrimRightFunc(value, unicode.IsSpace)) < len(value)
}

func (c *conversationCoverage) reset() {
	c.digest = nil
	c.bytes = 0
	c.set = false
	c.pendingSpace = false
}

func (c conversationCoverage) equal(other conversationCoverage) bool {
	if !c.set || !other.set || c.bytes != other.bytes || c.digest == nil || other.digest == nil {
		return false
	}
	return bytes.Equal(c.digest.Sum(nil), other.digest.Sum(nil))
}

func conversationActivityBytes(activity ConversationActivity) int {
	result := len(activity.RequestID) + len(activity.Kind) + len(activity.Summary) + 128
	if activity.Display != nil {
		encoded, _ := json.Marshal(activity.Display)
		result += len(encoded)
	}
	if activity.Permission != nil {
		encoded, _ := json.Marshal(activity.Permission)
		result += len(encoded)
	}
	return result
}

func conversationPermission(line sessionlog.Line) *ConversationPermission {
	switch line.Type {
	case "permission_requested":
		var pending worker.PendingPermission
		if json.Unmarshal(line.Data, &pending) != nil || pending.ID == "" {
			return nil
		}
		return &ConversationPermission{Pending: &pending, ID: pending.ID, State: "waiting"}
	case "permission_resolved", "permission_abandoned":
		var value struct {
			ID       string `json:"permission_id"`
			Decision string `json:"decision"`
		}
		if json.Unmarshal(line.Data, &value) != nil || value.ID == "" {
			return nil
		}
		state := "resolved"
		decision := value.Decision
		if line.Type == "permission_abandoned" {
			state, decision = "abandoned", "unavailable"
		}
		return &ConversationPermission{ID: value.ID, State: state, Decision: decision}
	default:
		return nil
	}
}
