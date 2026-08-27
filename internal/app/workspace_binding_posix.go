//go:build darwin || linux

package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/yty-build/acp-gateway/internal/runtime"
)

const (
	workspaceRootMode    = 0o700
	workspaceBindingMode = 0o600
)

func inspectWorkspaceBindingFile(stateDir string, expected []byte, required bool) error {
	root, err := openWorkspaceBindingRoot(stateDir, false)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	found, err := readWorkspaceBinding(root, expected)
	if err != nil {
		return err
	}
	if !found {
		if required {
			return runtime.ErrEndpointUnsafe
		}
		entry, readErr := root.Readdirnames(1)
		if readErr == nil && len(entry) != 0 {
			return runtime.ErrEndpointUnsafe
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return runtime.ErrEndpointUnsafe
		}
	}
	return nil
}

func establishWorkspaceBindingFile(stateDir string, expected []byte) error {
	root, err := openWorkspaceBindingRoot(stateDir, true)
	if err != nil {
		return err
	}
	defer root.Close()

	if found, readErr := readWorkspaceBinding(root, expected); readErr != nil || found {
		return readErr
	}
	entry, readErr := root.Readdirnames(1)
	if readErr == nil && len(entry) != 0 {
		// Never claim a pre-existing unbound state root. It may contain metadata
		// created for a different project by an older or interrupted runtime.
		return runtime.ErrEndpointUnsafe
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return runtime.ErrEndpointUnsafe
	}

	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(root.Fd()), workspaceBindingFilename, flags, workspaceBindingMode)
	if errors.Is(err, unix.EEXIST) {
		found, readErr := readWorkspaceBinding(root, expected)
		if readErr != nil {
			return readErr
		}
		if !found {
			return runtime.ErrEndpointUnsafe
		}
		return nil
	}
	if err != nil {
		return runtime.ErrEndpointUnsafe
	}
	file := os.NewFile(uintptr(fd), workspaceBindingFilename)
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = unix.Unlinkat(int(root.Fd()), workspaceBindingFilename, 0)
		}
	}()
	if err := unix.Fchmod(fd, workspaceBindingMode); err != nil {
		return runtime.ErrEndpointUnsafe
	}
	if err := validateWorkspaceBindingDescriptor(fd); err != nil {
		return err
	}
	for remaining := expected; len(remaining) != 0; {
		written, writeErr := file.Write(remaining)
		if writeErr != nil || written <= 0 || written > len(remaining) {
			return runtime.ErrEndpointUnsafe
		}
		remaining = remaining[written:]
	}
	if err := file.Sync(); err != nil {
		return runtime.ErrEndpointUnsafe
	}
	if err := unix.Fsync(int(root.Fd())); err != nil {
		return runtime.ErrEndpointUnsafe
	}
	keep = true
	return nil
}

func openWorkspaceBindingRoot(stateDir string, create bool) (*os.File, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		return nil, runtime.ErrEndpointUnsafe
	}
	open := func() (int, error) {
		return unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	fd, err := open()
	if errors.Is(err, unix.ENOENT) && create {
		if mkdirErr := os.MkdirAll(stateDir, workspaceRootMode); mkdirErr != nil {
			return nil, runtime.ErrEndpointUnsafe
		}
		fd, err = open()
	}
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, runtime.ErrEndpointUnsafe
	}
	file := os.NewFile(uintptr(fd), stateDir)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		uint64(stat.Uid) != uint64(os.Geteuid()) {
		_ = file.Close()
		return nil, runtime.ErrEndpointUnsafe
	}
	if create {
		if err := unix.Fchmod(fd, workspaceRootMode); err != nil {
			_ = file.Close()
			return nil, runtime.ErrEndpointUnsafe
		}
	} else if stat.Mode&0o777 != workspaceRootMode {
		_ = file.Close()
		return nil, runtime.ErrEndpointUnsafe
	}
	return file, nil
}

func readWorkspaceBinding(root *os.File, expected []byte) (bool, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(int(root.Fd()), workspaceBindingFilename, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, runtime.ErrEndpointUnsafe
	}
	file := os.NewFile(uintptr(fd), workspaceBindingFilename)
	defer file.Close()
	if err := validateWorkspaceBindingDescriptor(fd); err != nil {
		return false, err
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(len(expected)+1)))
	if err != nil {
		return false, runtime.ErrEndpointUnsafe
	}
	if !bytes.Equal(content, expected) {
		return false, runtime.ErrConflict
	}
	return true, nil
}

func validateWorkspaceBindingDescriptor(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		uint64(stat.Uid) != uint64(os.Geteuid()) || uint64(stat.Nlink) != 1 ||
		stat.Mode&0o777 != workspaceBindingMode {
		return runtime.ErrEndpointUnsafe
	}
	return nil
}
