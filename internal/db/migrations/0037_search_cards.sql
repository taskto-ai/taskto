-- Full-text search of board cards (internal/search, cards.go): each card's title, body and
-- thread. The boards stay the truth: a card's row says which version of the card, and which
-- of its comments, the index reflects, and a search brings the cards it may look in up to
-- date first, so nothing here is the only copy of anything. The index is made from the
-- cards that exist when the server first searches or starts, so there is nothing to fill in
-- here.

-- What the index has of each card. A card that is deleted, with its board or space, takes
-- its row, and the row its text.
CREATE TABLE search_cards (
    card_id     INTEGER PRIMARY KEY REFERENCES cards (id) ON DELETE CASCADE,
    -- cards.version, and the newest comment's ID (0 for none), when the card was indexed.
    version     INTEGER NOT NULL,
    comment_id  INTEGER NOT NULL,
    -- How the text was split into words (textFormat in Go).
    text_format INTEGER NOT NULL
) STRICT;

-- Full text: the title, body and comments of each search_cards row (rowid = card_id), split
-- into words as the documents' text is.
CREATE VIRTUAL TABLE search_card_fts USING fts5(title, body, comments,
    tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

CREATE TRIGGER search_cards_fts_delete
AFTER DELETE ON search_cards
BEGIN
    DELETE FROM search_card_fts WHERE rowid = OLD.card_id;
END;
