-- What the sessions list shows (docs/api.md#sessions): a short device description made from the
-- browser's user agent when the login is made, and when the session was last used. The user agent
-- and the client address are not kept, only the browser's and the system's names.
--
-- last_seen_at is NULL until the session is used after its login; the list falls back to
-- created_at, and the server writes it at most once a minute per session.

ALTER TABLE sessions ADD COLUMN browser TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN os TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_seen_at TEXT;
