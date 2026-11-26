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
