-- The first-run checklist on an admin's Home (internal/checklist).
--
-- Whether a step is done is never stored: it is worked out from what exists (members and invites,
-- provider accounts, shared spaces, apps, the server's email and backups). Only what an admin
-- chose is kept, per workspace and shared by its admins:
--
--   - setup_checklist_dismissed: the checklist was put away, so Home stops showing it. One row
--     per workspace; deleting it brings the checklist back.
--   - setup_checklist_skipped: a step that doesn't apply here (a one-person workspace has no team
--     to invite), which then counts as handled. One row per workspace and step.
--
-- Rows go with the workspace; the person who made one is only remembered while their account is.

CREATE TABLE setup_checklist_dismissed (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    dismissed_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    dismissed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE setup_checklist_skipped (
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    item         TEXT NOT NULL CHECK (item IN ('invite', 'provider', 'space', 'app', 'email', 'backups')),
    skipped_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    skipped_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (workspace_id, item)
) STRICT, WITHOUT ROWID;
