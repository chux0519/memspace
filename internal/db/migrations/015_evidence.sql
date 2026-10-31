-- Optional evidence projections. Original events remain authoritative; the
-- journal freezes mutable source versions and relation assertions/retractions.
CREATE TABLE evidence_records (
 record_seq INTEGER PRIMARY KEY AUTOINCREMENT,
 record_id TEXT NOT NULL UNIQUE,
 project_id TEXT NOT NULL REFERENCES projects(project_id),
 actor_kind TEXT NOT NULL,
 source_event_id TEXT,
 kind TEXT NOT NULL,
 payload_json TEXT NOT NULL,
 payload_sha TEXT NOT NULL,
 recorded_at TEXT NOT NULL
);
CREATE TABLE evidence_objects (
 object_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(project_id),
 kind TEXT NOT NULL,
 ref_id TEXT NOT NULL,
 revision_sha TEXT NOT NULL,
 body_sha TEXT NOT NULL,
 source_kind TEXT NOT NULL,
 source_id TEXT NOT NULL,
 field_path TEXT NOT NULL,
 availability TEXT NOT NULL,
 session_id TEXT NOT NULL DEFAULT '',
 device_id TEXT NOT NULL DEFAULT '',
 role TEXT NOT NULL DEFAULT '',
 turn_key TEXT NOT NULL DEFAULT '',
 prompt_key TEXT NOT NULL DEFAULT '',
 native_message_id TEXT NOT NULL DEFAULT '',
 parent_message_id TEXT NOT NULL DEFAULT '',
 batch_key TEXT NOT NULL DEFAULT '',
 source_ordinal INTEGER,
 source_count INTEGER,
 occurred_at TEXT NOT NULL,
 is_current INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX idx_evidence_ref ON evidence_objects(project_id,kind,ref_id,revision_sha);
CREATE INDEX idx_evidence_turn ON evidence_objects(project_id,session_id,device_id,turn_key,prompt_key);
CREATE INDEX idx_evidence_message ON evidence_objects(project_id,session_id,device_id,native_message_id);
CREATE INDEX idx_evidence_parent ON evidence_objects(project_id,session_id,device_id,parent_message_id);
CREATE INDEX idx_evidence_batch ON evidence_objects(project_id,session_id,device_id,batch_key,source_ordinal);
CREATE TABLE evidence_links (
 link_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(project_id),
 relation TEXT NOT NULL,
 from_object_id TEXT NOT NULL REFERENCES evidence_objects(object_id),
 to_object_id TEXT NOT NULL REFERENCES evidence_objects(object_id),
 basis TEXT NOT NULL,
 status TEXT NOT NULL,
 rule_version TEXT NOT NULL,
 scope_json TEXT NOT NULL DEFAULT '{}',
 witness_refs TEXT NOT NULL DEFAULT '[]',
 assertion_record_id TEXT NOT NULL REFERENCES evidence_records(record_id)
);
CREATE INDEX idx_evidence_from ON evidence_links(project_id,from_object_id,relation,status);
CREATE INDEX idx_evidence_to ON evidence_links(project_id,to_object_id,relation,status);
