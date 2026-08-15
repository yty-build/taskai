package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	agentregistry "github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/buildinfo"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
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
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return ExitOK
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(stdout, info.String())
		return ExitOK
	}

	home, err := config.ResolveHome()
	if err != nil {
		return fail(stderr, ExitFailure, err)
	}
	database, err := store.Open(ctx, home.Database)
	if err != nil {
		return fail(stderr, ExitFailure, err)
	}
	defer database.Close()

	taskService := task.NewService(database)
	switch args[0] {
	case "_worker":
		err = runHiddenWorker(ctx, args[1:], home, database)
	case "tasks":
		err = runTasks(ctx, args[1:], stdout, taskService)
	case "task":
		err = runTask(ctx, args[1:], stdout, taskService)
	case "start":
		registry, loadErr := agentregistry.Load(home.Agents)
		if loadErr != nil {
			err = loadErr
			break
		}
		err = runStart(ctx, args[1:], stdout, taskService, registry)
	case "agents":
		registry, loadErr := agentregistry.Load(home.Agents)
		if loadErr != nil {
			err = loadErr
			break
		}
		err = runAgents(args[1:], stdout, registry)
	default:
		err = usagef("unknown command %q; run 'agent help' for usage", strings.Join(args, " "))
	}
	if err == nil {
		return ExitOK
	}
	var usageErr *commandUsageError
	if errors.As(err, &usageErr) {
		return fail(stderr, ExitUsage, usageErr)
	}
	return fail(stderr, ExitFailure, err)
}

func runHiddenWorker(ctx context.Context, args []string, home config.Home, database *store.Store) error {
	options, err := parseOptions(args, map[string]optionKind{"--session": stringOption})
	if err != nil || len(options.positionals) != 0 || options.values["--session"] == "" {
		return usagef("usage: agent _worker --session <session-id>")
	}
	return worker.RunSession(ctx, home, database, options.values["--session"])
}

func fail(stderr io.Writer, code int, err error) int {
	fmt.Fprintf(stderr, "agent: %s\n", err)
	return code
}

type commandUsageError struct{ message string }

func (e *commandUsageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &commandUsageError{message: fmt.Sprintf(format, args...)}
}

func runTasks(ctx context.Context, args []string, stdout io.Writer, service *task.Service) error {
	if len(args) != 0 {
		return usagef("usage: agent tasks")
	}
	tasks, err := service.List(ctx)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "No Tasks.")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tTITLE")
	for _, item := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", item.ID, item.Status, item.Title)
	}
	return w.Flush()
}

func runTask(ctx context.Context, args []string, stdout io.Writer, service *task.Service) error {
	if len(args) == 0 {
		return usagef("usage: agent task new|status|<task-id>")
	}
	switch args[0] {
	case "new":
		return runTaskNew(ctx, args[1:], stdout, service)
	case "status":
		if len(args) != 3 {
			return usagef("usage: agent task status <task-id> <status>")
		}
		status, err := task.ParseStatus(args[2])
		if err != nil {
			return usagef("%s", err)
		}
		updated, err := service.SetStatus(ctx, args[1], status)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Updated %s to %s\n", updated.ID, updated.Status)
		return nil
	default:
		if len(args) != 1 {
			return usagef("usage: agent task <task-id>")
		}
		return showTask(ctx, args[0], stdout, service)
	}
}

func runTaskNew(ctx context.Context, args []string, stdout io.Writer, service *task.Service) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--description": stringOption, "--external-ref": stringOption, "--metadata-json": stringOption,
	})
	if err != nil {
		return usagef("agent task new: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: agent task new <title> [--description <text>] [--external-ref <ref>] [--metadata-json <json>]")
	}
	created, err := service.Create(ctx, task.CreateParams{
		Title: options.positionals[0], Description: options.values["--description"], ExternalRef: options.values["--external-ref"],
		MetadataJSON: rawJSON(options.values["--metadata-json"]),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created %s\n", created.ID)
	return nil
}

func showTask(ctx context.Context, id string, stdout io.Writer, service *task.Service) error {
	detail, err := service.Get(ctx, id)
	if err != nil {
		return err
	}
	item := detail.Task
	fmt.Fprintf(stdout, "%s  %s\nStatus: %s\n", item.ID, item.Title, item.Status)
	if item.Description != "" {
		fmt.Fprintf(stdout, "Description: %s\n", item.Description)
	}
	if item.ExternalRef != "" {
		fmt.Fprintf(stdout, "External ref: %s\n", item.ExternalRef)
	}
	fmt.Fprintf(stdout, "Created: %s\nUpdated: %s\n\n", displayTime(item.CreatedAt), displayTime(item.UpdatedAt))
	if len(detail.Sessions) == 0 {
		fmt.Fprintln(stdout, "No Sessions.")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tROLE\tAGENT\tSTATUS")
	for _, current := range detail.Sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", current.ID, valueOrDash(current.Role), current.Agent, current.Status)
	}
	return w.Flush()
}

func runStart(ctx context.Context, args []string, stdout io.Writer, service *task.Service, registry *agentregistry.Registry) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--agent": stringOption, "--repo": stringOption, "--role": stringOption, "--description": stringOption,
		"--external-ref": stringOption, "--metadata-json": stringOption,
	})
	if err != nil {
		return usagef("agent start: %s", err)
	}
	if len(options.positionals) != 1 || strings.TrimSpace(options.values["--agent"]) == "" || strings.TrimSpace(options.values["--repo"]) == "" {
		return usagef("usage: agent start <title> --agent <name> --repo <path> [--role <role>]")
	}
	profile, err := registry.Get(options.values["--agent"])
	if err != nil {
		return err
	}
	repoPath, err := filepath.Abs(options.values["--repo"])
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	role := strings.TrimSpace(options.values["--role"])
	if role == "" {
		role = "worker"
	}
	createdTask, createdSession, err := service.Start(ctx,
		task.CreateParams{Title: options.positionals[0], Description: options.values["--description"], ExternalRef: options.values["--external-ref"], MetadataJSON: rawJSON(options.values["--metadata-json"]), Status: task.StatusWorking},
		session.CreateParams{Role: role, Agent: profile.Name, Provider: profile.Provider, RepoPath: filepath.Clean(repoPath), Status: session.StatusStarting},
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created %s\nStarted %s\n", createdTask.ID, createdSession.ID)
	return nil
}

func runAgents(args []string, stdout io.Writer, registry *agentregistry.Registry) error {
	if len(args) > 1 {
		return usagef("usage: agent agents [name]")
	}
	if len(args) == 1 {
		profile, err := registry.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Name: %s\nProvider: %s\nCommand: %s\n", profile.Name, profile.Provider, strings.Join(profile.Argv(), " "))
		if len(profile.Environment) == 0 {
			fmt.Fprintln(stdout, "Environment: -")
			return nil
		}
		keys := make([]string, 0, len(profile.Environment))
		for name := range profile.Environment {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		fmt.Fprintf(stdout, "Environment: %s (values hidden)\n", strings.Join(keys, ", "))
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tCOMMAND")
	for _, profile := range registry.List() {
		fmt.Fprintf(w, "%s\t%s\t%s\n", profile.Name, profile.Provider, profile.Command)
	}
	return w.Flush()
}

func rawJSON(value string) json.RawMessage {
	if value == "" {
		return nil
	}
	return json.RawMessage(value)
}

func displayTime(value time.Time) string { return value.UTC().Format(time.RFC3339) }

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
