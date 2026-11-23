-- Record revisions, history and recoverable deletion for the rows of personal apps
-- (internal/apps).
--
-- Every row carries a revision: 1 when it is created, bumped by each change, so an update or a
-- delete can say which state it saw and a stale one is refused (a conflict). Who made the
-- change, when, through the agent or an access token, and by which action of the app, is kept
-- on the row for its creation, its last change and its deletion; every revision is also kept
-- whole in app_row_history.
--
-- A delete no longer removes the row: it sets deleted_at (a tombstone), and ordinary reads,
-- screens, summaries, counts and the row limits all leave tombstones out. Restoring clears
-- the tombstone and bumps the revision. Tombstones are kept, so a client that syncs later can
-- be told what was deleted. A row goes for good only with its app (ON DELETE CASCADE).
--
-- Rows from before this migration get revision 1 and one history entry each, marked
-- synthetic: it holds the row's data as it stands now, dated at its creation and credited to
-- its creator, since earlier states were never kept.

ALTER TABLE app_rows ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1);
ALTER TABLE app_rows ADD COLUMN created_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (created_by_agent IN (0, 1));
ALTER TABLE app_rows ADD COLUMN created_via TEXT;
-- The app's action (its event name) that made the row, or NULL when it wasn't one.
ALTER TABLE app_rows ADD COLUMN created_action TEXT;
ALTER TABLE app_rows ADD COLUMN updated_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (updated_by_agent IN (0, 1));
ALTER TABLE app_rows ADD COLUMN updated_via TEXT;
ALTER TABLE app_rows ADD COLUMN updated_action TEXT;
-- NULL while the row is live; the time of the delete once it is a tombstone.
ALTER TABLE app_rows ADD COLUMN deleted_at TEXT;
ALTER TABLE app_rows ADD COLUMN deleted_by INTEGER REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE app_rows ADD COLUMN deleted_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (deleted_by_agent IN (0, 1));
ALTER TABLE app_rows ADD COLUMN deleted_via TEXT;
ALTER TABLE app_rows ADD COLUMN deleted_action TEXT;

-- The tombstones of an app, newest first, for the list of deleted records.
CREATE INDEX app_rows_deleted ON app_rows (app_id, deleted_at DESC) WHERE deleted_at IS NOT NULL;

-- Every version of a row. Reading it is reading the row: package apps filters by the row's
-- creator and the caller's scope exactly as for the row.
CREATE TABLE app_row_history (
    id           INTEGER PRIMARY KEY,
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    row_id       INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    revision     INTEGER NOT NULL CHECK (revision >= 1),
    operation    TEXT NOT NULL CHECK (operation IN ('insert', 'update', 'delete', 'restore')),
    -- The row's data as that revision left it (for a delete, as it was when deleted).
    data         TEXT NOT NULL CHECK (json_valid(data) AND json_type(data) = 'object'),
    at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor        INTEGER REFERENCES users (id) ON DELETE SET NULL,
    actor_agent  INTEGER NOT NULL DEFAULT 0 CHECK (actor_agent IN (0, 1)),
    actor_via    TEXT,
    action       TEXT,
    -- 1 for the entry this migration made for a row that already existed.
    synthetic    INTEGER NOT NULL DEFAULT 0 CHECK (synthetic IN (0, 1)),
    UNIQUE (row_id, revision)
) STRICT;

CREATE INDEX app_row_history_by_app ON app_row_history (app_id, row_id);

INSERT INTO app_row_history (app_id, row_id, revision, operation, data, at, actor, synthetic)
SELECT app_id, id, 1, 'insert', data, created_at, created_by, 1 FROM app_rows;

-- A row's revision only goes forward, and a history entry is never rewritten.
CREATE TRIGGER app_rows_revision_forward
BEFORE UPDATE OF revision ON app_rows
WHEN NEW.revision < OLD.revision
BEGIN
    SELECT RAISE(ABORT, 'a row''s revision cannot go back');
END;

CREATE TRIGGER app_row_history_immutable
BEFORE UPDATE ON app_row_history
BEGIN
    SELECT RAISE(ABORT, 'a row''s history is never rewritten');
END;
