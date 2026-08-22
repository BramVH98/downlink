markdown
# downlink

A simple, self-hosted log (and metrics) server built from scratch in Go — no external database, no heavyweight dependencies. One binary, a write-ahead log for durability, and a web UI for browsing and live-tailing logs.

Built as a learning project and a genuinely useful tool for homelab setups: point Apache, Nginx, Docker, or any syslog-capable device at it, and get a searchable, filterable, real-time log dashboard.

## Features

- **Durable by design** — every write is fsynced to a write-ahead log (WAL) before being acknowledged. Survives hard crashes (`kill -9`), not just clean shutdowns.
- **Structured logs** — attach arbitrary JSON fields to any log line (`user`, `duration_ms`, whatever you want) and query on them: `?field=user:123`, `?field_gt=duration_ms:1000`.
- **Live tail** — a web UI tab that streams new logs in real time over Server-Sent Events, no refresh needed.
- **Syslog receiver** — a built-in UDP syslog listener (RFC 3164 and RFC 5424) so devices/services that already speak syslog (routers, NAS boxes, Nginx, Docker) can be pointed at it directly, no extra shipper script required.
- **Retention + compaction** — old data is automatically dropped and remaining segments consolidated on a schedule.
- **Two-tier auth** — an admin login for the dashboard/API, and separate write-only tokens for collectors, so a leaked ingestion token can never be used to read data back.
- **Config file support** — Apache-style `key = value` config file, with CLI flags able to override individual settings.
- **Batch ingestion** — send one log or an array of logs in a single HTTP call.
- **CSV/JSON export.**
- **Systemd-ready** — an install script sets up a dedicated system user, a systemd service, and (where relevant) auto-configures Docker's syslog log driver.
- **Fully tested** — WAL durability, crash recovery, retention correctness, and concurrency safety are all covered by automated tests, run on every push via GitHub Actions.

## Quick start

```bash
git clone https://github.com/BramVH98/downlink.git
cd downlink
go build -o logserver ./cmd/logserver
./logserver -auth-user=admin -auth-pass=yourpassword
```

Open `http://localhost:8080` in a browser.

Send a log:
```bash
curl -u admin:yourpassword -X POST localhost:8080/logs \
  -d '{"service":"auth-api","level":"error","message":"Database timeout","fields":{"duration_ms":1500}}'
```

Query it back:
```bash
curl -u admin:yourpassword "localhost:8080/logs?service=auth-api&level=error"
curl -u admin:yourpassword "localhost:8080/logs?field_gt=duration_ms:1000"
```

## Installing as a system service

```bash
curl -fsSL https://raw.githubusercontent.com/BramVH98/downlink/main/install.sh | sudo bash
```

This builds from source (installing Go via `apt` if needed), creates a dedicated `downlink` system user, sets up a config file at `/etc/downlink/logserver.conf` with a randomly generated admin password, installs a hardened systemd service, and detects Docker/Apache/Nginx on the machine — printing the exact config snippet to wire each one in.

Once installed:
```bash
sudo systemctl start downlink
sudo systemctl stop downlink
sudo systemctl status downlink
journalctl -u downlink -f
```

## Architecture

Write path: HTTP/syslog → Engine → WAL (fsync) → Memtable/LogStore (hot, in-RAM)
│
flush threshold reached
▼
Segment file (cold, on disk)

Read path: Query → merge(hot buffer, cold segments) → response


