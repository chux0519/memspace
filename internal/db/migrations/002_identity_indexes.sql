-- M2: lookups used by idempotent project/session resolution and sync status.
CREATE INDEX IF NOT EXISTS idx_projects_git_identity ON projects (user_id, canonical_git_identity);
CREATE INDEX IF NOT EXISTS idx_projects_name ON projects (user_id, name);
CREATE INDEX IF NOT EXISTS idx_events_device_occurred ON events (device_id, occurred_at);
