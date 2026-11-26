//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// fakeBrowsers is the agent browsers as the worker sees them: it records whose it stopped, and
// whether their directory was still there then (their profile is in it, so it must be).
type fakeBrowsers struct {
	layout cliworker.Layout
	mu     sync.Mutex
	// stopped holds, per user, whether the directory was there when their browsers were stopped.
	stopped map[int64]bool
	again   func(int64) bool
	before  time.Time
}

func (f *fakeBrowsers) StopAll(_ context.Context, user int64) error {
	_, err := os.Lstat(f.layout.User(user).Root)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[user] = err == nil
	return nil
}

func (f *fakeBrowsers) Unforget(again func(int64) bool, before time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.again, f.before = again, before
}

// mine puts a file of the user's own in their working directory.
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

func setTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// legacy makes user's directory as a worker from before owners were recorded made it: with no
// owner, signed in, and a file of theirs.
func legacy(t *testing.T, h *harness, user int64) string {
	t.Helper()
	dirs, err := h.layout.Ensure(user)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	return mine(t, dirs)
}

// otherBoot is the boot of another server than the one the census comes from.
const otherBoot = "0000000000000000000000000000ab00"

// census is what the tests' server says: live users with the identities they were introduced
// with, unless given.
func census(next int64, owners map[int64]string, live ...int64) cliworker.Census {
	c := cliworker.Census{Live: live, Owners: map[int64]string{}, Next: next, Boot: cliworker.TestBoot, Before: time.Now()}
	for _, user := range live {
		c.Owners[user] = cliworker.IdentityForTest(user)
	}
	for user, id := range owners {
		c.Owners[user] = id
	}
	return c
}

