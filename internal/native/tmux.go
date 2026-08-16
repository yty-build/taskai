package native

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
)

var consoleSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

const tmuxOwnerOption = "@agent-task-console-owner"

const (
	tmuxOwnerRetryWindow   = 250 * time.Millisecond
	tmuxOwnerRetryInterval = 10 * time.Millisecond
)

type tmuxCommandFunc func(context.Context, string, []string, string, io.Reader, io.Writer, io.Writer) error

// MayContinueRunner reports that Run can return when the viewer detaches while
// the provider process remains alive. In that case the process owner, not the
// viewer, is responsible for requesting the eventual ACP refresh.
type MayContinueRunner interface {
	ProcessMayContinue() bool
}

// ActivityAwareRunner distinguishes a pre-launch validation/collision failure
// from an invocation that actually reached a provider process.
type ActivityAwareRunner interface {
	NativeActivityOccurred() bool
}

// TmuxRunner hosts the real provider harness in a tmux Session derived from
// the Task Console Session ID. The tmux pane invokes the private _native entry
// point so profile environment values never appear in tmux command metadata.
type TmuxRunner struct {
	Executable       string
	ConsoleSessionID string
	StateRoot        string
	mayContinue      bool
	activityOccurred bool
	isInteractive    func(io.Reader, io.Writer) bool
	lookupPath       func(string) (string, error)
	hasSession       func(context.Context, string, string) (bool, error)
	runCommand       tmuxCommandFunc
}

func (r *TmuxRunner) ProcessMayContinue() bool     { return r.mayContinue }
func (r *TmuxRunner) NativeActivityOccurred() bool { return r.activityOccurred }

func (r *TmuxRunner) Run(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	r.mayContinue = false
	r.activityOccurred = false
	isInteractive := interactiveTerminal
	if r.isInteractive != nil {
		isInteractive = r.isInteractive
	}
	if strings.TrimSpace(r.Executable) == "" || strings.TrimSpace(os.Getenv("TMUX")) != "" || !isInteractive(input, output) {
		return r.runDirect(ctx, invocation, input, output, errorOutput)
	}
	if !consoleSessionIDPattern.MatchString(r.ConsoleSessionID) {
		return fmt.Errorf("invalid Task Console Session ID %q for native harness", r.ConsoleSessionID)
	}
	lookupPath := exec.LookPath
	if r.lookupPath != nil {
		lookupPath = r.lookupPath
	}
	tmux, err := lookupPath("tmux")
	if err != nil {
		return r.runDirect(ctx, invocation, input, output, errorOutput)
	}
	name := TmuxSessionName(r.ConsoleSessionID)
	owner, err := tmuxOwnerIdentity(r.StateRoot, r.ConsoleSessionID)
	if err != nil {
		return err
	}
	hasSession := tmuxHasSession
	if r.hasSession != nil {
		hasSession = r.hasSession
	}
	runCommand := runTmuxCommand
	if r.runCommand != nil {
		runCommand = r.runCommand
	}
	exists, err := hasSession(ctx, tmux, name)
	if err != nil {
		return err
	}
	if exists {
		// Until the owner tag is verified, an existing process may still be our
		// provider. Conservatively prevent an ACP refresh on every collision or
		// inspection failure.
		r.mayContinue = true
	}
	if !exists {
		shellCommand := shellJoin(r.Executable, "_native", "--session", r.ConsoleSessionID)
		arguments := []string{
			"new-session", "-d", "-s", name, "-c", invocation.WorkingDirectory,
			"-e", "AGENT_HOME=" + r.StateRoot,
		}
		if currentPath := os.Getenv("PATH"); strings.TrimSpace(currentPath) != "" {
			arguments = append(arguments, "-e", "PATH="+currentPath)
		}
		arguments = append(arguments,
			shellCommand,
			// tmux parses the entire command sequence before new-session runs, so
			// its =name exact-target form cannot resolve the just-created Session
			// here. A full existing name wins before prefix matching; later reads
			// use =name once the Session definitely exists.
			";", "set-option", "-t", name, tmuxOwnerOption, owner,
		)
		if err := runCommand(ctx, tmux, arguments, "", nil, output, errorOutput); err != nil {
			// Two viewers may race to create the same derived tmux Session. If one
			// won, both attach to that same provider process.
			won, checkErr := hasSession(ctx, tmux, name)
			if checkErr != nil || !won {
				return fmt.Errorf("start native tmux harness %q: %w", name, err)
			}
			r.mayContinue = true
		} else {
			r.activityOccurred = true
			r.mayContinue = true
		}
	}
	owned, err := tmuxSessionOwned(ctx, runCommand, tmux, name, owner)
	if err != nil {
		return err
	}
	if !owned {
		return fmt.Errorf("tmux Session %q already exists but is not owned by this Agent Task Console state root; refusing to attach", name)
	}
	r.activityOccurred = true
	if errorOutput != nil {
		fmt.Fprintf(errorOutput, "Agent Task Console: native harness %s · detach with Ctrl-b d · reattach with 'acpctl session attach %s'.\n", name, r.ConsoleSessionID)
	}
	r.mayContinue = true
	if err := runCommand(ctx, tmux, []string{"attach-session", "-t", "=" + name}, "", input, output, errorOutput); err != nil {
		return fmt.Errorf("attach native tmux harness %q: %w", name, err)
	}
	return nil
}

func (r *TmuxRunner) runDirect(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	direct := &ExecRunner{}
	err := direct.Run(ctx, invocation, input, output, errorOutput)
	r.activityOccurred = direct.NativeActivityOccurred()
	return err
}

func tmuxOwnerIdentity(stateRoot, consoleSessionID string) (string, error) {
	if strings.TrimSpace(stateRoot) == "" {
		return "", fmt.Errorf("Agent Task Console state root is required for native tmux attachment")
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Agent Task Console state root: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return fmt.Sprintf("v1:%x:%s", sum, consoleSessionID), nil
}

func tmuxSessionOwned(ctx context.Context, run tmuxCommandFunc, tmux, name, expected string) (bool, error) {
	deadline := time.Now().Add(tmuxOwnerRetryWindow)
	for {
		var output strings.Builder
		// tmux 3.5a accepts =name here but silently returns an empty user-option
		// value. has-session already proved the exact name exists, and tmux gives
		// a full-name match precedence over prefix matching, so use the plain name.
		if err := run(ctx, tmux, []string{"show-options", "-qv", "-t", name, tmuxOwnerOption}, "", nil, &output, io.Discard); err != nil {
			return false, fmt.Errorf("verify native tmux harness %q: %w", name, err)
		}
		actual := strings.TrimSpace(output.String())
		if actual == expected {
			return true, nil
		}
		if actual != "" || time.Now().Add(tmuxOwnerRetryInterval).After(deadline) {
			return false, nil
		}
		timer := time.NewTimer(tmuxOwnerRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func runTmuxCommand(ctx context.Context, executable string, arguments []string, directory string, input io.Reader, output, errorOutput io.Writer) error {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Dir = directory
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	return command.Run()
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
