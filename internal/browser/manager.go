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

	"taskto.ai/taskto/internal/cliowner"
)

// Host starts a browser's processes for a user: as the server's own OS user (LocalHost), or
// as the user's own Unix user in the CLI worker (container mode).
type Host interface {
	// Bin is prog's executable, or "" if it isn't installed.
	Bin(prog Program) string
	// Start starts spec's process for user in a process group of its own, with extra as its
	// fds 3, 4, …, and returns it running.
	Start(a cliowner.Account, spec ProcSpec, extra []*os.File) (*exec.Cmd, error)
	// DialVNC connects to user's VNC socket.
	DialVNC(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error)
	// Clean removes user's testing profile.
	Clean(a cliowner.Account) error
	// Purge removes everything kept for user's browsers, the personal profile included, for a
	// deleted account. No process of theirs runs by then. Removing what isn't there is not an error.
	Purge(a cliowner.Account) error
}

// Config configures a Manager.
type Config struct {
	Host Host
	// Idle is how long a browser runs unused before it is stopped (default 5 minutes). An open
	// live view counts as use.
	Idle time.Duration
	// Max bounds the browsers running at once, of every user and kind (default 4).
	Max int
	// NoSandbox turns off Chromium's sandbox (container mode: docs/agent-browser.md).
	NoSandbox bool
	// StartTimeout bounds a browser's start (default 30 s); StopGrace is the time between
	// SIGTERM and SIGKILL when it stops (default 5 s).
	StartTimeout, StopGrace time.Duration
	// Tick is how often idle browsers are looked for (default 15 s).
	Tick time.Duration
	// Allow, when set, is asked before every operation on a user's browsers, the reading of a
	// running one's state and tabs too, apart from stopping them: an error refuses it (the CLI
	// worker's rule, cliworker.Worker.BrowserAllowed).
	Allow func(a cliowner.Account) error
	// Fence, when set, runs a deleted account's Forget (do) under the CLI worker's rule for
	// deletions (cliworker.Worker.FenceDeletion): only the latest server's, the account retired
	// first. A purge an earlier server sent then stops and removes nothing.
	Fence func(ctx context.Context, a cliowner.Account, do func() error) error
}

// allowed asks Config.Allow about a.
func (m *Manager) allowed(a cliowner.Account) error {
	if m.cfg.Allow == nil {
		return nil
	}
	return m.cfg.Allow(a)
}

// Manager runs the users' browsers. Its methods are safe for concurrent use.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	sessions map[key]*session
	// stopping is the browsers of each key still being stopped: a new one on the same profile
	// waits for them to be gone, so the old ones have written their cookies first.
	stopping map[key][]*session
	// forgotten is the accounts that are deleted (Forget), by identity, with when: no browser of
	// theirs starts. It is kept as long as the Manager runs, unless a restore of the server's data
	// goes back before it (Unforget).
	forgotten map[string]time.Time
	closed    bool
	stopTick  chan struct{}
	wg        sync.WaitGroup
}

// key is a browser of one account, by its identity: never by its number, which a restore of the
// server's data may give to another account.
type key struct {
	id   string
	kind Kind
}

// session is one running (or starting) browser.
type session struct {
	key      key
	acct     cliowner.Account
	ready    chan struct{} // closed when the start is over; err says how
	err      error
	inst     *instance
	procs    []*exec.Cmd // started so far, in order
	waits    []chan struct{}
	chromium *exec.Cmd

	// Under Manager.mu:
	lastUse time.Time
	live    int
	conns   map[*liveConn]struct{} // the live views open on it
	ending  bool
	ended   chan struct{} // closed when every process is gone
}

// NewManager returns a Manager.
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
	m := &Manager{cfg: cfg, sessions: map[key]*session{}, stopping: map[key][]*session{}, forgotten: map[string]time.Time{}, stopTick: make(chan struct{})}
	m.wg.Add(1)
	go m.idleLoop()
	return m
}

