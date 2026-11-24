-- Admin-managed pool of dedicated public IPs and their instance assignments.

CREATE TABLE IF NOT EXISTS dedicated_ips (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    public_ip     TEXT NOT NULL UNIQUE,
    listen_ip     TEXT NOT NULL DEFAULT '',
    network       TEXT NOT NULL DEFAULT 'incusbr0',
    label         TEXT NOT NULL DEFAULT '',
    instance_name TEXT NOT NULL DEFAULT '',
    target_ip     TEXT NOT NULL DEFAULT '',
    snat          INTEGER NOT NULL DEFAULT 1,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    assigned_at   DATETIME
);

CREATE INDEX IF NOT EXISTS idx_dedicated_ips_instance ON dedicated_ips(instance_name);
