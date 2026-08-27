// Package app composes the TaskAI command without owning feature behavior.
package app

import (
	"fmt"
	"io"

	"github.com/yty-build/acp-gateway/internal/buildinfo"
)

const helpText = `Usage:
  taskai --help
  taskai --version

Options:
  -h, --help  Show this help.
  --version   Show the TaskAI version.`

// Run executes the currently implemented command surface and returns a process
// exit status. The M01 baseline deliberately does not start the interactive
// application, resolve state, or launch a provider.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "taskai: interactive workspace is not implemented in this baseline")
		return 1
	}

	if len(args) != 1 {
		fmt.Fprintln(stderr, "taskai: invalid arguments; use --help")
		return 2
	}

	switch args[0] {
	case "-h", "--help":
		fmt.Fprintln(stdout, helpText)
		return 0
	case "--version":
		fmt.Fprintf(stdout, "taskai %s\n", buildinfo.Version)
		return 0
	default:
		fmt.Fprintln(stderr, "taskai: invalid arguments; use --help")
		return 2
	}
}
