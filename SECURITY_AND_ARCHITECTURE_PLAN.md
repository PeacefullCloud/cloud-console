# Security, Reliability & Lightsail/Proxmox Architecture Plan

Source: Comprehensive security, bug, and architectural audit (2026-11-24).
Design principles: **Fast, simple, async/non-blocking, efficient, and lightweight** (single Go binary, SQLite WAL, bounded worker pools, parallel I/O where safe, zero heavy external dependencies).

Legend: `[x]` fixed & verified, `[ ]` pending.

---

## Phase 1: Critical & High Security Hardening

### SEC-1 [CRITICAL] Restic `sftp:` SSH Option Injection (Host RCE)
- [x] **Problem:** `parseSFTPRepo` in `internal/backups/restic.go` does not reject leading hyphens (`-`) in `user`, `host`, or `userHost`, and `prepareSFTPOpts` passes `userHost` directly to `ssh` without `--`. A tenant (`operator`) can supply `sftp:-oProxyCommand=...@host/path` to execute arbitrary commands on the host.
- [x] **Fix:**
  - Reject leading `-` in `userHost`, `user`, and `hostPart` in `parseSFTPRepo`.
  - Validate `user` with a strict character whitelist (`[a-zA-Z0-9._-]`, non-leading `-`) and validate `hostPart` as a valid hostname or IP literal.
  - Validate `SSHKnownHosts` lines so control characters or invalid entries cannot corrupt the temporary `known_hosts` file.
  - Pass `"--"` before `userHost` when constructing the `ssh` command in `prepareSFTPOpts`.

### SEC-2 [CRITICAL] Restic `"local"` Arbitrary Host Filesystem Write
- [x] **Problem:** Any `operator` can create a `"local"` Restic destination pointing to arbitrary host directories (`/etc/caddy`, `/var/lib/peaceful-cloud/data`, etc.).
- [x] **Fix:**
  - Restrict arbitrary `"local"` filesystem paths to `admin` users, and/or confine `"local"` Restic repositories under `<BackupDir>/restic/<sanitized-subpath>` (rejecting `..` and absolute host paths outside the managed backup directory for non-admins).

### SEC-8 [MEDIUM] SSRF via Tenant-Controlled Restic S3 Endpoints
- [x] **Problem:** Tenants can specify loopback (`127.0.0.1:2019` Caddy Admin API), link-local (`169.254.169.254` metadata), or private network URLs in `s3:` Restic destinations.
- [x] **Fix:**
  - Parse the S3 endpoint URL and reject loopback, link-local, unspecified, and private IP literals/hosts in `validateS3Repo` (unless explicitly allowed for admin-configured internal endpoints).

### SEC-5 [HIGH] Web Terminal Lifecycle & Session Revocation Bypass
- [x] **Problem:** `serveConsole` in `internal/web/handlers_console.go` never re-checks session validity or instance ownership after initial WebSocket upgrade, and logout/password-reset/user-delete do not terminate active Web Terminal sessions.
- [x] **Fix:**
  - Add a non-blocking periodic re-validation ticker (every 30s) inside `serveConsole` that verifies `s.App.Auth.Authenticate(token)`, `auth.CanWrite(user.Role)`, and `s.canAccessInstance(user, name)`.
  - Track active console WebSocket cancel funcs per user/session and revoke both SFTP and Web Terminal sessions on logout, session revoke, password change/reset, role downgrade, and user deletion.

### SEC-6 [HIGH] Cross-Instance SFTP Session Collision on Multi-Tab Use
- [x] **Problem:** `s.sftpSessions` in `internal/web/handlers_sftp.go` is keyed only by `tokenHash`, causing a second tab opened for `instance-b` to reuse `instance-a`'s SFTP client and close it when Tab 2 exits.
- [x] **Fix:**
  - Key `s.sftpSessions` by composite key `sftpSessionKey{tokenHash, instanceName}`.
  - Update `openSFTP`, `closeSFTP`, `revokeSFTPHandle`, `revokeSFTPUser`, and `revokeSFTPInstance` to operate cleanly on composite keys.

### SEC-7 [MEDIUM] SFTP `copyRecursive` Symlink Escape
- [x] **Problem:** `copyRecursive` in `internal/web/handlers_sftp.go` walks child directories without checking `os.ModeSymlink` or re-resolving child paths against `sess.home`.
- [x] **Fix:**
  - Check `child.Mode()&os.ModeSymlink != 0` in `copyRecursive` (and verify each source/destination path via `sess.resolve`) to prevent dereferencing symlinks pointing outside `/home/cloud-user`.

### SEC-3 [HIGH] Multi-Tenant Resource Quota Bypass (`UpdateLimits` & Unlimited CPU/RAM)
- [x] **Problem:** Any `operator` can call `POST /instances/{name}/limits` with `cpu=0&memory_mb=0&disk_gb=10240` to remove CPU/RAM limits completely (`unlimited`) and grow disk to 10 TiB, without host capacity or per-user resource checks.
- [x] **Fix:**
  - Disallow `cpu=0` and `memory_mb=0` for non-admins in `UpdateLimits`.
  - Enforce `checkCapacity` in `UpdateLimits` just like `Create`.
  - Add optional per-user resource quotas (`max_cpu`, `max_memory_mb`, `max_disk_gb`) to `users` and enforce them across `Create` and `UpdateLimits`.

