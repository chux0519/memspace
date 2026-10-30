-- Keep the mixed Chinese/pinyin tokenizer and normalize English inflections.
-- Rebuild only the derived FTS index; source documents and embeddings keep IDs.
DROP TABLE search_fts;
CREATE VIRTUAL TABLE search_fts USING fts5(
    title, body, files,
    content = 'search_docs',
    content_rowid = 'doc_id',
    tokenize = 'porter simple'
);
INSERT INTO search_fts(search_fts) VALUES ('rebuild');
