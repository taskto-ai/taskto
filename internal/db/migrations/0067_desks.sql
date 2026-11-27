-- AUTOINCREMENT: a desk's id is never given to another desk. A person who leaves a workspace
-- loses their desks (the trigger below), and without it their next desk would get the old id with
-- version 1, so a client holding the old id and version would overwrite it instead of being told
-- the desk changed.
CREATE TABLE desks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- '' is the person's main desk in the workspace; named desks come later.
    name         TEXT NOT NULL DEFAULT '',
    layout       TEXT NOT NULL CHECK (json_valid(layout)),
    version      INTEGER NOT NULL DEFAULT 1,
    position     INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, workspace_id, name)
) STRICT;
CREATE TRIGGER memberships_drop_desks AFTER DELETE ON memberships
BEGIN
    DELETE FROM desks WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
END;
