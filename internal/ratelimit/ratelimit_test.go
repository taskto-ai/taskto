package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(2, time.Minute, func() time.Time { return now })

	for i := range 2 {
		if ok, _ := l.Take("k"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	ok, wait := l.Take("k")
	if ok || wait != time.Minute {
		t.Fatalf("third attempt = %v, %v; want refused for a minute", ok, wait)
	}
	if ok, _ := l.Take("other"); !ok {
		t.Fatal("another key was refused")
	}

	// A given-back attempt frees a place.
	l.Give("k")
	if ok, _ := l.Take("k"); !ok {
		t.Fatal("refused after give")
	}

	now = now.Add(30 * time.Second)
	if ok, wait := l.Take("k"); ok || wait != 30*time.Second {
		t.Fatalf("mid-window = %v, %v", ok, wait)
	}
	now = now.Add(30 * time.Second)
	if ok, _ := l.Take("k"); !ok {
		t.Fatal("refused after the window ended")
	}
}

func TestLimiterSweeps(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(1, time.Minute, func() time.Time { return now })
	for i := range SweepAt {
		l.Take(fmt.Sprint(i))
	}
	now = now.Add(time.Minute)
	l.Take("new")
	if n := l.Len(); n != 1 {
		t.Fatalf("%d keys after a sweep, want 1", n)
	}
}

func TestLimiterUsed(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(2, time.Minute, func() time.Time { return now })
	l.Take("a")
	l.Take("a")
	l.Take("a") // refused: not counted
	l.Take("b")
	if n := l.Used(); n != 3 {
		t.Fatalf("Used = %d, want 3", n)
	}
	l.Give("a")
	if n := l.Used(); n != 2 {
		t.Fatalf("Used after Give = %d, want 2", n)
	}
}

// A map held at the sweep size by keys that haven't expired is not scanned on every new key.
func TestLimiterSweepCostIsBounded(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(1, time.Minute, func() time.Time { return now })
	for i := range SweepAt + 1000 {
		l.Take(fmt.Sprint(i))
	}
	if l.sweeps > 1 {
		t.Fatalf("%d full scans for 1000 new keys over unexpired ones, want at most 1", l.sweeps)
	}
	now = now.Add(2 * time.Minute)
	l.Take("later")
	if n := l.Len(); n != 1 {
		t.Fatalf("%d keys after the windows ended, want 1", n)
	}
}
