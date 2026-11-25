//go:build unix

package agentrt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"taskto.ai/taskto/internal/drain"
	"taskto.ai/taskto/internal/lockfile"
)

// SupervisorConfig says how to run the sidecar. SocketDir and DB are required.
type SupervisorConfig struct {
	// Node is the Node.js executable, "node" by default (looked up in PATH).
	Node string
	// Entry is the sidecar's entry point, "agent/dist/index.js" by default.
	Entry string
	// SocketDir holds the sidecar's socket. It is created with mode 0700; an existing
	// directory must be owned by this user and closed to everyone else, or Start refuses it.
	SocketDir string
	// DB is the sidecar's database file. Pi Durable allows one process per database, so a
	// lock file next to it (DB + ".lock") is held for as long as the supervisor or any
	// sidecar it started is alive.
	DB string
	// Env is added to this process's environment for the sidecar.
	Env []string
	// DropEnv names variables of this process's environment the sidecar must not inherit, such
	// as a secret the server read from one. They are removed before Env is added.
	DropEnv []string
	// Logger receives the supervisor's events and the sidecar's output. Default: slog.Default().
	Logger *slog.Logger
	// Tools, if set, is served to the sidecar on a second owner-only socket in SocketDir
	// (tools.sock), whose path it gets in ToolsEnv: the channel its tools call back into
	// the server through. It is open while the supervisor runs, from after the database
	// lock is taken until the last sidecar has exited.
	Tools http.Handler

	// ReadyTimeout bounds how long a fresh sidecar may take to answer /healthz before it is
	// stopped and started again. Default 30 s.
	ReadyTimeout time.Duration
	// StopTimeout is how long a sidecar gets to exit after SIGTERM before SIGKILL. The
	// sidecar gives its Harness 10 s to close, so the default is 15 s.
	StopTimeout time.Duration
	// MinBackoff and MaxBackoff bound the delay before a restart, which doubles after each
	// exit and starts over once a sidecar has stayed ready (healthy) for StableAfter; time
	// spent starting or failing to get ready doesn't count. Defaults: 500 ms, 30 s, 1 min.
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	StableAfter time.Duration
}

// socketName is the socket's file name inside SocketDir.
const socketName = "agent.sock"

// toolsSocketName is the file name of the socket SupervisorConfig.Tools is served on.
const toolsSocketName = "tools.sock"

// ToolsEnv names the tools socket in the sidecar's environment, when there is one.
const ToolsEnv = "TASKTO_AGENT_TOOLS_SOCKET"

// SocketPath is where a supervisor with the SocketDir socketDir has its sidecar listen.
func SocketPath(socketDir string) string { return filepath.Join(socketDir, socketName) }

// maxSocketPath is the longest Unix socket path every supported platform accepts
// (sun_path is 104 bytes on macOS and the BSDs, including the terminating NUL).
const maxSocketPath = 103

// ErrStopped is returned by Ready and Start once the supervisor has been stopped.
var ErrStopped = errors.New("agentrt: supervisor stopped")

// ErrToolsBusy is returned by Stop when a handler of the tools socket was still running, after
// its context ended and its connection closed, when the time for it ran out. It wraps
// drain.ErrBusy: such a handler may still use the server's database.
var ErrToolsBusy = fmt.Errorf("agentrt: a tools call is still running: %w", drain.ErrBusy)

// toolsWait is how long Stop waits for the tools socket's handlers once the sidecar has exited.
// A variable for tests.
var toolsWait = 5 * time.Second

// ErrDatabaseInUse is returned by Start when another supervisor holds the database.
var ErrDatabaseInUse = errors.New("agentrt: database in use by another agent runtime")

// ErrSocketPathTooLong is returned by NewSupervisor when the socket's path is longer than
// a Unix socket path may be.
var ErrSocketPathTooLong = fmt.Errorf("agentrt: socket path longer than %d bytes", maxSocketPath)

