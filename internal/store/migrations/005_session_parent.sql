DROP INDEX IF EXISTS sessions_task_prepare_request_unique;

ALTER TABLE sessions ADD COLUMN parent_session_id TEXT REFERENCES sessions(id);

CREATE INDEX sessions_parent_session_id_idx ON sessions(parent_session_id);
