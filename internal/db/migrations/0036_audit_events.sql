-- The audit log (docs/audit.md): who did what to which thing, for security and admin actions.
-- Rows are written by the code that makes the change, in its transaction, and removed by the
-- retention sweep (internal/audit). The log never holds a secret: details is a small JSON of
-- non-secret facts (a role, a provider's host, the names of the fields that changed).
--
-- A workspace's events go with the workspace; an actor who is deleted leaves their events with
-- no actor. workspace_id is NULL for events that belong to no one workspace: a login, a
-- password change, a server-wide event.

CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    at           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    workspace_id INTEGER REFERENCES workspaces (id) ON DELETE CASCADE,
    actor_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    action       TEXT NOT NULL,
    target_type  TEXT NOT NULL DEFAULT '',
    target_id    INTEGER,
    -- The target's own created_at: SQLite hands a deleted row's id to a later one, so an event
    -- is about the object with this type, id and creation time. NULL when the target was gone
    -- or has no such time.
    target_created_at TEXT,
    details      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details)),
    client_addr  TEXT
) STRICT;

-- A workspace's log, newest first; the sweep and the server-wide view read by time alone.
CREATE INDEX audit_events_by_workspace ON audit_events (workspace_id, at DESC, id DESC);
CREATE INDEX audit_events_by_time ON audit_events (at);
CREATE INDEX audit_events_by_actor ON audit_events (actor_id) WHERE actor_id IS NOT NULL;
