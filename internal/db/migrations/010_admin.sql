-- Web UI administration. Login links carry a role: links made on the server
-- machine (`memspace server login-link`) open admin sessions, links made
-- with a device token (`memspace ui`) open read-only viewer sessions, so a
-- leaked device token cannot mint invites or revoke other devices.
ALTER TABLE ui_login_links ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer';
ALTER TABLE ui_sessions ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer';

-- Invites created from the UI carry a note (the intended device), and can be
-- withdrawn before use.
ALTER TABLE invites ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE invites ADD COLUMN revoked_at TEXT;
