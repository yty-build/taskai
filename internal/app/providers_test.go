package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/runtime"
)

func TestProductionProvidersUseLiteralPinnedCommandsAndTruthfulCapabilities(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "absent-claude-root"))
	t.Setenv("PATH", "")

	bundle, err := newProductionProviders(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.catalog) != 4 || len(bundle.registry.Profiles()) != 4 {
		t.Fatalf("bundle sizes: catalog=%d profiles=%d", len(bundle.catalog), len(bundle.registry.Profiles()))
	}

	codex, err := bundle.registry.Profile("codex")
	if err != nil {
		t.Fatal(err)
	}
	if codex.Capabilities != (connector.Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: true}) {
		t.Fatalf("Codex capabilities = %#v", codex.Capabilities)
	}
	codexChild, ok := codex.Children.(connector.ACPFactory)
	if !ok || codexChild.Command.Executable != "codex-acp" || len(codexChild.Command.Args) != 0 || codexChild.CWD != root {
		t.Fatalf("Codex child factory = %#v", codex.Children)
	}
	codexManager, ok := codex.Manager.(connector.CodexManager)
	if !ok {
		t.Fatalf("Codex Manager port = %T", codex.Manager)
	}
	launcher, ok := codexManager.Apps.(connector.CodexAppServerLauncher)
	if !ok || launcher.Command.Executable != "codex" || !reflect.DeepEqual(launcher.Command.Args, []string{"app-server"}) {
		t.Fatalf("Codex manager launcher = %#v", codexManager.Apps)
	}
	codexAuth, ok := codex.Auth.(pinnedProviderAuth)
	if !ok || codexAuth.probeCommand().Executable != "codex" ||
		!reflect.DeepEqual(codexAuth.probeCommand().Args, []string{"login", "status"}) ||
		!reflect.DeepEqual(codexAuth.authenticationCommand().Args, []string{"login"}) {
		t.Fatalf("Codex auth = %#v", codex.Auth)
	}

	claude, err := bundle.registry.Profile("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if claude.Capabilities != (connector.Capabilities{Subordinate: true, Resume: true}) {
		t.Fatalf("Claude capabilities with absent native root = %#v", claude.Capabilities)
	}
	claudeChild, ok := claude.Children.(connector.ACPFactory)
	if !ok || claudeChild.Command.Executable != "claude-agent-acp" || len(claudeChild.Command.Args) != 0 || claudeChild.CWD != root {
		t.Fatalf("Claude child factory = %#v", claude.Children)
	}
	claudeAuth, ok := claude.Auth.(pinnedProviderAuth)
	if !ok || claudeAuth.probeCommand().Executable != "claude" ||
		!reflect.DeepEqual(claudeAuth.probeCommand().Args, []string{"auth", "status"}) ||
		!reflect.DeepEqual(claudeAuth.authenticationCommand().Args, []string{"auth", "login"}) {
		t.Fatalf("Claude auth = %#v", claude.Auth)
	}

	for _, id := range []string{"gemini-cli", "grok-build"} {
		profile, profileErr := bundle.registry.Profile(id)
		if profileErr != nil {
			t.Fatal(profileErr)
		}
		if profile.Capabilities != (connector.Capabilities{}) || profile.Children != nil || profile.Replay.Port != nil {
			t.Fatalf("%s did not fail closed: %#v", id, profile)
		}
	}
	grok, err := bundle.registry.Profile("grok-build")
	if err != nil {
		t.Fatal(err)
	}
	grokAuth, ok := grok.Auth.(pinnedProviderAuth)
	if !ok || grokAuth.probeCommand().Executable != "grok" ||
		!reflect.DeepEqual(grokAuth.probeCommand().Args, []string{"models"}) ||
		!reflect.DeepEqual(grokAuth.authenticationCommand().Args, []string{"login"}) {
		t.Fatalf("Grok auth = %#v", grok.Auth)
	}

	wantCatalog := map[string][]string{
		"codex":       {"codex", "app-server"},
		"claude-code": {"claude-agent-acp"},
		"gemini-cli":  {"gemini", "--experimental-acp"},
		"grok-build":  {"grok", "agent", "stdio"},
	}
	for _, spec := range bundle.catalog {
		if !reflect.DeepEqual(spec.Launch.Argv, wantCatalog[string(spec.ID)]) {
			t.Fatalf("catalog[%s] = %#v", spec.ID, spec.Launch.Argv)
		}
	}
}

