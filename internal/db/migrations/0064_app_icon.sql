-- An app's icon: one emoji, or empty to show the initials of its name.
ALTER TABLE apps ADD COLUMN icon TEXT NOT NULL DEFAULT '' CHECK (length(CAST(icon AS BLOB)) <= 32);
