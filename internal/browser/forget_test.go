//go:build unix

package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

type profileHost struct {
	fakeHost
	root string

	mu           sync.Mutex
	procs        map[int64][]*exec.Cmd
	browsers     map[int64][]*exec.Cmd
	aliveAtPurge map[int64]int
	failPurge    int
	inPurge      chan int64
	release      chan struct{}
	inClean      chan struct{}
	releaseClean chan struct{}
	purges       int
}

func newProfileHost(t *testing.T) *profileHost {
	t.Helper()
	return &profileHost{
		fakeHost:     fakeHost{log: filepath.Join(t.TempDir(), "log")},
		root:         t.TempDir(),
		procs:        map[int64][]*exec.Cmd{},
		browsers:     map[int64][]*exec.Cmd{},
		aliveAtPurge: map[int64]int{},
	}
}

func (h *profileHost) dir(user int64) string {
	return filepath.Join(h.root, "u"+strconv.FormatInt(user, 10), "browser")
}

func (h *profileHost) profile(user int64) string { return filepath.Join(h.dir(user), "profile") }

func (h *profileHost) Start(a cliowner.Account, spec ProcSpec, extra []*os.File) (*exec.Cmd, error) {
	user := a.User
	if err := os.MkdirAll(h.profile(user), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(h.profile(user), "Cookies"), []byte("signed in"), 0o600); err != nil {
		return nil, err
	}
	cmd, err := h.fakeHost.Start(a, spec, extra)
	if err == nil {
		h.mu.Lock()
		h.procs[user] = append(h.procs[user], cmd)
		if spec.Program == Chromium {
			h.browsers[user] = append(h.browsers[user], cmd)
		}
		h.mu.Unlock()
	}
	return cmd, err
}

func (h *profileHost) alive(user int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.procs[user] {
		if c.Process != nil && syscall.Kill(c.Process.Pid, 0) == nil {
			n++
		}
	}
	return n
}

func (h *profileHost) browsersAlive(user int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.browsers[user] {
		if syscall.Kill(c.Process.Pid, 0) == nil {
			n++
		}
	}
	return n
}

func (h *profileHost) Purge(a cliowner.Account) error {
	user := a.User
	h.mu.Lock()
	h.aliveAtPurge[user] = 0
	for _, c := range h.procs[user] {
		if syscall.Kill(c.Process.Pid, 0) == nil {
			h.aliveAtPurge[user]++
		}
	}
	h.purges++
	fail := h.failPurge > 0
	if fail {
		h.failPurge--
	}
	h.mu.Unlock()
	if h.inPurge != nil {
		h.inPurge <- user
		<-h.release
	}
	if fail {
		return errors.New("a file is busy")
	}
	return os.RemoveAll(h.dir(user))
}

func (h *profileHost) Clean(cliowner.Account) error {
	h.mu.Lock()
	in := h.inClean
	h.inClean = nil
	h.mu.Unlock()
	if in != nil {
		close(in)
		<-h.releaseClean
	}
	return nil
}

func (h *profileHost) purged() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.purges
}

func (h *profileHost) exists(user int64) bool {
	_, err := os.Stat(h.profile(user))
	return err == nil
}

func (h *profileHost) alivePurge(user int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aliveAtPurge[user]
}

func echoView(c io.ReadWriteCloser) {
	defer c.Close()
	_, _ = io.Copy(io.Discard, c)
}

func waitEnded(t *testing.T, c io.Reader) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, c)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the deleted account's live view is still open")
	}
}

func serveRemote(t *testing.T, m *Manager) *Remote {
	t.Helper()
	dir, err := os.MkdirTemp("", "w")
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
	return NewRemote(sock)
}

func startAll(t *testing.T, m *Manager, user int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, kind := range []Kind{Personal, Testing} {
		if err := m.Start(ctx, acct(user), kind); err != nil {
			t.Fatalf("starting user %d's %s browser: %v", user, kind, err)
		}
	}
}

