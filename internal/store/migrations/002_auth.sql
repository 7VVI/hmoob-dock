-- Authentication schema of the control端.
--
-- Every timestamp is stored as UTC milliseconds in an INTEGER column and
-- every credential is stored as an irreversible digest. The plain password,
-- bootstrap credential and session token never reach the database.

-- The control端 has exactly one administrator. The check on the primary key
-- keeps a second row out of the table.
CREATE TABLE IF NOT EXISTS administrator (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- The one time credential that opens the first initialization. It is
-- consumed by the administrator that uses it.
CREATE TABLE IF NOT EXISTS bootstrap_tokens (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);

-- The sessions of the administrator. The session token and the CSRF token
-- are stored as digests, so a stolen database does not hand out sessions.
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    csrf_hash  TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);
