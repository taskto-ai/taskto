//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// waitEvent waits for an event of type typ on turn, and fails if the turn ends first.
func waitEvent(t *testing.T, turn *cliworker.Turn, typ cliworker.EventType, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	seen := 0
	for {
		events, done, err := turn.Wait(ctx, seen)
		if err != nil {
			t.Fatalf("no %s event (%q) arrived: %v", typ, text, err)
		}
		for _, e := range events {
			if e.Type == typ && strings.Contains(e.Text, text) {
				return
			}
		}
		seen += len(events)
		if done {
			t.Fatalf("the turn ended before a %s event (%q)", typ, text)
		}
	}
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s remains: %v", path, err)
	}
}

// Forgetting a user while a turn of theirs runs: the turn is stopped, and Forget returns only
// once it has ended, so nothing of the user is written into the directory that goes. Without
// that, the CLI keeps running in a directory that is removed under it and writes its session
// and refreshed credentials back after Forget returned.
func TestForgetStopsARunningTurnBeforeTheFilesGo(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	// The CLI takes a moment to stop and saves a file into its directory as it goes, as a real
	// one saves its session: Forget must wait for that, and remove the directory after it.
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000, TermWriteMS: 300}, true)
	turn, _, err := h.w.Start(spec("running", 1, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	// The CLI is running its tool: it has started, on this login.
	waitEvent(t, turn, cliworker.EventTool, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !turn.Over() {
		t.Fatal("Forget returned while the user's turn was still running on their login")
	}
	events, _, _ := turn.Events(0)
	if done := last(events); done.Status != cliworker.StatusStopped {
		t.Errorf("the turn's end = %+v, want stopped", done)
	}
	gone(t, dirs.Root)
}

// A sign-in in progress is stopped and waited for as well: one that ends while the files go could
// write its credentials back.
func TestForgetWaitsForASignInBeforeTheFilesGo(t *testing.T) {
	for _, cli := range []cliworker.CLI{cliworker.Claude, cliworker.Codex} {
		t.Run(string(cli), func(t *testing.T) {
			h := newHarness(t, nil)
			h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
			dirs := h.user(t, 1, cli, clitest.Scenario{TermWriteMS: 300}, false)
			l, err := h.w.StartLogin(1, cli, "")
			if err != nil {
				t.Fatal(err)
			}
			if l.Info().State != cliworker.LoginWaiting {
				t.Fatalf("fixture: the sign-in is %+v", l.Info())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.w.Forget(ctx, 1); err != nil {
				t.Fatal(err)
			}
			select {
			case <-l.Done():
			default:
				t.Fatal("Forget returned while the user's sign-in was still running")
			}
			gone(t, dirs.Root)
		})
	}
}

// A sign-in that was cancelled and replaced by a new one is still a CLI that may be running (the
// login of the account is the new one): Forget waits for it too.
func TestForgetWaitsForACancelledSignInThatANewOneReplaced(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = 10 * time.Second })
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{TermWriteMS: 1500}, false)
	old, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	h.w.CancelLogin(1, cliworker.Claude, "")
	// The replacement stops at once; only the cancelled one takes its time (it read its
	// scenario when it started).
	clitest.WriteScenario(t, dirs.Config(cliworker.Claude), clitest.Scenario{})
	replacement, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old {
		t.Fatal("fixture: the cancelled sign-in was reused")
	}
	select {
	case <-old.Done():
		t.Fatal("fixture: the cancelled sign-in had ended already")
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*cliworker.Login{"the cancelled one": old, "its replacement": replacement} {
		select {
		case <-l.Done():
		default:
			t.Errorf("Forget returned while %s was still running", name)
		}
	}
	gone(t, dirs.Root)
}

// A helper request in flight (here a slow status check, which writes into the directory as it
// ends) is waited for too.
func TestForgetWaitsForAHelperRequestBeforeTheFilesGo(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{StatusSleepMS: 1500}, true)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, _ = h.w.Status(context.Background(), 1, cliworker.Claude, "")
	}()
	// The check is running: the fake says so by the file it writes first.
	eventually(t, "the status check didn't start", func() bool {
		_, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.StatusStarted))
		return err == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	// Whatever the check did on its way out, nothing of it is left once it ends.
	select {
	case <-answered:
	case <-time.After(20 * time.Second):
		t.Fatal("the status check never ended")
	}
	gone(t, dirs.Root)
}

