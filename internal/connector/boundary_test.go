package connector_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/testkit"
)

func TestLiteralCommandUsesExecutableAndArgvWithoutShellOrEnvCopy(t *testing.T) {
	t.Parallel()
	dir := filepath.Clean(t.TempDir())
	spec := connector.CommandSpec{
		Executable: "/literal/provider connector;echo-not-run",
		Args:       []string{"--flag", "$(opaque-not-run)", "space preserved", "quote'preserved"},
		Dir:        dir,
	}
	cmd, err := connector.Command(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != spec.Executable {
		t.Fatalf("Path = %q, want literal %q", cmd.Path, spec.Executable)
	}
	wantArgs := append([]string{spec.Executable}, spec.Args...)
	if !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", cmd.Args, wantArgs)
	}
	if cmd.Dir != dir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, dir)
	}
	if cmd.Env != nil {
		t.Fatalf("Env copied into command: %#v", cmd.Env)
	}
	for _, arg := range cmd.Args {
		if arg == "-c" {
			t.Fatal("shell -c inserted")
		}
	}
}

func TestLiteralCommandRejectsInvalidFieldsAndErrorsAreContentFree(t *testing.T) {
	t.Parallel()
	if _, err := connector.Command(context.Background(), connector.CommandSpec{Executable: "x", Dir: "relative"}); !errors.Is(err, connector.ErrInvalidCommand) {
		t.Fatalf("relative cwd error = %v", err)
	}
	if _, err := connector.Command(context.Background(), connector.CommandSpec{Executable: "x\x00private"}); !errors.Is(err, connector.ErrInvalidCommand) {
		t.Fatalf("NUL executable error = %v", err)
	}
	canary := "/definitely-missing/PRIVATE-COMMAND-CANARY"
	_, err := (connector.Launcher{}).Start(context.Background(), connector.CommandSpec{Executable: canary})
	if err == nil || !errors.Is(err, connector.ErrLaunchFailed) {
		t.Fatalf("Start() error = %v", err)
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("launch error leaked command: %v", err)
	}
}

func TestMechanicalAuthProbeAndAction(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	auth := connector.CommandAuth{
		Runner:       runner,
		ProbeCommand: connector.CommandSpec{Executable: "provider", Args: []string{"auth", "status"}},
		AuthCommand:  connector.CommandSpec{Executable: "provider", Args: []string{"auth", "login"}},
	}
	if got := auth.Probe(context.Background()); got != connector.AuthConnected {
		t.Fatalf("connected Probe() = %q", got)
	}
	runner.mu.Lock()
	if runner.calls[0].stdout != io.Discard || runner.calls[0].stderr != io.Discard {
		t.Fatal("probe captured provider output")
	}
	runner.mu.Unlock()

	runner.fail = true
	if got := auth.Probe(context.Background()); got != connector.AuthNeedsRepair {
		t.Fatalf("failed Probe() = %q", got)
	}
	runner.fail = false
	if err := auth.Authenticate(context.Background(), connector.AuthIO{}); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	last := runner.calls[len(runner.calls)-1]
	runner.mu.Unlock()
	if !reflect.DeepEqual(last.spec.Args, []string{"auth", "login"}) {
		t.Fatalf("auth argv = %#v", last.spec.Args)
	}
}

func TestMechanicalAuthMissingExecutableIsDisconnected(t *testing.T) {
	t.Parallel()
	auth := connector.CommandAuth{
		ProbeCommand: connector.CommandSpec{Executable: filepath.Join(t.TempDir(), "missing-provider")},
		AuthCommand:  connector.CommandSpec{Executable: filepath.Join(t.TempDir(), "missing-provider")},
	}
	if got := auth.Probe(context.Background()); got != connector.AuthDisconnected {
		t.Fatalf("missing executable Probe() = %q, want disconnected", got)
	}
}

