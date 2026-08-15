package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	agentregistry "github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/api"
	"github.com/yty-build/acp-gateway/internal/buildinfo"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/console"
	"github.com/yty-build/acp-gateway/internal/render"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/web"
	"github.com/yty-build/acp-gateway/internal/worker"
)

var sessionRuntimeFactory = func(home config.Home, executable string) appservice.Runtime {
	return appservice.LocalRuntime{Executable: executable, Home: home}
}

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
	if args[0] != "_worker" {
		if _, err := worker.ReconcileStale(ctx, home, database); err != nil {
			return fail(stderr, ExitFailure, err)
		}
	}
	executable, executableErr := os.Executable()
	if executableErr != nil {
		return fail(stderr, ExitFailure, fmt.Errorf("resolve agent executable: %w", executableErr))
	}
	runtime := sessionRuntimeFactory(home, executable)

	taskService := task.NewService(database)
	switch args[0] {
	case "_worker":
		err = runHiddenWorker(ctx, args[1:], home, database)
	case "tasks":
		err = runTasks(ctx, args[1:], stdout, taskService)
	case "task":
		sessionService := appservice.NewSessionService(database, nil, home, runtime)
		err = runTask(ctx, args[1:], stdout, taskService, sessionService)
	case "start":
		registry, loadErr := agentregistry.Load(home.Agents)
		if loadErr != nil {
			err = loadErr
			break
		}
		sessionService := appservice.NewSessionService(database, registry, home, runtime)
		err = runStart(ctx, args[1:], stdout, sessionService)
	case "session":
		var registry *agentregistry.Registry
		if len(args) > 1 && (args[1] == "start" || args[1] == "review" || nativeAttachRequested(args[1:])) {
			registry, err = agentregistry.Load(home.Agents)
			if err != nil {
				break
			}
		}
		sessionService := appservice.NewSessionService(database, registry, home, runtime)
		err = runSession(ctx, args[1:], stdout, stderr, sessionService)
	case "agents":
		registry, loadErr := agentregistry.Load(home.Agents)
		if loadErr != nil {
			err = loadErr
			break
		}
		err = runAgents(args[1:], stdout, registry)
	case "serve":
		registry, loadErr := agentregistry.Load(home.Agents)
		if loadErr != nil {
			err = loadErr
			break
		}
		sessionService := appservice.NewSessionService(database, registry, home, runtime)
		err = runServe(ctx, args[1:], stdout, taskService, sessionService, registry)
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

func runServe(ctx context.Context, args []string, stdout io.Writer, tasks *task.Service, sessions *appservice.SessionService, registry *agentregistry.Registry) error {
	options, err := parseOptions(args, map[string]optionKind{"--listen": stringOption})
	if err != nil || len(options.positionals) != 0 {
		return usagef("usage: agent serve [--listen 127.0.0.1:8080]")
	}
	address := options.values["--listen"]
	if strings.TrimSpace(address) == "" {
		address = "127.0.0.1:8080"
	}
	server, err := web.New(tasks, sessions, registry)
	if err != nil {
		return err
	}
	listener, err := api.ListenLoopback(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	if _, err := fmt.Fprintf(stdout, "Agent Task Console listening at http://%s\nPress Ctrl-C to stop the web viewer; Session workers will continue.\n", listener.Addr().String()); err != nil {
		return err
	}
	return api.Serve(ctx, listener, server.Handler())
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

func runTask(ctx context.Context, args []string, stdout io.Writer, service *task.Service, sessions *appservice.SessionService) error {
	if len(args) == 0 {
		return usagef("usage: agent task new|status|attach|logs|<task-id>")
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
	case "logs":
		return runTaskLogs(ctx, args[1:], stdout, sessions)
	case "attach":
		if len(args) != 2 {
			return usagef("usage: agent task attach <task-id>")
		}
		return console.AttachTask(ctx, console.Services{Tasks: service, Sessions: sessions}, args[1], os.Stdin, stdout)
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

func runStart(ctx context.Context, args []string, stdout io.Writer, sessions *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--agent": stringOption, "--repo": stringOption, "--role": stringOption, "--description": stringOption,
		"--external-ref": stringOption, "--metadata-json": stringOption, "--prompt": stringOption, "--approve-all": boolOption,
		"--worktree": boolOption, "--allow-shared-checkout": boolOption,
	})
	if err != nil {
		return usagef("agent start: %s", err)
	}
	if len(options.positionals) != 1 || strings.TrimSpace(options.values["--agent"]) == "" || strings.TrimSpace(options.values["--repo"]) == "" {
		return usagef("usage: agent start <title> --agent <name> --repo <path> [--role <role>] [--worktree] [--allow-shared-checkout]")
	}
	started, err := sessions.StartTask(ctx,
		task.CreateParams{Title: options.positionals[0], Description: options.values["--description"], ExternalRef: options.values["--external-ref"], MetadataJSON: rawJSON(options.values["--metadata-json"]), Status: task.StatusWorking},
		appservice.StartSessionParams{
			Agent: options.values["--agent"], RepoPath: options.values["--repo"], Role: options.values["--role"], Prompt: options.values["--prompt"],
			ApproveAll: options.booleans["--approve-all"], Worktree: options.booleans["--worktree"], AllowSharedCheckout: options.booleans["--allow-shared-checkout"],
		},
	)
	if err != nil {
		if started.Task.ID != "" && started.Session.ID != "" {
			return fmt.Errorf("created Task %s and Session %s, but start failed: %w", started.Task.ID, started.Session.ID, err)
		}
		return err
	}
	fmt.Fprintf(stdout, "Created %s\nStarted %s\nInitial request %s (%s)\n", started.Task.ID, started.Session.ID, started.RequestID, started.State)
	return nil
}

func runSession(ctx context.Context, args []string, stdout, stderr io.Writer, service *appservice.SessionService) error {
	if len(args) == 0 {
		return usagef("usage: agent session start|review|attach|prompt|cancel|resume|stop|logs|<session-id>")
	}
	switch args[0] {
	case "start":
		return runSessionStart(ctx, args[1:], stdout, service)
	case "prompt":
		if len(args) != 3 {
			return usagef("usage: agent session prompt <session-id> <message>")
		}
		accepted, err := service.Prompt(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Accepted %s (%s)\n", accepted.RequestID, accepted.State)
		return nil
	case "cancel":
		if len(args) != 2 {
			return usagef("usage: agent session cancel <session-id>")
		}
		if err := service.Cancel(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Cancellation requested for %s\n", args[1])
		return nil
	case "resume":
		options, err := parseOptions(args[1:], map[string]optionKind{"--allow-shared-checkout": boolOption})
		if err != nil || len(options.positionals) != 1 {
			return usagef("usage: agent session resume <session-id> [--allow-shared-checkout]")
		}
		resumed, err := service.ResumeWithOptions(ctx, options.positionals[0], options.booleans["--allow-shared-checkout"])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Resumed %s\n", resumed.ID)
		return nil
	case "stop":
		if len(args) != 2 {
			return usagef("usage: agent session stop <session-id>")
		}
		if err := service.Stop(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Stopped %s\n", args[1])
		return nil
	case "logs":
		return runSessionLogs(ctx, args[1:], stdout, service)
	case "review":
		options, err := parseOptions(args[1:], map[string]optionKind{"--agent": stringOption, "--allow-active-producer": boolOption})
		if err != nil || len(options.positionals) != 1 || strings.TrimSpace(options.values["--agent"]) == "" {
			return usagef("usage: agent session review <producer-session-id> --agent <reviewer> [--allow-active-producer]")
		}
		launched, err := service.Review(ctx, appservice.ReviewSessionParams{
			ProducerID: options.positionals[0], Agent: options.values["--agent"], AllowActiveProducer: options.booleans["--allow-active-producer"],
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Started reviewer %s for %s\nInitial request %s (%s)\n", launched.Session.ID, options.positionals[0], launched.RequestID, launched.State)
		return nil
	case "attach":
		options, err := parseOptions(args[1:], map[string]optionKind{"--native": boolOption, "--cancel-active": boolOption, "--reconcile-only": boolOption})
		if err != nil || len(options.positionals) != 1 {
			return usagef("usage: agent session attach <session-id> [--native [--cancel-active|--reconcile-only]]")
		}
		if options.booleans["--cancel-active"] && options.booleans["--reconcile-only"] {
			return usagef("agent session attach: --cancel-active and --reconcile-only cannot be combined")
		}
		if options.booleans["--cancel-active"] || options.booleans["--reconcile-only"] {
			options.booleans["--native"] = true
		}
		if options.booleans["--native"] {
			return service.AttachNative(ctx, options.positionals[0], appservice.NativeAttachOptions{
				Input: os.Stdin, Output: stdout, ErrorOutput: stderr,
				CancelActive: options.booleans["--cancel-active"], ReconcileOnly: options.booleans["--reconcile-only"],
			})
		}
		return console.AttachSession(ctx, console.Services{Sessions: service}, options.positionals[0], os.Stdin, stdout)
	default:
		if len(args) != 1 {
			return usagef("usage: agent session <session-id>")
		}
		return showSession(ctx, args[0], stdout, service)
	}
}

func nativeAttachRequested(arguments []string) bool {
	if len(arguments) == 0 || arguments[0] != "attach" {
		return false
	}
	for _, argument := range arguments[1:] {
		if argument == "--native" || argument == "--cancel-active" || argument == "--reconcile-only" {
			return true
		}
	}
	return false
}

func runSessionStart(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--agent": stringOption, "--repo": stringOption, "--role": stringOption,
		"--prompt": stringOption, "--approve-all": boolOption, "--worktree": boolOption, "--allow-shared-checkout": boolOption,
	})
	if err != nil {
		return usagef("agent session start: %s", err)
	}
	if len(options.positionals) != 1 || options.values["--agent"] == "" || options.values["--repo"] == "" {
		return usagef("usage: agent session start <task-id> --agent <name> --repo <path> --role <role> [--worktree] [--allow-shared-checkout] [--prompt <text>] [--approve-all]")
	}
	launched, err := service.Start(ctx, appservice.StartSessionParams{
		TaskID: options.positionals[0], Agent: options.values["--agent"], RepoPath: options.values["--repo"],
		Role: options.values["--role"], Prompt: options.values["--prompt"], ApproveAll: options.booleans["--approve-all"],
		Worktree: options.booleans["--worktree"], AllowSharedCheckout: options.booleans["--allow-shared-checkout"],
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Started %s\nInitial request %s (%s)\n", launched.Session.ID, launched.RequestID, launched.State)
	return nil
}

func showSession(ctx context.Context, id string, stdout io.Writer, service *appservice.SessionService) error {
	current, err := service.Get(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\nTask: %s\nRole: %s\nAgent: %s\nProvider: %s\nStatus: %s\n", current.ID, current.TaskID, valueOrDash(current.Role), current.Agent, valueOrDash(current.Provider), current.Status)
	fmt.Fprintf(stdout, "Permission: %s\nRepo: %s\n", appservice.PermissionLabel(current), valueOrDash(current.RepoPath))
	if current.WorktreePath != "" {
		fmt.Fprintf(stdout, "Worktree: %s\n", current.WorktreePath)
	}
	if current.Branch != "" {
		fmt.Fprintf(stdout, "Branch: %s\n", current.Branch)
	}
	fmt.Fprintf(stdout, "Task Console ID: %s\nacpx record ID: %s\nACP Session ID: %s\nNative Session ID: %s\n", current.ID, valueOrDash(current.ACPXRecordID), valueOrDash(current.ACPSessionID), valueOrDash(current.NativeSessionID))
	fmt.Fprintf(stdout, "Worker PID: %s\nWorker socket: %s\nLog: %s\n", intOrDash(current.WorkerPID), valueOrDash(current.WorkerSocket), valueOrDash(current.LogPath))
	fmt.Fprintf(stdout, "Created: %s\nUpdated: %s\n", displayTime(current.CreatedAt), displayTime(current.UpdatedAt))
	return nil
}

func runSessionLogs(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{"--follow": boolOption, "--raw": boolOption, "--from": stringOption})
	if err != nil {
		return usagef("agent session logs: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: agent session logs <session-id> [--follow] [--raw] [--from <position>]")
	}
	cursor, err := parseCursor(options.values["--from"])
	if err != nil {
		return usagef("agent session logs: %s", err)
	}
	printLine := func(line sessionlog.Line) error {
		fmt.Fprintln(stdout, render.Activity(line, options.booleans["--raw"]))
		return nil
	}
	if options.booleans["--follow"] {
		err := service.Follow(ctx, options.positionals[0], cursor, printLine)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	lines, err := service.History(ctx, options.positionals[0], cursor)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if err := printLine(line); err != nil {
			return err
		}
	}
	return nil
}

func runTaskLogs(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{"--follow": boolOption, "--raw": boolOption})
	if err != nil {
		return usagef("agent task logs: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: agent task logs <task-id> [--follow] [--raw]")
	}
	taskID := options.positionals[0]
	stored, err := service.TaskHistory(ctx, taskID, nil)
	if err != nil {
		return err
	}
	cursors := make(map[string]uint64)
	for _, item := range stored {
		cursors[item.SessionID] = item.Line.Seq
		fmt.Fprintf(stdout, "%s  %s\n", item.SessionID, render.Activity(item.Line, options.booleans["--raw"]))
	}
	if !options.booleans["--follow"] {
		return nil
	}
	err = service.FollowTask(ctx, taskID, cursors, func(item appservice.TaskActivity) error {
		fmt.Fprintf(stdout, "%s  %s\n", item.SessionID, render.Activity(item.Line, options.booleans["--raw"]))
		return nil
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func parseCursor(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid stream position %q", value)
	}
	return cursor, nil
}

func intOrDash(value int) string {
	if value == 0 {
		return "-"
	}
	return strconv.Itoa(value)
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
