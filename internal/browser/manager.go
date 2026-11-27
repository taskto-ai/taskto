package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"
)

type Host interface {
	Bin(prog Program) string
	Start(user int64, spec ProcSpec, extra []*os.File) (*exec.Cmd, error)
	DialVNC(ctx context.Context, user int64) (io.ReadWriteCloser, error)
	Clean(user int64) error
	Purge(user int64) error
}

type Config struct {
	Host                    Host
	Idle                    time.Duration
	Max                     int
	NoSandbox               bool
	StartTimeout, StopGrace time.Duration
	Tick                    time.Duration
}

type Manager struct {
	cfg Config

	mu        sync.Mutex
	sessions  map[key]*session
	stopping  map[key][]*session
	forgotten map[int64]struct{}
	closed    bool
	stopTick  chan struct{}
	wg        sync.WaitGroup
}

type key struct {
	user int64
	kind Kind
}

type session struct {
	key      key
	ready    chan struct{}
	err      error
	inst     *instance
	procs    []*exec.Cmd
	waits    []chan struct{}
	chromium *exec.Cmd

	lastUse time.Time
	live    int
	conns   map[*liveConn]struct{}
	ending  bool
	ended   chan struct{}
}

func NewManager(cfg Config) *Manager {
	if cfg.Idle <= 0 {
		cfg.Idle = 5 * time.Minute
	}
	if cfg.Max <= 0 {
		cfg.Max = 4
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 30 * time.Second
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = 5 * time.Second
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 15 * time.Second
	}
	m := &Manager{cfg: cfg, sessions: map[key]*session{}, stopping: map[key][]*session{}, forgotten: map[int64]struct{}{}, stopTick: make(chan struct{})}
	m.wg.Add(1)
	go m.idleLoop()
	return m
}

func (m *Manager) Available(kind Kind) bool {
	h := m.cfg.Host
	if h == nil || h.Bin(Chromium) == "" {
		return false
	}
	return kind == Testing || (h.Bin(Xvfb) != "" && h.Bin(X11vnc) != "")
}

type State struct {
	Available bool       `json:"available"`
	Running   bool       `json:"running"`
	Live      bool       `json:"live"`
	StopsAt   *time.Time `json:"stops_at,omitempty"`
	Tabs      []Tab      `json:"tabs,omitempty"`
}

func (m *Manager) Availability(context.Context) map[Kind]bool {
	return map[Kind]bool{Personal: m.Available(Personal), Testing: m.Available(Testing)}
}

