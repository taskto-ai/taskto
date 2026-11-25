-- Deleting an account (docs/api.md#deleting-an-account, docs/install.md#privacy-and-account-deletion).
--
-- A person deletes their own account, or an admin deletes a member's, and the account is switched
-- off at once, with the disabling of docs/api.md#disabling-a-member (users.disabled_at), and kept
-- for a grace period. A row here says the switch-off is a deletion: it names when the account is
-- purged and who asked. Cancelling the deletion removes the row and switches the account back on;
-- the purge (internal/accountdeletion, run by the scheduler) deletes what the person owns alone
-- (their private spaces, conversations, CLI logins, tokens, sessions, ...) and then the account,
-- and the rows of everything shared that points at it become "a deleted user" through the schema's
-- ON DELETE SET NULL, as they always have.
--
-- The row goes with the account (ON DELETE CASCADE), and requested_by with the admin who asked if
-- that person is deleted before the purge.
CREATE TABLE account_deletions (
    user_id      INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    requested_at TEXT NOT NULL,
    purge_at     TEXT NOT NULL,
    -- The admin who deleted the account for the person; NULL for a person who deleted their own.
    requested_by INTEGER REFERENCES users (id) ON DELETE SET NULL,
    by_self      INTEGER NOT NULL CHECK (by_self IN (0, 1)),
    -- Whether the account was disabled (by an admin) before it was deleted: cancelling the deletion
    -- leaves it as it was then.
    was_disabled INTEGER NOT NULL DEFAULT 0 CHECK (was_disabled IN (0, 1)),
    -- Set when the purge begins deleting what the person owned: from then on the deletion can't be
    -- cancelled, since part of it can't be undone.
    purge_started_at TEXT,
    CHECK (by_self = 0 OR requested_by IS NULL)
) STRICT;

CREATE INDEX account_deletions_by_purge ON account_deletions (purge_at);

-- The audit log (docs/audit.md): an event whose actor's account was deleted has no actor
-- (actor_id is set NULL with the account), which reads like an anonymous attempt. These say that it
-- was a deleted account instead, so the log shows "a deleted user" where it shows a name, and an
-- event about the account itself (its deletion) says so too. Neither holds a name or an address:
-- the purge sets them, in the transaction that deletes the account.
ALTER TABLE audit_events ADD COLUMN actor_deleted INTEGER NOT NULL DEFAULT 0 CHECK (actor_deleted IN (0, 1));
ALTER TABLE audit_events ADD COLUMN target_deleted INTEGER NOT NULL DEFAULT 0 CHECK (target_deleted IN (0, 1));

-- A card whose assignee's account was deleted has no assignee (assignee_id is set NULL with the
-- account), which reads like a card nobody is assigned to. This says it was a deleted account's, so
-- the card shows "Deleted user"; assigning the card to anyone clears it.
ALTER TABLE cards ADD COLUMN assignee_deleted INTEGER NOT NULL DEFAULT 0 CHECK (assignee_deleted IN (0, 1));
