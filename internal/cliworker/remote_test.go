//go:build unix

package cliworker_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// serve serves h's worker on a Unix socket and returns a client of it.
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

// The server's client of a worker in its own process does what the worker does in-process.
func TestRemoteWorker(t *testing.T) {
	h := newHarness(t, func(c *cliworker.Config) { delete(c.Bins, cliworker.Grok) })
	r := serve(t, h)
	ctx := context.Background()

	if !r.Available(cliworker.Claude) || r.Available(cliworker.Grok) {
		t.Fatal("available CLIs")
	}
	// A login, through its code.
	info, err := r.StartLogin(1, cliworker.Claude, "")
	if err != nil || info.State != cliworker.LoginWaiting || !info.NeedsCode {
		t.Fatalf("login = %+v %v", info, err)
	}
	if err := r.SubmitLoginCode(1, cliworker.Claude, "", "has space"); !errors.Is(err, cliworker.ErrBadCode) {
		t.Fatalf("bad code: %v", err)
	}
	if err := r.SubmitLoginCode(1, cliworker.Codex, "", "x"); !errors.Is(err, cliworker.ErrNoLogin) {
		t.Fatalf("no login: %v", err)
	}
	if err := r.SubmitLoginCode(1, cliworker.Claude, "", clitest.GoodCode); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the login didn't connect", func() bool {
		info, ok := r.LoginInfo(1, cliworker.Claude, "")
		return ok && info.State == cliworker.LoginConnected
	})
	if st, err := r.Status(ctx, 1, cliworker.Claude, ""); err != nil || !st.LoggedIn {
		t.Fatalf("status = %+v %v", st, err)
	}
	if _, err := r.StartLogin(1, cliworker.Grok, ""); !errors.Is(err, cliworker.ErrUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
	// Another account of the same CLI is a login of its own.
	if st, err := r.Status(ctx, 1, cliworker.Claude, "2"); err != nil || st.LoggedIn {
		t.Fatalf("second account's status = %+v %v", st, err)
	}
	if _, ok := r.LoginInfo(1, cliworker.Claude, "2"); ok {
		t.Fatal("the second account has the first's login")
	}
	if info, err := r.StartLogin(1, cliworker.Claude, "2"); err != nil || info.Slot != "2" || info.State != cliworker.LoginWaiting {
		t.Fatalf("second account's login = %+v %v", info, err)
	}
	r.CancelLogin(1, cliworker.Claude, "2")
	if info, ok := r.LoginInfo(1, cliworker.Claude, ""); !ok || info.State != cliworker.LoginConnected {
		t.Fatalf("cancelling the second account's login touched the first's: %+v", info)
	}
	if _, err := r.Status(ctx, 1, cliworker.Claude, "../x"); err == nil {
		t.Fatal("a slot that isn't one was accepted")
	}

	// A turn, its events, re-attaching, and another user's clash.
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
	if found, ok := r.FindTurn("remote"); !ok || found.Spec().User != 1 {
		t.Fatal("find")
	}
	if _, ok := r.FindTurn("nope"); ok {
		t.Fatal("found a turn that doesn't exist")
	}
	other := s
	other.User = 2
	if _, _, err := r.StartTurn(other); !errors.Is(err, cliworker.ErrTurnClash) {
		t.Fatalf("clash: %v", err)
	}
	bad := s
	bad.ID = "../x"
	if _, _, err := r.StartTurn(bad); err == nil {
		t.Fatal("a bad spec was accepted")
	}

	// Stop, over the socket.
	clitest.WriteScenario(t, h.layout.User(1).Config(cliworker.Claude), clitest.Scenario{SleepMS: 30_000})
	long, _, err := r.StartTurn(spec("long", 1, cliworker.Claude, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := long.Wait(ctx, 0); err != nil {
		t.Fatal(err)
	}
	r.StopTurn("long")
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

	if err := r.Logout(ctx, 1, cliworker.Claude, ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.Status(ctx, 1, cliworker.Claude, ""); st.LoggedIn {
		t.Fatal("still signed in")
	}
}
