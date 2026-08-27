//go:build darwin || linux

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

var rootedVFSSequence atomic.Uint64

const (
	sqlitePendingByte  = 0x40000000
	sqliteReservedByte = sqlitePendingByte + 1
	sqliteSharedFirst  = sqlitePendingByte + 2
	sqliteSharedSize   = 510
)

// stateRoot pins the final state-directory component. All persistent SQLite
// opens and deletes are descriptor-relative, so renaming or replacing the
// configured path cannot retarget a later pooled connection.
type stateRoot struct {
	mu sync.Mutex

	file       *os.File
	displayDir string
	aliasDir   string
	namespace  string
	vfsName    string
	vfs        *rootedVFS
	mainFile   fileIdentity
	mainReady  bool
	closed     bool
}

type rootedVFS struct {
	root          *stateRoot
	baseFilenames vfs.VFSFilename
}

type rootedSQLiteFile struct {
	file    *os.File
	root    *stateRoot
	shm     vfs.SharedMemory
	shmFD   *os.File // Darwin keeps the connection-specific /dev/fd anchor live.
	flags   vfs.OpenFlag
	lock    vfs.LockLevel
	syncDir bool
	mu      sync.Mutex
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

var _ vfs.FileSharedMemory = (*rootedSQLiteFile)(nil)
var _ vfs.FileHasMoved = (*rootedSQLiteFile)(nil)

func openStateRoot(path string) (*stateRoot, error) {
	file, err := openStateDirectory(path)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.MkdirAll(path, stateDirectoryMode); mkdirErr != nil {
			return nil, fmt.Errorf("create state directory: %w", mkdirErr)
		}
		file, err = openStateDirectory(path)
	}
	if err != nil {
		return nil, classifyStateDirectoryOpenError(path, err)
	}

	cleanup := func(cause error) (*stateRoot, error) {
		return nil, errors.Join(cause, file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return cleanup(fmt.Errorf("inspect state directory descriptor: %w", err))
	}
	if err := validateOwnedDirectoryInfo(info, os.Geteuid()); err != nil {
		return cleanup(err)
	}
	if err := file.Chmod(stateDirectoryMode); err != nil {
		return cleanup(fmt.Errorf("secure state directory descriptor: %w", err))
	}

	aliasDir := descriptorDirectoryPath(file.Fd())
	aliasRoot, err := os.OpenRoot(aliasDir)
	if err != nil {
		return cleanup(fmt.Errorf("verify state directory descriptor alias: %w", err))
	}
	aliasInfo, statErr := aliasRoot.Stat(".")
	aliasCloseErr := aliasRoot.Close()
	if statErr != nil || aliasCloseErr != nil {
		return cleanup(errors.Join(
			fmt.Errorf("verify state directory descriptor alias: %w", statErr),
			aliasCloseErr,
		))
	}
	if !os.SameFile(info, aliasInfo) {
		return cleanup(fmt.Errorf("%w: state directory descriptor alias changed", ErrUnsafeStatePath))
	}

	return &stateRoot{
		file:       file,
		displayDir: path,
		aliasDir:   aliasDir,
	}, nil
}

func openStateDirectory(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func classifyStateDirectoryOpenError(path string, openErr error) error {
	info, inspectErr := os.Lstat(path)
	if inspectErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return fmt.Errorf("%w: state directory is not a real directory", ErrUnsafeStatePath)
	}
	if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
		return fmt.Errorf("%w: state directory is not a real directory", ErrUnsafeStatePath)
	}
	return fmt.Errorf("open state directory: %w", openErr)
}

func descriptorDirectoryPath(fd uintptr) string {
	base := "/dev/fd"
	if runtime.GOOS == "linux" {
		base = "/proc/self/fd"
	}
	return filepath.Join(base, strconv.FormatUint(uint64(fd), 10))
}

func (root *stateRoot) prepareSQLiteFiles() error {
	if err := root.secureDirectory(); err != nil {
		return err
	}
	for _, name := range sqliteStateFilenames {
		if _, err := root.secureExistingFile(name); err != nil {
			return err
		}
	}
	if err := root.prepareMainFile(); err != nil {
		return err
	}
	return root.ensureFile(databaseSHMFilename)
}

func (root *stateRoot) prepareMainFile() error {
	file, err := root.openStateFile(DatabaseFilename, true)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(fmt.Errorf("inspect main database descriptor: %w", err), file.Close())
	}
	identity, err := identityFromFileInfo(info)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	root.mainFile = identity
	root.mainReady = true
	return nil
}

