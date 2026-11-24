-- Email for notifications (internal/notifications/email.go). It is off until a person opts in, per
-- workspace and per kind of notification, and a mail carries no record data, only that something
-- happened and a link back to the app, like a push.

-- What a person chose for a kind in a workspace: 'instant' (a mail soon after the notification is
-- made) or 'digest' (counted in the daily summary). A kind with no row is off.
CREATE TABLE notification_email_prefs (
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('instant', 'digest')),
    PRIMARY KEY (user_id, workspace_id, kind)
) STRICT;

-- When the daily summary is sent (the hour of the person's own time zone, an IANA name) and the
-- last notification it has gone through, so one is counted in one summary only.
CREATE TABLE notification_email_settings (
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id  INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    digest_hour   INTEGER NOT NULL DEFAULT 8 CHECK (digest_hour BETWEEN 0 AND 23),
    timezone      TEXT NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 64),
    digest_cursor INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, workspace_id)
) STRICT;

-- The mails to send as they happen: one per notification, queued in the transaction that makes it
-- and only for someone who chose 'instant' for its kind, and sent by the worker with bounded
-- tries (and only if the server can send mail).
CREATE TABLE email_deliveries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    notification_id INTEGER NOT NULL UNIQUE REFERENCES notifications (id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;

CREATE INDEX email_deliveries_due ON email_deliveries (state, next_attempt_at);

-- Leaving a workspace ends the person's email choices for it, and the daily summary scheduled for it.
CREATE TRIGGER notification_email_membership_ended AFTER DELETE ON memberships
BEGIN
    DELETE FROM notification_email_prefs WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
    DELETE FROM notification_email_settings WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
    DELETE FROM scheduled_jobs
        WHERE kind = 'notifications.digest' AND user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
END;
