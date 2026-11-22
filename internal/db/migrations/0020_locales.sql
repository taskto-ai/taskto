-- Languages (docs/i18n.md). A user's locale is the one they picked for the web app, the
-- agent's replies and dictation; '' means none picked, and their browser's language is
-- used. A workspace's locale is its default, for members whose browser asks for a
-- language the web app doesn't have; '' means English. Both are BCP 47 tags such as
-- "pt-BR", checked by the server (internal/auth, ValidLocale).
ALTER TABLE users ADD COLUMN locale TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN locale TEXT NOT NULL DEFAULT '';
