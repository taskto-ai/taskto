//go:build unix

package agentrt

// Supervisor tests against a fake sidecar: this test binary, re-executed with
// TASKTO_FAKE_SIDECAR set. The fake records what happens to it in fake.log next to its
// database, and holds fake.lock while it runs, so two at once would be caught.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if modes := os.Getenv("TASKTO_FAKE_SIDECAR"); modes != "" {
		os.Exit(fakeSidecar(modes))
	}
	if dir := os.Getenv("TASKTO_FAKE_SERVER"); dir != "" {
		os.Exit(fakeServer(dir))
	}
	os.Exit(m.Run())
}

// fakeServer stands in for the server: it supervises a serving fake sidecar with its
// database in dir, prints the sidecar's PID, and waits to be killed.
func fakeServer(dir string) int {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	s, err := NewSupervisor(SupervisorConfig{
		Node:      exe,
		Entry:     "fake-entry.js",
		SocketDir: filepath.Join(dir, "run"),
		DB:        filepath.Join(dir, "agent.db"),
		Env:       []string{"TASKTO_FAKE_SIDECAR=serve", "TASKTO_FAKE_SERVER="},
	})
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Start(ctx); err != nil {
		panic(err)
	}
	fmt.Println(s.PID())
	select {}
}

// fakeSidecar runs one fake sidecar. modes is a comma-separated list: the nth start runs
// the nth mode, and the last one repeats.
//
//	serve     answer /healthz; exit 0 on SIGTERM
//	crash     answer /healthz, then exit 1 after 100 ms
//	linger    answer /healthz, close the socket after 100 ms, exit 1 300 ms later
//	mute      never listen; on SIGTERM take 300 ms to exit
//	stubborn  answer /healthz; ignore SIGTERM
//	probe     like the real sidecar: start up for 300 ms (Node loading), then exit 1 if
//	          something already answers on the socket, otherwise serve
//	callback  serve, after recording a call to the tools socket named in ToolsEnv
func fakeSidecar(modes string) int {
	socket, db := os.Getenv("TASKTO_AGENT_SOCKET"), os.Getenv("TASKTO_AGENT_DB")
	dir := filepath.Dir(db)
	record := func(format string, args ...any) {
		f, err := os.OpenFile(filepath.Join(dir, "fake.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G703: the test's own temp dir
		if err != nil {
			panic(err)
		}
		defer f.Close()
		fmt.Fprintf(f, "%d %d %s\n", os.Getpid(), time.Now().UnixNano(), fmt.Sprintf(format, args...))
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)

	lock, err := os.OpenFile(filepath.Join(dir, "fake.lock"), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G703: the test's own temp dir
	if err != nil {
		panic(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		record("overlap")
		return 3
	}
	starts := len(fakeEvents(dir, "start")) // this one not counted yet
	list := strings.Split(modes, ",")
	mode := list[min(starts, len(list)-1)]
	record("start %s", mode)
	record("secret %q", os.Getenv("TASKTO_TEST_DROPPED_SECRET"))
	fmt.Fprintf(os.Stderr, "fake sidecar up: %s\n", mode)
	if os.Getenv(ExitOnEOFEnv) == "1" {
		// Like the real sidecar: the end of stdin means the supervisor's process is gone.
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			record("eof")
			os.Exit(0)
		}()
	}

	if mode == "mute" {
		<-terms
		record("term")
		time.Sleep(300 * time.Millisecond)
		return 0
	}
	if mode == "probe" {
		// The window in which a supervisor's health checks could reach a stray sidecar.
		time.Sleep(300 * time.Millisecond)
		if c, err := net.DialTimeout("unix", socket, 200*time.Millisecond); err == nil { //nolint:gosec // G704: the test's own temp socket
			_ = c.Close()
			record("probe found another runtime")
			return 1
		}
	}
	_ = os.Remove(socket) //nolint:gosec // G703: the test's own temp dir
	ln, err := net.Listen("unix", socket)
	if err != nil {
		record("listen failed: %v", err)
		return 1
	}
	go func() {
		srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"status":"ok","pid":%d}`, os.Getpid())
		})}
		_ = srv.Serve(ln)
	}()
	switch mode {
	case "callback":
		record("tools %s", callTools(os.Getenv(ToolsEnv)))
	case "crash":
		time.Sleep(100 * time.Millisecond)
		record("exit")
		return 1
	case "linger":
		time.Sleep(100 * time.Millisecond)
		ln.Close()
		time.Sleep(300 * time.Millisecond)
		record("exit")
		return 1
	case "stubborn":
		for range terms {
			record("term")
		}
	}
	<-terms
	record("term")
	return 0
}

// callTools makes one request to the tools socket at path and returns what it got.
func callTools(path string) string {
	if path == "" {
		return "no socket"
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, "unix", path)
	}}}
	res, err := c.Get("http://tools/ping") //nolint:noctx // a test's one-off call
	if err != nil {
		return "error " + err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return fmt.Sprintf("%d %s %s", res.StatusCode, path, body)
}

type fakeEvent struct {
	pid  int
	at   time.Time
	what string
}

// fakeEvents returns the fake's records whose text starts with prefix ("" for all).
func fakeEvents(dir, prefix string) []fakeEvent {
	f, err := os.Open(filepath.Join(dir, "fake.log")) //nolint:gosec // G703: the test's own temp dir
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []fakeEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), " ", 3)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], prefix) {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		nanos, _ := strconv.ParseInt(fields[1], 10, 64)
		out = append(out, fakeEvent{pid: pid, at: time.Unix(0, nanos), what: fields[2]})
	}
	return out
}

type fakeEnv struct {
	dir string // holds the database, fake.log and fake.lock
	cfg SupervisorConfig
	log *syncBuffer
}

// newFake returns a config that runs the fake sidecar with the given modes.
func newFake(t *testing.T, modes string) *fakeEnv {
	t.Helper()
	// A short directory: Unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "taskto-sup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	return &fakeEnv{dir: dir, log: log, cfg: SupervisorConfig{
		Node:        exe,
		Entry:       "fake-entry.js",
		SocketDir:   filepath.Join(dir, "run"),
		DB:          filepath.Join(dir, "agent.db"),
		Env:         []string{"TASKTO_FAKE_SIDECAR=" + modes},
		Logger:      slog.New(slog.NewTextHandler(log, nil)),
		MinBackoff:  100 * time.Millisecond,
		MaxBackoff:  time.Second,
		StopTimeout: 2 * time.Second,
	}}
}

// start starts a supervisor with the fake's config and stops it when the test ends.
func (f *fakeEnv) start(t *testing.T) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Stop(context.Background())
		f.checkNoOverlap(t)
		if t.Failed() {
			t.Logf("supervisor log:\n%s", f.log.String())
		}
	})
	if _, err := s.Start(ctxFor(t)); err != nil {
		t.Fatalf("start: %v", err)
	}
	return s
}

func (f *fakeEnv) checkNoOverlap(t *testing.T) {
	t.Helper()
	for _, ev := range fakeEvents(f.dir, "overlap") {
		t.Errorf("sidecar %d started while another was running", ev.pid)
	}
}

// waitFor polls cond until it holds or 10 s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processGone reports whether pid has exited. A zombie has: an orphan whose server was
// killed waits for PID 1 to reap it, which some containers' PID 1 never does.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) // Linux only; elsewhere the reaper is reliable
	if err != nil {
		return false
	}
	// "pid (comm) state ...": comm may hold spaces and parentheses, so the state follows the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSupervisorStartsAndStops(t *testing.T) {
	f := newFake(t, "serve")
	s := f.start(t)

	if err := s.Client().Healthy(ctxFor(t)); err != nil {
		t.Fatalf("healthy after Start: %v", err)
	}
	info, err := os.Stat(f.cfg.SocketDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket directory mode = %o, want 700", perm)
	}
	pid := s.PID()
	if pid == 0 {
		t.Fatal("no PID while running")
	}
	// The sidecar's output goes to the logger.
	waitFor(t, "the sidecar's output in the log", func() bool {
		return strings.Contains(f.log.String(), "fake sidecar up: serve")
	})

	if err := s.Stop(ctxFor(t)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := fakeEvents(f.dir, "term"); len(got) != 1 {
		t.Fatalf("SIGTERMs recorded = %v, want one", got)
	}
	if !processGone(pid) {
		t.Fatal("the sidecar still runs after Stop")
	}
	if err := s.Stop(ctxFor(t)); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if _, err := s.Ready(ctxFor(t)); !errors.Is(err, ErrStopped) {
		t.Fatalf("Ready after Stop = %v, want ErrStopped", err)
	}
	if got := fakeEvents(f.dir, "start"); len(got) != 1 {
		t.Fatalf("starts = %d, want 1", len(got))
	}
}

// Launch returns without waiting for the sidecar; State follows it from starting to ready
// to stopped.
func TestSupervisorLaunchAndState(t *testing.T) {
	f := newFake(t, "probe") // takes 300 ms to answer
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()); f.checkNoOverlap(t) })
	if got := s.State(); got != StateStarting {
		t.Fatalf("state before Launch = %q, want %q", got, StateStarting)
	}
	if err := s.Launch(); err != nil {
		t.Fatal(err)
	}
	if got := s.State(); got != StateStarting {
		t.Fatalf("state right after Launch = %q, want %q", got, StateStarting)
	}
	if err := s.Launch(); err == nil {
		t.Fatal("a second Launch succeeded")
	}
	waitFor(t, "ready", func() bool { return s.State() == StateReady })
	if err := s.Client().Healthy(ctxFor(t)); err != nil {
		t.Fatalf("healthy while ready: %v", err)
	}
	if err := s.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	if got := s.State(); got != StateStopped {
		t.Fatalf("state after Stop = %q, want %q", got, StateStopped)
	}
}

func TestSupervisorLaunchFailureIsStopped(t *testing.T) {
	f := newFake(t, "serve")
	f.start(t)
	s, err := NewSupervisor(f.cfg) // the same database
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Launch(); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("launch = %v, want ErrDatabaseInUse", err)
	}
	if got := s.State(); got != StateStopped {
		t.Fatalf("state after a failed Launch = %q, want %q", got, StateStopped)
	}
}

func TestSupervisorSocketPathTooLong(t *testing.T) {
	_, err := NewSupervisor(SupervisorConfig{
		SocketDir: "/" + strings.Repeat("d", maxSocketPath),
		DB:        "/tmp/agent.db",
	})
	if !errors.Is(err, ErrSocketPathTooLong) {
		t.Fatalf("err = %v, want ErrSocketPathTooLong", err)
	}
}

// The sidecar's stdin is a pipe only the supervising process holds: when that process is
// killed with SIGKILL, so nothing of it runs, the sidecar still sees the pipe close and exits.
func TestSidecarExitsWhenTheServerIsKilled(t *testing.T) {
	dir, err := os.MkdirTemp("", "taskto-sup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(exe)
	server.Env = append(os.Environ(), "TASKTO_FAKE_SERVER="+dir)
	server.Stderr = testWriter{t}
	out, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the fake server printed no sidecar PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid == 0 {
		t.Fatalf("sidecar PID = %q", line)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if err := server.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = server.Wait()
	waitFor(t, "the sidecar to exit", func() bool { return processGone(pid) })
	if got := fakeEvents(dir, "eof"); len(got) != 1 || got[0].pid != pid {
		t.Fatalf("eof records = %v, want one from the sidecar %d", got, pid)
	}
}

func TestSupervisorRestartsWithBackoff(t *testing.T) {
	f := newFake(t, "crash,crash,crash,serve")
	s := f.start(t)

	waitFor(t, "the fourth start", func() bool { return len(fakeEvents(f.dir, "start")) == 4 })
	if _, err := s.Ready(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	starts, exits := fakeEvents(f.dir, "start"), fakeEvents(f.dir, "exit")
	if len(exits) != 3 {
		t.Fatalf("exits = %v, want 3", exits)
	}
	// Each restart waits at least the backoff, which doubles: 100, 200, 400 ms.
	backoff := f.cfg.MinBackoff
	for i, exit := range exits {
		if gap := starts[i+1].at.Sub(exit.at); gap < backoff {
			t.Errorf("restart %d came %s after the exit, want at least %s", i+1, gap, backoff)
		}
		backoff *= 2
	}
	if !strings.Contains(f.log.String(), "agent sidecar exited; restarting") {
		t.Errorf("restarts not logged:\n%s", f.log.String())
	}
}

// A sidecar that never gets healthy must not reset the backoff, however long each failed
// attempt takes: only time spent ready counts toward StableAfter.
func TestSupervisorBackoffGrowsWhenNeverReady(t *testing.T) {
	f := newFake(t, "mute")
	f.cfg.ReadyTimeout = 300 * time.Millisecond
	f.cfg.StableAfter = 200 * time.Millisecond // shorter than one failed attempt (~600 ms)
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Start waits for a ready sidecar, which never comes here: run it until the test ends.
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		defer close(started)
		_, _ = s.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-started
		_ = s.Stop(context.Background())
		f.checkNoOverlap(t)
	})

	waitFor(t, "the fourth start", func() bool { return len(fakeEvents(f.dir, "start")) == 4 })
	starts, terms := fakeEvents(f.dir, "start"), fakeEvents(f.dir, "term")
	// After each SIGTERM the mute fake takes 300 ms to exit, then the backoff runs: 100, 200,
	// 400 ms. A backoff reset each time would leave every gap near 400 ms.
	backoff := f.cfg.MinBackoff
	for i := range 3 {
		want := 300*time.Millisecond + backoff
		if gap := starts[i+1].at.Sub(terms[i].at); gap < want {
			t.Errorf("restart %d came %s after SIGTERM, want at least %s", i+1, gap, want)
		}
		backoff *= 2
	}
}

// A sidecar left behind by a crashed server still answers on the socket. The supervisor
// must not take its answer for its own sidecar's: it waits, and becomes ready only once the
// stray one is gone and its own sidecar answers.
func TestSupervisorIgnoresAStraySidecarOnTheSocket(t *testing.T) {
	f := newFake(t, "probe")
	if err := os.MkdirAll(f.cfg.SocketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stray, err := net.Listen("unix", filepath.Join(f.cfg.SocketDir, socketName))
	if err != nil {
		t.Fatal(err)
	}
	straySrv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","pid":1}`))
	})}
	go func() { _ = straySrv.Serve(stray) }()

	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Start waits until ready, which can't happen while the stray answers: run it in the
	// background and look at readiness with short waits.
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		defer close(started)
		_, _ = s.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-started
		_ = s.Stop(context.Background())
		f.checkNoOverlap(t)
		if t.Failed() {
			t.Logf("supervisor log:\n%s", f.log.String())
		}
	})
	waitFor(t, "the fake to find the stray sidecar", func() bool {
		return len(fakeEvents(f.dir, "probe")) >= 2 // it found it, exited, and was restarted
	})
	short, cancelShort := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelShort()
	if _, err := s.Ready(short); err == nil {
		t.Fatal("the supervisor was ready while only the stray sidecar answered")
	}

	_ = straySrv.Close()
	client, err := s.Ready(ctxFor(t))
	if err != nil {
		t.Fatalf("after the stray left: %v", err)
	}
	h, err := client.Health(ctxFor(t))
	if err != nil || h.PID == 1 {
		t.Fatalf("health after the stray left = %+v, %v; want the supervised sidecar", h, err)
	}
}

