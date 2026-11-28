//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func census(owners map[int64]string, live ...int64) cliworker.Census {
	c := cliworker.Census{Owners: map[int64]string{}, Boot: cliworker.TestBoot, Installation: cliworker.TestInstallation, Before: time.Now()}
	for _, user := range live {
		c.Owners[user] = cliworker.IdentityForTest(user)
	}
	for user, id := range owners {
		c.Owners[user] = id
	}
	return c
}

func otherAccount(user int64) cliowner.Account {
	return cliowner.Account{User: user, Identity: fmt.Sprintf("%031xe", user), Boot: cliworker.TestBoot, Installation: cliworker.TestInstallation}
}

func mine(t *testing.T, dirs cliworker.Dirs) string {
	t.Helper()
	if err := os.MkdirAll(dirs.Work, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dirs.Work, "mine.txt")
	if err := os.WriteFile(p, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnAccountGivenANumberAgainGetsAHomeOfItsOwn(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	oldFile := mine(t, h.user(t, 3, cliworker.Claude, clitest.Scenario{}, true))
	next := otherAccount(3)
	st, err := h.w.Status(ctx, next, cliworker.Claude, "")
	if err != nil || st.LoggedIn {
		t.Fatalf("the new account of number 3: %+v %v, want not signed in", st, err)
	}
	newHome, oldHome := h.w.HomeForTest(next), h.w.HomeForTest(acct(3))
	if newHome == oldHome {
		t.Fatal("the new account was given the old one's home")
	}
	if _, err := os.Stat(filepath.Join(h.layout.Home(newHome).Work, "mine.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the new account finds the old one's file: %v", err)
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Errorf("the old account's file went: %v", err)
	}
	if st, err := h.w.Status(ctx, acct(3), cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the old account's own request: %+v %v", st, err)
	}
	if login, ok := h.w.Login(next, cliworker.Claude, ""); ok {
		t.Errorf("the new account sees a sign-in: %+v", login.Info())
	}
}

func TestNothingBringsBackARetiredHome(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	dirs := h.user(t, 4, cliworker.Claude, clitest.Scenario{}, true)
	home := h.w.HomeForTest(acct(4))
	if err := h.w.Forget(ctx, acct(4)); err != nil {
		t.Fatal(err)
	}
	gone(t, dirs.Root)
	for name, err := range map[string]error{
		"a status check": func() error { _, err := h.w.Status(ctx, acct(4), cliworker.Claude, ""); return err }(),
		"a sign-in":      func() error { _, err := h.w.StartLogin(acct(4), cliworker.Claude, ""); return err }(),
		"a turn":         func() error { _, _, err := h.w.Start(spec("back", 4, cliworker.Claude, "hi")); return err }(),
		"its browser":    h.w.BrowserAllowed(acct(4)),
	} {
		if err == nil {
			t.Errorf("%s of a retired account went through", name)
		}
	}
	built := h.w.HelpersBuiltForTest()
	host := h.w.BrowserHost(map[browser.Program]string{browser.Xvfb: "/bin/sh"})
	if _, err := host.Start(acct(4), browser.ProcSpec{Program: browser.Xvfb, Kind: browser.Personal}, nil); err == nil {
		t.Error("a browser started for a retired account")
	}
	if _, err := host.DialVNC(ctx, acct(4)); err == nil {
		t.Error("a live view was dialled for a retired account")
	}
	if h.w.HelpersBuiltForTest() != built {
		t.Error("a helper was made for a retired account")
	}
	gone(t, dirs.Root)
	for user := int64(10); user < 15; user++ {
		if h.w.HomeForTest(acct(user)) == home {
			t.Fatalf("account %d was given a retired account's home", user)
		}
	}
}

func TestAHomeNumberIsNeverGivenTwice(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.introduce(t, 1)
	first := h.w.HomeForTest(acct(1))
	crashed := true
	h.w.SetAllocateHookForTest(func() error {
		if crashed {
			crashed = false
			return errors.New("the worker stopped here")
		}
		return nil
	})
	if _, err := h.w.Status(ctx, acct(2), cliworker.Claude, ""); err == nil {
		t.Fatal("fixture: the allocation didn't stop")
	}
	if _, err := os.Stat(h.w.AccountFileForTest(cliworker.IdentityForTest(2))); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an account was recorded with the home its allocation didn't finish: %v", err)
	}
	if _, err := h.w.Status(ctx, acct(2), cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	second := h.w.HomeForTest(acct(2))
	if second != first+2 {
		t.Errorf("the home after a crash = %d, want %d (the one taken by the crash skipped)", second, first+2)
	}
	h.w.Close()
	again, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	if again.HomeForTest(acct(1)) != first || again.HomeForTest(acct(2)) != second {
		t.Error("a worker started again gave recorded accounts other homes")
	}
	if third := again.HomeForTest(acct(3)); third <= second {
		t.Errorf("a worker started again gave home %d after %d", third, second)
	}
}

func TestLocalModeAdoptsADirectoryFromBeforeHomesOnce(t *testing.T) {
	for _, stop := range []string{"before the move", "after the move"} {
		t.Run(stop, func(t *testing.T) {
			h := newHarness(t, func(c *cliworker.Config) { c.AdoptLegacy = true })
			ctx := context.Background()
			legacy := h.layout.LegacyDirForTest(7)
			config := filepath.Join(legacy, "config", "claude")
			if err := os.MkdirAll(config, 0o700); err != nil {
				t.Fatal(err)
			}
			clitest.SignIn(t, "claude", config)
			stopped := true
			h.w.SetAdoptHookForTest(func() error {
				if stopped {
					stopped = false
					return errors.New("the worker stopped here")
				}
				return nil
			})
			if stop == "before the move" {
				if err := os.Chmod(h.layout.Root, 0o500); err != nil {
					t.Fatal(err)
				}
				_, err := h.w.Status(ctx, acct(7), cliworker.Claude, "")
				_ = os.Chmod(h.layout.Root, 0o700)
				if err == nil && os.Geteuid() != 0 {
					t.Fatal("fixture: the move went through")
				}
				stopped = false
			} else if _, err := h.w.Status(ctx, acct(7), cliworker.Claude, ""); err == nil {
				t.Fatal("fixture: the worker didn't stop after the move")
			}
			st, err := h.w.Status(ctx, acct(7), cliworker.Claude, "")
			if err != nil || !st.LoggedIn {
				t.Fatalf("the account's own login after the stop: %+v %v", st, err)
			}
			gone(t, legacy)
			if st, err := h.w.Status(ctx, otherAccount(7), cliworker.Claude, ""); err != nil || st.LoggedIn {
				t.Errorf("another account of number 7: %+v %v", st, err)
			}
		})
	}
}

func TestContainerModeRemovesDirectoriesFromBeforeHomes(t *testing.T) {
	h := newHarness(t, nil)
	legacy := h.layout.LegacyDirForTest(7)
	if err := os.MkdirAll(filepath.Join(legacy, "config", "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(context.Background(), census(nil, 7)); err != nil {
		t.Fatal(err)
	}
	gone(t, legacy)
	if st, err := h.w.Status(context.Background(), acct(7), cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the account of number 7: %+v %v", st, err)
	}
}

func bootCensus(boot string, before time.Time, owners map[int64]string) cliworker.Census {
	if owners == nil {
		owners = map[int64]string{}
	}
	return cliworker.Census{Owners: owners, Boot: boot, Installation: cliworker.TestInstallation, Before: before}
}

func TestReconcileRetiresTheAccountsThatAreGone(t *testing.T) {
	h := newHarness(t, nil)
	browsers := &fakeBrowsers{stopped: map[string]bool{}}
	h.w.SetBrowsers(browsers)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	live := mine(t, h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true))
	goneDirs := h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true)
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, start, map[int64]string{1: cliworker.IdentityForTest(1), 2: cliworker.IdentityForTest(2)})); err != nil {
		t.Fatal(err)
	}
	other := cliowner.Account{User: 3, Identity: strings.Repeat("3", 32), Boot: strings.Repeat("9", 32), Installation: cliworker.TestInstallation}
	if _, err := h.w.Status(ctx, other, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(other.Boot, start.Add(time.Minute), map[int64]string{1: cliworker.IdentityForTest(1), 2: cliworker.IdentityForTest(2), 3: other.Identity})); err != nil {
		t.Fatal(err)
	}
	again := bootCensus(strings.Repeat("a", 32), time.Now(), map[int64]string{1: cliworker.IdentityForTest(1)})
	since := acct(4)
	since.Boot = again.Boot
	if _, err := h.w.Status(ctx, since, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, again); err != nil {
		t.Fatal(err)
	}
	coming := cliowner.Account{User: 5, Identity: strings.Repeat("5", 32), Boot: strings.Repeat("e", 32), Installation: cliworker.TestInstallation}
	if _, err := h.w.Status(ctx, coming, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, again); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the live account's file went: %v", err)
	}
	gone(t, goneDirs.Root)
	for name, a := range map[string]cliowner.Account{"gone": acct(2), "another server's": other} {
		if _, err := h.w.Status(ctx, a, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
			t.Errorf("the %s account: %v, want ErrForgotten", name, err)
		}
		if !browsers.stopped[a.Identity] {
			t.Errorf("the %s account's browsers weren't stopped", name)
		}
	}
	for name, a := range map[string]cliowner.Account{"made by this server since its census": since, "of a server whose census hasn't come": coming} {
		if _, err := h.w.Status(ctx, a, cliworker.Claude, ""); err != nil {
			t.Errorf("an account %s: %v", name, err)
		}
	}
	if browsers.again == nil || !browsers.again(acct(1).Identity) || browsers.again(acct(2).Identity) {
		t.Error("the browsers' refusals weren't taken back for the live accounts only")
	}
}

