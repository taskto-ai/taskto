-- Workspace-shared CLI accounts (docs/account-switching.md): an API key a workspace's
-- admins add for a CLI, which its members' turns use through the server's proxy. Never a
-- subscription login: the providers' terms forbid sharing one. The key is sealed and never
-- leaves the server: the CLI gets a token for its turn, and the proxy adds the key.
CREATE TABLE cli_shared_accounts (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    cli          TEXT NOT NULL CHECK (cli IN ('claude', 'codex', 'grok')),
    label        TEXT NOT NULL CHECK (length(label) BETWEEN 1 AND 60),
    -- The provider's API root, before /v1 (https://api.anthropic.com), or a gateway's.
    base_url     TEXT NOT NULL,
    -- The model the CLI asks for, whatever the conversation chose; empty: the conversation's.
    model        TEXT NOT NULL DEFAULT '',
    -- The API key sealed with the server key (internal/serverkey), bound to this row's ID.
    -- NULL only between the insert and the update that seals it, in one transaction.
    api_key      BLOB,
    -- The newest limits the proxy saw (a 429 and when it resets), a JSON array, and when.
    limits       TEXT,
    limits_at    TEXT,
    created_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX cli_shared_accounts_by_workspace ON cli_shared_accounts (workspace_id);

-- A member's use of a shared account is an account directory of their own on the worker
-- (their sessions there): a cli_accounts row with no slot, pointing at the shared account.
ALTER TABLE cli_accounts ADD COLUMN shared_account_id INTEGER REFERENCES cli_shared_accounts (id) ON DELETE CASCADE;
CREATE UNIQUE INDEX cli_accounts_shared ON cli_accounts (user_id, shared_account_id);

-- Which accounts a member's turns in a workspace may use: own_else_shared (their own if one
-- is connected, else the workspace's), own, or shared. Admins set the workspace's default and
-- each member's; a member without a row has the default.
CREATE TABLE cli_workspace_modes (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    default_mode TEXT NOT NULL CHECK (default_mode IN ('own_else_shared', 'own', 'shared'))
) STRICT;

CREATE TABLE cli_member_modes (
    workspace_id INTEGER NOT NULL,
    user_id      INTEGER NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('own_else_shared', 'own', 'shared')),
    PRIMARY KEY (workspace_id, user_id),
    FOREIGN KEY (workspace_id, user_id) REFERENCES memberships (workspace_id, user_id) ON DELETE CASCADE
) STRICT;
