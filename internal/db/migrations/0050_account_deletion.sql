-- Deleting an account (docs/api.md#deleting-an-account, docs/install.md#privacy-and-account-deletion).
--
-- A person deletes their own account (nobody else can), and the account is switched off at once,
-- with the disabling of docs/api.md#disabling-a-member (users.disabled_at), and kept for a grace
-- period. A row here says the switch-off is a deletion: it names when the account is purged.
-- Cancelling the deletion removes the row and switches the account back on;
-- the purge (internal/accountdeletion, run by the scheduler) deletes what the person owns alone
-- (their private spaces, conversations, CLI logins, tokens, sessions, ...) and then the account,
-- and the rows of everything shared that points at it become "a deleted user" through the schema's
-- ON DELETE SET NULL, as they always have.
--
-- The row goes with the account (ON DELETE CASCADE).
CREATE TABLE account_deletions (
    user_id      INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    requested_at TEXT NOT NULL,
    purge_at     TEXT NOT NULL,
    -- Whether the account was disabled (by an admin) before it was deleted: cancelling the deletion
    -- leaves it as it was then.
    was_disabled INTEGER NOT NULL DEFAULT 0 CHECK (was_disabled IN (0, 1)),
    -- Set when the purge begins deleting what the person owned: from then on the deletion can't be
    -- cancelled, since part of it can't be undone.
    purge_started_at TEXT
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

-- A user's id is never given to another account. SQLite hands the next id as the largest one in the
-- table plus one, so after the newest account is purged its id would go to whoever registers next,
-- and what is kept by id would be theirs: the browser's drafts and offline records of the old
-- account, and the mentions of it in cards' comments. The mark is raised in the transaction that
-- deletes a user, whatever deletes it, and a new account takes an id above it (internal/auth,
-- insertUser), the way app_row_ids does for the records of apps.
CREATE TABLE user_ids (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    high INTEGER NOT NULL CHECK (high >= 0)
) STRICT;

INSERT INTO user_ids (id, high) VALUES (1, coalesce((SELECT max(id) FROM users), 0));

CREATE TRIGGER users_remember_id
BEFORE DELETE ON users
BEGIN
    UPDATE user_ids SET high = max(high, OLD.id) WHERE id = 1;
END;

-- The purge's own steps that can't be done in the transaction that deletes the account, because
-- they are about files and other processes: the agent runtime's records of a conversation (its
-- database keeps what was said until the server asks for it to be purged), the CLI worker's
-- directory of the person, and the providers file the agent runtime reads, which holds the
-- person's provider accounts and their keys. A row is made before the work it names and removed
-- when the work is done, so a step that couldn't be done (the agent runtime or the CLI worker is
-- off or isn't answering) is made again by the next try, by the next start, and while the server
-- runs. A row doesn't name the account by a foreign key: it outlives the account, and user ids
-- are never reused (user_ids).
CREATE TABLE purge_cleanups (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL,
    -- agent_conversation: target is the agent runtime's conversation id;
    -- cli_home: the user's id, for the CLI worker's directory;
    -- providers: 'feed:' and the user's id, for the providers file and the key files, one row
    -- for each account so that no run's acknowledgement clears another account's.
    kind       TEXT NOT NULL CHECK (kind IN ('agent_conversation', 'cli_home', 'providers')),
    target     TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    attempts   INTEGER NOT NULL DEFAULT 0,
    -- Why the last try failed, for the operator; never holds a secret.
    last_error TEXT,
    UNIQUE (kind, target)
) STRICT;

-- The purge of an account waiting to be deleted is the server's work and doesn't depend on any
-- membership: a job that goes when the person leaves a workspace (the trigger of migration 0038)
-- would be lost by removing them while their purge was failing, and not tried again until the
-- server restarted. Those jobs stay.
DROP TRIGGER memberships_drop_jobs;
CREATE TRIGGER memberships_drop_jobs
AFTER DELETE ON memberships
BEGIN
    DELETE FROM scheduled_jobs WHERE user_id = OLD.user_id AND workspace_id = OLD.workspace_id AND kind <> 'account.purge';
END;

-- What the effects of apps did for a person is a record of the shared app, not the person's: when
-- their account is purged, the finished entries and the receipts of operations stay, without who.
-- user_id was NOT NULL with ON DELETE CASCADE, which took them with the account. Both tables are
-- rebuilt with user_id optional and ON DELETE SET NULL (an entry still waiting is removed by the
-- purge, which can't act for no one). The numbers are kept as they were: both tables number their
-- rows with AUTOINCREMENT, and the sequence is moved to the new tables.
CREATE TABLE app_operations_new (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    user_id      INTEGER REFERENCES users (id) ON DELETE SET NULL,
    operation    TEXT NOT NULL,
    action       TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    record_id    INTEGER REFERENCES app_rows (id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (app_id, user_id, operation)
) STRICT;

CREATE TABLE app_effects_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    operation_id   INTEGER NOT NULL REFERENCES app_operations_new (id) ON DELETE CASCADE,
    position       INTEGER NOT NULL CHECK (position >= 0),
    name           TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL CHECK (kind IN ('create_card', 'update_card', 'create_document', 'open_route')),
    record_id      INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    record_revision INTEGER NOT NULL,
    user_id        INTEGER REFERENCES users (id) ON DELETE SET NULL,
    acted_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (acted_by_agent IN (0, 1)),
    acted_via      TEXT,
    action_kind    TEXT NOT NULL CHECK (action_kind IN ('insert', 'update')),
    payload        TEXT NOT NULL CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    state          TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'done', 'failed')),
    attempts       INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease          INTEGER NOT NULL DEFAULT 0 CHECK (lease >= 0),
    next_attempt_at TEXT NOT NULL,
    lease_until    TEXT,
    error_code     TEXT,
    error_params   TEXT CHECK (error_params IS NULL OR json_valid(error_params)),
    result         TEXT CHECK (result IS NULL OR (json_valid(result) AND json_type(result) = 'object')),
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    done_at        TEXT,
    UNIQUE (operation_id, position)
) STRICT;

