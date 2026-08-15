package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	info := buildinfo.Info{Version: "1.2.3", Commit: "abc", Date: "today"}
	code := Run(context.Background(), []string{"version"}, &stdout, &stderr, info)
	if code != ExitOK || stdout.String() != "agent 1.2.3 (commit abc, built today)\n" {
		t.Fatalf("code = %d, stdout = %q", code, stdout.String())
	}
}

func TestRunUnknownCommandHasStableError(t *testing.T) {
	t.Setenv("AGENT_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"wat"}, &stdout, &stderr, buildinfo.Info{})
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if got, want := stderr.String(), "agent: unknown command \"wat\"; run 'agent help' for usage\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}
