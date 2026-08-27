//go:build darwin || linux

package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/vfs"
	"golang.org/x/sys/unix"
)

const (
	permissiveUmaskHelperEnvironment = "TASKAI_TEST_PERMISSIVE_UMASK"
	openFailureHelperEnvironment     = "TASKAI_TEST_OPEN_FAILURE_CLEANUP"
)

func TestOpenRejectsFinalStateRootSymlinkWithoutTouchingTarget(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "outside-root")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	canaryPath := filepath.Join(target, "canary")
	if err := os.WriteFile(canaryPath, []byte("outside-root-must-not-change"), 0o640); err != nil {
		t.Fatal(err)
	}
	canary := snapshotFile(t, canaryPath)
	directoryBefore, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	directoryIdentity, err := identityFromFileInfo(directoryBefore)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(parent, "state")
	if err := os.Symlink(target, stateDir); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Open error = %v, want ErrUnsafeStatePath", err)
	}
	assertFileUnchanged(t, canaryPath, canary)
	directoryAfter, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	afterIdentity, err := identityFromFileInfo(directoryAfter)
	if err != nil {
		t.Fatal(err)
	}
	if directoryAfter.Mode() != directoryBefore.Mode() ||
		!directoryAfter.ModTime().Equal(directoryBefore.ModTime()) ||
		afterIdentity != directoryIdentity {
		t.Fatalf("outside state-root target changed: before=%v/%v/%+v after=%v/%v/%+v",
			directoryBefore.Mode(), directoryBefore.ModTime(), directoryIdentity,
			directoryAfter.Mode(), directoryAfter.ModTime(), afterIdentity)
	}
	linkInfo, err := os.Lstat(stateDir)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe root link was changed: info=%v err=%v", linkInfo, err)
	}
}

func TestOpenRejectsEverySQLiteStateSymlinkWithoutTouchingTarget(t *testing.T) {
	for _, name := range sqliteStateFilenames {
		name := name
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			if err := os.Mkdir(stateDir, stateDirectoryMode); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "outside-canary")
			canary := []byte("outside-state-must-not-change")
			if err := os.WriteFile(target, canary, 0o640); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, target)
			link := filepath.Join(stateDir, name)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsafeStatePath) {
				t.Fatalf("Open error = %v, want ErrUnsafeStatePath", err)
			}
			assertFileUnchanged(t, target, before)
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("unsafe link was changed or removed: info=%v err=%v", info, err)
			}
		})
	}
}

func TestOpenRejectsEverySQLiteStateHardLinkWithoutTouchingTarget(t *testing.T) {
	for _, name := range sqliteStateFilenames {
		name := name
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			if err := os.Mkdir(stateDir, stateDirectoryMode); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "outside-canary")
			canary := []byte("hard-linked-outside-state")
			if err := os.WriteFile(target, canary, stateFileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, filepath.Join(stateDir, name)); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, target)

			if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsafeStatePath) {
				t.Fatalf("Open error = %v, want ErrUnsafeStatePath", err)
			}
			assertFileUnchanged(t, target, before)
		})
	}
}

func TestOpenRejectsNonRegularSQLiteStateWithoutBlocking(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, stateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(stateDir, DatabaseFilename)
	if err := unix.Mkfifo(pipe, uint32(stateFileMode.Perm())); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("Open error = %v, want ErrUnsafeStatePath", err)
	}
	info, err := os.Lstat(pipe)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("named pipe was changed or removed: info=%v err=%v", info, err)
	}
}

func TestOwnershipValidatorsRejectAnotherEffectiveUser(t *testing.T) {
	root := t.TempDir()
	directoryInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, "owned")
	if err := os.WriteFile(filePath, nil, stateFileMode); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnedDirectoryInfo(directoryInfo, os.Geteuid()); err != nil {
		t.Fatalf("current directory owner rejected: %v", err)
	}
	if err := validateOwnedStateFileInfo(DatabaseFilename, fileInfo, os.Geteuid()); err != nil {
		t.Fatalf("current file owner rejected: %v", err)
	}
	foreignUID := os.Geteuid() + 1
	if err := validateOwnedDirectoryInfo(directoryInfo, foreignUID); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("foreign directory owner error = %v", err)
	}
	if err := validateOwnedStateFileInfo(DatabaseFilename, fileInfo, foreignUID); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("foreign file owner error = %v", err)
	}
}

