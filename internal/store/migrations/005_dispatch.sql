-- Schema of the dispatch of the deployments.
--
-- The tasks of a deployment keep the state of their dispatch: the node that
-- executes the task, the time the command was handed to it and the outcome
-- the node reported. The columns of the manual resolution keep the request
-- of the administrator, so the control端 and the node can be compared while
-- the resolution waits for its confirmation.

ALTER TABLE tasks ADD COLUMN dispatch_started_at INTEGER;
ALTER TABLE tasks ADD COLUMN result_code TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN result_message TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN rolled_back INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN partial_update INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN manual_resolve_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN manual_resolve_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN manual_resolve_pending INTEGER NOT NULL DEFAULT 0;

-- The audit events carry the detail of the action. The authentication does
-- not use it, so the column holds an empty string there.
ALTER TABLE audit_events ADD COLUMN detail TEXT NOT NULL DEFAULT '';

-- The index of the tasks that wait for the confirmation of a node. The
-- recovery of the control端 and the connection of a node use it to find the
-- tasks that have to be compared with a ledger.
CREATE INDEX IF NOT EXISTS tasks_of_agent
ON tasks(agent_id, state);
