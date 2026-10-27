-- Devices can ask to join instead of presenting an invite. The device shows
-- a short code; an administrator compares it and approves the request (web
-- UI or `memspace server approve`); the device then claims its token with a
-- secret only it holds. The token is created at claim time, so no token is
-- ever stored in plaintext.
CREATE TABLE device_requests (
    request_id     TEXT PRIMARY KEY,
    user_code      TEXT NOT NULL,                 -- e.g. K7QF-2M9P, shown on the device and in the UI
    claim_hash     TEXT NOT NULL UNIQUE,          -- SHA-256 of the device's claim secret
    name           TEXT NOT NULL,
    platform       TEXT NOT NULL,
    remote_addr    TEXT NOT NULL DEFAULT '',
    client_version TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,                 -- pending: created + TTL; approved: decided + TTL
    status         TEXT NOT NULL DEFAULT 'pending', -- pending | approved | denied | claimed
    decided_at     TEXT,
    device_id      TEXT REFERENCES devices(device_id)
);

CREATE INDEX idx_device_requests_code ON device_requests (user_code, status);
