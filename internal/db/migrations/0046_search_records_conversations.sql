-- Full-text search of app records and of people's own conversations (internal/search,
-- records.go and conversations.go). As for cards (0042), the apps' rows and the conversations
-- stay the truth: the triggers below put what changed in a queue, whoever wrote the change, in
-- the same transaction as the change, and a search indexes the queue's entries it looks in
-- before it answers. Nothing here is the only copy of anything.

-- ---------------------------------------------------------------------------------------
-- App records

-- The rows indexed, and in which textFormat (Go) their text was split into words. A row that
-- is deleted for good, with its app or space, takes its row, and the row its text.
CREATE TABLE search_records (
    row_id      INTEGER PRIMARY KEY REFERENCES app_rows (id) ON DELETE CASCADE,
    text_format INTEGER NOT NULL
) STRICT;

-- The rows whose indexed text may differ from what they show now: a row is here from the
-- moment it is added, changed, deleted or restored, or from the moment the live release of its
-- app changes (which fields the app declares to people), until the index has been written from
-- it again. A search never quotes a row in the queue. Every row that exists when this migration
-- runs is queued.
CREATE TABLE search_record_queue (
    row_id INTEGER PRIMARY KEY REFERENCES app_rows (id) ON DELETE CASCADE
) STRICT;

INSERT INTO search_record_queue (row_id) SELECT id FROM app_rows;

CREATE TRIGGER search_rows_added
AFTER INSERT ON app_rows
BEGIN
    INSERT OR IGNORE INTO search_record_queue (row_id) VALUES (NEW.id);
END;

CREATE TRIGGER search_rows_changed
AFTER UPDATE OF data, deleted_at ON app_rows
BEGIN
    INSERT OR IGNORE INTO search_record_queue (row_id) VALUES (NEW.id);
END;

-- The text of a row is that of the fields its app's live release declares: another release
-- going live (a publish, a rollback) can declare other fields, so every row of the app is
-- indexed again.
CREATE TRIGGER search_apps_released
AFTER UPDATE OF live_release ON apps
WHEN NEW.live_release IS NOT OLD.live_release
BEGIN
    INSERT OR IGNORE INTO search_record_queue (row_id) SELECT id FROM app_rows WHERE app_id = NEW.id;
END;

-- Full text: the values of each search_records row's fields (rowid = row_id), split into words
-- as the documents' text is.
CREATE VIRTUAL TABLE search_record_fts USING fts5(body,
    tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

CREATE TRIGGER search_records_fts_delete
AFTER DELETE ON search_records
BEGIN
    DELETE FROM search_record_fts WHERE rowid = OLD.row_id;
END;

-- ---------------------------------------------------------------------------------------
-- Conversations

-- The conversations indexed, and in which textFormat their text was split into words. A
-- conversation that is deleted takes its row, and the row its text.
CREATE TABLE search_conversations (
    conversation_id INTEGER PRIMARY KEY REFERENCES conversations (id) ON DELETE CASCADE,
    text_format     INTEGER NOT NULL
) STRICT;

-- The conversations whose indexed text may differ from the conversation's now: one is here
-- from the moment it is created, renamed, archived or marked updated (which is what a message
-- does, and the end of the agent's reply to it), until the index has been written from the
-- agent runtime's transcript again. A search never quotes a conversation in the queue.
-- Reading the transcript takes a call to the agent runtime, which no transaction waits for:
-- the revision counts the marks, so the index takes a conversation off the queue only if no
-- mark came while it was reading.
CREATE TABLE search_conversation_queue (
    conversation_id INTEGER PRIMARY KEY REFERENCES conversations (id) ON DELETE CASCADE,
    revision        INTEGER NOT NULL DEFAULT 1
) STRICT;

INSERT INTO search_conversation_queue (conversation_id) SELECT id FROM conversations;

CREATE TRIGGER search_conversations_added
AFTER INSERT ON conversations
BEGIN
    INSERT OR IGNORE INTO search_conversation_queue (conversation_id) VALUES (NEW.id);
END;

-- Whenever the column is written, changed or not: two marks within a millisecond are one value,
-- and the second still says there is more.
CREATE TRIGGER search_conversations_updated
AFTER UPDATE OF updated_at, title ON conversations
BEGIN
    INSERT INTO search_conversation_queue (conversation_id) VALUES (NEW.id)
    ON CONFLICT (conversation_id) DO UPDATE SET revision = revision + 1;
END;

-- Full text: each search_conversations row's title and what was said in it (rowid =
-- conversation_id).
CREATE VIRTUAL TABLE search_conversation_fts USING fts5(title, body,
    tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

CREATE TRIGGER search_conversations_fts_delete
AFTER DELETE ON search_conversations
BEGIN
    DELETE FROM search_conversation_fts WHERE rowid = OLD.conversation_id;
END;
