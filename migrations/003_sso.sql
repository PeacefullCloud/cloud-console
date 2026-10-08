-- SSO providers and linked identities.
--
-- Providers are configured in the Settings UI (admin only). Client secrets
-- are stored encrypted with the key from CONSOLE_SSO_KEY.

-- Per-user sign-in method: 'either' (default), 'password' or 'sso'.
ALTER TABLE users ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'either';

CREATE TABLE IF NOT EXISTS sso_providers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    issuer        TEXT    NOT NULL,
    client_id     TEXT    NOT NULL,
    client_secret TEXT    NOT NULL DEFAULT '',
    button_label  TEXT    NOT NULL DEFAULT '',
    default_role  TEXT    NOT NULL DEFAULT 'viewer',
    require_mfa   INTEGER NOT NULL DEFAULT 1,
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Links a console user to an identity at a provider. Several providers may
-- point at the same user.
CREATE TABLE IF NOT EXISTS user_identities (
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id INTEGER NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    subject     TEXT    NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (provider_id, subject),
    UNIQUE (user_id, provider_id)
);

CREATE INDEX IF NOT EXISTS idx_identities_user ON user_identities(user_id);
