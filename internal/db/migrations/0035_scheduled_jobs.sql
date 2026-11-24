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
    -- When it is due: UTC, millisecond text in exactly the form strftime('%Y-%m-%dT%H:%M:%fZ')
    -- writes, so times compare as strings, and within the years the scheduler supports (2000 to
    -- 8999). The round trip through strftime rejects any other text, so a time that sorts wrongly
    -- (year 10000 sorts before today) or doesn't parse can't be stored.
    run_at       TEXT NOT NULL CHECK (
        strftime('%Y-%m-%dT%H:%M:%fZ', run_at) IS run_at AND run_at >= '2000-01-01T00:00:00.000Z' AND run_at < '9000-01-01T00:00:00.000Z'),
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
    retry_at     TEXT CHECK (retry_at IS NULL OR (strftime('%Y-%m-%dT%H:%M:%fZ', retry_at) IS retry_at
        AND retry_at >= '2000-01-01T00:00:00.000Z' AND retry_at < '9000-01-01T00:00:00.000Z')),
    -- The token of the claim that holds the job: raised on every claim and on every reschedule and
    -- never reset, so a worker whose lease ran out (or whose job was changed meanwhile) can never
    -- finish it for a later claim.
    lease        INTEGER NOT NULL DEFAULT 0 CHECK (lease >= 0),
    lease_until  TEXT CHECK (lease_until IS NULL OR (strftime('%Y-%m-%dT%H:%M:%fZ', lease_until) IS lease_until
        AND lease_until >= '2000-01-01T00:00:00.000Z' AND lease_until < '9000-01-01T00:00:00.000Z')),
    -- Raised on every reschedule and never otherwise, so it is the same on every try of an occurrence
    -- and differs between a schedule and the one that replaced it, even at the same run_at: with the
    -- id and run_at it names what a handler makes only once.
    revision     INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    -- A short reason the last try failed, for the people who run the server; never a secret.
    last_error   TEXT,
    last_run_at  TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    done_at      TEXT,
    UNIQUE (user_id, kind, dedupe_key)
) STRICT;

-- The worker's questions, each answered by a partial index in the order it wants, so a poll reads
-- the first row of each and never scans the queue or sorts it: the pending job that is due first
-- (by its next try, else its run_at), and the running job whose lease ended first. A job that is
-- done or failed is in neither, however many there are; the third is what the purge walks.
CREATE INDEX scheduled_jobs_pending_due ON scheduled_jobs (coalesce(retry_at, run_at), id) WHERE state = 'pending';
CREATE INDEX scheduled_jobs_lease_ended ON scheduled_jobs (lease_until, id) WHERE state = 'running';
CREATE INDEX scheduled_jobs_finished ON scheduled_jobs (updated_at) WHERE state IN ('done', 'failed');
-- The per-person bound counts the jobs that wait or run.
CREATE INDEX scheduled_jobs_active_by_user ON scheduled_jobs (user_id) WHERE state IN ('pending', 'running');
CREATE INDEX scheduled_jobs_by_workspace ON scheduled_jobs (workspace_id);
CREATE INDEX scheduled_jobs_by_space ON scheduled_jobs (space_id) WHERE space_id IS NOT NULL;
