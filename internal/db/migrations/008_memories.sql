-- M4: memories (PRD §5.7). v0.1 generates them with rules (PRD §8.3):
-- an episodic memory per session summary, and procedural "fix experiences"
-- (a failure, file changes, then the same operation succeeding).
CREATE TABLE IF NOT EXISTS memories (
    memory_id   TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(project_id),
    session_id  TEXT REFERENCES sessions(session_id),
    dedup_key   TEXT NOT NULL UNIQUE,     -- one memory per rule and source
    scope       TEXT NOT NULL DEFAULT 'project',   -- project | global
    type        TEXT NOT NULL,            -- observation | episodic | semantic | procedural | decision | progress
    title       TEXT NOT NULL,
    content     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'candidate', -- candidate | verified | superseded | retracted
    confidence  REAL NOT NULL,
    source_refs TEXT NOT NULL DEFAULT '[]', -- JSON [{"kind": "event"|"session", "id": "..."}]
    generator   TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memories_project_updated ON memories (project_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memories_type ON memories (type, status);

-- Clients report their local spool after each sync, so the server can show
-- devices that still hold unsent events (PRD §2.5 scenario D).
ALTER TABLE devices ADD COLUMN spool_pending INTEGER;
ALTER TABLE devices ADD COLUMN spool_dead INTEGER;
ALTER TABLE devices ADD COLUMN spool_dropped INTEGER;
ALTER TABLE devices ADD COLUMN spool_oldest_pending_at TEXT;
ALTER TABLE devices ADD COLUMN spool_reported_at TEXT;
ALTER TABLE devices ADD COLUMN client_version TEXT;
