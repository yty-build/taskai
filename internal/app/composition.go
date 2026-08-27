package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/tui"
)

const (
	workerStartupTimeout = 5 * time.Second
	workerDialTimeout    = 500 * time.Millisecond
	workerRetryInitial   = 20 * time.Millisecond
	workerRetryMaximum   = 250 * time.Millisecond
)

type systemCommand struct {
	dial  func(context.Context, string) (runtime.ClientPort, error)
	spawn func() error
}

func (command systemCommand) View(ctx context.Context, streams IO) error {
	stateDir, err := ResolveStateDir()
	if err != nil {
		return runtime.ErrEndpointUnsafe
	}
	client, err := command.acquire(ctx, stateDir)
	if err != nil {
		return terminalError(err)
	}

	projectCWD, err := currentProjectCWD()
	if err != nil {
		_ = client.Close()
		return err
	}
	providers, err := newProductionProviders(projectCWD)
	if err != nil {
		_ = client.Close()
		return providerBuildError(err)
	}
	port, err := newTerminalPort(client, providers.registry, connector.AuthIO{
		In: streams.In, Out: streams.Out, Err: streams.Err,
	})
	if err != nil {
		_ = client.Close()
		return terminalError(err)
	}
	model, err := tui.New(tui.Config{
		Context: ctx, Catalog: port, Tasks: port, Providers: port, Runtime: port, Events: port,
	})
	if err != nil {
		_ = client.Close()
		return runtime.ErrTransport
	}

	program := tea.NewProgram(
		model,
		tea.WithContext(ctx),
		tea.WithInput(streams.In),
		tea.WithOutput(streams.Out),
		tea.WithoutSignalHandler(),
	)
	_, programErr := program.Run()
	model.Close()
	clientErr := client.Close()
	if programErr != nil {
		if ctx.Err() != nil {
			return runtime.ErrCancelled
		}
		return runtime.ErrTransport
	}
	return terminalError(clientErr)
}

// Work owns the persistent metadata store, Manager service, Supervisor, and
// restrictive local IPC endpoint. It performs no provider probe,
// authentication, Start, Resume, Replay, or process launch during bootstrap.
func (systemCommand) Work(ctx context.Context) error {
	stateDir, err := ResolveStateDir()
	if err != nil {
		return runtime.ErrEndpointUnsafe
	}
	projectCWD, err := currentProjectCWD()
	if err != nil {
		return err
	}
	metadata, err := store.Open(ctx, stateDir)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return runtime.ErrCancelled
		}
		return runtime.ErrPersistence
	}
	acceptor := &deferredClientAcceptor{}
	server, err := runtime.NewIPCServer(stateDir, acceptor)
	if err != nil {
		_ = metadata.Close()
		return terminalError(err)
	}
	providers, err := newProductionProviders(projectCWD)
	if err != nil {
		_ = server.Close()
		_ = metadata.Close()
		return providerBuildError(err)
	}
	managers, err := manager.NewService(providers.registry)
	if err != nil {
		_ = server.Close()
		_ = metadata.Close()
		return runtime.ErrUnsupported
	}
	supervisor, err := runtime.NewSupervisor(ctx, runtime.Config{
		Store: metadata, Registry: providers.registry, Managers: managers,
		ProjectCWD: projectCWD, Providers: cloneProviderSpecs(providers.catalog),
	})
	if err != nil {
		_ = server.Close()
		_ = metadata.Close()
		return terminalError(err)
	}
	if err := acceptor.install(supervisor); err != nil {
		_ = supervisor.Close()
		_ = server.Close()
		_ = metadata.Close()
		return err
	}

	serveErr := server.Serve(ctx)
	supervisorErr, serverErr, storeErr := closeWorkerResources(
		supervisor.Close, server.Close, metadata.Close,
	)
	if ctx.Err() != nil && (serveErr == nil || errors.Is(serveErr, context.Canceled) ||
		errors.Is(serveErr, context.DeadlineExceeded) || errors.Is(serveErr, runtime.ErrCancelled)) {
		serveErr = nil
	}
	if serveErr != nil || serverErr != nil || supervisorErr != nil {
		return runtime.ErrTransport
	}
	if storeErr != nil {
		return runtime.ErrPersistence
	}
	return nil
}

// closeWorkerResources keeps exclusive endpoint ownership until every live
// Manager tree has stopped and its terminal metadata is reconciled. The Store
// remains available through both Supervisor and endpoint teardown.
func closeWorkerResources(
	closeSupervisor func() error,
	closeServer func() error,
	closeStore func() error,
) (supervisorErr, serverErr, storeErr error) {
	supervisorErr = closeSupervisor()
	serverErr = closeServer()
	storeErr = closeStore()
	return supervisorErr, serverErr, storeErr
}

// deferredClientAcceptor lets the app acquire exclusive endpoint ownership
// before Supervisor bootstrap can reconcile metadata. A racing worker that
// loses the endpoint therefore performs no lifecycle mutation.
type deferredClientAcceptor struct {
	mu      sync.RWMutex
	backend runtime.ClientAcceptor
}