// After a restore of the server's data, the worker may hold directories of a history the restore
// went back from. Reconcile keeps a directory only for the account that owns it, by its identity,
// whatever any clock or file time says: a number given to another account, a number from next up
// that this server didn't introduce, and an account gone for good all lose theirs, with their
// browsers and running turns stopped first; the live accounts' own stay.
func TestReconcileKeepsADirectoryOnlyForItsOwner(t *testing.T) {
	h := newHarness(t, nil)
	browsers := &fakeBrowsers{layout: h.layout, stopped: map[int64]bool{}}
	h.w.SetBrowsers(browsers)
	ctx := context.Background()

	files := map[int64]string{}
	for _, user := range []int64{1, 2, 3, 5} {
		files[user] = mine(t, h.user(t, user, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true))
	}
	// 6 was introduced by another server (the one before the restore), 4 and 7 by a worker from
	// before owners were recorded.
	if err := h.w.Introduce(ctx, 6, cliworker.IdentityForTest(6), otherBoot); err != nil {
		t.Fatal(err)
	}
	files[6] = mine(t, h.layout.User(6))
	files[4], files[7] = legacy(t, h, 4), legacy(t, h, 7)
	// 2's number was given to someone else since; a turn of the old owner's still runs.
	turn, _, err := h.w.Start(spec("lost", 2, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, turn, cliworker.EventTool, "")
	// No clock decides: the times say the opposite of who owns what.
	for user, f := range files {
		at := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
		if user == 2 || user == 6 || user == 7 {
			at = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		setTime(t, f, at)
		setTime(t, h.layout.User(user).Root, at)
	}

	c := census(5, map[int64]string{2: strings.Repeat("e", 32)}, 1, 2, 4)
	c.AdoptUnowned = true // local mode
	if err := h.w.Reconcile(ctx, c); err != nil {
		t.Fatal(err)
	}

	// 1: the live account's own. 4: from before owners, adopted (local mode). 5: from next up,
	// introduced by this server since it read its database.
	kept, removed := []int64{1, 4, 5}, []int64{2, 3, 6, 7}
	for _, user := range kept {
		if _, err := os.Stat(files[user]); err != nil {
			t.Errorf("user %d's own file was removed: %v", user, err)
		}
	}
	for _, user := range removed {
		gone(t, h.layout.User(user).Root)
		gone(t, h.w.OwnerFileForTest(user))
	}
	if b, err := os.ReadFile(h.w.OwnerFileForTest(4)); err != nil || !strings.HasPrefix(string(b), cliworker.IdentityForTest(4)) {
		t.Errorf("the adopted directory's owner = %q %v", b, err)
	}
	if !turn.Over() {
		t.Error("the old owner's turn still runs")
	}
	var stopped []int64
	for user, there := range browsers.stopped {
		stopped = append(stopped, user)
		if !there {
			t.Errorf("user %d's browsers were stopped after their directory went", user)
		}
	}
	slices.Sort(stopped)
	if !slices.Equal(stopped, removed) {
		t.Errorf("browsers stopped for %v, want %v", stopped, removed)
	}
	if browsers.again == nil {
		t.Fatal("the browsers' refusals weren't reconciled")
	}
	for user, want := range map[int64]bool{1: true, 2: true, 3: false, 5: true, 100: true} {
		if got := browsers.again(user); got != want {
			t.Errorf("browser refusal of %d taken back = %v, want %v", user, got, want)
		}
	}

	// The account gone for good stays refused. The account given number 2 is unknown until the
	// server introduces it, and then starts with nothing of anyone's.
	if _, err := h.w.Status(ctx, 3, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("an account gone for good: %v", err)
	}
	if _, err := h.w.Status(ctx, 2, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrUnknownOwner) {
		t.Errorf("the number's new account before it was introduced: %v", err)
	}
	if err := h.w.Introduce(ctx, 2, strings.Repeat("e", 32), cliworker.TestBoot); err != nil {
		t.Fatal(err)
	}
	if st, err := h.w.Status(ctx, 2, cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the number's new account: %+v %v, want not signed in", st, err)
	}

	// Told again (the next start), nothing more goes.
	c.Before = time.Now()
	if err := h.w.Reconcile(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, user := range kept {
		if _, err := os.Stat(files[user]); err != nil {
			t.Errorf("user %d's own file was removed the second time: %v", user, err)
		}
	}
}

// A directory from before owners were recorded can't be told from another history's where the
// worker's directories aren't in the server's backups (container mode): it is removed, never handed
// to the live account of its number, which starts afresh. Where they are (local mode), it is that
// account's, and adopted.
func TestReconcileAdoptsADirectoryWithNoOwnerOnlyWhenTold(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		h := newHarness(t, nil)
		ctx := context.Background()
		file := legacy(t, h, 1)
		c := census(2, nil, 1)
		c.AdoptUnowned = adopt
		if err := h.w.Reconcile(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := h.w.Introduce(ctx, 1, cliworker.IdentityForTest(1), cliworker.TestBoot); err != nil {
			t.Fatal(err)
		}
		st, err := h.w.Status(ctx, 1, cliworker.Claude, "")
		if err != nil {
			t.Fatal(err)
		}
		_, ferr := os.Stat(file)
		if !adopt && (st.LoggedIn || !errors.Is(ferr, fs.ErrNotExist)) {
			t.Errorf("the live account was given a directory with no owner: signed in %v, file %v", st.LoggedIn, ferr)
		}
		if adopt && (!st.LoggedIn || ferr != nil) {
			t.Errorf("told to adopt, the live account lost its own directory: signed in %v, file %v", st.LoggedIn, ferr)
		}
	}
}

// Introducing an account whose number's directory is someone else's removes that one first, with
// the sign-ins, turns and permission questions the worker held for it; introducing the owner again
// changes nothing; nothing starts for an account the server hasn't introduced.
func TestIntroduceRemovesADirectoryThatIsNotTheAccounts(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, err := h.w.Status(ctx, 9, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrUnknownOwner) {
		t.Errorf("a status check before the account was introduced: %v", err)
	}
	if _, _, err := h.w.Start(spec("unknown", 9, cliworker.Claude, "hi")); !errors.Is(err, cliworker.ErrUnknownOwner) {
		t.Errorf("a turn before the account was introduced: %v", err)
	}
	if _, err := h.w.StartLogin(9, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrUnknownOwner) {
		t.Errorf("a sign-in before the account was introduced: %v", err)
	}

	old := mine(t, h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true))
	if done := last(run(t, h.w, spec("old-turn", 1, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Fatalf("the old owner's turn: %+v", done)
	}
	h.user(t, 1, cliworker.Codex, clitest.Scenario{}, false)
	if _, err := h.w.StartLogin(1, cliworker.Codex, ""); err != nil {
		t.Fatal(err)
	}
	// Again by the same account: nothing changes.
	if err := h.w.Introduce(ctx, 1, cliworker.IdentityForTest(1), cliworker.TestBoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("introducing the owner again removed their file: %v", err)
	}

	next := strings.Repeat("d", 32)
	if err := h.w.Introduce(ctx, 1, next, cliworker.TestBoot); err != nil {
		t.Fatal(err)
	}
	gone(t, old)
	if st, err := h.w.Status(ctx, 1, cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the new account: %+v %v, want not signed in", st, err)
	}
	if _, ok := h.w.Login(1, cliworker.Codex, ""); ok {
		t.Error("the old owner's sign-in is still there for the new account")
	}
	if _, ok := h.w.Turn("old-turn"); ok {
		t.Error("the old owner's turn is still there for the new account")
	}
	if qs := h.w.Permissions(1, 7); len(qs) != 0 {
		t.Errorf("the old owner's questions: %+v", qs)
	}

	// A forgotten account is refused.
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Introduce(ctx, 1, next, cliworker.TestBoot); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("introducing a forgotten account: %v", err)
	}
}

// A directory with read-only parts (an archive's, a tool's) is removed all the same: the worker
// takes it away from its path first, then deletes it as itself, and a link in it is removed, not
// followed.
func TestReconcileRemovesAReadOnlyDirectory(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	dirs := h.user(t, 3, cliworker.Claude, clitest.Scenario{}, true)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("not the user's"), 0o600); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(dirs.Work, "archive", "inner")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "file"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ro, "link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ro, filepath.Dir(ro)} {
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.w.Reconcile(ctx, census(4, nil, 1)); err != nil {
		t.Fatal(err)
	}
	gone(t, dirs.Root)
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file a link pointed to went: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.layout.Root, ".worker", "trash"))
	if len(entries) != 0 {
		t.Errorf("left in the trash: %v", entries)
	}
}

