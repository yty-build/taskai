package native

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/agents"
)

const (
	maxProviderTranscriptRecords        = 2048
	maxProviderTranscriptTextBytes      = 128 << 10
	maxProviderTranscriptAggregateBytes = 4 << 20
)

// TranscriptKind is a provider-independent, display-safe conversation role.
// Provider control records, authentication data, system instructions, raw
// tool payloads, and encrypted reasoning never become TranscriptRecords.
type TranscriptKind string

const (
	TranscriptUser      TranscriptKind = "user"
	TranscriptAssistant TranscriptKind = "assistant"
	TranscriptReasoning TranscriptKind = "reasoning"
)

// TranscriptRecord is a bounded semantic projection of one provider-owned
// transcript record. Key is opaque outside this package but stable while the
// provider's own record identity is stable.
type TranscriptRecord struct {
	Key       string
	Kind      TranscriptKind
	Text      string
	Timestamp time.Time
	Phase     string
	Truncated bool
}

// TranscriptSnapshot is disposable presentation input, not a persistent
// domain entity. Checkpoint is an opaque content generation suitable for
// change detection; callers must not parse it.
type TranscriptSnapshot struct {
	Records    []TranscriptRecord
	Checkpoint string
	Truncated  bool
}

// StoreTranscriptReader reads exact, provider-owned conversation history.
// Verifier defaults to StoreVerifier. Supplying a Verification still causes
// exact identity and checkout verification to be repeated before any content
// is returned, preventing a stale or forged path from being trusted.
type StoreTranscriptReader struct {
	Verifier Verifier
}

// ReadProviderTranscript verifies and reads one exact provider conversation.
func ReadProviderTranscript(ctx context.Context, profile agents.Profile, conversationID, workingDirectory string) (TranscriptSnapshot, error) {
	return (StoreTranscriptReader{}).Read(ctx, profile, conversationID, workingDirectory)
}

// ReadVerifiedProviderTranscript revalidates a prior verification before
// reading, so callers cannot accidentally trust a stale provider-store path.
func ReadVerifiedProviderTranscript(ctx context.Context, profile agents.Profile, verification Verification, workingDirectory string) (TranscriptSnapshot, error) {
	return (StoreTranscriptReader{}).ReadVerified(ctx, profile, verification, workingDirectory)
}

func (r StoreTranscriptReader) Read(ctx context.Context, profile agents.Profile, conversationID, workingDirectory string) (TranscriptSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptSnapshot{}, err
	}
	verification, err := r.verifier().Verify(ctx, profile, conversationID, workingDirectory)
	if err != nil {
		return TranscriptSnapshot{}, err
	}
	return readProviderTranscript(ctx, profile, verification, workingDirectory)
}

func (r StoreTranscriptReader) ReadVerified(ctx context.Context, profile agents.Profile, verification Verification, workingDirectory string) (TranscriptSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptSnapshot{}, err
	}
	if strings.TrimSpace(verification.ConversationID) == "" || strings.TrimSpace(verification.StorePath) == "" {
		return TranscriptSnapshot{}, fmt.Errorf("complete provider-store verification is required")
	}
	current, err := r.verifier().Verify(ctx, profile, verification.ConversationID, workingDirectory)
	if err != nil {
		return TranscriptSnapshot{}, err
	}
	providedPath, err := canonicalStorePath(verification.StorePath)
	if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("resolve verified provider store: %w", err)
	}
	currentPath, err := canonicalStorePath(current.StorePath)
	if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("resolve current provider store: %w", err)
	}
	if providedPath != currentPath || strings.TrimSpace(current.ConversationID) != strings.TrimSpace(verification.ConversationID) {
		return TranscriptSnapshot{}, fmt.Errorf("provider transcript verification changed; refusing mismatched store path")
	}
	current.StorePath = currentPath
	return readProviderTranscript(ctx, profile, current, workingDirectory)
}

func (r StoreTranscriptReader) verifier() Verifier {
	if r.Verifier != nil {
		return r.Verifier
	}
	return StoreVerifier{}
}

