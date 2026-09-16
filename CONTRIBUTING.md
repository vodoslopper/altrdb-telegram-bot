# Contributing

Small, reviewable changes beat big ones. If a change touches event semantics or
the schema, please open an issue first: both are hard to undo once a deployment
has data.

## Development setup

```sh
git clone <your fork> && cd altrdb-telegram-bot
cp .env.example .env        # fill in TELEGRAM_BOT_TOKEN
make build test             # or: make lint test-race
make run                    # runs bin/altrdb-telegram-bot with .env
```

Requirements: Go 1.25 or newer. Nothing else — SQLite is compiled in as pure Go
(`modernc.org/sqlite`), so `CGO_ENABLED=0` builds work everywhere and no system
library is needed.

The test suite never touches the network. Telegram and ALT RDB are replaced by
`httptest.Server` instances, time by `internal/clock.Fake`, and the database by
a file in `t.TempDir()`.

## Before sending a patch

```sh
make fmt          # gofmt
make lint         # go vet + gofmt -l + staticcheck
make test-race    # the whole suite under -race
make build
```

All four must pass. CI is whatever you run locally; there is no excuse for a
patch that only builds on one machine.

## Rules that keep this codebase honest

1. **Timestamps are RFC 3339 UTC text, and comparisons happen in Go.**
   Never compare two stored timestamps in SQL: `"…T00:00:00.5Z"` sorts *before*
   `"…T00:00:00Z"` as plain text. The one exception is `next_attempt_at`, which
   is compared in SQL and is therefore always written through
   `store.formatDeadline`, rounding *up* to whole seconds.

2. **Deduplication lives in the database.** `UNIQUE (chat_id, fingerprint)` is
   the final guard. Do not add an in-memory "already sent" map; it is wrong
   after a restart and it is the bug this design exists to avoid.

3. **The fingerprint is a contract.** It is
   `task_id | task_repo | task_owner | task_try | task_iter | task_state |
   task_stage | task_changed`, hashed. Changing any field changes the meaning of
   every stored snapshot at once, so a change there needs a migration note in
   the README's upgrade section.

4. **Callback data stays under 64 bytes and carries no free-form values.**
   Database surrogate ids, small integers and short list hashes only. A list
   position without its hash is a stale-button bug waiting to happen.

5. **Answer every callback query.** Telegram leaves the client spinner running
   until `answerCallbackQuery` arrives, including on error paths. The
   `answerOnce` helper in `internal/telegram/callbacks.go` exists so this is
   exactly-once.

6. **Never log the bot token, the webhook secret, or a whole update body.**
   `newLogger` in `cmd/altrdb-bot/main.go` redacts the obvious keys; do not
   defeat it with a `fmt.Sprintf("%+v", cfg)` of your own.

7. **New migrations are new files.** `migrations/NNNN_description.sql`, applied
   once, never edited afterwards. Bump nothing by hand — the version comes from
   the file name.

8. **Unknown is a valid answer.** A task state this binary has never seen must
   be stored, offered in the UI and matched, not dropped. Same for unknown JSON
   fields: they are ignored, never fatal.

## Layout

| Path | Responsibility |
| --- | --- |
| `cmd/altrdb-bot` | flags, wiring, supervised shutdown |
| `internal/config` | environment parsing and validation |
| `internal/model` | domain types, fingerprint, nickname rules |
| `internal/store` | SQLite: schema, subscriptions, snapshots, outbox, lease |
| `internal/rdb` | ALT RDB HTTP client: timeouts, retries, Retry-After |
| `internal/poller` | ingestion loop, baseline, window health, maintenance |
| `internal/notifier` | outbox delivery, backoff, message rendering |
| `internal/telegram` | commands, checkbox keyboards, callbacks, guards |
| `internal/health` | `/livez`, `/readyz`, `/healthz`, `/metrics` |
| `internal/clock` | injectable time source |
| `migrations` | embedded versioned SQL |
| `deploy` | systemd unit and helpers |

## Testing notes

- Table-driven tests everywhere; a new branch in a parser wants a new row, not a
  new function.
- `internal/telegram/e2e_test.go` holds the scenarios that must keep working:
  configure → baseline → transition → exactly one message → restart → no
  duplicate. Extend it rather than duplicating the wiring in a new file.
- Time is a `*clock.Fake`. A test that calls `time.Sleep` to wait for a retry is
  a test that will flake; advance the fake clock instead.
- `go test -race ./...` is part of the definition of done.

## Things deliberately not built

See the "Limitations" section of the README for the current list and the
reasoning. If you want to implement one of them, say so in an issue first so the
trade-off is written down before the code is.
