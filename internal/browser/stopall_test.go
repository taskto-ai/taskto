//go:build unix

package browser

import (
	"context"
	"errors"
	"testing"
	"time"
)

// StopAll stops a user's browsers and their live views and waits for their processes, without
// refusing later starts or removing a file (the CLI worker does both when it removes the user's
// whole directory); others' browsers go on.
func TestStopAllStopsWithoutRefusingOrRemoving(t *testing.T) {
	h := newProfileHost(t)
	h.vnc = echoView
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	startAll(t, m, 2)
	view, err := m.OpenLive(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StopAll(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if n := h.alive(1); n != 0 {
		t.Errorf("%d of the user's processes still run", n)
	}
	waitEnded(t, view)
	if !h.exists(1) {
		t.Error("StopAll removed the profile")
	}
	if n := h.browsersAlive(2); n != 2 {
		t.Errorf("user 2 runs %d browsers, had 2", n)
	}
	if err := m.Start(ctx, 1, Testing); err != nil {
		t.Errorf("a start after StopAll: %v", err)
	}
}

// Unforget takes back Forget's refusal of the users it is asked about, made before the time it is
// given: after a restore of the server's data, a person whose account is back, or a number given
// again. A refusal made after it (a deletion now) stays, and so does one of a user it isn't asked
// about.
func TestUnforgetTakesBackOnlyOlderRefusals(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, user := range []int64{1, 2} {
		if err := m.Forget(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	census := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := m.Forget(ctx, 3); err != nil {
		t.Fatal(err)
	}
	m.Unforget(func(user int64) bool { return user != 2 }, census)

	if err := m.Start(ctx, 1, Testing); err != nil {
		t.Errorf("the account that is back is refused: %v", err)
	}
	for _, user := range []int64{2, 3} {
		if err := m.Start(ctx, user, Testing); !errors.Is(err, ErrAccountClosing) {
			t.Errorf("user %d: %v, want ErrAccountClosing", user, err)
		}
	}
}
