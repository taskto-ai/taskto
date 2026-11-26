-- Payment links (internal/apps effects.go, internal/integrations paymentlink.go).
--
-- An app's effect create_payment_link asks an integration of the payments kind for a link to pay
-- an amount of a record. Its outbox entry is a row of app_effects like the other kinds', so the
-- table's kind CHECK has to admit it; SQLite can't change a CHECK in place, so the table is
-- rebuilt. Nothing refers to app_effects (cards remember which entry made them by its number, in
-- a column of their own), so no row of another table is touched by dropping it.
--
-- Its rows are numbered with AUTOINCREMENT, and the number must never be given again after its
-- row is cleaned up: cards and documents remember which entry made them by the entry's number,
-- and a payment link's reference is found by it. A rebuilt table starts its own sequence at the
-- highest number it holds, which is below the old one when the newest entries were cleaned up, so
-- the old sequence is carried over.
CREATE TABLE app_effects_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    operation_id   INTEGER NOT NULL REFERENCES app_operations (id) ON DELETE CASCADE,
    position       INTEGER NOT NULL CHECK (position >= 0),
    name           TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL CHECK (kind IN ('create_card', 'update_card', 'create_document', 'open_route', 'create_payment_link')),
    record_id      INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    record_revision INTEGER NOT NULL,
    user_id        INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
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

INSERT INTO app_effects_new (id, app_id, operation_id, position, name, kind, record_id, record_revision, user_id,
        acted_by_agent, acted_via, action_kind, payload, state, attempts, lease, next_attempt_at, lease_until,
        error_code, error_params, result, created_at, updated_at, done_at)
    SELECT id, app_id, operation_id, position, name, kind, record_id, record_revision, user_id,
        acted_by_agent, acted_via, action_kind, payload, state, attempts, lease, next_attempt_at, lease_until,
        error_code, error_params, result, created_at, updated_at, done_at
    FROM app_effects;

-- The sequence of the old table, which only grows, becomes the new one's (a table that never held a
-- row has none).
DELETE FROM sqlite_sequence WHERE name = 'app_effects_new';
INSERT INTO sqlite_sequence (name, seq) SELECT 'app_effects_new', seq FROM sqlite_sequence WHERE name = 'app_effects';

DROP TABLE app_effects;
ALTER TABLE app_effects_new RENAME TO app_effects;

-- The worker's queue.
CREATE INDEX app_effects_due ON app_effects (next_attempt_at) WHERE state IN ('pending', 'running');
-- An app's entries, and a record's.
CREATE INDEX app_effects_by_app ON app_effects (app_id, id DESC);
CREATE INDEX app_effects_by_record ON app_effects (record_id, id);

-- One row per payment link the server asked a provider for.
--
-- token is 128 random bits: the idempotency key of the call that makes the link and the reference
-- the provider echoes back with the payment, so a payment is found by what the server issued and
-- never by anything the provider's side says of a record. effect_id is unique: an outbox entry has
-- one reference, so an entry done again after a crash finds it and never makes a second link. The
-- entry may be cleaned up while the payment is still open, so the reference outlives it: effect_id
-- is only forgotten then (an entry's number is never given again).
--
-- Deleting an app or a record deletes its references with it, as they take their payments' history
-- with them. An integration that made links can't be deleted (the store answers that it is in
-- use: switch it off instead), so a payment still open never loses the account it is settled at.
CREATE TABLE integration_refs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    token              TEXT NOT NULL UNIQUE CHECK (length(token) = 32),
    integration_id     INTEGER NOT NULL REFERENCES integrations (id),
    app_id             INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    record_id          INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    slot               TEXT NOT NULL,
    effect_id          INTEGER UNIQUE REFERENCES app_effects (id) ON DELETE SET NULL,
    -- What the provider made, once it has answered: its object's id and the address the payer opens.
    provider_object_id TEXT,
    url                TEXT,
    -- The amount asked for in the currency's smallest unit (exact, never rounded) and its currency.
    amount_minor       INTEGER NOT NULL CHECK (amount_minor > 0),
    currency           TEXT NOT NULL CHECK (length(currency) = 3),
    -- How many times the provider answered with an error that it may have kept under the key: the
    -- next call uses a key of its own (a provider that saved a failure answers it again to the same
    -- key), so a call whose answer never came back is the only one that is repeated under its key.
    key_seq            INTEGER NOT NULL DEFAULT 0 CHECK (key_seq >= 0),
    -- The highest key_seq a call was made under (-1: none yet). A key a call was made under may hold the
    -- provider's saved answer to it, a failure or an object that has expired since, which the same key
    -- would answer again, so a try that found no object of the reference moves on from a key that was
    -- used and not from one that was not.
    key_called_seq     INTEGER NOT NULL DEFAULT -1 CHECK (key_called_seq >= -1),
    -- The provider's account the reference was made under (read from the provider with the integration's
    -- key when the reference was made) and the mode of that key (test or live: one account has both,
    -- with the same id, and what a test key made is not in the live objects), and the shape of the
    -- request: the two pages the payer returns
    -- to, both empty for a link whose payer stays on the provider's own confirmation. A provider
    -- keeps a call's key only for its own parameters, and finds what a lost call made only in the
    -- account and among the kind of objects it was made as, so a retry after a restart, a changed
    -- public address or another account's key must ask what the first try asked, and nowhere else.
    account_id         TEXT NOT NULL DEFAULT '',
    account_mode       TEXT NOT NULL DEFAULT '',
    success_url        TEXT NOT NULL DEFAULT '',
    cancel_url         TEXT NOT NULL DEFAULT '',
    -- Only forward: creating, then open, then one of paid, expired, failed (and paid, then refunded).
    state              TEXT NOT NULL DEFAULT 'creating' CHECK (state IN ('creating', 'open', 'paid', 'expired', 'failed', 'refunded')),
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    settled_at         TEXT,
    -- For the reconciliation by polling that reads open links' state.
    next_poll_at       TEXT,
    polls              INTEGER NOT NULL DEFAULT 0 CHECK (polls >= 0),
    UNIQUE (integration_id, provider_object_id)
) STRICT;

-- The newest create_payment_link entry of each field a link is written to, by app, record and field. An
-- entry is superseded when a newer one has the field, and the outbox's entries are cleaned up (a
-- done one after 30 days), so what supersedes cannot rest on them: this row stays as long as the
-- record does, as app_effect_cards does for cards. Written when an entry is queued, which only moves
-- it forward.
CREATE TABLE app_payment_link_latest (
    app_id    INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    record_id INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    field     TEXT NOT NULL,
    effect_id INTEGER NOT NULL,
    PRIMARY KEY (app_id, record_id, field)
) STRICT;

CREATE INDEX app_payment_link_latest_by_record ON app_payment_link_latest (record_id);

CREATE INDEX integration_refs_by_record ON integration_refs (record_id);
CREATE INDEX integration_refs_by_app ON integration_refs (app_id);
CREATE INDEX integration_refs_by_poll ON integration_refs (next_poll_at) WHERE state = 'open';
