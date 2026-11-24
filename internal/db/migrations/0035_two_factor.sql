-- Two-factor login with a time-based one-time code (TOTP) and recovery codes
-- (docs/api.md#two-factor-login).
--
-- users.totp_secret, left in 0002 for this, was never written: the secret is kept sealed
-- with the server's key (internal/serverkey) in its own table, and the column goes.

ALTER TABLE users DROP COLUMN totp_secret;

-- One row per user who has started or finished turning two-factor login on. The secret is
-- sealed (AES-GCM, bound to the user), never plain. enabled_at is NULL while the user has
-- the secret but hasn't confirmed it with a code: such a row doesn't ask for a code at
-- login. last_step is the last 30-second step whose code was accepted: a code of that
-- step or an earlier one is refused, so a code works once.
CREATE TABLE user_totp (
    user_id    INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    secret     BLOB NOT NULL,
    enabled_at TEXT,
    last_step  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- A user's recovery codes, for a lost authenticator. Only a keyed hash (HMAC under a key
-- derived from the server's) is kept; used_at is set when one is spent, so it works once.
CREATE TABLE user_recovery_codes (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, code_hash)
) STRICT;

-- The half-done login of someone whose password was right and who now owes a code. It is
-- not a session and nothing but the second step reads it; like a session, only the token's
-- SHA-256 is stored. attempts counts the wrong codes given with it.
CREATE TABLE login_challenges (
    id         INTEGER PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    attempts   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at TEXT NOT NULL
) STRICT;

CREATE INDEX login_challenges_by_expiry ON login_challenges (expires_at);
