//go:build unix

package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// The worker's API and the server's client of it, over a Unix socket as in container mode.
func TestRemote(t *testing.T) {
	vnc := func(c io.ReadWriteCloser) {
		defer c.Close()
		_, _ = io.WriteString(c, "RFB 003.008\n")
		_, _ = io.Copy(c, c) // echo what the viewer sends
	}
	m := NewManager(Config{Host: &fakeHost{log: filepath.Join(t.TempDir(), "log"), vnc: vnc}})
	t.Cleanup(m.Close)
	sock := filepath.Join(t.TempDir(), "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewHandler(m), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := NewRemote(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if av := c.Availability(ctx); !av[Personal] || !av[Testing] {
		t.Fatalf("availability = %v", av)
	}
	// Refusals come back as the Manager's errors.
	if _, err := c.OpenLive(ctx, acct(1)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("a live view of no browser: %v", err)
	}
	var bad *BadAction
	if _, err := c.Do(ctx, acct(1), Action{Op: OpOpen, Browser: Personal, URL: "file:///etc/passwd"}); !errors.As(err, &bad) {
		t.Fatalf("a file URL: %v", err)
	}
	if err := c.Start(ctx, acct(1), "other"); !errors.As(err, &bad) {
		t.Fatalf("an unknown browser: %v", err)
	}

	if err := c.Start(ctx, acct(1), Personal); err != nil {
		t.Fatal(err)
	}
	// An upgrade must say so in Connection too.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://cli-worker/browser/users/1/live", nil)
	req.Header.Set("Upgrade", upgradeProtocol)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an upgrade without Connection: %d", res.StatusCode)
	}
	if !headerHasToken(http.Header{"Connection": {"keep-alive, Upgrade"}}, "Connection", "upgrade") ||
		headerHasToken(http.Header{"Connection": {"keep-alive"}}, "Connection", "upgrade") {
		t.Error("headerHasToken")
	}
	st, err := c.Status(ctx, acct(1))
	if err != nil || !st[Personal].Running || st[Testing].Running || st[Personal].StopsAt == nil {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if other, _ := c.Status(ctx, acct(2)); other[Personal].Running {
		t.Fatal("user 2 sees user 1's browser running")
	}

	// The live view is an upgrade that then carries the stream both ways.
	live, err := c.OpenLive(ctx, acct(1))
	if err != nil {
		t.Fatal(err)
	}
	banner := make([]byte, 12)
	if _, err := io.ReadFull(live, banner); err != nil || string(banner) != "RFB 003.008\n" {
		t.Fatalf("banner %q, %v", banner, err)
	}
	if _, err := io.WriteString(live, "ping"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(live, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo %q, %v", echo, err)
	}
	if st, _ := c.Status(ctx, acct(1)); !st[Personal].Live || st[Personal].StopsAt != nil {
		t.Fatalf("with a live view: %+v", st[Personal])
	}
	live.Close()
	deadline := time.Now().Add(5 * time.Second)
	for st, _ := c.Status(ctx, acct(1)); st[Personal].Live; st, _ = c.Status(ctx, acct(1)) {
		if time.Now().After(deadline) {
			t.Fatal("the closed live view still counts")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// At most MaxLive views of one browser at once; closing one makes room.
	var views []io.ReadWriteCloser
	for range MaxLive {
		v, err := c.OpenLive(ctx, acct(1))
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, v)
	}
	if _, err := c.OpenLive(ctx, acct(1)); !errors.Is(err, ErrTooManyLive) {
		t.Fatalf("one view too many: %v", err)
	}
	views[0].Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		v, err := c.OpenLive(ctx, acct(1))
		if err == nil {
			views[0] = v
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after closing one: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, v := range views {
		v.Close()
	}
	// User 2 has no browser to watch.
	if _, err := c.OpenLive(ctx, acct(2)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("user 2's live view: %v", err)
	}

	if err := c.Stop(ctx, acct(1), Personal); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx, acct(1)); st[Personal].Running {
		t.Fatal("still running after stop")
	}
}

// Every sentinel has its place in the order refuse tries them in.
func TestRemoteErrorOrder(t *testing.T) {
	if len(remoteErrorOrder) != len(remoteErrors) {
		t.Fatalf("order %v, errors %d", remoteErrorOrder, len(remoteErrors))
	}
	for _, name := range remoteErrorOrder {
		if remoteErrors[name] == nil {
			t.Errorf("%s isn't an error", name)
		}
	}
}

// The CLI worker's refusals of a purge (it lost its history of server boots, it waits for the
// server's first census) reach the server as themselves over the worker's socket: the server says
// so to its operators, or keeps the purge's step and tries again.
func TestAPurgeRefusedForLostAuthoritySaysSoOverTheSocket(t *testing.T) {
	for _, refusal := range []error{cliowner.ErrAuthorityLost, cliowner.ErrCensusNeeded} {
		t.Run(refusal.Error(), func(t *testing.T) { purgeRefusedOverTheSocket(t, refusal) })
	}
}

func purgeRefusedOverTheSocket(t *testing.T, refusal error) {
	m := NewManager(Config{Host: &fakeHost{log: filepath.Join(t.TempDir(), "log")}, Fence: func(context.Context, cliowner.Account, func() error) error {
		return refusal
	}})
	t.Cleanup(m.Close)
	dir, err := os.MkdirTemp("/tmp", "br")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewHandler(m), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if err := NewRemote(sock).Forget(context.Background(), acct(1)); !errors.Is(err, refusal) {
		t.Errorf("the purge over the socket: %v, want %v", err, refusal)
	}
}
