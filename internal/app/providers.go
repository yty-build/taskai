package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/settings"
)

const (
	maxNativeScopeComponentBytes = 255
	codexExecutable              = "codex"
	codexChildExecutable         = "codex-acp"
	claudeChildExecutable        = "claude-agent-acp"
	geminiExecutable             = "gemini"
	grokExecutable               = "grok"
)

type productionProviders struct {
	registry *connector.Registry
	catalog  []runtime.ProviderSpec
}

// newProductionProviders builds immutable launch authority without probing,
// authenticating, launching, enumerating, or reading provider content. Claude
// Replay availability performs one bounded Lstat of its configured root so an
// absent, non-directory, or final symlink root fails closed; no Session name or
// content is accessed. Capability flags otherwise come only from the
// connector's pinned conformance intersection.
func newProductionProviders(projectCWD string) (productionProviders, error) {
	if projectCWD == "" || !filepath.IsAbs(projectCWD) || !utf8.ValidString(projectCWD) ||
		strings.IndexByte(projectCWD, 0) >= 0 {
		return productionProviders{}, runtime.ErrInvalidRequest
	}

	registry := connector.NewRegistry()
	rejectingHandler := acp.HandlerFuncs{}

	apps := connector.CodexAppServerLauncher{
		Command: connector.CommandSpec{
			Executable: codexExecutable, Args: []string{"app-server"},
		},
		InstalledVersion: connector.CodexAppServerVersion,
	}
	replayRoots := make(map[replay.ProviderID]string)
	if root := claudeConfigRoot(); root != "" {
		replayRoots[replay.ProviderClaude] = root
	}
	providerReplay, err := replay.NewService(replay.Dependencies{
		Codex: codexHistoryReader{apps: apps}, Roots: replayRoots,
	}, replay.Limits{})
	if err != nil {
		return productionProviders{}, runtime.ErrUnsupported
	}
	codexAuth := pinnedProviderAuth{kind: pinnedAuthCodex}
	codexChildren := connector.NewCodexSubordinateFactory(codexChildExecutable, projectCWD, rejectingHandler)
	codexManager := connector.CodexManager{Apps: apps}
	codex := connector.CodexProfile(
		connector.CodexConformance().Exact,
		codexAuth,
		codexChildren,
		codexManager,
		codexManager,
		connector.ReplayBinding{
			Provider: replay.ProviderCodex, InstalledVersion: connector.CodexAppServerVersion, Port: providerReplay,
		},
	)
	if err := registry.Register(codex); err != nil {
		return productionProviders{}, runtime.ErrUnsupported
	}

	claudeAuth := pinnedProviderAuth{kind: pinnedAuthClaude}
	var claudeChildren connector.ChildFactory
	var claudeReplay connector.ReplayBinding
	if projectKey, keyErr := claudeProjectKey(projectCWD); keyErr == nil {
		factory := connector.NewClaudeSubordinateFactory(claudeChildExecutable, projectCWD, projectKey, rejectingHandler)
		claudeChildren = factory
		claudeReplay = connector.ReplayBinding{
			Provider: replay.ProviderClaude, InstalledVersion: connector.ClaudeHarnessVersion, Port: providerReplay,
		}
	}
	claude := connector.ClaudeProfile(
		connector.ClaudeConformance().Exact, claudeAuth, claudeChildren, claudeReplay,
	)
	if err := registry.Register(claude); err != nil {
		return productionProviders{}, runtime.ErrUnsupported
	}

	// Gemini 0.25's native filename includes a minute timestamp that ACP does
	// not return, so an exact record key cannot be derived without forbidden
	// enumeration. Grok 1.0.5's exact cwd encoding is likewise not conformed in
	// this build. Keep both catalog rows and literal commands, but register no
	// launch port or capability rather than guessing a native scope.
	gemini := connector.GeminiProfile(
		connector.GeminiConformance().Exact, nil, nil, connector.ReplayBinding{},
	)
	if err := registry.Register(gemini); err != nil {
		return productionProviders{}, runtime.ErrUnsupported
	}
	grok := connector.GrokProfile(
		connector.GrokConformance().Exact, nil, nil, connector.ReplayBinding{},
	)
	if err := registry.Register(grok); err != nil {
		return productionProviders{}, runtime.ErrUnsupported
	}

	catalog := []runtime.ProviderSpec{
		providerSpec(replay.ProviderCodex, []string{codexExecutable, "app-server"}),
		providerSpec(replay.ProviderClaude, []string{claudeChildExecutable}),
		providerSpec(replay.ProviderGemini, []string{geminiExecutable, "--experimental-acp"}),
		providerSpec(replay.ProviderGrok, []string{grokExecutable, "agent", "stdio"}),
	}
	return productionProviders{registry: registry, catalog: catalog}, nil
}

