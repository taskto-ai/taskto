package boards

import "strings"

// Ranks order the cards of a column. A rank is a string of rankDigits, compared as
// bytes; it is a fraction in base 62 ("V" is one half, "0V" a 62nd of that), never ends
// with the zero digit, so there is always room before it, and there is always one
// between any two. Placing a card between two others gives it a rank between theirs, so
// a move changes one row and never renumbers a column. Ranks grow by about a digit per
// six cards inserted at the same place, which a board never gets near trouble with.

// rankDigits are the digits, in ascending byte order.
const rankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const rankBase = len(rankDigits)

// rankBetween returns a rank strictly between a and b, where "" for a means the start
// of the column and "" for b its end. a must be less than b unless b is "".
func rankBetween(a, b string) string {
	if b != "" {
		// Past its end, a reads as zeros.
		n := 0
		for n < len(b) && rankDigitAt(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + rankBetween(rankSuffix(a, n), b[n:])
		}
	}
	lo, hi := 0, rankBase
	if a != "" {
		lo = strings.IndexByte(rankDigits, a[0])
	}
	if b != "" {
		hi = strings.IndexByte(rankDigits, b[0])
	}
	if hi-lo > 1 {
		return string(rankDigits[(lo+hi)/2])
	}
	// The first digits are consecutive: b's first digit alone is between them, if b has
	// more; otherwise keep a's first digit and go one place further.
	if len(b) > 1 {
		return b[:1]
	}
	return string(rankDigits[lo]) + rankBetween(rankSuffix(a, 1), "")
}

func rankDigitAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return rankDigits[0]
}

func rankSuffix(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}