func (root *stateRoot) secureFiles() error {
	if err := root.secureDirectory(); err != nil {
		return err
	}
	for _, name := range sqliteStateFilenames {
		if _, err := root.secureExistingFile(name); err != nil {
			return err
		}
	}
	return nil
}

func (root *stateRoot) secureDirectory() error {
	info, err := root.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect state directory descriptor: %w", err)
	}
	if err := validateOwnedDirectoryInfo(info, os.Geteuid()); err != nil {
		return err
	}
	if err := root.file.Chmod(stateDirectoryMode); err != nil {
		return fmt.Errorf("secure state directory descriptor: %w", err)
	}
	return nil
}

func (root *stateRoot) ensureFile(name string) error {
	file, err := root.openStateFile(name, true)
	if err != nil {
		return err
	}
	return file.Close()
}

func (root *stateRoot) secureExistingFile(name string) (bool, error) {
	file, err := root.openStateFile(name, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

func (root *stateRoot) openStateFile(name string, create bool) (*os.File, error) {
	if !isSQLiteStateName(name) {
		return nil, unsafeStateFile(name, "name is outside the SQLite state allowlist")
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Openat(int(root.file.Fd()), name, flags, uint32(stateFileMode.Perm()))
	if err != nil {
		if !create && errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, root.classifyStateFileOpenError(name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root.displayDir, name))
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect %s descriptor: %w", name, err), file.Close())
	}
	if err := validateOwnedStateFileInfo(name, info, os.Geteuid()); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := root.validatePreparedMain(name, info); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := file.Chmod(stateFileMode); err != nil {
		return nil, errors.Join(fmt.Errorf("secure %s descriptor: %w", name, err), file.Close())
	}
	return file, nil
}

func (root *stateRoot) classifyStateFileOpenError(name string, openErr error) error {
	var stat unix.Stat_t
	inspectErr := unix.Fstatat(int(root.file.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if inspectErr == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return unsafeStateFile(name, "entry is not a regular file")
		}
		if uint64(stat.Uid) != uint64(os.Geteuid()) {
			return unsafeStateFile(name, "entry is not owned by the effective user")
		}
		if uint64(stat.Nlink) != 1 {
			return unsafeStateFile(name, "entry has multiple hard links")
		}
	}
	if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.EISDIR) || errors.Is(openErr, unix.ENOTDIR) {
		return unsafeStateFile(name, "entry cannot be opened without following a link")
	}
	return fmt.Errorf("open %s beneath state directory: %w", name, openErr)
}

func validateOwnedDirectoryInfo(info os.FileInfo, effectiveUID int) error {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: state directory is not a real directory", ErrUnsafeStatePath)
	}
	uid, _, ok := posixIdentity(info)
	if !ok {
		return fmt.Errorf("%w: state directory ownership is unavailable", ErrUnsafeStatePath)
	}
	if uid != uint64(effectiveUID) {
		return fmt.Errorf("%w: state directory is not owned by the effective user", ErrUnsafeStatePath)
	}
	return nil
}

func validateOwnedStateFileInfo(name string, info os.FileInfo, effectiveUID int) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return unsafeStateFile(name, "entry is not a regular file")
	}
	uid, links, ok := posixIdentity(info)
	if !ok {
		return unsafeStateFile(name, "ownership is unavailable")
	}
	if uid != uint64(effectiveUID) {
		return unsafeStateFile(name, "entry is not owned by the effective user")
	}
	if links != 1 {
		return unsafeStateFile(name, "entry has multiple hard links")
	}
	return nil
}

func posixIdentity(info os.FileInfo) (uid, links uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, false
	}
	return uint64(stat.Uid), uint64(stat.Nlink), true
}

