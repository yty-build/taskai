package native

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yty-build/acp-gateway/internal/agents"
	_ "modernc.org/sqlite"
)

const (
	maxConversationIDLength = 512
	maxTranscriptBytes      = 4 << 20
	maxTranscriptLineBytes  = 1 << 20
)

type Verification struct {
	ConversationID string
	StorePath      string
}

type Verifier interface {
	Verify(context.Context, agents.Profile, string, string) (Verification, error)
}

// StoreVerifier proves an exact provider conversation ID against the
// provider's own local store and the expected repo/worktree. It never selects a
// record by modification time or falls back to another conversation.
type StoreVerifier struct{}

func (StoreVerifier) Verify(ctx context.Context, profile agents.Profile, conversationID, workingDirectory string) (Verification, error) {
	conversationID = strings.TrimSpace(conversationID)
	if err := validateConversationID(conversationID); err != nil {
		return Verification{}, err
	}
	expectedDirectory, err := canonicalDirectory(workingDirectory)
	if err != nil {
		return Verification{}, fmt.Errorf("resolve Session checkout: %w", err)
	}
	switch profile.Name {
	case "codex":
		return verifyCodex(ctx, profile, conversationID, expectedDirectory)
	case "claude":
		return verifyClaude(ctx, profile, conversationID, expectedDirectory)
	case "grok", "grok-build":
		return verifyGrok(ctx, profile, conversationID, expectedDirectory)
	case "opencode":
		return verifyOpenCode(ctx, profile, conversationID, expectedDirectory)
	default:
		return Verification{}, fmt.Errorf("agent %q does not support exact provider-store verification", profile.Name)
	}
}

func verifyCodex(ctx context.Context, profile agents.Profile, conversationID, expectedDirectory string) (Verification, error) {
	home, err := providerHome(profile, "CODEX_HOME", ".codex")
	if err != nil {
		return Verification{}, err
	}
	root := filepath.Join(home, "sessions")
	return findExactTranscript(ctx, root, func(path string, entry fs.DirEntry) (bool, error) {
		name := entry.Name()
		if name != conversationID+".jsonl" && !strings.HasSuffix(name, "-"+conversationID+".jsonl") {
			return false, nil
		}
		matched, err := scanJSONLines(path, func(record map[string]any) bool {
			payload, _ := record["payload"].(map[string]any)
			return stringMapValue(payload, "id", "session_id") == conversationID && directoryMatches(stringMapValue(payload, "cwd"), expectedDirectory)
		})
		return matched, err
	}, conversationID, "Codex")
}

func verifyClaude(ctx context.Context, profile agents.Profile, conversationID, expectedDirectory string) (Verification, error) {
	home, err := providerHome(profile, "CLAUDE_CONFIG_DIR", ".claude")
	if err != nil {
		return Verification{}, err
	}
	root := filepath.Join(home, "projects")
	return findExactTranscript(ctx, root, func(path string, entry fs.DirEntry) (bool, error) {
		if entry.Name() != conversationID+".jsonl" {
			return false, nil
		}
		return scanJSONLines(path, func(record map[string]any) bool {
			return stringMapValue(record, "sessionId", "session_id") == conversationID && directoryMatches(stringMapValue(record, "cwd"), expectedDirectory)
		})
	}, conversationID, "Claude")
}

func verifyGrok(ctx context.Context, profile agents.Profile, conversationID, expectedDirectory string) (Verification, error) {
	home, err := providerHome(profile, "GROK_HOME", ".grok")
	if err != nil {
		return Verification{}, err
	}
	root := filepath.Join(home, "sessions")
	groups, err := os.ReadDir(root)
	if err != nil {
		return Verification{}, providerStoreError("Grok", root, err)
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		if !group.IsDir() {
			continue
		}
		candidate := filepath.Join(root, group.Name(), conversationID)
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.IsDir() {
			continue
		}
		groupDirectory := ""
		if body, readErr := os.ReadFile(filepath.Join(root, group.Name(), ".cwd")); readErr == nil {
			groupDirectory = strings.TrimSpace(string(body))
		}
		if groupDirectory == "" {
			if decoded, decodeErr := url.PathUnescape(group.Name()); decodeErr == nil {
				groupDirectory = decoded
			}
		}
		if directoryMatches(groupDirectory, expectedDirectory) {
			return Verification{ConversationID: conversationID, StorePath: candidate}, nil
		}
	}
	return Verification{}, notFoundError("Grok", conversationID, expectedDirectory)
}