// ExitOnEOFEnv is set to 1 in the sidecar's environment: its stdin is a pipe whose other
// end only the supervisor's process holds, and it exits when that pipe closes. However the
// server ends, even with SIGKILL, the kernel closes the pipe, so the sidecar never outlives it.
const ExitOnEOFEnv = "TASKTO_AGENT_EXIT_ON_EOF"

// Supervisor runs one sidecar for one database: it starts it, restarts it with backoff
// when it exits unexpectedly, and stops it.
//
// Only one goroutine ever starts or waits for a sidecar process, and it starts the next one
// only after the previous one has exited, so a supervisor never runs two at once. The lock
// on the database extends that to other supervisors and other processes.
type Supervisor struct {
	cfg    SupervisorConfig
	log    *slog.Logger
	socket string
	tools  string // the tools socket's path
	client *Client

	kill chan struct{} // closed when a Stop's ctx ends: SIGKILL without waiting
	done chan struct{} // closed once the run loop has returned and no sidecar runs

	toolsErr error // what closing the tools socket returned; set before done is closed

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc // ends the run loop; nil until it runs
	ready   chan struct{}      // closed while a sidecar answers; replaced when it goes away
	pid     int                // the running sidecar's PID, 0 when none
}

// NewSupervisor checks cfg and fills in defaults. It starts nothing; call Start.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.SocketDir == "" || cfg.DB == "" {
		return nil, errors.New("agentrt: SocketDir and DB are required")
	}
	if cfg.Node == "" {
		cfg.Node = "node"
	}
	if cfg.Entry == "" {
		cfg.Entry = filepath.Join("agent", "dist", "index.js")
	}
	setDefault(&cfg.ReadyTimeout, 30*time.Second)
	setDefault(&cfg.StopTimeout, 15*time.Second)
	setDefault(&cfg.MinBackoff, 500*time.Millisecond)
	setDefault(&cfg.MaxBackoff, 30*time.Second)
	setDefault(&cfg.StableAfter, time.Minute)
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	socket := SocketPath(cfg.SocketDir)
	tools := filepath.Join(cfg.SocketDir, toolsSocketName)
	for _, p := range []string{socket, tools} {
		if len(p) > maxSocketPath {
			return nil, fmt.Errorf("%w: %s is %d", ErrSocketPathTooLong, p, len(p))
		}
	}
	return &Supervisor{
		cfg:    cfg,
		log:    log.With("component", "agent-sidecar"),
		socket: socket,
		tools:  tools,
		client: New(socket),
		kill:   make(chan struct{}),
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}, nil
}

func setDefault(d *time.Duration, v time.Duration) {
	if *d <= 0 {
		*d = v
	}
}

// Socket returns the path of the sidecar's socket.
func (s *Supervisor) Socket() string { return s.socket }

// PID returns the running sidecar's process ID, or 0 while none runs.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// Client returns the client for the supervised sidecar. It stays valid across restarts;
// calls fail while no sidecar answers.
func (s *Supervisor) Client() *Client { return s.client }

// Start is Launch, then Ready: it waits until the sidecar answers /healthz. The sidecar keeps
// running after Start returns, until Stop; ctx only bounds the wait. If the wait fails, Start
// stops the supervisor before returning. Only one of Start and Launch may be called, once.
func (s *Supervisor) Start(ctx context.Context) (*Client, error) {
	if err := s.Launch(); err != nil {
		return nil, err
	}

	client, err := s.Ready(ctx)
	if err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), s.cfg.StopTimeout+5*time.Second)
		defer stopCancel()
		_ = s.Stop(stopCtx)
		return nil, err
	}
	return client, nil
}

// Launch prepares the socket directory, takes the database lock and starts the sidecar
// without waiting for it to answer: Ready and State tell when it does. The supervisor keeps
// it running, restarting it as needed, until Stop.
func (s *Supervisor) Launch() error {
	s.mu.Lock()
	again := s.started
	s.started = true
	s.mu.Unlock()
	if again {
		return errors.New("agentrt: supervisor already started")
	}
	if err := s.launch(); err != nil {
		_ = s.Stop(context.Background()) // nothing runs: this only marks the supervisor stopped
		return err
	}
	return nil
}

