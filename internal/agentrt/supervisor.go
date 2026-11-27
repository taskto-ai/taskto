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

type SupervisorConfig struct {
	Node      string
	Entry     string
	SocketDir string
	DB        string
	Env       []string
	DropEnv   []string
	Logger    *slog.Logger
	Tools     http.Handler

	ReadyTimeout time.Duration
	StopTimeout  time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	StableAfter  time.Duration
}

const socketName = "agent.sock"

const toolsSocketName = "tools.sock"

const ToolsEnv = "TASKTO_AGENT_TOOLS_SOCKET"

func SocketPath(socketDir string) string { return filepath.Join(socketDir, socketName) }

const maxSocketPath = 103

var ErrStopped = errors.New("agentrt: supervisor stopped")

var ErrToolsBusy = fmt.Errorf("agentrt: a tools call is still running: %w", drain.ErrBusy)

var toolsWait = 5 * time.Second

var ErrDatabaseInUse = errors.New("agentrt: database in use by another agent runtime")

var ErrSocketPathTooLong = fmt.Errorf("agentrt: socket path longer than %d bytes", maxSocketPath)

const ExitOnEOFEnv = "TASKTO_AGENT_EXIT_ON_EOF"

type Supervisor struct {
	cfg    SupervisorConfig
	log    *slog.Logger
	socket string
	tools  string
	client *Client

	kill chan struct{}
	done chan struct{}

	toolsErr error

	mu      sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	ready   chan struct{}
	pid     int
}

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

func (s *Supervisor) Socket() string { return s.socket }

func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *Supervisor) Client() *Client { return s.client }

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

func (s *Supervisor) Launch() error {
	s.mu.Lock()
	again := s.started
	s.started = true
	s.mu.Unlock()
	if again {
		return errors.New("agentrt: supervisor already started")
	}
	if err := s.launch(); err != nil {
		_ = s.Stop(context.Background())
		return err
	}
	return nil
}

func (s *Supervisor) launch() error {
	if err := ownerOnlyDir(s.cfg.SocketDir); err != nil {
		return err
	}
	lock, err := lockDB(s.cfg.DB)
	if err != nil {
		return err
	}
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

const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopped  = "stopped"
)

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

func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		if s.cancel != nil {
			s.cancel()
		} else {
			close(s.done)
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
		return s.toolsErr
	default:
	}
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

func (s *Supervisor) runOnce(ctx context.Context) (uptime time.Duration, err error) {
	out := &logWriter{log: s.log}
	defer out.flush()
	cmd := exec.CommandContext(context.WithoutCancel(ctx), s.cfg.Node, s.cfg.Entry)
	cmd.Env = append(s.childEnviron(), s.cfg.Env...)
	cmd.Env = append(cmd.Env, "TASKTO_AGENT_SOCKET="+s.socket, "TASKTO_AGENT_DB="+s.cfg.DB, ExitOnEOFEnv+"=1")
	tools := ""
	if s.cfg.Tools != nil {
		tools = s.tools
	}
	cmd.Env = append(cmd.Env, ToolsEnv+"="+tools)
	cmd.Stdout = out
	cmd.Stderr = out
	if _, err := cmd.StdinPipe(); err != nil {
		return 0, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
				continue
			}
			if !warned {
				s.log.Warn("another agent runtime answers on the socket; waiting for it to go", "its_pid", h.PID, "socket", s.socket)
				warned = true
			}
		}
	}
}

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
	select {
	case err := <-exited:
		s.log.Info("agent sidecar stopped", "pid", pid, "exit", exitError(err))
		return
	default:
	}
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

func exitError(err error) error {
	if err == nil {
		return errors.New("exited with status 0")
	}
	return err
}

func ownerOnlyDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err == nil {
		return os.Chmod(dir, 0o700)
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
	w.buf = bytes.Clone(w.buf)
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
