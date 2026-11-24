-- The general scheduler (internal/schedule): timed work that runs for a person, later.
--
-- A job is a kind (from the closed list of handlers the server's Go code registers), the person it
-- runs for, the workspace (and, where it is about one, the space) it is in, a small JSON payload of
-- IDs and settings (never a secret, never a copy of what people wrote: the handler reads what it
-- needs when it runs, as the owner, with the owner's access then), and when it is due.
--
-- The worker in the server process takes a due job with a lease and runs it at least once: a worker
-- that dies leaves the job running until its lease ends, and the job is then taken again. A job's
-- handler is therefore idempotent, and may use the job's id and run_at (the occurrence) as the key
-- of what it makes.
CREATE TABLE scheduled_jobs (
    -- AUTOINCREMENT: an id is never given again, so an id and a run_at name one occurrence for good.
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
    -- Whose job it is: it runs as them, with their access at the time it runs.
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    space_id     INTEGER REFERENCES spaces (id) ON DELETE CASCADE,
    -- Names what the job is for (a card's reminder, an integration's poll), so scheduling it again
    -- changes nothing and a changed schedule replaces it. Unique per owner and kind.
    dedupe_key   TEXT NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 200),
    payload      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    -- When it is due (UTC, millisecond text, so times compare as strings).
    run_at       TEXT NOT NULL,
    -- NULL for a job that runs once; else the seconds between the occurrences of a recurring one.
    every_seconds INTEGER CHECK (every_seconds IS NULL OR every_seconds >= 60),
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'done', 'failed')),
    -- The tries of the occurrence that is due, counted when taken, so a job that kills its worker
    -- every time is still bounded.
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- Occurrences of a recurring job in a row that used all their tries.
    failed_runs  INTEGER NOT NULL DEFAULT 0 CHECK (failed_runs >= 0),
    -- When the next try is, after a failed one (NULL: at run_at). run_at itself names the occurrence
    -- and stays as it was through its retries.
    retry_at     TEXT,
    -- The token of the claim that holds the job: raised on every claim and on every reschedule and
    -- never reset, so a worker whose lease ran out (or whose job was changed meanwhile) can never
    -- finish it for a later claim.
    lease        INTEGER NOT NULL DEFAULT 0 CHECK (lease >= 0),
    lease_until  TEXT,
    -- A short reason the last try failed, for the people who run the server; never a secret.
    last_error   TEXT,
    last_run_at  TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    done_at      TEXT,
    UNIQUE (user_id, kind, dedupe_key)
) STRICT;

-- The worker's question: what is due, or running with a lease that has ended.
CREATE INDEX scheduled_jobs_due ON scheduled_jobs (state, run_at);
CREATE INDEX scheduled_jobs_by_workspace ON scheduled_jobs (workspace_id);
CREATE INDEX scheduled_jobs_by_space ON scheduled_jobs (space_id) WHERE space_id IS NOT NULL;
