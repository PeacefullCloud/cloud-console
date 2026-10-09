# Code audit: fix list

Source: code audit of 2026-10-09. This file is the working list. Items are
fixed in order; tick each box only when the fix is in, tested, and
`go vet ./... && go test -race ./...` is green.

Legend: `[x]` fixed, `[ ]` open.

## Pass 1: high-priority fixes (in scope now)

### F1. SSO account takeover by username match
- [x] **Problem.** `resolveSSOUser` linked an IdP identity to any existing
  console user whose username equalled the IdP's `preferred_username`, `name`
  or `email`. On an IdP where users choose their display name, someone could
  sign in as `admin`. The link was also saved before the sign-in-method check.
- [x] **Fix.**
  - Never link by username. An unknown identity gets a *new* account, with a
    de-duplicated username (`admin` becomes `admin-2`).
  - Existing users link an identity explicitly: signed in, Settings, "Link"
    (`POST /auth/sso/link/{provider}`). The link completes only if the browser
    still holds the same session.
  - Bind the OIDC `state` to the browser with a cookie. This also closes SSO
    login CSRF (an attacker feeding a victim their own callback URL).
  - Auto-provisioned users get the provider's `default_role` (it was stored but
    ignored; everyone became a viewer).

### F2. Login and 2FA brute force
- [x] **Problem.** A wrong TOTP code returned a *fresh* challenge, so a caller
  who knew the password could guess 6-digit codes indefinitely. There was no
  rate limit anywhere, and a TOTP code could be replayed inside its window.
- [x] **Fix.**
  - Sliding-window throttle on password sign-in, per username and per IP.
  - Per-challenge attempt cap (5). Exhausting it forces a new password step.
    A wrong code no longer mints a new challenge.
  - Per-user TOTP failure throttle that survives re-login.
  - One-time use of each TOTP time step (replay protection).
  - Limits: 8 wrong passwords per username and 40 per IP, 6 wrong codes per
    user, all per 15 minutes. State is in memory (resets on restart).
  - The IP counter relies on `clientIP`, fixed in B1.

### F3. Path traversal in backup delete
- [x] **Problem.** `{name}` and `{backup}` from the URL reached
  `filepath.Join(DataDir, "backups", name, backup+".tar.gz")` unvalidated, and
  the router decodes `%2F`. Result: delete any `*.tar.gz` the console can reach.
- [x] **Fix.**
  - Route-level validation of `{name}`, `{snapshot}`, `{backup}` (one wrapper,
    so a future route cannot forget it).
  - `archivePath` rejects unsafe segments and checks the result stays under
    the backups directory. Defense in depth.

### F4. Stop and restart were hard kills
- [x] **Problem.** `Stop` and `Restart` sent `force=true`, which in the Incus
  API means "kill now", not "force after the timeout".
- [x] **Fix.**
  - Stop and Reboot are clean (`force=false`, 2 minute timeout). If the guest
    ignores the request the error says so and points at Force stop. The console
    never escalates to a kill by itself.
  - Explicit "Force stop" action (confirmation dialog) on the instance page and
    card menu.

### F5. Restore destroyed the live instance first
- [x] **Problem.** `handleRestore` deleted the existing instance, then
  imported. A corrupt or failed import left nothing.
- [x] **Fix.**
  - Import under a temporary name (the original is untouched).
  - Stop and rename the original aside, rename the import into place, start it.
  - Any failure rolls back to the original. The old copy is deleted only after
    the restored instance starts.
  - Also handles a *running* original (the old code failed on it).
  - A restore that does not boot rolls back to the original. With no original
    to fall back to, the restored instance is kept and the failure is logged.
  - Known gap: if the console process dies between "move aside" and "put in
    place", the original sits under a `console-old-*` name and must be renamed
    back by hand.

## Backlog (not started)

Ordered roughly by value. Each needs its own change and tests.

### Security
- [x] B1. `clientIP` trusted `X-Forwarded-For` blindly. Now the header is used
  only when the connecting peer is in `CONSOLE_TRUSTED_PROXIES` (default
  loopback), and then read from the right, skipping trusted hops. The peer is
  parsed with `net.SplitHostPort`.
- [x] B2. Listen address now defaults to `127.0.0.1:8080`. Secure cookies default
  to on when `CONSOLE_BASE_URL` is https. Startup warns about https with
  insecure cookies, and about a non-loopback listener without https.
- [x] B3. CSP (same-origin scripts), `X-Frame-Options`, `nosniff`,
  `Referrer-Policy`, `Cache-Control: no-store` on pages, HSTS when secure. The
  two inline handlers moved to `app.js`, htmx eval is off, and a test keeps
  templates free of inline scripts and handlers.
- [x] B4. TOTP setup replaced the secret and switched 2FA off with only a
  session. Setup is now refused while 2FA is on, and setup and disable both
  need the password (throttled: 5 wrong per 15 minutes).
- [x] B5. `FinishJob` and `FailStaleJobs` blank the payload, a full queue closes
  its job, and startup scrubs payloads left by older builds.
- [x] B6. TOTP secrets are sealed with AES-256-GCM (`internal/secrets`). The
  key is `CONSOLE_SECRET_KEY` or a generated `<data>/secret.key` (0600). It is
  separate from the SSO key, so rotating one cannot lock out the other.
  Plaintext values from older builds are sealed on startup.
