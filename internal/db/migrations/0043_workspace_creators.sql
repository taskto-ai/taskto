-- Who made each workspace, and which one is the server's own.
--
-- created_by: the user whose request made the workspace (setup, or POST /api/workspaces). It
-- bounds how many workspaces one account can make and how fast. NULL for a workspace made
-- before this column, and once its creator is deleted.
--
-- is_setup: 1 for the workspace the server's setup made, the first one, and for no other. Its
-- admins are the server's operators: they, and not the admins of a workspace anyone made
-- later, see what belongs to the whole server (its backups, its outgoing email). A stored
-- marker, not "the lowest id", so no change to ids can hand the role to someone else.
-- Servers that already ran setup mark their oldest workspace.

ALTER TABLE workspaces ADD COLUMN created_by INTEGER REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE workspaces ADD COLUMN is_setup INTEGER NOT NULL DEFAULT 0 CHECK (is_setup IN (0, 1));

UPDATE workspaces SET is_setup = 1 WHERE id = (SELECT MIN(id) FROM workspaces);

CREATE UNIQUE INDEX workspaces_one_setup ON workspaces (is_setup) WHERE is_setup = 1;
CREATE INDEX workspaces_by_creator ON workspaces (created_by, created_at) WHERE created_by IS NOT NULL;
