-- M1 review fixes.

-- Every authenticated request looks a device up by token hash.
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_token_hash ON devices (token_hash);

-- One project per Git identity, enforced by the database rather than by
-- the single-connection pool happening to serialize EnsureProject.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_user_git_identity_unique
    ON projects (user_id, canonical_git_identity) WHERE canonical_git_identity IS NOT NULL;

-- One-time registration invites (`memspace server invite`). Only the
-- SHA-256 of the code is stored.
CREATE TABLE IF NOT EXISTS invites (
    invite_hash       TEXT PRIMARY KEY,
    user_id           TEXT NOT NULL REFERENCES users(user_id),
    created_at        TEXT NOT NULL,
    expires_at        TEXT NOT NULL,
    used_at           TEXT,
    used_by_device_id TEXT REFERENCES devices(device_id)
);
