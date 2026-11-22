-- Personal access tokens: how a person's own agents (Claude Code, Codex, scripts) reach the
-- MCP server and the REST API as them (internal/auth, docs/api.md#access-tokens).
--
-- A token belongs to one user in one workspace and acts with exactly that user's access
-- there, checked again on every call. Like sessions, only the token's SHA-256 is stored;
-- the token itself is shown once, when it is made. Revoking one deletes its row.

CREATE TABLE access_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- What the user called it, like "laptop: Claude Code". Changes made with the token
    -- are marked with it.
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- NULL: the token doesn't expire.
    expires_at   TEXT,
    -- Updated at most once a minute.
    last_used_at TEXT
) STRICT;

CREATE INDEX access_tokens_by_member ON access_tokens (workspace_id, user_id);

-- Leaving a workspace ends its tokens, so they don't come back if the user rejoins. The
-- server checks the membership on every call as well.
CREATE TRIGGER memberships_drop_access_tokens
AFTER DELETE ON memberships
BEGIN
    DELETE FROM access_tokens WHERE workspace_id = OLD.workspace_id AND user_id = OLD.user_id;
END;

-- Board changes made with a token: the name of the token, besides the user (and the agent
-- flag, which a token's changes also set). NULL for changes made otherwise. A name, not a
-- reference: it stays after the token is revoked.
ALTER TABLE cards ADD COLUMN created_via TEXT;
ALTER TABLE cards ADD COLUMN updated_via TEXT;
ALTER TABLE card_comments ADD COLUMN via TEXT;
