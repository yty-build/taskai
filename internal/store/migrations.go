package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type migration struct {
	version    int
	statements []string
}

var schemaMigrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE tasks (
				id TEXT PRIMARY KEY NOT NULL
					CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
				title TEXT NOT NULL
					CHECK(length(CAST(title AS BLOB)) BETWEEN 1 AND 512 AND instr(title, char(0)) = 0),
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at)
			) STRICT, WITHOUT ROWID`,
			`CREATE TABLE sessions (
				id TEXT PRIMARY KEY NOT NULL
					CHECK(length(CAST(id AS BLOB)) BETWEEN 1 AND 128),
				task_id TEXT NOT NULL,
				parent_session_id TEXT,
				role TEXT NOT NULL CHECK(role IN ('manager', 'subordinate')),
				label TEXT NOT NULL
					CHECK(length(CAST(label AS BLOB)) <= 512 AND instr(label, char(0)) = 0),
				provider_id TEXT NOT NULL
					CHECK(length(CAST(provider_id AS BLOB)) BETWEEN 1 AND 128),
				provider_reference BLOB
					CHECK(provider_reference IS NULL OR length(provider_reference) BETWEEN 1 AND 4096),
				state TEXT NOT NULL
					CHECK(state IN ('starting', 'active', 'dormant', 'interrupted', 'closed')),
				created_at INTEGER NOT NULL,
				last_active_at INTEGER NOT NULL CHECK(last_active_at >= created_at),
				UNIQUE(id, task_id),
				FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE,
				FOREIGN KEY(parent_session_id, task_id)
					REFERENCES sessions(id, task_id) ON DELETE CASCADE
			) STRICT, WITHOUT ROWID`,
			`CREATE TABLE provider_settings (
				provider_id TEXT PRIMARY KEY NOT NULL
					CHECK(length(CAST(provider_id AS BLOB)) BETWEEN 1 AND 128),
				enabled INTEGER NOT NULL CHECK(enabled IN (0, 1)),
				probe_status TEXT NOT NULL
					CHECK(probe_status IN ('unknown', 'checking', 'connected', 'not_connected', 'disconnected', 'needs_repair', 'missing_connector')),
				launch_config_json TEXT NOT NULL
					CHECK(
						json_valid(launch_config_json)
						AND json_type(launch_config_json) IS 'object'
						AND json_type(launch_config_json, '$.argv') IS 'array'
						AND json_array_length(launch_config_json, '$.argv') BETWEEN 0 AND 64
						AND json_remove(launch_config_json, '$.argv') = '{}'
					),
				can_manager INTEGER NOT NULL CHECK(can_manager IN (0, 1)),
				can_subordinate INTEGER NOT NULL CHECK(can_subordinate IN (0, 1)),
				can_resume INTEGER NOT NULL CHECK(can_resume IN (0, 1)),
				can_replay INTEGER NOT NULL CHECK(can_replay IN (0, 1))
			) STRICT, WITHOUT ROWID`,
			`CREATE INDEX tasks_newest_created ON tasks(created_at DESC, id ASC)`,
			`CREATE INDEX sessions_by_task_state ON sessions(task_id, state)`,
			`CREATE INDEX sessions_by_task_created ON sessions(task_id, created_at ASC, id ASC)`,
			`CREATE INDEX sessions_by_parent_created ON sessions(parent_session_id, created_at ASC, id ASC)`,
			`CREATE INDEX provider_settings_enabled ON provider_settings(enabled, provider_id ASC)`,
			`CREATE TRIGGER sessions_validate_hierarchy_insert
			BEFORE INSERT ON sessions
			BEGIN
				SELECT CASE
					WHEN NEW.role = 'manager' AND NEW.parent_session_id IS NOT NULL
						THEN RAISE(ABORT, 'manager cannot have parent')
					WHEN NEW.role = 'subordinate' AND NEW.parent_session_id IS NULL
						THEN RAISE(ABORT, 'subordinate requires parent')
					WHEN NEW.role = 'subordinate' AND NOT EXISTS (
						SELECT 1 FROM sessions AS parent
						WHERE parent.id = NEW.parent_session_id
							AND parent.task_id = NEW.task_id
							AND parent.role = 'manager'
							AND parent.parent_session_id IS NULL
					)
						THEN RAISE(ABORT, 'subordinate parent must be same-Task manager')
				END;
			END`,
			`CREATE TRIGGER sessions_identity_immutable
			BEFORE UPDATE OF task_id, parent_session_id, role, provider_id ON sessions
			WHEN NEW.task_id IS NOT OLD.task_id
				OR NEW.parent_session_id IS NOT OLD.parent_session_id
				OR NEW.role IS NOT OLD.role
				OR NEW.provider_id IS NOT OLD.provider_id
			BEGIN
				SELECT RAISE(ABORT, 'Session identity is immutable');
			END`,
			`CREATE TRIGGER sessions_provider_reference_immutable
			BEFORE UPDATE OF provider_reference ON sessions
			WHEN OLD.provider_reference IS NOT NULL
				AND (NEW.provider_reference IS NULL OR NEW.provider_reference != OLD.provider_reference)
			BEGIN
				SELECT RAISE(ABORT, 'provider reference is immutable');
			END`,
			`PRAGMA user_version = 1`,
		},
	},
}

func migrate(ctx context.Context, db *sql.DB, migrations []migration) error {
	return withTx(ctx, db, func(tx *sql.Tx) error {
		var current int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
			return contextError(ctx, fmt.Errorf("read schema version: %w", err))
		}
		if current > len(migrations) {
			return fmt.Errorf("%w: database=%d supported=%d", ErrUnsupportedSchemaVersion, current, len(migrations))
		}
		for _, migration := range migrations {
			if migration.version <= current {
				continue
			}
			if migration.version != current+1 {
				return fmt.Errorf("%w: non-contiguous migration %d after %d", ErrUnsupportedSchemaVersion, migration.version, current)
			}
			for index, statement := range migration.statements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return contextError(ctx, fmt.Errorf("migration %d statement %d: %w", migration.version, index+1, err))
				}
			}
			current = migration.version
		}
		if current != len(migrations) {
			return errors.New("metadata migrations did not reach current schema")
		}
		return nil
	})
}
