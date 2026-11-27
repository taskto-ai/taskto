//go:build unix

package cliworker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
	gone := cliowner.Account{User: 2, Identity: cliworker.IdentityForTest(2), Boot: later, Installation: cliworker.TestInstallation}
	if err := h.w.Forget(ctx, gone); err != nil {
		t.Errorf("the later server's deletion: %v", err)
	}
	purged := false
	if err := h.w.FenceDeletion(ctx, cliowner.Account{User: 3, Identity: cliworker.IdentityForTest(3), Boot: later, Installation: cliworker.TestInstallation}, func() error { purged = true; return nil }); err != nil || !purged {
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

// slowHelper is a helper that waits before it does anything (a slow start), so a turn's home is
// made only after the worker admitted the turn. It touches the marker it returns as it starts.
func slowHelper(t *testing.T) (script, started string) { return slowHelperFor(t, 1) }

// slowHelperFor is slowHelper, waiting seconds.
func slowHelperFor(t *testing.T, seconds int) (script, started string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script, started = filepath.Join(dir, "helper"), filepath.Join(dir, "started")
	body := "#!/bin/sh\ntouch '" + started + "'\nsleep " + strconv.Itoa(seconds) + "\nexec '" + exe + "' \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { //nolint:gosec // G306: the test's own script
		t.Fatal(err)
	}
	return script, started
}

// helperStarted waits until the slow helper of a turn runs: the worker admitted the turn and
// started its helper, which hasn't made the home yet.
func helperStarted(t *testing.T, started string) {
	t.Helper()
	eventually(t, "the turn's helper didn't start", func() bool { _, err := os.Stat(started); return err == nil })
}

// A census that retires an account whose first turn was admitted, but whose helper hasn't made
// its home yet, stops that turn and leaves no home: nothing makes a retired account's home.
func TestACensusStopsAFirstTurnBeforeItsHomeIsMade(t *testing.T) {
	script, started := slowHelper(t)
	h := newHarness(t, func(c *cliworker.Config) { c.Helper = script })
	ctx := context.Background()
	turn, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	helperStarted(t, started)
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
	script, started := slowHelperFor(t, 4)
	h := newHarness(t, func(c *cliworker.Config) { c.Helper = script })
	ctx := context.Background()
	turn, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	helperStarted(t, started)
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
	// Stopped by that census itself, before any retry.
	select {
	case <-turnOver(turn):
	case <-time.After(2 * time.Second): // well before the helper would go on by itself
		t.Fatal("the census whose caller went after the retirement left the turn running")
	}
	if d := last(waitDone(t, turn, 20*time.Second)); d.Status == cliworker.StatusOK {
		t.Errorf("the retired account's turn finished: %+v", d)
	}
	if err := h.w.Reconcile(ctx, later); err != nil {
		t.Fatalf("the census tried again: %v", err)
	}
	time.Sleep(4500 * time.Millisecond) // past the helper's wait
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a retired account's home was made after its census: %v", err)
	}
}

// A retirement alone isn't a completed removal: a browsers' purge records one without stopping
// the account's CLI work. A census then still stops an admitted first turn whose home isn't made
// yet, and no home appears afterwards.
func TestARetirementWithoutItsRemovalStillGetsStopped(t *testing.T) {
	script, started := slowHelper(t)
	h := newHarness(t, func(c *cliworker.Config) { c.Helper = script })
	ctx := context.Background()
	c := bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})
	if err := h.w.Reconcile(ctx, c); err != nil {
		t.Fatal(err)
	}
	turn, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	helperStarted(t, started)
	home := h.layout.Home(h.w.HomeForTest(acct(1))).Root
	// The browsers' purge: retires the account, stops no CLI work.
	if err := h.w.FenceDeletion(ctx, acct(1), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: the home is there before the helper ran: %v", err)
	}
	if st, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Fatalf("fixture: the purge didn't retire the account: %+v %v", st, err)
	}
	// The census, tried again, no longer naming it.
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	if d := last(waitDone(t, turn, 20*time.Second)); d.Status == cliworker.StatusOK {
		t.Errorf("the retired account's turn finished: %+v", d)
	}
	time.Sleep(1500 * time.Millisecond) // past the helper's wait
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a retired account's home was made after its census: %v", err)
	}
	// Once removed, a census again leaves it alone, and it stays removed.
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
}

