//go:build darwin

package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

const (
	darwinOFDSetLock        = 90
	darwinOFDSetLockWait    = 91
	darwinOFDSetLockTimeout = 93
)

type darwinFlockTimeout struct {
	lock    unix.Flock_t
	timeout unix.Timespec
}

func (root *stateRoot) newSharedMemory(flags vfs.OpenFlag) (vfs.SharedMemory, *os.File, error) {
	// Opening /dev/fd duplicates the referenced open file description. Give
	// each SQLite main-file connection its own descriptor-relative anchor so
	// OFD WAL-index locks still conflict across concurrent connections.
	anchor, err := root.openStateFile(databaseSHMFilename, false)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join("/dev/fd", strconv.FormatUint(uint64(anchor.Fd()), 10))
	return vfs.NewSharedMemory(path, flags), anchor, nil
}

func syncSQLiteFile(file *os.File, flags vfs.OpenFlag, syncFlags vfs.SyncFlag) error {
	command := 0
	if syncFlags&vfs.SYNC_FULL == vfs.SYNC_FULL {
		if flags&vfs.OPEN_MAIN_JOURNAL != 0 {
			command = unix.F_BARRIERFSYNC
		} else {
			command = unix.F_FULLFSYNC
		}
	}
	for {
		err := error(unix.ENOTSUP)
		if command != 0 {
			_, err = unix.FcntlInt(file.Fd(), command, 0)
		}
		if errors.Is(err, unix.ENOTSUP) {
			err = unix.Fsync(int(file.Fd()))
		}
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func getSQLiteSharedLock(file *os.File) error {
	lock, _ := testSQLiteLock(file, sqlitePendingByte, 1, sqlite3.ExtendedErrorCode(sqlite3.IOERR))
	if lock == unix.F_WRLCK {
		return sqlite3.BUSY
	}
	return readSQLiteLock(file, sqliteSharedFirst, sqliteSharedSize, 0)
}

func getSQLiteReservedLock(file *os.File) error {
	return writeSQLiteLock(file, sqliteReservedByte, 1, 0)
}

func getSQLiteExclusiveLock(file *os.File, state *vfs.LockLevel) error {
	if *state == vfs.LOCK_RESERVED {
		if err := writeSQLiteLock(file, sqlitePendingByte, 1, -1); err != nil {
			return err
		}
		*state = vfs.LOCK_PENDING
	}
	return writeSQLiteLock(file, sqliteSharedFirst, sqliteSharedSize, time.Millisecond)
}

func downgradeSQLiteLock(file *os.File, state vfs.LockLevel) error {
	if state >= vfs.LOCK_EXCLUSIVE {
		if err := readSQLiteLock(file, sqliteSharedFirst, sqliteSharedSize, 0); err != nil {
			return vfs.SystemError(err, sqlite3.IOERR_RDLOCK)
		}
	}
	return unlockSQLiteRange(file, sqlitePendingByte, 2)
}

func releaseSQLiteLock(file *os.File, _ vfs.LockLevel) error {
	return unlockSQLiteRange(file, 0, 0)
}

func checkSQLiteReservedLock(file *os.File) (bool, error) {
	lock, err := testSQLiteLock(file, sqliteReservedByte, 1, sqlite3.IOERR_CHECKRESERVEDLOCK)
	return lock == unix.F_WRLCK, err
}

func readSQLiteLock(file *os.File, start, length int64, timeout time.Duration) error {
	return setSQLiteLock(file, unix.F_RDLCK, start, length, timeout, sqlite3.IOERR_RDLOCK)
}

func writeSQLiteLock(file *os.File, start, length int64, timeout time.Duration) error {
	return setSQLiteLock(file, unix.F_WRLCK, start, length, timeout, sqlite3.IOERR_LOCK)
}

func setSQLiteLock(file *os.File, typ int16, start, length int64, timeout time.Duration, code sqlite3.ExtendedErrorCode) error {
	value := darwinFlockTimeout{lock: unix.Flock_t{Type: typ, Start: start, Len: length}}
	var err error
	switch {
	case timeout == 0:
		err = unix.FcntlFlock(file.Fd(), darwinOFDSetLock, &value.lock)
	case timeout < 0:
		err = unix.FcntlFlock(file.Fd(), darwinOFDSetLockWait, &value.lock)
	default:
		value.timeout = unix.NsecToTimespec(int64(timeout))
		err = unix.FcntlFlock(file.Fd(), darwinOFDSetLockTimeout, &value.lock)
		runtime.KeepAlive(&value)
	}
	return sqliteLockError(err, code)
}

func unlockSQLiteRange(file *os.File, start, length int64) error {
	lock := unix.Flock_t{Type: unix.F_UNLCK, Start: start, Len: length}
	for {
		err := unix.FcntlFlock(file.Fd(), darwinOFDSetLock, &lock)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return vfs.SystemError(err, sqlite3.IOERR_UNLOCK)
		}
	}
}

func testSQLiteLock(file *os.File, start, length int64, code sqlite3.ExtendedErrorCode) (int16, error) {
	lock := unix.Flock_t{Type: unix.F_WRLCK, Start: start, Len: length}
	for {
		err := unix.FcntlFlock(file.Fd(), unix.F_GETLK, &lock)
		if err == nil {
			return lock.Type, nil
		}
		if !errors.Is(err, unix.EINTR) {
			return 0, vfs.SystemError(err, code)
		}
	}
}