func (m *Manager) Status(ctx context.Context, user int64) (map[Kind]State, error) {
	out := map[Kind]State{}
	for _, kind := range []Kind{Personal, Testing} {
		st := State{Available: m.Available(kind)}
		m.mu.Lock()
		s := m.sessions[key{user, kind}]
		var inst *instance
		if s != nil && !s.ending && isClosed(s.ready) && s.err == nil {
			st.Running, st.Live, inst = true, s.live > 0, s.inst
			if s.live == 0 {
				at := s.lastUse.Add(m.cfg.Idle)
				st.StopsAt = &at
			}
		}
		m.mu.Unlock()
		if inst != nil {
			tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			st.Tabs, _ = inst.tabs(tctx)
			cancel()
		}
		out[kind] = st
	}
	return out, nil
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (m *Manager) Start(ctx context.Context, user int64, kind Kind) error {
	if !kind.Valid() {
		return badf("browser must be %q or %q", Personal, Testing)
	}
	_, err := m.session(ctx, user, kind, true)
	return err
}

func (m *Manager) Stop(_ context.Context, user int64, kind Kind) error {
	if !kind.Valid() {
		return badf("browser must be %q or %q", Personal, Testing)
	}
	m.mu.Lock()
	s := m.sessions[key{user, kind}]
	if s != nil {
		m.endLocked(s)
	}
	m.mu.Unlock()
	if s != nil {
		<-s.ended
	}
	return nil
}

func (m *Manager) Forget(ctx context.Context, user int64) error {
	if user <= 0 {
		return badf("invalid user")
	}
	m.mu.Lock()
	m.forgotten[user] = struct{}{}
	var wait []*session
	var conns []*liveConn
	for _, kind := range []Kind{Personal, Testing} {
		k := key{user, kind}
		wait = append(wait, m.stopping[k]...)
		if s := m.sessions[k]; s != nil {
			for c := range s.conns {
				conns = append(conns, c)
			}
			m.endLocked(s)
			wait = append(wait, s)
		}
	}
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	for _, s := range wait {
		select {
		case <-s.ended:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.cfg.Host == nil {
		return nil
	}
	if err := m.cfg.Host.Purge(user); err != nil {
		return fmt.Errorf("removing the browser files: %w", err)
	}
	return nil
}

func (m *Manager) Do(ctx context.Context, user int64, a Action) (Result, error) {
	if err := a.Check(); err != nil {
		return Result{}, err
	}
	s, err := m.session(ctx, user, a.Browser, a.Op != OpTabs)
	if errors.Is(err, ErrNotRunning) && a.Op == OpTabs {
		return Result{Tabs: []Tab{}}, nil
	}
	if err != nil {
		return Result{}, err
	}
	res, err := s.inst.do(ctx, a)
	m.touch(s)
	if errors.Is(err, errPipeClosed) {
		return Result{}, errors.New("the browser stopped: try again")
	}
	return res, err
}

const MaxLive = 4

func (m *Manager) OpenLive(ctx context.Context, user int64) (io.ReadWriteCloser, error) {
	s, err := m.session(ctx, user, Personal, false)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if s.live >= MaxLive {
		m.mu.Unlock()
		return nil, ErrTooManyLive
	}
	s.live++
	s.lastUse = time.Now()
	m.mu.Unlock()
	conn, err := m.cfg.Host.DialVNC(ctx, user)
	if err != nil {
		m.mu.Lock()
		s.live--
		m.mu.Unlock()
		return nil, fmt.Errorf("the live view: %w", err)
	}
	lc := &liveConn{ReadWriteCloser: conn}
	lc.release = func() {
		m.mu.Lock()
		s.live--
		delete(s.conns, lc)
		s.lastUse = time.Now()
		m.mu.Unlock()
	}
	m.mu.Lock()
	if s.ending {
		m.mu.Unlock()
		_ = lc.Close()
		return nil, ErrNotRunning
	}
	if s.conns == nil {
		s.conns = map[*liveConn]struct{}{}
	}
	s.conns[lc] = struct{}{}
	m.mu.Unlock()
	return lc, nil
}

type liveConn struct {
	io.ReadWriteCloser
	once    sync.Once
	release func()
}

func (c *liveConn) Close() error {
	c.once.Do(c.release)
	return c.ReadWriteCloser.Close()
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stopTick)
	var all []*session
	for _, s := range m.sessions {
		m.endLocked(s)
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		<-s.ended
	}
	m.wg.Wait()
}

func (m *Manager) touch(s *session) {
	m.mu.Lock()
	s.lastUse = time.Now()
	m.mu.Unlock()
}

func (m *Manager) session(ctx context.Context, user int64, kind Kind, start bool) (*session, error) {
	k := key{user, kind}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if _, gone := m.forgotten[user]; gone {
		m.mu.Unlock()
		return nil, ErrAccountClosing
	}
	s := m.sessions[k]
	if s == nil {
		if !start {
			m.mu.Unlock()
			return nil, ErrNotRunning
		}
		if !m.Available(kind) {
			m.mu.Unlock()
			return nil, ErrUnavailable
		}
		if len(m.sessions) >= m.cfg.Max && !m.evictLocked() {
			m.mu.Unlock()
			return nil, ErrBusy
		}
		s = &session{key: k, ready: make(chan struct{}), ended: make(chan struct{}), lastUse: time.Now()}
		m.sessions[k] = s
		prev := append([]*session(nil), m.stopping[k]...)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.launch(s, prev)
		}()
	}
	s.lastUse = time.Now()
	m.mu.Unlock()
	select {
	case <-s.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func (m *Manager) evictLocked() bool {
	var lru *session
	for _, s := range m.sessions {
		if s.live == 0 && !s.ending && isClosed(s.ready) && (lru == nil || s.lastUse.Before(lru.lastUse)) {
			lru = s
		}
	}
	if lru == nil {
		return false
	}
	slog.Info("browser: stopping the least recently used browser to make room", "user", lru.key.user, "browser", lru.key.kind)
	m.endLocked(lru)
	return true
}

func (m *Manager) endLocked(s *session) {
	if s.ending {
		return
	}
	s.ending = true
	if m.sessions[s.key] == s {
		delete(m.sessions, s.key)
	}
	m.stopping[s.key] = append(m.stopping[s.key], s)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-s.ready
		m.stop(s)
		m.mu.Lock()
		m.stopping[s.key] = slices.DeleteFunc(m.stopping[s.key], func(x *session) bool { return x == s })
		if len(m.stopping[s.key]) == 0 {
			delete(m.stopping, s.key)
		}
		m.mu.Unlock()
	}()
}

func (m *Manager) idleLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-m.stopTick:
			return
		case now := <-t.C:
			m.mu.Lock()
			for _, s := range m.sessions {
				if s.live == 0 && isClosed(s.ready) && now.Sub(s.lastUse) >= m.cfg.Idle {
					slog.Info("browser: stopping an idle browser", "user", s.key.user, "browser", s.key.kind)
					m.endLocked(s)
				}
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) launch(s *session, prev []*session) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.StartTimeout)
	defer cancel()
	var err error
	for _, p := range prev {
		select {
		case <-p.ended:
		case <-ctx.Done():
			err = errors.New("the browser before it didn't stop in time")
		}
		if err != nil {
			break
		}
	}
	if err == nil {
		err = m.start(ctx, s)
	}
	if err != nil {
		slog.Warn("browser: could not start", "user", s.key.user, "browser", s.key.kind, "err", err)
		s.err = fmt.Errorf("the browser could not start: %w", err)
		close(s.ready)
		m.mu.Lock()
		m.endLocked(s)
		m.mu.Unlock()
		return
	}
	close(s.ready)
	go func() {
		select {
		case <-s.inst.cdp.Done():
		case <-s.ended:
			return
		}
		m.mu.Lock()
		if !s.ending {
			slog.Warn("browser: it exited on its own", "user", s.key.user, "browser", s.key.kind)
		}
		m.endLocked(s)
		m.mu.Unlock()
	}()
}

