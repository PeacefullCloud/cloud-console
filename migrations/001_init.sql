-- Peaceful Cloud Console — initial schema.
--
-- Only console metadata lives here. Incus stays the source of truth for
-- infrastructure state: CPU/RAM in use, IPs, running state, snapshots,
-- storage and networks are always read live from Incus.

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'operator',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_login_at DATETIME
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    user_agent TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- Console-side record of created instances.
CREATE TABLE IF NOT EXISTS instances (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    image        TEXT    NOT NULL DEFAULT '',
    image_label  TEXT    NOT NULL DEFAULT '',
    kind         TEXT    NOT NULL DEFAULT 'container',
    cpu          INTEGER NOT NULL DEFAULT 0,
    memory_mb    INTEGER NOT NULL DEFAULT 0,
    disk_gb      INTEGER NOT NULL DEFAULT 0,
    storage_pool TEXT    NOT NULL DEFAULT '',
    owner_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    primary_host TEXT    NOT NULL DEFAULT '',
    notes        TEXT    NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS domains (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_name TEXT    NOT NULL,
    domain        TEXT    NOT NULL UNIQUE,
    port          INTEGER NOT NULL DEFAULT 80,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_domains_instance ON domains(instance_name);

CREATE TABLE IF NOT EXISTS snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_name TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    note          TEXT    NOT NULL DEFAULT '',
    created_by    TEXT    NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(instance_name, name)
);

CREATE TABLE IF NOT EXISTS backups (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_name TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    target        TEXT    NOT NULL DEFAULT 'local',
    s3_key        TEXT    NOT NULL DEFAULT '',
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL DEFAULT 'pending',
    error         TEXT    NOT NULL DEFAULT '',
    created_by    TEXT    NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(instance_name, name)
);

CREATE INDEX IF NOT EXISTS idx_backups_instance ON backups(instance_name);

CREATE TABLE IF NOT EXISTS activity (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    username TEXT NOT NULL DEFAULT '',
    action   TEXT NOT NULL,
    target   TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT '',
    status   TEXT NOT NULL DEFAULT 'ok'
);

CREATE INDEX IF NOT EXISTS idx_activity_ts ON activity(ts DESC);

CREATE TABLE IF NOT EXISTS jobs (
    id         TEXT    PRIMARY KEY,
    kind       TEXT    NOT NULL,
    target     TEXT    NOT NULL DEFAULT '',
    payload    TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL DEFAULT 'queued',
    progress   INTEGER NOT NULL DEFAULT 0,
    message    TEXT    NOT NULL DEFAULT '',
    error      TEXT    NOT NULL DEFAULT '',
    created_by TEXT    NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at DATETIME,
    ended_at   DATETIME
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

-- Rolling monitoring samples. Incus only exposes live counters, so history is
-- sampled by the console's monitor.
CREATE TABLE IF NOT EXISTS metrics (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    instance_name TEXT    NOT NULL,
    ts            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    cpu_pct       REAL    NOT NULL DEFAULT 0,
    mem_used      INTEGER NOT NULL DEFAULT 0,
    mem_total     INTEGER NOT NULL DEFAULT 0,
    disk_used     INTEGER NOT NULL DEFAULT 0,
    disk_total    INTEGER NOT NULL DEFAULT 0,
    net_rx_bytes  INTEGER NOT NULL DEFAULT 0,
    net_tx_bytes  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_metrics_instance_ts ON metrics(instance_name, ts DESC);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