func identityFromFileInfo(info os.FileInfo) (fileIdentity, error) {
	if info == nil {
		return fileIdentity{}, fmt.Errorf("%w: filesystem identity is unavailable", ErrUnsafeStatePath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileIdentity{}, fmt.Errorf("%w: filesystem identity is unavailable", ErrUnsafeStatePath)
	}
	return fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func (root *stateRoot) validatePreparedMain(name string, info os.FileInfo) error {
	if name != DatabaseFilename || !root.mainReady {
		return nil
	}
	identity, err := identityFromFileInfo(info)
	if err != nil {
		return err
	}
	if identity != root.mainFile {
		return unsafeStateFile(name, "entry changed after the state root was prepared")
	}
	return nil
}

func unsafeStateFile(name, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrUnsafeStatePath, filepath.Base(name), reason)
}

func isSQLiteStateName(name string) bool {
	for _, allowed := range sqliteStateFilenames {
		if name == allowed {
			return true
		}
	}
	return false
}

func (root *stateRoot) relativeSQLiteName(name string) (string, bool) {
	for _, allowed := range sqliteStateFilenames {
		if name == allowed || name == filepath.Join(root.namespace, allowed) {
			return allowed, true
		}
	}
	return "", false
}

func (root *stateRoot) registerVFS() (string, error) {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed {
		return "", fmt.Errorf("register SQLite VFS: state root is closed")
	}
	if root.vfsName != "" {
		return root.vfsName, nil
	}
	baseFilenames, ok := vfs.Find("").(vfs.VFSFilename)
	if !ok {
		return "", fmt.Errorf("register SQLite VFS: default VFS lacks filename support")
	}
	sequence := strconv.FormatUint(rootedVFSSequence.Add(1), 10)
	name := "taskai_" + strconv.Itoa(os.Getpid()) + "_" + sequence
	root.namespace = string(filepath.Separator) + "taskai-state-" + strconv.Itoa(os.Getpid()) + "-" + sequence
	wrapped := &rootedVFS{root: root, baseFilenames: baseFilenames}
	vfs.Register(name, wrapped)
	if vfs.Find(name) != wrapped {
		vfs.Unregister(name)
		root.namespace = ""
		return "", fmt.Errorf("register SQLite VFS: registration was not retained")
	}
	root.vfsName = name
	root.vfs = wrapped
	return name, nil
}

func (root *stateRoot) unregisterVFS() error {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.vfsName == "" {
		return nil
	}
	if current := vfs.Find(root.vfsName); current != root.vfs {
		return fmt.Errorf("unregister SQLite VFS: registration identity changed")
	}
	vfs.Unregister(root.vfsName)
	root.vfsName = ""
	root.vfs = nil
	root.namespace = ""
	return nil
}

func (root *stateRoot) Close() error {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed {
		return nil
	}
	root.closed = true
	return root.file.Close()
}

func (wrapped *rootedVFS) FullPathname(name string) (string, error) {
	relative, ok := wrapped.root.relativeSQLiteName(name)
	if !ok {
		return "", unsafeStateFile(name, "path is outside the SQLite state allowlist")
	}
	return filepath.Join(wrapped.root.namespace, relative), nil
}

func (wrapped *rootedVFS) Open(name string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	if name == "" {
		return wrapped.baseFilenames.Open(name, flags)
	}
	relative, ok := wrapped.root.relativeSQLiteName(name)
	if !ok {
		return nil, flags, unsafeStateFile(name, "path is outside the SQLite state allowlist")
	}
	return wrapped.root.openSQLiteFile(relative, flags)
}

func (wrapped *rootedVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	if name == nil || name.String() == "" {
		return wrapped.baseFilenames.OpenFilename(name, flags)
	}
	relative, ok := wrapped.root.relativeSQLiteName(name.String())
	if !ok {
		return nil, flags, unsafeStateFile(name.String(), "path is outside the SQLite state allowlist")
	}
	return wrapped.root.openSQLiteFile(relative, flags)
}

func (root *stateRoot) openSQLiteFile(name string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	if !validSQLiteOpen(name, flags) {
		return nil, flags, unsafeStateFile(name, "SQLite file kind does not match the admitted name")
	}
	openFlags := unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	switch {
	case flags&vfs.OPEN_READWRITE != 0:
		openFlags |= unix.O_RDWR
	case flags&vfs.OPEN_READONLY != 0:
		openFlags |= unix.O_RDONLY
	default:
		return nil, flags, unsafeStateFile(name, "SQLite supplied no supported access mode")
	}
	if flags&vfs.OPEN_CREATE != 0 {
		openFlags |= unix.O_CREAT
	}
	if flags&vfs.OPEN_EXCLUSIVE != 0 {
		openFlags |= unix.O_EXCL
	}
	fd, err := unix.Openat(int(root.file.Fd()), name, openFlags, uint32(stateFileMode.Perm()))
	if err != nil {
		return nil, flags, root.classifyStateFileOpenError(name, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root.displayDir, name))
	info, err := file.Stat()
	if err != nil {
		return nil, flags, errors.Join(fmt.Errorf("inspect SQLite %s descriptor: %w", name, err), file.Close())
	}
	if err := validateOwnedStateFileInfo(name, info, os.Geteuid()); err != nil {
		return nil, flags, errors.Join(err, file.Close())
	}
	if err := root.validatePreparedMain(name, info); err != nil {
		return nil, flags, errors.Join(err, file.Close())
	}
	if err := file.Chmod(stateFileMode); err != nil {
		return nil, flags, errors.Join(fmt.Errorf("secure SQLite %s descriptor: %w", name, err), file.Close())
	}
	opened := &rootedSQLiteFile{
		file:    file,
		root:    root,
		flags:   flags,
		syncDir: flags&vfs.OPEN_CREATE != 0 && flags&(vfs.OPEN_MAIN_JOURNAL|vfs.OPEN_WAL) != 0,
	}
	if flags&vfs.OPEN_MAIN_DB != 0 {
		opened.shm, opened.shmFD, err = root.newSharedMemory(flags)
		if err != nil {
			return nil, flags, errors.Join(err, file.Close())
		}
	}
	return opened, flags, nil
}

func validSQLiteOpen(name string, flags vfs.OpenFlag) bool {
	const kindMask = vfs.OPEN_MAIN_DB |
		vfs.OPEN_TEMP_DB |
		vfs.OPEN_TRANSIENT_DB |
		vfs.OPEN_MAIN_JOURNAL |
		vfs.OPEN_TEMP_JOURNAL |
		vfs.OPEN_SUBJOURNAL |
		vfs.OPEN_SUPER_JOURNAL |
		vfs.OPEN_WAL
	const accessMask = vfs.OPEN_READONLY | vfs.OPEN_READWRITE
	if access := flags & accessMask; access != vfs.OPEN_READONLY && access != vfs.OPEN_READWRITE {
		return false
	}
	var expected vfs.OpenFlag
	switch name {
	case DatabaseFilename:
		expected = vfs.OPEN_MAIN_DB
	case databaseJournalFilename:
		expected = vfs.OPEN_MAIN_JOURNAL
	case databaseWALFilename:
		expected = vfs.OPEN_WAL
	default:
		return false
	}
	return flags&kindMask == expected
}

func (wrapped *rootedVFS) Delete(name string, syncDir bool) error {
	relative, ok := wrapped.root.relativeSQLiteName(name)
	if !ok || relative == DatabaseFilename {
		return unsafeStateFile(name, "SQLite may not delete this path")
	}
	exists, err := wrapped.root.secureExistingFile(relative)
	if err != nil {
		return err
	}
	if !exists {
		return vfs.SystemError(os.ErrNotExist, sqlite3.IOERR_DELETE_NOENT)
	}
	if err := unix.Unlinkat(int(wrapped.root.file.Fd()), relative, 0); err != nil {
		return vfs.SystemError(err, sqlite3.IOERR_DELETE)
	}
	if syncDir {
		if err := wrapped.root.file.Sync(); err != nil {
			return vfs.SystemError(err, sqlite3.IOERR_DIR_FSYNC)
		}
	}
	return nil
}

func (wrapped *rootedVFS) Access(name string, _ vfs.AccessFlag) (bool, error) {
	relative, ok := wrapped.root.relativeSQLiteName(name)
	if !ok {
		return false, unsafeStateFile(name, "path is outside the SQLite state allowlist")
	}
	return wrapped.root.secureExistingFile(relative)
}

func (file *rootedSQLiteFile) Close() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.shm != nil {
		// SQLite calls xShmUnmap before xClose. ncruces intentionally ignores
		// the subsequent SharedMemory.Close error after that unmap cleared its
		// embedded file, and its default VFS does the same.
		_ = file.shm.Close()
		file.shm = nil
	}
	unlockErr := file.unlockFile()
	var shmFDErr error
	if file.shmFD != nil {
		shmFDErr = file.shmFD.Close()
		file.shmFD = nil
	}
	return errors.Join(unlockErr, shmFDErr, file.file.Close())
}

