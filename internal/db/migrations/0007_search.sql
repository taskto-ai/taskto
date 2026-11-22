-- The knowledge base's search index (internal/search). Each space's git repository stays the
-- truth: the index says which of main's commits it reflects, and is brought up to date from
-- the repository whenever it is behind, so nothing here is the only copy of anything.

-- main's commit each space's index reflects.
CREATE TABLE search_spaces (
    space_id   INTEGER PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
    commit_id  TEXT NOT NULL,
    -- 1 while a sync that takes several transactions runs: searches still see what is
    -- indexed, and if it stops half-way the next sync starts over from main's files
    -- rather than trust commit_id.
    partial    INTEGER NOT NULL DEFAULT 0,
    indexed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- Every file of every space, as indexed: Markdown documents and uploads (indexed by path
-- only). version is the git blob ID of the content.
CREATE TABLE search_docs (
    id            INTEGER PRIMARY KEY,
    space_id      INTEGER NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    path          TEXT NOT NULL,
    version       TEXT NOT NULL,
    markdown      INTEGER NOT NULL,
    title         TEXT NOT NULL,
    -- The version and the embeddings model (account/model) the document's chunks were made
    -- from; NULL until they are. A Markdown document whose embed_version or embed_key
    -- differs from what it should be is waiting for the embeddings worker.
    embed_version TEXT,
    embed_key     TEXT,
    -- Failed attempts in a row, and when to try again.
    embed_tries   INTEGER NOT NULL DEFAULT 0,
    embed_after   TEXT,
    UNIQUE (space_id, path)
) STRICT;

-- Full text: path, title and body of each search_docs row (rowid = search_docs.id).
-- Uploads have an empty body.
CREATE VIRTUAL TABLE search_fts USING fts5(path, title, body, tokenize = 'unicode61 remove_diacritics 2');

CREATE TRIGGER search_docs_fts_delete
AFTER DELETE ON search_docs
BEGIN
    DELETE FROM search_fts WHERE rowid = OLD.id;
END;

-- Semantic search: a document's chunks, each with its embedding as little-endian float32s,
-- normalized to length 1. Similarity is computed in Go.
CREATE TABLE search_chunks (
    doc_id INTEGER NOT NULL REFERENCES search_docs (id) ON DELETE CASCADE,
    seq    INTEGER NOT NULL,
    text   TEXT NOT NULL,
    vector BLOB NOT NULL,
    PRIMARY KEY (doc_id, seq)
) STRICT;

-- A workspace's embeddings model: one of its provider accounts and a model it serves.
-- Without a row, the workspace's search is full-text only.
CREATE TABLE search_settings (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    account_id   INTEGER NOT NULL REFERENCES provider_accounts (id) ON DELETE CASCADE,
    model        TEXT NOT NULL,
    updated_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

-- Consent to send a space's documents to an embeddings endpoint, given by the space's owner
-- (a private space) or one of its editors (a shared one), for the one endpoint it names: the
-- account, the model and the account's base URL at the time. The workspace's embeddings
-- model reads a space, and gets the queries searching it, only while it is that very
-- endpoint, so an admin who changes the model, its account or the account's base URL can't
-- send a space's text anywhere its members didn't agree to. Full-text search never leaves
-- the server.
CREATE TABLE search_embedding_consent (
    space_id   INTEGER PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
    account_id INTEGER NOT NULL REFERENCES provider_accounts (id) ON DELETE CASCADE,
    model      TEXT NOT NULL,
    base_url   TEXT NOT NULL,
    given_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    given_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;
