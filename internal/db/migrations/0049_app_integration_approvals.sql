-- The approvals of apps' integrations (internal/apps, integration_approvals.go;
-- docs/integrations.md).
--
-- An app declares a slot of a kind of outside service ("payments") and nothing else: no provider,
-- no key, no integration id. The server works out the slot's contract from the draft (what the app
-- sends out, which fields the integration may write and with what) and a hash of it. The one who may
-- publish the app asks for that contract to be approved; a workspace admin approves it, choosing
-- an integration of the workspace that is allowed in the app's space, or turns it down. A publish
-- and a rollback need an approval for the contract of the release they make live. The admin sees
-- the contract and the names of the app and its space, never the app or its records, so an app in
-- a private space is approved the same way.
--
-- One row per app, slot and contract hash: the live release's contract stays approved while an
-- admin decides on the next draft's.
CREATE TABLE app_integration_approvals (
    id            INTEGER PRIMARY KEY,
    app_id        INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- The slot's name in the app's definition.
    slot          TEXT NOT NULL CHECK (length(slot) BETWEEN 1 AND 40),
    -- The slot's kind, checked in Go (a new kind is not a migration).
    kind          TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 40),
    -- The SHA-256, in hex, of the contract, and the contract as it was submitted.
    contract_hash TEXT NOT NULL CHECK (length(contract_hash) = 64),
    contract      TEXT NOT NULL CHECK (json_valid(contract) AND json_type(contract) = 'object'),
    -- The integration the admin bound the slot to: set while the approval is approved, and only
    -- then, so an integration with a live approval can't be deleted (an admin switches it off),
    -- and one whose approvals are all decided can.
    integration_id INTEGER REFERENCES integrations (id) ON DELETE RESTRICT,
    status        TEXT NOT NULL CHECK (status IN ('requested', 'approved', 'rejected', 'revoked')),
    -- When the row was made, which never changes: with the id it says which approval an audit event is
    -- about. requested_at is when the contract was last asked for.
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    requested_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    requested_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    decided_by    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    decided_at    TEXT,
    UNIQUE (app_id, slot, contract_hash),
    CHECK ((status = 'approved') = (integration_id IS NOT NULL)),
    CHECK ((status = 'requested') = (decided_at IS NULL))
) STRICT;

-- The admin's queue, by workspace, newest first, is read through the app's space.
CREATE INDEX app_integration_approvals_by_status ON app_integration_approvals (status, id);
CREATE INDEX app_integration_approvals_by_app ON app_integration_approvals (app_id);
CREATE INDEX app_integration_approvals_by_integration ON app_integration_approvals (integration_id)
    WHERE integration_id IS NOT NULL;

-- What was asked for never changes: another contract is another row.
CREATE TRIGGER app_integration_approvals_fixed
BEFORE UPDATE OF app_id, slot, kind, contract_hash, contract ON app_integration_approvals
WHEN NEW.app_id IS NOT OLD.app_id OR NEW.slot IS NOT OLD.slot OR NEW.kind IS NOT OLD.kind
  OR NEW.contract_hash IS NOT OLD.contract_hash OR NEW.contract IS NOT OLD.contract
BEGIN
    SELECT RAISE(ABORT, 'an approval''s app, slot and contract never change');
END;

-- An integration bound to an approval is one of the app's own workspace: an app never uses another
-- workspace's integration.
CREATE TRIGGER app_integration_approvals_same_workspace_insert
BEFORE INSERT ON app_integration_approvals
WHEN NEW.integration_id IS NOT NULL AND
     (SELECT workspace_id FROM integrations WHERE id = NEW.integration_id)
     IS NOT (SELECT s.workspace_id FROM apps a JOIN spaces s ON s.id = a.space_id WHERE a.id = NEW.app_id)
BEGIN
    SELECT RAISE(ABORT, 'an app uses only its own workspace''s integrations');
END;

CREATE TRIGGER app_integration_approvals_same_workspace_update
BEFORE UPDATE OF integration_id ON app_integration_approvals
WHEN NEW.integration_id IS NOT NULL AND
     (SELECT workspace_id FROM integrations WHERE id = NEW.integration_id)
     IS NOT (SELECT s.workspace_id FROM apps a JOIN spaces s ON s.id = a.space_id WHERE a.id = NEW.app_id)
BEGIN
    SELECT RAISE(ABORT, 'an app uses only its own workspace''s integrations');
END;

-- An app moved to another space leaves its approvals behind, as it leaves its roles: they were
-- given for the old space, whose members and allowed integrations were another's. The copy's and
-- the moved app's contracts are asked for again.
CREATE TRIGGER apps_move_drops_integration_approvals
AFTER UPDATE OF space_id ON apps
WHEN OLD.space_id IS NOT NEW.space_id
BEGIN
    DELETE FROM app_integration_approvals WHERE app_id = NEW.id;
END;
