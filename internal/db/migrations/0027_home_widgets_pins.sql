-- Home widgets and navigation pins (internal/apps, home.go).
--
-- An app's definition may declare contributions: widgets for people's Home page and a pin for
-- the navigation. The definition is the app's; WHICH widgets sit on someone's Home and which
-- apps they pinned is theirs, and kept here:
--
--   - home_items: one person's placements, in the order they chose. A row only says "this
--     widget of this app" or "this app"; what it shows, and whether the person may still see
--     it, is worked out from the app's live release and the person's access every time Home is
--     read, so a row for an app someone lost access to, moved away or archived gives them
--     nothing.
--   - home_defaults: what the people who publish an app of a SHARED space set as a default
--     for every member of the space: the member sees it unless they hide it.
--   - home_hidden: the defaults a member hid.
--
-- Rows go with the person, the app or the space they belong to.

CREATE TABLE home_items (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    app_id     INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('widget', 'pin')),
    -- The widget's id in the app's contributions; empty for a pin.
    widget     TEXT NOT NULL DEFAULT '' CHECK ((kind = 'widget') = (widget <> '')),
    position   INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (user_id, app_id, kind, widget)
) STRICT;

CREATE INDEX home_items_by_user ON home_items (user_id, kind, position, id);
CREATE INDEX home_items_by_app ON home_items (app_id);

CREATE TABLE home_defaults (
    id         INTEGER PRIMARY KEY,
    space_id   INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    app_id     INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('widget', 'pin')),
    widget     TEXT NOT NULL DEFAULT '' CHECK ((kind = 'widget') = (widget <> '')),
    set_by     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (app_id, kind, widget)
) STRICT;

CREATE INDEX home_defaults_by_space ON home_defaults (space_id, id);

CREATE TABLE home_hidden (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    default_id INTEGER NOT NULL REFERENCES home_defaults (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, default_id)
) STRICT, WITHOUT ROWID;
