-- M4: browser access to the web UI (PRD §12: the UI requires authentication).
-- A device (or the server operator) creates a short-lived single-use login
-- link; opening it exchanges the code for an HttpOnly session cookie. Only
-- hashes are stored. Sessions created through a device die with it.
CREATE TABLE IF NOT EXISTS ui_login_links (
    link_hash            TEXT PRIMARY KEY,
    user_id              TEXT NOT NULL REFERENCES users(user_id),
    device_id            TEXT REFERENCES devices(device_id),
    created_at           TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    used_at              TEXT
);

CREATE TABLE IF NOT EXISTS ui_sessions (
    session_hash         TEXT PRIMARY KEY,
    user_id              TEXT NOT NULL REFERENCES users(user_id),
    device_id            TEXT REFERENCES devices(device_id),
    user_agent           TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    last_seen_at         TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    revoked_at           TEXT
);
