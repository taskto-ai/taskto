package boards

import "strings"

const rankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const rankBase = len(rankDigits)

func rankBetween(a, b string) string {
	if b != "" {
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