func readProviderTranscript(ctx context.Context, profile agents.Profile, verification Verification, workingDirectory string) (TranscriptSnapshot, error) {
	expectedDirectory, err := canonicalDirectory(workingDirectory)
	if err != nil {
		return TranscriptSnapshot{}, fmt.Errorf("resolve Session checkout: %w", err)
	}
	builder := newTranscriptBuilder(profile.Name, verification.ConversationID)
	switch profile.Name {
	case "codex":
		err = readCodexTranscript(ctx, verification, expectedDirectory, builder)
	case "claude":
		err = readClaudeTranscript(ctx, verification, expectedDirectory, builder)
	case "grok", "grok-build":
		err = readGrokTranscript(ctx, verification, expectedDirectory, builder)
	case "opencode":
		err = readOpenCodeTranscript(ctx, verification, expectedDirectory, builder)
	default:
		err = fmt.Errorf("agent %q does not support provider transcript reading", profile.Name)
	}
	if err != nil {
		return TranscriptSnapshot{}, err
	}
	return builder.snapshot(), nil
}

func canonicalStorePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	evaluated, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(evaluated), nil
}

type transcriptBuilder struct {
	provider       string
	conversationID string
	records        []TranscriptRecord
	retainedBytes  int
	start          int
	truncated      bool
}

func newTranscriptBuilder(provider, conversationID string) *transcriptBuilder {
	return &transcriptBuilder{provider: provider, conversationID: conversationID}
}

func (b *transcriptBuilder) add(kind TranscriptKind, providerIdentity, fallback string, timestamp time.Time, phase, text string) {
	text = sanitizeTranscriptText(text)
	if strings.TrimSpace(text) == "" {
		return
	}
	text, textTruncated := truncateTranscriptText(text, maxProviderTranscriptTextBytes)
	keySource := strings.TrimSpace(providerIdentity)
	if keySource == "" {
		keySource = fallback
	}
	record := TranscriptRecord{
		Key: opaqueTranscriptKey(b.provider, b.conversationID, string(kind), keySource), Kind: kind,
		Text: text, Timestamp: timestamp.UTC(), Phase: safeTranscriptPhase(phase), Truncated: textTruncated,
	}
	b.records = append(b.records, record)
	b.retainedBytes += transcriptRecordBytes(record)
	if textTruncated {
		b.truncated = true
	}
	for len(b.records)-b.start > maxProviderTranscriptRecords || b.retainedBytes > maxProviderTranscriptAggregateBytes {
		b.retainedBytes -= transcriptRecordBytes(b.records[b.start])
		b.records[b.start] = TranscriptRecord{}
		b.start++
		b.truncated = true
	}
	// Streaming removes any whole-source ceiling, so compact evicted slots
	// periodically. Without this, the visible window would be bounded while the
	// backing slice still grew in proportion to the provider's lifetime history.
	if b.start >= maxProviderTranscriptRecords && b.start*2 >= len(b.records) {
		retained := len(b.records) - b.start
		compacted := make([]TranscriptRecord, retained, maxProviderTranscriptRecords*2)
		copy(compacted, b.records[b.start:])
		b.records = compacted
		b.start = 0
	}
}

func (b *transcriptBuilder) snapshot() TranscriptSnapshot {
	records := append([]TranscriptRecord(nil), b.records[b.start:]...)
	hash := sha256.New()
	_, _ = io.WriteString(hash, "provider-transcript-v1\x00"+b.provider+"\x00"+b.conversationID+"\x00")
	for _, record := range records {
		_, _ = io.WriteString(hash, record.Key+"\x00"+string(record.Kind)+"\x00"+record.Timestamp.Format(time.RFC3339Nano)+"\x00"+record.Phase+"\x00"+strconv.FormatBool(record.Truncated)+"\x00"+record.Text+"\x00")
	}
	if b.truncated {
		_, _ = io.WriteString(hash, "truncated\x00")
	}
	return TranscriptSnapshot{Records: records, Checkpoint: "pt1_" + hex.EncodeToString(hash.Sum(nil)), Truncated: b.truncated}
}

func transcriptRecordBytes(record TranscriptRecord) int {
	return len(record.Key) + len(record.Kind) + len(record.Text) + len(record.Phase) + 64
}

func opaqueTranscriptKey(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, value)
		_, _ = hash.Write([]byte{0})
	}
	return "pr1_" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func safeTranscriptPhase(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "final", "final_answer":
		return "final"
	case "commentary", "analysis":
		return "commentary"
	default:
		return ""
	}
}

