package integrations

import "testing"

// The table is the unit the payment provider's API takes, which isn't always ISO 4217's: Stripe
// documents COP, HUF and IDR as two-decimal currencies, JPY and its like as zero-decimal, and
// writes ISK with two decimals that are always "00" (5 ISK is 500).
func TestExponentsMatchTheProvidersUnits(t *testing.T) {
	want := map[string]int{"COP": 2, "HUF": 2, "IDR": 2, "ISK": 2, "TWD": 2, "EUR": 2, "USD": 2, "JPY": 0, "KRW": 0, "VND": 0, "CLP": 0}
	for code, exp := range want {
		if got, ok := Exponent(code); !ok || got != exp {
			t.Errorf("%s: exponent %d, listed %v; want %d", code, got, ok, exp)
		}
	}
	if !WholeUnitsOnly("ISK") || WholeUnitsOnly("EUR") || WholeUnitsOnly("HUF") {
		t.Error("only ISK is written with decimals that are always zero")
	}
}
