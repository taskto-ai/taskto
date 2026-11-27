//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func (h *harness) runningTurn(t *testing.T, id string, user int64, cli cliworker.CLI, slot cliworker.Slot, s clitest.Scenario) *cliworker.Turn {
	t.Helper()
	dirs, err := h.layout.EnsureSlot(user, cli, slot)
	if err != nil {
		t.Fatal(err)
	}
	config := dirs.ConfigOf(cli, slot)
	clitest.SignIn(t, string(cli), config)
	clitest.WriteScenario(t, config, s)
	sp := spec(id, user, cli, "work")
	sp.Slot = slot
	turn, _, err := h.w.Start(sp)
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, turn, cliworker.EventTool, "")
	return turn
}

func TestLogoutStopsTheTurnsOnTheAccountBeforeTheFilesGo(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1)
	mine := h.runningTurn(t, "mine", 1, cliworker.Claude, "", clitest.Scenario{SleepMS: 60_000, TermWriteMS: 300})
	second := h.runningTurn(t, "second", 1, cliworker.Claude, "2", clitest.Scenario{SleepMS: 60_000})
	other := h.runningTurn(t, "other", 2, cliworker.Claude, "", clitest.Scenario{SleepMS: 60_000})
	config := h.layout.User(1).Config(cliworker.Claude)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Logout(ctx, 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if !mine.Over() {
		t.Fatal("Logout returned while a turn was still running on the account")
	}
	events, _, _ := mine.Events(0)
	if done := last(events); done.Status != cliworker.StatusStopped {
		t.Errorf("the turn's end = %+v, want stopped", done)
	}
	if _, err := os.Stat(filepath.Join(config, clitest.LateWrite)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stopped CLI saved its file into the account's directory after it was removed: %v", err)
	}
	if st, _ := h.w.Status(ctx, 1, cliworker.Claude, ""); st.LoggedIn {
		t.Error("the account is still signed in")
	}
	if second.Over() || other.Over() {
		t.Errorf("another account's turn was stopped: the user's second account over %v, another user's over %v", second.Over(), other.Over())
	}
}

func TestLogoutClosesTheAccountWhileItsFilesGo(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = 10 * time.Second })
	h.w.SetGroupWaitForTest(-1)
	mine := h.runningTurn(t, "mine", 1, cliworker.Claude, "", clitest.Scenario{SleepMS: 60_000, TermWriteMS: 1500})
	h.user(t, 1, cliworker.Codex, clitest.Scenario{}, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	loggedOut := make(chan error, 1)
	go func() { loggedOut <- h.w.Logout(ctx, 1, cliworker.Claude, "") }()
	eventually(t, "the account was never closed to new work", func() bool {
		_, err := h.w.Status(ctx, 1, cliworker.Claude, "")
		return errors.Is(err, cliworker.ErrRemoving)
	})
	if mine.Over() {
		t.Fatal("fixture: the turn ended before the removal's work was seen")
	}
	if _, _, err := h.w.Start(spec("during", 1, cliworker.Claude, "hi")); !errors.Is(err, cliworker.ErrRemoving) {
		t.Errorf("a turn started on the account that is being removed: %v", err)
	}
	if _, err := h.w.StartLogin(1, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrRemoving) {
		t.Errorf("a sign-in started on the account that is being removed: %v", err)
	}
	if st, err := h.w.Status(ctx, 1, cliworker.Codex, ""); err != nil || !st.LoggedIn {
		t.Errorf("the user's other CLI during the removal: %+v %v", st, err)
	}
	if err := <-loggedOut; err != nil {
		t.Fatal(err)
	}
	if _, err := h.w.Status(ctx, 1, cliworker.Claude, ""); err != nil {
		t.Errorf("a check after the removal: %v", err)
	}
	clitest.SignIn(t, "claude", h.layout.User(1).Config(cliworker.Claude))
	if done := last(run(t, h.w, spec("after", 1, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Errorf("a turn after the removal: %+v", done)
	}
}

func TestLogoutCancelsAHelperRequestOnTheAccount(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{StatusSleepMS: 20_000}, true)
	errs := make(chan error, 1)
	go func() {
		_, err := h.w.Status(context.Background(), 1, cliworker.Claude, "")
		errs <- err
	}()
	eventually(t, "the status check didn't start", func() bool {
		_, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.StatusStarted))
		return err == nil
	})
	start := time.Now()
	if err := h.w.Logout(context.Background(), 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Logout waited %v for the check instead of cancelling it", took)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Error("the cancelled check answered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Logout returned while the check was still running")
	}
	if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.StatusFinished)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the check went on and wrote into the removed directory: %v", err)
	}
}

