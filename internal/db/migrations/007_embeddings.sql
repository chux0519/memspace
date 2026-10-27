-- Semantic search: embedding spaces, chunks and orphaned vectors.
--
-- A "space" is one embedding service configuration (fingerprint). Vectors
-- live in a per-space sqlite-vec table (created at runtime, its dimension is
-- only known then). Vectors from different spaces are never compared.

CREATE TABLE IF NOT EXISTS embedding_spaces (
    space_id     INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint  TEXT NOT NULL UNIQUE,     -- canonical JSON of the service identity
    model        TEXT NOT NULL,
    dimensions   INTEGER NOT NULL,
    vec_table    TEXT NOT NULL,
    status       TEXT NOT NULL,            -- building | active | retired
    created_at   TEXT NOT NULL,
    activated_at TEXT
);

-- Chunk ids are never reused (AUTOINCREMENT), so a vector left behind by a
-- replaced document can never be mistaken for a newer chunk.
CREATE TABLE IF NOT EXISTS embedding_chunks (
    chunk_id  INTEGER PRIMARY KEY AUTOINCREMENT,
    space_id  INTEGER NOT NULL REFERENCES embedding_spaces(space_id),
    doc_id    INTEGER NOT NULL REFERENCES search_docs(doc_id),
    chunk_no  INTEGER NOT NULL,
    text      TEXT NOT NULL,
    UNIQUE (space_id, doc_id, chunk_no)
);

CREATE INDEX IF NOT EXISTS idx_embedding_chunks_doc ON embedding_chunks (doc_id);

-- Vectors whose chunk was deleted (document replaced); removed from the
-- space's vec table by the embedding sweep.
CREATE TABLE IF NOT EXISTS embedding_orphans (
    chunk_id INTEGER PRIMARY KEY,
    space_id INTEGER NOT NULL
);
