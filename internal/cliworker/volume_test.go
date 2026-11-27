//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

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

func TestALostOrRolledBackCounterGivesNoHomeAgain(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"lost": os.Remove,
		"rolled back": func(p string) error {
			return os.WriteFile(p, []byte(strconv.Itoa(cliworker.FirstHomeForTest)+"\n"), 0o600)
		},
		"damaged": func(p string) error { return os.WriteFile(p, []byte("x"), 0o600) },
		"past the last number": func(p string) error {
			return os.WriteFile(p, []byte(strconv.FormatInt(cliworker.MaxHomeForTest+5, 10)+"\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.introduce(t, 1, 2)
			taken := map[int64]bool{h.w.HomeForTest(acct(1)): true, h.w.HomeForTest(acct(2)): true}
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

func TestACounterLostWhileTheWorkerRunsGivesNoHomeAgain(t *testing.T) {
	for name, damage := range map[string]func(path string) error{
		"lost": os.Remove,
		"rolled back": func(p string) error {
			return os.WriteFile(p, []byte(strconv.Itoa(cliworker.FirstHomeForTest)+"\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			ctx := context.Background()
			h.introduce(t, 1)
			taken := map[int64]bool{h.w.HomeForTest(acct(1)): true}
			h.user(t, 2, cliworker.Claude, clitest.Scenario{}, true)
			taken[h.w.HomeForTest(acct(2))] = true
			if err := h.w.Forget(ctx, acct(2)); err != nil {
				t.Fatal(err)
			}
			taken[h.w.HomeForTest(acct(3))] = true
			if err := damage(h.w.CounterFileForTest()); err != nil {
				t.Fatal(err)
			}
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
	if err := os.Chmod(h.layout.Root, 0o711); err != nil {
		t.Fatal(err)
	}
	if w, err := cliworker.New(h.cfg); err == nil {
		w.Close()
	}
	if fi, err := os.Stat(h.layout.Root); err != nil || fi.Mode().Perm() != 0o711 {
		t.Errorf("the second worker changed the root's mode: %v %v", fi.Mode().Perm(), err)
	}
	h.reopen(t)
}

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

func TestAnEarlierBootsCensusOrDeletionRetiresNothing(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	old := acct(1)
	if _, err := h.w.Status(ctx, old, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, start, map[int64]string{1: old.Identity})); err != nil {
		t.Fatal(err)
	}
	later := strings.Repeat("c", 32)
	made := cliowner.Account{User: 2, Identity: strings.Repeat("d", 32), Boot: later, Installation: cliworker.TestInstallation}
	dirs, err := h.w.AccountDirs(made, cliworker.Claude, "")
	if err != nil {
		t.Fatal(err)
	}
	clitest.SignIn(t, "claude", dirs.Config(cliworker.Claude))
	login := filepath.Join(dirs.Config(cliworker.Claude), ".credentials.json")
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, start, map[int64]string{1: old.Identity})); err != nil {
		t.Fatal(err)
	}
	if st, err := h.w.Status(ctx, made, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Fatalf("the later server's account after the earlier census: %+v %v", st, err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(later, time.Now(), map[int64]string{2: made.Identity})); err != nil {
		t.Fatal(err)
	}
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, time.Now().Add(time.Hour), nil)); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier boot's census: %v, want ErrStaleBoot", err)
	}
	earlier := made
	earlier.Boot = cliworker.TestBoot
	if err := h.w.Forget(ctx, earlier); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier boot's deletion: %v, want ErrStaleBoot", err)
	}
	if err := h.w.FenceDeletion(ctx, earlier, func() error { return errors.New("ran") }); !errors.Is(err, cliworker.ErrStaleBoot) {
		t.Errorf("the earlier boot's browser purge: %v, want ErrStaleBoot", err)
	}
	if st, err := h.w.Status(ctx, made, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Errorf("the later server's account: %+v %v", st, err)
	}
	if _, err := os.Stat(login); err != nil {
		t.Errorf("the later server's account's login: %v", err)
	}
	if _, err := h.w.Status(ctx, old, cliworker.Claude, ""); !errors.Is(err, cliworker.ErrForgotten) {
		t.Errorf("the account the later census doesn't name: %v, want ErrForgotten", err)
	}
}

func TestAnEarlierCensusRacingALaterServersAccountsRetiresNone(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	if err := h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, start, nil)); err != nil {
		t.Fatal(err)
	}
	later := strings.Repeat("c", 32)
	accounts := make([]cliowner.Account, 20)
	for i := range accounts {
		accounts[i] = cliowner.Account{User: int64(100 + i), Identity: fmt.Sprintf("%030x%02x", 7, i), Boot: later, Installation: cliworker.TestInstallation}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 20 {
			_ = h.w.Reconcile(ctx, bootCensus(cliworker.TestBoot, start, nil))
		}
	}()
	go func() {
		defer wg.Done()
		for _, a := range accounts {
			if _, err := h.w.Status(ctx, a, cliworker.Claude, ""); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	for _, a := range accounts {
		if _, err := h.w.Status(ctx, a, cliworker.Claude, ""); err != nil {
			t.Errorf("the later server's account %d after the earlier censuses: %v", a.User, err)
		}
	}
}

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
	h.w.Stop(s.ID)
	waitDone(t, turn, 20*time.Second)
}

func TestAPurgeStoppedAfterTheMoveIsFinishedBeforeTheNextOneSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	dirs := h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	private := filepath.Join(dirs.Work, "private")
	if err := os.WriteFile(private, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.w.SetDetachHookForTest(func() error { return errors.New("the worker stopped here") })
	if err := h.w.Forget(ctx, acct(1)); err == nil || !strings.Contains(err.Error(), "the worker stopped here") {
		t.Fatalf("fixture: the purge didn't stop where the test stops it: %v", err)
	}
	h.w.SetDetachHookForTest(nil)
	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Fatalf("the purge's retry: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(h.layout.Root, ".detached-*"))
	kept, _ := filepath.Glob(filepath.Join(h.layout.Root, ".worker", "trash", "*"))
	if len(left)+len(kept) > 0 {
		t.Errorf("the retry reported the purge done with the account's files still in %v %v", left, kept)
	}
}

func TestAPurgeRetryFindsWhatItLeftInARootWithGlobCharacters(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) {
		c.Layout.Root = c.Layout.Root + "[worker]"
	})
	ctx := context.Background()
	h.user(t, 1, cliworker.Claude, clitest.Scenario{}, true)
	h.w.SetDetachHookForTest(func() error { return errors.New("the worker stopped here") })
	if err := h.w.Forget(ctx, acct(1)); err == nil || !strings.Contains(err.Error(), "the worker stopped here") {
		t.Fatalf("fixture: the purge didn't stop where the test stops it: %v", err)
	}
	h.w.SetDetachHookForTest(nil)
	if err := h.w.Forget(ctx, acct(1)); err != nil {
		t.Fatalf("the purge's retry: %v", err)
	}
	for _, dir := range []string{h.layout.Root, filepath.Join(h.layout.Root, ".worker", "trash")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".detached-") || strings.HasPrefix(e.Name(), "h") {
				t.Errorf("the retry reported the purge done with %s still in %s", e.Name(), dir)
			}
		}
	}
}
