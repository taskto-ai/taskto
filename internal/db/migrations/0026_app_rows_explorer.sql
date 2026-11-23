-- Indexes for the data explorer of personal apps (internal/apps/query.go): a page of an app's
-- records is read by keyset, most recently changed first unless another order is asked for, and
-- every count and total is over the live rows of one app, all of them or only those one user
-- created (the read scope of their role in the app).
--
-- Both are partial indexes over live rows: the deleted ones have their own (0023).

CREATE INDEX app_rows_live_updated ON app_rows (app_id, updated_at DESC, id DESC) WHERE deleted_at IS NULL;
CREATE INDEX app_rows_live_by_creator ON app_rows (app_id, created_by, id) WHERE deleted_at IS NULL;