func (file *rootedSQLiteFile) ReadAt(buffer []byte, offset int64) (int, error) {
	return file.file.ReadAt(buffer, offset)
}

func (file *rootedSQLiteFile) WriteAt(buffer []byte, offset int64) (int, error) {
	written, err := file.file.WriteAt(buffer, offset)
	if errors.Is(err, syscall.ENOSPC) {
		return written, sqlite3.FULL
	}
	return written, err
}

func (file *rootedSQLiteFile) Truncate(size int64) error {
	if err := file.file.Truncate(size); err != nil {
		return vfs.SystemError(err, sqlite3.IOERR_TRUNCATE)
	}
	return nil
}

func (file *rootedSQLiteFile) Sync(flags vfs.SyncFlag) error {
	if err := syncSQLiteFile(file.file, file.flags, flags); err != nil {
		return vfs.SystemError(err, sqlite3.IOERR_FSYNC)
	}
	if file.syncDir {
		file.syncDir = false
		if err := file.root.file.Sync(); err != nil {
			return vfs.SystemError(err, sqlite3.IOERR_DIR_FSYNC)
		}
	}
	return nil
}

func (file *rootedSQLiteFile) Size() (int64, error) {
	info, err := file.file.Stat()
	if err != nil {
		return 0, vfs.SystemError(err, sqlite3.IOERR_FSTAT)
	}
	return info.Size(), nil
}

