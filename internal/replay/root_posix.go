//go:build darwin || linux

package replay

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

func rootPinningSupported() bool { return true }

// openPinnedRoot atomically rejects a symlink as the configured root's final
// component, pins the directory descriptor, and gives os.Root an independent
// descriptor through the process descriptor filesystem. A pathname swap after
// the first open cannot retarget subsequent provider-history reads.
func openPinnedRoot(name string) (*os.Root, error) {
	pinned, err := os.OpenFile(name, os.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer pinned.Close()
	pinnedInfo, err := pinned.Stat()
	if err != nil || !pinnedInfo.IsDir() {
		return nil, fmt.Errorf("provider root is not a directory")
	}

	descriptorRoot := fmt.Sprintf("/dev/fd/%d", pinned.Fd())
	if runtime.GOOS == "linux" {
		descriptorRoot = fmt.Sprintf("/proc/self/fd/%d", pinned.Fd())
	}
	root, err := os.OpenRoot(descriptorRoot)
	if err != nil {
		return nil, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(pinnedInfo, rootInfo) {
		root.Close()
		return nil, fmt.Errorf("provider root descriptor changed")
	}
	return root, nil
}
