package currency

import (
	"maps"
	"slices"
)

var exponents = map[string]int{
	"AED": 2, "ARS": 2, "AUD": 2, "BGN": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CLP": 0, "CNY": 2,
	"COP": 2, "CZK": 2, "DKK": 2, "EUR": 2, "GBP": 2, "HKD": 2, "HUF": 2, "IDR": 2, "ILS": 2,
	"INR": 2, "ISK": 2, "JPY": 0, "KRW": 0, "MXN": 2, "MYR": 2, "NOK": 2, "NZD": 2, "PEN": 2,
	"PHP": 2, "PLN": 2, "RON": 2, "SAR": 2, "SEK": 2, "SGD": 2, "THB": 2, "TRY": 2, "TWD": 2,
	"UAH": 2, "USD": 2, "UYU": 2, "VND": 0, "ZAR": 2,
}

var wholeUnitsOnly = map[string]bool{"ISK": true}

func WholeUnitsOnly(code string) bool { return wholeUnitsOnly[code] }

func Exponent(code string) (int, bool) {
	e, ok := exponents[code]
	return e, ok
}

func Known(code string) bool {
	_, ok := exponents[code]
	return ok
}

func Codes() []string {
	codes := make([]string, 0, len(exponents))
	for code := range exponents {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

func Exponents() map[string]int { return maps.Clone(exponents) }