func TestSupervisorNeverRunsTwoAtOnce(t *testing.T) {
	// mute: never ready, so the supervisor stops it after ReadyTimeout, and it takes 300 ms
	// to exit. linger: closes its socket 300 ms before it exits. Either way a supervisor that
	// started the next sidecar before the last one exited would make the fake record an overlap.
	f := newFake(t, "mute,linger,mute,linger,serve")
	f.cfg.ReadyTimeout = 200 * time.Millisecond
	f.cfg.MinBackoff = 10 * time.Millisecond
	f.cfg.MaxBackoff = 10 * time.Millisecond
	s := f.start(t) // ready at the first linger

	waitFor(t, "the fifth start", func() bool { return len(fakeEvents(f.dir, "start")) == 5 })
	if _, err := s.Ready(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	// f.start's cleanup fails the test if any of them found the fake's lock taken.
	// And by the records: each sidecar starts after everything the previous one recorded.
	var order []int
	first, last := map[int]time.Time{}, map[int]time.Time{}
	for _, ev := range fakeEvents(f.dir, "") {
		if _, ok := first[ev.pid]; !ok {
			first[ev.pid] = ev.at
			order = append(order, ev.pid)
		}
		last[ev.pid] = ev.at
	}
	for i := 1; i < len(order); i++ {
		if prev, next := order[i-1], order[i]; first[next].Before(last[prev]) {
			t.Errorf("pid %d started before pid %d finished", next, prev)
		}
	}
}

func TestSupervisorOneSupervisorPerDatabase(t *testing.T) {
	f := newFake(t, "serve")
	first := f.start(t)

	second := f.cfg
	second.SocketDir = filepath.Join(f.dir, "run2") // a different socket, the same database
	s, err := NewSupervisor(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctxFor(t)); !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("second supervisor: err = %v, want ErrDatabaseInUse", err)
	}

	// Once the first has stopped, the database is free again.
	if err := first.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	f.cfg.SocketDir = second.SocketDir
	f.start(t)
}

