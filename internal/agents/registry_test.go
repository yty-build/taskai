package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadEmbeddedProfiles(t *testing.T) {
	registry, err := Load(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, profile := range registry.List() {
		names = append(names, profile.Name)
	}
	want := []string{"claude", "codex", "gemini", "grok-build", "opencode"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("profiles = %v, want %v", names, want)
	}
	opencode, err := registry.Get("opencode")
	if err != nil {
		t.Fatal(err)
	}
	wantOpenCodeArgv := []string{"npx", "-y", "opencode-ai@1.18.18", "acp"}
	if got := opencode.Argv(); !reflect.DeepEqual(got, wantOpenCodeArgv) {
		t.Fatalf("opencode Argv() = %q, want %q", got, wantOpenCodeArgv)
	}
	gemini, err := registry.Get("gemini")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gemini.Argv(), []string{"gemini"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini Argv() = %q, want friendly profile %q", got, want)
	}
}

func TestUserProfilesOverlayDefaultsAndAddCustomCommands(t *testing.T) {
	directory := t.TempDir()
	writeProfile(t, directory, "codex.yaml", `
name: codex
provider: test-openai
command: /opt/acp/codex
args: [--stdio]
environment:
  SAFE_VALUE: literal
`)
	writeProfile(t, directory, "video.yaml", `
name: video-agent
provider: custom
command: /opt/My ACP/video-agent
args:
  - --acp
  - $(touch /tmp/never-interpolate)
`)
	registry, err := Load(directory)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := registry.Get("codex")
	if err != nil || codex.Provider != "test-openai" {
		t.Fatalf("codex = %#v, %v", codex, err)
	}
	custom, err := registry.Get("video-agent")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/My ACP/video-agent", "--acp", "$(touch /tmp/never-interpolate)"}
	if got := custom.Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Argv() = %q, want %q", got, want)
	}
}

func TestLoadRejectsUnknownFieldsAndInvalidProfiles(t *testing.T) {
	tests := map[string]string{
		"unknown field": "name: x\nprovider: p\ncommand: c\nrepo: /tmp\n",
		"invalid name":  "name: 'bad name'\nprovider: p\ncommand: c\n",
		"empty command": "name: x\nprovider: p\ncommand: ''\n",
		"invalid env":   "name: x\nprovider: p\ncommand: c\nenvironment:\n  'BAD-NAME': x\n",
		"two documents": "name: x\nprovider: p\ncommand: c\n---\nname: y\nprovider: p\ncommand: c\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			writeProfile(t, directory, "bad.yaml", body)
			if _, err := Load(directory); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestLoadRejectsDuplicateUserNames(t *testing.T) {
	directory := t.TempDir()
	body := "name: custom\nprovider: test\ncommand: custom\n"
	writeProfile(t, directory, "one.yaml", body)
	writeProfile(t, directory, "two.yaml", body)
	_, err := Load(directory)
	if err == nil || !strings.Contains(err.Error(), "defined by both") {
		t.Fatalf("Load() error = %v", err)
	}
}

func writeProfile(t *testing.T, directory, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
