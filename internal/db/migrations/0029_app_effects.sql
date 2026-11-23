-- Platform effects of apps and their transactional outbox (internal/apps, effects.go and
-- outbox.go).
--
-- An action of an app may declare effects: a card on a board, a change to a card the app made
-- earlier, a document, a route to open. They are not done inside the request. The record's change
-- and one entry per effect that applies are committed in the same transaction (the outbox), and a
-- worker in the server does the entries afterwards, as the user who acted and with that user's
-- access at that time, so the work goes on when the phone is closed and a crash loses none of it.
--
-- app_operations is what makes a request safe to repeat: an action names an operation (the
-- client's idempotency key, or one the server makes), and a second request with the same
-- operation changes nothing and queues nothing. An entry is the operation's n-th effect and is
-- never queued twice.

CREATE TABLE app_operations (
    id           INTEGER PRIMARY KEY,
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- The user who acted, or the user the agent worked for.
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    operation    TEXT NOT NULL,
    action       TEXT NOT NULL,
    -- What the request asked for (a hash of the action and its context), so reusing an operation
    -- for another request is told apart from repeating one.
    request_hash TEXT NOT NULL,
    record_id    INTEGER REFERENCES app_rows (id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (app_id, user_id, operation)
) STRICT;

CREATE TABLE app_effects (
    id             INTEGER PRIMARY KEY,
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    operation_id   INTEGER NOT NULL REFERENCES app_operations (id) ON DELETE CASCADE,
    -- The effect's place in its action's list: with the operation, its stable identity.
    position       INTEGER NOT NULL CHECK (position >= 0),
    -- The effect's name in the definition (create_card's, which update_card refers to), or ''.
    name           TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL CHECK (kind IN ('create_card', 'update_card', 'create_document', 'open_route')),
    -- The record the action changed, and its revision once changed.
    record_id      INTEGER NOT NULL REFERENCES app_rows (id) ON DELETE CASCADE,
    record_revision INTEGER NOT NULL,
    -- Who acted, as the entry will act: the same user, marked as the agent's if the agent acted.
    user_id        INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    acted_by_agent INTEGER NOT NULL DEFAULT 0 CHECK (acted_by_agent IN (0, 1)),
    acted_via      TEXT,
    -- The kind of action ('insert' or 'update'), which decides the app right the user needs.
    action_kind    TEXT NOT NULL CHECK (action_kind IN ('insert', 'update')),
    -- The effect with its templates already filled in from the record, as JSON: what the worker
    -- does, never anything that was sent by the client.
    payload        TEXT NOT NULL CHECK (json_valid(payload) AND json_type(payload) = 'object'),
    state          TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'done', 'failed')),
    attempts       INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT NOT NULL,
    -- While 'running': until when the worker that took the entry owns it. A worker that died
    -- leaves a lease that runs out, and the entry is taken again.
    lease_until    TEXT,
    -- Why a failed entry failed: a code the web app translates, and its parameters.
    error_code     TEXT,
    error_params   TEXT CHECK (error_params IS NULL OR json_valid(error_params)),
    -- What a done entry made, as JSON (the card's ids, the document's path, the route).
    result         TEXT CHECK (result IS NULL OR (json_valid(result) AND json_type(result) = 'object')),
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    done_at        TEXT,
    UNIQUE (operation_id, position)
) STRICT;

-- The worker's queue.
CREATE INDEX app_effects_due ON app_effects (next_attempt_at) WHERE state IN ('pending', 'running');
-- An app's entries, and a record's.
CREATE INDEX app_effects_by_app ON app_effects (app_id, id DESC);
CREATE INDEX app_effects_by_record ON app_effects (record_id, id);

-- A card made by an app's effect remembers which entry made it, so doing the entry again (after a
-- crash between making the card and noting it) finds the card instead of making another. The
-- key is the server's: no request can set it.
ALTER TABLE cards ADD COLUMN origin TEXT;
CREATE UNIQUE INDEX cards_by_origin ON cards (board_id, origin) WHERE origin IS NOT NULL;
