-- Several accounts per CLI (docs/account-switching.md): a user may connect more than one
-- login to a CLI, each in a config directory of its own on the worker. The slot names it: 1 is
-- the first (config/<cli>, as before), 2 to 9 the others (config/<cli>-<n>). NULL is kept for
-- an account directory that isn't a login of the user's own (a workspace's shared account).
--
-- SQLite can't change a UNIQUE constraint in place, so both tables are rebuilt. cli_sessions
-- refers to cli_accounts, so its rows are set aside first: dropping cli_accounts would
-- otherwise delete them (ON DELETE CASCADE).
CREATE TEMP TABLE cli_sessions_kept AS SELECT conversation_id, account_id, session_id, created_at FROM cli_sessions;
DROP TABLE cli_sessions;

CREATE TABLE cli_accounts_new (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cli         TEXT NOT NULL CHECK (cli IN ('claude', 'codex', 'grok')),
    slot        INTEGER CHECK (slot BETWEEN 1 AND 9),
    -- The user's name for it ("Work", "Personal"); empty: none.
    label       TEXT NOT NULL DEFAULT '' CHECK (length(label) <= 40),
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
    UNIQUE (user_id, cli, slot)
) STRICT;

INSERT INTO cli_accounts_new (id, user_id, cli, slot, state, auth_method, limits, limits_at, created_at, updated_at)
    SELECT id, user_id, cli, 1, state, auth_method, limits, limits_at, created_at, updated_at FROM cli_accounts;
DROP TABLE cli_accounts;
ALTER TABLE cli_accounts_new RENAME TO cli_accounts;

-- The CLI session a conversation runs in, per account, and which the conversation used last: a
-- turn on an account other than the conversation's last starts a new session with a hand-off,
-- even where the account has an older one, which doesn't know what happened since.
CREATE TABLE cli_sessions (
    conversation_id INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    account_id      INTEGER NOT NULL REFERENCES cli_accounts (id) ON DELETE CASCADE,
    session_id      TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Counts up per conversation each time a session is used: the highest is the last. A
    -- counter, not a time, so two uses in the same instant still have an order.
    used            INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (conversation_id, account_id)
) STRICT;

-- Before, a conversation had a session per CLI and used the one of its model's: no order to keep.
INSERT INTO cli_sessions (conversation_id, account_id, session_id, created_at)
    SELECT conversation_id, account_id, session_id, created_at FROM cli_sessions_kept;
DROP TABLE cli_sessions_kept;
