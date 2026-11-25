-- A disabled account (docs/api.md#disabling-a-member): signed out everywhere, and refused at login,
-- as an access token's user and as the owner of a conversation, with everything it owns, its
-- memberships and its tokens kept, so that enabling it again restores its access as it was.
--
-- disabled_at is NULL for an account in use. disabled_by is the admin who did it, kept for the
-- record and cleared if that person is deleted.

ALTER TABLE users ADD COLUMN disabled_at TEXT;
ALTER TABLE users ADD COLUMN disabled_by INTEGER REFERENCES users (id) ON DELETE SET NULL;
