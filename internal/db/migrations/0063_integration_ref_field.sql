-- What a payment link's reference keeps so that an invoice has one payable link at a time, and so
-- that its money stays as it is while that link may be paid (internal/integrations/paymentlink.go,
-- internal/apps/paymentlink.go).
--
-- A record asks for its payment link again when an action runs the effect again (the invoice was
-- sent again, or its expired link replaced): a new outbox entry, which until now made a reference of
-- its own and a new link while the record's earlier link might still take payment. Before it makes a
-- new one, an entry now reads every reference of the record (whatever field, slot or integration it
-- was made for) that may still be paid: one that is open is given again, one still being made or
-- whose outcome is unknown is taken over, and only when each is confirmed expired, or paid, is a new
-- link made. That needs nothing new here: the record is the key.

-- The record's fields the reference's money was read from: while its link may be paid, the record's
-- amount and currency can't change, and a release that no longer asks for links doesn't free them.
-- A reference made before this migration takes them from its entry, if the entry is still kept; one
-- whose entry is gone keeps '' (and '' is an effect's own currency too). A reference whose amount
-- field is '' locks every field any release of its app read money from (apps/paymentlink.go).
ALTER TABLE integration_refs ADD COLUMN amount_field TEXT NOT NULL DEFAULT '';
ALTER TABLE integration_refs ADD COLUMN currency_field TEXT NOT NULL DEFAULT '';

UPDATE integration_refs SET
    amount_field = coalesce((SELECT json_extract(e.payload, '$.amount_field') FROM app_effects e WHERE e.id = integration_refs.effect_id), ''),
    currency_field = coalesce((SELECT json_extract(e.payload, '$.currency_field') FROM app_effects e WHERE e.id = integration_refs.effect_id), '')
WHERE effect_id IS NOT NULL;

-- The entry whose slot the reference has (NULL: its own entry's). An entry that gives the record's
-- link again moves the reference to its slot, and only a newer entry than the one that last did may.
ALTER TABLE integration_refs ADD COLUMN slot_effect_id INTEGER;

-- When the reference's own link was seen expired, kept apart from its state: a reference paid on
-- another key's object stays paid, and its link that expired since can't be paid either.
ALTER TABLE integration_refs ADD COLUMN link_expired_at TEXT;

-- The provider's link a payment of the reference came through, when that is not the object the event
-- is about: a Stripe Payment Link (plink_...) is paid through a Checkout Session of its own, and the
-- event names the session. A one-use link paid so is used up, and its money is free again.
ALTER TABLE integration_refs ADD COLUMN paid_link_id TEXT;
ALTER TABLE integration_events ADD COLUMN link_id TEXT;
