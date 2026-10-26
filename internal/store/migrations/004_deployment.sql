-- Schema of the applications and of their deployments.
--
-- Every timestamp is stored as UTC milliseconds in an INTEGER column. The
-- configuration of a version is encrypted with the master key of the
-- control端, so a stolen database hands out neither the document nor the
-- variables of an application.

-- The applications of the control端. The target and the namespace of an
-- application never change: the namespace is the name of the Compose
-- project and of the Swarm stack, so a second application on the same
-- target may not use it.
CREATE TABLE IF NOT EXISTS apps (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    mode                  TEXT NOT NULL,
    target_id             TEXT NOT NULL,
    namespace             TEXT NOT NULL,
    latest_version_id     TEXT NOT NULL DEFAULT '',
    successful_version_id TEXT NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS one_app_per_namespace
ON apps(target_id, namespace);

-- The versions of an application. A version never changes: it holds the
-- document and the variables of a deployment as they were submitted,
-- encrypted with the application and the version as the additional data.
CREATE TABLE IF NOT EXISTS versions (
    id                   TEXT PRIMARY KEY,
    app_id               TEXT NOT NULL REFERENCES apps(id),
    digest               TEXT NOT NULL,
    yaml_ciphertext      BLOB NOT NULL,
    variables_ciphertext BLOB NOT NULL,
    created_at           INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS versions_of_app
ON versions(app_id, created_at, id);

-- The deployment tasks of an application. The idempotency key of a task is
-- unique per application, so a repeated request answers with the task of
-- the first request instead of a second one.
CREATE TABLE IF NOT EXISTS tasks (
    id                 TEXT PRIMARY KEY,
    app_id             TEXT NOT NULL REFERENCES apps(id),
    version_id         TEXT NOT NULL REFERENCES versions(id),
    agent_id           TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL,
    last_seq           INTEGER NOT NULL DEFAULT 0,
    digest             TEXT NOT NULL,
    idempotency_key    TEXT NOT NULL,
    request_digest     TEXT NOT NULL,
    base_version_id    TEXT NOT NULL DEFAULT '',
    preview_digest     TEXT NOT NULL DEFAULT '',
    inventory_revision TEXT NOT NULL DEFAULT '',
    removed_services   TEXT NOT NULL DEFAULT '[]',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    finished_at        INTEGER
);

-- An application has at most one task that is not finished. The index is
-- partial, so the finished tasks do not block a new deployment. A task
-- whose result is uncertain blocks the application as well: nobody may
-- prove that the target is untouched.
CREATE UNIQUE INDEX IF NOT EXISTS one_open_task_per_app
ON tasks(app_id)
WHERE state IN ('queued','preflight','running','verifying','uncertain');

CREATE UNIQUE INDEX IF NOT EXISTS one_idempotency_key_per_app
ON tasks(app_id, idempotency_key);

-- The output of a task. The rows hold the fragments in the order of their
-- offset, so a reader skips the fragments it has already seen.
CREATE TABLE IF NOT EXISTS task_output (
    task_id    TEXT NOT NULL REFERENCES tasks(id),
    offset     INTEGER NOT NULL,
    data       BLOB NOT NULL,
    truncated  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (task_id, offset)
);
