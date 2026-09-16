# altrdb-telegram-bot

A self-hosted Telegram bot that tells you when an **ALT Linux Girar build task**
changes, filtered by the maintainers, task states and repository branches *you*
picked — configured entirely from inside Telegram, with checkbox-style inline
keyboards.

It polls the public [ALT RDB (ALTRepo) API](https://rdb.altlinux.org/api/docs),
detects real transitions, and delivers each one **exactly once**, even across
crashes and restarts.

```
✅ Task #433219: BUILDING → DONE
Branch: sisyphus
Owner: @glebfm
Try/iteration: 2/1
Stage: task-save-repo
Packages: postfix 3.8.21-alt1
Changed: 2026-09-16 13:56 UTC
[ Open task #433219 ]
```

---

## Table of contents

- [Why polling](#why-polling)
- [Architecture](#architecture)
- [How an event becomes a message](#how-an-event-becomes-a-message)
- [Limitations you should know](#limitations-you-should-know)
- [Quick start](#quick-start)
- [Creating the bot with BotFather](#creating-the-bot-with-botfather)
- [User commands](#user-commands)
- [Configuration reference](#configuration-reference)
- [Health, readiness and metrics](#health-readiness-and-metrics)
- [Running under systemd](#running-under-systemd)
- [Running with Docker or Podman Compose](#running-with-docker-or-podman-compose)
- [Backup and restore](#backup-and-restore)
- [Upgrades and migrations](#upgrades-and-migrations)
- [Single-instance protection](#single-instance-protection)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [License](#license)

---

## Why polling

The published ALT RDB OpenAPI document (`GET /api/openapi.json`) exposes **no
task-event webhook and no streaming endpoint**. There is nothing to subscribe
to, so ingestion is polite HTTP polling:

- `GET /task/progress/all_tasks_branches` — the branch list, refreshed on a
  timer and cached, so the selector keeps working during an outage.
- `GET /task/progress/last_tasks?branch=<b>&tasks_limit=<n>` — the newest
  current records for one branch. **One fetch per active branch**, fanned out
  locally to every subscriber.
- `GET /site/all_maintainers_with_nicknames?branch=<b>` — the packager directory,
  cached, used to warn about (never block) an unknown nickname.
- `GET /task/progress/task_info/{id}` — on-demand detail for the `/task`
  diagnostic command only.

`GET /task/progress/find_tasks` is available but is not used in the polling path:
its `input` parameter is mandatory and a per-user search does not scale to many
subscribers. The deprecated `GET /site/tasks_by_maintainer` is not used at all.

The client sets a descriptive `User-Agent`, uses explicit connect / TLS /
response-header / overall timeouts, retries with bounded exponential backoff and
equal jitter, honours `Retry-After`, enforces a minimum spacing between
requests, caps the response size, and never polls the same branch twice at once.

## Architecture

```
                ┌──────────────────────────────────────────────────────────┐
                │                    altrdb-telegram-bot                   │
                │                                                          │
 ALT RDB API ──►│ internal/rdb      timeouts · retries · Retry-After · UA  │
 (HTTP poll)    │        │                                                 │
                │        ▼                                                 │
                │ internal/poller   one fetch per ACTIVE branch            │
                │        │          baseline · window health · pruning     │
                │        ▼                                                 │
                │ ┌─────────────────────────── SQLite (WAL) ─────────────┐  │
                │ │ task_snapshots   latest fingerprint per (repo, id)   │  │
                │ │ notifications    outbox, UNIQUE(chat_id,fingerprint) │  │
                │ │ chats · chat_{maintainers,states,branches}           │  │
                │ │ rdb_branches · rdb_states · maintainers_cache        │  │
                │ │ conversation_state · poll_state · app_lock · offset  │  │
                │ └──────────────────────────────────────────────────────┘  │
                │        ▲                            │                    │
                │        │                            ▼                    │
 Telegram   ───►│ internal/telegram           internal/notifier            │
 (updates)      │ commands · ☑/☐ keyboards     claim → send → sent/retry   │
                │ callbacks · guards           backoff · permanent failure │
                │        │                            │                    │
                │        ▼                            ▼                    │
                │ internal/health   /livez /readyz /healthz /metrics       │
                └──────────────────────────────────────────────────────────┘
```

| Package | Responsibility |
| --- | --- |
| `cmd/altrdb-bot` | flags, wiring, supervised goroutines, graceful shutdown |
| `internal/config` | environment parsing and validation |
| `internal/model` | domain types, fingerprint, state ordering, nickname rules |
| `internal/store` | SQLite: schema, subscriptions, snapshots, outbox, lease |
| `internal/rdb` | ALT RDB HTTP client |
| `internal/poller` | ingestion loop, baseline, window health, maintenance |
| `internal/notifier` | outbox delivery and message rendering |
| `internal/telegram` | commands, checkbox keyboards, callbacks, access guards |
| `internal/health` | liveness, readiness, status document, Prometheus metrics |
| `internal/clock` | injectable time source (every test uses a fake clock) |
| `migrations` | embedded, versioned SQL |

Design choices worth stating:

- **`github.com/go-telegram/bot` v1.27.0** (pinned). It is actively maintained,
  tracks the current Bot API, supports long polling *and* webhooks, exposes
  `WithInitialOffset` (so the persisted update offset survives a restart),
  `WithServerURL` (so tests can point it at `httptest`), and surfaces Telegram
  error classes as sentinel errors the delivery worker can classify. There was
  no reason to hand-roll a protocol client.
- **`modernc.org/sqlite` v1.59.0** (pinned). A pure-Go SQLite, so the default
  build has `CGO_ENABLED=0` and the binary is static — no libc or `libsqlite3`
  on the host, and a `FROM scratch`-style image is possible.
- Repository methods are narrow and SQL-specific rather than a generic
  abstraction layer; a PostgreSQL port means reimplementing `internal/store`
  against the same method set, which is the point.

## How an event becomes a message

1. **Fingerprint.** Each record is reduced to a deterministic hash of
   `task_id | task_repo | task_owner | task_try | task_iter | task_state |
   task_stage | task_changed`. Presentation-only fields (message text, package
   list, testonly flag) are deliberately *excluded*, so cosmetic churn does not
   produce notifications.
2. **Snapshot compare.** The newest snapshot per `(task_repo, task_id)` is
   stored. A differing fingerprint — including "no snapshot at all" — is a
   candidate event.
3. **Fan-out.** Every enabled chat whose *maintainer*, *state* and *branch*
   selection matches the record right now, and whose **activation watermark**
   for that branch is older than `task_changed`, gets one outbox row. Chats in
   "terminal only" mode are filtered to `TERMINAL_STATES`.
4. **One transaction.** Snapshot writes and outbox inserts commit together. A
   crash cannot record "already seen" for a task nobody was told about.
5. **`UNIQUE (chat_id, fingerprint)`** is the final deduplication guard. It is
   a database constraint, not an in-memory map, so repeated polls, overlapping
   windows and restarts are all covered by the same rule.
6. **Delivery.** A worker claims due rows (`pending` → `sending` inside one
   `BEGIN IMMEDIATE` transaction), sends them, and marks them `sent`. Transient
   failures cost one attempt and are rescheduled with exponential backoff plus
   jitter; Telegram `429 retry_after` reschedules *without* consuming the retry
   budget; `403`/`400`/`401` are permanent — the row goes `dead`, the chat is
   disabled with the reason recorded, and its queued rows are cancelled.
   Rows left in `sending` by a crash are recovered to `pending` at start-up and
   charged one attempt, so a poison message cannot loop forever.

### Baselines and watermarks

Two independent mechanisms stop the bot from replaying history:

- **Branch baseline.** The first successful poll of a branch that has *no stored
  snapshots* records them and queues nothing. This covers a fresh install and a
  wiped snapshot table.
- **Per-chat activation watermark.** Ticking a branch stamps
  `chat_branches.active_watermark = now`; only records with
  `task_changed > watermark` are ever notified for that chat. Ticking a branch
  off and on again re-stamps it.

Consequences, all intentional:

- A brand-new subscription never receives the tasks currently sitting in the
  feed.
- Editing maintainers or states never replays anything: matching happens when
  the event occurs.
- A record whose `task_changed` is *exactly* equal to the watermark is not
  notified. Timestamps are second-granular, and "equal" cannot be distinguished
  from "earlier", so the bot errs towards silence.

## Limitations you should know

These follow from polling a **capped current-state feed**, not from this
implementation:

1. **`last_tasks` is not a log.** It returns the newest `tasks_limit` records.
   A task that changes twice inside one polling interval is observed once, in
   its final state; intermediate states are unobservable. Raising
   `RDB_TASKS_LIMIT` and lowering `POLL_INTERVAL` narrows the gap but cannot
   close it.
2. **A fast task can be missed entirely** if more than `tasks_limit` records
   change in one interval and push it out of the window.
3. **Window health is observable, not guaranteed.** When a poll returns a full
   window whose oldest record is newer than `RDB_OVERLAP_WINDOW`, the bot sets
   `poll_window_saturated{branch}=1`, exposes `window_warning` on `/healthz`,
   and logs a warning naming the knob to turn. Watch that metric.
4. **Deleted tasks disappear** from the feed and are never reported as such;
   their snapshots are simply pruned after `SNAPSHOT_RETENTION`.
5. **The overlap window cannot recover from a long outage.** After downtime
   longer than the window's reach, whatever happened in between is gone. The
   bot baselines forward instead of inventing history.
6. **`/recent` reads the local snapshot table**, so it can only show tasks the
   bot has already seen — by design, it never adds API load and never moves a
   watermark.
7. **The maintainer directory is a cache.** A nickname can be absent from the
   directory of one branch and present in another, so an unknown nickname is
   saved with a warning rather than rejected.
8. **One process per database.** SQLite is single-writer; see
   [Single-instance protection](#single-instance-protection).

## Quick start

Requirements: Go 1.25+ and network access to the Go module proxy. Nothing else —
SQLite is compiled in.

```sh
git clone <this repository> && cd altrdb-telegram-bot
cp .env.example .env
chmod 600 .env
$EDITOR .env                      # set TELEGRAM_BOT_TOKEN
make build
make check-config                 # validates .env without starting anything
make migrations DATABASE_PATH=./bot.db
make run                          # or: ./bin/altrdb-telegram-bot
```

Then, in Telegram: `/start` → `/maintainers` → `/states` → `/branches`.

### Verifying the RDB path without a bot token

`-poll-once` runs a single ingestion cycle against the configured database and
exits. It is the fastest way to prove that the network, the live API schema and
the database all agree:

```sh
$ DATABASE_PATH=/tmp/altrdb-smoke/bot.db ./bin/altrdb-telegram-bot -migrate
database /tmp/altrdb-smoke/bot.db is at schema version 1

$ DATABASE_PATH=/tmp/altrdb-smoke/bot.db ./bin/altrdb-telegram-bot -poll-once
branches from RDB: sisyphus, p11, p10, p9, p8, c11f1, c10f2, c10f1, c9f2, c9f1, c8.1, c8, c7.1, c7, icarus
no branch is followed yet, so nothing was polled.
Start the bot and tick a branch with /branches, then run this again.
```

With a chat that follows something (normally created by `/branches` in
Telegram), the same command reports the poll result:

```
poll interval: 60s, tasks_limit: 200, tracked snapshots: 200
outbox: 0 pending, 0 sending, 0 dead
  sisyphus   tasks=200  baselined=true  last_success=2026-09-16T16:29:40Z oldest_change=2026-09-14T14:54:02Z saturated=false failures=0
```

Run it twice: the second run must not re-baseline and must not create duplicate
outbox rows.

## Creating the bot with BotFather

1. Open [@BotFather](https://t.me/BotFather) and send `/newbot`.
2. Choose a display name, e.g. `ALT build tasks`.
3. Choose a username ending in `bot`, e.g. `alt_build_tasks_bot`.
4. Copy the token into `TELEGRAM_BOT_TOKEN`. Treat it as a password: anyone
   holding it *is* your bot.
5. Optional but recommended:
   - `/setdescription` — what the bot does.
   - `/setcommands` — paste the list below so clients autocomplete it:
     ```
     start - register this chat and show the summary
     settings - current subscription, one tap to each screen
     maintainers - add or remove packager nicknames
     states - checkbox list of task states
     branches - checkbox list of repository branches
     recent - matching tasks right now, read-only
     pause - stop deliveries, keep the settings
     resume - start deliveries again
     test - send one sample notification
     task - fetch one task from RDB by id
     cancel - abandon a half-finished prompt
     help - command list
     ```
6. Find your own numeric user id (e.g. from [@userinfobot](https://t.me/userinfobot))
   if you want `ALLOWED_TELEGRAM_USER_IDS`.

## User commands

| Command | What it does |
| --- | --- |
| `/start` | Registers the chat, re-enables it if it was blocked before, shows the summary and the settings keyboard |
| `/settings` | Current maintainers, states, branches, status and mode, with one button per screen |
| `/maintainers` | Checkbox list of followed nicknames; `➕ Add` starts a prompt, each `☑ name` removes it |
| `/maintainers grenka rider` | Adds nicknames inline, without the prompt |
| `/states` | Checkbox list of task states (seeded lifecycle states, then anything learned from the feed, marked `⁇`) |
| `/states DONE FAILED` | Replaces the state selection |
| `/branches` | Checkbox list of branches loaded from RDB and cached |
| `/branches sisyphus p11` | Replaces the branch selection |
| `/recent [n]` | Up to 30 currently matching tasks from the local snapshot table. Never touches the watermark |
| `/pause`, `/resume` | Stop and restart delivery; settings are kept |
| `/test` | Sends one sample notification built from your saved settings, straight to the chat (not through the outbox) |
| `/task <id>` | Fetches one task from `task_info` and shows it with its approvals |
| `/cancel` | Abandons a pending prompt |
| `/help` | Command list |

Keyboard conventions:

- Every toggle button starts with `☑` (on) or `☐` (off) and edits **the same
  message** — Telegram has no native checkboxes.
- `✔ Select all`, `✖ Clear all`, `« Prev | 2/3 | Next »`, `⟳ Refresh` and
  `✅ Done` are always present. `⟳ Refresh` on the branch screen forces a new
  catalogue fetch; `✅ Done` returns to the settings hub, and from the hub it
  drops the keyboard.
- Callback data is at most 64 bytes and carries database surrogate ids, page
  numbers and short list hashes — never a raw nickname, state or branch name.
  A button whose list hash no longer matches is answered with
  *"That list changed under you — tap ⟳ Refresh."* instead of acting on the
  wrong row.
- A callback on a message the bot did not send, in another private chat, or
  older than 48 hours (Telegram makes it inaccessible) is refused with an
  explanation. Every callback is answered, including on every error path, so
  the client spinner always stops.

Nicknames are trimmed, stripped of a leading `@`, lower-cased, validated against
`[a-z0-9][a-z0-9._+-]{0,63}` and de-duplicated. Input may be separated by
spaces, commas, semicolons or newlines. Rejected tokens are listed back to you
rather than dropped silently.

Groups and channels are refused with a one-line explanation unless
`ALLOW_TELEGRAM_GROUPS=true`. With `ALLOWED_TELEGRAM_USER_IDS` set, everyone
else is told the bot is restricted, at most once per hour per chat, and is never
registered.

## Configuration reference

All configuration is environment variables. `make check-config` (or
`./bin/altrdb-telegram-bot -check-config`) validates them and prints the
resolved, **redacted** configuration. Every problem is reported at once, not one
per run.

| Variable | Default | Meaning |
| --- | --- | --- |
| `TELEGRAM_BOT_TOKEN` | — | **Required to serve.** Not required by `-migrate` or `-healthcheck` |
| `TELEGRAM_MODE` | `polling` | `polling` or `webhook`. Mutually exclusive |
| `TELEGRAM_WEBHOOK_URL` | — | Required in webhook mode, must be `https://` |
| `TELEGRAM_WEBHOOK_SECRET` | — | Required in webhook mode, ≥ 16 characters |
| `ALLOWED_TELEGRAM_USER_IDS` | empty | Comma-separated allowlist; empty means anyone |
| `ALLOW_TELEGRAM_GROUPS` | `false` | Allow group and supergroup chats |
| `RDB_BASE_URL` | `https://rdb.altlinux.org/api` | API root |
| `POLL_INTERVAL` | `60s` | Per-branch poll interval, **minimum `15s`** |
| `RDB_TASKS_LIMIT` | `200` | `tasks_limit` per poll, 1–5000 |
| `RDB_OVERLAP_WINDOW` | `24h` | How far back a full window should reach before it is reported as saturated |
| `RDB_MAX_CONCURRENT_BRANCHES` | `3` | Branches polled in parallel, 1–16 |
| `BRANCH_REFRESH_INTERVAL` | `30m` | How often the branch catalogue is refetched |
| `MAINTAINER_CACHE_TTL` | `6h` | How often a followed branch's packager directory is refetched |
| `RDB_USER_AGENT` | `altrdb-telegram-bot/<version>` | Sent on every RDB request |
| `HTTP_TIMEOUT` | `30s` | Overall per-request timeout |
| `HTTP_CONNECT_TIMEOUT` | `10s` | TCP connect timeout |
| `HTTP_TLS_TIMEOUT` | `10s` | TLS handshake timeout |
| `DATABASE_PATH` | `/var/lib/altrdb-telegram-bot/bot.db` | SQLite file; the directory is created |
| `SNAPSHOT_RETENTION` | `720h` | Snapshots with an older `task_changed` are pruned |
| `LEASE_TTL` | `90s` | How long a lease stays valid without a heartbeat |
| `LEASE_HEARTBEAT` | `30s` | Heartbeat interval; must be shorter than the TTL |
| `NOTIFY_INTERVAL` | `5s` | How often the outbox is drained, ≥ `1s` |
| `NOTIFY_MAX_ATTEMPTS` | `8` | Transient failures before a row goes `dead`, 1–100 |
| `NOTIFY_SEND_SPACING` | `50ms` | Minimum gap between two outgoing messages |
| `TERMINAL_STATES` | `DONE,FAILED` | States treated as final by "terminal only" mode |
| `HTTP_LISTEN_ADDR` | `127.0.0.1:8080` | Health/metrics listener, **loopback by default** |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | `text` or `json` |

### Webhook mode

Polling is the default because it needs no public inbound endpoint. In webhook
mode the same health listener also serves `POST /telegram/webhook`; no second
port is opened.

- The request must carry `X-Telegram-Bot-Api-Secret-Token` equal to
  `TELEGRAM_WEBHOOK_SECRET` (the library verifies it).
- Bodies are capped at 1 MiB before parsing.
- Terminate TLS at your reverse proxy and forward to `HTTP_LISTEN_ADDR`.
- Register the URL once with `./bin/altrdb-telegram-bot -set-webhook`.
- Switching back to polling is automatic: the process calls `deleteWebhook` at
  start-up, because `getUpdates` fails with `409` while a webhook is registered.
  `-delete-webhook` does it manually.
- The webhook carries **Telegram** interactions only. RDB ingestion is still
  polling either way.

Reverse proxy sketch (nginx):

```nginx
location /telegram/webhook {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    client_max_body_size 1m;
}
```

## Health, readiness and metrics

| Endpoint | Meaning |
| --- | --- |
| `GET /livez` | `200 ok` while the process is up. Touches nothing |
| `GET /readyz` | `200` when the database answers and the Telegram update loop is running; `503` otherwise, with the JSON body explaining why |
| `GET /healthz` | Full status document (below). `503` when degraded |
| `GET /metrics` | Prometheus exposition from a private registry |
| `GET /` | Redirects to `/healthz` |

`/healthz` reports, among other things: build version and commit, uptime,
database driver/OK/schema version, Telegram mode, whether the loop is running,
the persisted update offset and the last update time, per-branch poll state
(last attempt, last success, consecutive failures, last error, tasks seen,
oldest observed `task_changed`, window saturation, baselined), outbox backlog
(pending/sending/dead and the age of the oldest pending row), chat counts, the
lease holder, and a `window_warning` when a poll window is too small.

Useful metrics:

```
altrdb_polls_total{branch,result}
altrdb_poll_duration_seconds_bucket{branch}
altrdb_tasks_seen{branch}
altrdb_poll_window_saturated{branch}          # alert on this
altrdb_last_successful_poll_timestamp_seconds{branch}
altrdb_events_created_total
altrdb_duplicate_events_suppressed_total
altrdb_notifications_queued_total
altrdb_outbox_pending                          # alert when it grows
altrdb_outbox_dead_total
altrdb_telegram_sends_total{result}
altrdb_telegram_updates_total{kind}
altrdb_telegram_loop_running
altrdb_rdb_requests_total / altrdb_rdb_retries_total / altrdb_rdb_rate_limited_total
altrdb_chats_total / altrdb_chats_enabled
altrdb_build_info{version,commit}
```

Two alerts worth having: `altrdb_poll_window_saturated == 1` for more than a few
cycles (raise `RDB_TASKS_LIMIT`), and `altrdb_outbox_pending` growing steadily
(Telegram is rejecting or rate-limiting deliveries).

## Running under systemd

```sh
make build
sudo sh deploy/install.sh          # binary + unit + /etc/altrdb-telegram-bot/bot.env
sudo $EDITOR /etc/altrdb-telegram-bot/bot.env
sudo systemctl enable --now altrdb-telegram-bot
journalctl -u altrdb-telegram-bot -f
curl -s 127.0.0.1:8080/healthz
```

`deploy/install.sh` is POSIX `sh`, idempotent, and keeps an existing `bot.env`.

The unit uses `DynamicUser=yes` with `StateDirectory=altrdb-telegram-bot`, so
systemd allocates a transient uid and owns `/var/lib/altrdb-telegram-bot` — no
system user to create. A commented variant at the bottom of the unit shows the
dedicated-user setup for older systemd versions.

Sandboxing enabled: `NoNewPrivileges`, `PrivateTmp`, `PrivateDevices`,
`ProtectSystem=strict`, `ProtectHome`, `ProtectKernel{Tunables,Modules,Logs}`,
`ProtectControlGroups`, `ProtectClock`, `ProtectHostname`,
`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, `RestrictNamespaces`,
`RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`,
`MemoryDenyWriteExecute`, `SystemCallArchitectures=native`,
`SystemCallFilter=@system-service`, empty `CapabilityBoundingSet`, `UMask=0077`.
`ReadWritePaths=/var/lib/altrdb-telegram-bot` is what keeps SQLite (including
its `-wal` and `-shm` files) writable, and `RestrictAddressFamilies` keeps
outbound HTTPS to RDB and Telegram working.

## Running with Docker or Podman Compose

```sh
cp .env.example .env && chmod 600 .env   # set TELEGRAM_BOT_TOKEN
docker compose up -d --build             # or: podman-compose up -d --build
docker compose logs -f
curl -s 127.0.0.1:8080/healthz
```

The image is multi-stage: `golang:1.25-alpine` builds a static binary with
`CGO_ENABLED=0` and `-trimpath`, runs the test suite inside the build, and
`alpine:3.22` runs it as uid/gid `65532` with a read-only root filesystem,
`no-new-privileges`, all capabilities dropped, and a `HEALTHCHECK` that runs the
binary's own `-healthcheck` flag. The database lives on the named volume
`bot-data` mounted at `/var/lib/altrdb-telegram-bot`. The published health port
is bound to `127.0.0.1` on the host.

Inside a container `HTTP_LISTEN_ADDR` is set to `0.0.0.0:8080` — otherwise the
orchestrator cannot reach the probe. Go back to `127.0.0.1:8080` if you use host
networking.

## Backup and restore

Everything is in one SQLite file plus its WAL sidecars.

```sh
# Consistent online backup (never copy the live file directly):
sqlite3 /var/lib/altrdb-telegram-bot/bot.db ".backup '/var/backups/altrdb-bot-$(date -u +%F).db'"

# Or with the binary stopped:
systemctl stop altrdb-telegram-bot
cp -a /var/lib/altrdb-telegram-bot /var/backups/altrdb-telegram-bot

# Restore:
systemctl stop altrdb-telegram-bot
install -d -m 0700 /var/lib/altrdb-telegram-bot
cp /var/backups/altrdb-bot-2026-09-16.db /var/lib/altrdb-telegram-bot/bot.db
systemctl start altrdb-telegram-bot
```

Also back up `/etc/altrdb-telegram-bot/bot.env` (mode `0600`) — it holds the bot
token.

What a restore implies:

- Restoring an **older** database replays nothing: the outbox rows that were
  already `sent` are still there, so their `UNIQUE (chat_id, fingerprint)` rows
  block a second delivery.
- Restoring a database whose `telegram_offset` is older than Telegram's
  unconfirmed queue can redeliver a few *updates*; commands are idempotent
  enough for that, and a replayed update id at or below the persisted offset is
  ignored outright.
- Losing the file entirely is safe but noisy in one specific way: with no
  snapshots, the next poll of each branch is a **baseline**, so nothing is sent
  until tasks change again.

## Upgrades and migrations

Migrations are embedded in the binary, versioned as `migrations/NNNN_*.sql`, and
applied at start-up in one transaction each, recorded in `schema_migrations`.
They are idempotent: an already-applied version is skipped.

```sh
./bin/altrdb-telegram-bot -list-migrations     # what this build carries
./bin/altrdb-telegram-bot -migrate             # apply and report the version
```

Upgrade procedure:

```sh
systemctl stop altrdb-telegram-bot
sqlite3 /var/lib/altrdb-telegram-bot/bot.db ".backup '/var/backups/pre-upgrade.db'"
make build && sudo sh deploy/install.sh
systemctl start altrdb-telegram-bot
curl -s 127.0.0.1:8080/healthz | grep schema_version
```

Migrations only ever add. If a future release changes the fingerprint
definition, every stored snapshot will look "changed" once; the release notes
must say so, and the watermarks still prevent a history blast for chats that
subscribed earlier than those records.

## Single-instance protection

SQLite is single-writer, and two pollers would fetch the same feed and race for
the same outbox rows. The process claims a lease row (`app_lock`) inside an
`IMMEDIATE` transaction:

- No lease row → claim it.
- Lease held by us → heartbeat.
- Lease held by someone else whose heartbeat is newer than `LEASE_TTL` →
  **refuse to start**, with an error naming the holder.
- Lease held by someone else whose heartbeat is older than the TTL → steal it,
  with a warning in the log.

The lease is renewed every `LEASE_HEARTBEAT`; if it is ever lost, the process
shuts itself down rather than continuing to write to a database another process
now owns. It is released on any clean exit, so a restart does not wait for the
TTL. `DATABASE_PATH` must be the same file for this to work: two processes on
two different files are two independent bots.

Do not run more than one replica. There is no leader election and no shared
delivery queue; the outbox is designed for exactly one worker.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `another instance is already using …` | A live process holds the lease, or one crashed less than `LEASE_TTL` ago. Check `systemctl status`, then wait, or `sqlite3 bot.db 'DELETE FROM app_lock'` if you are certain nothing is running |
| `error call getMe` at start-up | Wrong `TELEGRAM_BOT_TOKEN`, or no outbound HTTPS to `api.telegram.org`. Verify with `curl -s https://api.telegram.org/bot<TOKEN>/getMe` |
| `409 Conflict: terminated by other getUpdates request` | Another process polls with the same token, or a webhook is still registered. Only one poller may run; in polling mode the process calls `deleteWebhook` at start-up |
| No notifications at all | `/settings` shows what is missing. All three of maintainers, states and branches must be set, the chat must be enabled, and the task must change *after* the branch watermark |
| Notifications stopped | Check `chats.disabled_reason`: a `403` from Telegram disables the chat and records why. `/start` re-enables it after the user unblocks the bot |
| `poll_window_saturated` is 1 | More than `RDB_TASKS_LIMIT` tasks changed inside `RDB_OVERLAP_WINDOW`. Raise `RDB_TASKS_LIMIT` |
| `consecutive_failures` grows | RDB is unreachable or slow. The cached branch list and snapshots are untouched; polling resumes on its own |
| `outbox_pending` grows | Telegram is rejecting or rate-limiting sends. `altrdb_telegram_sends_total{result}` says which; `notifications.last_error` has the text |
| Webhook does nothing | The public URL must reach `POST /telegram/webhook`, the header secret must match, and Telegram must have been told: `-set-webhook` |
| Group chat gets one line and nothing else | Expected. Set `ALLOW_TELEGRAM_GROUPS=true` to support groups |
| Database is huge | `SNAPSHOT_RETENTION` prunes old snapshots hourly; delivered rows are pruned after 14 days. `VACUUM` reclaims the space |

Logs are structured (`LOG_FORMAT=json` for machine parsing) and never contain
the bot token, the webhook secret, or a full update body.

## Development

```sh
make help          # every target
make fmt           # gofmt
make lint          # go vet + gofmt -l + staticcheck
make test          # unit and integration tests
make test-race     # the same under -race
make cover         # coverage.out plus the total
make build         # bin/altrdb-telegram-bot
make migrations DATABASE_PATH=./bot.db
make docker        # build the image (docker or podman, whichever exists)
```

The suite never touches the network: Telegram and ALT RDB are `httptest`
servers, time is `internal/clock.Fake`, and every database is a file in
`t.TempDir()`. `internal/telegram/e2e_test.go` holds the scenarios that must
keep working — configure → baseline → transition → exactly one message →
restart → repeated API response → no duplicate — plus a Telegram outage and an
RDB outage.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the invariants that keep the design
honest (timestamp comparisons in Go, deduplication in the database, callback
data hygiene, migrations as new files).

## License

MIT — see [LICENSE](LICENSE). This repository is an independent client of the
ALT RDB API; the ALTRepo server itself is AGPLv3 and is not vendored here.
