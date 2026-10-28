-- A client that cannot upgrade itself to the server's version reports the
-- failure with its spool state, so the web UI can show it.
ALTER TABLE devices ADD COLUMN upgrade_target TEXT;
ALTER TABLE devices ADD COLUMN upgrade_error TEXT;