// A directory that can't be removed yet (a process of it doesn't end in time) is kept, and its
// user refused, the agent browser too, until a later Reconcile removes it: the answer is
// ErrIncomplete, so the server goes on and tries again.
func TestReconcileRefusesWhatItCouldNotRemoveYet(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(300 * time.Millisecond)
	ctx := context.Background()
	file := mine(t, h.user(t, 7, cliworker.Claude, clitest.Scenario{}, true))
	if err := h.w.BrowserAllowed(7); err != nil {
		t.Fatalf("fixture: the browser of an introduced account: %v", err)
	}
	leftover := exec.Command("sleep", "60")
	leftover.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leftover.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leftover.Process.Kill(); _ = leftover.Wait() })
	h.w.TrackGroupForTest(leftover.Process.Pid, 7, cliworker.Claude, "")

	c := census(2, nil, 1)
	c.Boot = otherBoot // the next server's: 7 is of another history
	err := h.w.Reconcile(ctx, c)
	if !errors.Is(err, cliworker.ErrIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrIncomplete", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a file went while a process of its user ran: %v", err)
	}
	if _, err := h.w.StartLogin(7, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the number whose directory is left is served: %v", err)
	}
	if err := h.w.BrowserAllowed(7); !errors.Is(err, browser.ErrAccountClosing) {
		t.Errorf("the browser of the number whose directory is left: %v", err)
	}

	_ = leftover.Process.Kill()
	_ = leftover.Wait()
	c.Before = time.Now().Add(time.Second) // the next attempt reads its database after the mark
	if err := h.w.Reconcile(ctx, c); err != nil {
		t.Fatal(err)
	}
	gone(t, h.layout.User(7).Root)
	if err := h.w.Introduce(ctx, 7, strings.Repeat("c", 32), cliworker.TestBoot); err != nil {
		t.Errorf("the number, now clear, is still refused: %v", err)
	}
}

// Over the worker's API the census and an introduction arrive whole, and a Reconcile that couldn't
// remove everything yet answers ErrIncomplete, not the error of one of its users.
func TestRemoteReconcile(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	ctx := context.Background()
	file := mine(t, h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true))
	if err := r.Reconcile(ctx, census(3, map[int64]string{2: strings.Repeat("e", 32)}, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the account's identity didn't arrive: the old owner's directory is still there (%v)", err)
	}
	if err := r.Introduce(ctx, 2, strings.Repeat("e", 32), cliworker.TestBoot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Status(ctx, 2, cliworker.Claude, ""); err != nil {
		t.Errorf("after the introduction over the API: %v", err)
	}
	if _, err := r.Status(ctx, 4, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrUnknownOwner) {
		t.Errorf("an account not introduced, over the API: %v", err)
	}

	h.w.SetGroupWaitForTest(300 * time.Millisecond)
	h.user(t, 5, cliworker.Claude, clitest.Scenario{}, true)
	leftover := exec.Command("sleep", "60")
	leftover.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leftover.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leftover.Process.Kill(); _ = leftover.Wait() })
	h.w.TrackGroupForTest(leftover.Process.Pid, 5, cliworker.Claude, "")
	c := census(3, map[int64]string{2: strings.Repeat("e", 32)}, 2)
	c.Boot = otherBoot // 5 isn't this server's
	if err := r.Reconcile(ctx, c); !errors.Is(err, cliworker.ErrIncomplete) || errors.Is(err, cliworker.ErrBusy) {
		t.Errorf("Reconcile over the API = %v, want ErrIncomplete alone", err)
	}
}

// The census of a server with many users fits in the worker's API.
func TestRemoteReconcileTakesALargeCensus(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	live := make([]int64, 0, 50_000)
	for user := int64(1); user <= 50_000; user++ {
		live = append(live, user)
	}
	if err := r.Reconcile(context.Background(), census(50_001, nil, live...)); err != nil {
		t.Fatal(err)
	}
}

