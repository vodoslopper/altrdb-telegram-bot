-- Preserve the legacy replay boundary on upgrade. Future updates are deduped
-- individually, so an unseen lower ID is not confused with a replay.
ALTER TABLE telegram_offset ADD COLUMN replay_floor INTEGER NOT NULL DEFAULT 0;
UPDATE telegram_offset SET replay_floor = last_update_id;

CREATE TABLE telegram_update_receipts (
    update_id INTEGER PRIMARY KEY
);