func TestPinnedRootedVFSDoesNotRetargetAfterConfiguredPathReplacement(t *testing.T) {
	parent := t.TempDir()
	stateDir := filepath.Join(parent, "state")
	if err := os.Mkdir(stateDir, stateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	root, err := openStateRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.prepareSQLiteFiles(); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	vfsName, err := root.registerVFS()
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}

	parked := filepath.Join(parent, "pinned-state")
	outside := filepath.Join(parent, "outside")
	if err := os.Rename(stateDir, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, stateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, stateDir); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", databaseDSNWithVFS(DatabaseFilename, vfsName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = root.unregisterVFS()
		_ = root.Close()
	})
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode = %q, %v", journal, err)
	}
	if _, err := db.Exec(`CREATE TABLE pinned(value TEXT); INSERT INTO pinned VALUES ('safe')`); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("replacement target was touched: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(parked, DatabaseFilename)); err != nil {
		t.Fatalf("pinned database is unavailable: %v", err)
	}
}

func TestRootedVFSRejectsTraversalUnknownNamesAndMainDeletion(t *testing.T) {
	root, cleanup := openPreparedTestRoot(t)
	defer cleanup()
	wrapped := root.vfs
	for _, name := range []string{"../metadata.sqlite3", "/outside.sqlite3", "unknown.sqlite3", root.namespace + "/../outside"} {
		if _, err := wrapped.FullPathname(name); !errors.Is(err, ErrUnsafeStatePath) {
			t.Errorf("FullPathname(%q) error = %v", name, err)
		}
		if _, err := wrapped.Access(name, vfs.ACCESS_EXISTS); !errors.Is(err, ErrUnsafeStatePath) {
			t.Errorf("Access(%q) error = %v", name, err)
		}
	}
	if err := wrapped.Delete(filepath.Join(root.namespace, DatabaseFilename), true); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("main database delete error = %v", err)
	}
	if _, _, err := root.openSQLiteFile(databaseWALFilename, vfs.OPEN_MAIN_DB|vfs.OPEN_READWRITE); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("mismatched SQLite file kind error = %v", err)
	}
	for _, flags := range []vfs.OpenFlag{
		vfs.OPEN_MAIN_DB,
		vfs.OPEN_MAIN_DB | vfs.OPEN_READONLY | vfs.OPEN_READWRITE,
		vfs.OPEN_MAIN_DB | vfs.OPEN_WAL | vfs.OPEN_READWRITE,
		vfs.OPEN_MAIN_DB | vfs.OPEN_TEMP_DB | vfs.OPEN_READWRITE,
	} {
		if validSQLiteOpen(DatabaseFilename, flags) {
			t.Errorf("invalid mixed SQLite flags accepted: %#x", flags)
		}
	}
	if !validSQLiteOpen(DatabaseFilename, vfs.OPEN_MAIN_DB|vfs.OPEN_READWRITE|vfs.OPEN_CREATE) {
		t.Fatal("ordinary main database flags were rejected")
	}
}

func TestRootedVFSRejectsMainAndSidecarSwapsAfterPreflight(t *testing.T) {
	for _, name := range sqliteStateFilenames {
		name := name
		t.Run(name, func(t *testing.T) {
			root, cleanup := openPreparedTestRoot(t)
			defer cleanup()
			outside := filepath.Join(t.TempDir(), "outside")
			canary := []byte("swap-target")
			if err := os.WriteFile(outside, canary, stateFileMode); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, outside)
			inside := filepath.Join(root.displayDir, name)
			if err := os.Remove(inside); err != nil &&
				!((name == databaseJournalFilename || name == databaseWALFilename) && errors.Is(err, os.ErrNotExist)) {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, inside); err != nil {
				t.Fatal(err)
			}

			db, err := sql.Open("sqlite3", databaseDSNWithVFS(DatabaseFilename, root.vfsName))
			if err != nil {
				t.Fatal(err)
			}
			err = exerciseSQLiteStateName(db, name)
			_ = db.Close()
			if err == nil {
				t.Fatal("SQLite unexpectedly accepted a swapped state file")
			}
			assertFileUnchanged(t, outside, before)
			info, lstatErr := os.Lstat(inside)
			if lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("unsafe swapped link was changed: info=%v err=%v", info, lstatErr)
			}
		})
	}
}

func TestRootedVFSRejectsHardLinksInstalledAfterPreflight(t *testing.T) {
	for _, name := range sqliteStateFilenames {
		name := name
		t.Run(name, func(t *testing.T) {
			root, cleanup := openPreparedTestRoot(t)
			defer cleanup()
			inside := filepath.Join(root.displayDir, name)
			if err := os.Remove(inside); err != nil &&
				!((name == databaseJournalFilename || name == databaseWALFilename) && errors.Is(err, os.ErrNotExist)) {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("hard-link-after-preflight"), stateFileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, inside); err != nil {
				t.Fatal(err)
			}
			before := snapshotFile(t, outside)

			db, err := sql.Open("sqlite3", databaseDSNWithVFS(DatabaseFilename, root.vfsName))
			if err != nil {
				t.Fatal(err)
			}
			err = exerciseSQLiteStateName(db, name)
			_ = db.Close()
			if err == nil {
				t.Fatal("SQLite unexpectedly accepted a post-preflight hard link")
			}
			assertFileUnchanged(t, outside, before)
			insideInfo, insideErr := os.Stat(inside)
			outsideInfo, outsideErr := os.Stat(outside)
			if insideErr != nil || outsideErr != nil || !os.SameFile(insideInfo, outsideInfo) {
				t.Fatalf("unsafe hard link was changed: inside=%v/%v outside=%v/%v", insideInfo, insideErr, outsideInfo, outsideErr)
			}
		})
	}
}

