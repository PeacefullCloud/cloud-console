-- Two-factor authentication (TOTP).

-- totp_secret holds the base32 TOTP secret. It is stored as soon as setup
-- starts so the settings page can show it, but it is only enforced once
-- totp_enabled is 1 after the user confirms a code from their app.
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled INTEGER NOT NULL DEFAULT 0;
