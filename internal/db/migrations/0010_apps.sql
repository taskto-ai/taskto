-- Personal apps: a screen built from a fixed component catalog, a collection with a
-- schema, and actions the server runs (internal/apps). An app belongs to one space, and
-- whoever may read or write the space may read or write its apps and their rows: package
-- access decides, package apps asks it. The definition is checked strictly when it is
-- saved; the database only holds it.

CREATE TABLE apps (
    id               INTEGER PRIMARY KEY,
    -- Moving an app to another space (to share it) moves its rows with it.
    space_id         INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    -- The screen, collection, actions and summaries, as JSON (apps.Definition).
    definition       TEXT NOT NULL CHECK (json_valid(definition)),
    -- An app taken offline: it can't be launched and its actions don't run; its rows are
    -- kept until it is restored.
    archived_at      TEXT,
    -- Bumped by every change, so a client's update can say which state it saw.
    version          INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    -- 1 when the agent made the change, for created_by or updated_by.
    created_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (created_by_agent IN (0, 1)),
    -- The name of the access token a change was made with, or NULL (as for cards, 0009).
    created_via      TEXT,
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (updated_by_agent IN (0, 1)),
    updated_via      TEXT
) STRICT;

CREATE INDEX apps_by_space ON apps (space_id);

-- An app moves only between spaces of one workspace.
CREATE TRIGGER apps_same_workspace
BEFORE UPDATE OF space_id ON apps
WHEN (SELECT workspace_id FROM spaces WHERE id = OLD.space_id) IS NOT (SELECT workspace_id FROM spaces WHERE id = NEW.space_id)
BEGIN
    SELECT RAISE(ABORT, 'an app cannot move to another workspace');
END;

-- An app's rows: each a JSON object of the collection's fields, checked by the server
-- against the collection when it is written.
CREATE TABLE app_rows (
    id         INTEGER PRIMARY KEY,
    app_id     INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    data       TEXT NOT NULL CHECK (json_valid(data) AND json_type(data) = 'object'),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_by INTEGER REFERENCES users (id) ON DELETE SET NULL
) STRICT;

CREATE INDEX app_rows_by_app ON app_rows (app_id, id);

CREATE TRIGGER app_rows_fixed_app
BEFORE UPDATE OF app_id ON app_rows
BEGIN
    SELECT RAISE(ABORT, 'a row cannot move to another app');
END;
