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
)

type Invocation struct {
	Command          string
	Args             []string
	WorkingDirectory string
	Environment      map[string]string
}

type Runner interface {
	Run(context.Context, Invocation, io.Reader, io.Writer, io.Writer) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, invocation Invocation, input io.Reader, output, errorOutput io.Writer) error {
	if strings.TrimSpace(invocation.Command) == "" {
		return fmt.Errorf("native command is required")
	}
	command := exec.CommandContext(ctx, invocation.Command, invocation.Args...)
	command.Dir = invocation.WorkingDirectory
	command.Env = mergeEnvironment(invocation.Environment)
	command.Stdin, command.Stdout, command.Stderr = input, output, errorOutput
	if err := command.Run(); err != nil {
		return fmt.Errorf("native %s process: %w", invocation.Command, err)
	}
	return nil
}

// BuildResume returns only documented, ID-based resume invocations. Profiles
// without a known mapping are unavailable instead of falling back to a new
// provider conversation.
func BuildResume(profile agents.Profile, nativeSessionID, workingDirectory string) (Invocation, error) {
	if err := profile.Validate(); err != nil {
		return Invocation{}, err
	}
	if strings.TrimSpace(nativeSessionID) == "" {
		return Invocation{}, fmt.Errorf("provider-native Session ID is required")
	}
	if strings.TrimSpace(workingDirectory) == "" {
		return Invocation{}, fmt.Errorf("native working directory is required")
	}
	arguments := append([]string(nil), profile.Args...)
	switch profile.Name {
	case "codex":
		arguments = append(arguments, "resume", nativeSessionID)
	case "claude", "gemini":
		arguments = append(arguments, "--resume", nativeSessionID)
	case "opencode":
		arguments = append(arguments, "--session", nativeSessionID)
	default:
		return Invocation{}, fmt.Errorf("agent %q does not declare a supported provider-native resume command", profile.Name)
	}
	return Invocation{
		Command: profile.Command, Args: arguments, WorkingDirectory: workingDirectory,
		Environment: cloneEnvironment(profile.Environment),
	}, nil
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
