-- People, workspaces and who belongs where. A solo install is a workspace with one
-- member; a team is the same workspace with more. Times are UTC text in the format of
-- strftime('%Y-%m-%dT%H:%M:%fZ'), so they compare correctly as strings.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    -- The server lower-cases addresses; NOCASE keeps uniqueness case-insensitive anyway.
    email         TEXT NOT NULL COLLATE NOCASE UNIQUE,
    name          TEXT NOT NULL,
    -- An argon2id hash in the PHC string format, with its parameters.
    password_hash TEXT NOT NULL,
    -- Room for two-factor login: the TOTP secret once a user turns it on. Unused yet.
    totp_secret   TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE workspaces (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE memberships (
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, user_id)
) STRICT;

CREATE INDEX memberships_by_user ON memberships (user_id);

-- A login. The browser holds the token; the database only its SHA-256.
CREATE TABLE sessions (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT NOT NULL
) STRICT;

CREATE INDEX sessions_by_user ON sessions (user_id);
CREATE INDEX sessions_by_expiry ON sessions (expires_at);

-- An invitation to join a workspace, for one email address. Single use: accepted_at is
-- set when it is used. Like sessions, only the token's SHA-256 is stored.
CREATE TABLE invites (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    email        TEXT NOT NULL COLLATE NOCASE,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    created_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at   TEXT NOT NULL,
    accepted_at  TEXT,
    accepted_by  INTEGER REFERENCES users (id) ON DELETE SET NULL
) STRICT;

CREATE INDEX invites_by_workspace ON invites (workspace_id);
