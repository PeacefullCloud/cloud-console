# Peaceful Cloud Console

A lightweight, single-binary web console for managing **Incus containers and virtual machines** on a single physical Linux server.

Built with **Go**, **HTMX**, the **Incus Go SDK**, **SQLite**, and **Caddy**—with zero external queues, caches, or separate frontend build pipelines.

## Highlights

* **Instances & Blueprints** — Create, start, stop, reboot, resize, rename, and rebuild LXC containers and QEMU VMs from OS or application blueprints.
* **Web Terminal & SFTP File Manager** — Browser-based shell and file manager connected over Incus `exec` without exposing SSH ports.
* **Domains & Dedicated Public IPs** — Automatic HTTPS reverse proxying via Caddy, per-instance port forwarding, and 1:1 dedicated public IP assignment.
* **Snapshots & Off-site Backups** — Instant local copy-on-write snapshots (manual or scheduled) and portable backups with optional S3-compatible storage.
* **Monitoring & Audit Trail** — Resource charts, `/healthz`, Prometheus `/metrics`, failure webhook alerts, and an exportable activity log.
* **Multi-User Security** — Role-based access (`admin`, `operator`, `viewer`), tenant isolation, per-user quotas, SSH key vault, 2FA (TOTP), and OIDC SSO.

## Architecture

```text
Browser  ──HTTPS──►  Caddy  ──►  cloud-console (Go + SQLite)  ──Unix socket──►  Incus
```

* **Incus** remains the source of truth for instance state, storage pools, networking, and snapshots.
* **SQLite** stores console metadata (users, sessions, domains, dedicated IPs, jobs, metrics, and audit logs).
* **Single static binary** embeds all templates, CSS, JavaScript, and terminal WASM assets.

## Quick Start

Requires **Linux with Incus** (`incusd`) and **Go 1.26+** to build:

```bash
git clone https://github.com/PeacefullCloud/cloud-console
cd cloud-console
CGO_ENABLED=0 go build -trimpath -o cloud-console .
cp .env.sample .env
./cloud-console
```

On first run, if `CONSOLE_ADMIN_PASSWORD` is unset, an initial `admin` password is written to `data/initial-admin-password` (`0600`).

## Documentation

Full guides and screenshots are published at [peacefullcloud.github.io/cloud-console](https://peacefullcloud.github.io/cloud-console/index.html):

* [Overview & Features](https://peacefullcloud.github.io/cloud-console/index.html)
* [Installation, Environment Variables & Deployment Guide](https://peacefullcloud.github.io/cloud-console/installation.html)
* [Architecture & Management Guide](https://peacefullcloud.github.io/cloud-console/management.html)
