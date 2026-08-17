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
var nativeGenerationPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

const tmuxOwnerOption = "@agent-task-console-owner"

// nativeOwnerProtocolVersion deliberately changes when native handoff
// semantics become incompatible. The owner token is disposable runtime state;
// bumping it makes an older live harness fail closed instead of being reused by
// a newer console that cannot prove the old process follows the same contract.
const nativeOwnerProtocolVersion = "v3"

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

// PreexistingOwnershipAwareRunner lets the service constrain an outer viewer
// that did not acquire fresh ownership to attach-only behavior. Such a viewer
// must never direct-run or create a replacement provider process.
type PreexistingOwnershipAwareRunner interface {
	SetPreexistingOwnership(bool)
}

// PreexistingOwnershipStateAwareRunner gives a runner the worker's proven
// phase for an existing ownership generation. Reserved ownership can be
// recovered after exact harness absence; active ownership must remain blocked
// because tmux disappearance does not prove provider-process exit.
type PreexistingOwnershipStateAwareRunner interface {
	SetPreexistingOwnershipState(string)
}

const (
	PreexistingOwnershipReserved = "reserved"
	PreexistingOwnershipActive   = "active"
)

// ReservationRunner separates an outer tmux viewer's durable reservation from
// the hidden process that can actually write the provider conversation. A
// direct fallback must activate the reservation immediately before it starts
// the provider.
type ReservationRunner interface {
	PreexistingOwnershipAwareRunner
	SetReservationActivator(func(context.Context) error)
	SetReservationGeneration(string)
}

// ReservationRecoveryVerifier proves that the one exact stable tmux harness
// is absent. The worker independently requires a reservation (never an active
// provider owner) before it will clear ownership.
type ReservationRecoveryVerifier interface {
	VerifyReservationAbsent(context.Context, string) error
}

type TmuxReservationRecoveryVerifier struct {
	lookupPath func(string) (string, error)
	hasSession func(context.Context, string, string) (bool, error)
}

// TmuxRunner hosts the real provider harness in a tmux Session derived from
// the Task Console Session ID. The tmux pane invokes the private _native entry
// point so profile environment values never appear in tmux command metadata.
type TmuxRunner struct {
	Executable            string
	ConsoleSessionID      string
	StateRoot             string
	OwnerToken            string
	ReservationGeneration string
	preexistingOwner      bool
	preexistingState      string
	activateReservation   func(context.Context) error
	mayContinue           bool
	activityOccurred      bool
	isInteractive         func(io.Reader, io.Writer) bool
	lookupPath            func(string) (string, error)
	hasSession            func(context.Context, string, string) (bool, error)
	runCommand            tmuxCommandFunc
}

func (r *TmuxRunner) ProcessMayContinue() bool     { return r.mayContinue }
func (r *TmuxRunner) NativeActivityOccurred() bool { return r.activityOccurred }
func (r *TmuxRunner) SetPreexistingOwnership(value bool) {
	r.preexistingOwner = value
	if !value {
		r.preexistingState = ""
	} else if r.preexistingState == "" {
		// Older callers can express only the conservative case.
		r.preexistingState = PreexistingOwnershipActive
	}
}
func (r *TmuxRunner) SetPreexistingOwnershipState(state string) {
	r.preexistingState = state
	r.preexistingOwner = state != ""
}
func (r *TmuxRunner) SetReservationActivator(activate func(context.Context) error) {
	r.activateReservation = activate
}
func (r *TmuxRunner) SetReservationGeneration(generation string) {
	r.ReservationGeneration = generation
}

