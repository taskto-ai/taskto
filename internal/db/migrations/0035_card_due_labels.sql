-- A card's due date and labels.
--
-- The due date is a calendar date, written YYYY-MM-DD, with no time and no time zone: a card
-- due on the 5th is due on the 5th wherever its readers are. The check keeps the shape (the
-- server also checks that it is a real date).
--
-- Labels are free text, per card, not a set kept by the board: the server normalizes each to
-- lower case with single spaces, so "Bug" and "bug " are one label. They are filtered by
-- exact match, which the index on (label, card_id) serves.

ALTER TABLE cards ADD COLUMN due TEXT
    CHECK (due IS NULL OR due GLOB '[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]');

CREATE INDEX cards_by_due ON cards (board_id, due) WHERE due IS NOT NULL;

CREATE TABLE card_labels (
    card_id INTEGER NOT NULL REFERENCES cards (id) ON DELETE CASCADE,
    label   TEXT NOT NULL CHECK (label != ''),
    PRIMARY KEY (card_id, label)
) STRICT;

CREATE INDEX card_labels_by_label ON card_labels (label, card_id);
