package integrations

import (
	"slices"
	"strings"

	"taskto.ai/taskto/internal/currency"
)

// The currencies an integration's limits may name are the closed list of internal/currency, which
// an app's declaration of what it asks for reads too, and the code that opens a payment (a later
// card) reads its amounts from the same table, so a limit and a payment mean the same unit.

// Exponent returns the number of decimals of a currency, and whether the currency is on the
// closed list.
func Exponent(code string) (int, bool) { return currency.Exponent(code) }

// Currencies lists the codes on the closed list, sorted: what a form offers.
func Currencies() []string { return currency.Codes() }

// Exponents is a copy of the table, by code: what the web app writes an amount with.
func Exponents() map[string]int { return currency.Exponents() }

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
		if !currency.Known(c) || slices.Contains(out, c) {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