// turnOver is closed once turn has ended.
func turnOver(turn *cliworker.Turn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !turn.Over() {
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return done
}

// losePart removes the named files of the worker's state, with the worker stopped, and starts it
// again.
func (h *harness) losePart(t *testing.T, files ...string) {
	t.Helper()
	h.w.Close()
	for _, f := range files {
		if err := os.Remove(filepath.Join(h.layout.Root, ".worker", f)); err != nil {
			t.Fatal(err)
		}
	}
	h.reopen(t)
}

// Losing the history of boots, alone or with the copy of the latest, leaves the worker unable to
// tell the current server from an earlier one still trying: it refuses every census and every
// deletion, from any boot, the earlier server's regular census included, so nothing is retired;
// other work goes on. The state stays across a start of the worker, until an operator hands it
// over to the current server, whose census and deletions then go through.
func TestALostHistoryOfBootsRefusesEveryRetirementUntilAnOperatorHandsItOver(t *testing.T) {
	for name, files := range map[string][]string{
		"the history":                {"boots.json"},
		"the history and the latest": {"boots.json", "latest-boot"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			ctx := context.Background()
			earlier, later := cliworker.TestBoot, strings.Repeat("c", 32)
			if err := h.w.Reconcile(ctx, bootCensus(earlier, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
				t.Fatal(err)
			}
			old := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
			if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); err != nil {
				t.Fatal(err)
			}
			kept := cliowner.Account{User: 5, Identity: strings.Repeat("5", 32), Boot: later, Installation: cliworker.TestInstallation}
			dirs, err := h.w.AccountDirs(kept, cliworker.Claude, "")
			if err != nil {
				t.Fatal(err)
			}
			clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
			h.losePart(t, files...)
			if h.w.AuthorityLost() == "" {
				t.Fatal("the worker doesn't say it lost its authority")
			}
			// And says so over its socket, where the server asks, and refuses a deletion there too.
			r := serve(t, h)
			if why, err := r.Authority(ctx); err != nil || why.Lost == "" {
				t.Errorf("over the socket: %q %v", why, err)
			}
			if err := r.Forget(ctx, cliowner.Account{User: 1, Identity: cliworker.IdentityForTest(1), Boot: later, Installation: cliworker.TestInstallation}); !errors.Is(err, cliworker.ErrAuthorityLost) {
				t.Errorf("a deletion over the socket: %v, want ErrAuthorityLost", err)
			}
			// Everything that would retire is refused, from either server.
			for what, err := range map[string]error{
				"the earlier server's census":      h.w.Reconcile(ctx, bootCensus(earlier, time.Now(), nil)),
				"the later server's census":        h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)),
				"an unknown server's census":       h.w.Reconcile(ctx, bootCensus(strings.Repeat("e", 32), time.Now(), nil)),
				"the earlier server's deletion":    h.w.Forget(ctx, cliowner.Account{User: 5, Identity: kept.Identity, Boot: earlier, Installation: cliworker.TestInstallation}),
				"the later server's deletion":      h.w.Forget(ctx, cliowner.Account{User: 1, Identity: cliworker.IdentityForTest(1), Boot: later, Installation: cliworker.TestInstallation}),
				"the later server's browser purge": h.w.FenceDeletion(ctx, kept, func() error { return nil }),
			} {
				if !errors.Is(err, cliworker.ErrAuthorityLost) {
					t.Errorf("%s: %v, want ErrAuthorityLost", what, err)
				}
			}
			if st, err := h.w.Status(ctx, kept, cliworker.Claude, ""); err != nil || !st.LoggedIn {
				t.Errorf("the later server's account, other work going on: %+v %v", st, err)
			}
			// The state outlives a start of the worker.
			h.w.Close()
			h.reopen(t)
			if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); !errors.Is(err, cliworker.ErrAuthorityLost) {
				t.Errorf("a census after the worker started again: %v, want ErrAuthorityLost", err)
			}
			// The operator hands it over to the later server, with the worker stopped.
			if err := cliworker.TrustServer(h.layout, later, ""); err == nil {
				t.Error("a handover while the worker runs was taken")
			}
			h.w.Close()
			if err := cliworker.TrustServer(h.layout, later, ""); err != nil {
				t.Fatal(err)
			}
			h.reopen(t)
			if h.w.AuthorityLost() != "" {
				t.Errorf("still lost after the handover: %s", h.w.AuthorityLost())
			}
			if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), map[int64]string{5: kept.Identity})); err != nil {
				t.Fatalf("the trusted server's census: %v", err)
			}
			gone(t, old.Root)
			if st, err := h.w.Status(ctx, kept, cliworker.Claude, ""); err != nil || !st.LoggedIn {
				t.Errorf("the trusted server's account: %+v %v", st, err)
			}
			if err := h.w.Forget(ctx, cliowner.Account{User: 9, Identity: cliworker.IdentityForTest(9), Boot: earlier, Installation: cliworker.TestInstallation}); !errors.Is(err, cliworker.ErrStaleBoot) {
				t.Errorf("the earlier server's deletion after the handover: %v, want ErrStaleBoot", err)
			}
			if err := h.w.Forget(ctx, cliowner.Account{User: 9, Identity: cliworker.IdentityForTest(9), Boot: later, Installation: cliworker.TestInstallation}); err != nil {
				t.Errorf("the trusted server's deletion: %v", err)
			}
		})
	}
}

