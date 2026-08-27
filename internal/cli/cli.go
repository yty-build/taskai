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
	"sync"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

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
  acpctl <command> [arguments]

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

Provider-native terminals are invoked directly and remain outside managed Sessions.
`

// Run executes the command-line application and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	stderr = newTerminalSafeWriter(stderr)
	if !stdoutCarriesRawOrInteractiveTerminal(args) {
		stdout = newTerminalSafeWriter(stdout)
	}
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
		return fail(stderr, ExitFailure, fmt.Errorf("resolve acpctl executable: %w", executableErr))
	}
	runtime := sessionRuntimeFactory(home, executable)

	taskService := task.NewService(database)
	switch args[0] {
	case "_worker":
		err = runHiddenWorker(ctx, args[1:], home, database)
	case "tasks":
		err = runTasks(ctx, args[1:], stdout, taskService)
	case "task":
		var registry *agentregistry.Registry
		if len(args) > 1 && args[1] == "attach" {
			registry, err = agentregistry.Load(home.Agents)
			if err != nil {
				break
			}
		}
		sessionService := appservice.NewSessionService(database, registry, home, runtime)
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
		if len(args) > 1 && (args[1] == "start" || args[1] == "attach") {
			registry, err = agentregistry.Load(home.Agents)
			if err != nil {
				break
			}
		}
		sessionService := appservice.NewSessionService(database, registry, home, runtime)
		err = runSession(ctx, args[1:], stdout, sessionService)
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
		err = usagef("unknown command %q; run 'acpctl help' for usage", strings.Join(args, " "))
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

// terminalSafeWriter is the non-raw CLI output boundary. It preserves the
// CLI's own line/tab layout while preventing dynamic values from emitting
// terminal controls, including Unicode format and bidi characters. Raw JSONL
// and the interactive console have their own explicit rendering contracts.
type terminalSafeWriter struct {
	mu          sync.Mutex
	destination io.Writer
	pending     []byte
}

func newTerminalSafeWriter(destination io.Writer) io.Writer {
	return &terminalSafeWriter{destination: destination}
}

func (writer *terminalSafeWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()

	originalLength := len(value)
	input := append([]byte(nil), writer.pending...)
	input = append(input, value...)
	writer.pending = writer.pending[:0]
	var safe strings.Builder
	for len(input) > 0 {
		if !utf8.FullRune(input) {
			writer.pending = append(writer.pending, input...)
			break
		}
		r, size := utf8.DecodeRune(input)
		input = input[size:]
		if r == utf8.RuneError && size == 1 {
			continue
		}
		switch r {
		case '\n', '\t':
			safe.WriteRune(r)
		case '\r':
			safe.WriteByte(' ')
		default:
			if unicode.IsPrint(r) {
				safe.WriteRune(r)
			}
		}
	}
	if safe.Len() == 0 {
		return originalLength, nil
	}
	written, err := io.WriteString(writer.destination, safe.String())
	if err != nil {
		return 0, err
	}
	if written != safe.Len() {
		return 0, io.ErrShortWrite
	}
	return originalLength, nil
}

func stdoutCarriesRawOrInteractiveTerminal(args []string) bool {
	if len(args) >= 2 && ((args[0] == "session" && args[1] == "attach") || (args[0] == "task" && args[1] == "attach")) {
		return true
	}
	if len(args) < 2 || !((args[0] == "session" || args[0] == "task") && args[1] == "logs") {
		return false
	}
	for _, argument := range args[2:] {
		if argument == "--raw" {
			return true
		}
	}
	return false
}

func runServe(ctx context.Context, args []string, stdout io.Writer, tasks *task.Service, sessions *appservice.SessionService, registry *agentregistry.Registry) error {
	options, err := parseOptions(args, map[string]optionKind{"--listen": stringOption})
	if err != nil || len(options.positionals) != 0 {
		return usagef("usage: acpctl serve [--listen 127.0.0.1:8080]")
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
		return usagef("usage: acpctl _worker --session <session-id>")
	}
	return worker.RunSession(ctx, home, database, options.values["--session"])
}

func fail(stderr io.Writer, code int, err error) int {
	fmt.Fprintf(stderr, "acpctl: %s\n", err)
	return code
}

type commandUsageError struct{ message string }

func (e *commandUsageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &commandUsageError{message: fmt.Sprintf(format, args...)}
}

func runTasks(ctx context.Context, args []string, stdout io.Writer, service *task.Service) error {
	if len(args) != 0 {
		return usagef("usage: acpctl tasks")
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
		return usagef("usage: acpctl task new|status|attach|logs|<task-id>")
	}
	switch args[0] {
	case "new":
		return runTaskNew(ctx, args[1:], stdout, service)
	case "status":
		if len(args) != 3 {
			return usagef("usage: acpctl task status <task-id> <status>")
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
			return usagef("usage: acpctl task attach <task-id>")
		}
		return console.AttachTask(ctx, console.Services{Tasks: service, Sessions: sessions}, args[1], os.Stdin, stdout)
	default:
		if len(args) != 1 {
			return usagef("usage: acpctl task <task-id>")
		}
		return showTask(ctx, args[0], stdout, service)
	}
}

func runTaskNew(ctx context.Context, args []string, stdout io.Writer, service *task.Service) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--description": stringOption, "--external-ref": stringOption, "--metadata-json": stringOption,
	})
	if err != nil {
		return usagef("acpctl task new: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: acpctl task new <title> [--description <text>] [--external-ref <ref>] [--metadata-json <json>]")
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
		"--external-ref": stringOption, "--metadata-json": stringOption, "--approve-all": boolOption,
		"--worktree": boolOption, "--allow-shared-checkout": boolOption,
	})
	if err != nil {
		return usagef("acpctl start: %s", err)
	}
	if len(options.positionals) != 1 || strings.TrimSpace(options.values["--agent"]) == "" || strings.TrimSpace(options.values["--repo"]) == "" {
		return usagef("usage: acpctl start <title> --agent <name> --repo <path> [--role <role>] [--worktree] [--allow-shared-checkout]")
	}
	connected, err := sessions.ConnectTask(ctx,
		task.CreateParams{Title: options.positionals[0], Description: options.values["--description"], ExternalRef: options.values["--external-ref"], MetadataJSON: rawJSON(options.values["--metadata-json"]), Status: task.StatusWorking},
		appservice.StartSessionParams{
			Agent: options.values["--agent"], RepoPath: options.values["--repo"], Role: options.values["--role"],
			ApproveAll: options.booleans["--approve-all"], Worktree: options.booleans["--worktree"], AllowSharedCheckout: options.booleans["--allow-shared-checkout"],
		},
	)
	if err != nil {
		if connected.Task.ID != "" && connected.Session.ID != "" {
			return fmt.Errorf("created Task %s and Session %s with status %s, but its Session Channel is unavailable; inspect it with `acpctl session %s`", connected.Task.ID, connected.Session.ID, connected.Session.Status, connected.Session.ID)
		}
		return err
	}
	fmt.Fprintf(stdout, "Created %s\nConnected %s (%s)\n", connected.Task.ID, connected.Session.ID, connected.Session.Status)
	return nil
}

func runSession(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	if len(args) == 0 {
		return usagef("usage: acpctl session start|attach|prompt|cancel|resume|stop|logs|<session-id>")
	}
	switch args[0] {
	case "start":
		return runSessionStart(ctx, args[1:], stdout, service)
	case "prompt":
		if len(args) != 3 {
			return usagef("usage: acpctl session prompt <session-id> <message>")
		}
		accepted, err := service.Prompt(ctx, args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Accepted %s (%s)\n", accepted.RequestID, accepted.State)
		return nil
	case "cancel":
		if len(args) != 2 {
			return usagef("usage: acpctl session cancel <session-id>")
		}
		if err := service.Cancel(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Cancellation requested for %s\n", args[1])
		return nil
	case "resume":
		options, err := parseOptions(args[1:], map[string]optionKind{"--allow-shared-checkout": boolOption})
		if err != nil || len(options.positionals) != 1 {
			return usagef("usage: acpctl session resume <session-id> [--allow-shared-checkout]")
		}
		resumed, err := service.ResumeWithOptions(ctx, options.positionals[0], options.booleans["--allow-shared-checkout"])
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Resumed %s\n", resumed.ID)
		return nil
	case "stop":
		if len(args) != 2 {
			return usagef("usage: acpctl session stop <session-id>")
		}
		if err := service.Stop(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Stopped %s\n", args[1])
		return nil
	case "logs":
		return runSessionLogs(ctx, args[1:], stdout, service)
	case "attach":
		if len(args) != 2 {
			return usagef("usage: acpctl session attach <session-id>")
		}
		return console.AttachSession(ctx, console.Services{Sessions: service}, args[1], os.Stdin, stdout)
	default:
		if len(args) != 1 {
			return usagef("usage: acpctl session <session-id>")
		}
		return showSession(ctx, args[0], stdout, service)
	}
}

func runSessionStart(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{
		"--agent": stringOption, "--repo": stringOption, "--role": stringOption,
		"--parent-session": stringOption,
		"--approve-all":    boolOption, "--worktree": boolOption, "--allow-shared-checkout": boolOption,
	})
	if err != nil {
		return usagef("acpctl session start: %s", err)
	}
	if len(options.positionals) != 1 || options.values["--agent"] == "" || options.values["--repo"] == "" {
		return usagef("usage: acpctl session start <task-id> --agent <name> --repo <path> --role <role> [--parent-session <session-id>] [--worktree] [--allow-shared-checkout] [--approve-all]")
	}
	connected, err := service.ConnectSession(ctx, appservice.StartSessionParams{
		TaskID: options.positionals[0], Agent: options.values["--agent"], RepoPath: options.values["--repo"],
		ParentSessionID: options.values["--parent-session"],
		Role:            options.values["--role"], ApproveAll: options.booleans["--approve-all"],
		Worktree: options.booleans["--worktree"], AllowSharedCheckout: options.booleans["--allow-shared-checkout"],
	})
	if err != nil {
		if connected.Session.ID != "" {
			return fmt.Errorf("Session %s was created with status %s, but its Session Channel is unavailable; inspect it with `acpctl session %s`", connected.Session.ID, connected.Session.Status, connected.Session.ID)
		}
		return err
	}
	fmt.Fprintf(stdout, "Connected %s (%s)\n", connected.Session.ID, connected.Session.Status)
	return nil
}

func showSession(ctx context.Context, id string, stdout io.Writer, service *appservice.SessionService) error {
	current, err := service.Get(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\nTask: %s\nRole: %s\nAgent: %s\nProvider: %s\nStatus: %s\n", current.ID, current.TaskID, valueOrDash(current.Role), current.Agent, valueOrDash(current.Provider), current.Status)
	if current.ParentSessionID != "" {
		fmt.Fprintf(stdout, "Parent Session: %s\n", current.ParentSessionID)
	}
	fmt.Fprintf(stdout, "Permission: %s\nRepo: %s\n", appservice.PermissionLabel(current), valueOrDash(current.RepoPath))
	if current.WorktreePath != "" {
		fmt.Fprintf(stdout, "Worktree: %s\n", current.WorktreePath)
	}
	if current.Branch != "" {
		fmt.Fprintf(stdout, "Branch: %s\n", current.Branch)
	}
	fmt.Fprintf(stdout, "Provider reference: %s\n", displayProviderReference(current.ProviderRef))
	fmt.Fprintf(stdout, "Worker PID: %s\nWorker socket: %s\nLog: %s\n", intOrDash(current.WorkerPID), valueOrDash(current.WorkerSocket), valueOrDash(current.LogPath))
	fmt.Fprintf(stdout, "Created: %s\nUpdated: %s\n", displayTime(current.CreatedAt), displayTime(current.UpdatedAt))
	return nil
}

func runSessionLogs(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{"--follow": boolOption, "--raw": boolOption, "--from": stringOption})
	if err != nil {
		return usagef("acpctl session logs: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: acpctl session logs <session-id> [--follow] [--raw] [--from <position>]")
	}
	cursor, err := parseCursor(options.values["--from"])
	if err != nil {
		return usagef("acpctl session logs: %s", err)
	}
	printLine := func(line sessionlog.Line) error {
		fmt.Fprintln(stdout, render.Activity(line, options.booleans["--raw"]))
		return nil
	}
	if options.booleans["--follow"] {
		if cursor == 0 {
			var clipped bool
			cursor, clipped, err = service.FreshReplayCursor(ctx, options.positionals[0], 256)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Replay window: cursor=%d earliest_sequence=%d clipped=%t\n", cursor, replayEarliest(cursor), clipped)
		}
		err := service.Follow(ctx, options.positionals[0], cursor, printLine)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	_, err = service.Replay(ctx, options.positionals[0], cursor, printLine)
	return err
}

func runTaskLogs(ctx context.Context, args []string, stdout io.Writer, service *appservice.SessionService) error {
	options, err := parseOptions(args, map[string]optionKind{"--follow": boolOption, "--raw": boolOption})
	if err != nil {
		return usagef("acpctl task logs: %s", err)
	}
	if len(options.positionals) != 1 {
		return usagef("usage: acpctl task logs <task-id> [--follow] [--raw]")
	}
	taskID := options.positionals[0]
	printActivity := func(item appservice.TaskActivity) error {
		fmt.Fprintf(stdout, "%s  %s\n", item.SessionID, render.Activity(item.Line, options.booleans["--raw"]))
		return nil
	}
	if options.booleans["--follow"] {
		cursors, windows, windowErr := service.TaskReplayWindows(ctx, taskID, nil, 256)
		if windowErr != nil {
			return windowErr
		}
		sessionIDs := make([]string, 0, len(windows))
		for sessionID := range windows {
			sessionIDs = append(sessionIDs, sessionID)
		}
		sort.Strings(sessionIDs)
		for _, sessionID := range sessionIDs {
			window := windows[sessionID]
			fmt.Fprintf(stdout, "%s  Replay window: cursor=%d earliest_sequence=%d clipped=%t\n", sessionID, window.Cursor, window.EarliestSequence, window.Clipped)
		}
		err = service.FollowTask(ctx, taskID, cursors, printActivity)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return service.ReplayTask(ctx, taskID, nil, printActivity)
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

func replayEarliest(cursor uint64) uint64 {
	if cursor == ^uint64(0) {
		return cursor
	}
	return cursor + 1
}

func intOrDash(value int) string {
	if value == 0 {
		return "-"
	}
	return strconv.Itoa(value)
}

func runAgents(args []string, stdout io.Writer, registry *agentregistry.Registry) error {
	if len(args) > 1 {
		return usagef("usage: acpctl agents [name]")
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

func displayProviderReference(value string) string {
	if value == "" {
		return "-"
	}
	return strconv.QuoteToASCII(value)
}

func valueOrProviderDefault(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Provider default"
	}
	return value
}
