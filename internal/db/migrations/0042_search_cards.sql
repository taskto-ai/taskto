-- Full-text search of board cards (internal/search, cards.go): each card's title, body and
-- thread. The boards stay the truth: the triggers below put a card in a queue of the cards
-- whose text changed, whoever wrote the change, in the same transaction as the change, and
-- a search indexes the queue's cards of the spaces it looks in before it answers, so nothing
-- here is the only copy of anything.

-- The cards indexed, and in which textFormat (Go) their text was split into words. A card
-- that is deleted, with its board or space, takes its row, and the row its text.
CREATE TABLE search_cards (
    card_id     INTEGER PRIMARY KEY REFERENCES cards (id) ON DELETE CASCADE,
    text_format INTEGER NOT NULL
) STRICT;

-- The cards whose indexed text may differ from the card's now: a card is here from the
-- moment its title or body changes, or a comment is added to its thread, edited or deleted,
-- until the index has been written from it again. A search never quotes a card in the
-- queue. Every card that exists when this migration runs is queued.
CREATE TABLE search_card_queue (
    card_id INTEGER PRIMARY KEY REFERENCES cards (id) ON DELETE CASCADE
) STRICT;

INSERT INTO search_card_queue (card_id) SELECT id FROM cards;

CREATE TRIGGER search_cards_added
AFTER INSERT ON cards
BEGIN
    INSERT OR IGNORE INTO search_card_queue (card_id) VALUES (NEW.id);
END;

CREATE TRIGGER search_cards_edited
AFTER UPDATE OF title, body ON cards
WHEN NEW.title IS NOT OLD.title OR NEW.body IS NOT OLD.body
BEGIN
    INSERT OR IGNORE INTO search_card_queue (card_id) VALUES (NEW.id);
END;

CREATE TRIGGER search_comments_added
AFTER INSERT ON card_comments
BEGIN
    INSERT OR IGNORE INTO search_card_queue (card_id) VALUES (NEW.card_id);
END;

CREATE TRIGGER search_comments_edited
AFTER UPDATE OF card_id, body ON card_comments
BEGIN
    INSERT OR IGNORE INTO search_card_queue (card_id) SELECT id FROM cards WHERE id IN (OLD.card_id, NEW.card_id);
END;

-- Not for a comment that goes with its card: the card is gone, and so is its queue row.
CREATE TRIGGER search_comments_deleted
AFTER DELETE ON card_comments
BEGIN
    INSERT OR IGNORE INTO search_card_queue (card_id) SELECT id FROM cards WHERE id = OLD.card_id;
END;

-- Full text: the title, body and comments of each search_cards row (rowid = card_id), split
-- into words as the documents' text is.
CREATE VIRTUAL TABLE search_card_fts USING fts5(title, body, comments,
    tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

CREATE TRIGGER search_cards_fts_delete
AFTER DELETE ON search_cards
BEGIN
    DELETE FROM search_card_fts WHERE rowid = OLD.card_id;
END;
