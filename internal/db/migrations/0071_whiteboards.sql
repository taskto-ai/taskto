-- The live state of a whiteboard document (docs/whiteboard.md). AUTOINCREMENT: the id is the live
-- topic's resource id and is never given to another board. version is the blob the log applies on.
CREATE TABLE whiteboards (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    space_id      INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    generation    INTEGER NOT NULL,
    path          TEXT NOT NULL,
    version       TEXT NOT NULL,
    seq           INTEGER NOT NULL DEFAULT 0 CHECK (seq >= 0),
    committed_seq INTEGER NOT NULL DEFAULT 0 CHECK (committed_seq >= 0 AND committed_seq <= seq),
    editor_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    UNIQUE (space_id, generation, path)
) STRICT;

CREATE TABLE whiteboard_ops (
    board_id INTEGER NOT NULL REFERENCES whiteboards (id) ON DELETE CASCADE,
    seq      INTEGER NOT NULL CHECK (seq > 0),
    ops      TEXT NOT NULL CHECK (json_valid(ops)),
    PRIMARY KEY (board_id, seq)
) STRICT, WITHOUT ROWID;
