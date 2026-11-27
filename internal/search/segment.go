package search

import (
	"strings"
	"unicode"
)

const sep = '\x1F'

const textFormat = 1

func unspaced(r rune) bool {
	switch {
	case r < 0x0E00:
		return false
	case r == 0x30FC || r == 0xFF70:
		return true
	}
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana,
		unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

func segment(s string) string {
	if !strings.ContainsFunc(s, unspaced) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) * 3 / 2)
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

func unsegment(s string) string {
	return strings.ReplaceAll(s, string(sep), "")
}