// Available reports whether kind can run here: Chromium for both, and Xvfb and x11vnc for the
// personal browser.
func (m *Manager) Available(kind Kind) bool {
	h := m.cfg.Host
	if h == nil || h.Bin(Chromium) == "" {
		return false
	}
	return kind == Testing || (h.Bin(Xvfb) != "" && h.Bin(X11vnc) != "")
}

// State is one browser's state, for Status.
type State struct {
	Available bool `json:"available"`
	Running   bool `json:"running"`
	// Live says a live view is open; StopsAt is when the browser stops if nothing uses it.
	Live    bool       `json:"live"`
	StopsAt *time.Time `json:"stops_at,omitempty"`
	Tabs    []Tab      `json:"tabs,omitempty"`
}

// Availability says which browsers can run here.
func (m *Manager) Availability(context.Context) map[Kind]bool {
	return map[Kind]bool{Personal: m.Available(Personal), Testing: m.Available(Testing)}
}

// Status is user's two browsers. The tabs are listed for a running browser that answers in a
// few seconds.
func (m *Manager) Status(ctx context.Context, a cliowner.Account) (map[Kind]State, error) {
	if err := m.allowed(a); err != nil {
		return nil, err
	}
	out := map[Kind]State{}
	for _, kind := range []Kind{Personal, Testing} {
		st := State{Available: m.Available(kind)}
		m.mu.Lock()
		s := m.sessions[key{a.Identity, kind}]
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

// Start starts user's kind browser if it isn't running.
func (m *Manager) Start(ctx context.Context, a cliowner.Account, kind Kind) error {
	if !kind.Valid() {
		return badf("browser must be %q or %q", Personal, Testing)
	}
	_, err := m.session(ctx, a, kind, true)
	return err
}

// Stop stops user's kind browser, and returns when it is gone.
func (m *Manager) Stop(_ context.Context, a cliowner.Account, kind Kind) error {
	if !kind.Valid() {
		return badf("browser must be %q or %q", Personal, Testing)
	}
	m.mu.Lock()
	s := m.sessions[key{a.Identity, kind}]
	if s != nil {
		m.endLocked(s)
	}
	m.mu.Unlock()
	if s != nil {
		<-s.ended
	}
	return nil
}

// Forget removes everything of user's browsers, for an account that is deleted: no browser of
// theirs starts from the first moment (ErrAccountClosing), their live views are closed, their
// running browsers are stopped and waited for (a browser still starting or already stopping too),
// and then the personal profile and the rest of their browser directory are removed. It returns
// nil only when the processes are gone and the files removed; on an error (ctx ended before they
// were gone, or a file couldn't be removed) it is safe to call again. Other users' browsers are not
// touched.
//
// The refusal lasts as long as the Manager runs: an account's id is never given to another
// account, unless a restore of the server's data goes back before the deletion (Unforget). A
// Manager that restarts forgets it, and then the account's being switched off is what keeps its
// browsers from starting.
func (m *Manager) Forget(ctx context.Context, a cliowner.Account) error {
	if a.User <= 0 || (a.Identity != "" && !cliowner.IsIdentity(a.Identity)) {
		return badf("invalid account")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Identity != "" && m.cfg.Fence != nil {
		return m.cfg.Fence(ctx, a, func() error { return m.forget(ctx, a) })
	}
	return m.forget(ctx, a)
}

func (m *Manager) forget(ctx context.Context, a cliowner.Account) error {
	// A purge from before identities were kept names none: no session is keyed by it, and the
	// host removes only the number's directory from before homes.
	if a.Identity != "" {
		if err := m.stopAll(ctx, a, true); err != nil {
			return err
		}
	}
	if m.cfg.Host == nil {
		return nil
	}
	if err := m.cfg.Host.Purge(a); err != nil {
		return fmt.Errorf("removing the browser files: %w", err)
	}
	return nil
}

// StopAll stops user's browsers and waits until their processes are gone, as Forget does, but
// neither refuses later starts nor removes a file: the CLI worker does both around it when it
// removes a user's whole directory, the browser's profile with it (cliworker.Worker.Reconcile).
// Other users' browsers are not touched.
func (m *Manager) StopAll(ctx context.Context, a cliowner.Account) error {
	if !a.Valid() {
		return badf("invalid account")
	}
	return m.stopAll(ctx, a, false)
}

// Unforget drops the refusals Forget made before `before` of the users again says yes to: an
// account a restore of the server's data brought back, or a number it gives to someone again
// (cliworker.Worker.Reconcile). A refusal made after it is a deletion the restored database can't
// know of, and stays.
func (m *Manager) Unforget(again func(identity string) bool, before time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, at := range m.forgotten {
		if at.Before(before) && again(id) {
			delete(m.forgotten, id)
		}
	}
}

// stopAll closes user's live views and stops their browsers, and waits for them to be gone (one
// still starting or already stopping too). With forget, nothing of theirs starts any more.
func (m *Manager) stopAll(ctx context.Context, a cliowner.Account, forget bool) error {
	m.mu.Lock()
	if forget {
		m.forgotten[a.Identity] = time.Now()
	}
	var wait []*session
	var conns []*liveConn
	for _, kind := range []Kind{Personal, Testing} {
		k := key{a.Identity, kind}
		// Every browser already stopping is waited for too: a newer one whose start gave up waiting
		// for them doesn't take their place.
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
	return nil
}

// Do runs a on user's browser, starting it first unless a only lists the tabs.
func (m *Manager) Do(ctx context.Context, acct cliowner.Account, a Action) (Result, error) {
	if err := a.Check(); err != nil {
		return Result{}, err
	}
	s, err := m.session(ctx, acct, a.Browser, a.Op != OpTabs)
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

// MaxLive bounds the live views open on one browser at once: each holds a connection, and
// keeps the browser from stopping when idle.
const MaxLive = 4

// OpenLive connects to user's personal browser's display, for a live view: an RFB stream.
// The browser must be running; while the view is open it doesn't stop for being idle.
func (m *Manager) OpenLive(ctx context.Context, a cliowner.Account) (io.ReadWriteCloser, error) {
	s, err := m.session(ctx, a, Personal, false)
	if err != nil {
		return nil, err
	}
	// A place is taken before connecting, so views opened at once can't pass the cap.
	m.mu.Lock()
	if s.live >= MaxLive {
		m.mu.Unlock()
		return nil, ErrTooManyLive
	}
	s.live++
	s.lastUse = time.Now()
	m.mu.Unlock()
	conn, err := m.cfg.Host.DialVNC(ctx, a)
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
	// A browser that began to end while the view was connecting (its account is being deleted,
	// say) is not shown: the view is closed here, not left to a Forget that has already looked.
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

// Close stops every browser and waits for them.
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

// session returns user's kind browser, ready, starting it if start is set.
func (m *Manager) session(ctx context.Context, a cliowner.Account, kind Kind, start bool) (*session, error) {
	if !a.Valid() {
		return nil, badf("invalid account")
	}
	k := key{a.Identity, kind}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if _, gone := m.forgotten[a.Identity]; gone {
		m.mu.Unlock()
		return nil, ErrAccountClosing
	}
	if err := m.allowed(a); err != nil {
		m.mu.Unlock()
		return nil, err
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
		s = &session{key: k, acct: a, ready: make(chan struct{}), ended: make(chan struct{}), lastUse: time.Now()}
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

// evictLocked stops the least recently used browser without a live view, to make room; false
// if there is none.
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
	slog.Info("browser: stopping the least recently used browser to make room", "user", lru.acct.User, "browser", lru.key.kind)
	m.endLocked(lru)
	return true
}

// endLocked takes s off the map and stops it in the background.
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
					slog.Info("browser: stopping an idle browser", "user", s.acct.User, "browser", s.key.kind)
					m.endLocked(s)
				}
			}
			m.mu.Unlock()
		}
	}
}

// launch starts s's processes, once prev (the same user's same browser, the ones being stopped) are
// gone, and closes s.ready. A browser that fails to start is taken off the map, with whatever
// started of it stopped.
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
		slog.Warn("browser: could not start", "user", s.acct.User, "browser", s.key.kind, "err", err)
		s.err = fmt.Errorf("the browser could not start: %w", err)
		close(s.ready)
		m.mu.Lock()
		m.endLocked(s)
		m.mu.Unlock()
		return
	}
	close(s.ready)
	// The browser ending on its own (it crashed, or the user closed its window) ends the
	// session: the next request starts a new one.
	go func() {
		select {
		case <-s.inst.cdp.Done():
		case <-s.ended:
			return
		}
		m.mu.Lock()
		if !s.ending {
			slog.Warn("browser: it exited on its own", "user", s.acct.User, "browser", s.key.kind)
		}
		m.endLocked(s)
		m.mu.Unlock()
	}()
}

