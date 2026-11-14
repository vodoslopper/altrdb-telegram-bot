-- updated_at was refreshed on every API observation, even without changes.
-- Preserve that observation time when upgrading existing snapshots.
ALTER TABLE task_snapshots ADD COLUMN last_seen_at TEXT NOT NULL DEFAULT '';
UPDATE task_snapshots SET last_seen_at = updated_at;
CREATE INDEX idx_task_snapshots_last_seen ON task_snapshots (last_seen_at);
