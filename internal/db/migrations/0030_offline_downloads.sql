-- Whether the people of a space may download its apps for offline use (internal/apps,
-- offline.go). Admins turn it off for a space whose data shouldn't be kept on devices; a new
-- space allows it, as every space did before this column existed. A private space is its owner's
-- alone and always allows it.
ALTER TABLE spaces ADD COLUMN offline_downloads INTEGER NOT NULL DEFAULT 1 CHECK (offline_downloads IN (0, 1));
