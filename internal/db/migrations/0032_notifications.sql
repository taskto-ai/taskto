-- The notification inbox and optional Web Push (internal/notifications).
--
-- A notification is a reference, never a copy: its kind and the IDs of what it is about (a card, an
-- app), for one person in one workspace. What it says is worked out when it is read, or pushed, from
-- what that person may read at that moment (package notifications, render.go), so nothing in this
-- table is shown as it is, and a person who lost access to what it is about is told only that
-- something changed.
CREATE TABLE notifications (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    -- The IDs the kind says what they are (JSON object of numbers): space, board, card, app.
    ref          TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(ref)),
    -- Makes a producer's retry add nothing twice; '' for one that has none.
    dedupe_key   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    read_at      TEXT
) STRICT;

CREATE INDEX notifications_by_user ON notifications (user_id, workspace_id, id DESC);
CREATE UNIQUE INDEX notifications_dedupe ON notifications (user_id, kind, dedupe_key) WHERE dedupe_key <> '';

-- The server's VAPID key pair (RFC 8292), made once, on first start: it names the server to the
-- browsers' push services, and each subscription is bound to its public key, so replacing it would
-- end every subscription. The private key is sealed with the server key and never leaves the
-- package.
CREATE TABLE push_vapid (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    public_key     BLOB NOT NULL,
    private_sealed BLOB NOT NULL,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- A browser that opted in to push: one row per device (its endpoint), for one person. The endpoint
-- is a capability URL of the browser's push service, and p256dh and auth are the browser's keys for
-- encrypting to it (RFC 8291): the auth secret is sealed with the server key. A subscription is
-- bound to the session that made it, so signing out (or a session that expires, or signing out
-- everywhere) removes it.
CREATE TABLE push_subscriptions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    session_id   INTEGER NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    endpoint     TEXT NOT NULL UNIQUE,
    p256dh       BLOB NOT NULL,
    auth_sealed  BLOB NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX push_subscriptions_by_user ON push_subscriptions (user_id);
CREATE INDEX push_subscriptions_by_session ON push_subscriptions (session_id);

-- The pushes to send: one per notification and subscription, queued in the transaction that makes
-- the notification, so a device that opted in later is never sent an older one, and sent by the
-- worker with bounded tries.
CREATE TABLE push_deliveries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    notification_id INTEGER NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
    subscription_id INTEGER NOT NULL REFERENCES push_subscriptions (id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    -- The HTTP status of the last try, 0 when the request never got an answer.
    last_status     INTEGER NOT NULL DEFAULT 0,
    updated_at      TEXT NOT NULL,
    UNIQUE (notification_id, subscription_id)
) STRICT;

CREATE INDEX push_deliveries_due ON push_deliveries (state, next_attempt_at);

-- Leaving a workspace ends its notifications for that person (and with them the pushes queued), and
-- leaving the last one ends their push subscriptions: nothing of a workspace is delivered to someone
-- who is no longer in it.
CREATE TRIGGER notifications_membership_ended AFTER DELETE ON memberships
BEGIN
    DELETE FROM notifications WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id;
    DELETE FROM push_subscriptions
        WHERE user_id = OLD.user_id
          AND NOT EXISTS (SELECT 1 FROM memberships WHERE user_id = OLD.user_id);
END;
