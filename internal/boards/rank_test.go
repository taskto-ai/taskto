package boards

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func TestRankBetween(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "", "V"},
		{"V", "", "k"},
		{"", "V", "F"},
		{"", "1", "0V"},
		{"0V", "1", "0k"},
		{"1", "1V", "1F"},
		{"1", "2", "1V"},
		{"1", "10V", "10F"},
		{"y", "", "z"},
		{"z", "", "zV"},
		{"A", "B5", "B"},
	}
	for _, tc := range cases {
		if got := rankBetween(tc.a, tc.b); got != tc.want {
			t.Errorf("rankBetween(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRanksKeepOrder inserts at random places, in the worst spots too, and checks the
// ranks stay strictly ordered, never end in the zero digit, and stay short.
func TestRanksKeepOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- repeatable test data, not a secret
	for _, where := range []string{"random", "front", "back", "second"} {
		var ranks []string
		for range 2000 {
			i := len(ranks)
			switch where {
			case "random":
				i = rng.IntN(len(ranks) + 1)
			case "front":
				i = 0
			case "second":
				i = min(1, len(ranks))
			}
			a, b := "", ""
			if i > 0 {
				a = ranks[i-1]
			}
			if i < len(ranks) {
				b = ranks[i]
			}
			r := rankBetween(a, b)
			if r <= a || (b != "" && r >= b) || strings.HasSuffix(r, "0") {
				t.Fatalf("%s: rankBetween(%q, %q) = %q", where, a, b, r)
			}
			ranks = slices.Insert(ranks, i, r)
		}
		if !slices.IsSorted(ranks) {
			t.Fatalf("%s: ranks out of order", where)
		}
		longest := 0
		for _, r := range ranks {
			longest = max(longest, len(r))
		}
		if longest > 400 {
			t.Errorf("%s: a rank of %d digits after 2000 inserts", where, longest)
		}
	}
}
