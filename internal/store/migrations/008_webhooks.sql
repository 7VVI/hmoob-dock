-- Schema of the notification targets of the control端.
--
-- The address of a webhook carries a secret in its path or in its query,
-- so it is stored encrypted and only its masked form is ever returned.
-- The allowed networks are the private networks the administrator allows
-- explicitly: a target of a private network is never used without them.
--
-- The delivery state of a notification lives in the outbox of the alerts:
-- pending waits for its delivery, delivered reached its target, failed
-- gave up after the last try and skipped was never queued because the
-- queue was full. The lease holds a notification that a worker claimed,
-- so a worker that died does not block it forever.

CREATE TABLE IF NOT EXISTS webhook_targets (
    id TEXT PRIMARY KEY,
    url_ciphertext BLOB NOT NULL,
    url_masked TEXT NOT NULL DEFAULT '',
    allowed_private_cidrs TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

ALTER TABLE notification_outbox ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'pending';

ALTER TABLE notification_outbox ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0;

-- The worker reads the notifications that wait for their delivery in the
-- order of their next attempt.
CREATE INDEX IF NOT EXISTS notifications_to_deliver
ON notification_outbox(delivery_state, next_attempt_at);
