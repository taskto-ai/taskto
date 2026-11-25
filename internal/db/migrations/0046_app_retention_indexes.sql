-- Indexes and an id high-water mark for the retention of an app's deleted records (internal/apps, retention.go).
--
-- The sweep finds the records deleted before a cutoff across every app, and removing a record
-- cascades to the tables that refer to it, and sets an operation's record aside first; each of
-- those lookups is by the record's id, which only some of the tables had an index for.

CREATE INDEX app_rows_deleted_by_time ON app_rows (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX app_operations_by_record ON app_operations (record_id) WHERE record_id IS NOT NULL;
CREATE INDEX app_effect_cards_by_record ON app_effect_cards (record_id);

-- The largest id a record of an app has had, kept apart from the rows. A record's id is a SQLite
-- rowid, which a new row takes as the largest id there is plus one, so once the sweep removes the
-- largest record, or a cascade removes whole apps, its id would be given to the next record, and a
-- stale change that names it (an offline edit that sends no expected revision) would land on
-- that one. The sweep raises the mark to the largest id before it removes records, and a new
-- record takes an id above it (internal/apps, apply).
CREATE TABLE app_row_ids (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    high INTEGER NOT NULL CHECK (high >= 0)
) STRICT;

INSERT INTO app_row_ids (id, high) VALUES (1, coalesce((SELECT max(id) FROM app_rows), 0));