func TestSupervisorStopKillsAStubbornSidecar(t *testing.T) {
	f := newFake(t, "stubborn")
	f.cfg.StopTimeout = 300 * time.Millisecond
	s := f.start(t)
	pid := s.PID()

	began := time.Now()
	if err := s.Stop(ctxFor(t)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took < f.cfg.StopTimeout {
		t.Fatalf("Stop returned after %s, before the %s SIGTERM grace", took, f.cfg.StopTimeout)
	}
	if got := fakeEvents(f.dir, "term"); len(got) == 0 {
		t.Fatal("the sidecar never got SIGTERM")
	}
	if !processGone(pid) {
		t.Fatal("the sidecar survived Stop")
	}
}

func TestSupervisorStopRespectsContext(t *testing.T) {
	f := newFake(t, "stubborn")
	f.cfg.StopTimeout = time.Minute
	s := f.start(t)
	pid := s.PID()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop = %v, want DeadlineExceeded", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Stop took %s with a 200 ms context", took)
	}
	if !processGone(pid) {
		t.Fatal("the sidecar survived Stop")
	}
}

func TestSupervisorStartRespectsContext(t *testing.T) {
	f := newFake(t, "mute")
	f.cfg.ReadyTimeout = time.Minute
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := s.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start = %v, want DeadlineExceeded", err)
	}
	// A failed Start stops what it started.
	starts := fakeEvents(f.dir, "start")
	if len(starts) != 1 || !processGone(starts[0].pid) {
		t.Fatalf("after a failed Start: starts %v, want one, gone", starts)
	}
	if _, err := s.Ready(ctxFor(t)); !errors.Is(err, ErrStopped) {
		t.Fatalf("Ready after a failed Start = %v, want ErrStopped", err)
	}
}

func TestSupervisorRefusesALooseSocketDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o710, 0o701} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFake(t, "serve")
			if err := os.Mkdir(f.cfg.SocketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.cfg.SocketDir, mode); err != nil {
				t.Fatal(err)
			}
			s, err := NewSupervisor(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Start(ctxFor(t)); err == nil || !strings.Contains(err.Error(), "want 700") {
				t.Fatalf("start = %v, want a refusal of the directory", err)
			}
			if got := fakeEvents(f.dir, "start"); len(got) != 0 {
				t.Fatalf("a sidecar was started: %v", got)
			}
		})
	}
}

func TestSupervisorRefusesASymlinkedSocketDirectory(t *testing.T) {
	f := newFake(t, "serve")
	target := filepath.Join(f.dir, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.cfg.SocketDir); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctxFor(t)); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("start = %v, want a refusal of the symlink", err)
	}
}

func TestLogWriterSplitsLines(t *testing.T) {
	var buf syncBuffer
	w := &logWriter{log: slog.New(slog.NewTextHandler(&buf, nil))}
	_, _ = w.Write([]byte("one\ntw"))
	_, _ = w.Write([]byte("o\r\nthree"))
	w.flush()
	got := buf.String()
	for _, want := range []string{"line=one", "line=two", "line=three"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("log has %d records, want 3:\n%s", n, got)
	}
}

func TestLockFileSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "agent.db")
	if err := os.Symlink(target, db+".lock"); err != nil {
		t.Fatal(err)
	}
	if f, err := lockDB(db); err == nil {
		_ = f.Close()
		t.Fatal("lockDB followed a symlinked lock file")
	}
}

// A line longer than the cap is split even when it arrives whole, newline included.
func TestLogWriterSplitsALongCompleteLine(t *testing.T) {
	var out syncBuffer
	w := &logWriter{log: slog.New(slog.NewTextHandler(&out, nil))}
	line := strings.Repeat("x", 2*maxLogLine+100) + "\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	w.flush()
	records := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3 pieces", len(records))
	}
	for i, r := range records {
		if n := strings.Count(r, "x"); n > maxLogLine {
			t.Errorf("record %d holds %d bytes of the line, want at most %d", i, n, maxLogLine)
		}
	}
}

func TestToolsSocket(t *testing.T) {
	f := newFake(t, "callback")
	f.cfg.Tools = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			_, _ = io.WriteString(w, "pong")
		}
	})
	tools := filepath.Join(f.cfg.SocketDir, toolsSocketName)
	// A stale socket a crashed server left behind is replaced.
	if err := os.MkdirAll(f.cfg.SocketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", tools)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	s := f.start(t)
	waitFor(t, "the sidecar's call to the tools socket", func() bool { return len(fakeEvents(f.dir, "tools")) > 0 })
	if got, want := fakeEvents(f.dir, "tools")[0].what, "tools 200 "+tools+" pong"; got != want {
		t.Fatalf("the sidecar's call got %q, want %q", got, want)
	}
	info, err := os.Lstat(tools)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tools socket mode = %o, want 600", perm)
	}
	if err := s.Stop(ctxFor(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tools); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tools socket outlived the supervisor: %v", err)
	}
}

