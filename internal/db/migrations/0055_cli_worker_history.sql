-- Which CLI workers have run on this data directory (docs/cli-workers.md), so the purge of a
-- deleted account (internal/accountdeletion) knows where the person's CLI files may be: the local
-- worker keeps them under the data directory (<data>/cli/u<id>), the container worker in a volume
-- of its own, which only that worker can reach.
--
-- The server adds its kind of worker when it starts with CLI workers on, before it serves
-- (local for local and local-unsafe, container), and a row stays. A purge then waits for each
-- kind that ran to remove the person's files, and records no such step for a kind that never ran:
-- an install that never had CLI workers has nothing to clean.
--
-- unknown: a worker of either kind may have run before this record was kept, for any account
-- that existed then (users.created_at at or before since). This migration adds it to a database
-- that has ever had an account; a restore moves it to its own time, since a backup's record says
-- nothing of what ran after it was made.
CREATE TABLE cli_worker_history (
    worker TEXT PRIMARY KEY CHECK (worker IN ('local', 'container', 'unknown')),
    since  TEXT NOT NULL
) STRICT;

-- Not before any account was made, whatever the clock says.
INSERT INTO cli_worker_history (worker, since)
SELECT 'unknown', max(strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), coalesce((SELECT max(created_at) FROM users), ''))
WHERE EXISTS (SELECT 1 FROM users) OR (SELECT high FROM user_ids WHERE id = 1) > 0;

-- A purge's CLI step names the worker it waits for: 'local:' or 'container:' and the user's id. A
-- step recorded before (the user's id alone) comes from an install whose history isn't known, so
-- it becomes both.
INSERT INTO purge_cleanups (user_id, kind, target, created_at, attempts, last_error)
SELECT c.user_id, c.kind, w.prefix || c.target, c.created_at, c.attempts, c.last_error
FROM purge_cleanups c, (SELECT 'local:' AS prefix UNION ALL SELECT 'container:') w
WHERE c.kind = 'cli_home' AND c.target NOT LIKE '%:%'
ON CONFLICT (kind, target) DO NOTHING;

DELETE FROM purge_cleanups WHERE kind = 'cli_home' AND target NOT LIKE '%:%';