// launch prepares the socket directory, takes the database lock, opens the tools socket
// and starts the run loop.
func (s *Supervisor) launch() error {
	if err := ownerOnlyDir(s.cfg.SocketDir); err != nil {
		return err
	}
	lock, err := lockDB(s.cfg.DB)
	if err != nil {
		return err
	}
	// After the lock: a socket left here is a crashed server's, never a running one's.
	closeTools := func() error { return nil }
	if s.cfg.Tools != nil {
		if closeTools, err = serveTools(s.tools, s.cfg.Tools, s.log); err != nil {
			lock.Close()
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		_ = closeTools()
		lock.Close()
		return ErrStopped
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go s.run(ctx, lock, closeTools)
	return nil
}

// serveTools serves h on a new socket at path, readable and writable by this user only
// (it is in the owner-only socket directory too), and returns the func that closes it.
func serveTools(path string, h http.Handler, log *slog.Logger) (func() error, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("agentrt: %s exists and isn't a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("agentrt: tools socket: %w", err)
		}
	}
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("agentrt: tools socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("agentrt: tools socket: %w", err)
	}
	inflight := drain.New()
	srv := &http.Server{
		Handler:           inflight.Handler(h),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Error("agent tools socket stopped serving", "err", err)
		}
	}()
	return func() error {
		// The sidecar has exited, so no call is in flight that anyone waits for: end their
		// contexts and connections. A handler still running after that may use the server's
		// database, so it is waited for, and Stop says if one never returned.
		err := inflight.Shutdown(srv, 0, toolsWait)
		<-done
		_ = os.Remove(path)
		if errors.Is(err, drain.ErrBusy) {
			log.Error("agent tools socket: a call did not return", "err", err)
			return ErrToolsBusy
		}
		return nil
	}, nil
}

// Ready waits until a sidecar answers /healthz and returns its client. While the sidecar
// restarts, it waits for the next one. It returns ErrStopped once the supervisor stops.
func (s *Supervisor) Ready(ctx context.Context) (*Client, error) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil, ErrStopped
	default:
	}
	select {
	case <-ready:
		return s.client, nil
	case <-s.done:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// The supervisor's states, as State reports them.
const (
	StateStarting = "starting" // no sidecar answers yet: starting, or restarting after an exit
	StateReady    = "ready"    // a sidecar answers
	StateStopped  = "stopped"  // Stop was called, or Launch failed
)

// State reports whether a sidecar answers right now, without waiting.
func (s *Supervisor) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return StateStopped
	default:
	}
	if s.stopped {
		return StateStopped
	}
	select {
	case <-s.ready:
		return StateReady
	default:
		return StateStarting
	}
}

// Stop stops the sidecar: SIGTERM, then SIGKILL if it hasn't exited within StopTimeout, or
// as soon as ctx ends. It returns once the sidecar has exited and the database lock is
// released, with ctx's error if ctx ended first, and ErrToolsBusy if a call on the tools
// socket was still running at the end. It may be called more than once, and
// concurrently with Start.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		if s.cancel != nil {
			s.cancel()
		} else {
			close(s.done) // the run loop never started
		}
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return s.toolsErr
	case <-ctx.Done():
	}
	select {
	case <-s.done:
		return s.toolsErr // stopped in time after all
	default:
	}
	// Out of time: kill now, and still wait, so the caller never sees Stop return while a
	// sidecar holds the database. A killed process is reaped promptly.
	s.mu.Lock()
	select {
	case <-s.kill:
	default:
		close(s.kill)
	}
	s.mu.Unlock()
	<-s.done
	return errors.Join(ctx.Err(), s.toolsErr)
}

