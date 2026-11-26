// Package currency is the closed list of currencies that payments may be asked in, with the number
// of decimals each has. An integration's limits (internal/integrations) and an app's declaration
// of what it asks for (internal/apps) read the same table, so a limit and a payment mean the same
// unit, and neither package has to import the other.
package currency

import (
	"maps"
	"slices"
)

// exponents are the currencies, with the number of decimals each has in the provider's API (the
// exponent of the unit its amounts are written in: 2 for euros, 0 for yen). This is the unit the
// payment providers use, which is not always ISO 4217's: COP, HUF and IDR have two decimals, and
// Stripe writes the króna (ISK) as two as well, the decimals always "00" (see
// integrations.WholeUnitsOnly). It is a closed list: a code outside it is refused.
var exponents = map[string]int{
	"AED": 2, "ARS": 2, "AUD": 2, "BGN": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CLP": 0, "CNY": 2,
	"COP": 2, "CZK": 2, "DKK": 2, "EUR": 2, "GBP": 2, "HKD": 2, "HUF": 2, "IDR": 2, "ILS": 2,
	"INR": 2, "ISK": 2, "JPY": 0, "KRW": 0, "MXN": 2, "MYR": 2, "NOK": 2, "NZD": 2, "PEN": 2,
	"PHP": 2, "PLN": 2, "RON": 2, "SAR": 2, "SEK": 2, "SGD": 2, "THB": 2, "TRY": 2, "TWD": 2,
	"UAH": 2, "USD": 2, "UYU": 2, "VND": 0, "ZAR": 2,
}

// Exponent returns the number of decimals of a currency, and whether the currency is on the list.
func Exponent(code string) (int, bool) {
	e, ok := exponents[code]
	return e, ok
}

// Known reports whether code is on the list. Codes are upper case.
func Known(code string) bool {
	_, ok := exponents[code]
	return ok
}

// Codes lists the codes on the list, sorted: what a form offers.
func Codes() []string {
	codes := make([]string, 0, len(exponents))
	for code := range exponents {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	return codes
}

// Exponents is a copy of the table, by code: what the web app writes an amount with.
func Exponents() map[string]int { return maps.Clone(exponents) }
