//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func TestAccountSlots(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1, 2)
	dirs, err := h.layout.Ensure(h.w.HomeForTest(acct(1)))
	if err != nil {
		t.Fatal(err)
	}
	first, second := dirs.Config(cliworker.Claude), dirs.ConfigOf(cliworker.Claude, "2")
	if second != filepath.Join(dirs.Root, "config", "claude-2") {
		t.Fatalf("second account's directory = %s", second)
	}

	l, err := h.w.StartLogin(acct(1), cliworker.Claude, "2")
	if err != nil {
		t.Fatal(err)
	}
	if l.Info().Slot != "2" {
		t.Errorf("login info = %+v", l.Info())
	}
	if _, ok := h.w.Login(acct(1), cliworker.Claude, ""); ok {
		t.Error("the first account has the second's login")
	}
	if err := h.w.SubmitLoginCode(acct(1), cliworker.Claude, "2", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	if info := waitLogin(t, l); info.State != cliworker.LoginConnected {
		t.Fatalf("login = %+v", info)
	}
	ctx := context.Background()
	if st, _ := h.w.Status(ctx, acct(1), cliworker.Claude, "2"); !st.LoggedIn {
		t.Error("the second account isn't signed in")
	}
	if st, _ := h.w.Status(ctx, acct(1), cliworker.Claude, ""); st.LoggedIn {
		t.Error("signing in the second account signed in the first")
	}

	s := spec("slot-2", 1, cliworker.Claude, "hello")
	s.Slot = "2"
	turn, _, err := h.w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	if done := last(waitDone(t, turn, 20*time.Second)); done.Status != cliworker.StatusOK {
		t.Fatalf("turn: %+v", done)
	}
	calls := clitest.Calls(t, second)
	if len(calls) == 0 || !slices.Contains(calls[len(calls)-1].Env, "CLAUDE_CONFIG_DIR="+second) {
		t.Fatalf("the turn didn't run in the second account's directory: %+v", calls)
	}
	for _, c := range clitest.Calls(t, first) {
		if slices.Contains(c.Args, "-p") {
			t.Errorf("a turn ran in the first account's directory: %+v", c)
		}
	}

	s.Slot = ""
	if _, _, err := h.w.Start(s); err == nil || !strings.Contains(err.Error(), "different turn") {
		t.Errorf("same ID, other account: %v", err)
	}

	clitest.SignIn(t, "claude", first)
	if err := h.w.Logout(ctx, acct(1), cliworker.Claude, "2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.w.Status(ctx, acct(1), cliworker.Claude, "2"); st.LoggedIn {
		t.Error("still signed in after the logout")
	}
	if st, _ := h.w.Status(ctx, acct(1), cliworker.Claude, ""); !st.LoggedIn {
		t.Error("signing out the second account signed out the first")
	}

	for _, bad := range []cliworker.Slot{"1", "10", "../x", "s0", "s", "2/..", "a"} {
		if _, err := h.w.StartLogin(acct(1), cliworker.Claude, bad); err == nil {
			t.Errorf("slot %q accepted for a login", bad)
		}
		s.ID, s.Slot = "bad-slot", bad
		if _, _, err := h.w.Start(s); err == nil {
			t.Errorf("slot %q accepted for a turn", bad)
		}
	}
	if _, err := h.w.StartLogin(acct(1), cliworker.Claude, "s3"); err == nil {
		t.Error("a login started in a shared account's directory")
	}
}

func TestForgetRemovesEverythingKeptForAUser(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1, 2)
	ctx := context.Background()
	mine, err := h.layout.EnsureSlot(h.w.HomeForTest(acct(1)), cliworker.Claude, "2")
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.layout.Ensure(h.w.HomeForTest(acct(2)))
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", mine.Config(cliworker.Claude))
	clitest.SignIn(t, "claude", mine.ConfigOf(cliworker.Claude, "2"))
	clitest.SignIn(t, "claude", other.Config(cliworker.Claude))
	if st, _ := h.w.Status(ctx, acct(1), cliworker.Claude, "2"); !st.LoggedIn {
		t.Fatal("fixture: the second account isn't signed in")
	}

	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine.Root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the user's directory remains: %v", err)
	}
	if st, _ := h.w.Status(ctx, acct(2), cliworker.Claude, ""); !st.LoggedIn {
		t.Error("forgetting one user signed another out")
	}
	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Errorf("forgetting again: %v", err)
	}
	if err := h.w.Forget(ctx, acct(0)); err == nil {
		t.Error("user 0 was accepted")
	}
}
