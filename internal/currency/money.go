package currency

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

var (
	ErrNotANumber      = errors.New("currency: the amount isn't a number")
	ErrNotPositive     = errors.New("currency: the amount isn't more than zero")
	ErrTooManyDecimals = errors.New("currency: the amount has more decimals than the currency")
	ErrNotWhole        = errors.New("currency: the currency takes whole units only")
	ErrTooLarge        = errors.New("currency: the amount is too large")
	ErrUnknown         = errors.New("currency: the currency isn't on the list")
)

func Decimal(v float64) (string, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "", ErrNotANumber
	}
	return strconv.FormatFloat(v, 'f', -1, 64), nil
}

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
	decimals := frac != ""
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

func MinorOf(v float64, code string) (int64, error) {
	text, err := Decimal(v)
	if err != nil {
		return 0, err
	}
	return Minor(text, code)
}

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
