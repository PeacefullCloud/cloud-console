# Peaceful Cloud Console

A small, Lightsail-style web UI for managing **Incus instances on a single
physical server**. Go backend, the official Incus Go SDK, SQLite, HTML +
HTMX and one hand-written CSS file.

```text
HTML + HTMX  →  Go backend  →  Incus Go SDK  →  Incus  →  ZFS
```

The layout follows the AWS Lightsail console (top bar, left sidebar, instance
cards, per-instance tabs, size/blueprint pickers) with a blue palette on a
blue-white background instead of orange on white.

The name follows a family convention: **Peaceful** is the GitHub organisation,
**Peaceful Cloud** is the product family, and this repository is its compute
piece — the Console. Future services such as S3-compatible object storage drop
into the same pattern (`peaceful/cloud-*`).

---

## Contents

- [What it does](#what-it-does)
- [Architecture](#architecture)
- [Requirements](#requirements)
- [Install](#install)
- [Configuration](#configuration)
- [Running it](#running-it)
- [Host setup](#host-setup)
- [How the pieces fit together](#how-the-pieces-fit-together)
- [Security](#security)
- [Development](#development)
- [V1 scope](#v1-scope)

---

## What it does

**Instances** — create from a blueprint, start, stop, reboot, rename, rebuild
from a fresh image, resize CPU/memory/disk, delete, view the private IP and
status.

**Domains** — attach hostnames to instances. The console rewrites the host
Caddyfile and reloads Caddy, which issues and renews HTTPS certificates
automatically. Create the A record; everything else is automatic.

**Snapshots** — create, list, restore and delete Incus snapshots. A snapshot is
a fast, local rollback point.

**Backups** — create, restore, delete and export. A backup is a portable
archive written to the console's disk and optionally uploaded to any
S3-compatible bucket, so it survives the server.

**Monitoring** — sampled CPU, memory, disk and network history with charts for
1 hour, 6 hours, 1 day, 1 week and 1 month windows.

**Access & operations** — SSH/console command snippets, storage pools, networks,
an audit log of every action, and background jobs with live progress for slow
operations.

The UI stays server-rendered. HTMX fetches tab panels, the blueprint picker and
job progress; everything else is plain HTML forms, which means the console works
with JavaScript disabled.

---

## Architecture

```text
                ┌──────────────────┐
                │  HTML + HTMX UI  │
                └────────┬─────────┘
                         │
                         ▼
                ┌──────────────────┐
                │    Go Backend    │
                │                  │
                │ Incus SDK        │
                │ SQLite           │
                │ Caddy Manager    │
                └────────┬─────────┘
                         │
                         ▼
                     ┌───────┐
                     │ Incus │
                     └───┬───┘
                         │
                       ZFS
                         │
          ┌──────────────┼──────────────┐
          ▼              ▼              ▼
       Ubuntu         Debian         WordPress
       Instance       Instance       Instance
```

Five rules drive the design:

1. **Incus does infrastructure.** Incus is the source of truth for running
   state, IPs, resource usage, snapshots and storage.
2. **Go does orchestration and UI logic.**
3. **SQLite stores only console metadata** — users, sessions, domains, the
   friendly image name, owners, backup records, activity and sampled metrics.
   CPU, RAM, IPs and running state are never duplicated.
4. **Caddy handles public web traffic.** Containers only ever hold private
   addresses on the Incus bridge.
5. **Keep everything else out until it is actually needed.** No Redis, no
   message broker, no separate frontend build.

---

## Requirements

- Go 1.26 or newer (to build; driven by the pure-Go SQLite driver). With
  `GOTOOLCHAIN=auto`, an older Go will fetch the right toolchain automatically.
- Incus (the daemon `incusd`) on the same host
- A storage pool, ideally ZFS
- Caddy on the host (to serve domains publicly)
- Optional: any S3-compatible endpoint for off-host backups

The console talks to Incus over its **local Unix socket** only. The Incus API is
never exposed to the internet.

---

## Install

```bash
git clone <your-repo> cloud-console
cd cloud-console

# Development build
go build -o cloud-console .

# Static, dependency-free release build
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=1.0.0" -o cloud-console .
```

`CGO_ENABLED=0` works because both the Incus SDK client and the SQLite driver
(`modernc.org/sqlite`) are pure Go. The result is one static binary with the
templates, CSS and JavaScript embedded — no runtime assets to copy.

---

## Configuration

Everything is read from the environment, so a single binary fits any host.

Copy the sample file and edit it — the console loads `.env` from its working
directory automatically:

```bash
cp .env.sample .env
$EDITOR .env
```

Real environment variables always take precedence over the file, so
`CONSOLE_ADDR=127.0.0.1:9000 ./cloud-console` and systemd `Environment=…` lines still
win. Set `CONSOLE_ENV_FILE` to point at a file somewhere else, for example
`/etc/peaceful-cloud/console.env`. The file is plain `KEY=value` with `#` comments, so
it also works as a systemd `EnvironmentFile=`.

### Console

| Variable | Default | Purpose |
| --- | --- | --- |
| `CONSOLE_ADDR` | `:8080` | HTTP listen address |
| `CONSOLE_BASE_URL` | `http://localhost:8080` | Public URL, used for the console's own Caddy block |
| `CONSOLE_DATA_DIR` | `./data` | SQLite database and backup archives |
| `CONSOLE_DB_PATH` | `<data>/console.db` | SQLite file path |
| `CONSOLE_ENV_FILE` | `./.env` | Alternative env file to load |
| `CONSOLE_SECURE_COOKIES` | `false` | Set `true` when serving over HTTPS |
| `CONSOLE_SESSION_TTL_HOURS` | `168` | Session lifetime |
| `CONSOLE_MAX_INSTANCES` | `100` | Informational limit for the UI |
| `CONSOLE_ADMIN_USER` | `admin` | Bootstrap administrator username |
| `CONSOLE_ADMIN_PASSWORD` | *(random)* | Bootstrap password; a random one is printed once when unset |
| `CONSOLE_DEV` | `false` | Load templates/static from disk for live editing |

### Incus

| Variable | Default | Purpose |
| --- | --- | --- |
| `INCUS_SOCKET` | `/var/lib/incus/unix.socket` | Path to the Incus Unix socket |
| `INCUS_PROJECT` | *(default project)* | Incus project to operate in |

### Caddy

| Variable | Default | Purpose |
| --- | --- | --- |
| `CADDY_ADMIN_URL` | *(unset)* | Preferred: e.g. `http://localhost:2019`. The console POSTs the Caddyfile to `/load`. |
| `CADDY_CONFIG_PATH` | *(unset)* | Write the generated Caddyfile here |
| `CADDY_RELOAD_CMD` | `caddy reload --config <path>` | Fallback reload command |

If neither `CADDY_ADMIN_URL` nor `CADDY_CONFIG_PATH` is set, domain changes are
still recorded but nothing is reloaded, and the console says so in the UI.

### Backups (S3-compatible)

| Variable | Default | Purpose |
| --- | --- | --- |
| `S3_ENDPOINT` | *(unset)* | e.g. `s3.eu-west-1.amazonaws.com` or `minio.local:9000` |
| `S3_BUCKET` | *(unset)* | Target bucket (created if missing) |
| `S3_ACCESS_KEY` | *(unset)* | Access key ID |
| `S3_SECRET_KEY` | *(unset)* | Secret access key |
| `S3_REGION` | `us-east-1` | Region |
| `S3_PREFIX` | `peaceful-cloud` | Key prefix inside the bucket |
| `S3_USE_SSL` | `true` | Use HTTPS for the endpoint |

### Monitoring

| Variable | Default | Purpose |
| --- | --- | --- |
| `CONSOLE_METRICS_INTERVAL` | `60` | Seconds between samples (minimum 10) |
| `CONSOLE_METRICS_RETENTION_HOURS` | `168` | How long samples are kept |

---

## Running it

Start from the sample environment file:

```bash
cp .env.sample .env
$EDITOR .env        # set CONSOLE_BASE_URL, CADDY_ADMIN_URL, S3_* ...

./cloud-console
```

Or pass everything inline instead:

```bash
INCUS_SOCKET=/var/lib/incus/unix.socket \
CONSOLE_ADDR=127.0.0.1:8080 \
CONSOLE_BASE_URL=https://console.example.com \
CONSOLE_SECURE_COOKIES=true \
CADDY_ADMIN_URL=http://localhost:2019 \
./cloud-console
```

On first start the console creates the SQLite schema, then prints the bootstrap
administrator password if you did not set one:

```text
level=INFO msg="applied migration" name=001_init.sql
level=INFO msg="loaded environment file" path=.env
level=INFO msg="connected to incus" server=incus-01 version=6.23 driver="lxc | qemu"
level=WARN msg="created the initial administrator account" username=admin password=Kf7mQp2Xr9Lt
```

Sign in, change that password in **Settings → Your password**, and create the
users you need.

### Health check

`GET /healthz` returns `200 ok` when the console can reach Incus, and `503` with
the error otherwise.

### As a systemd service

```ini
# /etc/systemd/system/cloud-console.service
[Unit]
Description=Peaceful Cloud Console
After=network-online.target incus.service
Wants=incus.service

[Service]
Type=simple
DynamicUser=yes
SupplementaryGroups=incus-admin
WorkingDirectory=/var/lib/peaceful-cloud
EnvironmentFile=/etc/peaceful-cloud/console.env
ExecStart=/usr/local/bin/cloud-console
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

`/etc/peaceful-cloud/console.env` is a copy of `.env.sample` with the values you
want; the console also loads a `.env` file from its working directory when no
`EnvironmentFile=` is set.

The service user must be in a group that can read the Incus socket
(`incus-admin` on most distributions).

---

## Host setup

One-time host preparation.

### 1. A storage pool

```bash
incus storage create default zfs size=1TiB
```

### 2. A network bridge for instances

```bash
incus network create incusbr0 ipv4.address=10.10.0.1/24 ipv4.nat=true
```

Instances get private addresses such as `10.10.0.21`. Only the host is public.

### 3. Caddy with an admin endpoint

Enable the admin API in `/etc/caddy/Caddyfile` so the console can apply changes
without a restart:

```caddyfile
{
	admin localhost:2019
}
```

Then let the console own a Caddyfile that your own config imports. Either import
the whole directory:

```caddyfile
import /etc/caddy/sites/*.caddy
```

or import just the generated file, which is what the examples below assume:

```caddyfile
import /etc/caddy/cloud-console.caddy
```

Set `CADDY_CONFIG_PATH=/etc/caddy/sites/cloud-console.caddy` so the console
never rewrites your own Caddyfile. Then either let the console drive Caddy
through the admin API (`CADDY_ADMIN_URL=http://localhost:2019`, recommended) or
let it run `caddy reload`.

### 4. Optional: a WordPress blueprint

The wizard can offer an application image containing Caddy, PHP-FPM, WordPress
and MariaDB. Build it once on the host and give it the alias the catalog
expects:

```bash
incus launch images:ubuntu/24.04 wordpress-builder
incus exec wordpress-builder -- bash -c '<install caddy, php-fpm, wordpress, mariadb>'
incus stop wordpress-builder
incus publish wordpress-builder --alias wordpress-php8.3
incus delete wordpress-builder
```

The catalog in `internal/incus/catalog.go` then offers it as **WordPress**.

### 5. Public DNS

For every domain you add in the console, create an `A` record pointing at the
host's public IP. Caddy requests a certificate as soon as DNS resolves and ports
80/443 are reachable.

---

## How the pieces fit together

### Creating an instance

```text
Browser → POST /instances
            ↓ validate
          job "instance.create" queued
            ↓
          Incus: create instance from image
            ↓ apply limits.cpu / limits.memory / root device size
          Incus: start
            ↓ wait for a private address
          SQLite: record image label, owner, limits, domain
            ↓
          Caddy: re-render Caddyfile + reload
            ↓
          UI: job progress → instance shows as Running
```

Slow work runs in a small in-process worker pool (`internal/jobs`) with the job
row in SQLite, and the UI follows progress by polling. No external queue.

### Snapshots versus backups

| | Snapshot | Backup |
| --- | --- | --- |
| Cost | Instant, copy-on-write | Minutes, full archive |
| Lives on | The same storage pool | Console disk **and** S3 |
| Survives disk loss | No | Yes |
| Use for | Rolling back an update | Disaster recovery, migration |

### Domains

```text
example.com → host Caddy → 10.10.0.21:80 → container Caddy → WordPress
```

The console regenerates the Caddyfile from the `domains` table whenever a domain
or an instance changes, and periodically, so an instance that was stopped when a
domain was added picks it up when it comes back.

---

## Security

```text
Browser ──HTTPS──► Caddy ──► Go Console ──Unix socket──► Incus
```

- The Incus API is never exposed. The console is the only thing that can reach it.
- Sessions are random 32-byte tokens; only their SHA-256 hash is stored, so a
  database leak cannot be replayed as a live cookie.
- Passwords are bcrypt hashed.
- CSRF uses a double-submit token in an `HttpOnly` cookie, verified on every
  mutating request.
- Session and flash cookies are `HttpOnly` and `SameSite=Lax`; set
  `CONSOLE_SECURE_COOKIES=true` behind HTTPS.
- Roles: `admin` (everything), `operator` (manage instances), `viewer`
  (read only).
- Every mutating action is written to the audit log with the user and result.
- Set `CONSOLE_SECURE_COOKIES=true` and put the console behind Caddy or another TLS
  terminator before exposing it.

---

## Development

```bash
# Live template and static file reloading
CONSOLE_DEV=1 go run .

# Unit tests, including a render test that executes every page and tab
go test ./...

# Vet
go vet ./...
```

### Previewing the UI without Incus

The render test can write every screen to static HTML, so you can review the
design without a running daemon:

```bash
CONSOLE_PREVIEW_DIR=./.preview go test ./internal/web -run TestWriteDesignPreview
# then open .preview/instances.html (and the other files) in a browser
cp static/htmx.min.js static/app.js .preview/
```

---

## V1 scope

Implemented:

- Instances: create, start, stop, reboot, rename, rebuild, delete, resource
  limits, image label, owner, notes
- Domains: add, remove, primary host, automatic HTTPS through Caddy
- Snapshots: create, list, restore, delete
- Backups: create, list, restore, delete, export to S3-compatible storage
- Monitoring: CPU, memory, disk and network history with charts
- Access: console and diagnostic command snippets
- Settings: host capacity, storage pools, console users, password change,
  S3 status, generated Caddyfile preview
- Activity: audit log and background job history

Deliberately not in V1 (add when actually needed):

- A web console/VNC stream into instances (use `incus exec` / `incus console`)
- Scheduled snapshots and backups (use `incus snapshot schedule` for now)
- Clustering and multi-host support
- A REST API for third parties
- Redis, RabbitMQ or any external queue
