-- 0001_init.sql -- initial schema for altrdb-telegram-bot.
--
-- Conventions used across every migration:
--   * all timestamps are TEXT holding RFC 3339 in UTC ("2026-09-16T14:23:32Z"),
--     so they sort lexicographically and port to PostgreSQL TIMESTAMPTZ later;
--   * every child table declares ON DELETE CASCADE towards its parent;
--   * anything the poller fans out over is covered by an index.

CREATE TABLE chats (
    id              INTEGER PRIMARY KEY,             -- Telegram chat id
    chat_type       TEXT    NOT NULL,                -- private | group | supergroup | channel
    username        TEXT    NOT NULL DEFAULT '',
    first_name      TEXT    NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,      -- 0 = paused / permanently disabled
    notify_mode     TEXT    NOT NULL DEFAULT 'all'
                  CHECK (notify_mode IN ('all', 'terminal')),
    disabled_reason TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
);

-- Maintainer nicknames a chat follows. Matching is exact and lower-case.
CREATE TABLE chat_maintainers (
    chat_id    INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    nickname   TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (chat_id, nickname)
);

-- Reverse index: given a task owner, which chats care about it.
CREATE INDEX idx_chat_maintainers_nickname ON chat_maintainers (nickname, chat_id);

-- Task states a chat follows, e.g. DONE, FAILED.
CREATE TABLE chat_states (
    chat_id    INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    state      TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    PRIMARY KEY (chat_id, state)
);

CREATE INDEX idx_chat_states_state ON chat_states (state, chat_id);

-- Branches a chat follows plus the activation watermark that stops the very
-- first poll after subscribing from replaying history.
CREATE TABLE chat_branches (
    chat_id           INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    branch            TEXT    NOT NULL,
    active_watermark  TEXT    NOT NULL,              -- only task_changed > this is notified
    created_at        TEXT    NOT NULL,
    PRIMARY KEY (chat_id, branch)
);

CREATE INDEX idx_chat_branches_branch ON chat_branches (branch, chat_id);

-- Multi-step command state (e.g. "/maintainers -> Add -> waiting for text").
CREATE TABLE conversation_state (
    chat_id    INTEGER PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    step       TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '',          -- small JSON blob, step specific
    updated_at TEXT    NOT NULL
);

-- Branch list as reported by GET /task/progress/all_tasks_branches. Kept as a
-- cache so the selector still works while RDB is unreachable.
CREATE TABLE rdb_branches (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    branch        TEXT    NOT NULL UNIQUE,
    sort_order    INTEGER NOT NULL DEFAULT 0,        -- position in the API response
    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL
);

-- Every task state ever observed. `known` marks the seeded lifecycle states;
-- anything else was learned from a live response and is still selectable.
CREATE TABLE rdb_states (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    state         TEXT    NOT NULL UNIQUE,
    known         INTEGER NOT NULL DEFAULT 0,
    sort_rank     INTEGER NOT NULL DEFAULT 1000,
    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL
);

-- Cached GET /site/all_maintainers_with_nicknames results, per branch.
CREATE TABLE maintainers_cache (
    branch           TEXT    NOT NULL,
    nickname         TEXT    NOT NULL,
    packager_name    TEXT    NOT NULL DEFAULT '',
    count_source_pkg INTEGER NOT NULL DEFAULT 0,
    fetched_at       TEXT    NOT NULL,
    PRIMARY KEY (branch, nickname)
);

CREATE INDEX idx_maintainers_cache_nick ON maintainers_cache (nickname);

-- Latest snapshot of every (task_repo, task_id) we have ever seen. The
-- fingerprint is what makes an event; comparing it decides "changed".
CREATE TABLE task_snapshots (
    task_repo     TEXT    NOT NULL,
    task_id       INTEGER NOT NULL,
    fingerprint   TEXT    NOT NULL,
    task_state    TEXT    NOT NULL,
    task_owner    TEXT    NOT NULL,
    task_try      INTEGER NOT NULL DEFAULT 0,
    task_iter     INTEGER NOT NULL DEFAULT 0,
    task_testonly INTEGER NOT NULL DEFAULT 0,
    task_stage    TEXT    NOT NULL DEFAULT '',
    task_message  TEXT    NOT NULL DEFAULT '',
    task_changed  TEXT    NOT NULL,
    packages      TEXT    NOT NULL DEFAULT '[]',     -- JSON array of "name evr" strings
    first_seen_at TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    PRIMARY KEY (task_repo, task_id)
);

CREATE INDEX idx_task_snapshots_changed ON task_snapshots (task_repo, task_changed);
CREATE INDEX idx_task_snapshots_owner ON task_snapshots (task_owner, task_repo);

-- Transactional outbox. UNIQUE (chat_id, fingerprint) is the final
-- deduplication guard: it holds across crashes and restarts because it lives
-- in the database, not in a process-local map.
CREATE TABLE notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id         INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    fingerprint     TEXT    NOT NULL,
    task_repo       TEXT    NOT NULL,
    task_id         INTEGER NOT NULL,
    payload         TEXT    NOT NULL,                -- JSON model.TaskEvent
    status          TEXT    NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sending', 'sent', 'dead')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT    NOT NULL,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    sent_at         TEXT,
    UNIQUE (chat_id, fingerprint)
);

-- Drives the delivery worker: "give me the due rows".
CREATE INDEX idx_notifications_due ON notifications (status, next_attempt_at);
CREATE INDEX idx_notifications_chat ON notifications (chat_id, status);

-- Per-branch polling health, surfaced by /healthz and /metrics.
CREATE TABLE poll_state (
    branch               TEXT    PRIMARY KEY,
    last_attempt_at      TEXT,
    last_success_at      TEXT,
    last_error           TEXT    NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    tasks_seen           INTEGER NOT NULL DEFAULT 0,
    oldest_task_changed  TEXT,
    window_saturated     INTEGER NOT NULL DEFAULT 0,
    baselined            INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT    NOT NULL
);

-- Single-instance lease. A second process against the same SQLite file must
-- refuse to start rather than double-poll and double-send.
CREATE TABLE app_lock (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    holder       TEXT    NOT NULL,
    pid          INTEGER NOT NULL DEFAULT 0,
    acquired_at  TEXT    NOT NULL,
    heartbeat_at TEXT    NOT NULL
);

-- Highest Telegram update_id already handed to the dispatcher, so a restart
-- resumes where it stopped instead of replaying the backlog.
CREATE TABLE telegram_offset (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    last_update_id INTEGER NOT NULL DEFAULT 0,
    updated_at     TEXT    NOT NULL
);

-- Free-form key/value state: baseline markers, schema notes, version.
CREATE TABLE meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