func (m *Manager) start(ctx context.Context, s *session) error {
	h, acct := m.cfg.Host, s.acct
	display := 0
	if s.key.kind == Personal {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		err = m.add(s, h, acct, ProcSpec{Program: Xvfb, Kind: Personal}, w)
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
	// Chromium reads commands on its fd 3 and writes replies on its fd 4.
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
	err = m.add(s, h, acct, ProcSpec{Program: Chromium, Kind: s.key.kind, Display: display, NoSandbox: m.cfg.NoSandbox}, cmdR, outW)
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
		if err := m.add(s, h, acct, ProcSpec{Program: X11vnc, Kind: Personal, Display: display}); err != nil {
			return fmt.Errorf("starting x11vnc: %w", err)
		}
	}
	return nil
}

// add starts one process of s and keeps it.
func (m *Manager) add(s *session, h Host, acct cliowner.Account, spec ProcSpec, extra ...*os.File) error {
	cmd, err := h.Start(acct, spec, extra)
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

// readLineWithin reads the display number Xvfb writes, unless it exits or ctx ends first.
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
		r.Close() // ends the read
		return 0, errors.New("it didn't start in time")
	}
}

// stop ends s: Chromium closed over DevTools first, so the profile is written, then every
// process group, last started first, with SIGTERM and then SIGKILL.
func (m *Manager) stop(s *session) {
	defer close(s.ended)
	if s.inst != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.inst.cdp.Call(ctx, "", "Browser.close", nil, nil)
		cancel()
		_ = s.inst.cdp.Close()
	}
	for i := len(s.procs) - 1; i >= 0; i-- {
		// Chromium gets a moment to exit after Browser.close; Xvfb and x11vnc never exit on
		// their own.
		var wait time.Duration
		if s.procs[i] == s.chromium {
			wait = m.cfg.StopGrace / 2
		}
		stopGroup(s.procs[i], s.waits[i], wait, m.cfg.StopGrace)
	}
	if s.key.kind == Testing && m.cfg.Host != nil {
		if err := m.cfg.Host.Clean(s.acct); err != nil {
			slog.Warn("browser: removing the testing profile", "user", s.acct.User, "err", err)
		}
	}
}

// stopGroup waits up to wait for cmd to exit on its own, then signals its group: SIGTERM,
// and SIGKILL after grace. Once the leader is reaped its group ID may be reused, so the group
// is only signalled while the leader runs; what a browser starts dies with it (Chromium's
// helpers watch it), and in container mode the worker's start-up sweep stops anything of a
// browser user's left.
func stopGroup(cmd *exec.Cmd, exited <-chan struct{}, wait, grace time.Duration) {
	pgid := cmd.Process.Pid
	// select picks at random between ready cases: look once more just before each signal.
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
