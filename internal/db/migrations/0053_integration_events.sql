-- Verified provider events (docs/integrations.md): what an integration's webhook route accepted,
-- kept once and in normalized form, for the worker that applies them.
--
-- The raw body is NEVER stored. A provider's event carries a payer's name, email and card
-- summary, and "no card data in TaskTo" is simplest to keep by never writing the body: what is
-- kept is the closed, typed fields the provider's adapter read (internal/integrations,
-- Normalize) and a SHA-256 of the body, which support can compare with what the provider shows.
-- A text a payer typed at the provider can't reach a record through this table: there is no
-- column for it.
CREATE TABLE integration_events (
    -- AUTOINCREMENT: the worker takes events in the order they were accepted.
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    integration_id      INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    -- The provider's id of the event (Stripe's evt_...), validated by the adapter. A second
    -- delivery of the same event, a replay or a provider's retry, finds this row and is
    -- answered like the first: it is stored once.
    provider_event_id   TEXT NOT NULL CHECK (length(provider_event_id) BETWEEN 1 AND 255),
    -- 'webhook', or 'poll' for the events reconciliation makes from what it reads at the provider.
    source              TEXT NOT NULL CHECK (source IN ('webhook', 'poll')),
    -- The normalized event ('payment.succeeded', 'payment.expired', 'payment.failed',
    -- 'payment.refunded'): the closed list is the adapter's, not a CHECK, so another kind of
    -- integration doesn't need this table rebuilt.
    kind                TEXT NOT NULL CHECK (length(kind) BETWEEN 1 AND 64),
    -- The token of ours that the provider echoed back (a hex string of 128 random bits), or NULL
    -- for an event that carries none, such as a refund: the worker maps those through
    -- provider_payment_id. Not a foreign key: the refs it names are the next card's table.
    ref_token           TEXT CHECK (ref_token IS NULL OR length(ref_token) = 32),
    -- The provider's id of the object the event is about (a Checkout Session, a charge) and of
    -- the payment it belongs to (a payment intent), both validated identifiers.
    object_id           TEXT CHECK (object_id IS NULL OR length(object_id) BETWEEN 1 AND 255),
    provider_payment_id TEXT CHECK (provider_payment_id IS NULL OR length(provider_payment_id) BETWEEN 1 AND 255),
    -- In the currency's smallest unit; for a refund, what has been refunded so far.
    amount_minor        INTEGER NOT NULL CHECK (amount_minor >= 0),
    -- ISO 4217, upper case.
    currency            TEXT NOT NULL CHECK (length(currency) = 3),
    -- Whether the event is of live money (1) or of the provider's test mode (0).
    livemode            INTEGER NOT NULL DEFAULT 0 CHECK (livemode IN (0, 1)),
    -- SHA-256 of the exact body, hex.
    body_sha256         TEXT NOT NULL CHECK (length(body_sha256) = 64),
    -- When the provider says the event happened, and when the server took it in.
    occurred_at         TEXT NOT NULL,
    received_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- The worker's lifecycle, as the outbox's (app_effects): 'pending' waits for a worker,
    -- 'running' is leased until lease_until, 'applied' and 'ignored' are done and 'failed' has
    -- given up (or the apply step refused it) and waits for an admin.
    state               TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'running', 'applied', 'ignored', 'failed')),
    attempts            INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    -- Counts the claims: a worker that lost its lease can't note a result for a later claim.
    lease               INTEGER NOT NULL DEFAULT 0,
    lease_until         TEXT,
    next_attempt_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- Why the event isn't applied: a code of internal/apierr and its params (JSON), never text
    -- from the provider.
    error_code          TEXT,
    error_params        TEXT CHECK (error_params IS NULL OR json_valid(error_params)),
    processed_at        TEXT,
    UNIQUE (integration_id, provider_event_id)
) STRICT;

-- The worker's next event: the ones that wait or whose lease ran out.
CREATE INDEX integration_events_due ON integration_events (state, next_attempt_at);

-- The purge: finished events by age.
CREATE INDEX integration_events_by_received ON integration_events (received_at);

-- A keyed fingerprint of each webhook secret (current and previous), so that whether another
-- integration holds the same secret is an index lookup and not a scan that opens every sealed
-- secret under the write lock. It is an HMAC of the secret under a key derived from the server
-- key (internal/serverkey), never the secret and never a plain hash: it says nothing of the
-- secret to whoever reads the database without the key. NULL for an API key, and for a webhook
-- secret saved before this column existed, which the server fills in when it starts and before
-- it checks a secret for reuse.
--
-- The index is not UNIQUE: a database from before this check can hold the same secret twice,
-- and the route refuses both (it verifies with neither) instead of the migration failing. A
-- previous secret stops counting when it expires, which an index can't express; the writes
-- check under the write lock that no other unexpired secret has the fingerprint.
ALTER TABLE integration_secrets ADD COLUMN fingerprint BLOB CHECK (fingerprint IS NULL OR length(fingerprint) = 32);

CREATE INDEX integration_secrets_by_fingerprint ON integration_secrets (fingerprint) WHERE fingerprint IS NOT NULL;
