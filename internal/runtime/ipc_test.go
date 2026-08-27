//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestIPCRoundTripUsesOnePersistentViewerAndBoundedActions(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	backend := newIPCTestBackend()
	server, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	assertIPCMode(t, filepath.Join(stateDir, SocketFilename), os.ModeSocket, 0o600)
	assertIPCMode(t, filepath.Join(stateDir, LockFilename), 0, 0o600)

	serveDone := make(chan error, 1)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	go func() { serveDone <- server.Serve(serveCtx) }()

	client, err := Dial(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != "task_existing" || snapshot.StreamGeneration != 7 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	created, err := client.AddTask(context.Background(), "new title")
	if err != nil || created != "task_created" {
		t.Fatalf("AddTask = %q, %v", created, err)
	}
	if err := client.Attach(context.Background(), "task_existing"); err != nil {
		t.Fatal(err)
	}
	backend.port.events <- Event{
		Generation: 7, Sequence: 12, Kind: EventSessionUpsert,
		TaskID: "task_existing", SessionID: "manager_existing",
		Session: Session{ID: "manager_existing", TaskID: "task_existing", Role: session.RoleManager,
			ProviderID: "codex", State: session.StateActive},
	}
	event, err := client.Next(context.Background(), Cursor{Generation: 7, After: 11})
	if err != nil || event.Sequence != 12 || event.SessionID != "manager_existing" {
		t.Fatalf("Next = %#v, %v", event, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, backend.port.closed)
	backend.mu.Lock()
	opened := backend.opened
	backend.mu.Unlock()
	if opened != 1 {
		t.Fatalf("OpenClient calls = %d, want one persistent viewer", opened)
	}
	backend.port.mu.Lock()
	gotTitle, gotTask := backend.port.addedTitle, backend.port.attachedTask
	backend.port.mu.Unlock()
	if gotTitle != "new title" || gotTask != "task_existing" {
		t.Fatalf("actions = title %q task %q", gotTitle, gotTask)
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	for _, name := range []string{SocketFilename, LockFilename} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains after Close: %v", name, err)
		}
	}
}

func TestIPCConstructorOwnsEndpointBeforeBackendOrServe(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	backend := newIPCTestBackend()
	first, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := NewIPCServer(stateDir, backend); !errors.Is(err, ErrEndpointBusy) {
		t.Fatalf("second constructor = %v, want endpoint busy", err)
	}
	backend.mu.Lock()
	opened := backend.opened
	backend.mu.Unlock()
	if opened != 0 {
		t.Fatalf("backend opened before Serve/Dial: %d", opened)
	}
}

func TestIPCDialAbsentStateDirIsUnavailable(t *testing.T) {
	stateDir := filepath.Join(shortIPCStateDir(t), "missing")
	client, err := Dial(context.Background(), stateDir)
	if client != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Dial absent state directory = %#v, %v, want unavailable", client, err)
	}
}

