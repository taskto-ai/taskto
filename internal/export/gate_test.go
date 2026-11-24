package export

import (
	"testing"
	"time"
)

func TestGateLimitsEachPerson(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := newGate(2, time.Hour, -1, func() time.Time { return now })

	leave, why, _ := g.enter(1)
	if why != allowed {
		t.Fatalf("first = %v", why)
	}
	// One at a time: a second while the first runs, and it isn't counted.
	if _, why, _ := g.enter(1); why != running {
		t.Errorf("second at once = %v, want running", why)
	}
	leave()
	leave() // ending twice is harmless
	leave, why, _ = g.enter(1)
	if why != allowed {
		t.Fatalf("second = %v", why)
	}
	leave()
	// Two this hour: the third waits for the first to age out.
	_, why, wait := g.enter(1)
	if why != tooMany || wait != time.Hour {
		t.Errorf("third = %v after %v, want tooMany after 1h", why, wait)
	}
	if _, why, _ := g.enter(2); why != allowed {
		t.Errorf("another person = %v, want allowed", why)
	}
	now = now.Add(time.Hour)
	if _, why, _ := g.enter(1); why != allowed {
		t.Errorf("after the window = %v, want allowed", why)
	}
}

func TestGateLimitsTheServer(t *testing.T) {
	g := newGate(-1, time.Hour, 2, time.Now)
	var leaves []func()
	for u := int64(1); u <= 2; u++ {
		leave, why, _ := g.enter(u)
		if why != allowed {
			t.Fatalf("person %d = %v", u, why)
		}
		leaves = append(leaves, leave)
	}
	if _, why, wait := g.enter(3); why != busy || wait <= 0 {
		t.Errorf("third at once = %v after %v, want busy", why, wait)
	}
	leaves[0]()
	if _, why, _ := g.enter(3); why != allowed {
		t.Errorf("after one ended = %v, want allowed", why)
	}
}
