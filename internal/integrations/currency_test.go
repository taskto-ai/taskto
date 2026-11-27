package integrations

import (
	"testing"

	"taskto.ai/taskto/internal/currency"
)

func TestExponentsMatchTheProvidersUnits(t *testing.T) {
	want := map[string]int{"COP": 2, "HUF": 2, "IDR": 2, "ISK": 2, "TWD": 2, "EUR": 2, "USD": 2, "JPY": 0, "KRW": 0, "VND": 0, "CLP": 0}
	for code, exp := range want {
		if got, ok := Exponent(code); !ok || got != exp {
			t.Errorf("%s: exponent %d, listed %v; want %d", code, got, ok, exp)
		}
	}
	if !currency.WholeUnitsOnly("ISK") || currency.WholeUnitsOnly("EUR") || currency.WholeUnitsOnly("HUF") {
		t.Error("only ISK is written with decimals that are always zero")
	}
}
