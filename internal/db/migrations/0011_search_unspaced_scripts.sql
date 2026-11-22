-- Full-text search in scripts written without spaces between words (Chinese, Japanese,
-- Thai…): their text is now indexed one character at a time (internal/search, segment),
-- and the tokenizer keeps marks within words (Thai vowels and tone marks, Indic vowel
-- signs) rather than split words at them. Latin text is indexed as before.
--
-- The full-text index is made again from git: the table is emptied, and every space is
-- marked for a full sync, which the server starts at once and every search continues; a
-- document whose text_format is behind is indexed again. Its search_docs row stays, and
-- with it its embeddings, which are of the same version.

-- The way each document's full text was indexed (textFormat in Go); 0 for the way before.
ALTER TABLE search_docs ADD COLUMN text_format INTEGER NOT NULL DEFAULT 0;

DROP TABLE search_fts;
CREATE VIRTUAL TABLE search_fts USING fts5(path, title, body,
    tokenize = "unicode61 remove_diacritics 2 categories 'L* N* Co M*'");

UPDATE search_spaces SET partial = 1;
