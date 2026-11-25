-- Indexes for the retention of an app's deleted records (internal/apps, retention.go).
--
-- The sweep finds the records deleted before a cutoff across every app, and removing a record
-- cascades to the tables that refer to it, and sets an operation's record aside first; each of
-- those lookups is by the record's id, which only some of the tables had an index for.

CREATE INDEX app_rows_deleted_by_time ON app_rows (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX app_operations_by_record ON app_operations (record_id) WHERE record_id IS NOT NULL;
CREATE INDEX app_effect_cards_by_record ON app_effect_cards (record_id);
