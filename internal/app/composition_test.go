package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/runtime"
)

func TestProductionWorkerServesMetadataAndViewerCloseOnlyDetaches(t *testing.T) {
	shortRoot, err := os.MkdirTemp("", "taskai-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(shortRoot); removeErr != nil {
			t.Errorf("remove short state fixture: %v", removeErr)
		}
	})
	stateDir := filepath.Join(shortRoot, "state")
	t.Setenv(StateDirEnvironment, stateDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "absent-claude-root"))
	// Worker bootstrap is required to be provider-process-free. An empty PATH
	// makes an accidental eager probe, auth, or connector launch fail the test.
	t.Setenv("PATH", "")

	workerCtx, stopWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- (systemCommand{}).Work(workerCtx) }()
	defer func() {
		stopWorker()
		select {
		case err := <-workerDone:
			if err != nil {
				t.Errorf("worker shutdown error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	}()

	first := awaitWorkerClient(t, stateDir, workerDone)
	defer first.Close()
	snapshot, err := first.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 0 || len(snapshot.Sessions) != 0 || len(snapshot.Providers) != 4 {
		t.Fatalf("first-launch snapshot = %#v", snapshot)
	}
	for _, filename := range []string{runtime.SocketFilename, runtime.LockFilename} {
		info, statErr := os.Lstat(filepath.Join(stateDir, filename))
		if statErr != nil {
			t.Fatalf("stat %s: %v", filename, statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", filename, info.Mode().Perm())
		}
	}
	var terminal bytes.Buffer
	var terminalErrors bytes.Buffer
	viewerCtx, stopViewer := context.WithTimeout(t.Context(), 2*time.Second)
	status := runCommand(viewerCtx, nil, IO{
		In: bytes.NewBufferString("q"), Out: &terminal, Err: &terminalErrors,
	}, systemCommand{})
	stopViewer()
	if status != 0 || terminalErrors.Len() != 0 {
		t.Fatalf("no-args viewer status=%d stderr=%q", status, terminalErrors.String())
	}

	created, err := first.AddTask(t.Context(), "metadata-only task")
	if err != nil || created.Validate() != nil {
		t.Fatalf("AddTask id=%q error=%v", created, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh viewer must attach to the same worker and observe the metadata
	// written before the preceding viewer detached.
	second := awaitWorkerClient(t, stateDir, workerDone)
	defer second.Close()
	snapshot, err = second.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != created || snapshot.Tasks[0].Title != "metadata-only task" {
		t.Fatalf("reattached snapshot tasks = %#v", snapshot.Tasks)
	}
}

func TestWorkerCancelledBeforeBootstrapCreatesNoState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "cancelled-state")
	t.Setenv(StateDirEnvironment, stateDir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := (systemCommand{}).Work(ctx); !errors.Is(err, runtime.ErrCancelled) {
		t.Fatalf("cancelled Work error = %v", err)
	}
	if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled Work created state: %v", err)
	}
}

func awaitWorkerClient(t *testing.T, stateDir string, workerDone chan error) runtime.ClientPort {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	retry := time.NewTimer(0)
	defer retry.Stop()
	for {
		select {
		case <-retry.C:
			if info, err := os.Lstat(stateDir); err == nil && info.IsDir() {
				attemptCtx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
				client, dialErr := runtime.Dial(attemptCtx, stateDir)
				cancel()
				if dialErr == nil {
					return client
				}
				if !errors.Is(dialErr, runtime.ErrUnavailable) && !errors.Is(dialErr, runtime.ErrEndpointBusy) &&
					!errors.Is(dialErr, runtime.ErrCancelled) {
					t.Fatalf("Dial worker: %v", dialErr)
				}
			}
			retry.Reset(5 * time.Millisecond)
		case err := <-workerDone:
			workerDone <- err
			t.Fatalf("worker exited before ready: %v", err)
		case <-deadline.C:
			t.Fatal("worker did not publish its endpoint")
		}
	}
}

func TestAcquireReusesExistingWorkerWithoutSpawn(t *testing.T) {
	want := &portFake{}
	spawned := 0
	command := systemCommand{
		dial:  func(context.Context, string) (runtime.ClientPort, error) { return want, nil },
		spawn: func() error { spawned++; return nil },
	}
	got, err := command.acquire(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != want || spawned != 0 {
		t.Fatalf("client=%p want=%p spawned=%d", got, want, spawned)
	}
}

func TestAcquireStartsOneWorkerAndWaitsForEndpoint(t *testing.T) {
	want := &portFake{}
	dials := 0
	spawned := 0
	command := systemCommand{
		dial: func(context.Context, string) (runtime.ClientPort, error) {
			dials++
			if dials < 3 {
				return nil, runtime.ErrUnavailable
			}
			return want, nil
		},
		spawn: func() error { spawned++; return nil },
	}
	got, err := command.acquire(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != want || spawned != 1 || dials != 3 {
		t.Fatalf("client=%p want=%p spawned=%d dials=%d", got, want, spawned, dials)
	}
}

func TestAcquireFailsClosedWithoutSpawnForUnsafeEndpoint(t *testing.T) {
	spawned := 0
	command := systemCommand{
		dial: func(context.Context, string) (runtime.ClientPort, error) {
			return nil, runtime.ErrEndpointUnsafe
		},
		spawn: func() error { spawned++; return nil },
	}
	if _, err := command.acquire(t.Context(), t.TempDir()); !errors.Is(err, runtime.ErrEndpointUnsafe) {
		t.Fatalf("acquire error = %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned %d workers for unsafe endpoint", spawned)
	}
}

func TestAcquireCancelledBeforeDialDoesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dials := 0
	spawned := 0
	command := systemCommand{
		dial: func(context.Context, string) (runtime.ClientPort, error) {
			dials++
			return nil, runtime.ErrUnavailable
		},
		spawn: func() error { spawned++; return nil },
	}
	if _, err := command.acquire(ctx, t.TempDir()); !errors.Is(err, runtime.ErrCancelled) {
		t.Fatalf("acquire error = %v", err)
	}
	if dials != 0 || spawned != 0 {
		t.Fatalf("cancelled acquire contacted dial=%d spawn=%d", dials, spawned)
	}
}

func TestDialAttemptDeadlineRetriesButParentCancellationDoesNot(t *testing.T) {
	blockedDial := func(ctx context.Context, _ string) (runtime.ClientPort, error) {
		<-ctx.Done()
		return nil, runtime.ErrCancelled
	}
	if _, err := dialClientWithin(t.Context(), t.TempDir(), blockedDial, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || !retryableDialError(err) {
		t.Fatalf("attempt-local deadline error = %v", err)
	}

	parent, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := dialClientWithin(parent, t.TempDir(), blockedDial, time.Second); !errors.Is(err, runtime.ErrCancelled) || retryableDialError(err) {
		t.Fatalf("parent cancellation error = %v", err)
	}
}

type acceptingFake struct {
	calls int
	port  runtime.ClientPort
}

func (fake *acceptingFake) OpenClient(string) (runtime.ClientPort, error) {
	fake.calls++
	return fake.port, nil
}

func TestDeferredClientAcceptorFailsClosedUntilSingleInstall(t *testing.T) {
	acceptor := &deferredClientAcceptor{}
	if _, err := acceptor.OpenClient("viewer"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("pre-install OpenClient error = %v", err)
	}
	port := &portFake{}
	backend := &acceptingFake{port: port}
	if err := acceptor.install(backend); err != nil {
		t.Fatal(err)
	}
	got, err := acceptor.OpenClient("viewer")
	if err != nil || got != port || backend.calls != 1 {
		t.Fatalf("installed OpenClient port=%p err=%v calls=%d", got, err, backend.calls)
	}
	if err := acceptor.install(&acceptingFake{}); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("duplicate install error = %v", err)
	}
}

type detachedProcessFake struct {
	releaseErr error
	killErr    error
	releases   int
	kills      int
	waits      int
}

func (fake *detachedProcessFake) Release() error {
	fake.releases++
	return fake.releaseErr
}

func (fake *detachedProcessFake) Kill() error {
	fake.kills++
	return fake.killErr
}

func (fake *detachedProcessFake) Wait() (*os.ProcessState, error) {
	fake.waits++
	return nil, nil
}

func TestReleaseDetachedProcessCleansUpWhenOwnershipTransferFails(t *testing.T) {
	success := &detachedProcessFake{}
	if err := releaseDetachedProcess(success); err != nil {
		t.Fatal(err)
	}
	if success.releases != 1 || success.kills != 0 || success.waits != 0 {
		t.Fatalf("successful transfer trace = %#v", success)
	}

	failure := &detachedProcessFake{releaseErr: errors.New("release failed")}
	if err := releaseDetachedProcess(failure); !errors.Is(err, runtime.ErrTransport) {
		t.Fatalf("failed transfer error = %v", err)
	}
	if failure.releases != 1 || failure.kills != 1 || failure.waits != 1 {
		t.Fatalf("failed transfer trace = %#v", failure)
	}

	unsafeToWait := &detachedProcessFake{
		releaseErr: errors.New("release failed"), killErr: errors.New("kill failed"),
	}
	if err := releaseDetachedProcess(unsafeToWait); !errors.Is(err, runtime.ErrTransport) {
		t.Fatalf("failed kill error = %v", err)
	}
	if unsafeToWait.releases != 1 || unsafeToWait.kills != 1 || unsafeToWait.waits != 0 {
		t.Fatalf("unsafe wait trace = %#v", unsafeToWait)
	}
}

func TestWorkerCleanupRetainsEndpointUntilSupervisorStops(t *testing.T) {
	var order []string
	record := func(name string, err error) func() error {
		return func() error {
			order = append(order, name)
			return err
		}
	}
	supervisorFailure := errors.New("supervisor close")
	serverFailure := errors.New("server close")
	storeFailure := errors.New("store close")
	supervisorErr, serverErr, storeErr := closeWorkerResources(
		record("supervisor", supervisorFailure),
		record("server", serverFailure),
		record("store", storeFailure),
	)
	if got, want := order, []string{"supervisor", "server", "store"}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
	if !errors.Is(supervisorErr, supervisorFailure) || !errors.Is(serverErr, serverFailure) ||
		!errors.Is(storeErr, storeFailure) {
		t.Fatalf("cleanup errors = %v / %v / %v", supervisorErr, serverErr, storeErr)
	}
}