func TestRemovalWaitsForLeftoverProcessesOfTheAccount(t *testing.T) {
	for name, remove := range map[string]func(*cliworker.Worker, context.Context) error{
		"logout": func(w *cliworker.Worker, ctx context.Context) error { return w.Logout(ctx, 1, cliworker.Claude, "") },
		"forget": func(w *cliworker.Worker, ctx context.Context) error { return w.Forget(ctx, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.w.SetGroupWaitForTest(300 * time.Millisecond)
			dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
			leftover := exec.Command("sleep", "60")
			leftover.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := leftover.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = leftover.Process.Kill(); _ = leftover.Wait() })
			h.w.TrackGroupForTest(leftover.Process.Pid, 1, cliworker.Claude, "")

			ctx := context.Background()
			if err := remove(h.w, ctx); !errors.Is(err, cliworker.ErrBusy) {
				t.Fatalf("with a process still running: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")); err != nil {
				t.Errorf("the account's files went while a process of it ran: %v", err)
			}
			if err := leftover.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := remove(h.w, ctx); err != nil {
				t.Fatalf("with only a zombie left: %v", err)
			}
			if name == "forget" {
				gone(t, dirs.Root)
			} else if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the account is still signed in: %v", err)
			}
		})
	}
}

func TestLogoutsOfOneAccountDoNotOverlap(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = 10 * time.Second })
	h.runningTurn(t, "mine", 1, cliworker.Claude, "", clitest.Scenario{SleepMS: 60_000, TermWriteMS: 800})
	ctx := context.Background()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- h.w.Logout(ctx, 1, cliworker.Claude, "") }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(fmt.Errorf("a disconnect: %w", err))
		}
	}
}

func TestForgetWaitsForADisconnectInProgress(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{LogoutSleepMS: 1500}, true)
	loggedOut := make(chan error, 1)
	go func() { loggedOut <- h.w.Logout(context.Background(), 1, cliworker.Claude, "") }()
	eventually(t, "the sign-out didn't start", func() bool {
		_, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.LogoutStarted))
		return err == nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.w.Forget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-loggedOut:
		if err != nil {
			t.Errorf("the disconnect: %v", err)
		}
	default:
		t.Fatal("Forget returned while a disconnect of the user's account was still running")
	}
	gone(t, dirs.Root)
}

func TestLogoutWaitsForACancelledSignInThatANewOneReplaced(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = 10 * time.Second })
	h.w.SetGroupWaitForTest(-1)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{TermWriteMS: 1500}, false)
	old, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	h.w.CancelLogin(1, cliworker.Claude, "")
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
	if err := h.w.Logout(context.Background(), 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*cliworker.Login{"the cancelled one": old, "its replacement": replacement} {
		select {
		case <-l.Done():
		default:
			t.Errorf("Logout returned while %s was still running", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.LateWrite)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the cancelled sign-in saved its file after the account's files were removed: %v", err)
	}
}

func TestAnAlreadyCancelledRemovalRemovesNothing(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	creds := filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	launched := h.w.HelperLaunchesForTest()

	if err := h.w.Logout(cancelled, 1, cliworker.Claude, ""); err == nil {
		t.Error("a disconnect for a caller that had given up went through")
	}
	if _, err := os.Stat(creds); err != nil {
		t.Errorf("the login's files went for a caller that had given up: %v", err)
	}
	if err := h.w.Forget(cancelled, 1); err == nil {
		t.Error("a removal for a caller that had given up went through")
	}
	if _, err := os.Stat(creds); err != nil {
		t.Errorf("the user's files went for a caller that had given up: %v", err)
	}
	if n := h.w.HelperLaunchesForTest(); n != launched {
		t.Errorf("%d helpers were started for callers that had given up", n-launched)
	}
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	gone(t, dirs.Root)
}

func TestAGroupRecordSeenDeadIsNotTakenForItsReplacement(t *testing.T) {
	h := newHarness(t, nil)
	const pgid = 4242
	h.w.TrackGroupForTest(pgid, 1, cliworker.Claude, "")
	seen := h.w.GroupSeqForTest(pgid)
	h.w.TrackGroupForTest(pgid, 1, cliworker.Claude, "")
	if now := h.w.GroupSeqForTest(pgid); now == seen {
		t.Fatal("fixture: the new registration has the old one's number")
	}
	if !h.w.DropStaleGroupForTest(pgid, seen) {
		t.Error("the record of a newer group was dropped because the old one with its ID was seen dead")
	}
	if h.w.DropStaleGroupForTest(pgid, h.w.GroupSeqForTest(pgid)) {
		t.Error("the record that was seen dead was not collected")
	}
}