func sanitizeTranscriptText(value string) string {
	value = ansi.Strip(strings.ToValidUTF8(value, "�"))
	var builder strings.Builder
	builder.Grow(len(value))
	lastCR := false
	for _, character := range value {
		switch {
		case character == '\r':
			builder.WriteByte('\n')
			lastCR = true
		case character == '\n':
			if !lastCR {
				builder.WriteByte('\n')
			}
			lastCR = false
		case character == '\t':
			builder.WriteByte('\t')
			lastCR = false
		case character < 0x20 || (character >= 0x7f && character <= 0x9f):
			lastCR = false
		default:
			builder.WriteRune(character)
			lastCR = false
		}
	}
	return strings.TrimSpace(builder.String())
}

func truncateTranscriptText(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func parseTranscriptTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func walkBoundedJSONLines(ctx context.Context, path string, consume func(int, json.RawMessage) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(contextTranscriptReader{ctx: ctx, reader: file}, 64<<10)
	for lineNumber := 1; ; lineNumber++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, terminated, err := readBoundedJSONLine(reader, lineNumber)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			if !terminated {
				return nil
			}
			return fmt.Errorf("decode provider transcript line %d: invalid JSON", lineNumber)
		}
		if err := consume(lineNumber, append(json.RawMessage(nil), line...)); err != nil {
			return err
		}
	}
	// Unreachable: every exit is an EOF, cancellation, validation, or callback result.
}

// readBoundedJSONLine reads one physical JSONL line without ever buffering
// more than the accepted per-line limit. The boolean reports whether a newline
// terminated the record, allowing callers to ignore only a malformed trailing
// partial write while still rejecting malformed interior records.
func readBoundedJSONLine(reader *bufio.Reader, lineNumber int) ([]byte, bool, error) {
	line := make([]byte, 0, 64<<10)
	for {
		fragment, err := reader.ReadSlice('\n')
		terminated := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if terminated {
			fragment = fragment[:len(fragment)-1]
		}
		if len(line)+len(fragment) > maxTranscriptLineBytes {
			return nil, terminated, fmt.Errorf("provider transcript line %d exceeds %d MiB limit", lineNumber, maxTranscriptLineBytes>>20)
		}
		line = append(line, fragment...)
		if terminated {
			return line, true, nil
		}
		switch err {
		case nil:
			continue
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, false, io.EOF
			}
			return line, false, nil
		default:
			return nil, false, err
		}
	}
}

type contextTranscriptReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextTranscriptReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func readCodexTranscript(ctx context.Context, verification Verification, expectedDirectory string, builder *transcriptBuilder) error {
	path, err := canonicalStorePath(verification.StorePath)
	if err != nil {
		return providerStoreError("Codex", verification.StorePath, err)
	}
	identitySeen := false
	err = walkBoundedJSONLines(ctx, path, func(lineNumber int, encoded json.RawMessage) error {
		var envelope struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			return err
		}
		switch envelope.Type {
		case "session_meta":
			var payload struct {
				ID  string `json:"id"`
				CWD string `json:"cwd"`
			}
			if json.Unmarshal(envelope.Payload, &payload) != nil {
				return fmt.Errorf("decode Codex session identity")
			}
			if payload.ID != verification.ConversationID || !directoryMatches(payload.CWD, expectedDirectory) {
				return fmt.Errorf("Codex transcript identity or checkout changed; refusing mismatched history")
			}
			identitySeen = true
		case "event_msg":
			var payload struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Payload, &payload) == nil && payload.Type == "user_message" {
				builder.add(TranscriptUser, "", "codex-user:"+strconv.Itoa(lineNumber), parseTranscriptTime(envelope.Timestamp), "", payload.Message)
			}
		case "response_item":
			var payload struct {
				ID      string `json:"id"`
				Type    string `json:"type"`
				Role    string `json:"role"`
				Phase   string `json:"phase"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Summary []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"summary"`
			}
			if json.Unmarshal(envelope.Payload, &payload) != nil {
				return nil
			}
			if payload.Type == "message" && payload.Role == "assistant" {
				var text []string
				for _, content := range payload.Content {
					if content.Type == "output_text" || content.Type == "text" {
						text = append(text, content.Text)
					}
				}
				builder.add(TranscriptAssistant, payload.ID, "codex-assistant:"+strconv.Itoa(lineNumber), parseTranscriptTime(envelope.Timestamp), payload.Phase, strings.Join(text, "\n"))
			}
			if payload.Type == "reasoning" {
				var summaries []string
				for _, summary := range payload.Summary {
					if summary.Type == "summary_text" || summary.Type == "text" {
						summaries = append(summaries, summary.Text)
					}
				}
				builder.add(TranscriptReasoning, payload.ID, "codex-reasoning:"+strconv.Itoa(lineNumber), parseTranscriptTime(envelope.Timestamp), "", strings.Join(summaries, "\n"))
			}
		}
		return nil
	})
	if err != nil {
		return providerStoreError("Codex", path, err)
	}
	if !identitySeen {
		return fmt.Errorf("Codex transcript did not contain the verified conversation identity")
	}
	return nil
}

