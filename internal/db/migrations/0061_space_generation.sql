-- A space's generation: what tells a space from another that had its ID.
--
-- SQLite gives a new row the largest ID in the table plus one, so after the newest space is
-- removed (an account's purge) its ID goes to the next space made. Everything that is kept or
-- decided by a space's ID while the space is being removed (the knowledge base's refusal of a
-- removed space, the apps' hold of its files, the search index, an authorization read before the
-- removal) has to tell the two apart, and a creation time can't: it has the precision of a
-- millisecond, and the same pair (ID, creation time) can come twice.
--
-- The generation is a number no space ever has twice. A trigger gives each new space the next
-- value of a counter, in the transaction of the INSERT, so no one reads a space that has none (0
-- means "not given yet" and is only ever seen inside that trigger); the counter is a table of its
-- own, raised and never lowered, so removing spaces doesn't take its value back. A space's
-- generation never changes. Rows that exist now get their ID, which is unique among them, and the
-- counter starts at the largest, so the next space is above every one of them.
--
-- The column is added, not the table rebuilt: other tables refer to spaces ON DELETE CASCADE, and
-- dropping it would take their rows with it.
ALTER TABLE spaces ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;

UPDATE spaces SET generation = id;

CREATE TABLE space_generations (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    last INTEGER NOT NULL CHECK (last >= 0)
) STRICT;

INSERT INTO space_generations (id, last) VALUES (1, coalesce((SELECT max(generation) FROM spaces), 0));

CREATE TRIGGER spaces_give_generation
AFTER INSERT ON spaces
BEGIN
    UPDATE space_generations SET last = last + 1 WHERE id = 1;
    UPDATE spaces SET generation = (SELECT last FROM space_generations WHERE id = 1) WHERE id = NEW.id;
END;

-- Once given, the generation is never changed (and a row inserted with one of its own is refused:
-- the trigger above could not replace it).
CREATE TRIGGER spaces_fixed_generation
BEFORE UPDATE OF generation ON spaces
WHEN OLD.generation <> 0 AND NEW.generation IS NOT OLD.generation
BEGIN
    SELECT RAISE(ABORT, 'a space''s generation is never changed');
END;

CREATE TRIGGER spaces_no_counter_lowering
BEFORE UPDATE OF last ON space_generations
WHEN NEW.last < OLD.last
BEGIN
    SELECT RAISE(ABORT, 'the counter of space generations is never lowered');
END;

CREATE TRIGGER space_generations_keep_counter
BEFORE DELETE ON space_generations
BEGIN
    SELECT RAISE(ABORT, 'the counter of space generations is never removed');
END;

-- And it is one row, never replaced: INSERT OR REPLACE would delete the row and put another, which
-- the guards on UPDATE and DELETE don't see (SQLite fires no delete trigger for a replacement unless
-- recursive triggers are on).
CREATE TRIGGER space_generations_one_row
BEFORE INSERT ON space_generations
WHEN EXISTS (SELECT 1 FROM space_generations)
BEGIN
    SELECT RAISE(ABORT, 'the counter of space generations is one row, and is never replaced');
END;

-- The index's documents are numbered the same way: search_docs.id (which is also the rowid of
-- the document's row in search_fts, and the key of its chunks) was the table's largest plus one,
-- so after a space was removed its documents' IDs could go to the documents of a space made
-- with its ID, and anything that held a document's ID across a wait (a hit whose snippet is read
-- later, an embedding job that read its pending list, the vector cache) read the new space's
-- text under the old name. With AUTOINCREMENT an ID is never given twice.
--
-- The table is rebuilt, and nothing that exists is lost or renumbered: the rows keep their IDs, so
-- the full text rows (rowid = id) and the chunks with their vectors still belong to them, and no
-- document is embedded again. The chunks are kept aside first, because dropping the table takes
-- the rows that refer to it with it (ON DELETE CASCADE; the implicit delete fires no trigger, so the
-- full text rows stay), and put back after.
CREATE TABLE search_chunks_keep AS SELECT doc_id, seq, text, vector FROM search_chunks;

CREATE TABLE search_docs_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    space_id      INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    path          TEXT NOT NULL,
    version       TEXT NOT NULL,
    markdown      INTEGER NOT NULL,
    title         TEXT NOT NULL,
    embed_version TEXT,
    embed_key     TEXT,
    embed_tries   INTEGER NOT NULL DEFAULT 0,
    embed_after   TEXT,
    text_format   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (space_id, path)
) STRICT;

INSERT INTO search_docs_new (id, space_id, path, version, markdown, title, embed_version, embed_key, embed_tries, embed_after, text_format)
    SELECT id, space_id, path, version, markdown, title, embed_version, embed_key, embed_tries, embed_after, text_format FROM search_docs;

DROP TABLE search_docs;
ALTER TABLE search_docs_new RENAME TO search_docs;

CREATE TRIGGER search_docs_fts_delete
AFTER DELETE ON search_docs
BEGIN
    DELETE FROM search_fts WHERE rowid = OLD.id;
END;

INSERT INTO search_chunks (doc_id, seq, text, vector) SELECT doc_id, seq, text, vector FROM search_chunks_keep;
DROP TABLE search_chunks_keep;