func TestCodexProfileFailsClosedOnVersionOrPortMismatch(t *testing.T) {
	t.Parallel()
	versions := connector.CodexConformance().Exact
	factory := connector.NewCodexSubordinateFactory("/opt/taskai/codex-acp", t.TempDir(), nil)
	manager := fakeManagerFactory{}
	managerResume := fakeManagerResumeFactory{}
	profile := connector.CodexProfile(versions, nil, factory, manager, managerResume, connector.ReplayBinding{})
	want := connector.Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: false}
	if profile.Capabilities != want {
		t.Fatalf("exact capabilities = %+v, want %+v", profile.Capabilities, want)
	}
	replayPort := &scriptedReplayPort{invocations: 1}
	profile = connector.CodexProfile(versions, nil, factory, manager, managerResume, connector.ReplayBinding{
		Provider: replay.ProviderCodex, InstalledVersion: connector.CodexAppServerVersion, Port: replayPort,
	})
	if !profile.Capabilities.Replay {
		t.Fatalf("exact native Replay binding was not advertised: %+v", profile.Capabilities)
	}
	profile = connector.CodexProfile(versions, nil, factory, manager, managerResume, connector.ReplayBinding{
		Provider: replay.ProviderCodex, InstalledVersion: "0.151.0", Port: replayPort,
	})
	if profile.Capabilities.Replay {
		t.Fatalf("mismatched Replay version advertised: %+v", profile.Capabilities)
	}

	mismatched := versions
	mismatched.Connector = "1.6.2"
	profile = connector.CodexProfile(mismatched, nil, factory, manager, managerResume, connector.ReplayBinding{})
	if profile.Capabilities != (connector.Capabilities{}) {
		t.Fatalf("mismatched capabilities = %+v", profile.Capabilities)
	}
	mismatched = versions
	mismatched.ACPProtocol = 2
	profile = connector.CodexProfile(mismatched, nil, factory, manager, managerResume, connector.ReplayBinding{})
	if profile.Capabilities != (connector.Capabilities{}) {
		t.Fatalf("non-v1 ACP capabilities = %+v", profile.Capabilities)
	}

	profile = connector.CodexProfile(versions, nil, nil, manager, managerResume, connector.ReplayBinding{})
	if profile.Capabilities.Subordinate || profile.Capabilities.Replay {
		t.Fatalf("absent production ports advertised: %+v", profile.Capabilities)
	}
}

func TestProviderSubordinateFactoriesPinLiteralCommandsAndConformance(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	geminiKey := connector.GeminiRecordKeyFunc(func(string) (string, error) {
		return "session-record.json", nil
	})
	tests := []struct {
		name       string
		factory    connector.ACPFactory
		executable string
		args       []string
		provider   replay.ProviderID
		versions   connector.Versions
		resume     bool
	}{
		{
			name: "codex", factory: connector.NewCodexSubordinateFactory("/resolved/codex-acp", cwd, nil),
			executable: "/resolved/codex-acp", provider: replay.ProviderCodex,
			versions: connector.CodexConformance().Exact, resume: true,
		},
		{
			name: "claude", factory: connector.NewClaudeSubordinateFactory("/resolved/claude-agent-acp", cwd, "project-key", nil),
			executable: "/resolved/claude-agent-acp", provider: replay.ProviderClaude,
			versions: connector.ClaudeConformance().Exact, resume: true,
		},
		{
			name: "gemini", factory: connector.NewGeminiSubordinateFactory("/resolved/gemini", cwd, "project-hash", geminiKey, nil),
			executable: "/resolved/gemini", args: []string{"--experimental-acp"}, provider: replay.ProviderGemini,
			versions: connector.GeminiConformance().Exact, resume: false,
		},
		{
			name: "grok", factory: connector.NewGrokSubordinateFactory("/resolved/grok", cwd, "workspace-key", nil),
			executable: "/resolved/grok", args: []string{"agent", "stdio"}, provider: replay.ProviderGrok,
			versions: connector.GrokConformance().Exact, resume: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.factory.Command.Executable != test.executable || !reflect.DeepEqual(test.factory.Command.Args, test.args) || test.factory.Command.Dir != "" {
				t.Fatalf("literal command = %#v", test.factory.Command)
			}
			evidence, ok := test.factory.SubordinateConformance()
			if !ok || evidence.Provider != test.provider || evidence.Versions != test.versions || evidence.Resume != test.resume {
				t.Fatalf("factory evidence = %+v, ok=%t", evidence, ok)
			}
		})
	}
}

