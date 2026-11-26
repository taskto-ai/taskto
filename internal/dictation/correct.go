package dictation

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The glossary correction: after any engine, words that sound like a glossary term but
// came out spelled otherwise are replaced by the term. It runs on the server, so it works
// for every engine, hosted ones included, without the glossary leaving the server.
//
//   - A run of up to maxSpan words matches a term when they are equal ignoring case,
//     accents, punctuation and the spaces between the words ("task to" is "taskto").
//   - A run of as many words as the term matches it when, the same way, they are within a
//     small edit distance of it: 1 for terms of 6 to 9 letters, 2 for longer ones, and the
//     first letter is the same. Shorter terms must match exactly.
//   - A match that would only change case or accents is left alone for terms under four
//     letters, so a term like "Go" doesn't capitalize every "go".

const maxSpan = 4

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r)
}

// accents maps accented Latin letters, lower-cased, to their base letter.
var accents = func() map[rune]rune {
	m := map[rune]rune{}
	for base, list := range map[rune]string{
		'a': "àáâãäåāăą", 'c': "çćĉċč", 'd': "ďđ", 'e': "èéêëēĕėęě", 'g': "ĝğġģ", 'h': "ĥħ",
		'i': "ìíîïĩīĭįı", 'j': "ĵ", 'k': "ķ", 'l': "ĺļľŀł", 'n': "ñńņňŉ", 'o': "òóôõöøōŏő",
		'r': "ŕŗř", 's': "śŝşšș", 't': "ţťŧț", 'u': "ùúûüũūŭůűų", 'w': "ŵ", 'y': "ýÿŷ", 'z': "źżž",
	} {
		for _, r := range list {
			m[r] = base
		}
	}
	return m
}()

// fold is s as matching compares it: lower case, without accents, of word characters only.
func fold(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if !isWordRune(r) || unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		if b, ok := accents[r]; ok {
			r = b
		}
		out = append(out, r)
	}
	return out
}

// token is a word of the text, at text[start:end].
type token struct {
	start, end int
	key        []rune
}

// tokenize finds the words of text: runs of word characters, with an apostrophe, hyphen or
// period between two of them kept inside ("don't", "Wi-Fi", "Node.js").
func tokenize(text string) []token {
	var out []token
	start := -1
	for i, r := range text {
		switch {
		case isWordRune(r):
			if start < 0 {
				start = i
			}
		case start >= 0 && strings.ContainsRune("'’-.", r) && i+utf8.RuneLen(r) < len(text):
			next, _ := utf8.DecodeRuneInString(text[i+utf8.RuneLen(r):])
			if isWordRune(next) {
				continue
			}
			fallthrough
		default:
			if start >= 0 {
				out = append(out, token{start: start, end: i, key: fold(text[start:i])})
				start = -1
			}
		}
	}
	if start >= 0 {
		out = append(out, token{start: start, end: len(text), key: fold(text[start:])})
	}
	return out
}

type term struct {
	text  string
	key   []rune
	words int
}

// corrector holds a glossary ready for matching.
type corrector struct {
	exact map[string]term
	// fuzzy holds the terms long enough to match approximately, by word count and first letter.
	fuzzy map[fuzzyKey][]term
}

type fuzzyKey struct {
	words int
	first rune
}

func newCorrector(terms []string) *corrector {
	c := &corrector{exact: map[string]term{}, fuzzy: map[fuzzyKey][]term{}}
	for _, text := range terms {
		words := len(tokenize(text))
		key := fold(text)
		if len(key) == 0 || words == 0 || words > maxSpan {
			continue
		}
		t := term{text: text, key: key, words: words}
		if _, dup := c.exact[string(key)]; !dup {
			c.exact[string(key)] = t
		}
		if allowedDistance(len(key)) > 0 {
			fk := fuzzyKey{words, key[0]}
			c.fuzzy[fk] = append(c.fuzzy[fk], t)
		}
	}
	return c
}

// allowedDistance is how far a term of n letters may be from what was heard.
func allowedDistance(n int) int {
	switch {
	case n >= 10:
		return 2
	case n >= 6:
		return 1
	}
	return 0
}

// Correct returns text with glossary terms put in.
func Correct(text string, terms []string) string {
	if len(terms) == 0 {
		return text
	}
	return newCorrector(terms).correct(text)
}

func (c *corrector) correct(text string) string {
	toks := tokenize(text)
	var b strings.Builder
	prev := 0
	for i := 0; i < len(toks); {
		span, t, ok := c.match(text, toks[i:])
		if !ok {
			i++
			continue
		}
		start, end := toks[i].start, toks[i+span-1].end
		heard := text[start:end]
		if heard != t.text && (len(t.key) >= 4 || !strings.EqualFold(heard, t.text)) {
			b.WriteString(text[prev:start])
			b.WriteString(t.text)
			prev = end
		}
		i += span
	}
	b.WriteString(text[prev:])
	return b.String()
}

// match finds the term that the words at the start of toks are: the longest exact match,
// else the closest approximate one. It returns how many words it covers.
func (c *corrector) match(text string, toks []token) (int, term, bool) {
	n := 1
	// Words join a run only across spaces, never across punctuation.
	for n < len(toks) && n < maxSpan && strings.TrimSpace(text[toks[n-1].end:toks[n].start]) == "" {
		n++
	}
	keys := make([][]rune, n)
	var joined []rune
	for i := range n {
		joined = append(joined, toks[i].key...)
		keys[i] = append([]rune(nil), joined...)
	}
	for span := n; span >= 1; span-- {
		if t, ok := c.exact[string(keys[span-1])]; ok {
			return span, t, true
		}
	}
	best, bestSpan, bestDist := term{}, 0, -1
	for span := n; span >= 1; span-- {
		key := keys[span-1]
		if len(key) == 0 {
			continue
		}
		for _, t := range c.fuzzy[fuzzyKey{span, key[0]}] {
			limit := allowedDistance(len(t.key))
			if d := distance(key, t.key, limit); d <= limit && (bestDist < 0 || d < bestDist) {
				best, bestSpan, bestDist = t, span, d
			}
		}
	}
	return bestSpan, best, bestDist >= 0
}

// distance is the Levenshtein distance between a and b, or limit+1 once it is over limit.
func distance(a, b []rune, limit int) int {
	if d := len(a) - len(b); d > limit || -d > limit {
		return limit + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
