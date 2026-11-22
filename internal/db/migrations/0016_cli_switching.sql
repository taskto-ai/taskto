-- Automatic switching (docs/account-switching.md): when the account a turn runs on stops
-- (a usage limit, an expired login), the turn goes on with the next account of the user's
-- queue that can run. Off until the user turns it on.
CREATE TABLE cli_switching (
    user_id INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1))
) STRICT;

-- The user's queue, in order: their own accounts (cli_accounts rows with a slot) and shared
-- accounts of workspaces they are a member of. An account that goes leaves the queue with it.
CREATE TABLE cli_switch_queue (
    user_id           INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    account_id        INTEGER REFERENCES cli_accounts (id) ON DELETE CASCADE,
    shared_account_id INTEGER REFERENCES cli_shared_accounts (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, position),
    CHECK ((account_id IS NULL) <> (shared_account_id IS NULL))
) STRICT;

CREATE UNIQUE INDEX cli_switch_queue_account ON cli_switch_queue (user_id, account_id);
CREATE UNIQUE INDEX cli_switch_queue_shared ON cli_switch_queue (user_id, shared_account_id);
