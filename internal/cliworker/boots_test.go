//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// Boots are ordered by when the worker registered them, never by a clock: a later server whose
// clock is behind the earlier one's (a step back across a restart) is the latest, its census is
// applied, its deletions are taken, and the earlier server's census, whatever time it says, is
// stale.
func TestBootsAreOrderedWithoutAClock(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	now := time.Now()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, now, map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Fatal(err)
	}
	later := strings.Repeat("c", 32)
	behind := now.Add(-time.Hour) // the later server's clock stepped back
	if err := h.w.Reconcile(ctx, bootCensus(later, behind, map[int64]string{1: cliworker.IdentityForTest(1), 2: cliworker.IdentityForTest(2)})); err != nil {
		t.Fatalf("the later server's census, its clock behind: %v", err)
	}
	gone := cliowner.Account{User: 2, Identity: cliworker.IdentityForTest(2), Boot: later}
	if err := h.w.Forget(ctx, gone); err != nil {
		t.Errorf("the later server's deletion: %v", err)
	}
	purged := false
	if err := h.w.FenceDeletion(ctx, cliowner.Account{User: 3, Identity: cliworker.IdentityForTest(3), Boot: later}, func() error { purged = true; return nil }); err != nil || !purged {
		t.Errorf("the later server's browser purge: %v (ran %v)", err, purged)
	}
	// The earlier server tries its census again with a fresh time: still the earlier one.
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, now.Add(time.Hour), nil)); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier server's census, its time later: %v, want ErrStaleBoot", err)
	}
	if _, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); err != nil {
		t.Errorf("an account both censuses name: %v", err)
	}
}

// What a census retires doesn't depend on the list of boots: with it lost (or past its bound),
// an account made under an earlier server is still one a later census retires, never one of a
// server still to come. Meanwhile deletions wait for that census, which sets the order again.
func TestALostListOfBootsLeavesNoAccountUnretired(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Fatal(err)
	}
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.w.Close()
	if err := os.Remove(filepath.Join(h.layout.Root, ".worker", "boots.json")); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	later := strings.Repeat("c", 32)
	if err := h.w.Forget(ctx, cliowner.Account{User: 9, Identity: cliworker.IdentityForTest(9), Boot: later}); !errors.Is(err, cliowner.ErrCensusNeeded) {
		t.Errorf("a deletion before a census set the order again: %v, want ErrCensusNeeded", err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the earlier server's account after a census that doesn't name it: %v, want ErrForgotten", err)
	}
	gone(t, dirs.Root)
	if err := h.w.Forget(ctx, cliowner.Account{User: 9, Identity: cliworker.IdentityForTest(9), Boot: later}); err != nil {
		t.Errorf("a deletion once the census set the order again: %v", err)
	}
}

// slowHelper is a helper that waits before it does anything (a slow start), so a turn's home is
// made only after the worker admitted the turn.
func slowHelper(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 1\nexec '"+exe+"' \"$@\"\n"), 0o700); err != nil { //nolint:gosec // G306: the test's own script
		t.Fatal(err)
	}
	return script
}

// A census that retires an account whose first turn was admitted, but whose helper hasn't made
// its home yet, stops that turn and leaves no home: nothing makes a retired account's home.
func TestACensusStopsAFirstTurnBeforeItsHomeIsMade(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.Helper = slowHelper(t) })
	ctx := context.Background()
	turn, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	home := h.layout.Home(h.w.HomeForTest(acct(1))).Root
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: the home is there before the helper ran: %v", err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(strings.Repeat("a", 32), time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	if d := last(waitDone(t, turn, 20*time.Second)); d.Status == cliworker.StatusOK {
		t.Errorf("the retired account's turn finished: %+v", d)
	}
	time.Sleep(1500 * time.Millisecond) // past the helper's wait
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a retired account's home was made after its census: %v", err)
	}
}