func TestTheIdentityNeverReachesACLI(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 5, cliworker.Claude, clitest.Scenario{Probes: []string{"self"}}, true)
	done := last(run(t, h.w, spec("self", 5, cliworker.Claude, "hi")))
	if done.Status != cliworker.StatusOK {
		t.Fatalf("the turn: %+v", done)
	}
	id := cliworker.IdentityForTest(5)
	if self := probeResult(done.Text, "self"); !strings.HasPrefix(self, "ok ") || strings.Contains(self, id) {
		t.Errorf("the CLI's environment, arguments or working directory hold the identity: %q", self)
	}
	err := filepath.WalkDir(dirs.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(p, id) {
			t.Errorf("a path in the home names the identity: %s", p)
		}
		if d.Type().IsRegular() {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), id) {
				t.Errorf("%s holds the identity", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type fakeBrowsers struct {
	mu      sync.Mutex
	stopped map[string]bool
	again   func(string) bool
}

func (f *fakeBrowsers) StopAll(_ context.Context, a cliowner.Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[a.Identity] = true
	return nil
}

func (f *fakeBrowsers) Unforget(again func(string) bool, _ time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.again = again
}

func TestABrowserPurgeOfAnAccountWithNoHomeDoesNothing(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a := cliowner.Account{User: 8, Identity: cliworker.IdentityForTest(8), Boot: cliworker.TestBoot, Installation: cliworker.TestInstallation}
	host := h.w.BrowserHost(map[browser.Program]string{})
	built := h.w.HelpersBuiltForTest()
	if err := h.w.FenceDeletion(ctx, a, func() error { return host.Purge(a) }); err != nil {
		t.Errorf("the purge of an account with no home: %v", err)
	}
	if h.w.HelpersBuiltForTest() != built {
		t.Error("a helper was made for an account with no home")
	}
	if err := os.MkdirAll(filepath.Join(h.layout.Root, "h0"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := host.Purge(a); err != nil {
		t.Errorf("the purge again, with h0 there: %v", err)
	}
}
