-- What a purge removed, kept across its tries (docs/audit.md, account_purged).
--
-- Some of the purge's steps are done before the transaction that deletes the account: the removal
-- of the person's unfinished uploads, and the rewriting of their name in the shared spaces' history.
-- A try that fails after them is tried again, and the next try finds that work done and counts
-- nothing for it. So each step's count is kept here with the work, and the transaction that deletes
-- the account reads the totals for the audit event. The rows go with the deletion's (the account's).

-- Unfinished uploads removed: raised in the transaction that deletes each upload's row.
ALTER TABLE account_deletions ADD COLUMN uploads_removed INTEGER NOT NULL DEFAULT 0 CHECK (uploads_removed >= 0);

-- Commits whose author was replaced, per shared space: written before the space's rewrite is
-- journaled (internal/knowledge, AnonymizeAuthorsNoting), and replaced by a later try's only when
-- that try finds commits still to rewrite (the earlier one stopped before its journal), so a
-- rewrite that a later try or the next start finishes is counted once.
CREATE TABLE purge_anonymized (
    user_id  INTEGER NOT NULL REFERENCES account_deletions (user_id) ON DELETE CASCADE,
    space_id INTEGER NOT NULL,
    commits  INTEGER NOT NULL CHECK (commits >= 0),
    PRIMARY KEY (user_id, space_id)
) STRICT;
