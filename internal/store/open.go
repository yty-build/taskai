package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const (
	// DatabaseFilename is the only TaskAI metadata database in a state directory.
	DatabaseFilename = "metadata.sqlite3"
	// SchemaVersion is the newest deterministic metadata schema version.
	SchemaVersion = 1
	// BusyTimeout bounds ordinary SQLite lock contention.
	BusyTimeout = 5 * time.Second
)

const (
	stateDirectoryMode os.FileMode = 0o700
	stateFileMode      os.FileMode = 0o600
)

// Store persists only Task, Session, and non-secret provider Settings metadata.
type Store struct {
	mu       sync.RWMutex
	db       *sql.DB
	stateDir string
	dbPath   string
	closed   bool
}

// Open creates or opens the metadata database beneath stateDir.
func Open(ctx context.Context, stateDir string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("%w: state directory must be absolute", ErrUnsafeStatePath)
	}
	absDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	if err := ensureStateDirectory(absDir); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(absDir, DatabaseFilename)
	if err := ensureDatabaseFile(dbPath); err != nil {
		return nil, err
	}

	dsn := databaseDSN(dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open metadata database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	opened := &Store{db: db, stateDir: absDir, dbPath: dbPath}
	cleanup := func(cause error) (*Store, error) {
		closeErr := db.Close()
		return nil, errors.Join(cause, closeErr)
	}

	if err := db.PingContext(ctx); err != nil {
		return cleanup(contextError(ctx, fmt.Errorf("ping metadata database: %w", err)))
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return cleanup(contextError(ctx, fmt.Errorf("enable WAL: %w", err)))
	}
	if journalMode != "wal" {
		return cleanup(fmt.Errorf("enable WAL: SQLite returned %q", journalMode))
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return cleanup(contextError(ctx, fmt.Errorf("enable foreign keys: %w", err)))
	}
	if err := migrate(ctx, db, schemaMigrations); err != nil {
		return cleanup(err)
	}
	if err := opened.secureFiles(); err != nil {
		return cleanup(err)
	}
	return opened, nil
}

// Path returns the metadata database path. It never identifies provider-native state.
func (store *Store) Path() string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.dbPath
}

// StateDir returns the restrictive TaskAI metadata state directory.
func (store *Store) StateDir() string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.stateDir
}

// Close cleanly closes the database. It is safe and idempotent.
func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	closeErr := store.db.Close()
	permissionErr := store.secureFilesUnlocked()
	return errors.Join(closeErr, permissionErr)
}

// UserVersion returns SQLite PRAGMA user_version through a cancellable query.
func (store *Store) UserVersion(ctx context.Context) (int, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return 0, ErrClosed
	}
	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, contextError(ctx, err)
	}
	return version, nil
}

func databaseDSN(path string) string {
	value := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("_txlock", "immediate")
	query["_pragma"] = []string{
		"busy_timeout(" + strconv.FormatInt(BusyTimeout.Milliseconds(), 10) + ")",
		"foreign_keys(1)",
		"synchronous(FULL)",
	}
	value.RawQuery = query.Encode()
	return value.String()
}

func ensureStateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: state directory is not a real directory", ErrUnsafeStatePath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect state directory: %w", err)
	} else if err := os.MkdirAll(path, stateDirectoryMode); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(path, stateDirectoryMode); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	return nil
}

// ensureDatabaseFile rejects an observed symlink/non-file. Descriptor-relative
// no-follow traversal that closes the remaining Lstat/Open race is deferred to
// the coordinated M09 filesystem-hardening packet.
func ensureDatabaseFile(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: database is not a regular file", ErrUnsafeStatePath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect metadata database: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, stateFileMode)
	if err != nil {
		return fmt.Errorf("create metadata database: %w", err)
	}
	closeErr := file.Close()
	chmodErr := os.Chmod(path, stateFileMode)
	return errors.Join(closeErr, chmodErr)
}

func (store *Store) secureFiles() error {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.secureFilesUnlocked()
}

func (store *Store) secureFilesUnlocked() error {
	if err := os.Chmod(store.stateDir, stateDirectoryMode); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	for _, path := range []string{store.dbPath, store.dbPath + "-wal", store.dbPath + "-shm"} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect state file: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrUnsafeStatePath, filepath.Base(path))
		}
		if err := os.Chmod(path, stateFileMode); err != nil {
			return fmt.Errorf("secure %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func (store *Store) afterWrite(ctx context.Context, err error) error {
	permissionErr := store.secureFilesUnlocked()
	return contextError(ctx, errors.Join(err, permissionErr))
}

func contextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, err)
	}
	return err
}

func classifyWriteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sqlite3.CONSTRAINT) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}

func withTx(ctx context.Context, db *sql.DB, operation func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return contextError(ctx, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	if err = operation(tx); err != nil {
		return err
	}
	err = tx.Commit()
	return contextError(ctx, err)
}
