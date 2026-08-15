package native

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/agents"
)

func TestBuildResumeUsesExactProviderArgv(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
	}{
		{name: "codex", command: "/opt/codex", args: []string{"--global", "resume", "native-123"}},
		{name: "claude", command: "/opt/claude", args: []string{"--global", "--resume", "native-123"}},
		{name: "gemini", command: "/opt/gemini", args: []string{"--global", "--resume", "native-123"}},
		{name: "grok", command: "/opt/grok", args: []string{"--global", "--resume", "native-123"}},
		{name: "opencode", command: "/opt/opencode", args: []string{"--global", "--session", "native-123"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := agents.Profile{Name: test.name, Provider: "provider", Command: "/opt/" + test.name, Args: []string{"--global"}, Environment: map[string]string{"TOKEN": "secret"}}
			invocation, err := BuildResume(profile, "native-123", "/repo/worktree")
			if err != nil {
				t.Fatal(err)
			}
			if invocation.Command != test.command || !reflect.DeepEqual(invocation.Args, test.args) || invocation.WorkingDirectory != "/repo/worktree" || invocation.Environment["TOKEN"] != "secret" {
				t.Fatalf("invocation = %#v", invocation)
			}
			invocation.Args[0] = "changed"
			invocation.Environment["TOKEN"] = "changed"
			if profile.Args[0] != "--global" || profile.Environment["TOKEN"] != "secret" {
				t.Fatal("BuildResume aliased profile data")
			}
		})
	}
}

func TestBuildResumeRejectsMissingIDAndUnsupportedProfile(t *testing.T) {
	profile := agents.Profile{Name: "codex", Provider: "openai", Command: "codex"}
	if _, err := BuildResume(profile, "", "/repo"); err == nil || !strings.Contains(err.Error(), "ID is required") {
		t.Fatalf("missing ID error = %v", err)
	}
	profile = agents.Profile{Name: "custom", Provider: "custom", Command: "custom-agent"}
	if _, err := BuildResume(profile, "native", "/repo"); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("unsupported profile error = %v", err)
	}
}

func TestBuildResumeMapsLegacyGrokAndACPOpenCodeProfiles(t *testing.T) {
	grok, err := BuildResume(agents.Profile{Name: "grok-build", Provider: "xai", Command: "grok-build"}, "grok-id", "/repo")
	if err != nil || grok.Command != "grok" || !reflect.DeepEqual(grok.Args, []string{"--resume", "grok-id"}) {
		t.Fatalf("legacy Grok invocation = %#v, %v", grok, err)
	}
	opencode, err := BuildResume(agents.Profile{Name: "opencode", Provider: "opencode", Command: "npx", Args: []string{"-y", "opencode-ai@1.18.18", "acp"}}, "open-id", "/repo")
	if err != nil || opencode.Command != "opencode" || !reflect.DeepEqual(opencode.Args, []string{"--session", "open-id"}) {
		t.Fatalf("ACP OpenCode invocation = %#v, %v", opencode, err)
	}
}

func TestExecRunnerLaunchesFakeNativeCLIWithExactContext(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fake-native"))
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "native-record")
	workingDirectory := t.TempDir()
	canonicalWorkingDirectory, err := filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	profile := agents.Profile{
		Name: "codex", Provider: "openai", Command: fixture, Args: []string{"--profile-arg"},
		Environment: map[string]string{"FAKE_NATIVE_RECORD": record, "FAKE_NATIVE_MARKER": "profile-environment"},
	}
	invocation, err := BuildResume(profile, "native-exact", workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := (ExecRunner{}).Run(context.Background(), invocation, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"cwd=" + canonicalWorkingDirectory, "arg=--profile-arg", "arg=resume", "arg=native-exact", "marker=profile-environment",
	} {
		if !strings.Contains(string(body), expected+"\n") {
			t.Errorf("fake native record missing %q:\n%s", expected, body)
		}
	}
	if stdout.String() != "fake native attached\n" || stderr.Len() != 0 {
		t.Fatalf("native output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

type captureRunner struct{ invocation Invocation }

func (r *captureRunner) Run(_ context.Context, invocation Invocation, _ io.Reader, _, _ io.Writer) error {
	r.invocation = invocation
	return nil
}
