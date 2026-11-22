-- Conversations with the agent. The agent runtime (Pi Durable in the sidecar) keeps their
-- content and knows nothing about users; this table says whose each one is. A conversation
-- is private to its owner, in one workspace, and only reachable while the owner is a member
-- there (internal/conversations).
CREATE TABLE conversations (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- Like a private space, a conversation outlives its owner's membership (it comes back if
    -- they rejoin), so deleting an account has to decide what becomes of it first.
    owner_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- The agent runtime's conversation ID. Never shown by the API.
    agent_id     INTEGER NOT NULL UNIQUE,
    title        TEXT NOT NULL DEFAULT '',
    -- Set when the owner archives it: hidden from the default list, readable, not writable.
    archived_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- The last rename, archive or message.
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX conversations_by_owner ON conversations (workspace_id, owner_id, updated_at);

-- A conversation never moves between workspaces, owners or agent runtime conversations.
CREATE TRIGGER conversations_fixed_identity
BEFORE UPDATE OF workspace_id, owner_id, agent_id ON conversations
BEGIN
    SELECT RAISE(ABORT, 'a conversation''s workspace, owner and agent conversation cannot change');
END;
