-- Schema of the alerts of the control端.
--
-- A rule names what it watches: a kind, the object it watches, the metric of
-- a resource rule and the threshold. The condition of one rule and one
-- object holds the continuity of the observation: the moment an anomaly
-- began and the moment a recovery began. The events are the history, and the
-- outbox holds one notification per transition, so a restart of the control
-- does not lose one and a repeated observation does not send two.

CREATE TABLE IF NOT EXISTS alert_rules (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    scope_id TEXT NOT NULL,
    metric TEXT NOT NULL DEFAULT '',
    threshold REAL NOT NULL DEFAULT 0,
    for_ms INTEGER NOT NULL DEFAULT 0,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- One row per rule and object: the continuity of the observation.
--
-- pending_since is the first sample above the threshold that is not yet an
-- event, recovering_since is the first sample below it that does not yet
-- close an event. A gap of the observation clears both: the continuity has
-- to be proven again by fresh samples.
CREATE TABLE IF NOT EXISTS alert_conditions (
    rule_id TEXT NOT NULL,
    object_id TEXT NOT NULL,
    pending_since INTEGER NOT NULL DEFAULT 0,
    recovering_since INTEGER NOT NULL DEFAULT 0,
    last_observed_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (rule_id, object_id)
);

-- The events are the history of the alerts.
--
-- transition_seq counts the state changes of one event: one notification is
-- sent per transition, so the recovery of an event is never merged with its
-- opening.
CREATE TABLE IF NOT EXISTS alert_events (
    id TEXT PRIMARY KEY,
    rule_id TEXT NOT NULL,
    object_id TEXT NOT NULL,
    state TEXT NOT NULL,
    summary TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    last_observed_at INTEGER NOT NULL,
    resolved_at INTEGER,
    acknowledged_at INTEGER,
    transition_seq INTEGER NOT NULL DEFAULT 1
);

-- An object holds at most one active event per rule. A repeated sample of an
-- active anomaly updates the event, it never opens a second one.
CREATE UNIQUE INDEX IF NOT EXISTS one_active_alert
ON alert_events(rule_id, object_id)
WHERE state = 'active';

CREATE TABLE IF NOT EXISTS notification_outbox (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL,
    transition_seq INTEGER NOT NULL,
    state TEXT NOT NULL,
    occurred_at INTEGER NOT NULL,
    summary TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT '',
    object TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    delivered_at INTEGER,
    last_error TEXT NOT NULL DEFAULT ''
);

-- One notification per transition of one event. A repeated observation that
-- does not change the state inserts nothing.
CREATE UNIQUE INDEX IF NOT EXISTS one_notification_per_transition
ON notification_outbox(event_id, transition_seq);

CREATE INDEX IF NOT EXISTS alerts_of_an_object
ON alert_events(rule_id, object_id, started_at);
