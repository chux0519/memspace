-- MemSpace initial schema (M1: Foundation)
-- Tables: users, devices, projects, project_locations, sessions, events

PRAGMA foreign_keys = ON;

-- Single-owner model for v0.1; keeps user_id field for future multi-user upgrade.
CREATE TABLE IF NOT EXISTS users (
    user_id    TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS devices (
    device_id    TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(user_id),
    name         TEXT NOT NULL,
    platform     TEXT NOT NULL,
    token_hash   TEXT NOT NULL,            -- SHA-256 hex of the bearer token
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at TEXT,
    revoked_at   TEXT                       -- set when token rotated / device removed
);

CREATE TABLE IF NOT EXISTS projects (
    project_id            TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL REFERENCES users(user_id),
    name                  TEXT NOT NULL,
    canonical_git_identity TEXT,            -- normalized git remote, nullable
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS project_locations (
    location_id       TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL REFERENCES projects(project_id),
    device_id         TEXT NOT NULL REFERENCES devices(device_id),
    absolute_path     TEXT NOT NULL,
    git_branch        TEXT,
    git_head          TEXT,
    last_observed_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (device_id, absolute_path)
);

CREATE TABLE IF NOT EXISTS sessions (
    session_id         TEXT PRIMARY KEY,
    project_id         TEXT NOT NULL REFERENCES projects(project_id),
    location_id        TEXT REFERENCES project_locations(location_id),
    device_id          TEXT NOT NULL REFERENCES devices(device_id),
    agent_type         TEXT NOT NULL,
    native_session_id  TEXT NOT NULL,
    started_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at           TEXT,
    status             TEXT NOT NULL DEFAULT 'active'   -- active | ended | interrupted
);

-- Same agent native session id may collide across devices; scope uniqueness per device.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_device_native
    ON sessions (device_id, agent_type, native_session_id);

CREATE TABLE IF NOT EXISTS events (
    event_id        TEXT PRIMARY KEY,       -- stable client-supplied id (ULID/UUID)
    session_id      TEXT REFERENCES sessions(session_id),
    project_id      TEXT NOT NULL REFERENCES projects(project_id),
    device_id       TEXT NOT NULL REFERENCES devices(device_id),
    event_type      TEXT NOT NULL,
    occurred_at     TEXT NOT NULL,
    received_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    sequence_number INTEGER,
    payload_json    TEXT NOT NULL,
    content_hash    TEXT,
    schema_version  INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_events_session ON events (session_id);
CREATE INDEX IF NOT EXISTS idx_events_project_time ON events (project_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_events_device_time ON events (device_id, received_at);