// A deleted account's purge that comes late, after a restore gave the number to another account,
// reaches nothing of that account: neither its files, its CLIs nor its browsers, which aren't
// refused. The purge names the account it is for (cliowner), and the worker checks it under the
// number's gate. A purge for the account that owns the number still removes its files, and nothing
// is done or marked for a caller that has given up.
func TestALatePurgeNeverReachesTheNumbersNextOwner(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	m := browser.NewManager(browser.Config{Host: h.w.BrowserHost(nil), Allow: h.w.BrowserAllowed})
	t.Cleanup(m.Close)
	h.w.SetBrowsers(m)
	h.user(t, 3, cliworker.Claude, clitest.Scenario{}, true)
	deleted := cliworker.IdentityForTest(3)

	// A restore gave 3 to another account, which signed in and made a browser profile.
	next := strings.Repeat("a", 32)
	if err := h.w.Reconcile(ctx, census(4, map[int64]string{3: next}, 3)); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Introduce(ctx, 3, next, cliworker.TestBoot); err != nil {
		t.Fatal(err)
	}
	dirs, err := h.layout.Ensure(3)
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	profile := filepath.Join(h.layout.BrowserPaths(3).Root, "personal", "Cookies")
	if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("the new account's"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The deleted account's purge comes now: its browser step, then its CLI step.
	late := cliowner.With(ctx, deleted)
	if err := m.Forget(late, 3); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Forget(late, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Errorf("the late purge removed the new account's profile: %v", err)
	}
	if st, err := h.w.Status(ctx, 3, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the new account after the late purge: %+v %v", st, err)
	}
	if _, err := m.Do(ctx, 3, browser.Action{Op: browser.OpTabs, Browser: browser.Personal}); err != nil {
		t.Errorf("the new account's browser is refused after the late purge: %v", err)
	}

	// A caller that has given up does nothing, and marks nothing.
	cancelled, cancel := context.WithCancel(cliowner.With(ctx, next))
	cancel()
	if err := h.w.Forget(cancelled, 3); err == nil {
		t.Error("a cancelled Forget reported done")
	}
	if err := m.Forget(cancelled, 3); err == nil {
		t.Error("a cancelled browser purge reported done")
	}
	if _, err := h.w.Status(ctx, 3, cliworker.Claude, ""); err != nil {
		t.Errorf("a cancelled Forget marked the account: %v", err)
	}
	if _, err := m.Do(ctx, 3, browser.Action{Op: browser.OpTabs, Browser: browser.Personal}); err != nil {
		t.Errorf("a cancelled browser purge refused the account: %v", err)
	}

	// The account's own purge removes its files.
	if err := m.Forget(cliowner.With(ctx, next), 3); err != nil {
		t.Fatal(err)
	}
	gone(t, h.layout.BrowserPaths(3).Root)
	if err := h.w.Forget(cliowner.With(ctx, next), 3); err != nil {
		t.Fatal(err)
	}
	gone(t, h.layout.User(3).Root)
}

// A turn's process group, once the turn is over and the group holds no live process, is no
// longer listed: a group ID the system gives again later is not taken for this account's.
func TestAnEndedTurnsGroupIsNotListed(t *testing.T) {
	h := newHarness(t, nil)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	if done := last(run(t, h.w, spec("one", 1, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Fatalf("the turn: %+v", done)
	}
	if _, err := h.w.Status(context.Background(), 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.w.GroupsForTest() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d process groups are still listed after their turn and helper ended", h.w.GroupsForTest())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A live view's dial that is on its way when a removal of the user's directory begins starts no
// helper: it is registered from its first step, so the removal cancels it and waits for it before
// the files go, and no helper is made while the removal runs.
func TestALiveViewDialDuringARemovalStartsNothing(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.user(t, 4, cliworker.Claude, clitest.Scenario{}, true)
	host := h.w.BrowserHost(nil)
	removed := make(chan error, 1)
	var built int64
	h.w.SetDialHookForTest(func() {
		built = h.w.HelpersBuiltForTest()
		// A restore gave 4 to another account, introduced now: its old directory goes.
		go func() { removed <- h.w.Introduce(ctx, 4, strings.Repeat("f", 32), cliworker.TestBoot) }()
		for !h.w.GatedForTest(4) {
			time.Sleep(time.Millisecond)
		}
	})
	if _, err := host.DialVNC(ctx, 4); err == nil {
		t.Fatal("a live view opened during the removal")
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if n := h.w.HelpersBuiltForTest() - built; n != 0 {
		t.Errorf("%d helpers were made for the dial during the removal", n)
	}
	gone(t, filepath.Join(h.layout.User(4).Root, "work"))
}
