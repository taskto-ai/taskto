-- The system writer for the records of personal apps (internal/apps): the one path that
-- changes a record with no user behind it, for the server's own work (writing back a link,
-- applying an event from an integration), bounded by the fields an app's contract declares.
--
-- A user's id can't name it: app_rows.updated_by and app_row_history.actor are user ids with
-- ON DELETE SET NULL, so NULL already means "the account was deleted". The system actor gets
-- columns of its own, '<kind>:<id>' (today 'integration:<id>'), and the user columns stay NULL
-- beside it. created_by is never touched by a system write, so who may read their own records
-- doesn't change.

ALTER TABLE app_rows ADD COLUMN updated_system TEXT
    CHECK (updated_system IS NULL OR updated_system GLOB 'integration:[0-9]*');

ALTER TABLE app_row_history ADD COLUMN actor_system TEXT
    CHECK (actor_system IS NULL OR (actor_system GLOB 'integration:[0-9]*' AND actor IS NULL));

-- The immutability trigger names the columns it protects, so a column added later is
-- unprotected until the trigger lists it: recreated, with actor_system.
DROP TRIGGER app_row_history_immutable;

-- The one change allowed is the actor's account going (actor ON DELETE SET NULL).
CREATE TRIGGER app_row_history_immutable
BEFORE UPDATE OF id, app_id, row_id, revision, operation, data, at, actor_agent, actor_via, actor_system, action, synthetic
ON app_row_history
BEGIN
    SELECT RAISE(ABORT, 'a row''s history is never rewritten');
END;
