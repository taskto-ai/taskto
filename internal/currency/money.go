package currency

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Money is exact. A number field of an app holds a float64, which can't hold 0.1 or 120.10 exactly,
// so an amount is never computed on: its shortest decimal text (the one that reads back as the
// same float64, Decimal) is read digit by digit into the currency's smallest unit, and an amount
// that doesn't fit the unit as written is refused instead of rounded. 0.1 + 0.2 is the float
// 0.30000000000000004: it has more decimals than any currency, so it is refused.

var (
	// ErrNotANumber is for text that isn't a plain decimal number, and for NaN and infinity.
	ErrNotANumber = errors.New("currency: the amount isn't a number")
	// ErrNotPositive is for an amount of zero or less.
	ErrNotPositive = errors.New("currency: the amount isn't more than zero")
	// ErrTooManyDecimals is for an amount with more decimals than the currency has.
	ErrTooManyDecimals = errors.New("currency: the amount has more decimals than the currency")
	// ErrNotWhole is for an amount with decimals in a currency that takes whole units only (ISK).
	ErrNotWhole = errors.New("currency: the currency takes whole units only")
	// ErrTooLarge is for an amount that doesn't fit the smallest unit's 64-bit number.
	ErrTooLarge = errors.New("currency: the amount is too large")
	// ErrUnknown is for a code that isn't on the list.
	ErrUnknown = errors.New("currency: the currency isn't on the list")
)

// Decimal is the amount as text in plain decimal notation, never an exponent, as short as it can
// be: 120.1 for 120.10, 3 for 3.0. It is what an effect stores, so that what is converted when the
// entry runs is exactly what the record held when the action was made.
func Decimal(v float64) (string, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", ErrNotANumber
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

// Minor reads a decimal amount, such as "120.1" or "5", in the unit of the currency code (upper
// case) into its smallest unit: 12010 for EUR, 5 for JPY. It refuses what isn't a plain decimal
// number, an amount of zero or less, an amount with more decimals than the currency has (a trailing
// zero doesn't count: "5.50" is fine for 2, "5.500" too, "5.501" is not), a code that isn't on the
// list, an amount with decimals in a currency of whole units only (ISK) and an amount whose
// smallest unit doesn't fit an int64. It never rounds.
func Minor(amount, code string) (int64, error) {
	exp, ok := Exponent(code)
	if !ok {
		return 0, ErrUnknown
	}
	s := amount
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || !digits(whole) || (strings.Contains(s, ".") && (frac == "" || !digits(frac))) {
		return 0, ErrNotANumber
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > exp {
		if negative {
			return 0, ErrNotPositive
		}
		return 0, ErrTooManyDecimals
	}
	decimals := frac != "" // what is left once the trailing zeros are gone
	frac += strings.Repeat("0", exp-len(frac))
	n := strings.TrimLeft(whole+frac, "0")
	if n == "" || negative {
		return 0, ErrNotPositive
	}
	if wholeUnitsOnly[code] && decimals {
		return 0, ErrNotWhole
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil {
		return 0, ErrTooLarge
	}
	return v, nil
}

// MinorOf is Minor of a float64 number field's value.
func MinorOf(v float64, code string) (int64, error) {
	text, err := Decimal(v)
	if err != nil {
		return 0, err
	}
	return Minor(text, code)
}

// Format writes an amount of the smallest unit as decimal text with the currency's decimals:
// 12010 of EUR is "120.10", 5 of JPY is "5". It is for messages and the preview, not for talking to a
// provider.
func Format(minor int64, code string) string {
	exp, _ := Exponent(code)
	s := strconv.FormatInt(minor, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	if exp == 0 {
		return sign + s
	}
	if len(s) <= exp {
		s = strings.Repeat("0", exp-len(s)+1) + s
	}
	return sign + s[:len(s)-exp] + "." + s[len(s)-exp:]
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