// A copy of the latest boot that is gone or behind the history (a stop between the two writes) is
// written again: the history is whole, and nothing is lost. One that isn't in the history is
// damage, and the worker can't trust either.
func TestTheCopyOfTheLatestBootIsRepairedOnlyFromAWholeHistory(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	later := strings.Repeat("c", 32)
	if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	h.losePart(t, "latest-boot")
	if why := h.w.AuthorityLost(); why != "" {
		t.Fatalf("the copy alone gone: lost (%s)", why)
	}
	if err := h.w.Forget(ctx, cliowner.Account{User: 9, Identity: cliworker.IdentityForTest(9), Boot: later, Installation: cliworker.TestInstallation}); err != nil {
		t.Errorf("the latest server's deletion: %v", err)
	}
	h.w.Close()
	if err := os.WriteFile(filepath.Join(h.layout.Root, ".worker", "latest-boot"), []byte(`{"boot":"`+strings.Repeat("e", 32)+`","seq":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	if h.w.AuthorityLost() == "" {
		t.Error("a copy of the latest boot the history doesn't have was trusted")
	}
}

// In local mode one server only can ever talk to the worker (it runs in the server's process, on
// the server's data directory): a lost history of boots doesn't stop it, and the server's next
// census sets the history again and tidies as usual.
func TestALostHistoryInLocalModeIsSetAgainByTheNextCensus(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.OneServer = true })
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Fatal(err)
	}
	old := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.losePart(t, "boots.json", "latest-boot")
	if why := h.w.AuthorityLost(); why != "" {
		t.Fatalf("local mode lost its authority: %s", why)
	}
	next := strings.Repeat("c", 32)
	if err := h.w.Reconcile(ctx, bootCensus(next, time.Now(), nil)); err != nil {
		t.Fatalf("the server's census: %v", err)
	}
	gone(t, old.Root)
}

// The history of boots is trusted only whole: one that lost an earlier entry, even with its latest
// entry and the copy of the latest intact, and one with an entry that isn't a boot, a boot twice or
// numbers out of order, leave the worker unable to tell the current server, so it refuses every
// census and deletion; a whole one is trusted.
func TestOnlyAWholeHistoryOfBootsIsTrusted(t *testing.T) {
	type entry struct {
		Boot string `json:"boot"`
		Seq  int64  `json:"seq"`
	}
	a, b, c := cliworker.TestBoot, strings.Repeat("c", 32), strings.Repeat("d", 32)
	sum := func(es []entry) string {
		raw, _ := json.Marshal(es)
		s := sha256.Sum256(raw)
		return hex.EncodeToString(s[:])
	}
	for name, edit := range map[string]func(es []entry) ([]entry, int, string){
		"an earlier entry gone, its count and sum kept": func(es []entry) ([]entry, int, string) {
			return es[1:], len(es), sum(es)
		},
		"an entry that isn't a boot": func(es []entry) ([]entry, int, string) {
			es[0].Boot = "x"
			return es, len(es), sum(es)
		},
		"a boot twice": func(es []entry) ([]entry, int, string) {
			es[1].Boot = es[0].Boot
			return es, len(es), sum(es)
		},
		"numbers out of order": func(es []entry) ([]entry, int, string) {
			es[0].Seq, es[1].Seq = es[1].Seq, es[0].Seq
			return es, len(es), sum(es)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			ctx := context.Background()
			for _, boot := range []string{a, b, c} {
				if err := h.w.Reconcile(ctx, bootCensus(boot, time.Now(), nil)); err != nil {
					t.Fatal(err)
				}
			}
			h.w.Close()
			path := filepath.Join(h.layout.Root, ".worker", "boots.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var l struct {
				Boots []entry `json:"boots"`
			}
			if err := json.Unmarshal(raw, &l); err != nil || len(l.Boots) != 3 {
				t.Fatalf("fixture: %s %v", raw, err)
			}
			es, count, s := edit(l.Boots)
			out, _ := json.Marshal(map[string]any{"boots": es, "count": count, "sum": s})
			if err := os.WriteFile(path, out, 0o600); err != nil {
				t.Fatal(err)
			}
			h.reopen(t)
			if h.w.AuthorityLost() == "" {
				t.Fatal("a history that isn't whole was trusted")
			}
			if err := h.w.Reconcile(ctx, bootCensus(a, time.Now(), nil)); !errors.Is(err, cliworker.ErrAuthorityLost) {
				t.Errorf("the earlier server's census with a history that isn't whole: %v, want ErrAuthorityLost", err)
			}
		})
	}
	// A whole history, as the worker writes it, is trusted, and its earlier boots stay earlier.
	h := newHarness(t, nil)
	ctx := context.Background()
	for _, boot := range []string{a, b, c} {
		if err := h.w.Reconcile(ctx, bootCensus(boot, time.Now(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	h.w.Close()
	h.reopen(t)
	if why := h.w.AuthorityLost(); why != "" {
		t.Fatalf("a whole history wasn't trusted: %s", why)
	}
	if err := h.w.Reconcile(ctx, bootCensus(a, time.Now(), nil)); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earliest server's census: %v, want ErrStaleBoot", err)
	}
}

// A worker's volume belongs to one taskto installation: the first census pins it, and a server of
// another installation (another data directory, during a move to a new host, say) has every census,
// deletion, browser purge and new account refused, whether its census comes after or before the
// other's, so no boot order can let it retire what the volume's installation keeps; known accounts'
// other work goes on. The pin outlives a start of the worker, and only an operator's handover moves
// the volume to the other installation.
func TestAWorkerVolumeBelongsToOneInstallation(t *testing.T) {
	other := strings.Repeat("e", 32)
	census2 := func(boot, inst string, owners map[int64]string) cliworker.Census {
		c := bootCensus(boot, time.Now(), owners)
		c.Installation = inst
		return c
	}
	t.Run("the other installation's census comes after", func(t *testing.T) {
		h := newHarness(t, nil)
		ctx := context.Background()
		if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
			t.Fatal(err)
		}
		mine := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
		later := strings.Repeat("c", 32) // a later boot, of another installation
		theirs := cliowner.Account{User: 2, Identity: cliworker.IdentityForTest(2), Boot: later, Installation: other}
		for what, err := range map[string]error{
			"its census":        h.w.Reconcile(ctx, census2(later, other, nil)),
			"its deletion":      h.w.Forget(ctx, cliowner.Account{User: 1, Identity: cliworker.IdentityForTest(1), Boot: later, Installation: other}),
			"its browser purge": h.w.FenceDeletion(ctx, cliowner.Account{User: 1, Identity: cliworker.IdentityForTest(1), Boot: later, Installation: other}, func() error { return nil }),
			"its new account":   func() error { _, err := h.w.Status(ctx, theirs, cliworker.Claude, ""); return err }(),
		} {
			if !errors.Is(err, cliworker.ErrOtherInstallation) {
				t.Errorf("%s: %v, want ErrOtherInstallation", what, err)
			}
		}
		if _, err := os.Stat(mine.Root); err != nil {
			t.Errorf("the volume's installation's account: %v", err)
		}
		// A known account's other work goes on, whoever asks.
		known := acct(1)
		known.Installation = other
		if st, err := h.w.Status(ctx, known, cliworker.Claude, ""); err != nil || !st.LoggedIn {
			t.Errorf("a known account's status: %+v %v", st, err)
		}
		// The pin outlives a start of the worker.
		h.w.Close()
		h.reopen(t)
		if err := h.w.Reconcile(ctx, census2(later, other, nil)); !errors.Is(err, cliworker.ErrOtherInstallation) {
			t.Errorf("its census after the worker started again: %v, want ErrOtherInstallation", err)
		}
		// The operator moves the volume to the other installation.
		h.w.Close()
		if err := cliworker.TrustServer(h.layout, later, other); err != nil {
			t.Fatal(err)
		}
		h.reopen(t)
		if err := h.w.Reconcile(ctx, census2(later, other, nil)); err != nil {
			t.Errorf("the other installation's census after the handover: %v", err)
		}
		if err := h.w.Reconcile(ctx, bootCensus(strings.Repeat("f", 32), time.Now(), nil)); !errors.Is(err, cliworker.ErrOtherInstallation) {
			t.Errorf("the first installation's census after the handover: %v, want ErrOtherInstallation", err)
		}
	})
	t.Run("the other installation's census came first", func(t *testing.T) {
		// The volume's installation's server is late (its first census failed and is tried again):
		// the other's, arriving first, pins the volume, and the late one is refused, its boot never
		// registered, whatever order it arrives in.
		h := newHarness(t, nil)
		ctx := context.Background()
		if err := h.w.Reconcile(ctx, census2(strings.Repeat("c", 32), other, nil)); err != nil {
			t.Fatal(err)
		}
		if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), nil)); !errors.Is(err, cliworker.ErrOtherInstallation) {
			t.Errorf("the late server's census: %v, want ErrOtherInstallation", err)
		}
	})
}

// A purge step from before identities were kept (it names no identity) is a deletion as any other:
// refused while the worker lost its authority, from an earlier boot or from another installation,
// and the number's directory from before homes stays until a deletion the worker takes.
func TestADeletionWithNoIdentityIsFencedToo(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	earlier, later := cliworker.TestBoot, strings.Repeat("c", 32)
	for _, boot := range []string{earlier, later} {
		if err := h.w.Reconcile(ctx, bootCensus(boot, time.Now(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	legacy := h.layout.LegacyDirForTest(5)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	old := func(boot, inst string) cliowner.Account {
		return cliowner.Account{User: 5, Boot: boot, Installation: inst}
	}
	if err := h.w.Forget(ctx, old(earlier, cliworker.TestInstallation)); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("an earlier boot's: %v, want ErrStaleBoot", err)
	}
	if err := h.w.Forget(ctx, old(later, strings.Repeat("e", 32))); !errors.Is(err, cliworker.ErrOtherInstallation) {
		t.Errorf("another installation's: %v, want ErrOtherInstallation", err)
	}
	h.losePart(t, "boots.json")
	if err := h.w.Forget(ctx, old(later, cliworker.TestInstallation)); !errors.Is(err, cliworker.ErrAuthorityLost) {
		t.Errorf("with the authority lost: %v, want ErrAuthorityLost", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("a refused deletion removed the directory: %v", err)
	}
	h.w.Close()
	if err := cliworker.TrustServer(h.layout, later, ""); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	if err := h.w.Forget(ctx, old(later, cliworker.TestInstallation)); err != nil {
		t.Fatal(err)
	}
	gone(t, legacy)
}

// A volume that lost only its installation binding is still some installation's: the worker can't
// tell whose, so it loses its authority rather than let the first census to come take it, and the
// operator's handover must name the installation. The homes stay.
func TestALostInstallationBindingIsAuthorityLost(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Fatal(err)
	}
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.losePart(t, "installation")
	if h.w.AuthorityLost() == "" {
		t.Fatal("a used volume without its installation was trusted")
	}
	other := strings.Repeat("e", 32)
	c := bootCensus(strings.Repeat("c", 32), time.Now(), nil)
	c.Installation = other
	if err := h.w.Reconcile(ctx, c); !errors.Is(err, cliworker.ErrAuthorityLost) {
		t.Errorf("another installation's first census: %v, want ErrAuthorityLost", err)
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("the volume's home: %v", err)
	}
	h.w.Close()
	if err := cliworker.TrustServer(h.layout, cliworker.TestBoot, ""); err == nil {
		t.Error("a handover that doesn't name the installation was taken")
	}
	if err := cliworker.TrustServer(h.layout, cliworker.TestBoot, cliworker.TestInstallation); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Errorf("the named installation's census: %v", err)
	}
	if err := h.w.Reconcile(ctx, c); !errors.Is(err, cliworker.ErrOtherInstallation) {
		t.Errorf("the other installation's census: %v, want ErrOtherInstallation", err)
	}
}

// A damaged boot counter is a lost authority, never a stale census of the current server's; the
// handover makes it again from the history and the records, and says it worked only once a
// starting worker would trust it.
func TestADamagedBootCounterIsHandedOverSoundly(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	h.w.Close()
	if err := os.WriteFile(filepath.Join(h.layout.Root, ".worker", "boot-seq"), []byte("9223372036854775807\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	if h.w.AuthorityLost() == "" {
		t.Fatal("a damaged boot counter was trusted")
	}
	later := strings.Repeat("c", 32)
	if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); !errors.Is(err, cliworker.ErrAuthorityLost) {
		t.Errorf("a census with the counter damaged: %v, want ErrAuthorityLost", err)
	}
	h.w.Close()
	if err := cliworker.TrustServer(h.layout, later, ""); err != nil {
		t.Fatalf("the handover: %v", err)
	}
	h.reopen(t)
	if why := h.w.AuthorityLost(); why != "" {
		t.Fatalf("the handover said it worked, and the worker still doesn't trust it: %s", why)
	}
	if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), nil)); err != nil {
		t.Errorf("the trusted server's census: %v", err)
	}
}

// The first account a volume gets pins it to its server's installation, before any census: a
// census of another installation that comes first can't take the volume or remove that home.
func TestTheFirstAccountPinsTheVolume(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true) // the account of the tests' installation
	c := bootCensus(strings.Repeat("c", 32), time.Now(), nil)
	c.Installation = strings.Repeat("e", 32)
	if err := h.w.Reconcile(ctx, c); !errors.Is(err, cliworker.ErrOtherInstallation) {
		t.Errorf("another installation's census, first to come: %v, want ErrOtherInstallation", err)
	}
	if st, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the first account's home: %+v %v", st, err)
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("the first account's home: %v", err)
	}
}

// A volume that has never seen a census stays new however often the worker starts on it: starting
// writes nothing of boots, so the next start doesn't take it for one that lost its history.
func TestANewVolumeStaysNewAcrossStarts(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	for range 2 {
		h.w.Close()
		h.reopen(t)
		if why := h.w.AuthorityLost(); why != "" {
			t.Fatalf("a new volume started again lost its authority: %s", why)
		}
	}
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})); err != nil {
		t.Errorf("the first census after the starts: %v", err)
	}
}
