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
	"sync"
	"syscall"
	"testing"
	"time"

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

// After a restore of the server's data, the worker may hold directories of a history the restore
// went back from: the server's census at its start says which numbers are live users and which
// come next, and Reconcile removes every directory that isn't a live user's own, so no one given a
// number again gets another person's logins and files, while nothing of a live user is removed.
func TestReconcileRemovesTheDirectoriesThatAreNoLiveUsersOwn(t *testing.T) {
	h := newHarness(t, nil)
	browsers := &fakeBrowsers{layout: h.layout, stopped: map[int64]bool{}}
	h.w.SetBrowsers(browsers)
	ctx := context.Background()
	now := time.Now()
	hourAgo, twoHoursAgo := now.Add(-time.Hour), now.Add(-2*time.Hour)

	files := map[int64]string{}
	for user := int64(1); user <= 9; user++ {
		files[user] = mine(t, h.user(t, user, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true))
	}
	// When the worker first acted for each, where it recorded it; the others' directories are from
	// before it recorded that.
	for user, at := range map[int64]time.Time{
		1: hourAgo,              // live, made after the account: theirs
		2: twoHoursAgo,          // live, but made before the account: someone else's, of a lost history
		4: twoHoursAgo,          // live, with no time of the account sent: not checked
		6: now.Add(time.Minute), // a number from next up, made after the census: a person added since
	} {
		setTime(t, h.w.NoteBirthForTest(user), at)
	}
	// 5 had the number in a lost history, and a turn of theirs is still running.
	turn, _, err := h.w.Start(spec("lost", 5, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, turn, cliworker.EventTool, "")
	setTime(t, h.w.NoteBirthForTest(5), hourAgo)
	// 8 and 9 are live, their directories from before the worker recorded when it made them: 8's is
	// older than the account, 9's isn't.
	setTime(t, h.layout.User(8).Root, twoHoursAgo)

	census := cliworker.Census{
		Live:    []int64{1, 2, 4, 8, 9},
		Created: map[int64]time.Time{1: twoHoursAgo, 2: hourAgo, 8: hourAgo, 9: hourAgo},
		Next:    5,
		Before:  now,
	}
	if err := h.w.Reconcile(ctx, census); err != nil {
		t.Fatal(err)
	}

	kept, removed := []int64{1, 4, 6, 9}, []int64{2, 3, 5, 7, 8}
	for _, user := range kept {
		if _, err := os.Stat(files[user]); err != nil {
			t.Errorf("user %d's own file was removed: %v", user, err)
		}
	}
	for _, user := range removed {
		gone(t, h.layout.User(user).Root)
	}
	if !turn.Over() {
		t.Error("the lost history's turn still runs in the directory that went")
	}
	// Their browsers were stopped before the directory went, and nobody else's.
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
	// The browsers' refusals go as the marks do: for live users and numbers from next up.
	if browsers.again == nil || !browsers.before.Equal(now) {
		t.Fatal("the browsers' refusals weren't reconciled")
	}
	for user, want := range map[int64]bool{1: true, 2: true, 3: false, 4: true, 5: true, 9: true, 100: true} {
		if got := browsers.again(user); got != want {
			t.Errorf("browser refusal of %d taken back = %v, want %v", user, got, want)
		}
	}

	// The account that is gone for good stays refused; a live user whose directory went, and a
	// number to be given again, start afresh, with nothing of anyone's.
	if _, err := h.w.Status(ctx, 3, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("an account gone for good: %v", err)
	}
	for _, user := range []int64{2, 5} {
		st, err := h.w.Status(ctx, user, cliworker.Claude, "")
		if err != nil || st.LoggedIn {
			t.Errorf("user %d after the directory went: %+v %v, want not signed in", user, st, err)
		}
		if _, err := os.Stat(files[user]); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("user %d finds the old file: %v", user, err)
		}
	}

	// Told again (the next start), nothing more goes: 2 has a directory of their own now.
	census.Before = time.Now()
	if err := h.w.Reconcile(ctx, census); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.layout.User(2).Root); err != nil {
		t.Errorf("the live user's new directory went: %v", err)
	}
	for _, user := range kept {
		if _, err := os.Stat(files[user]); err != nil {
			t.Errorf("user %d's own file was removed the second time: %v", user, err)
		}
	}
}

// A directory that can't be removed yet (a process of it doesn't end in time) is kept, and its
// user refused until a later Reconcile removes it: the answer is ErrIncomplete, so the server goes
// on and tries again.
func TestReconcileRefusesWhatItCouldNotRemoveYet(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(300 * time.Millisecond)
	ctx := context.Background()
	file := mine(t, h.user(t, 7, cliworker.Claude, clitest.Scenario{}, true))
	leftover := exec.Command("sleep", "60")
	leftover.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leftover.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leftover.Process.Kill(); _ = leftover.Wait() })
	h.w.TrackGroupForTest(leftover.Process.Pid, 7, cliworker.Claude, "")

	census := cliworker.Census{Live: []int64{1}, Next: 2, Before: time.Now()}
	err := h.w.Reconcile(ctx, census)
	if !errors.Is(err, cliworker.ErrIncomplete) {
		t.Fatalf("Reconcile = %v, want ErrIncomplete", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a file went while a process of its user ran: %v", err)
	}
	if _, err := h.w.StartLogin(7, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the number whose directory is left is served: %v", err)
	}

	_ = leftover.Process.Kill()
	_ = leftover.Wait()
	census.Before = time.Now().Add(time.Second) // the next attempt reads its database after the mark
	if err := h.w.Reconcile(ctx, census); err != nil {
		t.Fatal(err)
	}
	gone(t, h.layout.User(7).Root)
	if _, err := h.w.Status(ctx, 7, cliworker.Claude, ""); err != nil {
		t.Errorf("the number, now clear, is still refused: %v", err)
	}
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

// Over the worker's API the census arrives whole, the account times too, and a Reconcile that
// couldn't remove everything yet answers ErrIncomplete, not the error of one of its users.
func TestRemoteReconcile(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	ctx := context.Background()
	file := mine(t, h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true))
	setTime(t, h.w.NoteBirthForTest(2), time.Now().Add(-2*time.Hour))
	census := cliworker.Census{Live: []int64{2}, Created: map[int64]time.Time{2: time.Now().Add(-time.Hour)}, Next: 3, Before: time.Now()}
	if err := r.Reconcile(ctx, census); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the account's time didn't arrive: the directory made before it is still there (%v)", err)
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
	census.Before = time.Now()
	if err := r.Reconcile(ctx, census); !errors.Is(err, cliworker.ErrIncomplete) || errors.Is(err, cliworker.ErrBusy) {
		t.Errorf("Reconcile over the API = %v, want ErrIncomplete alone", err)
	}
}
