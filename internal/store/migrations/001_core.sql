-- Core schema of the control端.
--
-- Every timestamp is stored as UTC milliseconds in an INTEGER column.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY
);

CREATE TABLE audit_events (
    id          TEXT PRIMARY KEY,
    occurred_at INTEGER NOT NULL,
    action      TEXT NOT NULL,
    target_id   TEXT NOT NULL,
    result      TEXT NOT NULL
);