func TestForgetStopsTheBrowsersAndRemovesTheProfile(t *testing.T) {
	h := newProfileHost(t)
	h.vnc = echoView
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	startAll(t, m, 2)
	gone, err := m.OpenLive(ctx, acct(1))
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.OpenLive(ctx, acct(2))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if h.alive(1) == 0 || !h.exists(1) {
		t.Fatalf("before: %d processes, profile %v", h.alive(1), h.exists(1))
	}
	if n := h.browsersAlive(2); n != 2 {
		t.Fatalf("user 2 runs %d browsers before", n)
	}

	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}

	if n := h.alivePurge(1); n != 0 {
		t.Errorf("the profile was removed with %d of the user's processes still running", n)
	}
	if n := h.alive(1); n != 0 {
		t.Errorf("%d of the user's processes still run", n)
	}
	if h.exists(1) {
		t.Error("the profile is still there")
	}
	st, _ := m.Status(ctx, acct(1))
	if st[Personal].Running || st[Testing].Running {
		t.Errorf("status after: %+v", st)
	}
	waitEnded(t, gone)

	st2, _ := m.Status(ctx, acct(2))
	if !st2[Personal].Running || !st2[Testing].Running || !st2[Personal].Live {
		t.Errorf("user 2's status: %+v", st2)
	}
	if n := h.browsersAlive(2); n != 2 {
		t.Errorf("user 2 runs %d browsers after, had 2", n)
	}
	if !h.exists(2) {
		t.Error("user 2's profile was removed")
	}
	if _, err := other.Write([]byte("still here")); err != nil {
		t.Errorf("user 2's live view: %v", err)
	}
}

func TestForgetRefusesStartsWhileItRuns(t *testing.T) {
	h := newProfileHost(t)
	h.vnc = echoView
	h.inPurge = make(chan int64)
	h.release = make(chan struct{})
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	errc := make(chan error, 1)
	go func() { errc <- m.Forget(ctx, acct(1)) }()
	select {
	case u := <-h.inPurge:
		if u != 1 {
			t.Fatalf("purging user %d", u)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Forget never reached the files")
	}

	for _, kind := range []Kind{Personal, Testing} {
		if err := m.Start(ctx, acct(1), kind); !errors.Is(err, ErrAccountClosing) {
			t.Errorf("starting the %s browser during the purge: %v", kind, err)
		}
		if _, err := m.Do(ctx, acct(1), Action{Op: OpOpen, Browser: kind, URL: "https://example.com/"}); !errors.Is(err, ErrAccountClosing) {
			t.Errorf("opening a page in the %s browser during the purge: %v", kind, err)
		}
	}
	if _, err := m.OpenLive(ctx, acct(1)); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("a live view during the purge: %v", err)
	}
	if h.alive(1) != 0 {
		t.Errorf("%d processes of the user run during the purge", h.alive(1))
	}

	if err := m.Start(ctx, acct(2), Testing); err != nil {
		t.Errorf("another user's start during the purge: %v", err)
	}

	close(h.release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if h.exists(1) {
		t.Error("the profile is still there")
	}
	if err := m.Start(ctx, acct(1), Personal); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("a start after the purge: %v", err)
	}
	if h.exists(1) {
		t.Error("a refused start made the profile again")
	}
}

func TestForgetStopsABrowserStillStarting(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	started := make(chan struct{})
	go func() { _ = m.Start(ctx, acct(1), Personal); close(started) }()
	for h.alive(1) == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	<-started
	if n := h.alivePurge(1); n != 0 {
		t.Errorf("%d processes ran when the files were removed", n)
	}
	if n := h.alive(1); n != 0 {
		t.Errorf("%d processes still run", n)
	}
}

func TestForgetIsRetryable(t *testing.T) {
	h := newProfileHost(t)
	h.failPurge = 1
	m := NewManager(Config{Host: h})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	if err := m.Forget(ctx, acct(1)); err == nil {
		t.Fatal("a failed removal reported as done")
	}
	if !h.exists(1) {
		t.Fatal("the files went although the removal failed")
	}
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatalf("the second try: %v", err)
	}
	if h.exists(1) {
		t.Error("the profile is still there")
	}
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatalf("a third try: %v", err)
	}
	if err := m.Forget(ctx, acct(0)); err == nil {
		t.Error("an invalid user")
	}
}

