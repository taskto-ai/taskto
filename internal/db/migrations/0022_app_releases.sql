-- App drafts and immutable releases (internal/apps). Until now an app was one definition that
-- every change replaced, and the app everyone used was whatever was saved last. From here:
--
--   - apps.definition is the app's DRAFT: what builders (and the agent) edit. Editing it never
--     changes what people use.
--   - app_releases holds the RELEASES: each a numbered, immutable copy of a draft that was
--     published after the server checked it against the catalog and the app's rows. An app
--     serves one release, apps.live_release, until someone with the publish capability
--     publishes a newer draft or rolls back to an older release. Neither touches the rows.
--
-- Each release records the version of the definition format it was written in and the catalog
-- capabilities its screen needs, so a client that doesn't know one can say it has to update
-- instead of drawing a broken app. The same two values are in the definition's JSON
-- (definition_version, requires).
--
-- Every existing app becomes release 1, live, with its current definition as its draft too, so
-- what people use is what they used before. No role assignment, row or version changes.

ALTER TABLE apps ADD COLUMN live_release INTEGER;
ALTER TABLE apps ADD COLUMN live_at TEXT;
-- 1 when the draft is not the live release's definition: there is something to publish.
ALTER TABLE apps ADD COLUMN draft_differs INTEGER NOT NULL DEFAULT 0 CHECK (draft_differs IN (0, 1));

CREATE TABLE app_releases (
    id                 INTEGER PRIMARY KEY,
    app_id             INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- 1, 2, 3 … per app, never reused.
    number             INTEGER NOT NULL CHECK (number >= 1),
    definition         TEXT NOT NULL CHECK (json_valid(definition)),
    definition_version INTEGER NOT NULL CHECK (definition_version >= 1),
    -- The catalog capabilities the screen needs (JSON array of text), such as component.Stat.
    requires           TEXT NOT NULL CHECK (json_valid(requires) AND json_type(requires) = 'array'),
    published_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    published_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    UNIQUE (app_id, number)
) STRICT;

-- The draft: every existing definition says its format version and what it needs. The
-- capabilities are the components it uses and the catalog functions it calls (the same as
-- apps.Capabilities works out in Go, which a test holds this to).
UPDATE apps SET definition = json_set(definition,
    '$.definition_version', 1,
    '$.requires', json(coalesce((
        SELECT json_group_array(cap) FROM (
            SELECT 'component.' || json_extract(c.value, '$.component') AS cap
            FROM json_each(apps.definition, '$.components') c
            WHERE json_type(c.value, '$.component') = 'text'
            UNION
            SELECT 'function.' || t.atom
            FROM json_tree(apps.definition, '$.components') t
            WHERE t.key = 'call' AND t.type = 'text'
              AND t.atom IN ('and', 'email', 'formatCurrency', 'formatDate', 'formatNumber', 'formatString',
                             'length', 'not', 'numeric', 'or', 'pluralize', 'required')
            ORDER BY cap
        )), '[]')))
WHERE json_valid(definition) AND json_type(definition) = 'object';

INSERT INTO app_releases (app_id, number, definition, definition_version, requires, published_at, published_by)
SELECT id, 1, definition, 1, coalesce(json_extract(definition, '$.requires'), '[]'), updated_at, updated_by
FROM apps;

UPDATE apps SET live_release = 1, live_at = updated_at;

-- A release never changes, and goes only with its app.
-- The one change allowed is the publisher's account going (published_by ON DELETE SET NULL).
CREATE TRIGGER app_releases_immutable
BEFORE UPDATE OF id, app_id, number, definition, definition_version, requires, published_at
ON app_releases
BEGIN
    SELECT RAISE(ABORT, 'a release cannot be changed');
END;

CREATE TRIGGER app_releases_publisher_only_cleared
BEFORE UPDATE OF published_by ON app_releases
WHEN NEW.published_by IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a release cannot be changed');
END;

CREATE TRIGGER app_releases_kept
BEFORE DELETE ON app_releases
WHEN EXISTS (SELECT 1 FROM apps WHERE id = OLD.app_id)
BEGIN
    SELECT RAISE(ABORT, 'a release stays as long as its app does');
END;

-- An app serves a release it has.
CREATE TRIGGER apps_live_release_exists
BEFORE UPDATE OF live_release ON apps
WHEN NEW.live_release IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM app_releases WHERE app_id = NEW.id AND number = NEW.live_release)
BEGIN
    SELECT RAISE(ABORT, 'an app can only serve one of its releases');
END;
