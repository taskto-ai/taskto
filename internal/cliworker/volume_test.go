//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// reopen closes h's worker and starts another on the same volume.
func (h *harness) reopen(t *testing.T) {
	t.Helper()
	h.w.Close()
	w, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	h.w = w
}

// A home number is never given twice, whatever happened to the counter: one lost, rolled back or
// damaged is moved past every home the volume has, by record, by directory (a record that was
// lost) and in the trash.
func TestALostOrRolledBackCounterGivesNoHomeAgain(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"lost": os.Remove,
		"rolled back": func(p string) error {
			return os.WriteFile(p, []byte(strconv.Itoa(cliworker.FirstHomeForTest)+"\n"), 0o600)
		},
		"damaged": func(p string) error { return os.WriteFile(p, []byte("x"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.introduce(t, 1, 2)
			taken := map[int64]bool{h.w.HomeForTest(acct(1)): true, h.w.HomeForTest(acct(2)): true}
			// A home whose record went, its directory still there.
			orphan := h.w.HomeForTest(acct(3))
			if _, err := h.layout.Ensure(orphan); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(h.w.AccountFileForTest(cliworker.IdentityForTest(3))); err != nil {
				t.Fatal(err)
			}
			taken[orphan] = true
			h.w.Close()
			if err := damage(h.w.CounterFileForTest()); err != nil {
				t.Fatal(err)
			}
			h.reopen(t)
			for user := int64(4); user < 7; user++ {
				if home := h.w.HomeForTest(acct(user)); taken[home] {
					t.Fatalf("account %d was given home %d, which another has", user, home)
				} else {
					taken[home] = true
				}
			}
		})
	}
}

// Two records with the same home are a damaged volume: the worker doesn't start on it.
func TestTwoAccountsWithOneHomeStopTheWorker(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1)
	h.w.Close()
	b, err := os.ReadFile(h.w.AccountFileForTest(cliworker.IdentityForTest(1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.w.AccountFileForTest(cliworker.IdentityForTest(2)), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if w, err := cliworker.New(h.cfg); err == nil {
		w.Close()
		t.Fatal("a worker started on a volume where two accounts have one home")
	}
}

// One worker per volume: a second one doesn't start while the first runs, so the two never take
// the same home number, and it leaves the first's sockets alone. Once the first stops, it starts.
func TestASecondWorkerOnTheVolumeDoesNotStart(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1)
	if w, err := cliworker.New(h.cfg); err == nil {
		w.Close()
		t.Fatal("a second worker started on a volume the first uses")
	} else if !strings.Contains(err.Error(), "another CLI worker") {
		t.Fatalf("the second worker: %v", err)
	}
	if st, err := h.w.Status(context.Background(), acct(1), cliworker.Claude, ""); err != nil || st.LoggedIn {
		t.Errorf("the first worker after the second's attempt: %+v %v", st, err)
	}
	h.reopen(t)
}

// Every home number up to the last is given, and then none: new accounts are refused, the
// accounts there are go on.
func TestTheLastHomeNumberIsTheLast(t *testing.T) {
	h := newHarness(t, nil)
	h.introduce(t, 1)
	h.w.Close()
	if err := os.WriteFile(h.w.CounterFileForTest(), []byte(strconv.FormatInt(cliworker.MaxHomeForTest-1, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.reopen(t)
	ctx := context.Background()
	if _, err := h.w.Status(ctx, acct(2), cliworker.Claude, ""); err != nil {
		t.Fatalf("the last home: %v", err)
	}
	if got := h.w.HomeForTest(acct(2)); got != cliworker.MaxHomeForTest-1 {
		t.Errorf("the last home = %d, want %d", got, cliworker.MaxHomeForTest-1)
	}
	if _, err := h.w.Status(ctx, acct(3), cliworker.Claude, ""); !errors.Is(err, cliworker.ErrFull) {
		t.Errorf("an account after the last home: %v, want ErrFull", err)
	}
	if _, err := h.w.Status(ctx, acct(1), cliworker.Claude, ""); err != nil {
		t.Errorf("an account there already: %v", err)
	}
}

// A census or a deletion from a server boot older than one the worker has heard from since (a
// server a restore replaced, still running or delayed) retires nothing: the accounts the later
// server made keep their homes.
func TestAnEarlierBootsCensusOrDeletionRetiresNothing(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	old := acct(1) // the tests' boot: the earlier server's
	if _, err := h.w.Status(ctx, old, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	later := strings.Repeat("c", 32)
	made := cliowner.Account{User: 2, Identity: strings.Repeat("d", 32), Boot: later}
	dirs, err := h.w.AccountDirs(made, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	// The earlier server's census, which doesn't know the later one's account.
	if err := h.w.Reconcile(ctx, census(nil, 1)); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier boot's census: %v, want ErrStaleBoot", err)
	}
	// And its deletion of the account, delayed past the restore that brought the account back.
	earlier := made
	earlier.Boot = cliworker.TestBoot
	if err := h.w.Forget(ctx, earlier); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier boot's deletion: %v, want ErrStaleBoot", err)
	}
	if st, err := h.w.Status(ctx, made, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the later server's account: %+v %v", st, err)
	}
	// The later server's own census is applied.
	now := census(map[int64]string{2: made.Identity})
	now.Boot = later
	if err := h.w.Reconcile(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.w.Status(ctx, old, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the account the later census doesn't name: %v, want ErrForgotten", err)
	}
	if _, err := os.Stat(filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")); err != nil {
		t.Errorf("the later server's account's login: %v", err)
	}
}

// A retired account reaches nothing of what the worker still holds of it (a removal that failed
// before it dropped it): not its turn's state or events, not its sign-in, not its permission
// questions.
func TestARetiredAccountReachesNothingItHad(t *testing.T) {
	h := newHarness(t, nil)
	h.user(t, 1, cliworker.Claude, clitest.Scenario{Tool: &clitest.ToolUse{Name: "Bash", Input: map[string]any{"command": "touch x"}}}, true)
	s := spec("held", 1, cliworker.Claude, "go")
	turn, _, err := h.w.Start(s)
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, h.w, 1)
	if _, err := h.w.StartLogin(acct(1), cliworker.Codex, ""); err != nil {
		t.Fatal(err)
	}
	r := serve(t, h)
	if err := h.w.RetireOnlyForTest(cliworker.IdentityForTest(1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.w.TurnOf(acct(1), s.ID); ok {
		t.Error("the retired account found its turn")
	}
	if _, err := r.FindTurn(acct(1), s.ID); err == nil {
		t.Error("the retired account found its turn over the worker's API")
	}
	if events, _, err := r.TurnForTest(s).Wait(context.Background(), 0); err == nil || len(events) > 0 {
		t.Errorf("the retired account read its turn's events: %d %v", len(events), err)
	}
	if _, ok := h.w.Login(acct(1), cliworker.Codex, ""); ok {
		t.Error("the retired account found its sign-in")
	}
	if qs := h.w.Permissions(acct(1), s.Conversation); len(qs) != 0 {
		t.Errorf("the retired account read its permission questions: %+v", qs)
	}
	if err := h.w.Answer(acct(1), s.Conversation, q.ID, true); err == nil {
		t.Error("the retired account answered a permission question")
	}
	h.w.Stop(s.ID) // the removal's own way, by the object: it still reaches the turn
	waitDone(t, turn, 20*time.Second)
}
