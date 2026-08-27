package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestMigrationFromVersionZeroIsIdempotentAndPreservesMetadata(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	first, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	createTask(t, first, "task_migration", "exact migration title", fixedTime)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	version, err := second.UserVersion(context.Background())
	if err != nil || version != SchemaVersion {
		t.Fatalf("version after reopen = %d, %v", version, err)
	}
	stored, err := second.GetTask(context.Background(), "task_migration")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "exact migration title" || !stored.CreatedAt.Equal(fixedTime) {
		t.Fatalf("metadata changed across migration reopen: %#v", stored)
	}
}

func TestMigrationRejectsFutureVersionWithoutChangingDatabase(t *testing.T) {
	t.Parallel()

	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	createTask(t, store, "task_future", "future", fixedTime)
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	raw := openRawDatabase(t, path)
	if _, err := raw.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), stateDir); !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("future version error = %v, want ErrUnsupportedSchemaVersion", err)
	}

	raw = openRawDatabase(t, path)
	defer raw.Close()
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("future version was changed to %d", version)
	}
	var title string
	if err := raw.QueryRow(`SELECT title FROM tasks WHERE id = 'task_future'`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "future" {
		t.Fatalf("future database metadata changed: %q", title)
	}
}

func TestFailedMigrationRollsBackSchemaAndVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rollback.sqlite3")
	db := openRawDatabase(t, path)
	defer db.Close()
	broken := []migration{{
		version: 1,
		statements: []string{
			`CREATE TABLE rollback_probe (id INTEGER PRIMARY KEY)`,
			`INSERT INTO definitely_missing_table(value) VALUES (1)`,
			`PRAGMA user_version = 1`,
		},
	}}
	if err := migrate(context.Background(), db, broken); err == nil {
		t.Fatal("broken migration unexpectedly succeeded")
	}
	var tableCount, version int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'rollback_probe'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 || version != 0 {
		t.Fatalf("failed migration left table=%d version=%d", tableCount, version)
	}
}

func TestMigrationRejectsNonContiguousPlanWithoutMutation(t *testing.T) {
	t.Parallel()

	db := openRawDatabase(t, filepath.Join(t.TempDir(), "noncontiguous.sqlite3"))
	defer db.Close()
	plan := []migration{{version: 2, statements: []string{`CREATE TABLE must_not_exist (id INTEGER)`}}}
	if err := migrate(context.Background(), db, plan); !errors.Is(err, ErrUnsupportedSchemaVersion) {
		t.Fatalf("non-contiguous error = %v, want ErrUnsupportedSchemaVersion", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'must_not_exist'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("non-contiguous migration mutated database")
	}
}

func TestFreshMigrationsProduceDeterministicSchema(t *testing.T) {
	t.Parallel()

	first := openTestStore(t)
	second := openTestStore(t)
	if got, want := schemaFingerprint(t, first.db), schemaFingerprint(t, second.db); got != want {
		t.Fatalf("fresh schema differs:\nfirst:\n%s\nsecond:\n%s", got, want)
	}
}

func TestMigrationHonorsCancelledContext(t *testing.T) {
	t.Parallel()

	db := openRawDatabase(t, filepath.Join(t.TempDir(), "cancelled.sqlite3"))
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := migrate(ctx, db, schemaMigrations); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled migration error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cancelled migration created %d product tables", count)
	}
}

func openRawDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", databaseDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func schemaFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`
		SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE sql IS NOT NULL
		ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := ""
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		result += fmt.Sprintf("%s|%s|%s|%s\n", kind, name, table, statement)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
