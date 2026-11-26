package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
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
	theirs := h.user(t, 2, cliworker.Claude, clitest.Scenario{SleepMS: 60_000}, true)

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

	// Another user: their turn ran through it, and ends only when it is stopped.
	if other.Over() {
		t.Error("forgetting one user stopped another's turn")
	}
	h.w.Stop("theirs")
	if done := last(waitDone(t, other, 15*time.Second)); done.Status != cliworker.StatusStopped {
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

// A status check is a helper that makes directories and writes the CLI's settings: while a
// removal covers the account it is refused, and doesn't bring back what the removal took.
func TestStatusIsRefusedWhileAccountFilesAreRemoved(t *testing.T) {
	h := newHarness(t, nil)
	for _, cli := range []cliworker.CLI{cliworker.Claude, cliworker.Codex} {
		dirs := h.user(t, 1, cli, clitest.Scenario{}, true)
		end, err := h.w.BeginRemovalForTest(context.Background(), 1, true, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dirs.Root); err != nil {
			t.Fatal(err)
		}
		before := h.w.HelperLaunchesForTest()
		if _, err := h.w.Status(context.Background(), 1, cli, ""); !errors.Is(err, cliworker.ErrRemoving) {
			t.Errorf("%s: a status check during the removal = %v, want ErrRemoving", cli, err)
		}
		if h.w.HelperLaunchesForTest() != before {
			t.Errorf("%s: a helper started during the removal", cli)
		}
		if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the status check made the directory again: %v", cli, err)
		}
		end()
	}
}

// A status check already running when a removal begins is stopped and waited for: it can't
// make directories after the removal's.
func TestRemovalDrainsARunningStatusCheck(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{StatusSleepMS: 60_000}, true)
	errc := make(chan error, 1)
	go func() {
		_, err := h.w.Status(context.Background(), 1, cliworker.Claude, "")
		errc <- err
	}()
	started(t, dirs.Config(cliworker.Claude))
	pid := clitest.Calls(t, dirs.Config(cliworker.Claude))[0].PID

	end, err := h.w.BeginRemovalForTest(context.Background(), 1, false, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if alive(pid) {
		t.Error("the removal began with a status check of the account still running")
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Error("the stopped status check answered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the status check didn't return")
	}
}

// Removals of the same account run one after the other; those of different accounts don't wait.
func TestOverlappingRemovalsAreSerialized(t *testing.T) {
	h := newHarness(t, nil)
	first, err := h.w.BeginRemovalForTest(context.Background(), 1, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan func(), 1)
	go func() {
		end, err := h.w.BeginRemovalForTest(context.Background(), 1, false, cliworker.Claude, "")
		if err != nil {
			t.Error(err)
			end = func() {}
		}
		second <- end
	}()
	select {
	case <-second:
		t.Fatal("a removal of an account began while another removal of it was running")
	case <-time.After(300 * time.Millisecond):
	}
	// Another user's removal doesn't wait.
	other, err := h.w.BeginRemovalForTest(context.Background(), 2, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	other()
	first()
	select {
	case end := <-second:
		end()
	case <-time.After(10 * time.Second):
		t.Fatal("the second removal didn't begin after the first ended")
	}
}

// A removal that waits for another gives up with its caller.
func TestQueuedRemovalGivesUpWithItsCaller(t *testing.T) {
	h := newHarness(t, nil)
	first, err := h.w.BeginRemovalForTest(context.Background(), 1, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := h.w.BeginRemovalForTest(ctx, 1, false, cliworker.Claude, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a queued removal = %v, want its caller's deadline", err)
	}
}

// startGroup starts a process that sleeps in a group of its own, as a CLI's leftovers do.
func startGroup(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// A process group the account started that still has a live process after the wait keeps the
// files and is a retryable error, until the process is gone: also one of a turn that is over,
// and again on the retry.
func TestRemovalKeepsFilesWhileAGroupHasALiveProcess(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(300 * time.Millisecond)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	live := startGroup(t)
	h.w.TrackGroupForTest(live.Process.Pid, 1, cliworker.Claude, "")

	for i := 0; i < 2; i++ {
		if err := h.w.Forget(context.Background(), 1); !errors.Is(err, cliworker.ErrBusy) {
			t.Fatalf("try %d: Forget = %v, want ErrBusy", i, err)
		}
		if _, err := os.Stat(dirs.Root); err != nil {
			t.Fatalf("try %d: the files went with a live process in the group: %v", i, err)
		}
	}
	// The account's turns can start again, as after any failed removal.
	if _, err := h.w.StartLogin(1, cliworker.Claude, ""); errors.Is(err, cliworker.ErrRemoving) {
		t.Error("the account stayed closed after a removal that failed")
	}
	h.w.CancelLogin(1, cliworker.Claude, "")

	_ = live.Process.Kill()
	_ = live.Wait()
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory remains: %v", err)
	}
}

// A group with only zombies (killed, not yet reaped) holds nothing: the removal doesn't wait
// for the time limit because signal 0 still finds them. Another user's group isn't waited for.
func TestRemovalDoesNotWaitForZombies(t *testing.T) {
	h := newHarness(t, nil)
	h.w.SetGroupWaitForTest(20 * time.Second)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	zombie := startGroup(t)
	_ = zombie.Process.Kill() // not waited for: a zombie until the test ends
	eventually(t, "the process wasn't killed", func() bool { return !alive(zombie.Process.Pid) })
	h.w.TrackGroupForTest(zombie.Process.Pid, 1, cliworker.Claude, "")
	others := startGroup(t)
	h.w.TrackGroupForTest(others.Process.Pid, 2, cliworker.Claude, "")

	start := time.Now()
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Forget took %v: it waited for a zombie", took)
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory remains: %v", err)
	}
}

// A sign-in that was cancelled and replaced by a new one is still a process until it exits,
// and a removal waits for it, not only for the new one.
func TestRemovalWaitsForACancelledSignInThatWasReplaced(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { c.StopGrace = time.Second })
	// Its process group is not what is waited for here: that is a wait of its own.
	h.w.SetGroupWaitForTest(50 * time.Millisecond)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{IgnoreTerm: true}, false)
	old, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	h.w.CancelLogin(1, cliworker.Claude, "")
	if !old.Info().State.Final() {
		t.Fatal("the cancelled sign-in isn't over")
	}
	// The new sign-in ends at once when it is stopped.
	clitest.WriteScenario(t, dirs.Config(cliworker.Claude), clitest.Scenario{})
	replacement, err := h.w.StartLogin(1, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old {
		t.Fatal("the cancelled sign-in wasn't replaced")
	}
	if err := h.w.Forget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.Done():
	default:
		t.Error("Forget returned while the replaced sign-in's process was still running")
	}
	if _, err := os.Stat(dirs.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directory remains: %v", err)
	}
}

// A caller that has already given up starts nothing: not the helper that deletes either.
func TestRemovalWithACancelledContextStartsNoHelper(t *testing.T) {
	h := newHarness(t, nil)
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := h.w.HelperLaunchesForTest()
	if err := h.w.Forget(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("Forget = %v, want the cancellation", err)
	}
	if err := h.w.Logout(ctx, 1, cliworker.Claude, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Logout = %v, want the cancellation", err)
	}
	if n := h.w.HelperLaunchesForTest(); n != before {
		t.Errorf("%d helpers started for a cancelled request", n-before)
	}
	if _, err := os.Stat(dirs.Root); err != nil {
		t.Errorf("the files went: %v", err)
	}
	// Nor does one that is cancelled after the wait.
	end, err := h.w.BeginRemovalForTest(context.Background(), 1, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	end()
}
