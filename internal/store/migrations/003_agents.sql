-- Schema of the agents of the control端.
--
-- Every timestamp is stored as UTC milliseconds in an INTEGER column. The
-- enrollment token and the credential of a node are stored as irreversible
-- digests, so a stolen database hands out neither of them.

-- The one time token that opens the enrollment of a node. It stays valid
-- for a short time and is consumed by the first enrollment that uses it.
CREATE TABLE IF NOT EXISTS agent_tokens (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);

-- The nodes that are enrolled at the control端. The engine ID of the Docker
-- engine anchors the identity of a node: it is unique, so a second
-- enrollment of the same engine does not replace the node that exists
-- already.
CREATE TABLE IF NOT EXISTS nodes (
    id              TEXT PRIMARY KEY,
    engine_id       TEXT NOT NULL UNIQUE,
    swarm_id        TEXT NOT NULL DEFAULT '',
    swarm_node_id   TEXT NOT NULL DEFAULT '',
    role            TEXT NOT NULL DEFAULT '',
    hostname        TEXT NOT NULL DEFAULT '',
    credential_hash TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    revoked_at      INTEGER
);