func (file *rootedSQLiteFile) Lock(target vfs.LockLevel) error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if target <= file.lock {
		return nil
	}
	if target >= vfs.LOCK_RESERVED && file.flags&vfs.OPEN_READONLY != 0 {
		return sqlite3.IOERR_LOCK
	}
	switch target {
	case vfs.LOCK_SHARED:
		if file.lock != vfs.LOCK_NONE {
			return sqlite3.IOERR_LOCK
		}
		if err := getSQLiteSharedLock(file.file); err != nil {
			return err
		}
	case vfs.LOCK_RESERVED:
		if file.lock != vfs.LOCK_SHARED {
			return sqlite3.IOERR_LOCK
		}
		if err := getSQLiteReservedLock(file.file); err != nil {
			return err
		}
	case vfs.LOCK_EXCLUSIVE:
		if file.lock <= vfs.LOCK_NONE || file.lock >= vfs.LOCK_EXCLUSIVE {
			return sqlite3.IOERR_LOCK
		}
		if err := getSQLiteExclusiveLock(file.file, &file.lock); err != nil {
			return err
		}
	default:
		return sqlite3.IOERR_LOCK
	}
	file.lock = target
	return nil
}

func (file *rootedSQLiteFile) Unlock(target vfs.LockLevel) error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if target >= file.lock {
		return nil
	}
	var err error
	switch target {
	case vfs.LOCK_SHARED:
		err = downgradeSQLiteLock(file.file, file.lock)
	case vfs.LOCK_NONE:
		err = releaseSQLiteLock(file.file, file.lock)
	default:
		return sqlite3.IOERR_UNLOCK
	}
	file.lock = target
	return err
}

func (file *rootedSQLiteFile) unlockFile() error {
	if file.lock == vfs.LOCK_NONE {
		return nil
	}
	err := releaseSQLiteLock(file.file, file.lock)
	file.lock = vfs.LOCK_NONE
	return err
}

func (file *rootedSQLiteFile) CheckReservedLock() (bool, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.lock >= vfs.LOCK_RESERVED {
		return true, nil
	}
	return checkSQLiteReservedLock(file.file)
}

func (file *rootedSQLiteFile) SectorSize() int { return 4096 }

func (file *rootedSQLiteFile) DeviceCharacteristics() vfs.DeviceCharacteristic { return 0 }

func (file *rootedSQLiteFile) SharedMemory() vfs.SharedMemory { return file.shm }

func (file *rootedSQLiteFile) HasMoved() (bool, error) {
	if file.flags&vfs.OPEN_MAIN_DB == 0 {
		return false, nil
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(file.root.file.Fd()), DatabaseFilename, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true, nil
		}
		return false, vfs.SystemError(err, sqlite3.IOERR_FSTAT)
	}
	current := fileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
	return current != file.root.mainFile ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		uint64(stat.Uid) != uint64(os.Geteuid()) ||
		uint64(stat.Nlink) != 1, nil
}

func sqliteLockError(err error, code sqlite3.ExtendedErrorCode) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.EAGAIN) ||
		errors.Is(err, unix.EBUSY) ||
		errors.Is(err, unix.EWOULDBLOCK) ||
		errors.Is(err, unix.EINTR) ||
		errors.Is(err, unix.ENOLCK) ||
		errors.Is(err, unix.EDEADLK) ||
		errors.Is(err, unix.ETIMEDOUT) {
		return sqlite3.BUSY
	}
	if errors.Is(err, unix.EPERM) {
		return sqlite3.PERM
	}
	return vfs.SystemError(err, code)
}

var _ io.ReaderAt = (*rootedSQLiteFile)(nil)
