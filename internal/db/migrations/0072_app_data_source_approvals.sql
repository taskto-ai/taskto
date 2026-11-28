-- The approvals of apps' data sources (internal/apps, data_source_approvals.go; docs/data-sources.md).
-- As for the integrations' approvals (0052): one row per request, so the live release's contract
-- stays approved while the next draft's is decided; a contract asked for again after it was turned
-- down or revoked is a new row; at most one request per app, slot and contract waits or is approved.
CREATE TABLE app_data_source_approvals (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id        INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    slot          TEXT NOT NULL CHECK (length(slot) BETWEEN 1 AND 40),
    kind          TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 40),
    contract_hash TEXT NOT NULL CHECK (length(contract_hash) = 64),
    contract      TEXT NOT NULL CHECK (json_valid(contract) AND json_type(contract) = 'object'),
    status        TEXT NOT NULL CHECK (status IN ('requested', 'approved', 'rejected', 'revoked')),
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    requested_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    requested_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    decided_by    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    decided_at    TEXT,
    CHECK ((status = 'requested') = (decided_at IS NULL))
) STRICT;

CREATE INDEX app_data_source_approvals_by_status ON app_data_source_approvals (status, id);
CREATE INDEX app_data_source_approvals_by_app ON app_data_source_approvals (app_id);
CREATE UNIQUE INDEX app_data_source_approvals_one_active ON app_data_source_approvals (app_id, slot, contract_hash)
    WHERE status IN ('requested', 'approved');

CREATE TRIGGER app_data_source_approvals_fixed
BEFORE UPDATE OF app_id, slot, kind, contract_hash, contract ON app_data_source_approvals
WHEN NEW.app_id IS NOT OLD.app_id OR NEW.slot IS NOT OLD.slot OR NEW.kind IS NOT OLD.kind
  OR NEW.contract_hash IS NOT OLD.contract_hash OR NEW.contract IS NOT OLD.contract
BEGIN
    SELECT RAISE(ABORT, 'an approval''s app, slot and contract never change');
END;

-- An app moved to another space leaves its approvals behind, as it leaves its roles.
CREATE TRIGGER apps_move_drops_data_source_approvals
AFTER UPDATE OF space_id ON apps
WHEN OLD.space_id IS NOT NEW.space_id
BEGIN
    DELETE FROM app_data_source_approvals WHERE app_id = NEW.id;
END;