- **`internal/wal`** — the write-ahead log. Every write is length-prefixed, CRC32-checksummed, and fsynced before the caller is told "done." Replay on startup recovers everything since the last flush, and safely stops at a torn/corrupted tail (from a crash mid-write) rather than erroring out.
- **`internal/storage`** — the data types (`Point` for metrics, `LogEntry` for logs) and their binary encode/decode, plus `Memtable`/`LogStore` (the in-memory hot buffers) and `Segment` (immutable gob-encoded cold storage files).
- **`internal/engine`** — ties the above together: durable writes, merged hot+cold queries, flush-to-segment, and combined retention+compaction (dropping expired data and consolidating remaining segments into one file, in the same pass).
- **`internal/syslogrecv`** — the UDP syslog listener and RFC 3164/5424 parser.
- **`internal/tail`** — a small generic pub/sub broadcaster powering the live-tail SSE endpoint.
- **`internal/webui`** — the embedded single-page dashboard (Go's `//go:embed`, so it ships inside the binary).
- **`cmd/logserver`** — the HTTP server, auth, config file parsing, and everything that wires the above into a runnable service.

## HTTP API

| Endpoint | Method | Description |
|---|---|---|
| `/logs` | `POST` | Ingest one log object or a JSON array of them |
| `/logs` | `GET` | Query logs — see filters below |
| `/tail` | `GET` | Server-Sent Events stream of new logs as they arrive |
| `/services` | `GET` | List distinct service names seen |
| `/stats` | `GET` | Counts by level and by service |
| `/export/logs` | `GET` | Export matching logs as JSON or CSV (`?format=csv`) |
| `/health` | `GET` | Health check (no auth required) |

### Query filters (`GET /logs`)

| Param | Example | Meaning |
|---|---|---|
| `service` | `?service=auth-api` | Exact match on service/source |
| `level` | `?level=error` | Exact match on level |
| `contains` | `?contains=timeout` | Case-insensitive substring match on the message |
| `start` / `end` | `?start=1700000000000` | Unix milliseconds, inclusive range |
| `field` | `?field=user:123` | Exact match on a structured field (repeatable) |
| `field_gt` / `field_lt` | `?field_gt=duration_ms:1000` | Numeric comparison on a structured field |
| `limit` / `offset` | `?limit=50&offset=100` | Pagination |

### Ingesting a log

```json
POST /logs
{
  "service": "auth-api",
  "level": "error",
  "message": "Database timeout",
  "timestamp": "2026-01-01T12:00:00Z",
  "fields": { "user": "123", "duration_ms": 1500 }
}
```

`timestamp` is optional (defaults to now) and accepts either an RFC3339 string or a unix-milliseconds number. A batch is just an array of the same shape.

## Auth

Two separate credential types, deliberately not interchangeable:

- **Admin login** (`-auth-user` / `-auth-pass`, HTTP Basic Auth) — full access: read, export, dashboard.
- **Collector tokens** (`-ingest-tokens="apache:sometoken,nginx:othertoken"`, sent as `Authorization: Bearer <token>`) — write-only. A token can POST to `/logs`, and nothing else — it cannot read data back, export, or view the dashboard, even if leaked.

If `-auth-user` is left empty, the server runs with no authentication at all — fine for local testing, not recommended for anything reachable beyond localhost.

## Syslog receiver

```bash
./logserver -syslog-addr=:5514 -syslog-allow=192.168.1.0/24
```

Syslog over UDP has no authentication built into the protocol itself, so `-syslog-allow` (a comma-separated list of IPs/CIDRs) is the only access control available — use it. Things that can be pointed at this directly, no extra script needed:

- **Nginx** (native syslog support): `access_log syslog:server=HOST:5514,tag=nginx combined;`
- **Docker**: `--log-driver=syslog --log-opt syslog-address=udp://HOST:5514`
- **Routers/NAS boxes**: almost all have a "Remote Syslog Server" field in their admin UI
- **Apache** (via the standard `logger` utility): `CustomLog "|/usr/bin/logger -n HOST -P 5514 -d -t apache" combined`

## Config file

Instead of long command lines, settings can live in a file:
/etc/downlink/logserver.conf

addr = :8080
auth-user = admin
auth-pass = supersecret
data = /var/lib/downlink/data
retention = 720h
retention-check = 1h
syslog-addr = :5514
syslog-allow = 127.0.0.1/32


```bash
./logserver -config=/etc/downlink/logserver.conf
```

Any flag also passed on the command line overrides the matching config file value — the file sets steady-state defaults, flags are for one-off overrides.

## All flags

| Flag | Default | Description |
|---|---|---|
| `-data` | `./data` | Directory for the WAL and segment files |
| `-addr` | `:8080` | HTTP listen address |
| `-auth-user` / `-auth-pass` | *(empty)* | Admin credentials; empty disables auth |
| `-ingest-tokens` | *(empty)* | `name:token,name2:token2` — write-only collector credentials |
| `-flush-threshold` | `5000` | Flush hot buffers to a segment after this many total points+logs |
| `-retention` | `720h` (30 days) | How long to keep data before it's dropped |
| `-retention-check` | `1h` | How often the retention+compaction sweep runs |
| `-syslog-addr` | *(empty)* | UDP address to receive syslog on; empty disables it |
| `-syslog-allow` | *(empty)* | Comma-separated IPs/CIDRs allowed to send syslog |
| `-config` | *(empty)* | Path to a config file |

## Testing

```bash
go test ./...              # run everything
go test ./... -race        # with the race detector (recommended)
go test ./internal/wal -v  # a specific package, verbose
```

Test coverage focuses on the parts where correctness actually matters:

- **`wal`** — torn/truncated writes, corrupted checksums, reopen-and-append, concurrent writes under `-race`.
- **`storage`** — encode/decode round-trips, exhaustive truncated-data handling, structured-field query matching.
- **`engine`** — crash recovery via WAL replay, flush-to-segment, retention correctness (including a regression test for a real bug where WAL-replayed data was briefly invisible to retention).
- **`syslogrecv`** — RFC 3164/5424 parsing edge cases, including a regression test for a real crash found during manual testing.
- **`tail`** — pub/sub fan-out, cancellation semantics, and the non-blocking-on-a-full-subscriber guarantee, under `-race`.

CI runs `gofmt`, `go vet`, a full build, and the race-enabled test suite on every push to `main` via GitHub Actions.

## Status / roadmap

**Done:** WAL durability, crash recovery, retention+compaction, structured fields, batch ingestion, live tail, syslog receiver, two-tier auth, config file, systemd install script, full test coverage, CI.

**Not yet done:** prebuilt release binaries (install script currently builds from source), segment compression, a Docker image, Grafana-compatible query API.

This is a personal project, not a company's, and not built with or using any employer's resources or code.

## License

MIT
