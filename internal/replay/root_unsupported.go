//go:build !darwin && !linux

package replay

import (
	"errors"
	"os"
)

func rootPinningSupported() bool { return false }

func openPinnedRoot(string) (*os.Root, error) {
	return nil, errors.New("descriptor-pinned provider roots are unavailable")
}
