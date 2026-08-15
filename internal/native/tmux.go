package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/term"
)

var consoleSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// MayContinueRunner reports that Run can return when the viewer detaches while
// the provider process remains alive. In that case the process owner, not the
// viewer, is responsible for requesting the eventual ACP refresh.
type MayContinueRunner interface {
	ProcessMayContinue() bool
}

// TmuxRunner hosts the real provider harness in a tmux Session derived from
// the Task Console Session ID. The tmux pane invokes the private _native entry
// point so profile environment values never appear in tmux command metadata.
type TmuxRunner struct {
	Executable       string
	ConsoleSessionID string
	mayContinue      bool
}

func (r *TmuxRunner) ProcessMayContinue() bool { return r.mayContinue }

func (r *TmuxRunner) Run(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	r.mayContinue = false
	if strings.TrimSpace(r.Executable) == "" || strings.TrimSpace(os.Getenv("TMUX")) != "" || !interactiveTerminal(input, output) {
		return (ExecRunner{}).Run(ctx, invocation, input, output, errorOutput)
	}
	if !consoleSessionIDPattern.MatchString(r.ConsoleSessionID) {
		return fmt.Errorf("invalid Task Console Session ID %q for native harness", r.ConsoleSessionID)
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return (ExecRunner{}).Run(ctx, invocation, input, output, errorOutput)
	}
	name := TmuxSessionName(r.ConsoleSessionID)
	exists, err := tmuxHasSession(ctx, tmux, name)
	if err != nil {
		return err
	}
	if !exists {
		shellCommand := shellJoin(r.Executable, "_native", "--session", r.ConsoleSessionID)
		command := exec.CommandContext(ctx, tmux, "new-session", "-d", "-s", name, "-c", invocation.WorkingDirectory, shellCommand)
		command.Stdout, command.Stderr = output, errorOutput
		if err := command.Run(); err != nil {
			// Two viewers may race to create the same derived tmux Session. If one
			// won, both attach to that same provider process.
			if won, checkErr := tmuxHasSession(ctx, tmux, name); checkErr != nil || !won {
				return fmt.Errorf("start native tmux harness %q: %w", name, err)
			}
		}
	}
	if errorOutput != nil {
		fmt.Fprintf(errorOutput, "Agent Task Console: native harness %s · detach with Ctrl-b d · reattach with 'acpctl session attach %s'.\n", name, r.ConsoleSessionID)
	}
	r.mayContinue = true
	command := exec.CommandContext(ctx, tmux, "attach-session", "-t", "="+name)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if err := command.Run(); err != nil {
		return fmt.Errorf("attach native tmux harness %q: %w", name, err)
	}
	return nil
}

func interactiveTerminal(input io.Reader, output io.Writer) bool {
	inputFile, inputOK := input.(*os.File)
	outputFile, outputOK := output.(*os.File)
	return inputOK && outputOK && term.IsTerminal(inputFile.Fd()) && term.IsTerminal(outputFile.Fd())
}

func TmuxSessionName(consoleSessionID string) string {
	return "agent-" + consoleSessionID
}

func tmuxHasSession(ctx context.Context, tmux, name string) (bool, error) {
	command := exec.CommandContext(ctx, tmux, "has-session", "-t", "="+name)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect native tmux harness %q: %w", name, err)
}

func shellJoin(arguments ...string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = shellQuote(argument)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
