ALTER TABLE notifications ADD COLUMN completed_at TEXT;

-- Sent rows have a delivery timestamp. Old dead rows have no recorded
-- completion time; keep them for a full retention period from this upgrade.
UPDATE notifications
SET completed_at = COALESCE(sent_at, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
WHERE status IN ('sent', 'dead');

CREATE INDEX idx_notifications_completed ON notifications (status, completed_at);