func TestPinnedProviderVersionTuplesAndCapabilityClaims(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		conformance    connector.Conformance
		versions       connector.Versions
		capabilities   connector.Capabilities
		replayProvider replay.ProviderID
		replayVersion  string
	}{
		{
			name: "codex", conformance: connector.CodexConformance(),
			versions: connector.Versions{
				ACPProtocol: 1, ManagerRuntime: "0.150.1", Connector: "1.1.7",
				ProviderHarness: "0.145.0", ConnectorSDK: "1.3.0",
			},
			capabilities:   connector.Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: true},
			replayProvider: replay.ProviderCodex, replayVersion: "0.150.1",
		},
		{
			name: "claude", conformance: connector.ClaudeConformance(),
			versions: connector.Versions{
				ACPProtocol: 1, Connector: "0.64.0", ProviderHarness: "2.1.226", ConnectorSDK: "0.3.220",
			},
			capabilities:   connector.Capabilities{Subordinate: true, Resume: true, Replay: true},
			replayProvider: replay.ProviderClaude, replayVersion: "2.1.226",
		},
		{
			name: "gemini", conformance: connector.GeminiConformance(),
			versions: connector.Versions{
				ACPProtocol: 1, Connector: "0.25.0-preview.0",
				ProviderHarness: "0.25.0-preview.0", ConnectorSDK: "0.11.0",
			},
			capabilities:   connector.Capabilities{Subordinate: true, Replay: true},
			replayProvider: replay.ProviderGemini, replayVersion: "0.25.0-preview.0",
		},
		{
			name: "grok", conformance: connector.GrokConformance(),
			versions: connector.Versions{
				ACPProtocol: 1, Connector: "1.0.5", ProviderHarness: "1.0.5",
			},
			capabilities:   connector.Capabilities{Subordinate: true, Resume: true, Replay: true},
			replayProvider: replay.ProviderGrok, replayVersion: "1.0.5",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.conformance.Exact != test.versions || test.conformance.Proven != test.capabilities ||
				test.conformance.ReplayProvider != test.replayProvider || test.conformance.ReplayVersion != test.replayVersion {
				t.Fatalf("conformance = %+v", test.conformance)
			}
		})
	}
}

func TestSubordinateOnlyProfilesIntersectVersionsResumeAndReplay(t *testing.T) {
	t.Parallel()
	type profileFunc func(connector.Versions, connector.AuthPort, connector.ChildFactory, connector.ReplayBinding) connector.Profile
	cwd := t.TempDir()
	replayPort := &scriptedReplayPort{invocations: 1}
	tests := []struct {
		name        string
		provider    replay.ProviderID
		conformance connector.Conformance
		factory     connector.ACPFactory
		profile     profileFunc
		resume      bool
	}{
		{
			name: "claude", provider: replay.ProviderClaude, conformance: connector.ClaudeConformance(),
			factory: connector.NewClaudeSubordinateFactory("/resolved/claude-agent-acp", cwd, "project-key", nil),
			profile: connector.ClaudeProfile, resume: true,
		},
		{
			name: "gemini", provider: replay.ProviderGemini, conformance: connector.GeminiConformance(),
			factory: connector.NewGeminiSubordinateFactory(
				"/resolved/gemini", cwd, "project-hash",
				func(string) (string, error) { return "session-record.json", nil }, nil,
			),
			profile: connector.GeminiProfile, resume: false,
		},
		{
			name: "grok", provider: replay.ProviderGrok, conformance: connector.GrokConformance(),
			factory: connector.NewGrokSubordinateFactory("/resolved/grok", cwd, "workspace-key", nil),
			profile: connector.GrokProfile, resume: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := connector.ReplayBinding{
				Provider: test.provider, InstalledVersion: test.conformance.ReplayVersion, Port: replayPort,
			}
			profile := test.profile(test.conformance.Exact, nil, test.factory, binding)
			want := connector.Capabilities{Subordinate: true, Resume: test.resume, Replay: true}
			if profile.Capabilities != want {
				t.Fatalf("capabilities = %+v, want %+v", profile.Capabilities, want)
			}
			if profile.Manager != nil || profile.ManagerResume != nil || profile.Capabilities.Manager {
				t.Fatalf("subordinate-only provider advertised Manager: %+v", profile)
			}
			if (profile.ChildResume != nil) != test.resume {
				t.Fatalf("Resume port present=%t, want %t", profile.ChildResume != nil, test.resume)
			}
			if err := connector.NewRegistry().Register(profile); err != nil {
				t.Fatal(err)
			}

			withoutReplay := test.profile(test.conformance.Exact, nil, test.factory, connector.ReplayBinding{})
			if withoutReplay.Capabilities.Replay || !withoutReplay.Capabilities.Subordinate || withoutReplay.Capabilities.Resume != test.resume {
				t.Fatalf("missing Replay port capabilities = %+v", withoutReplay.Capabilities)
			}
			wrongReplay := binding
			wrongReplay.InstalledVersion += "+mismatch"
			withWrongReplay := test.profile(test.conformance.Exact, nil, test.factory, wrongReplay)
			if withWrongReplay.Capabilities.Replay || !withWrongReplay.Capabilities.Subordinate || withWrongReplay.Capabilities.Resume != test.resume {
				t.Fatalf("mismatched Replay binding capabilities = %+v", withWrongReplay.Capabilities)
			}
			mismatch := test.conformance.Exact
			mismatch.Connector += "+mismatch"
			if got := test.profile(mismatch, nil, test.factory, binding).Capabilities; got != (connector.Capabilities{}) {
				t.Fatalf("version mismatch capabilities = %+v", got)
			}
			mutated := test.factory
			mutated.Command.Args = append(mutated.Command.Args, "--unconformed")
			if got := test.profile(test.conformance.Exact, nil, mutated, connector.ReplayBinding{}).Capabilities; got != (connector.Capabilities{}) {
				t.Fatalf("mutated command capabilities = %+v", got)
			}
			mutated = test.factory
			mutated.Command.Executable += "-replacement"
			if got := test.profile(test.conformance.Exact, nil, mutated, connector.ReplayBinding{}).Capabilities; got != (connector.Capabilities{}) {
				t.Fatalf("mutated executable capabilities = %+v", got)
			}
			mutated = test.factory
			mutated.CWD = t.TempDir()
			if got := test.profile(test.conformance.Exact, nil, mutated, connector.ReplayBinding{}).Capabilities; got != (connector.Capabilities{}) {
				t.Fatalf("mutated CWD capabilities = %+v", got)
			}
		})
	}
}

