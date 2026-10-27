-- M3: full-text search (PRD §7.2).
-- search_docs holds derived, short-form documents (not raw event JSON); the
-- FTS5 index uses the statically linked `simple` tokenizer (per-character
-- Chinese + pinyin; queries are segmented with jieba).

CREATE TABLE IF NOT EXISTS search_docs (
    doc_id      INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,               -- prompt | reply | failure | command | change | summary
    ref_id      TEXT NOT NULL,               -- event_id, or session_id for summaries
    project_id  TEXT NOT NULL REFERENCES projects(project_id),
    session_id  TEXT REFERENCES sessions(session_id),
    device_id   TEXT REFERENCES devices(device_id),
    agent_type  TEXT,
    occurred_at TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    files       TEXT NOT NULL DEFAULT '',
    UNIQUE (kind, ref_id)
);

CREATE INDEX IF NOT EXISTS idx_search_docs_project_time ON search_docs (project_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_search_docs_session ON search_docs (session_id);

CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(
    title, body, files,
    content = 'search_docs',
    content_rowid = 'doc_id',
    tokenize = 'simple'
);
