-- Which CLI worker has kept users' files on this install (docs/cli-workers.md,
-- docs/install.md#privacy-and-account-deletion).
--
-- The purge of a deleted account removes the person's CLI files (internal/accountdeletion). The
-- files the local worker keeps are under the data directory, which the server can always remove.
-- A container worker keeps its own copy in its own volume, out of the server's reach unless the
-- server runs in container mode, so the purge has to wait for that mode when such files may exist,
-- and has nothing to wait for when they never did. The server records here, at every start in
-- container mode, that the install has run one ('container'). A database that had users when this
-- migration ran may have run one before there was a record: it is marked 'unknown', which the purge
-- treats as a container worker that may hold files, so nothing is ever closed on a guess.
CREATE TABLE cli_worker_history (
    worker   TEXT PRIMARY KEY CHECK (worker IN ('container', 'unknown')),
    first_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- 'Had users' is any trace of one: an account (user_ids keeps the highest id ever given, so it shows
-- an account that was purged too) or a purge step still recorded for one.
INSERT INTO cli_worker_history (worker)
    SELECT 'unknown' WHERE EXISTS (SELECT 1 FROM users)
        OR coalesce((SELECT high FROM user_ids WHERE id = 1), 0) > 0
        OR EXISTS (SELECT 1 FROM purge_cleanups);
