-- The webhook address an integration's signing secret was saved for (internal/integrations).
--
-- An admin registers the address the server shows (the public URL, /hooks/ and the public id) at the
-- provider and saves the signing secret the provider gives for it. Square signs each delivery over
-- that exact address and the body, so a delivery to it verifies only against the address it was
-- registered with: a change of the server's public URL afterwards leaves the provider signing over
-- the old one. The server keeps the address it showed when the secret was saved, verifies Square's
-- deliveries against it (and against the current one), and tells the admin when the two differ.
--
-- NULL when the secret was saved without a public URL, or before this column existed: there is then
-- nothing to compare, and the current address is the only one.
ALTER TABLE integrations ADD COLUMN webhook_registered_url TEXT
    CHECK (webhook_registered_url IS NULL OR length(webhook_registered_url) BETWEEN 1 AND 2048);
