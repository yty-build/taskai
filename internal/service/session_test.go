package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type channelRuntime struct {
	mu                 sync.Mutex
	repo               *store.Store
	home               config.Home
	spawns             int
	sockets            map[string]string
	commands           []worker.Command
	active             bool
	protocolVersion    int
	workerStatus       session.Status
	catalog            acp.ProviderOptionCatalog
	permissions        []worker.PendingPermission
	permissionSequence uint64
	spawnHook          func(context.Context, string) (worker.SpawnResult, error)
}

type casHookRepository struct {
	*store.Store
	mu     sync.Mutex
	before func(context.Context, string, session.WorkerState, session.WorkerState)
}

func (r *casHookRepository) CompareAndSwapSessionWorker(ctx context.Context, id string, before, after session.WorkerState, logPath string) (session.Session, bool, error) {
	r.mu.Lock()
	hook := r.before
	r.before = nil
	r.mu.Unlock()
	if hook != nil {
		hook(ctx, id, before, after)
	}
	return r.Store.CompareAndSwapSessionWorker(ctx, id, before, after, logPath)
}

func (r *channelRuntime) Spawn(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
	r.mu.Lock()
	r.spawns++
	spawnHook := r.spawnHook
	pid := 7000 + r.spawns
	socket := filepath.Join(r.home.Workers, sessionID+".sock")
	if spawnHook == nil {
		r.sockets[socket] = sessionID
	}
	r.mu.Unlock()
	if spawnHook != nil {
		return spawnHook(ctx, sessionID)
	}
	if _, err := r.repo.UpdateSessionProviderRef(ctx, sessionID, "record-"+sessionID, "provider-"+sessionID); err != nil {
		return worker.SpawnResult{}, err
	}
	if _, err := r.repo.UpdateSessionWorker(ctx, sessionID, pid, socket, sessionlog.ChannelPath(r.home.Logs, sessionID), session.StatusWaiting); err != nil {
		return worker.SpawnResult{}, err
	}
	return worker.SpawnResult{PID: pid, Socket: socket, PortReady: true}, nil
}

func (r *channelRuntime) Send(ctx context.Context, socket string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	sessionID, exists := r.sockets[socket]
	r.commands = append(r.commands, command)
	active := r.active
	protocolVersion := r.protocolVersion
	workerStatus := r.workerStatus
	catalog := r.catalog
	permissions := append([]worker.PendingPermission(nil), r.permissions...)
	permissionSequence := r.permissionSequence
	r.mu.Unlock()
	if !exists {
		return worker.Message{}, fmt.Errorf("test provider Port is unavailable")
	}
	switch command.Type {
	case "ping":
		version := protocolVersion
		if version == 0 {
			version = worker.ProtocolVersion
		}
		capabilities := []string{worker.CapabilitySessionChannel}
		if version < worker.ProtocolVersion {
			capabilities = nil
		}
		return worker.Message{Type: "pong", SessionID: sessionID, ProtocolVersion: version, Capabilities: capabilities}, nil
	case "provider_options":
		return worker.Message{Type: "provider_options", ProviderOptions: &catalog}, nil
	case "configure":
		if command.Model != "" {
			catalog.Model.CurrentValue = command.Model
			r.mu.Lock()
			r.catalog.Model.CurrentValue = command.Model
			r.mu.Unlock()
		}
		if command.ReasoningEffort != "" {
			catalog.ReasoningEffort.CurrentValue = command.ReasoningEffort
			r.mu.Lock()
			r.catalog.ReasoningEffort.CurrentValue = command.ReasoningEffort
			r.mu.Unlock()
		}
		return worker.Message{Type: "configured", ProviderOptions: &catalog}, nil
	case "prompt":
		if active {
			return worker.Message{}, &worker.BusyError{SessionID: sessionID, ActiveRequestID: "REQ-active"}
		}
		return worker.Message{Type: "accepted", State: "working", RequestID: command.RequestID}, nil
	case "cancel":
		return worker.Message{Type: "cancel_requested"}, nil
	case "status":
		status := workerStatus
		if status == "" {
			status = session.StatusWaiting
		}
		return worker.Message{Type: "status", Status: string(status), Seq: permissionSequence, Permissions: permissions}, nil
	case "permission_response":
		return worker.Message{Type: "permission_resolved"}, nil
	case "stop":
		if _, err := r.repo.ClearSessionWorker(ctx, sessionID, session.StatusStopped); err != nil {
			return worker.Message{}, err
		}
		r.mu.Lock()
		delete(r.sockets, socket)
		r.mu.Unlock()
		return worker.Message{Type: "stopping"}, nil
	default:
		return worker.Message{}, fmt.Errorf("unexpected Session Channel command %q", command.Type)
	}
}