// A turn waiting for a place when the user is forgotten never starts: it would make the
// directory again and run on a login that was just removed.
func TestForgetEndsAQueuedTurnWithoutStartingIt(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.MaxTurnsPerUser = 1 })
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000, TermWriteMS: 200}, true)
	first, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, first, cliworker.EventTool, "")
	queued, _, err := h.w.Start(spec("queued", 1, cliworker.Claude, "more"))
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, queued, cliworker.EventNotice, "waiting for another turn")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !first.Over() || !queued.Over() {
		t.Fatalf("after Forget: running turn over %v, queued turn over %v", first.Over(), queued.Over())
	}
	events, _, _ := queued.Events(0)
	if done := last(events); done.Status == cliworker.StatusOK {
		t.Errorf("the queued turn ran: %+v", done)
	}
	gone(t, dirs.Root)
}

// Nothing starts for a forgotten user: a turn, a sign-in or a status check would each make
// their directory again, with a login's files in it. Another user is not affected, and the
// refusal is the same through the worker's API (container mode).
func TestNothingStartsForAForgottenUser(t *testing.T) {
	h := newHarness(t, nil)
	r := serve(t, h)
	ctx := context.Background()
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true)
	if err := r.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	gone(t, dirs.Root)

	if turn, _, err := h.w.Start(spec("late", 1, cliworker.Claude, "hi")); err == nil {
		t.Errorf("a turn started for a forgotten user: %+v", last(waitDone(t, turn, 20*time.Second)))
	}
	if _, _, err := r.StartTurn(spec("late-remote", 1, cliworker.Claude, "hi")); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("a turn through the worker's API: %v", err)
	}
	if _, err := h.w.StartLogin(1, cliworker.Claude, ""); err == nil {
		t.Error("a sign-in started for a forgotten user")
	}
	if _, err := h.w.Status(ctx, 1, cliworker.Claude, ""); err == nil {
		t.Error("a status check ran for a forgotten user")
	}
	if err := h.w.Logout(ctx, 1, cliworker.Claude, ""); err == nil {
		t.Error("a sign-out ran for a forgotten user")
	}
	gone(t, dirs.Root)

	// Forgetting is still idempotent, and others go on.
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Errorf("forgetting again: %v", err)
	}
	if st, err := h.w.Status(ctx, 2, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("another user: %+v %v", st, err)
	}
	if done := last(run(t, h.w, spec("other", 2, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Errorf("another user's turn: %+v", done)
	}
}

// A worker that restarts remembers who was forgotten: a turn that a replay or a late request
// brings after it must not make the directory again.
func TestForgottenUserSurvivesARestart(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	h.w.Close()
	w, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	if _, _, err := w.Start(spec("after-restart", 1, cliworker.Claude, "hi")); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("a turn after a restart: %v", err)
	}
	gone(t, dirs.Root)
}

// Reconcile takes back the marks a restore of the server's data made wrong, and only those: the
// marks of users that are live, of numbers that come next or later, and none made after the
// server read the database (a purge running now).
func TestReconcileTakesBackOnlyTheMarksARestoreMadeWrong(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	for _, user := range []int64{2, 3, 4, 5} {
		if err := h.w.Forget(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	// 2 is a person whose account is back; 3 is gone for good; 4 and 5 come next and later; 5's
	// purge ran after the server read its database.
	time.Sleep(20 * time.Millisecond)
	snapshot := time.Now()
	time.Sleep(20 * time.Millisecond) // the mark's time is on the filesystem's clock
	if err := h.w.Forget(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, cliworker.Census{Live: []int64{1, 2}, Next: 4, Before: snapshot}); err != nil {
		t.Fatal(err)
	}
	refused := func(user int64) bool {
		_, err := h.w.Status(ctx, user, cliworker.Claude, "")
		return errors.Is(err, cliworker.ErrForgotten)
	}
	for user, want := range map[int64]bool{2: false, 3: true, 4: false, 5: true} {
		if got := refused(user); got != want {
			t.Errorf("user %d refused = %v, want %v", user, got, want)
		}
	}
	// A mark can't be read: the user stays refused rather than started.
	if err := os.Chmod(filepath.Join(h.layout.Root, ".worker"), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(h.layout.Root, ".worker"), 0o700) //nolint:errcheck // the temporary directory is removed anyway
	if os.Geteuid() != 0 && !refused(6) {
		t.Error("a mark that can't be looked up counted as no mark")
	}
}
