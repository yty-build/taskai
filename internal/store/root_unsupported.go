//go:build !darwin && !linux

package store

import (
	"fmt"
	"runtime"
)

// Other platforms compile but fail closed until their filesystem and SQLite
// boundaries have descriptor-relative conformance equivalent to Darwin/Linux.
type stateRoot struct{}

func openStateRoot(string) (*stateRoot, error) {
	return nil, fmt.Errorf("%w: descriptor-pinned state roots are unavailable on %s", ErrUnsafeStatePath, runtime.GOOS)
}

func (*stateRoot) prepareSQLiteFiles() error { return ErrUnsafeStatePath }
func (*stateRoot) secureFiles() error        { return ErrUnsafeStatePath }
func (*stateRoot) registerVFS() (string, error) {
	return "", ErrUnsafeStatePath
}
func (*stateRoot) unregisterVFS() error { return nil }
func (*stateRoot) Close() error         { return nil }
