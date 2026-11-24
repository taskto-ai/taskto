-- Email for notifications (internal/notifications/email.go). It is off until a person opts in, per
-- workspace and per kind of notification, and a mail carries no record data, only that something
-- happened and a link back to the app, like a push.

-- What a person chose for a kind in a workspace: 'instant' (a mail soon after the notification is
-- made) or 'digest' (counted in the daily summary). A kind with no row is off. since is the
-- newest notification there was when the kind was last turned to 'digest': the summary counts only
-- what came after it, so what was mailed as it happened, or came before the choice, is never counted.
CREATE TABLE notification_email_prefs (
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('instant', 'digest')),
    since        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, workspace_id, kind)
) STRICT;

-- When the daily summary is sent (the hour of the person's own time zone, an IANA name) and the
-- last notification it has gone through, so one is counted in one summary only. timezone_chosen says
-- whether the person has set the time and zone themselves: until then the zone is a default the
-- settings page may replace with the browser's.
CREATE TABLE notification_email_settings (
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id  INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    digest_hour   INTEGER NOT NULL DEFAULT 8 CHECK (digest_hour BETWEEN 0 AND 23),
    timezone      TEXT NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 64),
    digest_cursor INTEGER NOT NULL DEFAULT 0,
    timezone_chosen INTEGER NOT NULL DEFAULT 0 CHECK (timezone_chosen IN (0, 1)),
    PRIMARY KEY (user_id, workspace_id)
) STRICT;

-- The mails to send as they happen: one per notification, queued in the transaction that makes it
-- and only for someone who chose 'instant' for its kind and verified their address, and sent by
-- the email worker with bounded tries (and only if the server can send mail).
CREATE TABLE email_deliveries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    notification_id INTEGER NOT NULL UNIQUE REFERENCES notifications (id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    updated_at      TEXT NOT NULL
) STRICT;

CREATE INDEX email_deliveries_due ON email_deliveries (state, next_attempt_at);

-- Every mail the server has submitted, or is about to, written BEFORE it is submitted, so that a
-- server that stops between the relay's acceptance and the note that it was sent never sends it
-- again: a key that has a row is not sent a second time unless the relay definitely refused it.
-- The key names what the mail is for ('instant:<notification>', 'digest:<job occurrence>').
--   sending   written, and not yet answered: if a worker finds one it died on it, and whether the
--             relay took it is unknown
--   sent      the relay accepted it
--   refused   the relay definitely did not take it (so a later try may send it)
--   unknown   the connection was lost after the message was sent: the relay may have taken it, so
--             it is never sent again (at most once)
--   cancelled the person's choice, access or the job changed before it was submitted
-- It is the per-person history the hourly limit counts, and so is kept apart from the
-- notifications and from memberships: deleting a notification, or leaving a workspace, doesn't free
-- a slot. upto is the newest notification a summary covers.
CREATE TABLE notification_email_ledger (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key         TEXT NOT NULL UNIQUE CHECK (length(key) BETWEEN 1 AND 200),
    kind        TEXT NOT NULL CHECK (kind IN ('instant', 'digest')),
    state       TEXT NOT NULL CHECK (state IN ('sending', 'sent', 'refused', 'unknown', 'cancelled')),
    upto        INTEGER NOT NULL DEFAULT 0,
    attempts    INTEGER NOT NULL DEFAULT 0,
    reserved_at TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

CREATE INDEX notification_email_ledger_by_user ON notification_email_ledger (user_id, kind, reserved_at);

-- Whether a person's address is theirs: nothing is mailed to an address until its owner has opened
-- a link sent to it. One row per person, for the address the link went to; it counts only while
-- that is still the person's address. The token is stored hashed (SHA-256), is single use and
-- expires; sent_at is what limits how often a link is sent.
CREATE TABLE notification_email_verification (
    user_id     INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    address     TEXT NOT NULL COLLATE NOCASE,
    token_hash  TEXT,
    expires_at  TEXT,
    sent_at     TEXT,
    verified_at TEXT
) STRICT;

-- Leaving a workspace ends the person's email choices for it, and the daily summary scheduled for it.
-- (The ledger stays: it is what the hourly limit counts.)
CREATE TRIGGER notification_email_membership_ended AFTER DELETE ON memberships
BEGIN
    DELETE FROM notification_email_prefs WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
    DELETE FROM notification_email_settings WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
    DELETE FROM scheduled_jobs
        WHERE kind = 'notifications.digest' AND user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
END;
