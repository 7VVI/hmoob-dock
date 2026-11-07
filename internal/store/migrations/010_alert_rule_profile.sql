-- The profile of an alert rule: the name the administrator reads and the
-- severity the rule carries.
--
-- The existing rules keep an empty name: the frontend falls back to the
-- label of the kind when a rule has no name of its own. The severity of an
-- old rule is a warning, so an old rule reads like it always did.

ALTER TABLE alert_rules ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_rules ADD COLUMN severity TEXT NOT NULL DEFAULT 'warning';
