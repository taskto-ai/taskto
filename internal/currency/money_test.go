package currency

import (
	"errors"
	"math"
	"testing"
)

func TestMinor(t *testing.T) {
	cases := []struct {
		amount, code string
		want         int64
		err          error
	}{
		{"120.1", "EUR", 12010, nil},
		{"120.10", "EUR", 12010, nil},
		{"120", "EUR", 12000, nil},
		{"0.01", "EUR", 1, nil},
		{"5.500", "EUR", 550, nil}, // a trailing zero is not a decimal
		{"5", "JPY", 5, nil},
		{"5.0", "JPY", 5, nil},
		{"007.5", "USD", 750, nil},
		{"90071992547409.93", "USD", 9007199254740993, nil}, // beyond what a float holds, still exact as text

		{"5.5", "JPY", 0, ErrTooManyDecimals},
		{"5.501", "EUR", 0, ErrTooManyDecimals},
		{"0.30000000000000004", "EUR", 0, ErrTooManyDecimals},
		{"0", "EUR", 0, ErrNotPositive},
		{"0.00", "EUR", 0, ErrNotPositive},
		{"-1", "EUR", 0, ErrNotPositive},
		{"-1.005", "EUR", 0, ErrNotPositive},
		{"-0", "EUR", 0, ErrNotPositive},
		{"92233720368547758.08", "EUR", 0, ErrTooLarge},
		{"10000000000000000000000", "EUR", 0, ErrTooLarge},
		{"", "EUR", 0, ErrNotANumber},
		{".5", "EUR", 0, ErrNotANumber},
		{"5.", "EUR", 0, ErrNotANumber},
		{"1e3", "EUR", 0, ErrNotANumber},
		{"+5", "EUR", 0, ErrNotANumber},
		{" 5", "EUR", 0, ErrNotANumber},
		{"5", "XXX", 0, ErrUnknown},
		{"5", "eur", 0, ErrUnknown},
	}
	for _, c := range cases {
		got, err := Minor(c.amount, c.code)
		if !errors.Is(err, c.err) || got != c.want {
			t.Errorf("Minor(%q, %s) = %d, %v; want %d, %v", c.amount, c.code, got, err, c.want, c.err)
		}
	}
}

func TestMinorOfNeverRounds(t *testing.T) {
	a, b := 0.1, 0.2 // variables: constants would add exactly
	if _, err := MinorOf(a+b, "EUR"); !errors.Is(err, ErrTooManyDecimals) {
		t.Errorf("0.1 + 0.2 = %v, want too many decimals", err)
	}
	if got, err := MinorOf(0.1, "EUR"); err != nil || got != 10 {
		t.Errorf("0.1 = %d, %v", got, err)
	}
	// Three decimals in euros, and the yen with decimals.
	if _, err := MinorOf(1.005, "EUR"); !errors.Is(err, ErrTooManyDecimals) {
		t.Errorf("1.005 EUR = %v", err)
	}
	if _, err := MinorOf(100.5, "JPY"); !errors.Is(err, ErrTooManyDecimals) {
		t.Errorf("100.5 JPY = %v", err)
	}
	if got, err := MinorOf(100, "JPY"); err != nil || got != 100 {
		t.Errorf("100 JPY = %d, %v", got, err)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := MinorOf(v, "EUR"); !errors.Is(err, ErrNotANumber) {
			t.Errorf("%v = %v, want not a number", v, err)
		}
	}
	if _, err := MinorOf(1e300, "EUR"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("1e300 = %v, want too large", err)
	}
	if _, err := MinorOf(-5, "EUR"); !errors.Is(err, ErrNotPositive) {
		t.Errorf("-5 = %v", err)
	}
	// Every price that people write with two decimals converts to exactly the cents written.
	for cents := int64(1); cents <= 200000; cents++ {
		v := float64(cents) / 100
		got, err := MinorOf(v, "EUR")
		if err != nil || got != cents {
			t.Fatalf("%d cents (%v) = %d, %v", cents, v, got, err)
		}
	}
}

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		minor int64
		code  string
		want  string
	}{{12010, "EUR", "120.10"}, {5, "EUR", "0.05"}, {0, "EUR", "0.00"}, {5, "JPY", "5"}, {100, "USD", "1.00"}, {-250, "USD", "-2.50"}} {
		if got := Format(c.minor, c.code); got != c.want {
			t.Errorf("Format(%d, %s) = %q, want %q", c.minor, c.code, got, c.want)
		}
	}
}