func (r *TmuxRunner) Run(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	r.mayContinue = r.preexistingOwner
	r.activityOccurred = false
	isInteractive := interactiveTerminal
	if r.isInteractive != nil {
		isInteractive = r.isInteractive
	}
	insideTmux := strings.TrimSpace(os.Getenv("TMUX")) != ""
	interactive := isInteractive(input, output)
	if r.preexistingOwner && !interactive {
		return fmt.Errorf("preexisting native ownership requires an interactive tmux attachment; refusing to start a second provider-native process")
	}
	if insideTmux {
		if strings.TrimSpace(r.Executable) == "" && !r.preexistingOwner {
			return fmt.Errorf("the current terminal is inside tmux, but the Agent Task Console executable is unavailable; refusing to start a second provider-native process")
		}
		if !interactive {
			return fmt.Errorf("the current terminal is inside tmux, but native handoff has no interactive client; refusing to start a second provider-native process")
		}
	} else if (strings.TrimSpace(r.Executable) == "" || !interactive) && !r.preexistingOwner {
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
		if r.preexistingOwner {
			return fmt.Errorf("preexisting native ownership requires the existing tmux harness, but the tmux executable is unavailable; refusing to start a second provider-native process: %w", err)
		}
		if insideTmux {
			return fmt.Errorf("the current terminal is inside tmux, but the tmux executable is unavailable; refusing to start a second provider-native process: %w", err)
		}
		return r.runDirect(ctx, invocation, input, output, errorOutput)
	}
	name := TmuxSessionName(r.ConsoleSessionID)
	owner, err := OwnerToken(r.StateRoot, r.ConsoleSessionID)
	if err != nil {
		return err
	}
	if r.OwnerToken != "" && r.OwnerToken != owner {
		return fmt.Errorf("native owner token does not match this Agent Task Console state root and Session")
	}
	r.OwnerToken = owner
	if !nativeGenerationPattern.MatchString(r.ReservationGeneration) {
		return fmt.Errorf("native reservation generation is missing or invalid; refusing provider handoff")
	}
	harnessOwner := owner + ":" + r.ReservationGeneration
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
		// The worker has already granted native ownership. An inspection failure
		// cannot prove that an older provider process is absent.
		r.mayContinue = true
		return err
	}
	if exists {
		// Until the owner tag is verified, an existing process may still be our
		// provider. Conservatively prevent an ACP refresh on every collision or
		// inspection failure.
		r.mayContinue = true
	}
	if !exists && r.preexistingOwner {
		if r.preexistingState == PreexistingOwnershipReserved {
			return fmt.Errorf("pre-launch native reservation exists, but tmux Session %q is missing; refusing to create a second provider-native process. Run acpctl session recover-native %s to verify exact harness absence and schedule reconciliation before retrying attachment", name, r.ConsoleSessionID)
		}
		return fmt.Errorf("active provider-native ownership exists, but tmux Session %q is missing; refusing to create a second provider-native process. Missing tmux does not prove provider exit, so automated recovery is refused and ACP remains blocked", name)
	}
	if !exists {
		shellCommand := shellJoin(
			r.Executable, "_native", "--session", r.ConsoleSessionID,
			"--owner-token", owner, "--reservation-generation", r.ReservationGeneration,
		)
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
			";", "set-option", "-t", name, tmuxOwnerOption, harnessOwner,
		)
		if err := runCommand(ctx, tmux, arguments, "", nil, output, errorOutput); err != nil {
			// Two viewers may race to create the same derived tmux Session. If one
			// won, both attach to that same provider process.
			won, checkErr := hasSession(ctx, tmux, name)
			if checkErr != nil {
				r.mayContinue = true
				return fmt.Errorf("inspect native tmux harness %q after create uncertainty: %w", name, checkErr)
			}
			if !won {
				return fmt.Errorf("start native tmux harness %q: %w", name, err)
			}
			r.mayContinue = true
		} else {
			r.activityOccurred = true
			r.mayContinue = true
		}
	}
	owned, err := tmuxSessionOwned(ctx, runCommand, tmux, name, harnessOwner)
	if err != nil {
		return err
	}
	if !owned {
		// A failed owner check cannot prove that the existing provider is gone.
		// Retain exclusive worker ownership so ACP can never write concurrently.
		r.mayContinue = true
		return fmt.Errorf("tmux Session %q has a different native owner protocol, state root, or generation; refusing to attach. Do not kill it based on this mismatch alone because it may be a legitimate newer harness. Return to the console, inspect Session status, and retry attachment. Run acpctl session recover-native %s only after the worker reports a pre-launch reservation and the exact harness is independently known absent", name, r.ConsoleSessionID)
	}
	r.activityOccurred = true
	r.mayContinue = true
	if insideTmux {
		if errorOutput != nil {
			fmt.Fprintf(errorOutput, "Agent Task Console: switching to native harness %s · Ctrl-b L returns to this console · Ctrl-b d detaches while the provider continues.\n", name)
		}
		if err := runCommand(ctx, tmux, []string{"switch-client", "-t", "=" + name}, "", nil, output, errorOutput); err != nil {
			return fmt.Errorf("switch current tmux client to native harness %q: %w", name, err)
		}
		return nil
	}
	if errorOutput != nil {
		fmt.Fprintf(errorOutput, "Agent Task Console: native harness %s · detach with Ctrl-b d · reattach with 'acpctl session attach %s'.\n", name, r.ConsoleSessionID)
	}
	if err := runCommand(ctx, tmux, []string{"attach-session", "-t", "=" + name}, "", input, output, errorOutput); err != nil {
		return fmt.Errorf("attach native tmux harness %q: %w", name, err)
	}
	return nil
}

