-- Password reset links an admin makes for a member (docs/api.md#password-reset-links). Like
-- invites and sessions, only the token's SHA-256 is stored; the link is shown once, to the
-- admin who made it. A user has at most one: a new link replaces the last, and using one,
-- or changing the password, deletes them all.

CREATE TABLE password_resets (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT NOT NULL
) STRICT;

CREATE INDEX password_resets_by_user ON password_resets (user_id);
