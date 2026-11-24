-- What the sessions list shows (docs/api.md#sessions): a short device description made from the
-- browser's user agent when the login is made, and when the session was last used. The user agent
-- and the client address are not kept, only the browser's and the system's names.
--
-- last_seen_at is NULL until the session is used after its login; the list falls back to
-- created_at, and the server writes it at most once a minute per session.

ALTER TABLE sessions ADD COLUMN browser TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN os TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_seen_at TEXT;

-- A session's public ID, the name the API and the web app give it: 128 random bits in hex, never
-- reused. The row's integer ID can't be that: SQLite hands the highest ID out again once its row
-- is deleted, so a page that listed a session since signed out could end a newer login by it.
-- The integer ID stays the key other tables point at.
ALTER TABLE sessions ADD COLUMN public_id TEXT NOT NULL DEFAULT '';
UPDATE sessions SET public_id = lower(hex(randomblob(16)));
CREATE UNIQUE INDEX sessions_by_public_id ON sessions (public_id);
