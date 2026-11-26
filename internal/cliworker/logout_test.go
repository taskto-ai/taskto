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

// runningTurn prepares user's account of cli in slot (signed in, with scenario s) and starts a
// turn on it that is running its tool when this returns.
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

// Disconnecting an account removes the files its turns use. A turn on it is stopped and waited for
// first, so it can't write its session back into the directory that went; the user's other
// accounts, and other users, are not touched.
func TestLogoutStopsTheTurnsOnTheAccountBeforeTheFilesGo(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	// The CLI takes a moment to stop and saves a file as it goes, as a real one saves its session.
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

// While an account's files go, nothing starts on it (a turn, a sign-in, a check), and the user's
// other accounts go on. Nothing marks the user: once it is over the account works again.
func TestLogoutClosesTheAccountWhileItsFilesGo(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = 10 * time.Second })
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
	mine := h.runningTurn(t, "mine", 1, cliworker.Claude, "", clitest.Scenario{SleepMS: 60_000, TermWriteMS: 1500})
	h.user(t, 1, cliworker.Codex, clitest.Scenario{}, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	loggedOut := make(chan error, 1)
	go func() { loggedOut <- h.w.Logout(ctx, 1, cliworker.Claude, "") }()
	// A check is harmless to ask, and is refused from the moment the account is closed.
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
	// Over: it is a signed-out account again, which starts.
	if _, err := h.w.Status(ctx, 1, cliworker.Claude, ""); err != nil {
		t.Errorf("a check after the removal: %v", err)
	}
	clitest.SignIn(t, "claude", h.layout.User(1).Config(cliworker.Claude))
	if done := last(run(t, h.w, spec("after", 1, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Errorf("a turn after the removal: %+v", done)
	}
}

// A helper request on the account that is running when it is removed (a slow check) is cancelled,
// not waited out, and its answer is an error: what it was reading is going.
func TestLogoutCancelsAHelperRequestOnTheAccount(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(-1) // the wait for the groups would answer what this test is about
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
	default:
		t.Fatal("Logout returned while the check was still running")
	}
	if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), clitest.StatusFinished)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the check went on and wrote into the removed directory: %v", err)
	}
}

// A process a turn left behind is not always dead when the turn ends: a removal waits until no
// process of the account's groups can run, and keeps the files, with ErrBusy, if one still does.
// A zombie (killed, not reaped) holds nothing, so it doesn't count. The same for Forget.
func TestRemovalWaitsForLeftoverProcessesOfTheAccount(t *testing.T) {
	for name, remove := range map[string]func(*cliworker.Worker, context.Context) error{
		"logout": func(w *cliworker.Worker, ctx context.Context) error { return w.Logout(ctx, 1, cliworker.Claude, "") },
		"forget": func(w *cliworker.Worker, ctx context.Context) error { return w.Forget(ctx, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.w.SetGroupWaitForTest(300 * time.Millisecond)
			dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
			// A process in a group of its own, listed as one the worker started for the account.
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
			// Killed and not yet reaped: a zombie, which doesn't hold the removal.
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

// Two disconnects of one account run one after the other, and another account's is not held by
// them.
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
