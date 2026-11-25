-- A person's own time zone (internal/auth, timezone.go): an IANA name such as "Europe/Lisbon", which the
-- server checks. '' means none is known yet; the web app then sends the browser's once, and until
-- there is one the server uses UTC. Due-date reminders go out at 08:00 of this zone
-- (internal/boards, reminders.go), and so does the daily email summary until the person chooses a
-- time zone for it in a workspace (internal/notifications, email.go).
ALTER TABLE users ADD COLUMN time_zone TEXT NOT NULL DEFAULT '' CHECK (length(time_zone) <= 64);
