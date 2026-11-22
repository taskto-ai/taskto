//go:build unix

package cliworker_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// A user's second account for a CLI is a config directory of its own: its login, status,
// turns and sign-out never touch the first's.
func TestAccountSlots(t *testing.T) {
	h := newHarness(t, nil)
	dirs, err := h.layout.Ensure(1)
	if err != nil {
		t.Fatal(err)
	}
	first, second := dirs.Config(cliworker.Claude), dirs.ConfigOf(cliworker.Claude, "2")
	if second != filepath.Join(dirs.Root, "config", "claude-2") {
		t.Fatalf("second account's directory = %s", second)
	}

	// The second account signs in through its own login, in its own directory.
	l, err := h.w.StartLogin(1, cliworker.Claude, "2")
	if err != nil {
		t.Fatal(err)
	}
	if l.Info().Slot != "2" {
		t.Errorf("login info = %+v", l.Info())
	}
	if _, ok := h.w.Login(1, cliworker.Claude, ""); ok {
		t.Error("the first account has the second's login")
	}
	if err := h.w.SubmitLoginCode(1, cliworker.Claude, "2", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	if info := waitLogin(t, l); info.State != cliworker.LoginConnected {
		t.Fatalf("login = %+v", info)
	}
	ctx := context.Background()
	if st, _ := h.w.Status(ctx, 1, cliworker.Claude, "2"); !st.LoggedIn {
		t.Error("the second account isn't signed in")
	}
	if st, _ := h.w.Status(ctx, 1, cliworker.Claude, ""); st.LoggedIn {
		t.Error("signing in the second account signed in the first")
	}

	// A turn on the second account runs with its directory.
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

	// The same turn ID on another account is another turn.
	s.Slot = ""
	if _, _, err := h.w.Start(s); err == nil || !strings.Contains(err.Error(), "different turn") {
		t.Errorf("same ID, other account: %v", err)
	}

	// Signing the second account out leaves the first alone.
	clitest.SignIn(t, "claude", first)
	if err := h.w.Logout(ctx, 1, cliworker.Claude, "2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.w.Status(ctx, 1, cliworker.Claude, "2"); st.LoggedIn {
		t.Error("still signed in after the logout")
	}
	if st, _ := h.w.Status(ctx, 1, cliworker.Claude, ""); !st.LoggedIn {
		t.Error("signing out the second account signed out the first")
	}

	// A slot is a name the worker knows, never a path.
	for _, bad := range []cliworker.Slot{"1", "10", "../x", "s0", "s", "2/..", "a"} {
		if _, err := h.w.StartLogin(1, cliworker.Claude, bad); err == nil {
			t.Errorf("slot %q accepted for a login", bad)
		}
		s.ID, s.Slot = "bad-slot", bad
		if _, _, err := h.w.Start(s); err == nil {
			t.Errorf("slot %q accepted for a turn", bad)
		}
	}
	// A shared account's directory has no login of its own.
	if _, err := h.w.StartLogin(1, cliworker.Claude, "s3"); err == nil {
		t.Error("a login started in a shared account's directory")
	}
}
