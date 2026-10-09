-- One-time codes that stand in for the authenticator app when it is lost.
-- Only a hash is stored; the codes are shown once, when they are generated.
CREATE TABLE IF NOT EXISTS recovery_codes (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT    NOT NULL,
    used_at   DATETIME,
    UNIQUE (user_id, code_hash)
);

CREATE INDEX IF NOT EXISTS idx_recovery_user ON recovery_codes(user_id);
