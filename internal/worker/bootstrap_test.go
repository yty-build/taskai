package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestRunSessionRefusesRetainedQuarantinedOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-quarantine", Title: "Quarantined provider"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-quarantine", TaskID: "T-quarantine", Agent: "missing-profile", Provider: "test",
		RepoPath: root, ProviderRef: "provider-exact", Status: session.StatusFailed,
		LogPath: sessionlog.ChannelPath(home.Logs, "S-quarantine"),
	})
	if err != nil {
		t.Fatal(err)
	}
	retainedSocket := filepath.Join(home.Workers, created.ID+".sock")
	retainedLog := sessionlog.ChannelPath(home.Logs, created.ID)
	if _, err := repository.UpdateSessionWorker(ctx, created.ID, 999999, retainedSocket, retainedLog, session.StatusFailed); err != nil {
		t.Fatal(err)
	}

	err = RunSession(ctx, home, repository, created.ID)
	if err == nil || !strings.Contains(err.Error(), "retains worker ownership") {
		t.Fatalf("RunSession quarantine error = %v", err)
	}
	current, loadErr := repository.GetSession(ctx, created.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if current.WorkerPID != 999999 || current.WorkerSocket != retainedSocket || current.ProviderRef != "provider-exact" || current.Status != session.StatusFailed {
		t.Fatalf("RunSession mutated quarantine evidence: %#v", current)
	}
}

func TestRunSessionRefusesOwnerlessSessionWithoutStartingAdmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := config.Home{Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents")}
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-not-admitted", Title: "Not admitted"}); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateSession(ctx, session.CreateParams{
		ID: "S-not-admitted", TaskID: "T-not-admitted", Agent: "missing-profile",
		Provider: "test", ProviderRef: "provider-exact", Status: session.StatusStopped,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = RunSession(ctx, home, repository, created.ID)
	if err == nil || !strings.Contains(err.Error(), "not admitted for worker startup") {
		t.Fatalf("RunSession ownerless stopped error = %v", err)
	}
	current, loadErr := repository.GetSession(ctx, created.ID)
	if loadErr != nil || current.WorkerState() != created.WorkerState() || current.Status != session.StatusStopped {
		t.Fatalf("unadmitted Session was mutated: %#v, %v", current, loadErr)
	}
}

func TestValidateBootstrapProfileRejectsMissingOrChangedProvider(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		want     string
	}{
		{name: "missing persisted provider", want: "no persisted provider"},
		{name: "blank persisted provider", provider: "   ", want: "no persisted provider"},
		{name: "provider adapter changed", provider: "original-provider", want: "crossing provider adapters"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateBootstrapProfile(
				session.Session{ID: "S-profile-drift", Agent: "drift-agent", Provider: test.provider},
				agents.Profile{Name: "drift-agent", Provider: "replacement-provider", Command: "fixture-agent"},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("provider validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateBootstrapCheckoutRejectsRepositoryDrift(t *testing.T) {
	missingCheckout := filepath.Join(t.TempDir(), "not-a-checkout")
	if err := os.MkdirAll(missingCheckout, 0o700); err != nil {
		t.Fatal(err)
	}
	err := validateBootstrapCheckout(context.Background(), session.Session{ID: "S-repo-drift", RepoPath: missingCheckout})
	if err == nil || !strings.Contains(err.Error(), "validate persisted repository") {
		t.Fatalf("repository validation error = %v", err)
	}
}

func TestValidateBootstrapCheckoutRejectsForeignWorktree(t *testing.T) {
	ctx := context.Background()
	repository := initializeBootstrapGitRepository(t, filepath.Join(t.TempDir(), "repository"))
	foreign := initializeBootstrapGitRepository(t, filepath.Join(t.TempDir(), "foreign"))
	err := validateBootstrapCheckout(ctx, session.Session{
		ID: "S-foreign-worktree", RepoPath: repository, WorktreePath: foreign,
	})
	if err == nil || !strings.Contains(err.Error(), "no longer belongs to repository") {
		t.Fatalf("foreign worktree error = %v", err)
	}
}

func TestSpawnReturnsStartingBeforeBootstrapDriftIsReplayable(t *testing.T) {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "acpspawn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	for _, directory := range []string{home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home.Agents, "drift.yaml"), []byte("name: drift-agent\nprovider: replacement-provider\ncommand: fixture-agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoPath := initializeBootstrapGitRepository(t, filepath.Join(root, "repository"))
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-spawn-drift", Title: "Spawn drift"}); err != nil {
		t.Fatal(err)
	}
	const sessionID = "S-spawn-drift"
	if _, err := repository.CreateSession(ctx, session.CreateParams{
		ID: sessionID, TaskID: "T-spawn-drift", Agent: "drift-agent", Provider: "original-provider",
		RepoPath: repoPath, Status: session.StatusStarting, LogPath: sessionlog.ChannelPath(home.Logs, sessionID),
	}); err != nil {
		t.Fatal(err)
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "acpctl-worker-helper")
	script := "#!/bin/sh\nexec \"$ACP_GATEWAY_TEST_BINARY\" -test.v -test.run '^TestRunSessionBootstrapHelperProcess$' -- \"$@\" >\"$AGENT_HOME/helper.log\" 2>&1\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACP_GATEWAY_TEST_BINARY", testBinary)
	t.Setenv("ACP_GATEWAY_BOOTSTRAP_HELPER", "1")
	spawnContext, cancelSpawn := context.WithTimeout(ctx, 5*time.Second)
	spawned, err := Spawn(spawnContext, wrapper, home, sessionID)
	cancelSpawn()
	if err != nil {
		detail, _ := os.ReadFile(filepath.Join(home.Root, "helper.log"))
		t.Fatalf("%v: %s", err, detail)
	}
	if spawned.PID <= 0 || spawned.Socket != filepath.Join(home.Workers, sessionID+".sock") || spawned.PortReady {
		t.Fatalf("Spawn admission = %#v, want admitted starting worker", spawned)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		current, loadErr := repository.GetSession(ctx, sessionID)
		if loadErr == nil && current.Status == session.StatusFailed && current.WorkerPID == 0 && current.WorkerSocket == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bootstrap drift was not persisted after Spawn admission: %#v, %v", current, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines, err := sessionlog.ReadFile(sessionlog.ChannelPath(home.Logs, sessionID), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := requestLineTypes(lines), []string{FrameConnection, FrameWorkerError, FrameConnection}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Spawn bootstrap drift frame order = %v, want %v", got, want)
	}
	if strings.Contains(string(lines[1].Data), "original-provider") || strings.Contains(string(lines[1].Data), "replacement-provider") {
		t.Fatalf("Spawn bootstrap failure leaked private adapter detail: %s", lines[1].Data)
	}
}

func TestRunSessionBootstrapHelperProcess(t *testing.T) {
	if os.Getenv("ACP_GATEWAY_BOOTSTRAP_HELPER") != "1" {
		return
	}
	var sessionID string
	for index, argument := range os.Args {
		if argument == "--session" && index+1 < len(os.Args) {
			sessionID = os.Args[index+1]
			break
		}
	}
	if sessionID == "" {
		t.Fatal("helper Session ID is missing")
	}
	home, err := config.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	repository, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := RunSession(context.Background(), home, repository, sessionID); err == nil {
		t.Fatal("helper worker unexpectedly survived bootstrap drift")
	} else {
		t.Logf("helper worker stopped: %v", err)
	}
}

func initializeBootstrapGitRepository(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "-b", "main")
	command.Dir = path
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init %q: %v: %s", path, err, output)
	}
	resolved, err := gitrepo.ValidateRepository(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
