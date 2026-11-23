-- Photo attachments of personal apps (internal/apps/attachments.go).
--
-- A record never holds file bytes: an attachment field holds a list of attachment ids, and the
-- bytes live in the managed storage of the space that holds the app, under its data directory
-- (.taskto/app-files/<app>/<id>, with a thumbnail beside it). This table is the only thing the
-- database keeps of a file: who is uploading it or uploaded it, which app and field it is
-- for, how far the upload got, what it turned out to be, and which record it was attached to.
--
-- One row is both the upload and the attachment it becomes:
--
--   uploading  bytes arrive in chunks at their offsets, 'received' says how many there are;
--              only the uploader may add to it, ask about it or abandon it
--   ready      finalized: the bytes were checked (type sniffed from them, size, pixels) and
--              a thumbnail made; it can be attached to one record, by its uploader
--
-- An attachment is attached to a record once (row_id), and stays attached: a record's
-- history refers to it, so changing a record's list, or deleting the record, keeps the file.
-- One that was never attached (row_id IS NULL) is an orphan, which the server removes after a
-- day. The files are removed with the row, never the other way round.
--
-- space_id is the space whose directory holds the bytes. It is the app's space, except for a
-- while after the app was moved to another space of its workspace: the server then copies the
-- files over and updates it, and reads follow it meanwhile.

CREATE TABLE app_attachments (
    -- 32 hexadecimal characters from the system's random source, never a counter.
    id           TEXT PRIMARY KEY CHECK (length(id) = 32 AND id NOT GLOB '*[^0-9a-f]*'),
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    space_id     INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    -- The attachment field of the app's collection it was uploaded for.
    field        TEXT NOT NULL,
    -- The uploader. If their account goes, nobody can attach it, and it is removed as an orphan.
    owner        INTEGER REFERENCES users (id) ON DELETE SET NULL,
    status       TEXT NOT NULL CHECK (status IN ('uploading', 'ready')),
    -- The declared size of the upload while it is uploading: what finalizing needs to have
    -- received. Once ready, the stored size: the bytes of the picture as it is kept (its metadata
    -- removed), and the length it is served with.
    size         INTEGER NOT NULL CHECK (size >= 1),
    -- How many bytes of the upload the server has; once ready, the same as size.
    received     INTEGER NOT NULL DEFAULT 0 CHECK (received >= 0 AND received <= size),
    -- Once ready: the type found in the bytes (image/jpeg, image/png or image/gif), the
    -- pixels, and the thumbnail's size in bytes.
    content_type TEXT,
    width        INTEGER,
    height       INTEGER,
    thumb_size   INTEGER,
    -- The record it is attached to, once it is.
    row_id       INTEGER REFERENCES app_rows (id) ON DELETE CASCADE,
    attached_at  TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- The last chunk, or the last change of state: what an abandoned upload is aged by.
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK ((status = 'ready') = (content_type IS NOT NULL)),
    CHECK (row_id IS NULL OR status = 'ready')
) STRICT;

CREATE INDEX app_attachments_by_app ON app_attachments (app_id, owner);
CREATE INDEX app_attachments_by_row ON app_attachments (row_id);
CREATE INDEX app_attachments_orphans ON app_attachments (updated_at) WHERE row_id IS NULL;
CREATE INDEX app_attachments_by_space ON app_attachments (space_id);

-- An attachment belongs to one app and field, and once attached to one record, for good. Its
-- uploader can't be changed either, but an account that goes leaves it unattributed (owner ON
-- DELETE SET NULL is an update of owner to NULL, which the second trigger lets through).
CREATE TRIGGER app_attachments_fixed_app
BEFORE UPDATE OF app_id, field ON app_attachments
BEGIN
    SELECT RAISE(ABORT, 'an attachment cannot move to another app or field');
END;

CREATE TRIGGER app_attachments_fixed_owner
BEFORE UPDATE OF owner ON app_attachments
WHEN NEW.owner IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'an attachment cannot change its uploader');
END;

CREATE TRIGGER app_attachments_attached_once
BEFORE UPDATE OF row_id ON app_attachments
WHEN OLD.row_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'an attachment stays with the record it was attached to');
END;
