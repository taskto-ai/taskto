-- Skills: named, described instructions for the agent, text only (docs/skills.md).

CREATE TABLE skills (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id     INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    scope            TEXT NOT NULL CHECK (scope IN ('user', 'space', 'workspace')),
    user_id          INTEGER REFERENCES users (id) ON DELETE CASCADE,
    space_id         INTEGER REFERENCES spaces (id) ON DELETE CASCADE,
    space_generation INTEGER,
    name             TEXT NOT NULL,
    title            TEXT NOT NULL,
    description      TEXT NOT NULL,
    body             TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    origin           TEXT NOT NULL DEFAULT '',
    created_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((scope = 'user') = (user_id IS NOT NULL)),
    CHECK ((scope = 'space') = (space_id IS NOT NULL)),
    CHECK ((scope = 'space') = (space_generation IS NOT NULL)),
    CHECK (length(name) BETWEEN 2 AND 40)
) STRICT;

CREATE UNIQUE INDEX skills_user_name ON skills (workspace_id, user_id, name) WHERE scope = 'user';
CREATE UNIQUE INDEX skills_space_name ON skills (space_id, name) WHERE scope = 'space';
CREATE UNIQUE INDEX skills_workspace_name ON skills (workspace_id, name) WHERE scope = 'workspace';
CREATE INDEX skills_by_space ON skills (space_id) WHERE scope = 'space';

CREATE TRIGGER skills_space_matches
BEFORE INSERT ON skills
WHEN NEW.scope = 'space' AND NOT EXISTS (
    SELECT 1 FROM spaces s
    WHERE s.id = NEW.space_id AND s.workspace_id = NEW.workspace_id AND s.kind = 'shared' AND s.generation = NEW.space_generation
)
BEGIN
    SELECT RAISE(ABORT, 'a space''s skill belongs to a shared space of its workspace');
END;

CREATE TRIGGER skills_user_is_member
BEFORE INSERT ON skills
WHEN NEW.scope = 'user' AND NOT EXISTS (
    SELECT 1 FROM memberships m WHERE m.workspace_id = NEW.workspace_id AND m.user_id = NEW.user_id
)
BEGIN
    SELECT RAISE(ABORT, 'a person''s own skill belongs to a member of its workspace');
END;

CREATE TRIGGER skills_fixed_identity
BEFORE UPDATE OF workspace_id, scope, user_id, space_id, space_generation ON skills
WHEN NEW.workspace_id IS NOT OLD.workspace_id OR NEW.scope IS NOT OLD.scope OR NEW.user_id IS NOT OLD.user_id
    OR NEW.space_id IS NOT OLD.space_id OR NEW.space_generation IS NOT OLD.space_generation
BEGIN
    SELECT RAISE(ABORT, 'a skill never changes its owner');
END;

CREATE TRIGGER skills_membership_ended AFTER DELETE ON memberships
BEGIN
    DELETE FROM skills WHERE scope = 'user' AND user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
END;
