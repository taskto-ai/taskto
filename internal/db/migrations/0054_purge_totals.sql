-- What a purge removed, kept across its tries (docs/audit.md, account_purged).
--
-- Some of the purge's steps are done before the transaction that deletes the account: the removal
-- of the person's unfinished uploads, and the rewriting of their name in the shared spaces' history.
-- A try that fails after them is tried again, and the next try finds that work done and counts
-- nothing for it. Each step adds what it did here as soon as it is done, and the transaction that
-- deletes the account reads the totals for the audit event. The row goes with the account.
ALTER TABLE account_deletions ADD COLUMN uploads_removed INTEGER NOT NULL DEFAULT 0 CHECK (uploads_removed >= 0);
ALTER TABLE account_deletions ADD COLUMN documents_anonymized INTEGER NOT NULL DEFAULT 0 CHECK (documents_anonymized >= 0);
