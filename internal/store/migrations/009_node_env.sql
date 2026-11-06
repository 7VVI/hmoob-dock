-- The environment of a node as its agent last reported it.
--
-- The identity of an engine carries the facts the page shows on the detail
-- of a node: the version of the engine, the type of its operating system
-- and the architecture of its host. They live in the record of the node, so
-- a node that is away still shows the environment of its last report. The
-- columns are nullable: a node that was enrolled before the platform asked
-- for these facts reports them with its next inventory.
--
-- The version of the agent is informational. It travels in the hello of a
-- connection and is stored once per report, so the page can show an agent
-- that runs an older build.

ALTER TABLE nodes ADD COLUMN docker_version TEXT NOT NULL DEFAULT '';

ALTER TABLE nodes ADD COLUMN os_type TEXT NOT NULL DEFAULT '';

ALTER TABLE nodes ADD COLUMN os_arch TEXT NOT NULL DEFAULT '';

ALTER TABLE nodes ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';
