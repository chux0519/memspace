-- Pending assertions are a projection of immutable, authenticated events.
-- They can arrive before their source endpoints or before a retracted assertion.
CREATE TABLE evidence_inputs (
 event_id TEXT PRIMARY KEY REFERENCES events(event_id),
 project_id TEXT NOT NULL REFERENCES projects(project_id),
 operation TEXT NOT NULL,
 target_event_id TEXT NOT NULL DEFAULT '',
 from_ref TEXT NOT NULL DEFAULT '',
 to_ref TEXT NOT NULL DEFAULT '',
 witness_refs TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL,
 diagnostic TEXT NOT NULL DEFAULT '',
 link_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_evidence_input_pending ON evidence_inputs(project_id,status,from_ref,to_ref);
CREATE INDEX idx_evidence_input_target ON evidence_inputs(project_id,operation,target_event_id);
CREATE INDEX idx_evidence_input_link ON evidence_inputs(project_id,link_id,status);