func TestGeminiProfileRequiresExactRecordKeyResolver(t *testing.T) {
	t.Parallel()
	factory := connector.NewGeminiSubordinateFactory(
		"/resolved/gemini", t.TempDir(), "project-hash", nil, nil,
	)
	profile := connector.GeminiProfile(
		connector.GeminiConformance().Exact, nil, factory, connector.ReplayBinding{},
	)
	if profile.Capabilities != (connector.Capabilities{}) || profile.Children != nil || profile.ChildResume != nil {
		t.Fatalf("unresolved Gemini native reference advertised capabilities = %+v", profile.Capabilities)
	}
}

func TestProviderProfileRejectsForeignFactoryEvidence(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	foreign := connector.NewGrokSubordinateFactory("/resolved/grok", cwd, "workspace-key", nil)
	profile := connector.ClaudeProfile(connector.ClaudeConformance().Exact, nil, foreign, connector.ReplayBinding{})
	if profile.Capabilities != (connector.Capabilities{}) || profile.Children != nil || profile.ChildResume != nil {
		t.Fatalf("foreign factory capabilities = %+v", profile.Capabilities)
	}
}

func TestManagerStartFactoryDoesNotAdvertiseExactResumeWithoutResumeFactory(t *testing.T) {
	t.Parallel()
	versions := connector.CodexConformance().Exact
	profile := connector.CodexProfile(
		versions, nil, nil, fakeManagerFactory{}, nil, connector.ReplayBinding{},
	)
	if !profile.Capabilities.Manager || profile.Capabilities.Resume {
		t.Fatalf("Start-only Manager capabilities = %+v", profile.Capabilities)
	}
	if profile.Manager == nil || profile.ManagerResume != nil {
		t.Fatalf("Start/Resume factories = %#v / %#v", profile.Manager, profile.ManagerResume)
	}
	if err := connector.NewRegistry().Register(profile); err != nil {
		t.Fatalf("register Start-only Manager: %v", err)
	}

	invalid := connector.Profile{
		ID: "resume-without-manager", DisplayName: "Invalid",
		ManagerResume: fakeManagerResumeFactory{},
	}
	if err := connector.NewRegistry().Register(invalid); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("orphan Manager Resume factory error = %v", err)
	}
}