func (r *TmuxRunner) runDirect(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	if r.activateReservation != nil {
		if err := r.activateReservation(ctx); err != nil {
			return fmt.Errorf("activate native provider reservation before direct execution: %w", err)
		}
	}
	direct := &ExecRunner{}
	err := direct.Run(ctx, invocation, input, output, errorOutput)
	r.activityOccurred = direct.NativeActivityOccurred()
	return err
}

// OwnerToken is the protocol-versioned, stable per-state-root, per-Session
// identity shared by the outer viewer and hidden tmux provider owner. It
// contains no filesystem path or provider credential. A protocol bump rejects
// older live harnesses; within one protocol it makes acquisition idempotent.
func OwnerToken(stateRoot, consoleSessionID string) (string, error) {
	if strings.TrimSpace(stateRoot) == "" {
		return "", fmt.Errorf("Agent Task Console state root is required for native tmux attachment")
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Agent Task Console state root: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return fmt.Sprintf("%s:%x:%s", nativeOwnerProtocolVersion, sum, consoleSessionID), nil
}

func tmuxOwnerIdentity(stateRoot, consoleSessionID string) (string, error) {
	return OwnerToken(stateRoot, consoleSessionID)
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

func (v TmuxReservationRecoveryVerifier) VerifyReservationAbsent(ctx context.Context, consoleSessionID string) error {
	if !consoleSessionIDPattern.MatchString(consoleSessionID) {
		return fmt.Errorf("invalid Task Console Session ID %q for native reservation recovery", consoleSessionID)
	}
	lookupPath := exec.LookPath
	if v.lookupPath != nil {
		lookupPath = v.lookupPath
	}
	tmux, err := lookupPath("tmux")
	if err != nil {
		return fmt.Errorf("verify orphaned native reservation: tmux executable is unavailable: %w", err)
	}
	hasSession := tmuxHasSession
	if v.hasSession != nil {
		hasSession = v.hasSession
	}
	name := TmuxSessionName(consoleSessionID)
	exists, err := hasSession(ctx, tmux, name)
	if err != nil {
		return fmt.Errorf("verify orphaned native reservation: %w", err)
	}
	if exists {
		return fmt.Errorf("native reservation recovery refused while exact tmux Session %q still exists. Do not kill it from this check alone; return to the console, inspect Session status, and retry recovery only after the harness is independently known absent", name)
	}
	return nil
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
