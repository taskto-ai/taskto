package integrations

import (
	"maps"
	"slices"
	"strings"
)

// exponents are the currencies an integration's limits may name, with the number of decimals
// each has (the exponent of its smallest unit: 2 for euros, 0 for yen). It is a closed list: a
// code outside it is refused, and the code that opens a payment (a later card) reads its
// amounts from the same table, so a limit and a payment mean the same unit.
var exponents = map[string]int{
	"AED": 2, "ARS": 2, "AUD": 2, "BGN": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CLP": 0, "CNY": 2,
	"COP": 2, "CZK": 2, "DKK": 2, "EUR": 2, "GBP": 2, "HKD": 2, "HUF": 2, "IDR": 2, "ILS": 2,
	"INR": 2, "ISK": 0, "JPY": 0, "KRW": 0, "MXN": 2, "MYR": 2, "NOK": 2, "NZD": 2, "PEN": 2,
	"PHP": 2, "PLN": 2, "RON": 2, "SAR": 2, "SEK": 2, "SGD": 2, "THB": 2, "TRY": 2, "TWD": 2,
	"UAH": 2, "USD": 2, "UYU": 2, "VND": 0, "ZAR": 2,
}

// Exponent returns the number of decimals of a currency, and whether the currency is on the
// closed list.
func Exponent(code string) (int, bool) {
	e, ok := exponents[code]
	return e, ok
}

// Currencies lists the codes on the closed list, sorted: what a form offers.
func Currencies() []string {
	codes := make([]string, 0, len(exponents))
	for code := range exponents {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

// Exponents is a copy of the table, by code: what the web app writes an amount with.
func Exponents() map[string]int { return maps.Clone(exponents) }

// Limits of an integration's limits.
const (
	maxCurrencies = 20
	// MaxAmountMinor is the largest ceiling: a trillion of the smallest unit, far above what a
	// payment link takes and safely inside what a JavaScript number holds exactly.
	MaxAmountMinor = 1_000_000_000_000
)

// normalizeCurrencies upper-cases and checks a currency list: 1 to maxCurrencies codes of the
// closed list, none twice. ok is false for any other list.
func normalizeCurrencies(in []string) (out []string, ok bool) {
	if len(in) == 0 || len(in) > maxCurrencies {
		return nil, false
	}
	out = make([]string, 0, len(in))
	for _, c := range in {
		c = strings.ToUpper(strings.TrimSpace(c))
		if _, known := exponents[c]; !known || slices.Contains(out, c) {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