func providerSpec(id replay.ProviderID, command []string) runtime.ProviderSpec {
	return runtime.ProviderSpec{
		ID:     settings.ProviderID(id),
		Launch: settings.LaunchConfig{Argv: append([]string(nil), command...)},
	}
}

// claudeProjectKey implements Claude Code 2.1.226's project-scope transform:
// preserve ASCII alphanumerics and replace every other path character with a
// hyphen. It never reads or enumerates Claude's configuration directory.
func claudeProjectKey(projectCWD string) (string, error) {
	if projectCWD == "" || !filepath.IsAbs(projectCWD) || !utf8.ValidString(projectCWD) ||
		strings.IndexByte(projectCWD, 0) >= 0 {
		return "", runtime.ErrInvalidRequest
	}
	var builder strings.Builder
	builder.Grow(len(projectCWD))
	for _, char := range projectCWD {
		// Claude 2.1.226's JavaScript transform is proven only where one Go
		// rune corresponds to one UTF-16 code unit. Astral characters would
		// require two replacements; fail the whole scoped capability closed.
		if char > 0xffff {
			return "", runtime.ErrUnsupported
		}
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
		if builder.Len() > maxNativeScopeComponentBytes {
			return "", runtime.ErrUnsupported
		}
	}
	result := builder.String()
	if result == "" || result == "." || result == ".." || strings.ContainsAny(result, `/\\`) {
		return "", runtime.ErrUnsupported
	}
	return result, nil
}

// claudeConfigRoot resolves only Claude's documented configuration root. The
// returned directory remains process-local composition data: it is never put
// in TaskAI Settings or metadata. Capability construction performs only the
// pinned root's bounded final-entry check; Replay opens and reads one exact
// referenced Session only when Replay is explicitly invoked.
func claudeConfigRoot() string {
	if configured := os.Getenv("CLAUDE_CONFIG_DIR"); configured != "" {
		if !filepath.IsAbs(configured) || strings.IndexByte(configured, 0) >= 0 {
			return ""
		}
		return filepath.Clean(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) || strings.IndexByte(home, 0) >= 0 {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// codexHistoryReader launches an isolated, pinned App Server only when an
// explicit Replay call asks for one exact thread. Construction and capability
// listing remain process- and provider-state-free.
type codexHistoryReader struct {
	apps connector.CodexAppServerFactory
}

func (reader codexHistoryReader) ThreadRead(ctx context.Context, threadID string) (json.RawMessage, error) {
	if reader.apps == nil || threadID == "" {
		return nil, connector.ErrProviderUnsupported
	}
	app, err := reader.apps.New(ctx, rejectingDynamicTool{})
	if err != nil {
		return nil, err
	}
	defer app.Close()
	history, ok := app.(replay.CodexThreadReader)
	if !ok {
		return nil, connector.ErrProviderUnsupported
	}
	return history.ThreadRead(ctx, threadID)
}

type rejectingDynamicTool struct{}

func (rejectingDynamicTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, connector.ErrProviderUnsupported
}

var _ replay.CodexThreadReader = codexHistoryReader{}

func cloneProviderSpecs(values []runtime.ProviderSpec) []runtime.ProviderSpec {
	result := make([]runtime.ProviderSpec, len(values))
	for index, value := range values {
		result[index] = runtime.ProviderSpec{ID: value.ID, Launch: value.Launch.Clone()}
	}
	return result
}

func providerBuildError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return runtime.ErrCancelled
	}
	return runtime.ErrUnsupported
}
