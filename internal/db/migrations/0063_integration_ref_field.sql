-- A payment link's reference remembers the record's field its link is written to (internal/integrations
-- paymentlink.go).
--
-- A record asks for its payment link again when an action runs the effect again (the invoice was
-- sent again, or its expired link replaced): a new outbox entry, which until now made a reference of
-- its own and a new link while the record's earlier link might still take payment. Before it makes a
-- new one, an entry now reads the record's newest reference for the same field: one that may still be
-- paid, is still being made or whose outcome is unknown is used again (the same link, nothing new),
-- and only one confirmed expired, or paid, lets a new link be made. The entries themselves are cleaned
-- up (a done one after 30 days) while a link may stay open, so the field is kept on the reference.
--
-- A reference made before this migration takes the field from its entry, if the entry is still kept;
-- one whose entry is gone keeps '', which the lookup counts as any field of its record (never
-- fewer links looked at than there are).
ALTER TABLE integration_refs ADD COLUMN url_field TEXT NOT NULL DEFAULT '';

UPDATE integration_refs SET url_field = coalesce((
    SELECT json_extract(e.payload, '$.url_field') FROM app_effects e WHERE e.id = integration_refs.effect_id), '')
WHERE effect_id IS NOT NULL;