func verifyOpenCode(ctx context.Context, profile agents.Profile, conversationID, expectedDirectory string) (Verification, error) {
	databasePath, err := openCodeDatabasePath(profile)
	if err != nil {
		return Verification{}, err
	}
	connectionURL := url.URL{Scheme: "file", Path: databasePath}
	query := connectionURL.Query()
	query.Set("mode", "ro")
	connectionURL.RawQuery = query.Encode()
	database, err := sql.Open("sqlite", connectionURL.String())
	if err != nil {
		return Verification{}, providerStoreError("OpenCode", databasePath, err)
	}
	defer database.Close()
	var directory string
	if err := database.QueryRowContext(ctx, `SELECT directory FROM session WHERE id = ?`, conversationID).Scan(&directory); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Verification{}, notFoundError("OpenCode", conversationID, expectedDirectory)
		}
		return Verification{}, providerStoreError("OpenCode", databasePath, err)
	}
	if !directoryMatches(directory, expectedDirectory) {
		return Verification{}, notFoundError("OpenCode", conversationID, expectedDirectory)
	}
	return Verification{ConversationID: conversationID, StorePath: databasePath}, nil
}

func findExactTranscript(ctx context.Context, root string, match func(string, fs.DirEntry) (bool, error), conversationID, provider string) (Verification, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrPermission) {
				return walkErr
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		ok, err := match(path, entry)
		if err != nil {
			return err
		}
		if ok {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return Verification{}, providerStoreError(provider, root, err)
	}
	if found == "" {
		return Verification{}, notFoundError(provider, conversationID, "the Session checkout")
	}
	return Verification{ConversationID: conversationID, StorePath: found}, nil
}

func scanJSONLines(path string, match func(map[string]any) bool) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	reader := io.LimitReader(file, maxTranscriptBytes)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxTranscriptLineBytes)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil && match(record) {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func validateConversationID(value string) error {
	if value == "" {
		return fmt.Errorf("canonical provider conversation ID is required")
	}
	if len(value) > maxConversationIDLength || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") {
		return fmt.Errorf("canonical provider conversation ID %q is unsafe", value)
	}
	return nil
}

func providerHome(profile agents.Profile, environmentName, fallbackName string) (string, error) {
	if configured := strings.TrimSpace(profile.Environment[environmentName]); configured != "" {
		return filepath.Abs(configured)
	}
	if configured := strings.TrimSpace(os.Getenv(environmentName)); configured != "" {
		return filepath.Abs(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, fallbackName), nil
}

func openCodeDatabasePath(profile agents.Profile) (string, error) {
	if configured := firstEnvironment(profile, "OPENCODE_DB"); configured != "" {
		return filepath.Abs(configured)
	}
	if dataHome := firstEnvironment(profile, "XDG_DATA_HOME"); dataHome != "" {
		absolute, err := filepath.Abs(dataHome)
		if err != nil {
			return "", err
		}
		return filepath.Join(absolute, "opencode", "opencode.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode.db"), nil
}

func firstEnvironment(profile agents.Profile, name string) string {
	if value := strings.TrimSpace(profile.Environment[name]); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(name))
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = evaluated
	}
	return filepath.Clean(absolute), nil
}

func directoryMatches(actual, expected string) bool {
	if strings.TrimSpace(actual) == "" {
		return false
	}
	canonical, err := canonicalDirectory(actual)
	return err == nil && canonical == expected
}

func stringMapValue(values map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := values[name].(string); ok {
			return value
		}
	}
	return ""
}

func providerStoreError(provider, path string, err error) error {
	return fmt.Errorf("inspect %s conversation store %q: %w", provider, path, err)
}

func notFoundError(provider, conversationID, workingDirectory string) error {
	return fmt.Errorf("%s conversation %q was not found for %s; refusing to open a different conversation", provider, conversationID, workingDirectory)
}
