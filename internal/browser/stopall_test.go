//go:build unix

package browser

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

func TestStopAllStopsWithoutRefusingOrRemoving(t *testing.T) {
	h := newProfileHost(t)
	h.vnc = echoView
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	startAll(t, m, 2)
	view, err := m.OpenLive(ctx, acct(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.StopAll(ctx, acct(1)); err != nil {
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
	if err := m.Start(ctx, acct(1), Testing); err != nil {
		t.Errorf("a start after StopAll: %v", err)
	}
}

func TestUnforgetTakesBackOnlyOlderRefusals(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, user := range []int64{1, 2} {
		if err := m.Forget(ctx, acct(user)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	census := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := m.Forget(ctx, acct(3)); err != nil {
		t.Fatal(err)
	}
	m.Unforget(func(id string) bool { return id != acct(2).Identity }, census)

	if err := m.Start(ctx, acct(1), Testing); err != nil {
		t.Errorf("the account that is back is refused: %v", err)
	}
	for _, user := range []int64{2, 3} {
		if err := m.Start(ctx, acct(user), Testing); !errors.Is(err, ErrAccountClosing) {
			t.Errorf("user %d: %v, want ErrAccountClosing", user, err)
		}
	}
}

func TestAllowHoldsForARunningBrowser(t *testing.T) {
	h := newProfileHost(t)
	h.vnc = echoView
	var refused atomic.Bool
	m := NewManager(Config{Host: h, Max: 8, Allow: func(a cliowner.Account) error {
		if a.User == 1 && refused.Load() {
			return ErrAccountClosing
		}
		return nil
	}})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	startAll(t, m, 1)
	startAll(t, m, 2)

	refused.Store(true)
	if _, err := m.Status(ctx, acct(1)); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("status: %v", err)
	}
	for _, a := range []Action{{Op: OpTabs, Browser: Personal}, {Op: OpSnapshot, Browser: Personal, Tab: "T1"}} {
		if _, err := m.Do(ctx, acct(1), a); !errors.Is(err, ErrAccountClosing) {
			t.Errorf("%s: %v", a.Op, err)
		}
	}
	if _, err := m.OpenLive(ctx, acct(1)); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("live view: %v", err)
	}
	if err := m.Start(ctx, acct(1), Testing); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("start: %v", err)
	}
	if err := m.Stop(ctx, acct(1), Personal); err != nil {
		t.Errorf("stop: %v", err)
	}
	if st, err := m.Status(ctx, acct(2)); err != nil || !st[Personal].Running {
		t.Errorf("another user's browser: %+v %v", st, err)
	}
}
