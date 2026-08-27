//go:build linux

package store

import (
	"errors"
	"os"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

func (root *stateRoot) newSharedMemory(flags vfs.OpenFlag) (vfs.SharedMemory, *os.File, error) {
	// NewSharedMemory opens lazily. Revalidate and descriptor-chmod the exact
	// entry for every new main connection, including after pool churn removed
	// the final previous WAL-index mapping.
	file, err := root.openStateFile(databaseSHMFilename, true)
	if err != nil {
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		return nil, nil, err
	}
	return vfs.NewSharedMemory(root.aliasDir+"/"+databaseSHMFilename, flags), nil, nil
}

func syncSQLiteFile(file *os.File, _ vfs.OpenFlag, _ vfs.SyncFlag) error {
	for {
		err := unix.Fdatasync(int(file.Fd()))
		if err != unix.EINTR {
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
	lock := unix.Flock_t{Type: typ, Start: start, Len: length}
	command := unix.F_OFD_SETLK
	if timeout < 0 {
		command = unix.F_OFD_SETLKW
	}
	return sqliteLockError(unix.FcntlFlock(file.Fd(), command, &lock), code)
}

func unlockSQLiteRange(file *os.File, start, length int64) error {
	lock := unix.Flock_t{Type: unix.F_UNLCK, Start: start, Len: length}
	for {
		err := unix.FcntlFlock(file.Fd(), unix.F_OFD_SETLK, &lock)
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
