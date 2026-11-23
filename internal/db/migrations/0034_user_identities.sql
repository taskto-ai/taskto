-- Single sign-on (docs/sso.md): the identity an OpenID Connect provider vouches for, linked to
-- a user. An identity is the provider's issuer and the user's subject there, never the email,
-- which a provider may let people change. Deleting the user deletes their links.

CREATE TABLE user_identities (
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (issuer, subject)
) STRICT;

CREATE INDEX user_identities_by_user ON user_identities (user_id);
