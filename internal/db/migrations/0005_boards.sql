-- Boards: every member's personal board, in their private space, and the team boards of
-- shared spaces. A board belongs to one space, and whoever may read or write the space may
-- read or write its boards: package access decides, package boards (internal/boards) asks
-- it. The triggers below keep the rules that must hold whatever code writes the tables.

CREATE TABLE boards (
    id          INTEGER PRIMARY KEY,
    space_id    INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    -- 'personal': the one board of a private space, created with it. 'team': a board of a
    -- shared space; a shared space has any number of them.
    kind        TEXT NOT NULL CHECK (kind IN ('personal', 'team')),
    name        TEXT NOT NULL,
    -- An archived board stays readable, and nobody changes it or its cards. A personal
    -- board is never archived.
    archived_at TEXT,
    -- Bumped by every change, so a client's update can say which state it saw.
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    CHECK (kind = 'team' OR archived_at IS NULL)
) STRICT;

CREATE INDEX boards_by_space ON boards (space_id);
CREATE UNIQUE INDEX boards_one_personal ON boards (space_id) WHERE kind = 'personal';

-- A personal board is in a private space, a team board in a shared one.
CREATE TRIGGER boards_kind_matches_space
BEFORE INSERT ON boards
WHEN NOT EXISTS (
    SELECT 1 FROM spaces s
    WHERE s.id = NEW.space_id AND (s.kind = 'private') = (NEW.kind = 'personal')
)
BEGIN
    SELECT RAISE(ABORT, 'a personal board belongs in a private space, a team board in a shared one');
END;

CREATE TRIGGER boards_fixed_identity
BEFORE UPDATE OF space_id, kind ON boards
BEGIN
    SELECT RAISE(ABORT, 'a board''s space and kind cannot change');
END;

-- A private space comes with its personal board, however it was created.
CREATE TRIGGER spaces_personal_board
AFTER INSERT ON spaces
WHEN NEW.kind = 'private'
BEGIN
    INSERT INTO boards (space_id, kind, name, created_by, updated_by)
    VALUES (NEW.id, 'personal', 'Personal', NEW.owner_id, NEW.owner_id);
END;

INSERT INTO boards (space_id, kind, name, created_by, updated_by)
SELECT id, 'personal', 'Personal', owner_id, owner_id FROM spaces WHERE kind = 'private' ORDER BY id;

CREATE TABLE cards (
    id               INTEGER PRIMARY KEY,
    board_id         INTEGER NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    title            TEXT NOT NULL,
    -- Markdown.
    body             TEXT NOT NULL DEFAULT '',
    -- The card's column ("column" itself is an SQL keyword).
    col              TEXT NOT NULL CHECK (col IN ('backlog', 'todo', 'doing', 'review', 'done', 'archived')),
    -- The order within the column: a fractional index, compared as bytes. Moving a card
    -- gives it a key between its new neighbours', so a move writes one row.
    rank             TEXT NOT NULL,
    -- The column an archived card came from, where restoring puts it back.
    archived_from    TEXT CHECK (archived_from IN ('backlog', 'todo', 'doing', 'review', 'done')),
    -- The assignee: a person (a member who can read the space, checked when assigned) or
    -- the agent, which works the card for the person who asks it to. Never both.
    assignee_id      INTEGER REFERENCES users (id) ON DELETE SET NULL,
    assignee_agent   INTEGER NOT NULL DEFAULT 0 CHECK (assignee_agent IN (0, 1)),
    -- Why the card is blocked, or NULL. card_blockers holds the cards it waits for.
    blocked_reason   TEXT,
    version          INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    created_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    -- 1 when the agent made the change, for created_by or updated_by.
    created_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (created_by_agent IN (0, 1)),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (updated_by_agent IN (0, 1)),
    CHECK (NOT (assignee_agent AND assignee_id IS NOT NULL)),
    CHECK ((col = 'archived') = (archived_from IS NOT NULL))
) STRICT;

CREATE UNIQUE INDEX cards_by_board_column ON cards (board_id, col, rank);
CREATE INDEX cards_by_assignee ON cards (assignee_id);

CREATE TRIGGER cards_fixed_board
BEFORE UPDATE OF board_id ON cards
BEGIN
    SELECT RAISE(ABORT, 'a card cannot move to another board');
END;

-- The cards a card waits for: on the same board, and not itself.
CREATE TABLE card_blockers (
    card_id    INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    blocker_id INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    PRIMARY KEY (card_id, blocker_id),
    CHECK (card_id != blocker_id)
) STRICT;

CREATE INDEX card_blockers_by_blocker ON card_blockers (blocker_id);

CREATE TRIGGER card_blockers_same_board
BEFORE INSERT ON card_blockers
WHEN (SELECT board_id FROM cards WHERE id = NEW.card_id) IS NOT (SELECT board_id FROM cards WHERE id = NEW.blocker_id)
BEGIN
    SELECT RAISE(ABORT, 'a card can only be blocked by cards of its own board');
END;

-- What a card links to: knowledge-base docs, by path within the card's space, and
-- conversations, by the agent runtime's ID. Neither is checked to exist: a link can point
-- to a doc not written yet, or one since moved.
CREATE TABLE card_links (
    card_id INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('doc', 'conversation')),
    target  TEXT NOT NULL,
    PRIMARY KEY (card_id, kind, target)
) STRICT;

-- A card's thread.
CREATE TABLE card_comments (
    id         INTEGER PRIMARY KEY,
    card_id    INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    author_id  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    -- 1 when the agent wrote it, for author_id.
    by_agent   INTEGER NOT NULL DEFAULT 0 CHECK (by_agent IN (0, 1)),
    body       TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX card_comments_by_card ON card_comments (card_id, id);
