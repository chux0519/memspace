-- M3: durable job queue (PRD §8.1) and rule-based session summaries (§8.2).

CREATE TABLE IF NOT EXISTS jobs (
    job_id      INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,                  -- e.g. session.summarize
    dedup_key   TEXT NOT NULL UNIQUE,           -- one live job per key
    payload     TEXT NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'queued', -- queued | running | done | failed
    run_after   TEXT NOT NULL,
    queued_at   TEXT NOT NULL,                  -- first enqueue since last completion (bounds debouncing)
    attempts    INTEGER NOT NULL DEFAULT 0,
    lease_until TEXT,
    lease_token TEXT,
    last_error  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_ready ON jobs (status, run_after);

CREATE TABLE IF NOT EXISTS session_summaries (
    session_id        TEXT PRIMARY KEY REFERENCES sessions(session_id),
    project_id        TEXT NOT NULL REFERENCES projects(project_id),
    status            TEXT NOT NULL,              -- active | ended | interrupted
    objective         TEXT NOT NULL DEFAULT '',
    latest_request    TEXT NOT NULL DEFAULT '',
    completed_work    TEXT NOT NULL DEFAULT '',
    changed_files     TEXT NOT NULL DEFAULT '[]', -- JSON array of paths
    test_results      TEXT NOT NULL DEFAULT '[]', -- JSON array
    unresolved_issues TEXT NOT NULL DEFAULT '[]', -- JSON array
    next_steps        TEXT NOT NULL DEFAULT '[]', -- JSON array
    evidence          TEXT NOT NULL DEFAULT '[]', -- JSON array of event ids
    event_count       INTEGER NOT NULL,
    first_event_at    TEXT NOT NULL,
    last_event_at     TEXT NOT NULL,
    generator         TEXT NOT NULL,
    generated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_summaries_project ON session_summaries (project_id, last_event_at);
