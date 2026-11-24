-- Per-tenant BYO-storage backup destinations (S3, SFTP, local) and
-- Restic snapshot metadata.

CREATE TABLE IF NOT EXISTS backup_destinations (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('s3', 'sftp', 'local')),
    repo_url          TEXT NOT NULL,
    repo_password     TEXT NOT NULL,
    s3_access_key     TEXT NOT NULL DEFAULT '',
    s3_secret_key     TEXT NOT NULL DEFAULT '',
    s3_region         TEXT NOT NULL DEFAULT '',
    ssh_private_key   TEXT NOT NULL DEFAULT '',
    ssh_public_key    TEXT NOT NULL DEFAULT '',
    ssh_known_hosts   TEXT NOT NULL DEFAULT '',
    keep_last         INTEGER NOT NULL DEFAULT 7,
    keep_daily        INTEGER NOT NULL DEFAULT 7,
    keep_weekly       INTEGER NOT NULL DEFAULT 4,
    last_checked_at   DATETIME,
    last_check_status TEXT NOT NULL DEFAULT '',
    created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, name)
);

CREATE INDEX IF NOT EXISTS idx_backup_destinations_user ON backup_destinations(user_id);

CREATE TABLE IF NOT EXISTS restic_snapshots (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    destination_id   INTEGER NOT NULL REFERENCES backup_destinations(id) ON DELETE CASCADE,
    instance_name    TEXT NOT NULL,
    snapshot_id      TEXT NOT NULL,
    backup_name      TEXT NOT NULL DEFAULT '',
    repo_prefix      TEXT NOT NULL,
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    data_added_bytes INTEGER NOT NULL DEFAULT 0,
    duration_sec     REAL NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'complete',
    error            TEXT NOT NULL DEFAULT '',
    created_by       TEXT NOT NULL DEFAULT '',
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_restic_snapshots_instance ON restic_snapshots(instance_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_restic_snapshots_dest ON restic_snapshots(destination_id, created_at DESC);
