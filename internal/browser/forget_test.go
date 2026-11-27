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

// profileHost is a fakeHost whose users have a browser directory with a profile on disk, made
// when their browser starts, as the real hosts do. It records every process it started, and
// what Purge saw.
type profileHost struct {
	fakeHost
	root string

	mu    sync.Mutex
	procs map[int64][]*exec.Cmd
	// browsers is the Chromium processes alone: the fake display's programs may end on their own.
	browsers map[int64][]*exec.Cmd
	// aliveAtPurge is the processes of the user still running when Purge was called.
	aliveAtPurge map[int64]int
	// failPurge makes the next Purge calls fail, one each.
	failPurge int
	// inPurge, if set, is told when Purge is called, which then waits for release.
	inPurge chan int64
	release chan struct{}
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

// alive counts user's processes that still run. A process the Manager has waited for is gone from
// the table, so signal 0 finds no such process.
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

// browsersAlive counts user's Chromium processes that still run.
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

func (h *profileHost) exists(user int64) bool {
	_, err := os.Stat(h.profile(user))
	return err == nil
}

func (h *profileHost) alivePurge(user int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.aliveAtPurge[user]
}

// echoView is the display's side of a live view: it reads what the viewer sends until the view
// is closed.
func echoView(c io.ReadWriteCloser) {
	defer c.Close()
	_, _ = io.Copy(io.Discard, c)
}

// waitEnded reads from c until it ends, which it must do soon (a banner may come first).
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

// serveRemote serves m's API on a Unix socket, as the CLI worker does, and returns the client.
func serveRemote(t *testing.T, m *Manager) *Remote {
	t.Helper()
	// A short directory: a Unix socket path is limited to about 104 bytes on macOS, which
	// t.TempDir() with a long test name can exceed.
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

// A deleted account's browsers are stopped, with their live views, and waited for before the
// profile is removed; another user's browsers, live views and files are not touched.
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
	// The live view was closed: its stream ends.
	waitEnded(t, gone)

	// The other user's browsers, view and files are as they were.
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

// While an account's browsers are being forgotten, and after, no browser of theirs starts,
// whatever the kind or the way in; others' do.
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
	// Forget has stopped the browsers and is removing the files.
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

	// Another user's start is not held up by it (their own Purge is never called here).
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
	// Ids are never given again: the refusal outlasts the call, and nothing of theirs is made.
	if err := m.Start(ctx, acct(1), Personal); !errors.Is(err, ErrAccountClosing) {
		t.Errorf("a start after the purge: %v", err)
	}
	if h.exists(1) {
		t.Error("a refused start made the profile again")
	}
}

// A browser still starting when its account is forgotten is waited for and stopped, so no
// process is left to write under the profile that is removed.
func TestForgetStopsABrowserStillStarting(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	started := make(chan struct{})
	go func() { _ = m.Start(ctx, acct(1), Personal); close(started) }()
	// Forget as soon as the start has begun: either it is found starting or already running.
	for h.alive(1) == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	<-started // started or refused: either way no process is left
	if n := h.alivePurge(1); n != 0 {
		t.Errorf("%d processes ran when the files were removed", n)
	}
	if n := h.alive(1); n != 0 {
		t.Errorf("%d processes still run", n)
	}
}

// A purge that couldn't remove the files says so, and is tried again to the end; the browsers
// stay off meanwhile.
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
	// Nothing to stop and nothing to remove is done, not an error.
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatalf("a third try: %v", err)
	}
	if err := m.Forget(ctx, acct(0)); err == nil {
		t.Error("an invalid user")
	}
}

// Forget gives up when its context ends before the browsers are gone, and says so: the files
// are not removed under a process that still runs.
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
	// And a later try, with time, finishes.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if err := m.Forget(ctx2, acct(1)); err != nil {
		t.Fatal(err)
	}
	if h.alivePurge(1) != 0 || h.exists(1) {
		t.Errorf("alive at purge %d, profile there %v", h.alivePurge(1), h.exists(1))
	}
}

// The worker's API: the forget route answers for the user in its path alone.
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

// ErrAccountClosing crosses the worker's socket as itself.
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

// A browser still stopping when a newer one gave up waiting for it (its start timed out) is still
// waited for by Forget: the newer one, which never ran, doesn't take its place.
func TestForgetWaitsForABrowserThatAStartGaveUpOn(t *testing.T) {
	h := newProfileHost(t)
	m := NewManager(Config{Host: h, StartTimeout: 100 * time.Millisecond})
	t.Cleanup(m.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := m.Start(ctx, acct(1), Testing); err != nil {
		t.Fatal(err)
	}
	// The fake takes 700 ms to exit after it is told to close.
	stopped := make(chan struct{})
	go func() { _ = m.Stop(ctx, acct(1), Testing); close(stopped) }()
	for {
		st, _ := m.Status(ctx, acct(1))
		if !st[Testing].Running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The start waits 100 ms for the old browser, then gives up.
	if err := m.Start(ctx, acct(1), Testing); err == nil {
		t.Fatal("a start while the old browser was still stopping succeeded")
	}
	if h.alive(1) == 0 {
		t.Skip("the old browser was already gone: nothing to wait for")
	}
	if err := m.Forget(ctx, acct(1)); err != nil {
		t.Fatal(err)
	}
	if n := h.alivePurge(1); n != 0 {
		t.Errorf("the files were removed with %d processes still running", n)
	}
	<-stopped
}

// A deleted account's Forget runs under the CLI worker's fence for deletions: one it refuses (an
// earlier server's purge) stops no browser and removes no file.
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
