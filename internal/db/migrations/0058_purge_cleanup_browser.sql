-- A purge's step for the person's agent browsers: they are stopped (and their live views closed)
-- and their profile removed before the CLI worker's directory, which holds it, goes. Like the
-- others it is recorded before it is done and removed when it is done, so a worker that doesn't
-- answer leaves it for the next try.
--
-- The kind's CHECK can't be altered, so the table is rebuilt with 'browser' among the kinds. Its
-- rows are kept as they are, with the numbers they have (the steps are done in the order of their
-- ids), and the sequence is moved to the new table.
CREATE TABLE purge_cleanups_new (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL,
    -- agent_conversation: target is the agent runtime's conversation id;
    -- browser: the user's id, for their agent browsers' processes and profile;
    -- cli_home: the user's id, for the CLI worker's directory;
    -- providers: 'feed:' and the user's id, for the providers file and the key files, one row
    -- for each account so that no run's acknowledgement clears another account's.
    kind       TEXT NOT NULL CHECK (kind IN ('agent_conversation', 'browser', 'cli_home', 'providers')),
    target     TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    attempts   INTEGER NOT NULL DEFAULT 0,
    -- Why the last try failed, for the operator; never holds a secret.
    last_error TEXT,
    UNIQUE (kind, target)
) STRICT;

INSERT INTO purge_cleanups_new (id, user_id, kind, target, created_at, attempts, last_error)
    SELECT id, user_id, kind, target, created_at, attempts, last_error FROM purge_cleanups;

DELETE FROM sqlite_sequence WHERE name = 'purge_cleanups_new';
UPDATE sqlite_sequence SET name = 'purge_cleanups_new' WHERE name = 'purge_cleanups';

DROP TABLE purge_cleanups;
ALTER TABLE purge_cleanups_new RENAME TO purge_cleanups;

-- A purge's CLI home step recorded before this migration has no browser step: the account's
-- browser may still run in the worker, and its profile is in the directory the step removes. Each
-- gets the step now, and it comes first.
INSERT INTO purge_cleanups (user_id, kind, target, created_at)
    SELECT user_id, 'browser', target, created_at FROM purge_cleanups WHERE kind = 'cli_home'
    ON CONFLICT (kind, target) DO NOTHING;