### SEC-4 [HIGH] Incus Bridge (`incusbr0`) L2/L3 Spoofing & Lateral Movement
- [x] **Problem:** Instances on `incusbr0` lack `security.mac_filtering`, `security.ipv4_filtering`, and `security.ipv6_filtering`, allowing ARP/IP spoofing (hijacking Caddy upstream traffic) and unrestricted east-west traffic between tenants.
- [x] **Fix:**
  - Automatically configure explicit `eth0` NIC security filters (`security.mac_filtering=true`, `security.ipv4_filtering=true`, `security.ipv6_filtering=true`) on container creation (Incus supports these on bridged container NICs; handle gracefully if the backing network/kernel doesn't support netfilter/ebtables).
  - Provide per-instance Firewall rules (Incus Network ACLs) and TCP/UDP Port Forwards (`incus network forward`).

### SEC-9, SEC-10, SEC-11 [MEDIUM/LOW] SSH Key Normalization, Domain/Name Reservation & Key Derivation
- [x] **SEC-9:** Normalize raw `req.SSHKey` on instance creation using `sshkeys.Parse` so multi-line payloads or injected `authorized_keys` options are rejected.
- [x] **SEC-10:** Reserve exact instance name `"console"` in `ValidateName` and route instance-creation domain registration through `Domains.Add` so `ConsoleHost()` collision checks are always enforced.
- [x] **SEC-11:** Derive `CONSOLE_SSO_KEY` passphrases using Argon2id with a deterministic domain-separation salt instead of single-round unsalted SHA-256.

---

## Phase 2: Bug Fixes & Reliability Improvements

### BUG-1 [HIGH] Caddy `detectLANIP()` Binds Public Domains to `incusbr0` (`10.10.0.1`)
- [x] **Problem:** `detectLANIP()` picks the first RFC1918 interface on the host, which on a public cloud server is `incusbr0` (`10.10.0.1`), causing Caddy to emit `bind 10.10.0.1` and drop external public traffic.
- [x] **Fix:**
  - Ignore virtual/container bridge interfaces (`incusbr*`, `lxdbr*`, `docker*`, `veth*`, `virbr*`, `br-*`, `cni*`, `flannel*`, `tailscale*`, `wg*`) in `detectLANIP()` and `DetectPublicIP()`.
  - Do not emit `bind <private-ip>` in `caddy/manager.go` unless a physical private LAN IP exists and no public IP is present on the host (or only bind when explicitly configured).

### BUG-2 [HIGH] Instance Rebuild Wipes `cloud-user`, SSH Server & Keys, and Stale Image Metadata
- [x] **Problem:** `handleRebuild` in `internal/instances/create.go` replaces the root disk without re-running `WaitForAgent`, `EnsureConsoleUser`, or `EnsureSSH`, and leaves the old `image`/`image_label` in SQLite.
- [x] **Fix:**
  - Update `instances.image` and `instances.image_label` in SQLite during rebuild.
  - Run `EnsureConsoleUser` and `EnsureSSH` concurrently in parallel goroutines after `WaitForAgent` during rebuild (re-installing the owner's saved SSH keys if present).

### BUG-3, BUG-4, BUG-5 [MEDIUM] Backup & Restic Lifecycle Across Rename, Delete, and S3 Restore
- [x] **BUG-3:** Store `owner_id` on `restic_snapshots` (and preserve `backups` metadata or scope Restic snapshots by `owner_id`) so non-admin tenants can view and restore Restic snapshots/backups even after the original instance is deleted.
- [x] **BUG-4:** Update `restic_snapshots.instance_name` and `metrics.instance_name` inside `RenameInstanceMeta` transaction when an instance is renamed.
- [x] **BUG-5:** Show the Restore button in `templates/backups.html` and `templates/partials/tab_backups.html` when `{{if or .LocalExists .InS3}}`.

### BUG-6, BUG-7, BUG-8 [MEDIUM/LOW] 2FA Non-HTMX Recovery Codes, SSE `jobFeeds` Cleanup, and IPv6 `WaitForIP`
- [x] **BUG-6:** Generate and display 2FA recovery codes in `handleTOTPEnable` even when submitted without HTMX (standard form POST).
- [x] **BUG-7:** Evict idle per-user `jobFeeds` when the last SSE subscriber disconnects.
- [x] **BUG-8:** Support global-unicast IPv6 addresses as fallback in `WaitForIP` and `PrimaryIP` when an instance lives on an IPv6-only network.

---

## Phase 3: Lightsail & Lightweight Proxmox Feature Upgrades (Async & Lightweight)

### FEAT-1 Cloud-Init / Launch Script Support
- [x] Allow optional **Launch Script** (`user_data` shell script / cloud-init config) in the Create Instance wizard.
- [x] Pass `cloud-init.user-data` in Incus instance config on creation and execute custom post-boot scripts asynchronously without blocking instance boot.

### FEAT-2 Attachable Block Storage Volumes (Storage Tab)
- [x] Add Incus custom storage volume management (`CreateCustomVolume`, `AttachVolume`, `DetachVolume`, `DeleteCustomVolume`) in `internal/incus/client.go`.
- [x] Upgrade the instance **Storage** tab (`templates/partials/tab_storage.html`) from read-only to interactive so users can create, attach, detach, and delete extra block storage volumes mounted inside their instance.

### FEAT-3 Per-Instance Firewall & TCP/UDP Port Forwarding (Networking Tab)
- [x] Add TCP/UDP port forwarding rules (using Incus `proxy` devices in `nat=true` or `listen=tcp:0.0.0.0:<host_port> connect=tcp:127.0.0.1:<guest_port>` mode, which works reliably across every Incus bridge/host setup without requiring dedicated forward IPs!) in the instance **Networking** tab.
- [x] Allow users to expose SSH (e.g., host port -> guest port 22) or custom TCP/UDP ports with strict port range validation (`10000-60000`) and conflict detection.

### FEAT-4 Automated Scheduled Restic Backups
- [x] Extend the background scheduler (`internal/snapshots/schedule.go` / `internal/backups`) to optionally trigger automated Restic snapshots for instances with configured Restic destinations, running asynchronously via the job queue.