func (acceptor *deferredClientAcceptor) install(backend runtime.ClientAcceptor) error {
	if acceptor == nil || backend == nil {
		return runtime.ErrInvalidRequest
	}
	acceptor.mu.Lock()
	defer acceptor.mu.Unlock()
	if acceptor.backend != nil {
		return runtime.ErrConflict
	}
	acceptor.backend = backend
	return nil
}

func (acceptor *deferredClientAcceptor) OpenClient(id string) (runtime.ClientPort, error) {
	if acceptor == nil {
		return nil, runtime.ErrUnavailable
	}
	acceptor.mu.RLock()
	backend := acceptor.backend
	acceptor.mu.RUnlock()
	if backend == nil {
		return nil, runtime.ErrUnavailable
	}
	return backend.OpenClient(id)
}

var _ runtime.ClientAcceptor = (*deferredClientAcceptor)(nil)

func currentProjectCWD() (string, error) {
	value, err := os.Getwd()
	if err != nil {
		return "", runtime.ErrInvalidRequest
	}
	value = filepath.Clean(value)
	if !filepath.IsAbs(value) {
		return "", runtime.ErrInvalidRequest
	}
	return value, nil
}

func (command systemCommand) acquire(ctx context.Context, stateDir string) (runtime.ClientPort, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, runtime.ErrCancelled
	}
	dial := command.dial
	if dial == nil {
		dial = runtime.Dial
	}
	client, err := dialClientOnce(ctx, stateDir, dial)
	if err == nil {
		return client, nil
	}
	if !retryableDialError(err) {
		return nil, err
	}
	spawn := command.spawn
	if spawn == nil {
		spawn = spawnDetachedWorker
	}
	if err := spawn(); err != nil {
		return nil, runtime.ErrTransport
	}

	waitCtx, cancel := context.WithTimeout(ctx, workerStartupTimeout)
	defer cancel()
	delay := workerRetryInitial
	for {
		client, err = dialClientOnce(waitCtx, stateDir, dial)
		if err == nil {
			return client, nil
		}
		if !retryableDialError(err) {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if ctx.Err() != nil {
				return nil, runtime.ErrCancelled
			}
			return nil, runtime.ErrUnavailable
		case <-timer.C:
		}
		delay = min(delay*2, workerRetryMaximum)
	}
}

func retryableDialError(err error) bool {
	return errors.Is(err, runtime.ErrUnavailable) || errors.Is(err, runtime.ErrNotFound) ||
		errors.Is(err, runtime.ErrEndpointBusy) || errors.Is(err, context.DeadlineExceeded)
}

func dialClientOnce(
	ctx context.Context,
	stateDir string,
	dial func(context.Context, string) (runtime.ClientPort, error),
) (runtime.ClientPort, error) {
	return dialClientWithin(ctx, stateDir, dial, workerDialTimeout)
}

func dialClientWithin(
	ctx context.Context,
	stateDir string,
	dial func(context.Context, string) (runtime.ClientPort, error),
	timeout time.Duration,
) (runtime.ClientPort, error) {
	if ctx == nil || dial == nil || timeout <= 0 {
		return nil, runtime.ErrInvalidRequest
	}
	attempt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := dial(attempt, stateDir)
	// IPC deliberately binds before Supervisor installation and Serve. A
	// viewer can therefore connect while bootstrap is still unable to answer
	// its hello. Runtime reports the bounded child-context expiry as Cancelled;
	// translate only that attempt-local deadline so acquire keeps waiting. A
	// genuine parent cancellation remains terminal and never starts/retries a
	// worker.
	if err != nil && ctx.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded) {
		if client != nil {
			_ = client.Close()
		}
		return nil, context.DeadlineExceeded
	}
	return client, err
}

func spawnDetachedWorker() error {
	executable, err := os.Executable()
	if err != nil || executable == "" || !filepath.IsAbs(executable) {
		return runtime.ErrTransport
	}
	command := exec.Command(executable, hiddenWorkerArgument)
	// Nil standard streams connect the child to the null device. Authentication
	// runs in a current viewer via tea.Exec; the worker must never keep a stale
	// terminal descriptor alive.
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if err := configureDetachedWorker(command); err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return runtime.ErrTransport
	}
	return releaseDetachedProcess(command.Process)
}

type detachedProcess interface {
	Release() error
	Kill() error
	Wait() (*os.ProcessState, error)
}

// releaseDetachedProcess gives ownership to the operating system only after
// Release succeeds. If that transfer fails, TaskAI still owns the child and
// terminates and reaps it so a failed viewer startup cannot leak a worker.
func releaseDetachedProcess(process detachedProcess) error {
	if process == nil {
		return runtime.ErrTransport
	}
	if err := process.Release(); err != nil {
		killErr := process.Kill()
		if killErr == nil || errors.Is(killErr, os.ErrProcessDone) {
			_, _ = process.Wait()
		}
		return runtime.ErrTransport
	}
	return nil
}