func TestIPCServeCancellationRetainsOwnershipUntilExplicitClose(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	backend := newIPCTestBackend()
	server, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(serveCtx) }()
	client, err := Dial(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	cancelServe()
	if err := <-serveDone; !errors.Is(err, ErrCancelled) {
		t.Fatalf("Serve after cancellation = %v", err)
	}
	if _, err := NewIPCServer(stateDir, backend); !errors.Is(err, ErrEndpointBusy) {
		t.Fatalf("successor before explicit Close = %v, want endpoint busy", err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	successor, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatalf("successor after Close = %v", err)
	}
	if err := successor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIPCRecoversOnlyExactSecureStaleSocket(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	socketPath := filepath.Join(stateDir, SocketFilename)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	server, err := NewIPCServer(stateDir, newIPCTestBackend())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	assertIPCMode(t, socketPath, os.ModeSocket, 0o600)
}

func TestIPCRejectsSymlinkedLockWithoutTouchingTarget(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	target := filepath.Join(stateDir, "target")
	canary := []byte("lock-target-canary")
	if err := os.WriteFile(target, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(stateDir, LockFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIPCServer(stateDir, newIPCTestBackend()); !errors.Is(err, ErrEndpointUnsafe) {
		t.Fatalf("NewIPCServer = %v, want unsafe endpoint", err)
	}
	if client, err := Dial(context.Background(), stateDir); client != nil || !errors.Is(err, ErrEndpointUnsafe) {
		t.Fatalf("Dial symlinked lock = %#v, %v", client, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, canary) {
		t.Fatalf("target = %q, %v", got, err)
	}
}

func TestIPCRejectsSymlinkedStateDirectory(t *testing.T) {
	parent := shortIPCStateDir(t)
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "state-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIPCServer(link, newIPCTestBackend()); !errors.Is(err, ErrEndpointUnsafe) {
		t.Fatalf("symlinked state directory = %v", err)
	}
}

func TestIPCDialRejectsSocketPermissionReplacement(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	server, err := NewIPCServer(stateDir, newIPCTestBackend())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	socketPath := filepath.Join(stateDir, SocketFilename)
	if err := os.Chmod(socketPath, 0o666); err != nil {
		t.Fatal(err)
	}
	if client, err := Dial(context.Background(), stateDir); client != nil || !errors.Is(err, ErrEndpointUnsafe) {
		t.Fatalf("Dial permissive socket = %#v, %v", client, err)
	}
}

func TestIPCNextCancellationReleasesPersistentViewer(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	backend := newIPCTestBackend()
	backend.port.nextEntered = make(chan struct{})
	server, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()
	client, err := Dial(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	nextDone := make(chan error, 1)
	go func() {
		_, nextErr := client.Next(ctx, Cursor{Generation: 7, After: 11})
		nextDone <- nextErr
	}()
	<-backend.port.nextEntered
	cancel()
	if err := <-nextDone; !errors.Is(err, ErrCancelled) {
		t.Fatalf("Next cancellation = %v", err)
	}
	awaitSignal(t, backend.port.closed)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestPeerVerificationFailsClosedForNonUnixConnection(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	if err := verifyLocalPeer(left); !errors.Is(err, ErrPeerUnauthorized) {
		t.Fatalf("verify non-Unix peer = %v", err)
	}
}

func TestIPCWireBoundsStrictJSONAndRequestIDs(t *testing.T) {
	var oversized bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(MaxWireMessageBytes+1))
	oversized.Write(header[:])
	if err := readWireFrame(&oversized, &wireHello{}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("oversized frame = %v", err)
	}

	var unknown bytes.Buffer
	if err := writeWireFrame(&unknown, struct {
		Protocol int    `json:"protocol"`
		Role     string `json:"role"`
		ClientID string `json:"client_id"`
		Extra    bool   `json:"extra"`
	}{Protocol: ipcProtocolVersion, Role: ipcRoleEvents, ClientID: "00000000000000000000000000000000", Extra: true}); err != nil {
		t.Fatal(err)
	}
	if err := readWireFrame(&unknown, &wireHello{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown JSON field = %v", err)
	}

	for value := uint64(1); value <= 3; value++ {
		id := formatRequestID(value)
		parsed, err := parseRequestID(id)
		if err != nil || parsed != value || len(id) != ipcRequestIDWidth {
			t.Fatalf("request ID %q = %d, %v", id, parsed, err)
		}
	}
	for _, invalid := range []string{"", "0000000000000000", "000000000000000g", "1"} {
		if _, err := parseRequestID(invalid); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid request ID %q = %v", invalid, err)
		}
	}
}

func TestIPCSafeErrorRoundTripDoesNotExposeRawError(t *testing.T) {
	stateDir := shortIPCStateDir(t)
	backend := newIPCTestBackend()
	backend.port.addTaskErr = ErrConflict
	server, err := NewIPCServer(stateDir, backend)
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()
	client, err := Dial(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddTask(context.Background(), "content-canary"); !errors.Is(err, ErrConflict) {
		t.Fatalf("AddTask error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func shortIPCStateDir(t *testing.T) string {
	t.Helper()
	value, err := os.MkdirTemp("/tmp", "taskai-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(value, 0o700); err != nil {
		_ = os.RemoveAll(value)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(value) })
	return value
}

func assertIPCMode(t *testing.T, path string, kind os.FileMode, permission os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if kind != 0 && info.Mode()&kind == 0 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
	if kind == 0 && !info.Mode().IsRegular() {
		t.Fatalf("%s is not regular: %v", path, info.Mode())
	}
	if info.Mode().Perm() != permission {
		t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), permission)
	}
}

func awaitSignal(t *testing.T, value <-chan struct{}) {
	t.Helper()
	select {
	case <-value:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deterministic signal")
	}
}

type ipcTestBackend struct {
	mu     sync.Mutex
	opened int
	port   *ipcTestPort
}

func newIPCTestBackend() *ipcTestBackend {
	return &ipcTestBackend{port: &ipcTestPort{events: make(chan Event, 1), closed: make(chan struct{})}}
}

func (backend *ipcTestBackend) OpenClient(string) (ClientPort, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.opened++
	return backend.port, nil
}

type ipcTestPort struct {
	mu           sync.Mutex
	addedTitle   string
	attachedTask task.ID
	addTaskErr   error
	events       chan Event
	nextEntered  chan struct{}
	nextOnce     sync.Once
	closed       chan struct{}
	closeOnce    sync.Once
}

func (port *ipcTestPort) Snapshot(context.Context) (Snapshot, error) {
	return Snapshot{
		Tasks:            []Task{{ID: "task_existing", Title: "existing"}},
		StreamGeneration: 7, ThroughSequence: 11,
	}, nil
}

func (port *ipcTestPort) AddTask(_ context.Context, title string) (task.ID, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.addedTitle = title
	return "task_created", port.addTaskErr
}

func (*ipcTestPort) AddManager(context.Context, task.ID, settings.ProviderID, string) (session.ID, error) {
	return "manager_created", nil
}

func (*ipcTestPort) SetEnabled(context.Context, settings.ProviderID, bool) error { return nil }
func (*ipcTestPort) Authenticate(context.Context, settings.ProviderID) error     { return nil }
func (*ipcTestPort) Recheck(context.Context, settings.ProviderID) error          { return nil }

func (port *ipcTestPort) Attach(_ context.Context, id task.ID) error {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.attachedTask = id
	return nil
}

func (*ipcTestPort) Detach(context.Context, task.ID) error          { return nil }
func (*ipcTestPort) Send(context.Context, session.ID, string) error { return nil }
func (*ipcTestPort) Resume(context.Context, session.ID) error       { return nil }
func (*ipcTestPort) Stop(context.Context, session.ID) error         { return nil }

func (port *ipcTestPort) Next(ctx context.Context, _ Cursor) (Event, error) {
	if port.nextEntered != nil {
		port.nextOnce.Do(func() { close(port.nextEntered) })
	}
	select {
	case value := <-port.events:
		return value, nil
	case <-ctx.Done():
		return Event{}, ErrCancelled
	}
}

func (port *ipcTestPort) Close() error {
	port.closeOnce.Do(func() { close(port.closed) })
	return nil
}
