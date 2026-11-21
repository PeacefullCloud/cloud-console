-- Multi-tenant isolation.
--
-- Instances already carry owner_id. Jobs and activity entries need the same
-- column so a tenant's job tracker and audit log cannot show another tenant's
-- work. users.max_instances is a per-owner quota; 0 means unlimited and the
-- host-wide CONSOLE_MAX_INSTANCES limit still applies on top.

ALTER TABLE users ADD COLUMN max_instances INTEGER NOT NULL DEFAULT 0;

ALTER TABLE jobs ADD COLUMN owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE activity ADD COLUMN owner_id INTEGER REFERENCES users(id) ON DELETE SET NULL;

-- Backfill the subject of a job or activity entry: the instance owner when the
-- target is a known instance, otherwise the acting user. Entries that resolve
-- to neither stay unowned and are visible to administrators only.
UPDATE jobs
SET owner_id = COALESCE(
    (SELECT i.owner_id FROM instances i WHERE i.name = jobs.target),
    (SELECT u.id FROM users u WHERE u.username = jobs.created_by)
)
WHERE owner_id IS NULL;

UPDATE activity
SET owner_id = COALESCE(
    (SELECT i.owner_id FROM instances i WHERE i.name = activity.target),
    (SELECT u.id FROM users u WHERE u.username = activity.username)
)
WHERE owner_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_instances_owner ON instances(owner_id);
CREATE INDEX IF NOT EXISTS idx_jobs_owner ON jobs(owner_id);
CREATE INDEX IF NOT EXISTS idx_activity_owner ON activity(owner_id);
