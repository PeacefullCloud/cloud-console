-- Tenant-isolated SSH public keys.
--
-- Only the public half of a key is stored. When the console generates a
-- keypair the private key is returned to the user once and is never written
-- anywhere. owner_id ties every key to exactly one console account, so one
-- tenant can never list, read, rename, delete or assign another tenant's key.

CREATE TABLE IF NOT EXISTS ssh_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(owner_id, name)
);

CREATE INDEX IF NOT EXISTS idx_ssh_keys_owner ON ssh_keys(owner_id);
