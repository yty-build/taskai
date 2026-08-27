//go:build darwin || linux

package runtime

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type unixLocalEndpoint struct {
	listener *net.UnixListener
	lock     *os.File

	socketPath string
	lockPath   string
	socketInfo os.FileInfo
	lockInfo   os.FileInfo

	interruptOnce sync.Once
	interruptErr  error
	closeOnce     sync.Once
	closeErr      error
}

func listenLocalEndpoint(stateDir string) (localEndpoint, error) {
	socketPath, lockPath, err := validateEndpointPaths(stateDir)
	if err != nil {
		return nil, err
	}
	lock, lockInfo, err := acquireEndpointLock(lockPath)
	if err != nil {
		return nil, err
	}
	cleanupLock := func() {
		_ = removeExactEndpoint(lockPath, lockInfo)
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}
	if err := removeStaleSocket(socketPath); err != nil {
		cleanupLock()
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		cleanupLock()
		return nil, ErrEndpointUnsafe
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		cleanupLock()
		return nil, ErrEndpointUnsafe
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil || validateEndpointInfo(socketInfo, endpointSocket) != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		cleanupLock()
		return nil, ErrEndpointUnsafe
	}
	return &unixLocalEndpoint{
		listener: listener, lock: lock,
		socketPath: socketPath, lockPath: lockPath,
		socketInfo: socketInfo, lockInfo: lockInfo,
	}, nil
}

func (endpoint *unixLocalEndpoint) Accept() (net.Conn, error) {
	connection, err := endpoint.listener.AcceptUnix()
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func (endpoint *unixLocalEndpoint) Interrupt() error {
	endpoint.interruptOnce.Do(func() { endpoint.interruptErr = endpoint.listener.Close() })
	return classifyEndpointCleanup(endpoint.interruptErr)
}

func (endpoint *unixLocalEndpoint) Close() error {
	endpoint.closeOnce.Do(func() {
		listenerErr := endpoint.Interrupt()
		socketErr := removeExactEndpoint(endpoint.socketPath, endpoint.socketInfo)
		lockRemoveErr := removeExactEndpoint(endpoint.lockPath, endpoint.lockInfo)
		unlockErr := unix.Flock(int(endpoint.lock.Fd()), unix.LOCK_UN)
		lockCloseErr := endpoint.lock.Close()
		endpoint.closeErr = errors.Join(
			classifyEndpointCleanup(listenerErr), classifyEndpointCleanup(socketErr),
			classifyEndpointCleanup(lockRemoveErr), classifyEndpointCleanup(unlockErr),
			classifyEndpointCleanup(lockCloseErr),
		)
	})
	return endpoint.closeErr
}

func validateEndpointPaths(stateDir string) (string, string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir ||
		len(stateDir) > MaxSocketPathBytes || strings.IndexByte(stateDir, 0) >= 0 {
		return "", "", ErrEndpointUnsafe
	}
	info, err := os.Lstat(stateDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		endpointUID(info) != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return "", "", ErrEndpointUnsafe
	}
	socketPath := filepath.Join(stateDir, SocketFilename)
	lockPath := filepath.Join(stateDir, LockFilename)
	pathLimit := 107
	if runtime.GOOS == "darwin" {
		pathLimit = 103
	}
	if len(socketPath) > pathLimit || len(socketPath) > MaxSocketPathBytes || len(lockPath) > MaxSocketPathBytes {
		return "", "", ErrEndpointUnsafe
	}
	return socketPath, lockPath, nil
}

func acquireEndpointLock(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, nil, ErrEndpointUnsafe
		}
		return nil, nil, ErrEndpointUnsafe
	}
	file := os.NewFile(uintptr(fd), filepath.Base(path))
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, ErrEndpointUnsafe
	}
	fail := func(value error) (*os.File, os.FileInfo, error) {
		_ = file.Close()
		return nil, nil, value
	}
	info, err := file.Stat()
	if err != nil || validateEndpointInfo(info, endpointLock) != nil {
		return fail(ErrEndpointUnsafe)
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(ErrEndpointUnsafe)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail(ErrEndpointBusy)
		}
		return fail(ErrEndpointUnsafe)
	}
	if err := file.Truncate(0); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return fail(ErrEndpointUnsafe)
	}
	info, err = file.Stat()
	if err != nil || validateEndpointInfo(info, endpointLock) != nil || info.Mode().Perm() != 0o600 {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return fail(ErrEndpointUnsafe)
	}
	return file, info, nil
}

type endpointKind uint8

const (
	endpointLock endpointKind = iota + 1
	endpointSocket
)

func validateEndpointInfo(info os.FileInfo, kind endpointKind) error {
	if info == nil || endpointUID(info) != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 {
		return ErrEndpointUnsafe
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return ErrEndpointUnsafe
	}
	switch kind {
	case endpointLock:
		if !info.Mode().IsRegular() {
			return ErrEndpointUnsafe
		}
	case endpointSocket:
		if info.Mode()&os.ModeSocket == 0 {
			return ErrEndpointUnsafe
		}
	default:
		return ErrEndpointUnsafe
	}
	return nil
}

func endpointUID(info os.FileInfo) uint32 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint32(0)
	}
	return stat.Uid
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || validateEndpointInfo(info, endpointSocket) != nil {
		return ErrEndpointUnsafe
	}
	probe, dialErr := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if dialErr == nil {
		peerErr := verifyLocalPeer(probe)
		_ = probe.Close()
		if peerErr != nil {
			return peerErr
		}
		return ErrEndpointBusy
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return ErrEndpointUnsafe
	}
	return removeExactEndpoint(path, info)
}

func removeExactEndpoint(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || expected == nil || !os.SameFile(current, expected) {
		return ErrEndpointUnsafe
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrEndpointUnsafe
	}
	return nil
}

func classifyEndpointCleanup(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if errors.Is(err, ErrEndpointUnsafe) {
		return err
	}
	return ErrTransport
}

func dialLocalEndpoint(ctx context.Context, stateDir string) (net.Conn, error) {
	if _, err := os.Lstat(stateDir); errors.Is(err, os.ErrNotExist) &&
		filepath.IsAbs(stateDir) && filepath.Clean(stateDir) == stateDir &&
		len(stateDir) <= MaxSocketPathBytes && strings.IndexByte(stateDir, 0) < 0 {
		return nil, ErrUnavailable
	}
	socketPath, lockPath, err := validateEndpointPaths(stateDir)
	if err != nil {
		return nil, err
	}
	lockInfo, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnavailable
	}
	if err != nil || validateEndpointInfo(lockInfo, endpointLock) != nil {
		return nil, ErrEndpointUnsafe
	}
	socketInfo, err := os.Lstat(socketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrUnavailable
		}
		return nil, ErrEndpointUnsafe
	}
	if validateEndpointInfo(socketInfo, endpointSocket) != nil {
		return nil, ErrEndpointUnsafe
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrCancelled
		}
		return nil, ErrUnavailable
	}
	if err := verifyLocalPeer(connection); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}