func readClaudeTranscript(ctx context.Context, verification Verification, expectedDirectory string, builder *transcriptBuilder) error {
	path, err := canonicalStorePath(verification.StorePath)
	if err != nil {
		return providerStoreError("Claude", verification.StorePath, err)
	}
	lineage := make(map[string]bool)
	identitySeen := false
	err = walkBoundedJSONLines(ctx, path, func(lineNumber int, encoded json.RawMessage) error {
		var record struct {
			Type         string `json:"type"`
			UUID         string `json:"uuid"`
			ParentUUID   string `json:"parentUuid"`
			SessionID    string `json:"sessionId"`
			SessionIDAlt string `json:"session_id"`
			CWD          string `json:"cwd"`
			Timestamp    string `json:"timestamp"`
			PromptSource string `json:"promptSource"`
			IsMeta       bool   `json:"isMeta"`
			IsSidechain  bool   `json:"isSidechain"`
			Origin       struct {
				Kind string `json:"kind"`
			} `json:"origin"`
			Message struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				StopReason string          `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(encoded, &record) != nil {
			return nil
		}
		semantic := record.Type == "user" || record.Type == "assistant"
		transparent := record.Type == "attachment" || record.Type == "system"
		if !semantic && !transparent {
			return nil
		}
		sessionID := record.SessionID
		if sessionID == "" {
			sessionID = record.SessionIDAlt
		}
		if sessionID != verification.ConversationID || !directoryMatches(record.CWD, expectedDirectory) {
			return fmt.Errorf("Claude transcript identity or checkout changed; refusing mismatched history")
		}
		identitySeen = true
		if transparent {
			lineage[record.UUID] = !record.IsSidechain && lineage[record.ParentUUID]
			return nil
		}
		if record.Type == "user" {
			allowed := !record.IsMeta && !record.IsSidechain && record.Message.Role == "user" &&
				(record.Origin.Kind == "human" || record.PromptSource == "sdk" || record.PromptSource == "typed")
			if !allowed {
				// Claude represents a tool result as a user-role protocol
				// message. It is not user-visible text, but it keeps the
				// assistant branch rooted in the preceding human prompt.
				lineage[record.UUID] = !record.IsMeta && !record.IsSidechain &&
					lineage[record.ParentUUID] && claudeContainsToolResult(record.Message.Content)
				return nil
			}
			lineage[record.UUID] = true
			text := claudeUserText(record.Message.Content)
			builder.add(TranscriptUser, record.UUID, "claude-user:"+strconv.Itoa(lineNumber), parseTranscriptTime(record.Timestamp), "", text)
			return nil
		}
		allowed := !record.IsSidechain && record.Message.Role == "assistant" && lineage[record.ParentUUID]
		lineage[record.UUID] = allowed
		if !allowed {
			return nil
		}
		for blockIndex, block := range claudeAssistantBlocks(record.Message.Content) {
			phase := block.Phase
			if block.Kind == TranscriptAssistant {
				phase = claudeAssistantPhase(record.Message.StopReason)
			}
			builder.add(block.Kind, record.UUID+":"+strconv.Itoa(blockIndex), "claude-assistant:"+strconv.Itoa(lineNumber)+":"+strconv.Itoa(blockIndex), parseTranscriptTime(record.Timestamp), phase, block.Text)
		}
		return nil
	})
	if err != nil {
		return providerStoreError("Claude", path, err)
	}
	if !identitySeen {
		return fmt.Errorf("Claude transcript did not contain the verified conversation identity")
	}
	return nil
}

func claudeAssistantPhase(stopReason string) string {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "", "tool_use", "tool-use", "tool_calls", "tool-calls":
		return "commentary"
	default:
		return "final"
	}
}

func claudeContainsToolResult(encoded json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(encoded, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

type transcriptBlock struct {
	Kind  TranscriptKind
	Text  string
	Phase string
}

func claudeUserText(encoded json.RawMessage) string {
	var direct string
	if json.Unmarshal(encoded, &direct) == nil {
		return direct
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(encoded, &blocks) != nil {
		return ""
	}
	var text []string
	for _, block := range blocks {
		if block.Type == "text" {
			text = append(text, block.Text)
		}
	}
	return strings.Join(text, "\n")
}

func claudeAssistantBlocks(encoded json.RawMessage) []transcriptBlock {
	var direct string
	if json.Unmarshal(encoded, &direct) == nil {
		return []transcriptBlock{{Kind: TranscriptAssistant, Text: direct}}
	}
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if json.Unmarshal(encoded, &blocks) != nil {
		return nil
	}
	result := make([]transcriptBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			result = append(result, transcriptBlock{Kind: TranscriptAssistant, Text: block.Text})
		case "thinking":
			result = append(result, transcriptBlock{Kind: TranscriptReasoning, Text: block.Thinking})
		}
	}
	return result
}

func readGrokTranscript(ctx context.Context, verification Verification, expectedDirectory string, builder *transcriptBuilder) error {
	root, err := canonicalStorePath(verification.StorePath)
	if err != nil {
		return providerStoreError("Grok", verification.StorePath, err)
	}
	if filepath.Base(root) != verification.ConversationID {
		return fmt.Errorf("Grok transcript path does not match the verified conversation identity")
	}
	group := filepath.Dir(root)
	groupDirectory := ""
	if body, readErr := os.ReadFile(filepath.Join(group, ".cwd")); readErr == nil {
		groupDirectory = strings.TrimSpace(string(body))
	}
	if groupDirectory == "" {
		if decoded, decodeErr := url.PathUnescape(filepath.Base(group)); decodeErr == nil {
			groupDirectory = decoded
		}
	}
	if !directoryMatches(groupDirectory, expectedDirectory) {
		return fmt.Errorf("Grok transcript checkout changed; refusing mismatched history")
	}
	path := filepath.Join(root, "chat_history.jsonl")
	resolved, err := canonicalStorePath(path)
	if err != nil {
		return providerStoreError("Grok", path, err)
	}
	if filepath.Dir(resolved) != root {
		return fmt.Errorf("Grok transcript path escaped the verified conversation directory")
	}
	err = walkBoundedJSONLines(ctx, resolved, func(lineNumber int, encoded json.RawMessage) error {
		var record struct {
			Type            string          `json:"type"`
			ID              string          `json:"id"`
			PromptIndex     *int            `json:"prompt_index"`
			SyntheticReason string          `json:"synthetic_reason"`
			Content         json.RawMessage `json:"content"`
			Summary         json.RawMessage `json:"summary"`
		}
		if json.Unmarshal(encoded, &record) != nil {
			return nil
		}
		switch record.Type {
		case "user":
			if record.PromptIndex == nil || record.SyntheticReason != "" {
				return nil
			}
			text := grokUserText(record.Content)
			builder.add(TranscriptUser, "prompt:"+strconv.Itoa(*record.PromptIndex), "grok-user:"+strconv.Itoa(lineNumber), time.Time{}, "", text)
		case "reasoning":
			builder.add(TranscriptReasoning, record.ID, "grok-reasoning:"+strconv.Itoa(lineNumber), time.Time{}, "", grokReasoningSummary(record.Summary))
		case "assistant":
			var text string
			if json.Unmarshal(record.Content, &text) == nil {
				// Grok appends an assistant chat-history record only after that
				// response is complete. In-flight output is not a valid JSONL
				// assistant record; a trailing partial line is ignored by the
				// bounded walker.
				builder.add(TranscriptAssistant, "", "grok-assistant:"+strconv.Itoa(lineNumber), time.Time{}, "final", text)
			}
		}
		return nil
	})
	if err != nil {
		return providerStoreError("Grok", resolved, err)
	}
	return nil
}

func grokReasoningSummary(encoded json.RawMessage) string {
	var direct string
	if json.Unmarshal(encoded, &direct) == nil {
		return direct
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(encoded, &blocks) != nil {
		return ""
	}
	var text []string
	for _, block := range blocks {
		if block.Type == "summary_text" || block.Type == "text" {
			text = append(text, block.Text)
		}
	}
	return strings.Join(text, "\n")
}

func grokUserText(encoded json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(encoded, &blocks) != nil {
		return ""
	}
	var text []string
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		value := strings.TrimSpace(block.Text)
		if strings.HasPrefix(value, "<user_query>") && strings.HasSuffix(value, "</user_query>") {
			value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "<user_query>"), "</user_query>"))
		}
		text = append(text, value)
	}
	return strings.Join(text, "\n")
}

func readOpenCodeTranscript(ctx context.Context, verification Verification, expectedDirectory string, builder *transcriptBuilder) error {
	path, err := canonicalStorePath(verification.StorePath)
	if err != nil {
		return providerStoreError("OpenCode", verification.StorePath, err)
	}
	connectionURL := url.URL{Scheme: "file", Path: path}
	query := connectionURL.Query()
	query.Set("mode", "ro")
	connectionURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", connectionURL.String())
	if err != nil {
		return providerStoreError("OpenCode", path, err)
	}
	defer database.Close()
	transaction, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return providerStoreError("OpenCode", path, err)
	}
	defer transaction.Rollback()
	var directory string
	if err := transaction.QueryRowContext(ctx, `SELECT directory FROM session WHERE id = ?`, verification.ConversationID).Scan(&directory); err != nil {
		return providerStoreError("OpenCode", path, err)
	}
	if !directoryMatches(directory, expectedDirectory) {
		return fmt.Errorf("OpenCode transcript identity or checkout changed; refusing mismatched history")
	}
	rows, err := transaction.QueryContext(ctx, `
		WITH recent AS (
			SELECT p.id, p.message_id, p.time_created, p.data,
			       json_extract(m.data, '$.role') AS role,
			       json_extract(p.data, '$.type') AS part_type,
			       json_extract(m.data, '$.time.completed') AS message_completed,
			       json_extract(m.data, '$.finish') AS message_finish
			FROM part p JOIN message m ON m.id = p.message_id AND m.session_id = p.session_id
			WHERE p.session_id = ?
			  AND json_extract(m.data, '$.role') IN ('user', 'assistant')
			  AND json_extract(p.data, '$.type') IN ('text', 'reasoning')
			ORDER BY p.time_created DESC, p.id DESC
			LIMIT ?
		)
		SELECT id, message_id, time_created, role, part_type, message_completed, message_finish,
		       CAST(substr(CAST(json_extract(data, '$.text') AS BLOB), 1, ?) AS TEXT),
		       length(CAST(json_extract(data, '$.text') AS BLOB))
		FROM recent
		ORDER BY time_created, id`, verification.ConversationID, maxProviderTranscriptRecords+1, maxProviderTranscriptTextBytes+1)
	if err != nil {
		return providerStoreError("OpenCode", path, err)
	}
	defer rows.Close()
	rowCount := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var partID, messageID, role, partType string
		var created int64
		var completed sql.NullInt64
		var finish sql.NullString
		var text sql.NullString
		var textBytes sql.NullInt64
		if err := rows.Scan(&partID, &messageID, &created, &role, &partType, &completed, &finish, &text, &textBytes); err != nil {
			return providerStoreError("OpenCode", path, err)
		}
		rowCount++
		if rowCount > maxProviderTranscriptRecords {
			builder.truncated = true
		}
		kind := TranscriptAssistant
		if role == "user" {
			kind = TranscriptUser
		} else if partType == "reasoning" {
			kind = TranscriptReasoning
		}
		phase := ""
		if role == "assistant" {
			phase = "commentary"
			if completed.Valid && completed.Int64 > 0 && finish.Valid && openCodeFinishIsFinal(finish.String) {
				phase = "final"
			}
		}
		builder.add(kind, partID, "opencode:"+messageID+":"+strconv.Itoa(rowCount), time.UnixMilli(created), phase, text.String)
		if textBytes.Valid && textBytes.Int64 > maxProviderTranscriptTextBytes {
			builder.truncated = true
			if len(builder.records) > builder.start {
				builder.records[len(builder.records)-1].Truncated = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return providerStoreError("OpenCode", path, err)
	}
	return nil
}

func openCodeFinishIsFinal(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "tool-calls", "tool_calls", "tool-use", "tool_use":
		return false
	default:
		return true
	}
}
