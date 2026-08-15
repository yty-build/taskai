CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT,
    status TEXT NOT NULL,
    external_ref TEXT,
    metadata_json TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),

    role TEXT,
    agent TEXT NOT NULL,
    provider TEXT,

    repo_path TEXT,
    worktree_path TEXT,
    branch TEXT,

    acp_session_id TEXT,
    native_session_id TEXT,

    status TEXT NOT NULL,

    worker_pid INTEGER,
    worker_socket TEXT,
    log_path TEXT,
    metadata_json TEXT,

    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    last_activity_at TEXT
);

CREATE INDEX sessions_task_id_idx ON sessions(task_id);
CREATE INDEX sessions_status_idx ON sessions(status);
