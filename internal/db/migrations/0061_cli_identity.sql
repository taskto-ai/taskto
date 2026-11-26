-- Each account's identity for the CLI worker (docs/cli-workers.md): random, set when the account
-- is made, never changed and never given to another account. The worker records it as the owner
-- of the account's directory, and keeps a directory only for the account it records: a restore of
-- the server's data gives user numbers again, while the worker's own directory (a volume of its
-- own in container mode) isn't part of the backup and may still hold, under the same number, the
-- directory of someone from the history the restore went back from.
ALTER TABLE users ADD COLUMN cli_identity TEXT;

UPDATE users SET cli_identity = lower(hex(randomblob(16)));

CREATE UNIQUE INDEX users_cli_identity ON users (cli_identity);

-- Every account gets one, however it is made.
CREATE TRIGGER users_cli_identity_set AFTER INSERT ON users
WHEN NEW.cli_identity IS NULL
BEGIN
    UPDATE users SET cli_identity = lower(hex(randomblob(16))) WHERE id = NEW.id;
END;

-- And keeps it.
CREATE TRIGGER users_cli_identity_kept BEFORE UPDATE OF cli_identity ON users
WHEN OLD.cli_identity IS NOT NULL AND NEW.cli_identity IS NOT OLD.cli_identity
BEGIN
    SELECT RAISE(ABORT, 'an account''s CLI identity never changes');
END;
