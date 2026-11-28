-- The schedules a live release of an app declares, one row each: who they run as (the person
-- who published or rolled back to the release that declares them), the calendar they were
-- planned from, and how the last run ended. What a run inserts lives in the release, never here.
-- Without a runner (the account is gone) a schedule isn't planned.
CREATE TABLE app_schedules (
    app_id      INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    runner_id   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    calendar    TEXT NOT NULL CHECK (json_valid(calendar)),
    last_run_at TEXT,
    last_error  TEXT CHECK (last_error IN ('access', 'invalid', 'full', 'limit')),
    PRIMARY KEY (app_id, name)
) STRICT;

-- A schedule that goes (its release no longer declares it, or its app is removed with its space)
-- takes its job with it, so a job never outlives what it was planned from and can't reach an app
-- that later has the same ID.
CREATE TRIGGER app_schedules_drop_job
AFTER DELETE ON app_schedules
BEGIN
    DELETE FROM scheduled_jobs
    WHERE kind = 'app.scheduled-action' AND dedupe_key = 'app:' || OLD.app_id || ':' || OLD.name;
END;
