-- The installation itself: a single row, written when the database is created.
-- Workspaces, users and the rest of the schema come in later migrations.
CREATE TABLE instance (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

INSERT INTO instance (id) VALUES (1);
