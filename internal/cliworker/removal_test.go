package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// started waits for the fake CLI of the turn running in configDir to have started.
func started(t *testing.T, configDir string) {
	t.Helper()
	eventually(t, "the CLI didn't start", func() bool { return len(clitest.Calls(t, configDir)) > 0 })
}

// Forgetting a deleted user while a turn runs on their login stops the turn first, waits for
// its CLI to end, and only then removes the files: the CLI can't go on with the login it was
// started with, or write its session back into a directory that is gone. Another user's
// running turn, and their files, are not touched.
func TestForgetStopsTheRunningTurnsBeforeRemovingFiles(t *testing.T) {
	h := newHarness(t, nil)
	mine := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000, Background: true}, true)
	theirs := h.user(t, 2, cliworker.Claude, clitest.Scenario{SleepMS: 1500}, true)

	turn, _, err := h.w.Start(spec("mine", 1, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := h.w.Start(spec("theirs", 2, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	started(t, mine.Config(cliworker.Claude))
	started(t, theirs.Config(cliworker.Claude))
	eventually(t, "the background process didn't start", func() bool {
		_, err := os.Stat(mine.Config(cliworker.Claude) + "/background.pid")
		return err == nil
	})
	cliPID := clitest.Calls(t, mine.Config(cliworker.Claude))[0].PID
	bg := backgroundPID(t, mine.Config(cliworker.Claude))

	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// Forget has returned: nothing of the turn may still run.
	if !turn.Over() {
		t.Fatal("Forget returned while the user's turn was still running")
	}
	if alive(cliPID) || alive(bg) {
		t.Error("a process of the forgotten user's turn is still running")
	}
	if _, err := os.Stat(mine.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the user's directory remains: %v", err)
	}
	// And it stays removed: no CLI is left to write into it.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(mine.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory came back: %v", err)
	}
	events, _, _ := turn.Events(0)
	if done := last(events); done.Status != cliworker.StatusStopped {
		t.Errorf("the turn ended %+v, want stopped", done)
	}

	// Another user: their turn ran through it, and ends on its own.
	if other.Over() {
		t.Error("forgetting one user stopped another's turn")
	}
	if done := last(waitDone(t, other, 15*time.Second)); done.Status != cliworker.StatusOK {
		t.Errorf("the other user's turn = %+v", done)
	}
	if st, _ := h.w.Status(context.Background(), 2, cliworker.Claude, ""); !st.LoggedIn {
		t.Error("forgetting one user signed another out")
	}
}

// While a user's files are removed, no turn and no sign-in starts for them; a turn of someone
// else does, and the user's accounts are free again when the removal is over.
func TestNothingStartsWhileAUsersFilesAreRemoved(t *testing.T) {
	h := newHarness(t, nil)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true)

	end, err := h.w.BeginRemovalForTest(context.Background(), 1, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.w.Start(spec("late", 1, cliworker.Claude, "hi")); !errors.Is(err, cliworker.ErrRemoving) {
		t.Errorf("a turn started during the removal: %v", err)
	}
	if _, err := h.w.StartLogin(1, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrRemoving) {
		t.Errorf("a sign-in started during the removal: %v", err)
	}
	if done := last(run(t, h.w, spec("fine", 2, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Errorf("another user's turn = %+v", done)
	}
	end()
	if done := last(run(t, h.w, spec("after", 1, cliworker.Claude, "hi"))); done.Status != cliworker.StatusOK {
		t.Errorf("a turn after the removal = %+v", done)
	}
}

// A removal that can't stop a turn keeps the files and says so, and lets turns start again.
func TestRemovalKeepsFilesWhenATurnCannotBeWaitedFor(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true)
	turn, _, err := h.w.Start(spec("held", 1, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	started(t, dirs.Config(cliworker.Claude))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller gave up before the turn ended
	if err := h.w.Forget(ctx, 1); err == nil {
		t.Fatal("Forget removed files it couldn't make safe")
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("the files went although the wait failed: %v", err)
	}
	// The turn was told to stop; a second Forget finishes the job.
	waitDone(t, turn, 15*time.Second)
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the user's directory remains: %v", err)
	}
}

// Disconnecting an account stops the turns running on it before its directory goes, and leaves
// the same user's other account, and other users' accounts, alone.
func TestLogoutStopsTheTurnsOnTheAccount(t *testing.T) {
	h := newHarness(t, nil)
	first := h.user(t, 1, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true)
	second, err := h.layout.EnsureSlot(1, cliworker.Claude, "2")
	if err != nil {
		t.Fatal(err)
	}
	second2 := second.ConfigOf(cliworker.Claude, "2")
	clitest.SignIn(t, "claude", second2)
	clitest.WriteScenario(t, second2, clitest.Scenario{SleepMS: 60_000})
	third := h.user(t, 2, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true)

	onFirst, _, err := h.w.Start(spec("first", 1, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	s := spec("second", 1, cliworker.Claude, "work")
	s.Slot = "2"
	onSecond, _, err := h.w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	onThird, _, err := h.w.Start(spec("third", 2, cliworker.Claude, "work"))
	if err != nil {
		t.Fatal(err)
	}
	started(t, first.Config(cliworker.Claude))
	started(t, second2)
	started(t, third.Config(cliworker.Claude))

	if err := h.w.Logout(context.Background(), 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if !onFirst.Over() {
		t.Error("Logout returned while a turn on the account was running")
	}
	if onSecond.Over() || onThird.Over() {
		t.Error("disconnecting one account stopped a turn on another")
	}
	h.w.Stop("second")
	h.w.Stop("third")
	waitDone(t, onSecond, 15*time.Second)
	waitDone(t, onThird, 15*time.Second)
}