func (m *Manager) start(ctx context.Context, s *session) error {
	h, user := m.cfg.Host, s.key.user
	display := 0
	if s.key.kind == Personal {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		err = m.add(s, h, user, ProcSpec{Program: Xvfb, Kind: Personal}, w)
		w.Close()
		if err != nil {
			r.Close()
			return fmt.Errorf("starting Xvfb: %w", err)
		}
		display, err = readLineWithin(ctx, r, s.waits[0])
		r.Close()
		if err != nil {
			return fmt.Errorf("starting Xvfb: %w", err)
		}
	}
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		cmdR.Close()
		cmdW.Close()
		return err
	}
	err = m.add(s, h, user, ProcSpec{Program: Chromium, Kind: s.key.kind, Display: display, NoSandbox: m.cfg.NoSandbox}, cmdR, outW)
	cmdR.Close()
	outW.Close()
	if err != nil {
		cmdW.Close()
		outR.Close()
		return fmt.Errorf("starting Chromium: %w", err)
	}
	s.chromium = s.procs[len(s.procs)-1]
	c := newCDP(cmdW, outR)
	s.inst = newInstance(s.key.kind, c)
	if err := c.Call(ctx, "", "Browser.getVersion", nil, nil); err != nil {
		return fmt.Errorf("the browser didn't answer: %w", err)
	}
	if s.key.kind == Personal {
		if err := m.add(s, h, user, ProcSpec{Program: X11vnc, Kind: Personal, Display: display}); err != nil {
			return fmt.Errorf("starting x11vnc: %w", err)
		}
	}
	return nil
}

func (m *Manager) add(s *session, h Host, user int64, spec ProcSpec, extra ...*os.File) error {
	cmd, err := h.Start(user, spec, extra)
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	s.procs = append(s.procs, cmd)
	s.waits = append(s.waits, exited)
	return nil
}

func readLineWithin(ctx context.Context, r *os.File, exited <-chan struct{}) (int, error) {
	type line struct {
		s   string
		err error
	}
	got := make(chan line, 1)
	go func() {
		s, err := bufio.NewReader(r).ReadString('\n')
		got <- line{s, err}
	}()
	select {
	case l := <-got:
		if l.err != nil && l.s == "" {
			return 0, errors.New("it exited without a display")
		}
		return readDisplay(l.s)
	case <-exited:
		return 0, errors.New("it exited")
	case <-ctx.Done():
		r.Close()
		return 0, errors.New("it didn't start in time")
	}
}

func (m *Manager) stop(s *session) {
	defer close(s.ended)
	if s.inst != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.inst.cdp.Call(ctx, "", "Browser.close", nil, nil)
		cancel()
		_ = s.inst.cdp.Close()
	}
	for i := len(s.procs) - 1; i >= 0; i-- {
		var wait time.Duration
		if s.procs[i] == s.chromium {
			wait = m.cfg.StopGrace / 2
		}
		stopGroup(s.procs[i], s.waits[i], wait, m.cfg.StopGrace)
	}
	if s.key.kind == Testing && m.cfg.Host != nil {
		if err := m.cfg.Host.Clean(s.key.user); err != nil {
			slog.Warn("browser: removing the testing profile", "user", s.key.user, "err", err)
		}
	}
}

func stopGroup(cmd *exec.Cmd, exited <-chan struct{}, wait, grace time.Duration) {
	pgid := cmd.Process.Pid
	running := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
	select {
	case <-exited:
		return
	case <-time.After(wait):
	}
	if running() {
		_ = signalGroup(pgid, sigTerm)
	}
	select {
	case <-exited:
		return
	case <-time.After(grace):
	}
	if running() {
		_ = signalGroup(pgid, sigKill)
	}
	<-exited
}