func TestRootedVFSRejectsRegularMainInodeSwap(t *testing.T) {
	root, cleanup := openPreparedTestRoot(t)
	defer cleanup()
	mainPath := filepath.Join(root.displayDir, DatabaseFilename)
	parked := mainPath + ".parked"
	if err := os.Rename(mainPath, parked); err != nil {
		t.Fatal(err)
	}
	canary := []byte("different-regular-file")
	if err := os.WriteFile(mainPath, canary, stateFileMode); err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(t, mainPath)
	db, err := sql.Open("sqlite3", databaseDSNWithVFS(DatabaseFilename, root.vfsName))
	if err != nil {
		t.Fatal(err)
	}
	err = db.PingContext(context.Background())
	_ = db.Close()
	if !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("main inode swap error = %v, want ErrUnsafeStatePath", err)
	}
	assertFileUnchanged(t, mainPath, before)
}

func TestSecureFilesRejectsMainReplacementBeforeChangingItsMode(t *testing.T) {
	root, cleanup := openPreparedTestRoot(t)
	defer cleanup()
	mainPath := filepath.Join(root.displayDir, DatabaseFilename)
	if err := os.Rename(mainPath, mainPath+".parked"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, []byte("replacement"), 0o640); err != nil {
		t.Fatal(err)
	}
	before := snapshotFile(t, mainPath)
	if err := root.secureFiles(); !errors.Is(err, ErrUnsafeStatePath) {
		t.Fatalf("secureFiles error = %v, want ErrUnsafeStatePath", err)
	}
	assertFileUnchanged(t, mainPath, before)
}