func TestClaudeReplayAdvertisesOnlyForSafeExistingRootWithoutReadingContent(t *testing.T) {
	root := t.TempDir()
	providerRoot := filepath.Join(root, "claude-root")
	if err := os.Mkdir(providerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	canaryPath := filepath.Join(providerRoot, "provider-content-canary")
	canary := []byte("provider-owned-content-must-not-be-read")
	if err := os.WriteFile(canaryPath, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", providerRoot)

	bundle, err := newProductionProviders(root)
	if err != nil {
		t.Fatal(err)
	}
	claude, err := bundle.registry.Profile("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if claude.Capabilities != (connector.Capabilities{Subordinate: true, Resume: true, Replay: true}) {
		t.Fatalf("Claude capabilities = %#v", claude.Capabilities)
	}
	after, err := os.Stat(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(canaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, canary) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("provider content changed during metadata-only composition")
	}

	unsafeRoot := filepath.Join(root, "relative-root")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Base(unsafeRoot))
	bundle, err = newProductionProviders(root)
	if err != nil {
		t.Fatal(err)
	}
	claude, _ = bundle.registry.Profile("claude-code")
	if claude.Capabilities.Replay {
		t.Fatal("Claude Replay advertised for unsafe relative root")
	}

	realRoot := filepath.Join(root, "real-claude-root")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(root, "linked-claude-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", linkedRoot)
	bundle, err = newProductionProviders(root)
	if err != nil {
		t.Fatal(err)
	}
	claude, _ = bundle.registry.Profile("claude-code")
	if claude.Capabilities.Replay {
		t.Fatal("Claude Replay advertised for a symlinked provider root")
	}
}

func TestClaudeProjectKeyExactTransformAndBounds(t *testing.T) {
	got, err := claudeProjectKey("/Users/example/Project One/.git")
	if err != nil {
		t.Fatal(err)
	}
	if got != "-Users-example-Project-One--git" {
		t.Fatalf("project key = %q", got)
	}
	if _, err := claudeProjectKey("relative/project"); !errors.Is(err, runtime.ErrInvalidRequest) {
		t.Fatalf("relative project error = %v", err)
	}
	long := "/" + strings.Repeat("a", maxNativeScopeComponentBytes)
	if _, err := claudeProjectKey(long); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("overlong project key error = %v", err)
	}
	if _, err := claudeProjectKey("/tmp/non-bmp-😀"); !errors.Is(err, runtime.ErrUnsupported) {
		t.Fatalf("non-BMP project key error = %v", err)
	}
}

func TestClaudeScopedCapabilitiesFailClosedForNonBMPCWD(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	bundle, err := newProductionProviders("/tmp/non-bmp-😀")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := bundle.registry.Profile("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if claude.Children != nil || claude.Capabilities.Subordinate || claude.Capabilities.Resume || claude.Capabilities.Replay {
		t.Fatalf("Claude scoped capability escaped non-BMP fail-close: %#v", claude)
	}
}

type historyFactory struct {
	created int
	app     *historyApp
}

func (factory *historyFactory) New(_ context.Context, handler connector.DynamicToolHandler) (connector.CodexAppServer, error) {
	if handler == nil {
		return nil, errors.New("missing rejecting handler")
	}
	factory.created++
	return factory.app, nil
}

type historyApp struct {
	closed  int
	reads   int
	thread  string
	payload json.RawMessage
}

func (*historyApp) Version() string                 { return connector.CodexAppServerVersion }
func (*historyApp) DelegateHandlerRegistered() bool { return true }
func (*historyApp) ThreadStart(context.Context, connector.ThreadStartParams) (connector.CodexThreadSession, error) {
	return connector.CodexThreadSession{}, connector.ErrProviderUnsupported
}
func (*historyApp) ThreadResume(context.Context, connector.ThreadResumeParams) (connector.CodexThreadSession, error) {
	return connector.CodexThreadSession{}, connector.ErrProviderUnsupported
}
func (*historyApp) ConnectManager(replay.OpaqueRef) (connector.ManagerConnection, error) {
	return nil, connector.ErrProviderUnsupported
}
func (app *historyApp) ThreadRead(_ context.Context, thread string) (json.RawMessage, error) {
	app.reads++
	app.thread = thread
	return append(json.RawMessage(nil), app.payload...), nil
}
func (app *historyApp) Close() error { app.closed++; return nil }

func TestCodexHistoryReaderIsLazyExactAndClosesPeer(t *testing.T) {
	app := &historyApp{payload: json.RawMessage(`{"id":"thread-a","turns":[]}`)}
	factory := &historyFactory{app: app}
	reader := codexHistoryReader{apps: factory}
	if factory.created != 0 || app.reads != 0 {
		t.Fatal("history peer contacted during construction")
	}
	raw, err := reader.ThreadRead(t.Context(), "thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if factory.created != 1 || app.reads != 1 || app.thread != "thread-a" || app.closed != 1 || !bytes.Equal(raw, app.payload) {
		t.Fatalf("history trace: created=%d reads=%d thread=%q closed=%d raw=%s", factory.created, app.reads, app.thread, app.closed, raw)
	}
}

func TestCloneProviderSpecsDefendsLiteralVectors(t *testing.T) {
	values := []runtime.ProviderSpec{providerSpec(replay.ProviderCodex, []string{codexExecutable, "app-server"})}
	cloned := cloneProviderSpecs(values)
	cloned[0].Launch.Argv[0] = "mutated"
	if values[0].Launch.Argv[0] != "codex" {
		t.Fatal("provider launch vector aliased")
	}
}