func TestNoToolsSocketWithoutTools(t *testing.T) {
	f := newFake(t, "callback")
	f.cfg.Env = append(f.cfg.Env, ToolsEnv+"=/somewhere/else.sock") // as if the server's environment had one
	f.start(t)
	waitFor(t, "the sidecar's record", func() bool { return len(fakeEvents(f.dir, "tools")) > 0 })
	if got := fakeEvents(f.dir, "tools")[0].what; got != "tools no socket" {
		t.Fatalf("without Tools the sidecar got %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.cfg.SocketDir, toolsSocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a tools socket exists without Tools: %v", err)
	}
}

func TestToolsSocketRefusesAFileInItsPlace(t *testing.T) {
	f := newFake(t, "serve")
	f.cfg.Tools = http.NotFoundHandler()
	if err := os.MkdirAll(f.cfg.SocketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.SocketDir, toolsSocketName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Launch(); err == nil || !strings.Contains(err.Error(), "isn't a socket") {
		t.Fatalf("Launch = %v, want a refusal", err)
	}
	if s.State() != StateStopped {
		t.Fatalf("state = %s after a failed Launch", s.State())
	}
	// The lock was released: another supervisor can start.
	f.cfg.Tools = nil
	f.start(t)
}

func TestDropEnvKeepsServerSecretsFromTheSidecar(t *testing.T) {
	t.Setenv("TASKTO_TEST_DROPPED_SECRET", "hunter2")
	for name, drop := range map[string][]string{"kept": nil, "dropped": {"TASKTO_TEST_DROPPED_SECRET"}} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, "serve")
			f.cfg.DropEnv = drop
			f.start(t)
			waitFor(t, "the sidecar's report of its environment", func() bool { return len(fakeEvents(f.dir, "secret")) > 0 })
			want := `secret "hunter2"`
			if drop != nil {
				want = `secret ""`
			}
			if got := fakeEvents(f.dir, "secret")[0].what; got != want {
				t.Fatalf("the sidecar saw %q, want %q", got, want)
			}
		})
	}
}