// run starts the sidecar and restarts it after each exit until ctx ends. It is the only
// goroutine that starts sidecar processes, and runOnce returns only after its process has
// exited, so one never starts before the last one is gone.
func (s *Supervisor) run(ctx context.Context, lock *os.File, closeTools func() error) {
	defer close(s.done)
	defer lock.Close()
	defer func() { s.toolsErr = closeTools() }()
	backoff := s.cfg.MinBackoff
	for {
		uptime, err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if uptime >= s.cfg.StableAfter {
			backoff = s.cfg.MinBackoff
		}
		s.log.Warn("agent sidecar exited; restarting", "err", err, "backoff", backoff)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, s.cfg.MaxBackoff)
	}
}

// childEnviron is this process's environment without the variables in DropEnv.
func (s *Supervisor) childEnviron() []string {
	env := os.Environ()
	if len(s.cfg.DropEnv) == 0 {
		return env
	}
	return slices.DeleteFunc(env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(s.cfg.DropEnv, name)
	})
}

// runOnce runs one sidecar process until it exits or ctx ends, and returns only after the
// process has exited: how long it was ready (zero if it never was), and why it ended.
func (s *Supervisor) runOnce(ctx context.Context) (uptime time.Duration, err error) {
	out := &logWriter{log: s.log}
	defer out.flush()
	// Not CommandContext: a cancelled context would SIGKILL it, and terminate stops it with
	// SIGTERM first. G204: the command and entry come from the server's config.
	cmd := exec.Command(s.cfg.Node, s.cfg.Entry) //nolint:noctx,gosec // see above
	cmd.Env = append(s.childEnviron(), s.cfg.Env...)
	cmd.Env = append(cmd.Env, "TASKTO_AGENT_SOCKET="+s.socket, "TASKTO_AGENT_DB="+s.cfg.DB, ExitOnEOFEnv+"=1")
	// Always set, empty without tools, so the sidecar never finds a socket from elsewhere.
	tools := ""
	if s.cfg.Tools != nil {
		tools = s.tools
	}
	cmd.Env = append(cmd.Env, ToolsEnv+"="+tools)
	cmd.Stdout = out
	cmd.Stderr = out
	// A pipe nothing writes to: only this process holds its write end (close-on-exec, so no
	// other child inherits it), and Wait closes it. If this process dies, even by SIGKILL,
	// the sidecar reads EOF and exits (see ExitOnEOFEnv).
	if _, err := cmd.StdinPipe(); err != nil {
		return 0, fmt.Errorf("stdin pipe: %w", err)
	}
	// The database lock stays with this process; it isn't handed to the sidecar, whose
	// children (tool commands) would inherit it and could hold it forever. A sidecar left
	// behind by a crashed server is still caught: a new sidecar's startup probe finds it on
	// the socket and exits before opening the database.
	// Its own process group: a Ctrl-C in the terminal goes to the server, which stops the
	// sidecar itself, instead of to both at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Don't hang on the output pipes if something the sidecar started still holds them.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	pid := cmd.Process.Pid
	s.log.Info("agent sidecar started", "pid", pid)
	s.mu.Lock()
	s.pid = pid
	s.mu.Unlock()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		s.mu.Lock()
		s.pid = 0
		s.mu.Unlock()
	}()

	if err := s.waitHealthy(ctx, exited, pid); err != nil {
		if errors.Is(err, errExited) {
			return 0, err
		}
		s.terminate(cmd, exited)
		return 0, err
	}
	s.setReady(true)
	defer s.setReady(false)
	readyAt := time.Now()
	s.log.Info("agent sidecar ready", "pid", pid, "socket", s.socket)

	select {
	case err := <-exited:
		return time.Since(readyAt), exitError(err)
	case <-ctx.Done():
		s.terminate(cmd, exited)
		return time.Since(readyAt), ctx.Err()
	}
}

var errExited = errors.New("exited before it was ready")

