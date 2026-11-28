//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

func serve(t *testing.T, h *harness) *cliworker.Remote {
	t.Helper()
	dir, err := os.MkdirTemp("", "cw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "w.sock")
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h.w.Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return cliworker.NewRemote(socket)
}

func TestRemoteWorker(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { delete(c.Bins, cliworker.Grok) })
	h.introduce(t, 1, 2)
	r := serve(t, h)
	ctx := context.Background()

	if !r.Available(cliworker.Claude) || r.Available(cliworker.Grok) {
		t.Fatal("available CLIs")
	}
	info, err := r.StartLogin(acct(1), cliworker.Claude, "")
	if err != nil || info.State != cliworker.LoginWaiting || !info.NeedsCode {
		t.Fatalf("login = %+v %v", info, err)
	}
	if err := r.SubmitLoginCode(acct(1), cliworker.Claude, "", "has space"); !errors.Is(err, cliworker.ErrBadCode) {
		t.Fatalf("bad code: %v", err)
	}
	if err := r.SubmitLoginCode(acct(1), cliworker.Codex, "", "x"); !errors.Is(err, cliworker.ErrNoLogin) {
		t.Fatalf("no login: %v", err)
	}
	if err := r.SubmitLoginCode(acct(1), cliworker.Claude, "", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the login didn't connect", func() bool {
		info, lerr := r.LoginInfo(acct(1), cliworker.Claude, "")
		return lerr == nil && info.State == cliworker.LoginConnected
	})
	if st, err := r.Status(ctx, acct(1), cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Fatalf("status = %+v %v", st, err)
	}
	if _, err := r.StartLogin(acct(1), cliworker.Grok, ""); !errors.Is(err, cliworker.ErrUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
	if st, err := r.Status(ctx, acct(1), cliworker.Claude, "2"); err != nil || st.LoggedIn {
		t.Fatalf("second account's status = %+v %v", st, err)
	}
	if _, lerr := r.LoginInfo(acct(1), cliworker.Claude, "2"); lerr == nil {
		t.Fatal("the second account has the first's login")
	}
	if info, err := r.StartLogin(acct(1), cliworker.Claude, "2"); err != nil || info.Slot != "2" || info.State != cliworker.LoginWaiting {
		t.Fatalf("second account's login = %+v %v", info, err)
	}
	_ = r.CancelLogin(acct(1), cliworker.Claude, "2")
	if info, lerr := r.LoginInfo(acct(1), cliworker.Claude, ""); lerr != nil || info.State != cliworker.LoginConnected {
		t.Fatalf("cancelling the second account's login touched the first's: %+v", info)
	}
	if _, err := r.Status(ctx, acct(1), cliworker.Claude, "../x"); err == nil {
		t.Fatal("a slot that isn't one was accepted")
	}

	s := spec("remote", 1, cliworker.Claude, "over the socket")
	turn, attached, err := r.StartTurn(s)
	if err != nil || attached {
		t.Fatalf("start = %v %v", attached, err)
	}
	var events []cliworker.Event
	for done := false; !done; {
		got, d, err := turn.Wait(ctx, len(events))
		if err != nil {
			t.Fatal(err)
		}
		events, done = append(events, got...), d
	}
	if d := last(events); d.Status != cliworker.StatusOK || d.Text != "echo: over the socket" {
		t.Fatalf("done = %+v", d)
	}
	if _, attached, err := r.StartTurn(s); err != nil || !attached {
		t.Fatalf("re-attach = %v %v", attached, err)
	}
	if found, err := r.FindTurn(acct(1), "remote"); err != nil || found.Spec().User != 1 {
		t.Fatal("find")
	}
	if _, err := r.FindTurn(acct(1), "nope"); err == nil {
		t.Fatal("found a turn that doesn't exist")
	}
	if _, err := r.FindTurn(acct(2), "remote"); err == nil {
		t.Fatal("another user found the turn")
	}
	if _, err := r.FindTurn(otherAccount(1), "remote"); err == nil {
		t.Fatal("another account of the same number found the turn")
	}
	for name, a := range map[string]cliowner.Account{"another number's": acct(2), "the number's other account's": otherAccount(1)} {
		theirs := s
		theirs.User, theirs.Identity = a.User, a.Identity
		if events, _, err := r.TurnForTest(theirs).Wait(ctx, 0); err == nil || len(events) > 0 {
			t.Errorf("%s handle read the turn's events: %d %v", name, len(events), err)
		}
	}
	other := s
	other.User, other.Identity = 2, cliworker.IdentityForTest(2)
	if _, _, err := r.StartTurn(other); !errors.Is(err, cliworker.ErrTurnClash) {
		t.Fatalf("clash: %v", err)
	}
	bad := s
	bad.ID = "../x"
	if _, _, err := r.StartTurn(bad); err == nil {
		t.Fatal("a bad spec was accepted")
	}

	clitest.WriteScenario(t, h.home(1).Config(cliworker.Claude), clitest.Scenario{SleepMS: 30_000})
	long, _, err := r.StartTurn(spec("long", 1, cliworker.Claude, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := long.Wait(ctx, 0); err != nil {
		t.Fatal(err)
	}
	_ = r.StopTurn(acct(2), "long")
	_ = r.StopTurn(otherAccount(1), "long")
	time.Sleep(100 * time.Millisecond)
	if found, err := r.FindTurn(acct(1), "long"); err != nil || found.Over() {
		t.Fatal("another user's stop stopped the turn")
	}
	_ = r.StopTurn(acct(1), "long")
	events = nil
	for done := false; !done; {
		got, d, err := long.Wait(ctx, len(events))
		if err != nil {
			t.Fatal(err)
		}
		events, done = append(events, got...), d
	}
	if d := last(events); d.Status != cliworker.StatusStopped {
		t.Fatalf("stopped = %+v", d)
	}

	if err := r.Logout(ctx, acct(1), cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Status(ctx, acct(1), cliworker.Claude, ""); st.LoggedIn {
		t.Fatal("still signed in")
	}

	if err := r.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.home(1).Root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the user's directory remains: %v", err)
	}
	if err := r.Forget(ctx, acct(0)); err == nil {
		t.Fatal("user 0 was accepted")
	}
}

func TestADeletionBeforeTheFirstCensusSaysSoOverTheSocket(t *testing.T) {
	h := newFreshHarness(t, nil)
	r := serve(t, h)
	if err := r.Forget(context.Background(), acct(1)); !errors.Is(err, cliworker.ErrCensusNeeded) {
		t.Errorf("a deletion before any census over the socket: %v, want ErrCensusNeeded", err)
	}
	a := acct(1)
	req := httptest.NewRequest(http.MethodPost, "/forget", strings.NewReader(`{"user":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Taskto-Identity", a.Identity)
	req.Header.Set("X-Taskto-Boot", a.Boot)
	req.Header.Set("X-Taskto-Installation", a.Installation)
	rec := httptest.NewRecorder()
	h.w.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"census_needed"`) {
		t.Errorf("a deletion before any census answers %d %s, want 409 census_needed", rec.Code, rec.Body.String())
	}
}
