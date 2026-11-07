CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    repo TEXT NOT NULL,
    base_ref TEXT NOT NULL,
    branch TEXT NOT NULL,
    worktree TEXT NOT NULL,
    backend TEXT NOT NULL,
    model TEXT NOT NULL,
    session_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'awaiting_permission', 'finished', 'failed', 'stopped')),
    prompts TEXT NOT NULL,
    permissions TEXT NOT NULL,
    error TEXT NOT NULL,
    summary TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE events (
    run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    time TEXT NOT NULL,
    type TEXT NOT NULL,
    data TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);