- [x] B7. The generated password goes to `<data>/initial-admin-password`
  (0600) and to the terminal if interactive, never to the log.
- [x] B8. Non-admins no longer get the user list, SSO providers, S3 details,
  Caddy paths or the generated Caddyfile (Settings, Domains, Networking, and the
  fragments returned after a refused request). The data is not loaded, and the
  templates hide it too.
- [x] B9. Each SSO provider has an account policy: `link_only` (default for
  new providers), `domains` (verified email at listed domains) or `open`.
  Existing providers keep `open` (migration 004) so upgrades change nothing;
  unknown policies refuse.
- [x] B10. `redirectBack` runs the Referer path through `safeNext`, which now
  also rejects backslashes and control characters.
- [x] B11. New `VerifyCredentials` checks the password without opening a
  session. The sign-in handler applies the SSO-only and 2FA rules first, and a
  session is created only when they pass.
- [x] B12. `/healthz` logs the Incus error and returns a fixed message.
  `/static/` directories answer 404.

### Reliability and correctness
- [x] B13. `Sync` is serialised. With the admin API the file is written only
  after Caddy accepts the config; with a reload command the previous file is
  restored on failure. Duplicate sites and the console host are dropped from
  the render, and `domains.Add` refuses the console host.
- [x] B14. State-changing actions (lifecycle, delete, rename, limits,
  snapshots, backup delete, domains) now run on a context detached from the
  request (`operationContext`, 10 min cap) so closing the tab cannot abort them
  halfway. Not moved to the job queue: the UI still waits for the result.
- [x] B15. `handleJobsStream` lifts the write deadline (`statusRecorder` now
  has `Unwrap`, so it works through the logging wrapper). Tested against a
  server with a short `WriteTimeout`.
- [x] B16. Instance rename moves local archives to the new name (never
  overwriting). Rename and delete are refused while a job targets the instance.
- [x] B17. Instance delete also removes local archives and the offsite (S3)
  copies, since the records that pointed at them go too.
- [x] B18. Blank limit fields keep the current value; only an explicit 0
  removes a CPU or memory limit. A form with nothing to change is refused.
- [x] B19. A panicking job is recorded as failed instead of crashing the
  console. Jobs for one instance run one at a time. `FailStaleJobs` now fails
  only jobs that were running, so queued jobs are requeued after a restart.
  `Enqueue` after `Stop` returns an error instead of panicking.
- [x] B20. A root disk that comes from a profile is copied (with pool and
  path) before resizing; resubmitting the displayed GiB of a fractional disk is
  a no-op instead of a "shrink" error.

### Product (reseller readiness)
- [~] B21. `CONSOLE_MAX_INSTANCES` is enforced (counts instances and queued
  creates, serialised) and a single request larger than the host is refused.
  **Not done:** owner-scoped roles, per-user quotas and tenant isolation. That
  is an authorisation redesign (every instance, backup, domain and job needs an
  owner), so every console user still sees every instance.
- [~] B22. Scheduled snapshots with retention (`CONSOLE_SNAPSHOT_INTERVAL_HOURS`,
  `CONSOLE_SNAPSHOT_KEEP`; off by default; only `auto-*` snapshots are pruned;
  decided from snapshot age, so restarts neither skip nor double a run).
  **Not done:** scheduled exported backups to S3 and per-instance schedules;
  the schedule is global.
- [x] B23. 2FA recovery codes (8 single-use codes, hashed, shown once,
  password-confirmed regeneration, same throttling as TOTP), admin password
  reset for other users (signs them out everywhere), and a "Signed-in devices"
  card to list and revoke your own sessions.
- [x] B24. `GET /metrics` (Prometheus text, bearer token, off unless
  `CONSOLE_METRICS_TOKEN` is set), a failure webhook
  (`CONSOLE_ALERT_WEBHOOK_URL`, never carries the job payload) and an
  admin-only audit CSV export with spreadsheet-formula neutralising.

### Performance
- [x] B25. `Incus.Instances` is cached for 2 s, shared by concurrent callers
  and dropped after any Incus operation (`wait`). Domain upstreams and Caddy
  routes now use that one listing; a failed listing aborts a Caddy sync
  instead of rendering an empty file.
- [x] B26. Backups: one Incus call per instance (not per backup), S3
  existence checks run in parallel (8) and are cached 30 s. Instances: image
  descriptions are cached per fingerprint (5 min).
- [x] B27. One renderer feeds every open job stream (`feed`); it polls only
  while someone is subscribed and sends only when the content changed.
- [x] B28. A sampling round is one transaction; chart history is averaged down
  to at most 480 points; default retention is now 720 h to match the 30-day
  chart (existing explicit settings are untouched).

### Tests
- [x] B29. New tests for `caddy`, `jobs`, `domains`, `database` (secrets, jobs,
  metrics), `backups` (archive moves), `incus` (list cache), `monitoring`,
  `instances` (limits, busy guard), `secrets`. Not covered: the Incus-backed
  paths (create/rebuild/restore end to end) and the S3 target, which need a
  live daemon or bucket.
- [x] B30. `internal/web/.preview/` is untracked (`git rm --cached`, files kept
  on disk) and ignored. Deletions are staged, not committed.
