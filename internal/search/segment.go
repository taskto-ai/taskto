package search

import (
	"strings"
	"unicode"
)

// Scripts written without spaces between words (Chinese, Japanese, Thai and their
// neighbours) give FTS5's unicode61 tokenizer nothing to split at: a sentence would be one
// long word, found by its start only. The index splits their text into characters
// instead, each with its marks, and a query in them becomes a phrase of the same
// characters, so it matches anywhere: "京都" finds "東京都庁" (the phrase 京 都), and so
// does a single character. Text in every other script is left as it is, so it is indexed,
// and ranked, as before.

// sep separates the characters of an unspaced script in the indexed text: a separator to
// unicode61 (it is not a letter, number or mark), one byte, and never part of a document's
// text (cleanBody replaces it). It is taken out of what is shown (snippets) and of what is
// embedded.
const sep = '\x1F' // the ASCII unit separator

// textFormat is how the index splits text into words. A document indexed in an older
// format (search_docs.text_format) is indexed again by the next sync: change it, and the
// migration that marks every space for a full sync, whenever segment changes.
const textFormat = 1

// unspaced reports whether r is of a script written without spaces between words.
func unspaced(r rune) bool {
	switch {
	case r < 0x0E00:
		return false // fast path: Latin, Greek, Cyrillic, Arabic, Indic…
	case r == 0x30FC || r == 0xFF70: // the prolonged sound mark ー, and its half-width form
		return true
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana,
		unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

// segment returns s with sep between the characters of unspaced scripts, and between
// them and the letters or numbers they touch; a character keeps the marks after it (Thai
// vowels and tone marks). s without such characters is returned as it is. The result is
// at most 1.5 times as long as s: sep, 1 byte, comes before a character only if it or the
// one before is of an unspaced script, 3 bytes in UTF-8, so at most twice in 4 bytes.
func segment(s string) string {
	if !strings.ContainsFunc(s, unspaced) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) * 3 / 2)
	// Of the last character that wasn't a mark: whether it was part of a word, and of an
	// unspaced script.
	word, inScript := false, false
	for _, r := range s {
		if unicode.Is(unicode.M, r) {
			b.WriteRune(r)
			continue
		}
		u := unspaced(r)
		w := u || unicode.IsLetter(r) || unicode.IsNumber(r)
		if word && w && (u || inScript) {
			b.WriteRune(sep)
		}
		b.WriteRune(r)
		word, inScript = w, u
	}
	return b.String()
}

// unsegment takes sep out of text segment made, as shown or embedded.
func unsegment(s string) string {
	return strings.ReplaceAll(s, string(sep), "")
}
