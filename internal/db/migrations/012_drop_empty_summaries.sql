-- Sessions without a user prompt (an agent opened and closed, or a CLI
-- subcommand such as `claude mcp list` firing SessionEnd) no longer get a
-- summary. Remove the empty summaries stored before, with their search
-- documents (FTS rows and embedding chunks included).
CREATE TEMP TABLE empty_sessions AS
    SELECT s.session_id FROM sessions s
    WHERE NOT EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.session_id AND e.event_type = 'user.prompt');

CREATE TEMP TABLE empty_docs AS
    SELECT doc_id, title, body, files FROM search_docs
    WHERE kind = 'summary' AND ref_id IN (SELECT session_id FROM empty_sessions);

INSERT OR IGNORE INTO embedding_orphans (chunk_id, space_id)
    SELECT chunk_id, space_id FROM embedding_chunks WHERE doc_id IN (SELECT doc_id FROM empty_docs);
DELETE FROM embedding_chunks WHERE doc_id IN (SELECT doc_id FROM empty_docs);
INSERT INTO search_fts (search_fts, rowid, title, body, files)
    SELECT 'delete', doc_id, title, body, files FROM empty_docs;
DELETE FROM search_docs WHERE doc_id IN (SELECT doc_id FROM empty_docs);
DELETE FROM session_summaries WHERE session_id IN (SELECT session_id FROM empty_sessions);

DROP TABLE empty_docs;
DROP TABLE empty_sessions;
