-- The scope and the condition of an alert rule.
--
-- description is the sentence the administrator writes for the rule. The
-- scope names the breadth the rule watches: one container, every container
-- or the containers of one namespace. The operator compares the observed
-- value with the threshold, so a rule may also fire below a limit. The
-- recover notification can be quiet, because not every rule has to speak
-- twice.

ALTER TABLE alert_rules ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE alert_rules ADD COLUMN scope_kind TEXT NOT NULL DEFAULT 'container';
ALTER TABLE alert_rules ADD COLUMN operator TEXT NOT NULL DEFAULT '>';
ALTER TABLE alert_rules ADD COLUMN recover_notify INTEGER NOT NULL DEFAULT 1;
