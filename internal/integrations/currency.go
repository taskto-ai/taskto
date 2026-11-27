package integrations

import (
	"slices"
	"strings"

	"taskto.ai/taskto/internal/currency"
)

func Exponent(code string) (int, bool) { return currency.Exponent(code) }

func Currencies() []string { return currency.Codes() }

func Exponents() map[string]int { return currency.Exponents() }

const (
	maxCurrencies  = 20
	MaxAmountMinor = 1_000_000_000_000
)

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