INSERT INTO app_operations_new (id, app_id, user_id, operation, action, request_hash, record_id, created_at)
    SELECT id, app_id, user_id, operation, action, request_hash, record_id, created_at FROM app_operations;
INSERT INTO app_effects_new (id, app_id, operation_id, position, name, kind, record_id, record_revision, user_id,
        acted_by_agent, acted_via, action_kind, payload, state, attempts, lease, next_attempt_at, lease_until,
        error_code, error_params, result, created_at, updated_at, done_at)
    SELECT id, app_id, operation_id, position, name, kind, record_id, record_revision, user_id,
        acted_by_agent, acted_via, action_kind, payload, state, attempts, lease, next_attempt_at, lease_until,
        error_code, error_params, result, created_at, updated_at, done_at FROM app_effects;

-- The numbers given so far, which the rows left may not show (cleaned up ones are never given again).
DELETE FROM sqlite_sequence WHERE name IN ('app_operations_new', 'app_effects_new');
UPDATE sqlite_sequence SET name = 'app_operations_new' WHERE name = 'app_operations';
UPDATE sqlite_sequence SET name = 'app_effects_new' WHERE name = 'app_effects';

-- The entries first: they refer to the operations.
DROP TABLE app_effects;
DROP TABLE app_operations;
ALTER TABLE app_operations_new RENAME TO app_operations;
ALTER TABLE app_effects_new RENAME TO app_effects;

CREATE INDEX app_operations_by_record ON app_operations (record_id) WHERE record_id IS NOT NULL;
CREATE INDEX app_effects_due ON app_effects (next_attempt_at) WHERE state IN ('pending', 'running');
CREATE INDEX app_effects_by_app ON app_effects (app_id, id DESC);
CREATE INDEX app_effects_by_record ON app_effects (record_id, id);