// A census still waiting for the census lock when its caller goes (its server stopped, the
// connection closed) does nothing once it gets the lock: it registers no boot and retires no
// account. So a dead server's census, queued, never retires anything.
func TestACensusWhoseCallerWentRetiresNothing(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Fatal(err)
	}
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	release := h.w.HoldCensusForTest()
	dead := strings.Repeat("d", 32)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Reconcile(cctx, bootCensus(dead, time.Now(), nil)) }()
	eventually(t, "the census didn't arrive", func() bool { _, ok := h.w.ReservedForTest(dead); return ok })
	cancel()                           // the server goes: its connection closes
	<-done                             //nolint:errcheck // the caller went: whatever it heard
	time.Sleep(300 * time.Millisecond) // the worker sees the connection closed
	release()
	eventually(t, "the census didn't end", func() bool { _, ok := h.w.ReservedForTest(dead); return !ok })
	if got := h.w.LatestBootForTest(); got != cliworker.TestBoot {
		t.Errorf("the latest boot = %q, want the live server's: the abandoned census registered its boot", got)
	}
	if st, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the account the abandoned census didn't name: %+v %v", st, err)
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("its home: %v", err)
	}
	// The same for a deletion and a browser purge whose callers went.
	for name, del := range map[string]func(context.Context) error{
		"a deletion":      func(c context.Context) error { return r.Forget(c, acct(1)) },
		"a browser purge": func(c context.Context) error { return h.w.FenceDeletion(c, acct(1), func() error { return nil }) },
	} {
		release := h.w.HoldCensusForTest()
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- del(cctx) }()
		time.Sleep(200 * time.Millisecond) // waiting for the lock
		cancel()
		time.Sleep(300 * time.Millisecond)
		release()
		<-done //nolint:errcheck // the caller went: whatever it heard
		time.Sleep(100 * time.Millisecond)
		if _, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); err != nil {
			t.Errorf("after %s whose caller went: %v", name, err)
		}
	}
}

// Of two censuses, the one that arrived first has the earlier number, whichever gets the lock
// first: here the first one is held between its arrival and the lock while the second runs, and
// is then the earlier server's.
func TestTheCensusThatArrivesFirstHasTheEarlierNumber(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	first, second := strings.Repeat("a", 32), strings.Repeat("b", 32)
	arrived, proceed := make(chan struct{}), make(chan struct{})
	h.w.SetArrivedHookForTest(func(boot string) {
		if boot == first {
			close(arrived)
			<-proceed
		}
	})
	done := make(chan error, 1)
	go func() { done <- h.w.Reconcile(ctx, bootCensus(first, time.Now(), nil)) }()
	<-arrived
	a, _ := h.w.ReservedForTest(first)
	if err := h.w.Reconcile(ctx, bootCensus(second, time.Now().Add(-time.Hour), nil)); err != nil {
		t.Fatalf("the second census: %v", err)
	}
	close(proceed)
	if err := <-done; !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the first census, run after the second: %v, want ErrStaleBoot", err)
	}
	if got := h.w.LatestBootForTest(); got != second {
		t.Errorf("the latest boot = %q, want the second census's", got)
	}
	if a == 0 {
		t.Error("the first census took no number as it arrived")
	}
}

// A census whose caller goes just after it recorded an account's retirement still stops what runs
// for the account: retired and stopped are one step. Then the census is tried again, and succeeds,
// and the account's first turn, admitted before and slow to make its home, makes none.
func TestACensusCancelledAfterARetirementStillStopsTheWork(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.Helper = slowHelper(t) })
	ctx := context.Background()
	turn, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	home := h.layout.Home(h.w.HomeForTest(acct(1))).Root
	cctx, cancel := context.WithCancel(ctx)
	h.w.SetRetiredHookForTest(func(id string) {
		if id == cliworker.IdentityForTest(1) {
			cancel() // the server goes right after the retirement is recorded
		}
	})
	later := bootCensus(strings.Repeat("a", 32), time.Now(), nil)
	_ = h.w.Reconcile(cctx, later) // canceled part way: whatever it answers
	h.w.SetRetiredHookForTest(nil)
	if err := h.w.Reconcile(ctx, later); err != nil {
		t.Fatalf("the census tried again: %v", err)
	}
	if d := last(waitDone(t, turn, 20*time.Second)); d.Status == cliworker.StatusOK {
		t.Errorf("the retired account's turn finished: %+v", d)
	}
	time.Sleep(1500 * time.Millisecond) // past the helper's wait
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a retired account's home was made after its census: %v", err)
	}
}