func (*channelRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
}

type channelHarness struct {
	service *SessionService
	repo    *store.Store
	runtime *channelRuntime
	task    task.Task
	path    string
}

func newChannelHarness(t *testing.T) *channelHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	for _, directory := range []string{home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	taskItem, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-channel", Title: "Session Channel", Description: "stream provider activity"})
	if err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "git", "init", "-b", "main")
	command.Dir = repoPath
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &channelRuntime{
		repo: repository, home: home, sockets: make(map[string]string),
		catalog: acp.ProviderOptionCatalog{
			Model:           acp.ProviderSelectCatalog{Supported: true, CurrentValue: "model-a", Choices: []acp.ProviderOptionChoice{{Value: "model-a", Label: "Model A"}, {Value: "model-b", Label: "Model B"}}},
			ReasoningEffort: acp.ProviderSelectCatalog{Supported: true, CurrentValue: "medium", Choices: []acp.ProviderOptionChoice{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}},
		},
	}
	return &channelHarness{service: NewSessionService(repository, registry, home, runtime), repo: repository, runtime: runtime, task: taskItem, path: repoPath}
}

func (h *channelHarness) connect(t *testing.T, role string, shared bool) ConnectedSession {
	t.Helper()
	connected, err := h.service.ConnectSession(context.Background(), StartSessionParams{
		TaskID: h.task.ID, Agent: "codex", Role: role, RepoPath: h.path, AllowSharedCheckout: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	return connected
}

func TestConnectSessionSpawnFailurePreservesConcurrentStop(t *testing.T) {
	harness := newChannelHarness(t)
	harness.runtime.mu.Lock()
	harness.runtime.spawnHook = func(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
		if _, err := harness.repo.UpdateSessionStatus(ctx, sessionID, session.StatusStopped); err != nil {
			return worker.SpawnResult{}, err
		}
		return worker.SpawnResult{}, errors.New("worker exited after an orderly concurrent Stop")
	}
	harness.runtime.mu.Unlock()

	connected, err := harness.service.ConnectSession(context.Background(), StartSessionParams{
		TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
	})
	if err == nil {
		t.Fatal("ConnectSession unexpectedly succeeded")
	}
	if connected.Session.ID == "" || connected.Session.Status != session.StatusStopped || connected.Session.WorkerPID != 0 || connected.Session.WorkerSocket != "" {
		t.Fatalf("concurrently stopped Session was overwritten: %#v", connected.Session)
	}
	stored, loadErr := harness.repo.GetSession(context.Background(), connected.Session.ID)
	if loadErr != nil || stored.Status != session.StatusStopped {
		t.Fatalf("stored concurrent Stop = %#v, %v", stored, loadErr)
	}
}

func TestStopBeforeWorkerClaimPreventsCreateFromRevivingSession(t *testing.T) {
	harness := newChannelHarness(t)
	spawnEntered := make(chan string, 1)
	releaseSpawn := make(chan struct{})
	harness.runtime.mu.Lock()
	harness.runtime.spawnHook = func(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
		spawnEntered <- sessionID
		<-releaseSpawn
		before := session.WorkerState{Status: session.StatusStarting}
		after := session.WorkerState{Status: session.StatusStarting, PID: 8124, Socket: filepath.Join(harness.runtime.home.Workers, sessionID+".sock")}
		_, swapped, err := harness.repo.CompareAndSwapSessionWorker(ctx, sessionID, before, after, sessionlog.ChannelPath(harness.runtime.home.Logs, sessionID))
		if err != nil {
			return worker.SpawnResult{}, err
		}
		if !swapped {
			return worker.SpawnResult{}, errors.New("worker claim lost to Stop")
		}
		return worker.SpawnResult{PID: after.PID, Socket: after.Socket}, nil
	}
	harness.runtime.mu.Unlock()

	type connectResult struct {
		connected ConnectedSession
		err       error
	}
	result := make(chan connectResult, 1)
	go func() {
		connected, err := harness.service.ConnectSession(context.Background(), StartSessionParams{
			TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
		})
		result <- connectResult{connected: connected, err: err}
	}()
	var sessionID string
	select {
	case sessionID = <-spawnEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Create did not reach worker claim barrier")
	}
	if err := harness.service.Stop(context.Background(), sessionID); err != nil {
		t.Fatalf("Stop before claim: %v", err)
	}
	close(releaseSpawn)
	select {
	case got := <-result:
		if got.err == nil || got.connected.Session.Status != session.StatusStopped {
			t.Fatalf("Create after winning Stop = %#v, %v", got.connected, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Create did not settle after stopped claim")
	}
	stored, err := harness.repo.GetSession(context.Background(), sessionID)
	if err != nil || stored.Status != session.StatusStopped || stored.WorkerPID != 0 || stored.WorkerSocket != "" {
		t.Fatalf("stopped pre-claim Session = %#v, %v", stored, err)
	}
}

func TestStopRetriesWhenWorkerClaimsBetweenReadAndCAS(t *testing.T) {
	harness := newChannelHarness(t)
	params, err := harness.service.PrepareContext(context.Background(), StartSessionParams{
		TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := harness.repo.CreateSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	hooked := &casHookRepository{Store: harness.repo}
	socket := filepath.Join(harness.runtime.home.Workers, created.ID+".sock")
	hooked.before = func(ctx context.Context, id string, before, after session.WorkerState) {
		if before != (session.WorkerState{Status: session.StatusStarting}) || after.Status != session.StatusStopped {
			t.Fatalf("unexpected intercepted transition: %#v -> %#v", before, after)
		}
		owner := session.WorkerState{Status: session.StatusStarting, PID: 8125, Socket: socket}
		if _, swapped, claimErr := harness.repo.CompareAndSwapSessionWorker(ctx, id, before, owner, sessionlog.ChannelPath(harness.runtime.home.Logs, id)); claimErr != nil || !swapped {
			t.Fatalf("interleaved worker claim swapped=%t, err=%v", swapped, claimErr)
		}
		harness.runtime.mu.Lock()
		harness.runtime.sockets[socket] = id
		harness.runtime.mu.Unlock()
	}
	service := NewSessionService(hooked, harness.service.registry, harness.runtime.home, harness.runtime)
	if err := service.Stop(context.Background(), created.ID); err != nil {
		t.Fatalf("Stop after interleaved claim: %v", err)
	}
	stored, err := harness.repo.GetSession(context.Background(), created.ID)
	if err != nil || stored.Status != session.StatusStopped || stored.WorkerPID != 0 || stored.WorkerSocket != "" {
		t.Fatalf("Stop did not settle claimed worker = %#v, %v", stored, err)
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	foundStop := false
	for _, command := range harness.runtime.commands {
		foundStop = foundStop || command.Type == "stop"
	}
	if !foundStop {
		t.Fatalf("Stop returned without addressing interleaved worker: %#v", harness.runtime.commands)
	}
}

func TestConnectTaskSpawnFailurePreservesWorkerQuarantine(t *testing.T) {
	harness := newChannelHarness(t)
	const retainedPID = 8123
	harness.runtime.mu.Lock()
	harness.runtime.spawnHook = func(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
		socket := filepath.Join(harness.runtime.home.Workers, sessionID+".sock")
		if _, err := harness.repo.UpdateSessionWorker(ctx, sessionID, retainedPID, socket, sessionlog.ChannelPath(harness.runtime.home.Logs, sessionID), session.StatusFailed); err != nil {
			return worker.SpawnResult{}, err
		}
		return worker.SpawnResult{}, errors.New("provider teardown was not proven")
	}
	harness.runtime.mu.Unlock()

	connected, err := harness.service.ConnectTask(context.Background(), task.CreateParams{
		ID: "T-spawn-quarantine", Title: "Spawn quarantine",
	}, StartSessionParams{Agent: "codex", Role: "worker", RepoPath: harness.path})
	if err == nil {
		t.Fatal("ConnectTask unexpectedly succeeded")
	}
	if connected.Session.ID == "" || connected.Session.Status != session.StatusFailed || connected.Session.WorkerPID != retainedPID || connected.Session.WorkerSocket == "" {
		t.Fatalf("Spawn quarantine was not preserved: %#v", connected.Session)
	}
	stored, loadErr := harness.repo.GetSession(context.Background(), connected.Session.ID)
	if loadErr != nil || stored.Status != session.StatusFailed || stored.WorkerPID != retainedPID || stored.WorkerSocket == "" {
		t.Fatalf("stored Spawn quarantine = %#v, %v", stored, loadErr)
	}
}

func TestSessionChannelConnectConfigureAndPrompt(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "manager", false)
	if connected.Session.ID == "" || connected.Session.WorkerSocket == "" {
		t.Fatalf("connected Session = %#v", connected.Session)
	}
	if connected.Session.Status != session.StatusStarting {
		t.Fatalf("ConnectSession admission status = %q, want starting", connected.Session.Status)
	}
	if connected.ProviderOptions.Model.Supported || connected.ProviderOptions.ReasoningEffort.Supported {
		t.Fatalf("ConnectSession fabricated provider options before observation: %#v", connected.ProviderOptions)
	}
	discovered, err := harness.service.ProviderOptions(context.Background(), connected.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !discovered.ProviderOptions.Model.Supported || len(discovered.ProviderOptions.Model.Choices) != 2 {
		t.Fatalf("provider catalog = %#v", discovered.ProviderOptions)
	}

	configured, err := harness.service.Configure(context.Background(), connected.Session.ID, session.ProviderOptions{Model: "model-b", ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if configured.ProviderOptions.Model.CurrentValue != "model-b" || configured.ProviderOptions.ReasoningEffort.CurrentValue != "high" {
		t.Fatalf("live provider options = %#v", configured.ProviderOptions)
	}
	configured, err = harness.service.Configure(context.Background(), connected.Session.ID, session.ProviderOptions{ReasoningEffort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if configured.ProviderOptions.Model.CurrentValue != "model-b" || configured.ProviderOptions.ReasoningEffort.CurrentValue != "medium" {
		t.Fatalf("partial live provider options = %#v", configured.ProviderOptions)
	}
	accepted, err := harness.service.Prompt(context.Background(), connected.Session.ID, "what directory are you on?")
	if err != nil || accepted.State != "working" {
		t.Fatalf("Prompt = %#v, %v", accepted, err)
	}

	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	var types []string
	for _, command := range harness.runtime.commands {
		types = append(types, command.Type)
		if command.Type == "prompt" && command.Message != "what directory are you on?" {
			t.Fatalf("provider prompt = %q", command.Message)
		}
	}
	lastConfigure := worker.Command{}
	for _, command := range harness.runtime.commands {
		if command.Type == "configure" {
			lastConfigure = command
		}
	}
	if lastConfigure.Model != "" || lastConfigure.ReasoningEffort != "medium" {
		t.Fatalf("partial configure did not preserve field presence: %#v", lastConfigure)
	}
	joined := strings.Join(types, ",")
	if strings.Contains(joined, "refresh") || strings.Contains(joined, "resume") || strings.Contains(joined, "launch") {
		t.Fatalf("Session Channel used historical activation commands: %s", joined)
	}
	if got := strings.Count(joined, "provider_options"); got != 1 {
		t.Fatalf("provider option discovery count = %d, commands=%s", got, joined)
	}
}

func TestPendingPermissionSnapshotCarriesWorkerSequenceWatermark(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	want := worker.PendingPermission{
		ID: "REQ-permission:tool-1", RequestID: "REQ-permission", Title: "Write file", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-once", Name: "Allow once", Kind: "allow_once"}},
	}
	harness.runtime.mu.Lock()
	harness.runtime.permissions = []worker.PendingPermission{want}
	harness.runtime.permissionSequence = 41
	harness.runtime.mu.Unlock()
	snapshot, err := harness.service.PendingPermissionSnapshot(context.Background(), connected.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sequence != 41 || !snapshot.DecisionsReady || len(snapshot.Permissions) != 1 || snapshot.Permissions[0].ID != want.ID {
		t.Fatalf("permission snapshot = %#v", snapshot)
	}
}

func TestPendingPermissionSnapshotBlocksBeforePortReadinessAndOnOldWorker(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	harness.runtime.mu.Lock()
	harness.runtime.workerStatus = session.StatusStarting
	harness.runtime.permissionSequence = 7
	harness.runtime.mu.Unlock()
	snapshot, err := harness.service.PendingPermissionSnapshot(context.Background(), connected.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DecisionsReady || snapshot.Sequence != 7 || snapshot.BlockedCode != "session_starting" {
		t.Fatalf("starting permission snapshot = %#v", snapshot)
	}

	harness.runtime.mu.Lock()
	harness.runtime.protocolVersion = worker.ProtocolVersion - 1
	commandsBefore := len(harness.runtime.commands)
	harness.runtime.mu.Unlock()
	snapshot, err = harness.service.PendingPermissionSnapshot(context.Background(), connected.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DecisionsReady || snapshot.BlockedCode != "worker_upgrade_required" || len(snapshot.Permissions) != 0 {
		t.Fatalf("old-worker permission snapshot = %#v", snapshot)
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	commandsAfter := harness.runtime.commands[commandsBefore:]
	if len(commandsAfter) != 1 || commandsAfter[0].Type != "ping" {
		t.Fatalf("old-worker snapshot commands = %#v, want protocol ping only", commandsAfter)
	}
}

func TestOwnerlessStartingRejectsEveryProviderCommandWithoutRuntimeSend(t *testing.T) {
	harness := newChannelHarness(t)
	params, err := harness.service.PrepareContext(context.Background(), StartSessionParams{
		TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := harness.repo.CreateSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	assertStarting := func(name string, err error) {
		t.Helper()
		var starting *worker.NotReadyError
		if !errors.As(err, &starting) || starting.SessionID != created.ID {
			t.Fatalf("%s error = %T %v, want NotReadyError", name, err, err)
		}
	}
	_, err = harness.service.ProviderOptions(context.Background(), created.ID)
	assertStarting("provider options", err)
	_, err = harness.service.Configure(context.Background(), created.ID, session.ProviderOptions{Model: "model-a"})
	assertStarting("configure", err)
	_, err = harness.service.Configure(context.Background(), created.ID, session.ProviderOptions{Model: "invalid\nmodel"})
	assertStarting("invalid configure while starting", err)
	_, err = harness.service.Prompt(context.Background(), created.ID, "exact prompt")
	assertStarting("prompt", err)
	assertStarting("cancel", harness.service.Cancel(context.Background(), created.ID))
	assertStarting("permission response", harness.service.DecidePermission(context.Background(), created.ID, "permission-one", "allow-once", false))
	snapshot, err := harness.service.PendingPermissionSnapshot(context.Background(), created.ID)
	if err != nil || snapshot.DecisionsReady || snapshot.BlockedCode != "session_starting" || len(snapshot.Permissions) != 0 {
		t.Fatalf("starting permission snapshot = %#v, %v", snapshot, err)
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	if len(harness.runtime.commands) != 0 {
		t.Fatalf("ownerless starting commands reached runtime: %#v", harness.runtime.commands)
	}
}

func TestResumeRefusesIdentitylessStoppedSessionWithoutSpawning(t *testing.T) {
	harness := newChannelHarness(t)
	params, err := harness.service.PrepareContext(context.Background(), StartSessionParams{
		TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
	})
	if err != nil {
		t.Fatal(err)
	}
	params.Status = session.StatusStopped
	created, err := harness.repo.CreateSession(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	spawnsBefore := harness.runtime.spawns
	harness.runtime.mu.Unlock()
	_, err = harness.service.ResumeWithOptions(context.Background(), created.ID, false)
	if err == nil || !strings.Contains(err.Error(), "no saved provider reference") {
		t.Fatalf("identityless Resume error = %v", err)
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	if harness.runtime.spawns != spawnsBefore {
		t.Fatalf("identityless Resume spawned provider: before=%d after=%d", spawnsBefore, harness.runtime.spawns)
	}
}

func TestQuarantinedWorkerOwnershipStillBlocksCheckoutReuse(t *testing.T) {
	harness := newChannelHarness(t)
	first := harness.connect(t, "worker", false)
	if _, err := harness.repo.UpdateSessionStatus(context.Background(), first.Session.ID, session.StatusFailed); err != nil {
		t.Fatal(err)
	}
	_, err := harness.service.ConnectSession(context.Background(), StartSessionParams{
		TaskID: harness.task.ID, Agent: "codex", Role: "worker", RepoPath: harness.path,
	})
	if err == nil || !strings.Contains(err.Error(), "already used by active Session") {
		t.Fatalf("quarantined checkout reuse error = %v", err)
	}
}

func TestSessionChannelRejectsConcurrentPromptWithoutQueueing(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	harness.runtime.mu.Lock()
	harness.runtime.active = true
	harness.runtime.mu.Unlock()
	_, err := harness.service.Prompt(context.Background(), connected.Session.ID, "second prompt")
	var busy *worker.BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Prompt error = %T %v, want BusyError", err, err)
	}
}

func TestManagerCreatesAndAddressesExactChild(t *testing.T) {
	harness := newChannelHarness(t)
	manager := harness.connect(t, "manager", true)
	child, err := harness.service.ConnectChild(context.Background(), manager.Session.ID, StartSessionParams{
		Agent: "claude", Role: "researcher", AllowSharedCheckout: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.ParentSessionID != manager.Session.ID || child.Session.TaskID != manager.Session.TaskID {
		t.Fatalf("child membership = %#v", child.Session)
	}
	children, err := harness.service.Children(context.Background(), manager.Session.ID)
	if err != nil || len(children) != 1 || children[0].ID != child.Session.ID {
		t.Fatalf("Children = %#v, %v", children, err)
	}
	if _, err := harness.service.PromptChild(context.Background(), manager.Session.ID, child.Session.ID, "investigate the adapter"); err != nil {
		t.Fatal(err)
	}
	other := harness.connect(t, "worker", true)
	if _, err := harness.service.PromptChild(context.Background(), manager.Session.ID, other.Session.ID, "wrong target"); err == nil {
		t.Fatal("PromptChild accepted a non-child Session")
	}
}

func TestProviderOptionsAreObservationalAndResumeIsExplicit(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	if _, err := harness.repo.ClearSessionWorker(context.Background(), connected.Session.ID, session.StatusStopped); err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	for socket, id := range harness.runtime.sockets {
		if id == connected.Session.ID {
			delete(harness.runtime.sockets, socket)
		}
	}
	before := harness.runtime.spawns
	harness.runtime.mu.Unlock()
	if _, err := harness.service.ProviderOptions(context.Background(), connected.Session.ID); err == nil {
		t.Fatal("ProviderOptions restarted a stopped Session")
	}
	harness.runtime.mu.Lock()
	if harness.runtime.spawns != before {
		t.Fatalf("observational ProviderOptions spawned a worker: %d -> %d", before, harness.runtime.spawns)
	}
	harness.runtime.mu.Unlock()
	resumed, err := harness.service.Resume(context.Background(), connected.Session.ID)
	if err != nil || resumed.WorkerSocket == "" || resumed.Status != session.StatusStarting {
		t.Fatalf("Resume = %#v, %v", resumed, err)
	}
}

func TestStopDuringResumeAdmissionPreventsWorkerClaim(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	if _, err := harness.repo.ClearSessionWorker(context.Background(), connected.Session.ID, session.StatusStopped); err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	for socket, id := range harness.runtime.sockets {
		if id == connected.Session.ID {
			delete(harness.runtime.sockets, socket)
		}
	}
	spawnEntered := make(chan struct{}, 1)
	releaseSpawn := make(chan struct{})
	harness.runtime.spawnHook = func(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
		spawnEntered <- struct{}{}
		<-releaseSpawn
		before := session.WorkerState{Status: session.StatusStarting}
		after := session.WorkerState{Status: session.StatusStarting, PID: 8126, Socket: filepath.Join(harness.runtime.home.Workers, sessionID+".sock")}
		_, swapped, err := harness.repo.CompareAndSwapSessionWorker(ctx, sessionID, before, after, sessionlog.ChannelPath(harness.runtime.home.Logs, sessionID))
		if err != nil {
			return worker.SpawnResult{}, err
		}
		if !swapped {
			return worker.SpawnResult{}, errors.New("Resume worker claim lost to Stop")
		}
		return worker.SpawnResult{PID: after.PID, Socket: after.Socket}, nil
	}
	harness.runtime.mu.Unlock()

	type resumeResult struct {
		current session.Session
		err     error
	}
	result := make(chan resumeResult, 1)
	go func() {
		current, err := harness.service.Resume(context.Background(), connected.Session.ID)
		result <- resumeResult{current: current, err: err}
	}()
	select {
	case <-spawnEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Resume did not reach worker claim barrier")
	}
	admitted, err := harness.repo.GetSession(context.Background(), connected.Session.ID)
	if err != nil || admitted.Status != session.StatusStarting || admitted.WorkerPID != 0 || admitted.WorkerSocket != "" || admitted.CanResume() {
		t.Fatalf("Resume admission state = %#v, %v", admitted, err)
	}
	if err := harness.service.Stop(context.Background(), connected.Session.ID); err != nil {
		t.Fatalf("Stop during Resume admission: %v", err)
	}
	close(releaseSpawn)
	select {
	case got := <-result:
		if got.err == nil || got.current.Status != session.StatusStopped || got.current.WorkerPID != 0 || got.current.WorkerSocket != "" {
			t.Fatalf("Resume after winning Stop = %#v, %v", got.current, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resume did not settle after stopped claim")
	}
	stored, err := harness.repo.GetSession(context.Background(), connected.Session.ID)
	if err != nil || stored.Status != session.StatusStopped || stored.ProviderRef == "" || !stored.CanResume() {
		t.Fatalf("stopped resumable Session = %#v, %v", stored, err)
	}
}

func TestPromptNeverImplicitlyResumesStoppedSession(t *testing.T) {
	harness := newChannelHarness(t)
	connected := harness.connect(t, "worker", false)
	if _, err := harness.repo.ClearSessionWorker(context.Background(), connected.Session.ID, session.StatusStopped); err != nil {
		t.Fatal(err)
	}
	harness.runtime.mu.Lock()
	for socket, id := range harness.runtime.sockets {
		if id == connected.Session.ID {
			delete(harness.runtime.sockets, socket)
		}
	}
	before := harness.runtime.spawns
	harness.runtime.mu.Unlock()
	_, err := harness.service.Prompt(context.Background(), connected.Session.ID, "must not auto-resume")
	if err == nil || !strings.Contains(err.Error(), "resume it first") {
		t.Fatalf("stopped Prompt error = %v", err)
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	if harness.runtime.spawns != before {
		t.Fatalf("Prompt implicitly spawned provider: %d -> %d", before, harness.runtime.spawns)
	}
}
