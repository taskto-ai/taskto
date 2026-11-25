-- Integrations (docs/integrations.md): a workspace admin's connection to an account at an
-- outside provider (the first kind is payments: Stripe, Square). The provider is a closed
-- registry in Go, so a new provider is not a migration; what is stored is only the
-- connection: its settings, the limits the admin sets, the spaces that may use it and its
-- sealed secrets.
--
-- Nothing in an app's definition names a row of these tables. An app declares a slot of a kind
-- and an admin binds it (a later migration holds those approvals).

CREATE TABLE integrations (
    id           INTEGER PRIMARY KEY,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- 128 random bits in hex: the only thing in the webhook's address, so it can't be guessed.
    public_id    TEXT NOT NULL UNIQUE CHECK (length(public_id) = 32),
    -- The registry's id ("stripe", "square"), checked in Go.
    provider     TEXT NOT NULL CHECK (length(provider) BETWEEN 1 AND 40),
    -- A slug, unique in the workspace and never changed; display_name is what people read.
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
    display_name TEXT NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
    -- Whether the credentials move real money: read from the key's prefix or the settings.
    mode         TEXT NOT NULL CHECK (mode IN ('test', 'live')),
    -- Not secret, a JSON object the provider's adapter validated (Square: location, environment).
    settings     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(settings) AND json_type(settings) = 'object'),
    -- The limits of one payment, set by the admin and never by an app: a ceiling in the currency's
    -- smallest unit, and the currencies (a JSON array of ISO 4217 codes) that may be asked for.
    max_amount_minor INTEGER NOT NULL CHECK (max_amount_minor > 0),
    currencies   TEXT NOT NULL CHECK (json_valid(currencies) AND json_type(currencies) = 'array' AND json_array_length(currencies) > 0),
    -- The address the admin was told to register at the provider, when the server had a public
    -- URL; '' when it had none (payments are then reconciled by polling).
    webhook_url  TEXT NOT NULL DEFAULT '',
    -- Set while an admin has switched the integration off: nothing sends or receives, and
    -- its approvals and events are kept.
    disabled_at  TEXT,
    disabled_by  INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- When the last verified event arrived; written by the webhook and polling code.
    last_event_at TEXT,
    UNIQUE (workspace_id, name),
    CHECK (disabled_by IS NULL OR disabled_at IS NOT NULL)
) STRICT;

CREATE INDEX integrations_by_workspace ON integrations (workspace_id, id);

-- The secrets of an integration, sealed with the server key (internal/serverkey) under
-- associated data that names the integration and the secret, so a sealed value copied to
-- another row or name doesn't open. Write-only: nothing reads one back to a person.
--
-- name is 'api_key', 'webhook_secret' or 'webhook_secret_previous'. When a webhook secret is
-- replaced the old one is kept as the previous one until expires_at, so events signed before
-- the admin changed it at the provider still verify; NULL for the others.
CREATE TABLE integration_secrets (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    name           TEXT NOT NULL CHECK (name IN ('api_key', 'webhook_secret', 'webhook_secret_previous')),
    sealed         BLOB NOT NULL,
    set_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at     TEXT,
    PRIMARY KEY (integration_id, name)
) STRICT, WITHOUT ROWID;

-- The spaces whose apps may use an integration. A new integration has none: until an admin
-- allows a space, no app there can use it.
CREATE TABLE integration_spaces (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    space_id       INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    allowed_by     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    allowed_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (integration_id, space_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX integration_spaces_by_space ON integration_spaces (space_id);

-- A space is in the integration's workspace: an integration of one workspace is never used in
-- another's.
CREATE TRIGGER integration_spaces_same_workspace
BEFORE INSERT ON integration_spaces
WHEN (SELECT workspace_id FROM integrations WHERE id = NEW.integration_id)
     IS NOT (SELECT workspace_id FROM spaces WHERE id = NEW.space_id)
BEGIN
    SELECT RAISE(ABORT, 'an integration is used only in its own workspace''s spaces');
END;

-- An integration never moves to another workspace, and its public id and name never change.
CREATE TRIGGER integrations_fixed
BEFORE UPDATE OF workspace_id, public_id, name, provider ON integrations
WHEN NEW.workspace_id IS NOT OLD.workspace_id OR NEW.public_id IS NOT OLD.public_id
  OR NEW.name IS NOT OLD.name OR NEW.provider IS NOT OLD.provider
BEGIN
    SELECT RAISE(ABORT, 'an integration''s workspace, public id, name and provider never change');
END;
