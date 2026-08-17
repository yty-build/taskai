// Package native maps supported agent profiles to their provider-native
// interactive resume command. It never guesses a conversation identifier.
package native

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/session"
)

type Invocation struct {
	Command          string
	Args             []string
	WorkingDirectory string
	Environment      map[string]string
}

type ProviderOptions = session.ProviderOptions

// ValidateProviderOptions proves that every requested option has a documented,
// argument-safe mapping for both the profile's ACP session and its exact native
// resume command. Empty options remain valid for every profile because they
// preserve provider defaults.
func ValidateProviderOptions(profile agents.Profile, options ProviderOptions) error {
	options.Model = strings.TrimSpace(options.Model)
	options.ReasoningEffort = strings.TrimSpace(options.ReasoningEffort)
	if err := options.Validate(); err != nil {
		return err
	}
	if options.Model == "" && options.ReasoningEffort == "" {
		return nil
	}
	switch profile.Name {
	case "codex":
		return nil
	case "claude":
		if strings.EqualFold(options.ReasoningEffort, "default") {
			return fmt.Errorf("agent %q uses a blank reasoning effort for Provider default; leave reasoning effort empty", profile.Name)
		}
		return nil
	case "grok", "grok-build":
		return fmt.Errorf("agent %q does not advertise verifiable ACP model or reasoning controls; leave both controls on Provider default", profile.Name)
	case "opencode":
		if options.ReasoningEffort != "" {
			return fmt.Errorf("agent %q does not advertise a provider-native reasoning effort option; leave reasoning effort on Provider default", profile.Name)
		}
		return nil
	default:
		return fmt.Errorf("agent %q does not declare documented provider-native model or reasoning effort flags; leave both controls on Provider default", profile.Name)
	}
}

type Runner interface {
	Run(context.Context, Invocation, io.Reader, io.Writer, io.Writer) error
}

// ExecRunner launches the provider directly and records whether the process
// actually started. Callers use that distinction to request provider transcript
// reconciliation only after an invocation reached the provider.
type ExecRunner struct {
	activityOccurred      bool
	preexistingOwner      bool
	reservationGeneration string
	activateReservation   func(context.Context) error
}

func (r *ExecRunner) NativeActivityOccurred() bool { return r.activityOccurred }
func (r *ExecRunner) SetPreexistingOwnership(value bool) {
	r.preexistingOwner = value
}
func (r *ExecRunner) SetReservationActivator(activate func(context.Context) error) {
	r.activateReservation = activate
}
func (r *ExecRunner) SetReservationGeneration(generation string) {
	r.reservationGeneration = generation
}

func (r *ExecRunner) Run(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	r.activityOccurred = false
	if r.preexistingOwner {
		return fmt.Errorf("preexisting native ownership has no attachable harness; refusing to start a second provider-native process")
	}
	if strings.TrimSpace(invocation.Command) == "" {
		return fmt.Errorf("native command is required")
	}
	command := exec.CommandContext(ctx, invocation.Command, invocation.Args...)
	command.Dir = invocation.WorkingDirectory
	command.Env = mergeEnvironment(invocation.Environment)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if r.activateReservation != nil {
		if err := r.activateReservation(ctx); err != nil {
			return fmt.Errorf("activate native provider reservation before execution: %w", err)
		}
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("native %s process: %w", invocation.Command, err)
	}
	r.activityOccurred = true
	if err := command.Wait(); err != nil {
		return fmt.Errorf("native %s process: %w", invocation.Command, err)
	}
	return nil
}

// BuildResume returns only documented, ID-based resume invocations. Profiles
// without a known mapping are unavailable instead of falling back to a new
// provider conversation.
func BuildResume(profile agents.Profile, conversationID, workingDirectory string, requested ...ProviderOptions) (Invocation, error) {
	if err := profile.Validate(); err != nil {
		return Invocation{}, err
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return Invocation{}, fmt.Errorf("canonical provider conversation ID is required")
	}
	if strings.TrimSpace(workingDirectory) == "" {
		return Invocation{}, fmt.Errorf("native working directory is required")
	}
	if len(requested) > 1 {
		return Invocation{}, fmt.Errorf("native provider options may be supplied only once")
	}
	options := ProviderOptions{}
	if len(requested) == 1 {
		options.Model = strings.TrimSpace(requested[0].Model)
		options.ReasoningEffort = strings.TrimSpace(requested[0].ReasoningEffort)
	}
	if err := ValidateProviderOptions(profile, options); err != nil {
		return Invocation{}, err
	}
	arguments := append([]string(nil), profile.Args...)
	command := profile.Command
	switch profile.Name {
	case "codex":
		arguments = append(arguments, "resume")
		arguments = appendNativeModel(arguments, options.Model)
		if options.ReasoningEffort != "" {
			arguments = append(arguments, "--config", `model_reasoning_effort="`+options.ReasoningEffort+`"`)
		}
		arguments = append(arguments, conversationID)
	case "claude":
		arguments = appendNativeModel(arguments, options.Model)
		if options.ReasoningEffort != "" {
			arguments = append(arguments, "--effort", options.ReasoningEffort)
		}
		arguments = append(arguments, "--resume", conversationID)
	case "grok", "grok-build":
		if filepathBase(command) == "grok-build" {
			command = "grok"
		}
		arguments = appendNativeModel(arguments, options.Model)
		if options.ReasoningEffort != "" {
			arguments = append(arguments, "--reasoning-effort", options.ReasoningEffort)
		}
		arguments = append(arguments, "--resume", conversationID)
	case "opencode":
		// Keep the same pinned OpenCode executable used by the ACP profile, but
		// remove its ACP subcommand when opening the real interactive harness.
		// This avoids resuming one conversation through two CLI versions.
		if filepathBase(command) == "npx" && isOpenCodeNPX(arguments) {
			arguments = arguments[:len(arguments)-1]
		} else if filepathBase(command) == "opencode" && len(arguments) > 0 && arguments[len(arguments)-1] == "acp" {
			arguments = arguments[:len(arguments)-1]
		} else if filepathBase(command) != "opencode" {
			command, arguments = "opencode", nil
		}
		arguments = appendNativeModel(arguments, options.Model)
		arguments = append(arguments, "--session", conversationID)
	default:
		return Invocation{}, fmt.Errorf("agent %q does not declare a supported provider-native resume command", profile.Name)
	}
	return Invocation{
		Command: command, Args: arguments, WorkingDirectory: workingDirectory,
		Environment: cloneEnvironment(profile.Environment),
	}, nil
}

func appendNativeModel(arguments []string, model string) []string {
	if model == "" {
		return arguments
	}
	return append(arguments, "--model", model)
}

func isOpenCodeNPX(arguments []string) bool {
	if len(arguments) < 2 || arguments[len(arguments)-1] != "acp" {
		return false
	}
	for _, argument := range arguments[:len(arguments)-1] {
		if argument == "opencode-ai" || strings.HasPrefix(argument, "opencode-ai@") {
			return true
		}
	}
	return false
}

func filepathBase(command string) string {
	command = strings.TrimRight(command, `/\\`)
	if index := strings.LastIndexAny(command, `/\\`); index >= 0 {
		return command[index+1:]
	}
	return command
}

func mergeEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			values[name] = value
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result
}

func cloneEnvironment(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}
