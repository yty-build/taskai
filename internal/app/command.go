// Package app composes the TaskAI command without owning feature behavior.
package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

const (
	hiddenWorkerArgument = "--taskai-runtime-worker"

	helpText = `Usage:
  taskai
  taskai --help
  taskai --version

Options:
  -h, --help  Show this help.
  --version   Show the TaskAI version.`
)

// IO is the complete process stream boundary. Interactive provider
// authentication receives these streams only while Bubble Tea has released
// terminal ownership; the persistent runtime worker never retains them.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type commandExecutor interface {
	View(context.Context, IO) error
	Work(context.Context) error
}

// Run executes TaskAI and returns a process exit status. Informational
// commands are resolved before the system executor can resolve or create the
// state directory, inspect a provider root, open IPC, or launch any process.
func Run(ctx context.Context, args []string, streams IO) int {
	return runCommand(ctx, args, streams, systemCommand{})
}

func runCommand(ctx context.Context, args []string, streams IO, executor commandExecutor) int {
	streams = normalizeIO(streams)
	if ctx == nil {
		ctx = context.Background()
	}

	if len(args) == 1 {
		switch args[0] {
		case "-h", "--help":
			fmt.Fprintln(streams.Out, helpText)
			return 0
		case "--version":
			fmt.Fprintf(streams.Out, "taskai %s\n", buildinfo.Version)
			return 0
		case hiddenWorkerArgument:
			if err := executor.Work(ctx); err != nil {
				writeCommandError(streams.Err, err)
				return 1
			}
			return 0
		}
	}

	if len(args) != 0 {
		fmt.Fprintln(streams.Err, "taskai: invalid arguments; use --help")
		return 2
	}
	if err := executor.View(ctx, streams); err != nil {
		writeCommandError(streams.Err, err)
		return 1
	}
	return 0
}

func normalizeIO(value IO) IO {
	if value.In == nil {
		value.In = strings.NewReader("")
	}
	if value.Out == nil {
		value.Out = io.Discard
	}
	if value.Err == nil {
		value.Err = io.Discard
	}
	return value
}

func writeCommandError(destination io.Writer, err error) {
	safe := terminalError(err)
	code := "transport_failed"
	if coded, ok := safe.(interface{ SafeCode() string }); ok {
		code = coded.SafeCode()
	}
	fmt.Fprintf(destination, "taskai: %s\n", code)
}
