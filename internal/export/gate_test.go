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

// A person turned away because the server is busy leaves nothing in the gate's memory.
func TestGateForgetsThePeopleItTurnsAway(t *testing.T) {
	g := newGate(-1, time.Hour, 1, time.Now)
	if _, why, _ := g.enter(1); why != allowed {
		t.Fatalf("first = %v", why)
	}
	for u := int64(2); u < 50; u++ {
		if _, why, _ := g.enter(u); why != busy {
			t.Fatalf("person %d = %v, want busy", u, why)
		}
	}
	if len(g.started) != 1 {
		t.Errorf("the gate remembers %d people, want only the one who started", len(g.started))
	}
}

// An export that fails before it is made doesn't use one of the person's hour.
func TestGateRefundsAnExportThatNeverBegan(t *testing.T) {
	g := newGate(1, time.Hour, -1, time.Now)
	leave, why, _ := g.enter(1)
	if why != allowed {
		t.Fatalf("first = %v", why)
	}
	g.refund(1)
	leave()
	leave, why, _ = g.enter(1)
	if why != allowed {
		t.Fatalf("after a refunded one = %v, want allowed", why)
	}
	leave()
	if _, why, _ := g.enter(1); why != tooMany {
		t.Errorf("after one that was made = %v, want tooMany", why)
	}
	if len(g.started) != 1 {
		t.Errorf("the gate remembers %d people", len(g.started))
	}
}