// waitHealthy polls /healthz until the sidecar answers, it exits, ReadyTimeout passes or ctx
// ends. When it exits, the error wraps errExited and the process has been reaped.
// Only the process it started counts: a sidecar left behind by a crashed server may still
// answer on the socket, and the new one then exits on its startup probe.
func (s *Supervisor) waitHealthy(ctx context.Context, exited <-chan error, pid int) error {
	warned := false
	deadline := time.NewTimer(s.cfg.ReadyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("%w: %w", errExited, exitError(err))
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("not healthy within %s", s.cfg.ReadyTimeout)
		case <-tick.C:
			probe, cancel := context.WithTimeout(ctx, time.Second)
			h, err := s.client.Health(probe)
			cancel()
			if err != nil {
				continue
			}
			if h.PID == pid && h.Status == "ok" {
				return nil
			}
			if h.PID == pid {
				continue // our sidecar, but not ok yet
			}
			if !warned {
				s.log.Warn("another agent runtime answers on the socket; waiting for it to go", "its_pid", h.PID, "socket", s.socket)
				warned = true
			}
		}
	}
}

// terminate stops a running sidecar: SIGTERM, then SIGKILL after StopTimeout or when Stop
// runs out of time. It returns once the process has exited.
func (s *Supervisor) terminate(cmd *exec.Cmd, exited <-chan error) {
	pid := cmd.Process.Pid
	s.log.Info("stopping agent sidecar", "pid", pid)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	t := time.NewTimer(s.cfg.StopTimeout)
	defer t.Stop()
	select {
	case err := <-exited:
		s.log.Info("agent sidecar stopped", "pid", pid, "exit", exitError(err))
		return
	case <-t.C:
		s.log.Warn("agent sidecar ignored SIGTERM; killing it", "pid", pid, "after", s.cfg.StopTimeout)
	case <-s.kill:
		s.log.Warn("out of time to stop the agent sidecar; killing it", "pid", pid)
	}
	// If it exited meanwhile it's already reaped and its pid may be reused: don't signal.
	select {
	case err := <-exited:
		s.log.Info("agent sidecar stopped", "pid", pid, "exit", exitError(err))
		return
	default:
	}
	// The whole group, so nothing the sidecar started outlives it.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	s.log.Info("agent sidecar killed", "pid", pid, "exit", <-exited)
}

func (s *Supervisor) setReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.ready:
		if !ready {
			s.ready = make(chan struct{})
		}
	default:
		if ready {
			close(s.ready)
		}
	}
}

// exitError describes how a process ended; a nil error still means it ended.
func exitError(err error) error {
	if err == nil {
		return errors.New("exited with status 0")
	}
	return err
}

// ownerOnlyDir creates dir with mode 0700, or checks that an existing one is a directory
// (not a symlink) owned by this user with no access for group or others.
func ownerOnlyDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil {
		return os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs x; 0700 is owner-only. Exact whatever the umask.
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("agentrt: socket directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("agentrt: socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("agentrt: socket directory %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("agentrt: socket directory %s has mode %o; want 700", dir, perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("agentrt: socket directory %s is owned by uid %d, not this user", dir, st.Uid)
	}
	return nil
}

// lockDB takes an exclusive lock on DB + ".lock" without waiting.
func lockDB(db string) (*os.File, error) {
	f, err := lockfile.Lock(db + ".lock")
	if errors.Is(err, lockfile.ErrLocked) {
		return nil, fmt.Errorf("%w: %w", ErrDatabaseInUse, err)
	}
	if err != nil {
		return nil, fmt.Errorf("agentrt: database lock: %w", err)
	}
	return f, nil
}

// logWriter logs each line the sidecar writes. A line longer than maxLogLine is logged in
// pieces.
type logWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

const maxLogLine = 16 << 10

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		// No complete line yet, or one longer than the cap: log full-size pieces first.
		if i < 0 || i > maxLogLine {
			if len(w.buf) >= maxLogLine {
				w.emit(w.buf[:maxLogLine])
				w.buf = w.buf[maxLogLine:]
				continue
			}
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	w.buf = bytes.Clone(w.buf) // don't pin a large backing array
	return len(p), nil
}

func (w *logWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

func (w *logWriter) emit(line []byte) {
	w.log.Info("agent sidecar output", "line", string(bytes.TrimRight(line, "\r")))
}
