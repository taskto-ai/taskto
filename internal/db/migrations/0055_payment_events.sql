-- Applying verified payment events to records (internal/integrations apply.go).
--
-- What a ref learns when its payment arrives. Both are set by the one statement that moves the ref
-- from unpaid to paid, so they are never there without the state (and an event that is applied twice,
-- as after a purge, finds them and changes nothing).
--
-- paid_object_id is the provider's object the payment arrived on (a Checkout Session) and payment_id
-- the provider's payment (a payment intent), from that verified event. A later paid event is the same
-- payment told again when both name the same payment, or, when one of them names none, the same
-- object; anything else is a second payment of the same ref. A refund carries no token of ours, only
-- the payment, and is mapped to the ref through payment_id. A payment belongs to one ref, so the pair
-- is unique.
ALTER TABLE integration_refs ADD COLUMN paid_object_id TEXT CHECK (paid_object_id IS NULL OR length(paid_object_id) BETWEEN 1 AND 255);
ALTER TABLE integration_refs ADD COLUMN payment_id TEXT CHECK (payment_id IS NULL OR length(payment_id) BETWEEN 1 AND 255);

CREATE UNIQUE INDEX integration_refs_by_payment ON integration_refs (integration_id, payment_id) WHERE payment_id IS NOT NULL;

-- Payments that arrived for a ref that was paid already, by another payment: the case the payment
-- link's notes describe (a crash with no stored id, then a day of waiting, leaves two payable links for
-- one ref). The second payment is money the payer is owed back or the admin must reconcile, so it is
-- kept for as long as the ref is, and not only while the event is (events are purged). One row per
-- payment: payment_key is the provider's payment id, or the object's when the event named no payment
-- (or, with neither, the event's own id), so the same event told again adds nothing and two payments
-- that both lack an object are two rows. No record data is here, only the provider's ids and the
-- amount.
CREATE TABLE integration_ref_duplicates (
    ref_id      INTEGER NOT NULL REFERENCES integration_refs (id) ON DELETE CASCADE,
    payment_key TEXT NOT NULL CHECK (length(payment_key) BETWEEN 1 AND 300),
    object_id   TEXT CHECK (object_id IS NULL OR length(object_id) BETWEEN 1 AND 255),
    payment_id  TEXT CHECK (payment_id IS NULL OR length(payment_id) BETWEEN 1 AND 255),
    amount_minor INTEGER NOT NULL CHECK (amount_minor >= 0),
    currency    TEXT NOT NULL CHECK (length(currency) = 3),
    recorded_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (ref_id, payment_key)
) STRICT, WITHOUT ROWID;

-- The admins' notifications of events' ends, owed and not written yet: one per event and claim (lease,
-- the notification's dedupe key), so an end told after an earlier one never replaces the earlier one's
-- debt. The end state and its debt are one transaction (the worker's, or the apply's own), and the
-- notification is written by a step of its own (events.go, sendEventNotices), so a notification that
-- can't be written never holds the event back, nor the events after it. due_at is when the step next
-- tries, moved on after a failure; the row goes once the notification is written. An event that owes
-- a notification is not purged.
CREATE TABLE integration_event_notices (
    event_id INTEGER NOT NULL REFERENCES integration_events (id) ON DELETE CASCADE,
    lease    INTEGER NOT NULL,
    due_at   TEXT NOT NULL,
    PRIMARY KEY (event_id, lease)
) STRICT, WITHOUT ROWID;

CREATE INDEX integration_event_notices_due ON integration_event_notices (due_at);
