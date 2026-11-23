-- App roles: who may build, publish, use and read the data of one app, inside the space
-- boundary (internal/access, apps.go). Space membership stays the outer boundary: a role
-- here only narrows what the user's role in the space already allows, never widens it.
--
-- Once an app has any row here it is restricted for every member: their own roles, else the
-- default's (user_id NULL), else none (they don't see the app).
--
-- An app without any row here is unrestricted, which is exactly what every app is today: the
-- space's editors (and a private space's owner) build, publish, submit and manage all its
-- records, and its viewers read all of them. This migration adds no row, so nothing changes
-- for any existing app until someone sets roles on it.
--
-- Own-record reading needs to know who made each record. app_rows.created_by has recorded
-- the user (or the user the agent worked for) since 0010, so no data is backfilled. A row
-- whose creator's account was deleted has a NULL created_by and is read by nobody who may
-- only read their own records: it stays visible to those who read all.

CREATE TABLE app_roles (
    app_id     INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- NULL: every member of the space who has no rows of their own for the app.
    user_id    INTEGER REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('builder', 'publisher', 'user', 'data_manager', 'reader')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- A user holds a role of an app once; user IDs start at 1, so 0 stands for NULL here.
CREATE UNIQUE INDEX app_roles_unique ON app_roles (app_id, coalesce(user_id, 0), role);
CREATE INDEX app_roles_by_user ON app_roles (user_id) WHERE user_id IS NOT NULL;

-- Reading only one's own records filters an app's rows by their creator.
CREATE INDEX app_rows_by_creator ON app_rows (app_id, created_by);

-- Roles are given to members of the app's space, for that space. Someone who leaves the
-- space (removed, or leaving the workspace, whose trigger removes their space memberships)
-- loses their roles in its apps, so rejoining later, or the app moving to a space they are
-- in, never brings back what they had.
CREATE TRIGGER space_members_leave_app_roles
AFTER DELETE ON space_members
BEGIN
    DELETE FROM app_roles
    WHERE user_id = OLD.user_id AND app_id IN (SELECT id FROM apps WHERE space_id = OLD.space_id);
END;

-- An app moved to another space leaves its roles behind: they were set for the old space's
-- members, and the new space's access decides until someone sets roles there.
CREATE TRIGGER apps_move_drops_roles
AFTER UPDATE OF space_id ON apps
WHEN OLD.space_id IS NOT NEW.space_id
BEGIN
    DELETE FROM app_roles WHERE app_id = NEW.id;
END;
