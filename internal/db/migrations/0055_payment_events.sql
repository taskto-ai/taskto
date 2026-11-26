-- Applying verified payment events to records (internal/integrations apply.go).
--
-- What a ref learns when its payment arrives. Both are set by the one statement that moves the ref
-- from unpaid to paid, so they are never there without the state (and an event that is applied twice,
-- as after a purge, finds them and changes nothing).
--
-- paid_object_id is the provider's object the payment arrived on (a Checkout Session): a later paid
-- event for the same object is the same payment told again, and one for another object is a second
-- payment of the same ref. payment_id is the provider's payment (a payment intent) from that verified
-- event: a refund carries no token of ours, only the payment, and is mapped to the ref through it. A
-- payment belongs to one ref, so the pair is unique.
ALTER TABLE integration_refs ADD COLUMN paid_object_id TEXT CHECK (paid_object_id IS NULL OR length(paid_object_id) BETWEEN 1 AND 255);
ALTER TABLE integration_refs ADD COLUMN payment_id TEXT CHECK (payment_id IS NULL OR length(payment_id) BETWEEN 1 AND 255);

CREATE UNIQUE INDEX integration_refs_by_payment ON integration_refs (integration_id, payment_id) WHERE payment_id IS NOT NULL;

-- Payments that arrived for a ref that was paid already, on another object: the case the payment link's
-- notes describe (a crash with no stored id, then a day of waiting, leaves two payable links for one
-- ref). The second payment is money the payer is owed back or the admin must reconcile, so it is kept
-- for as long as the ref is, and not only while the event is (events are purged). One row per object:
-- the same event told again adds nothing. No record data is here, only the provider's ids and the
-- amount.
CREATE TABLE integration_ref_duplicates (
    ref_id      INTEGER NOT NULL REFERENCES integration_refs (id) ON DELETE CASCADE,
    object_id   TEXT NOT NULL CHECK (length(object_id) BETWEEN 1 AND 255),
    payment_id  TEXT CHECK (payment_id IS NULL OR length(payment_id) BETWEEN 1 AND 255),
    amount_minor INTEGER NOT NULL CHECK (amount_minor >= 0),
    currency    TEXT NOT NULL CHECK (length(currency) = 3),
    recorded_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (ref_id, object_id)
) STRICT, WITHOUT ROWID;
