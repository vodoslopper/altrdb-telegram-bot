-- Keep compact per-chat event identities after payload history expires.
-- This preserves the outbox's existing no-repeat behavior for all statuses.
CREATE TABLE notification_fingerprints (
    chat_id     INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (chat_id, fingerprint)
);

INSERT INTO notification_fingerprints (chat_id, fingerprint)
SELECT chat_id, fingerprint FROM notifications;

-- Enforce the same rule for poller fanout, direct test inserts, and any future
-- producers. Both the identity and payload are recorded in one transaction.
CREATE TRIGGER notifications_deduplicate BEFORE INSERT ON notifications
WHEN EXISTS (
    SELECT 1 FROM notification_fingerprints
    WHERE chat_id = NEW.chat_id AND fingerprint = NEW.fingerprint
)
BEGIN
    SELECT RAISE(IGNORE);
END;

CREATE TRIGGER notifications_remember AFTER INSERT ON notifications
BEGIN
    INSERT INTO notification_fingerprints (chat_id, fingerprint)
    VALUES (NEW.chat_id, NEW.fingerprint);
END;
