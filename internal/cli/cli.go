package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
	"github.com/yty-build/acp-gateway/internal/config"
)

const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

const usage = `Agent Task Console controls persistent agent Sessions grouped under Tasks.

Usage:
  agent <command> [arguments]

Commands:
  start       Create a Task and its first Session
  task        Create, inspect, update, or attach to a Task
  tasks       List Tasks
  session     Start, inspect, control, or attach to a Session
  agents      List configured agent profiles
  serve       Start the loopback-only local web UI
  version     Print build version
  help        Show this help

Environment:
  AGENT_HOME  State directory (default: ~/.agent)
`

// Run executes the command-line application and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	_ = ctx
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return ExitOK
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(stdout, info.String())
		return ExitOK
	}

	if _, err := config.ResolveHome(); err != nil {
		return fail(stderr, ExitFailure, err)
	}

	return fail(stderr, ExitUsage, fmt.Errorf("unknown command %q; run 'agent help' for usage", strings.Join(args, " ")))
}

func fail(stderr io.Writer, code int, err error) int {
	fmt.Fprintf(stderr, "agent: %s\n", err)
	return code
}