func TestRegistryRejectsCapabilityAddedAfterConformance(t *testing.T) {
	t.Parallel()
	versions := connector.CodexConformance().Exact
	profile := connector.CodexProfile(
		versions,
		nil,
		connector.NewCodexSubordinateFactory("/opt/taskai/codex-acp", t.TempDir(), nil),
		fakeManagerFactory{},
		fakeManagerResumeFactory{},
		connector.ReplayBinding{},
	)
	profile.Capabilities.Replay = true
	if err := connector.NewRegistry().Register(profile); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("hard-coded Replay claim error = %v", err)
	}
}

func TestConformedSessionPathsDoNotInvokeAuthentication(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{}
	auth := connector.CommandAuth{
		Runner:       runner,
		ProbeCommand: connector.CommandSpec{Executable: "provider", Args: []string{"auth", "status"}},
		AuthCommand:  connector.CommandSpec{Executable: "provider", Args: []string{"auth", "login"}},
	}
	profile := connector.CodexProfile(
		connector.CodexConformance().Exact,
		auth,
		connector.NewCodexSubordinateFactory("/opt/taskai/codex-acp", t.TempDir(), nil),
		fakeManagerFactory{},
		fakeManagerResumeFactory{},
		connector.ReplayBinding{},
	)
	registry := connector.NewRegistry()
	if err := registry.Register(profile); err != nil {
		t.Fatal(err)
	}
	registered, err := registry.Profile("codex")
	if err != nil {
		t.Fatal(err)
	}
	events := &testkit.EventRecorder{}
	if _, err := registered.Manager.Start(context.Background(), connector.ManagerStartRequest{
		ParentSessionID: "manager-start", CWD: t.TempDir(), Registry: registry, Events: events,
	}); err != nil {
		t.Fatal(err)
	}
	managerReference, _ := replay.NewCodexReference("manager")
	if _, err := registered.ManagerResume.Resume(context.Background(), connector.ManagerResumeRequest{
		ParentSessionID: "manager-resume", NativeReference: managerReference, Registry: registry, Events: events,
	}); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 0 {
		t.Fatalf("normal Session paths invoked auth %d times", len(runner.calls))
	}
}

func TestRegistryRejectsClaimsWithoutPortsAndSortsProfiles(t *testing.T) {
	t.Parallel()
	registry := connector.NewRegistry()
	bad := connector.Profile{
		ID:           "bad",
		DisplayName:  "Bad",
		Capabilities: connector.Capabilities{Manager: true},
	}
	if err := registry.Register(bad); !errors.Is(err, connector.ErrProviderUnsupported) {
		t.Fatalf("Register(bad) error = %v", err)
	}
	for _, id := range []string{"zeta", "alpha"} {
		if err := registry.Register(connector.Profile{ID: id, DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	profiles := registry.Profiles()
	if len(profiles) != 2 || profiles[0].ID != "alpha" || profiles[1].ID != "zeta" {
		t.Fatalf("Profiles() order = %#v", profiles)
	}
}

type runnerCall struct {
	spec           connector.CommandSpec
	stdin          io.Reader
	stdout, stderr io.Writer
}

type recordingRunner struct {
	mu    sync.Mutex
	calls []runnerCall
	fail  bool
}

func (r *recordingRunner) Run(_ context.Context, spec connector.CommandSpec, stdin io.Reader, stdout, stderr io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, runnerCall{spec: spec, stdin: stdin, stdout: stdout, stderr: stderr})
	if r.fail {
		return connector.ErrProcessFailed
	}
	return nil
}

type fakeManagerFactory struct{}

func (fakeManagerFactory) Start(context.Context, connector.ManagerStartRequest) (connector.NativeManagerSession, error) {
	reference, _ := replay.NewCodexReference("manager")
	return connector.NativeManagerSession{NativeReference: reference}, nil
}

type fakeManagerResumeFactory struct{}

func (fakeManagerResumeFactory) Resume(context.Context, connector.ManagerResumeRequest) (connector.NativeManagerSession, error) {
	reference, _ := replay.NewCodexReference("manager")
	return connector.NativeManagerSession{NativeReference: reference}, nil
}