func TestOpenPermissionsUnderPermissiveUmask(t *testing.T) {
	if stateDir := os.Getenv(permissiveUmaskHelperEnvironment); stateDir != "" {
		previous := syscall.Umask(0)
		defer syscall.Umask(previous)
		store, err := Open(context.Background(), stateDir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		createTask(t, store, "task_umask", "umask", fixedTime)
		store.db.SetMaxIdleConns(0)
		store.db.SetMaxIdleConns(4)
		createTask(t, store, "task_umask_reopen", "umask reopen", fixedTime.Add(time.Second))
		assertOwnedMode(t, stateDir, stateDirectoryMode)
		for _, path := range []string{store.Path(), store.Path() + "-wal", store.Path() + "-shm"} {
			assertOwnedMode(t, path, stateFileMode)
		}
		return
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	command := exec.Command(os.Args[0], "-test.run=^TestOpenPermissionsUnderPermissiveUmask$", "-test.count=1")
	command.Env = append(os.Environ(), permissiveUmaskHelperEnvironment+"="+stateDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("permissive-umask helper failed: %v\n%s", err, output)
	}
}

func TestCloseUnregistersVFSAndClosesPinnedRoot(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	root := store.root
	vfsName := root.vfsName
	if vfs.Find(vfsName) == nil {
		t.Fatal("rooted VFS was not registered")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if vfs.Find(vfsName) != nil {
		t.Fatal("rooted VFS remained registered after Close")
	}
	if _, err := root.file.Stat(); err == nil {
		t.Fatal("pinned state-root descriptor remained open after Close")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpenFailureUnregistersVFSAndClosesPinnedRoot(t *testing.T) {
	if stateDir := os.Getenv(openFailureHelperEnvironment); stateDir != "" {
		if err := os.Mkdir(stateDir, stateDirectoryMode); err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite3", databaseDSN(filepath.Join(stateDir, DatabaseFilename)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
			_ = raw.Close()
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		rootInfo, err := os.Stat(stateDir)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsupportedSchemaVersion) {
			t.Fatalf("Open error = %v, want ErrUnsupportedSchemaVersion", err)
		}
		if sequence := rootedVFSSequence.Load(); sequence != 1 {
			t.Fatalf("VFS registrations = %d, want 1", sequence)
		}
		vfsName := fmt.Sprintf("taskai_%d_1", os.Getpid())
		if vfs.Find(vfsName) != nil {
			t.Fatal("failed Open left its rooted VFS registered")
		}
		aliasBase := filepath.Dir(descriptorDirectoryPath(0))
		aliasDirectory, err := os.Open(aliasBase)
		if err != nil {
			t.Fatal(err)
		}
		entries, readErr := aliasDirectory.Readdirnames(-1)
		closeErr := aliasDirectory.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal(errors.Join(readErr, closeErr))
		}
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join(aliasBase, entry))
			if err == nil && os.SameFile(rootInfo, info) {
				t.Fatalf("failed Open left pinned state-root descriptor %s open", entry)
			}
		}
		return
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	command := exec.Command(os.Args[0], "-test.run=^TestOpenFailureUnregistersVFSAndClosesPinnedRoot$", "-test.count=1")
	command.Env = append(os.Environ(), openFailureHelperEnvironment+"="+stateDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("open-failure cleanup helper failed: %v\n%s", err, output)
	}
}

func TestSimultaneousStoresHaveIndependentRootedVFSInstances(t *testing.T) {
	parent := t.TempDir()
	first, err := Open(context.Background(), filepath.Join(parent, "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), filepath.Join(parent, "second"))
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	defer second.Close()
	if first.root.vfsName == second.root.vfsName || first.root.namespace == second.root.namespace {
		t.Fatal("simultaneous Stores shared a rooted VFS identity")
	}
	secondVFS := second.root.vfsName
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if vfs.Find(secondVFS) == nil {
		t.Fatal("closing one Store unregistered the other Store's VFS")
	}
	createTask(t, second, "task_second_store", "second", fixedTime)
	if _, err := second.GetTask(context.Background(), "task_second_store"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreReopensSameWALDatabase(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	first, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	createTask(t, first, "task_reopen_one", "one", fixedTime)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.GetTask(context.Background(), "task_reopen_one"); err != nil {
		t.Fatal(err)
	}
	createTask(t, second, "task_reopen_two", "two", fixedTime)
	var journal string
	if err := second.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode after reopen = %q, %v", journal, err)
	}
}

func TestOpenAllowsSymlinkedAncestorButRequiresRealFinalRoot(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	if err := os.Mkdir(realParent, stateDirectoryMode); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(linkedParent, "taskai")
	store, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.StateDir() != filepath.Clean(stateDir) {
		t.Fatalf("StateDir = %q, want configured path %q", store.StateDir(), filepath.Clean(stateDir))
	}
	if _, err := os.Stat(filepath.Join(realParent, "taskai", DatabaseFilename)); err != nil {
		t.Fatalf("database beneath symlinked ancestor: %v", err)
	}
}

func openPreparedTestRoot(t *testing.T) (*stateRoot, func()) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	root, err := openStateRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.prepareSQLiteFiles(); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if _, err := root.registerVFS(); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	return root, func() {
		_ = root.unregisterVFS()
		_ = root.Close()
	}
}

func exerciseSQLiteStateName(db *sql.DB, name string) error {
	switch name {
	case DatabaseFilename:
		return db.PingContext(context.Background())
	case databaseJournalFilename:
		if _, err := db.Exec(`PRAGMA journal_mode = DELETE`); err != nil {
			return err
		}
		_, err := db.Exec(`CREATE TABLE force_journal(value TEXT)`)
		return err
	case databaseWALFilename, databaseSHMFilename:
		var journal string
		if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journal); err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE TABLE force_wal(value TEXT)`); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO force_wal VALUES ('mapped')`); err != nil {
			return err
		}
		var count int
		return db.QueryRow(`SELECT count(*) FROM force_wal`).Scan(&count)
	default:
		return fmt.Errorf("unknown SQLite state name %q", name)
	}
}

type fileSnapshot struct {
	contents []byte
	mode     os.FileMode
	size     int64
	modified int64
	identity fileIdentity
}

func snapshotFile(t *testing.T, path string) fileSnapshot {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identityFromFileInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnapshot{
		contents: append([]byte(nil), contents...),
		mode:     info.Mode(),
		size:     info.Size(),
		modified: info.ModTime().UnixNano(),
		identity: identity,
	}
}

func assertFileUnchanged(t *testing.T, path string, before fileSnapshot) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, before.contents) {
		t.Fatalf("outside target changed: got %q want %q", got, before.contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := identityFromFileInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != before.mode || info.Size() != before.size ||
		info.ModTime().UnixNano() != before.modified || identity != before.identity {
		t.Fatalf("outside target metadata changed: mode=%v size=%d modified=%d identity=%+v, want mode=%v size=%d modified=%d identity=%+v",
			info.Mode(), info.Size(), info.ModTime().UnixNano(), identity,
			before.mode, before.size, before.modified, before.identity)
	}
}

func assertOwnedMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode.Perm() {
		t.Fatalf("mode %s = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), mode.Perm())
	}
	uid, _, ok := posixIdentity(info)
	if !ok || uid != uint64(os.Geteuid()) {
		t.Fatalf("owner %s = uid %d available=%v, want %d", filepath.Base(path), uid, ok, os.Geteuid())
	}
}
