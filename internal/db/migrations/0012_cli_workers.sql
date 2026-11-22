-- CLI workers (docs/cli-workers.md): the official Claude Code, Codex and Grok Build CLIs,
-- signed in with each user's own subscription.
--
-- A CLI account is one user's login to one CLI. The login itself (the CLI's credentials)
-- never comes here: the CLI keeps it in the user's own directory on the worker. This table
-- keeps what the server shows and decides by: whether the login works, and the account's
-- last usage limits. No row means not connected.
CREATE TABLE cli_accounts (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cli         TEXT NOT NULL CHECK (cli IN ('claude', 'codex', 'grok')),
    -- connected: the CLI said it is signed in; reconnect: a turn or a check found the login
    -- expired or revoked, and the user has to sign in again.
    state       TEXT NOT NULL CHECK (state IN ('connected', 'reconnect')),
    -- How the CLI says it is signed in (claude.ai, chatgpt, oauth, …).
    auth_method TEXT NOT NULL DEFAULT '',
    -- The newest usage limits the CLI reported, a JSON array, and when (NULL: none yet).
    limits      TEXT,
    limits_at   TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, cli)
) STRICT;

-- The CLI session a conversation runs in, per account: recorded before the CLI starts
-- (Claude Code, Grok Build) or from its first event (Codex), so a restart resumes it.
-- A session lives in one account's config directory, so changing CLI or account starts
-- another one; disconnecting the account deletes its sessions.
CREATE TABLE cli_sessions (
    conversation_id INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    account_id      INTEGER NOT NULL REFERENCES cli_accounts (id) ON DELETE CASCADE,
    session_id      TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (conversation_id, account_id)
) STRICT;
