-- Model provider accounts: an endpoint, its models and, optionally, an API key. An account
-- belongs to a workspace (its admins manage it, its members use it) or to a user (personal,
-- usable only in that user's conversations). The server writes them into the agent
-- runtime's providers file (internal/providers).
CREATE TABLE provider_accounts (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      INTEGER REFERENCES users (id) ON DELETE CASCADE,
    -- Part of the agent runtime's provider ID (ws<workspace>-<name> or u<user>-<name>), so it
    -- is a slug and never changes.
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
    display_name TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('openai-compatible')),
    base_url     TEXT NOT NULL,
    -- The API key sealed with the server key (internal/serverkey), bound to this row's ID;
    -- NULL for a server without auth. Never returned by the API.
    api_key      BLOB,
    -- pi-ai's compatibility settings as a JSON object, or NULL.
    compat       TEXT,
    -- The models as a JSON array of {id, name, context_window?, max_tokens?, reasoning}.
    models       TEXT NOT NULL,
    created_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((workspace_id IS NULL) <> (user_id IS NULL))
) STRICT;

-- One name per owner. Rows of the other kind have NULL here, and NULLs never collide.
CREATE UNIQUE INDEX provider_accounts_workspace_name ON provider_accounts (workspace_id, name);
CREATE UNIQUE INDEX provider_accounts_user_name ON provider_accounts (user_id, name);