func TestForgetWaitsForTheProcesses(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h, StopGrace: 10 * time.Second})
	t.Cleanup(m.Close)
	startAll(t, m, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.Forget(ctx, acct(1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("with a context that has ended: %v", err)
	}
	if !h.exists(1) {
		t.Fatal("the files were removed before the processes were gone")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if err := m.Forget(ctx2, acct(1)); err != nil {
		t.Fatal(err)
	}
	if h.alivePurge(1) != 0 || h.exists(1) {
		t.Errorf("alive at purge %d, profile there %v", h.alivePurge(1), h.exists(1))
	}
}

func TestRemoteForget(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h, Max: 8})
	t.Cleanup(m.Close)
	c := serveRemote(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	startAll(t, m, 1)
	startAll(t, m, 2)
	if err := c.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	if h.alive(1) != 0 || h.exists(1) {
		t.Errorf("user 1: %d processes, profile there %v", h.alive(1), h.exists(1))
	}
	if st, _ := c.Status(ctx, acct(2)); !st[Personal].Running || !h.exists(2) {
		t.Errorf("user 2 was touched: %+v", st)
	}
	if err := c.Forget(ctx, acct(0)); err == nil {
		t.Error("an invalid user")
	}
}

func TestRemoteAccountClosing(t *testing.T) {
	h := newProfileHost(t)
	h.inPurge = make(chan int64)
	h.release = make(chan struct{})
	m := NewManager(Config{Host: h})
	t.Cleanup(m.Close)
	c := serveRemote(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- c.Forget(ctx, acct(1)) }()
	<-h.inPurge
	if err := c.Start(ctx, acct(1), Testing); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("a start during the purge: %v", err)
	}
	close(h.release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, acct(1), Testing); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("a start after it: %v", err)
	}
}

func TestForgetWaitsForABrowserThatAStartGaveUpOn(t *testing.T) {
	h := newProfileHost(t)
	inClean := make(chan struct{})
	h.inClean = inClean
	h.releaseClean = make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(h.releaseClean)
		}
	}
	m := NewManager(Config{Host: h, StartTimeout: 3 * time.Second})
	t.Cleanup(m.Close)
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := m.Start(ctx, acct(1), Testing); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { _ = m.Stop(ctx, acct(1), Testing); close(stopped) }()
	select {
	case <-inClean:
	case <-ctx.Done():
		t.Fatal("the old browser's stop didn't reach its last step")
	}
	if err := m.Start(ctx, acct(1), Testing); err == nil {
		t.Fatal("a start while the old browser was still stopping succeeded")
	}
	forgot := make(chan error, 1)
	go func() { forgot <- m.Forget(ctx, acct(1)) }()
	select {
	case err := <-forgot:
		t.Fatalf("Forget returned (%v) while the old browser was still stopping", err)
	case <-time.After(200 * time.Millisecond):
	}
	if n := h.purged(); n != 0 {
		t.Fatalf("the files were removed while the old browser was still stopping (%d purges)", n)
	}
	release()
	if err := <-forgot; err != nil {
		t.Fatal(err)
	}
	if n := h.alivePurge(1); n != 0 || h.purged() != 1 {
		t.Errorf("the files were removed with %d processes still running (%d purges)", n, h.purged())
	}
	<-stopped
}

func TestForgetRunsUnderTheFence(t *testing.T) {
	h := newProfileHost(t)
	refuse := errors.New("an earlier server's purge")
	var fenced []string
	m := NewManager(Config{Host: h, Max: 8, Fence: func(_ context.Context, a cliowner.Account, do func() error) error {
		fenced = append(fenced, a.Identity)
		if a.User == 1 {
			return refuse
		}
		return do()
	}})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	startAll(t, m, 1)
	if err := m.Forget(ctx, acct(1)); !errors.Is(err, refuse) {
		t.Fatalf("a refused Forget: %v", err)
	}
	if h.browsersAlive(1) == 0 || !h.exists(1) {
		t.Errorf("a refused Forget stopped the browsers (%d left) or removed the profile (%v)", h.browsersAlive(1), h.exists(1))
	}
	if err := m.Forget(ctx, acct(2)); err != nil {
		t.Fatal(err)
	}
	if len(fenced) != 2 {
		t.Errorf("the fence saw %v, want both deletions", fenced)
	}
}
