# log server for TOS

A unified log platform for **TerraMaster TOS 7.x**: receive syslog from network devices, collect local
files and journald, store and search everything locally, raise alerts, forward to remote log servers, and
manage the whole thing from a TOS-native web UI.

The application ships as a `.deb` package (`application_type: "deb"`) and is designed for TOS 7.x on
`x86_64` and `aarch64`. App ID: `log-server-for-tos`; display name: **log server for TOS**.

## Features

- **Syslog receiver** — UDP/TCP 514 and TLS 6514 (certificate required), per-source ACL and rate limiting.
- **File & journald collection** — arbitrary absolute paths, inode+offset checkpoints, rotation-safe,
  multiline merging, encoding detection, hot add/remove from the UI. journald is read-only and only used
  when the platform grants permission.
- **Local storage** — VictoriaLogs bundled in the package (source-reproducible build) with a storage
  adapter layer; storage location configurable on any data volume.
- **Search** — full-text/field filters, timeline histogram drill-down, context view, time-cursor paging,
  JSON/CSV export.
- **Forwarding** — multiple targets (UDP/TCP/TLS/HTTP), disk queues with at-least-once delivery and
  visible backlog.
- **Alerting** — built-in rules (OOM, disk/RAID, kernel, service failures, SSH brute force, account
  changes, containers…), suppression, acknowledgements, silences and maintenance windows; SMTP/Webhook
  notifications.
- **Reliability** — every received/collected record is written to a disk-backed ingest queue before it
  is written to the storage engine; engine outages and application restarts do not lose accepted logs.
- **Auditability** — append-only management-action log (`audit/actions.log`) recording configuration
  changes, exports/imports, storage changes, alert operations and denied admin attempts.
- **Self-observability** — rotating application log with pre-write redaction, one-click diagnostics bundle,
  configuration export/import, `/metrics`.

## Install

From the TOS App Center, install the `log-server-for-tos` package. For manual installation:

```bash
sudo dpkg -i log-server-for-tos_x86_64.deb     # or log-server-for-tos_aarch64.deb
```

- Web UI: `http://<NAS>:8181/log-server-for-tos/` (reverse-proxied by the platform nginx)
- Syslog: UDP/TCP `514`, TLS `6514` (needs a certificate)
- Runtime data: `/Volume*/@apps/log-server-for-tos/data/` — never on the system partition
- Service: dedicated non-root user `log-server-for-tos` with systemd sandboxing; no `Restart=` directive

### First run

TOS login sessions are treated as **operator** by default. To change configuration, storage location or
export configuration, unlock admin once:

1. On the NAS, read `<DataDir>/config/admin.token` (mode `0600`, generated on first start).
2. Open **Settings → Administrator** in the UI and paste the token.
3. The first administrator can alternatively use the one-time **Become administrator** button.

The first-run wizard walks through retention/storage, collection, notifications and forwarding.
It can be skipped and re-run from Settings.

## Build

Prerequisites: Go 1.22+, `dpkg-deb`, and network access for the pinned VictoriaLogs source archive
(or Docker; the build script supports both). The frontend is plain ES modules and needs no Node build.

```bash
# Both architectures (uses the source-reproducible VictoriaLogs recipe)
BUILD_MODE=local WITH_ENGINE=1 bash packaging/build.sh all

# Produces
#   out/log-server-for-tos_x86_64.deb
#   out/log-server-for-tos_aarch64.deb
```

Reproducibility materials:

- `packaging/repro-build.sh` — pinned upstream tag/commit/tarball + toolchain hashes, twice-built hash check
- `packaging/Dockerfile.repro` — Ubuntu 22.04 build base
- `packaging/VERIFICATION.md` — recorded hashes and verification method
- `packaging/upstream-vl-1.52.1.manifest.sha256` — per-file upstream source manifest

## Test

```bash
cd backend
CGO_ENABLED=0 go test ./...
```

Frontend syntax/import gates (Node only for syntax checking):

```bash
python3 frontend/check-imports.py
for f in $(find frontend/assets -name '*.js'); do node --check "$f"; done
```

## Architecture

```
receivers (UDP/TCP/TLS) ─┐
file / journald ─────────┼─▶ pipeline ─▶ disk ingest queue ─▶ VictoriaLogs
remote devices ──────────┘        │                              │
                                  ├─▶ forward queues ─▶ remote log servers
                                  ├─▶ alert engine ─▶ SMTP / Webhook
                                  └─▶ aggregate counters ─▶ overview UI
```

The data model keeps the raw message, an envelope of collection metadata and derived fields separately.
Timestamps use a 24-hour sanity gate, levels keep both the original text and a normalized syslog severity.
The storage adapter isolates the engine; business code does not issue engine queries directly.

## Security model

- Dedicated non-root service user plus systemd sandbox (`NoNewPrivileges`, `ProtectSystem=strict`,
  `ProtectHome`, restricted capabilities).
- No runtime writes to the system partition; data, queues, audit and logs live on a data volume.
- No `apt install`, no changes to rsyslog/journald, no adding the app user to system groups.
- Admin operations require a local token (or the one-time first-run claim); tokens are never logged.
- Credentials are stored only in `0600` application-private files and are redacted from logs/diagnostics.
- Source ACLs and per-source rate limits protect the receiver.

## Known limitations

- The storage engine currently has a **single global retention period** (default 14 days); per-class
  retention profiles are planned. The UI states this explicitly.
- Platform capabilities such as journald read authorization, persistent host logs, container log access
  and host audit integration depend on TOS-provided interfaces. The app reports them honestly and does
  not bypass the platform.
- This project does **not** claim regulatory compliance; it is “audit-ready”, not certified.

See [CHANGELOG.md](CHANGELOG.md) for release notes.

## License

MIT. Third-party components and licenses are listed in [NOTICE](NOTICE); VictoriaLogs is Apache-2.0.
