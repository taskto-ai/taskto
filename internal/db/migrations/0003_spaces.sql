-- Spaces: where a workspace's files, conversations, boards and apps live, and who may see
-- them. Every member of a workspace has one private space that only they can open; shared
-- spaces have a member list with a role each. package access (internal/access) is the one
-- place that answers "may this user read or write this space"; the triggers below keep the
-- rules that must hold whatever code writes the tables.

CREATE TABLE spaces (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('private', 'shared')),
    -- The owner of a private space; NULL for a shared one. A private space outlives its
    -- owner's membership (it comes back if they rejoin), so deleting an account has to
    -- decide what becomes of its spaces first: RESTRICT makes that explicit.
    owner_id     INTEGER REFERENCES users (id) ON DELETE RESTRICT,
    name         TEXT NOT NULL,
    -- Set when an admin archives a shared space: it stays readable to its members, and
    -- nobody can write to it.
    archived_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((kind = 'private') = (owner_id IS NOT NULL)),
    CHECK (kind = 'shared' OR archived_at IS NULL)
) STRICT;

CREATE INDEX spaces_by_workspace ON spaces (workspace_id);
-- One private space per member of a workspace.
CREATE UNIQUE INDEX spaces_private_owner ON spaces (workspace_id, owner_id) WHERE kind = 'private';

-- Who may use a shared space. A private space has no rows here: its owner is its only user.
CREATE TABLE space_members (
    space_id   INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('editor', 'viewer')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (space_id, user_id)
) STRICT;

CREATE INDEX space_members_by_user ON space_members (user_id);

-- A space never moves between workspaces, owners or kinds: access decisions and the
-- space's directory on disk depend on them.
CREATE TRIGGER spaces_fixed_identity
BEFORE UPDATE OF workspace_id, kind, owner_id ON spaces
BEGIN
    SELECT RAISE(ABORT, 'a space''s workspace, kind and owner cannot change');
END;

-- Only a shared space has members, and only members of its workspace.
CREATE TRIGGER space_members_are_workspace_members
BEFORE INSERT ON space_members
WHEN NOT EXISTS (
    SELECT 1 FROM spaces s JOIN memberships m ON m.workspace_id = s.workspace_id
    WHERE s.id = NEW.space_id AND s.kind = 'shared' AND m.user_id = NEW.user_id
)
BEGIN
    SELECT RAISE(ABORT, 'space members must be members of the space''s workspace');
END;

CREATE TRIGGER space_members_fixed_identity
BEFORE UPDATE OF space_id, user_id ON space_members
BEGIN
    SELECT RAISE(ABORT, 'change a space member''s role, or remove and add them');
END;

-- Joining a workspace, by setup, an invite or anything later, gives the member their
-- private space; rejoining gives back the one they had.
CREATE TRIGGER memberships_private_space
AFTER INSERT ON memberships
BEGIN
    INSERT INTO spaces (workspace_id, kind, owner_id, name)
    SELECT NEW.workspace_id, 'private', NEW.user_id, 'Private'
    WHERE NOT EXISTS (
        SELECT 1 FROM spaces WHERE workspace_id = NEW.workspace_id AND kind = 'private' AND owner_id = NEW.user_id
    );
END;

-- Leaving a workspace ends access to its shared spaces. The private space stays, closed
-- to everyone (access requires workspace membership) until its owner rejoins.
CREATE TRIGGER memberships_leave_spaces
AFTER DELETE ON memberships
BEGIN
    DELETE FROM space_members
    WHERE user_id = OLD.user_id AND space_id IN (SELECT id FROM spaces WHERE workspace_id = OLD.workspace_id);
END;

-- Existing members get their private spaces now.
INSERT INTO spaces (workspace_id, kind, owner_id, name)
SELECT workspace_id, 'private', user_id, 'Private' FROM memberships ORDER BY workspace_id, user_id;
